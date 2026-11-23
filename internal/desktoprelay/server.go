// Package desktoprelay provides independent desktop signaling and TURN over WSS.
// It never grants screen or input access through a session pairing credential.
package desktoprelay

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/muq9738/Vantry/internal/connectivity"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
	"github.com/pion/turn/v5"
	"github.com/pion/webrtc/v4"
)

type room struct {
	id, hostToken, viewerToken, turnUser, turnPassword string
	video                                              rtc.VideoConfig
	touched                                            time.Time
	peers                                              map[string]*signalPeer
	approved                                           bool
	rejected                                           bool
	viewerLabel                                        string
}
type signalPeer struct {
	conn   *websocket.Conn
	writes sync.Mutex
}

func (p *signalPeer) send(message rtc.SignalMessage) error {
	p.writes.Lock()
	defer p.writes.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return p.conn.WriteJSON(message)
}

const maxDesktopTunnels = 128

type streamOwner struct{ sessionID, role string }

type Server struct {
	key           string
	mu            sync.Mutex
	rooms         map[string]*room
	turn          *turn.Server
	listener      net.Listener
	ctx           context.Context
	cancel        context.CancelFunc
	streams       map[net.Conn]streamOwner
	activeTunnels int
	wg            sync.WaitGroup
	once          sync.Once
}

func New(key string) (*Server, error) {
	if len(key) < 32 {
		return nil, errors.New("桌面中继注册令牌至少需要 32 个字符")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{key: key, listener: listener, ctx: ctx, cancel: cancel, rooms: map[string]*room{}, streams: map[net.Conn]streamOwner{}}
	server, err := turn.NewServer(turn.ServerConfig{Realm: "vantry-desktop", AuthHandler: func(attributes *turn.RequestAttributes) (string, []byte, bool) {
		username, realm := attributes.Username, attributes.Realm
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, room := range s.rooms {
			if room.turnUser == username {
				return username, turn.GenerateAuthKey(username, realm, room.turnPassword), true
			}
		}
		return "", nil, false
	}, ListenerConfigs: []turn.ListenerConfig{{Listener: listener, RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{RelayAddress: net.IPv4(127, 0, 0, 1), Address: "127.0.0.1"}, PermissionHandler: func(_ net.Addr, peer net.IP) bool { return peer.Equal(net.IPv4(127, 0, 0, 1)) }}}})
	if err != nil {
		cancel()
		_ = listener.Close()
		return nil, err
	}
	s.turn = server
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.expire()
			case <-ctx.Done():
				return
			}
		}
	}()
	return s, nil
}
func (s *Server) Close() {
	s.once.Do(func() {
		s.cancel()
		s.mu.Lock()
		for _, room := range s.rooms {
			for _, peer := range room.peers {
				_ = peer.conn.Close()
			}
		}
		for conn := range s.streams {
			_ = conn.Close()
		}
		s.rooms = map[string]*room{}
		s.mu.Unlock()
		_ = s.turn.Close()
		_ = s.listener.Close()
		s.wg.Wait()
	})
}
func (s *Server) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, room := range s.rooms {
		if room.peers["host"] == nil && time.Since(room.touched) > 2*time.Minute {
			s.deleteLocked(id)
		}
	}
}
func (s *Server) deleteLocked(id string) {
	room := s.rooms[id]
	if room == nil {
		return
	}
	delete(s.rooms, id)
	for _, peer := range room.peers {
		_ = peer.conn.Close()
	}
	for conn, owner := range s.streams {
		if owner.sessionID == id {
			_ = conn.Close()
		}
	}
}
func (s *Server) closeRoleStreamsLocked(id, role string) {
	for conn, owner := range s.streams {
		if owner.sessionID == id && owner.role == role {
			_ = conn.Close()
		}
	}
}
func (s *Server) releaseTunnel() {
	s.mu.Lock()
	if s.activeTunnels > 0 {
		s.activeTunnels--
	}
	s.mu.Unlock()
}
func (s *Server) info(room *room, host bool) rtc.SessionInfo {
	info := rtc.SessionInfo{SessionID: room.id, ICEServers: []webrtc.ICEServer{{URLs: []string{"turn:127.0.0.1:" + portOf(s.listener.Addr()) + "?transport=tcp"}, Username: room.turnUser, Credential: room.turnPassword}}, ICETransportPolicy: "relay", ExpiresAt: time.Now().Add(10 * time.Minute), Video: room.video, Transport: "wss-tcp"}
	if host {
		info.HostToken = room.hostToken
		info.ViewerToken = room.viewerToken
	}
	return info
}
func portOf(address net.Addr) string { _, port, _ := net.SplitHostPort(address.String()); return port }
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if s.ctx.Err() != nil {
		http.Error(w, "Relay closed", 503)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/desktop")
	if path == "/v1/sessions" && req.Method == "POST" {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+s.key)) != 1 {
			http.Error(w, "Unauthorized", 401)
			return
		}
		var input struct {
			Video rtc.VideoConfig `json:"video"`
		}
		if connectivity.Decode(w, req, &input) != nil {
			http.Error(w, "Invalid request", 400)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.rooms) >= 64 {
			http.Error(w, "Capacity", 429)
			return
		}
		room := &room{id: connectivity.Token(), hostToken: connectivity.Token(), viewerToken: connectivity.Token(), turnUser: connectivity.Token(), turnPassword: connectivity.Token(), video: input.Video, touched: time.Now(), peers: map[string]*signalPeer{}}
		s.rooms[room.id] = room
		connectivity.JSON(w, 201, s.info(room, true))
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v1/sessions/"), "/")
	if !strings.HasPrefix(path, "/v1/sessions/") || len(parts) > 2 {
		http.NotFound(w, req)
		return
	}
	id := parts[0]
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	room := s.rooms[id]
	role := ""
	if room != nil {
		if token == room.hostToken {
			role = "host"
		} else if token == room.viewerToken {
			role = "viewer"
		}
	}
	if role == "" {
		s.mu.Unlock()
		http.Error(w, "Unauthorized", 401)
		return
	}
	room.touched = time.Now()
	if len(parts) == 1 {
		switch req.Method {
		case "GET":
			if role == "viewer" && room.rejected {
				s.mu.Unlock()
				http.Error(w, "共享端已拒绝此连接", 403)
				return
			}
			info := s.info(room, false)
			pending := room.viewerLabel
			approved := room.approved
			s.mu.Unlock()
			connectivity.JSON(w, 200, struct {
				rtc.SessionInfo
				PendingViewer string `json:"pendingViewer,omitempty"`
				Approved      bool   `json:"approved"`
			}{info, pending, approved})
			return
		case "DELETE":
			if role != "host" {
				s.mu.Unlock()
				http.Error(w, "Forbidden", 403)
				return
			}
			s.deleteLocked(id)
			s.mu.Unlock()
			w.WriteHeader(204)
			return
		}
		s.mu.Unlock()
		http.Error(w, "Method not allowed", 405)
		return
	}
	s.mu.Unlock()
	switch parts[1] {
	case "request":
		if req.Method != "POST" || role != "viewer" {
			http.Error(w, "Forbidden", 403)
			return
		}
		var input struct {
			Label string `json:"label"`
		}
		if connectivity.Decode(w, req, &input) != nil {
			http.Error(w, "Invalid request", 400)
			return
		}
		s.mu.Lock()
		if room.peers["viewer"] != nil {
			s.mu.Unlock()
			http.Error(w, "桌面已有连接方", 409)
			return
		}
		room.viewerLabel = nonemptyLabel(input.Label)
		room.approved = false
		room.rejected = false
		s.mu.Unlock()
		connectivity.JSON(w, 200, map[string]bool{"waiting": true})
	case "signal":
		if req.Method != "GET" || req.URL.Query().Get("role") != role {
			http.Error(w, "Invalid role", 403)
			return
		}
		s.signal(w, req, room, role)
	case "approve":
		if req.Method != "POST" || role != "host" {
			http.Error(w, "Forbidden", 403)
			return
		}
		var input struct {
			Allow bool `json:"allow"`
		}
		if connectivity.Decode(w, req, &input) != nil {
			http.Error(w, "Invalid request", 400)
			return
		}
		s.mu.Lock()
		room.approved = input.Allow
		room.rejected = !input.Allow
		if !input.Allow {
			s.closeRoleStreamsLocked(room.id, "viewer")
		}
		host, viewer := room.peers["host"], room.peers["viewer"]
		if !input.Allow {
			room.viewerLabel = ""
		}
		s.mu.Unlock()
		if input.Allow {
			if host != nil && viewer != nil {
				_ = host.send(rtc.SignalMessage{Type: "peer", Connected: true})
				_ = viewer.send(rtc.SignalMessage{Type: "peer", Connected: true})
			}
		} else if viewer != nil {
			_ = viewer.conn.Close()
		}
		connectivity.JSON(w, 200, map[string]bool{"approved": input.Allow})
	case "turn":
		if req.Method != "GET" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		s.tunnel(w, req, room, role)
	default:
		http.NotFound(w, req)
	}
}
func (s *Server) signal(w http.ResponseWriter, req *http.Request, room *room, role string) {
	s.mu.Lock()
	allowed := s.rooms[room.id] == room && s.ctx.Err() == nil && (role == "host" || room.approved)
	s.mu.Unlock()
	if !allowed {
		http.Error(w, "需要共享端确认", http.StatusForbidden)
		return
	}
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, req, nil)
	if err != nil {
		return
	}
	stopHeartbeat := connectivity.KeepAlive(conn)
	defer func() { _ = conn.Close(); stopHeartbeat() }()
	conn.SetReadLimit(64 << 10)
	peer := &signalPeer{conn: conn}
	s.mu.Lock()
	if s.rooms[room.id] != room || s.ctx.Err() != nil || (role == "viewer" && !room.approved) {
		s.mu.Unlock()
		return
	}
	old := room.peers[role]
	room.peers[role] = peer
	otherRole := "host"
	if role == "host" {
		otherRole = "viewer"
	}
	other := room.peers[otherRole]
	approved := room.approved
	s.mu.Unlock()
	if old != nil {
		_ = old.conn.Close()
	}
	if approved && other != nil {
		_ = other.send(rtc.SignalMessage{Type: "peer", Connected: true})
		_ = peer.send(rtc.SignalMessage{Type: "peer", Connected: true})
	}
	defer func() {
		s.mu.Lock()
		current := room.peers[role] == peer
		if current {
			delete(room.peers, role)
			if role == "viewer" {
				room.approved = false
				room.viewerLabel = ""
				s.closeRoleStreamsLocked(room.id, "viewer")
			}
		}
		other := room.peers[otherRole]
		s.mu.Unlock()
		if current && other != nil {
			_ = other.send(rtc.SignalMessage{Type: "peer", Connected: false})
		}
	}()
	for {
		var message rtc.SignalMessage
		if conn.ReadJSON(&message) != nil {
			return
		}
		if message.Type != "offer" && message.Type != "answer" && message.Type != "ice" && message.Type != "candidate" {
			continue
		}
		s.mu.Lock()
		other = room.peers[otherRole]
		approved = room.approved
		s.mu.Unlock()
		if approved && other != nil {
			if other.send(message) != nil {
				return
			}
		}
	}
}
func nonemptyLabel(label string) string {
	if label == "" {
		return "另一台 Vantry"
	}
	if len(label) > 128 {
		return label[:128]
	}
	return label
}
func (s *Server) tunnel(w http.ResponseWriter, req *http.Request, room *room, role string) {
	s.mu.Lock()
	allowed := s.rooms[room.id] == room && (role == "host" || room.approved)
	full := len(s.streams) >= 256 || s.activeTunnels >= maxDesktopTunnels
	if allowed && !full {
		s.activeTunnels++
	}
	s.mu.Unlock()
	if !allowed {
		http.Error(w, "请等待共享端确认观看与控制权限", 403)
		return
	}
	if full {
		http.Error(w, "Capacity", 429)
		return
	}
	defer s.releaseTunnel()
	upstream, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(req.Context(), "tcp", s.listener.Addr().String())
	if err != nil {
		http.Error(w, "TURN unavailable", 503)
		return
	}
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, req, nil)
	if err != nil {
		_ = upstream.Close()
		return
	}
	stream := connectivity.NewStream(conn)
	s.mu.Lock()
	if s.rooms[room.id] != room || s.ctx.Err() != nil || (role == "viewer" && !room.approved) {
		s.mu.Unlock()
		_ = stream.Close()
		_ = upstream.Close()
		return
	}
	s.streams[stream] = streamOwner{room.id, role}
	s.streams[upstream] = streamOwner{room.id, role}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.streams, stream); delete(s.streams, upstream); s.mu.Unlock() }()
	connectivity.Splice(s.ctx, stream, upstream)
}
