package desktoprelay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/muq9738/Vantry/internal/connectivity"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

type Ticket struct {
	Relay     string    `json:"relay"`
	Code      string    `json:"code,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	Token     string    `json:"token,omitempty"`
}

func (t Ticket) Link() string {
	data, _ := json.Marshal(t)
	return "vantry://desktop#" + base64.RawURLEncoding.EncodeToString(data)
}
func ParseTicket(raw string) (Ticket, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "vantry" || u.Host != "desktop" || len(u.Fragment) > 8192 {
		return Ticket{}, errors.New("请使用 Vantry 桌面共享码或链接；会话配对码不能用于桌面共享")
	}
	data, err := base64.RawURLEncoding.DecodeString(u.Fragment)
	if err != nil {
		return Ticket{}, errors.New("桌面共享链接不完整")
	}
	var ticket Ticket
	if json.Unmarshal(data, &ticket) != nil {
		return Ticket{}, errors.New("桌面共享链接无效")
	}
	if _, err = ValidateURL(ticket.Relay); err != nil {
		return Ticket{}, err
	}
	if ticket.Code == "" && (ticket.SessionID == "" || len(ticket.Token) < 32) {
		return Ticket{}, errors.New("桌面共享凭据不完整")
	}
	if ticket.Code != "" && !time.Now().Before(ticket.ExpiresAt) {
		return Ticket{}, errors.New("桌面共享码已过期，请在共享端重新生成")
	}
	return ticket, nil
}
func ValidateURL(raw string) (string, error) {
	return ValidateURLWithOptions(raw, os.Getenv("VANTRY_ALLOW_INSECURE_LAN_RELAY") == "1")
}
func ValidateURLWithOptions(raw string, allowInsecurePrivate bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("请输入桌面中继 HTTPS 地址")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || (!ip.IsLoopback() && (!ip.IsPrivate() || !allowInsecurePrivate)))) {
			return "", errors.New("公网或局域网桌面中继需要 HTTPS（私网明文 HTTP 仅限显式测试选项）")
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func Redeem(ctx context.Context, relay, input string) (Ticket, error) {
	if strings.HasPrefix(strings.TrimSpace(input), "vantry://") {
		ticket, err := ParseTicket(input)
		if err != nil {
			return Ticket{}, err
		}
		if ticket.Code == "" {
			return ticket, nil
		}
		relay = ticket.Relay
		input = ticket.Code
	}
	base, err := ValidateURL(relay)
	if err != nil {
		return Ticket{}, err
	}
	var pairing connectivity.Pairing
	if err = connectivity.Request(ctx, nil, "POST", base, "/desktop/pairings/redeem", "", map[string]string{"scope": connectivity.Desktop, "code": input}, &pairing); err != nil {
		return Ticket{}, err
	}
	var ticket Ticket
	if json.Unmarshal(pairing.Payload, &ticket) != nil {
		return Ticket{}, errors.New("桌面共享资料无效")
	}
	return ParseTicket(ticket.Link())
}

type HTTPRelay struct {
	base, key string
	viewer    bool
	client    *http.Client
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	listener  net.Listener
	streams   map[net.Conn]struct{}
	wg        sync.WaitGroup
	closed    bool
	info      rtc.SessionInfo
	pairing   connectivity.Pairing
}

func NewClient(base, key string, viewer bool) (*HTTPRelay, error) {
	base, err := ValidateURL(base)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &HTTPRelay{base: base, key: key, viewer: viewer, client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, ctx: ctx, cancel: cancel, streams: map[net.Conn]struct{}{}}, nil
}
func (r *HTTPRelay) Create(ctx context.Context, video rtc.VideoConfig) (rtc.SessionInfo, error) {
	var info rtc.SessionInfo
	if err := connectivity.Request(ctx, r.client, "POST", r.base, "/desktop/v1/sessions", r.key, map[string]any{"video": video}, &info); err != nil {
		return info, err
	}
	r.mu.Lock()
	r.info = info
	r.mu.Unlock()
	if err := r.prepare(&info, info.HostToken); err != nil {
		_ = r.Delete(ctx, info.SessionID, info.HostToken)
		return info, err
	}
	if _, err := r.GenerateCode(ctx); err != nil {
		_ = r.Delete(ctx, info.SessionID, info.HostToken)
		return info, err
	}
	return info, nil
}
func (r *HTTPRelay) GenerateCode(ctx context.Context) (connectivity.Pairing, error) {
	r.mu.Lock()
	info, old := r.info, r.pairing
	r.mu.Unlock()
	if info.SessionID == "" {
		return connectivity.Pairing{}, errors.New("请先开启本机桌面共享")
	}
	if old.ID != "" {
		_ = connectivity.Request(ctx, r.client, "POST", r.base, "/desktop/pairings/revoke", r.key, map[string]string{"id": old.ID}, nil)
	}
	payload, _ := json.Marshal(Ticket{Relay: r.base, SessionID: info.SessionID, Token: info.ViewerToken})
	var pairing connectivity.Pairing
	name, _ := os.Hostname()
	err := connectivity.Request(ctx, r.client, "POST", r.base, "/desktop/pairings", r.key, connectivity.Pairing{Scope: connectivity.Desktop, Name: name, ExpiresAt: time.Now().Add(5 * time.Minute), Payload: payload}, &pairing)
	if err == nil {
		r.mu.Lock()
		r.pairing = pairing
		r.mu.Unlock()
	}
	return pairing, err
}
func (r *HTTPRelay) Pairing() connectivity.Pairing {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pairing
}

// ViewerAccess is used only by Core's authenticated mobile gateway.
func (r *HTTPRelay) ViewerAccess() (Ticket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.info.SessionID == "" || r.info.ViewerToken == "" {
		return Ticket{}, errors.New("desktop sharing is not active")
	}
	return Ticket{Relay: r.base, SessionID: r.info.SessionID, Token: r.info.ViewerToken}, nil
}

func (r *HTTPRelay) AuthorizePairedViewer(ctx context.Context, label string) error {
	ticket, err := r.ViewerAccess()
	if err != nil {
		return err
	}
	if err := connectivity.Request(ctx, r.client, "POST", ticket.Relay, "/desktop/v1/sessions/"+ticket.SessionID+"/request", ticket.Token, map[string]string{"label": label}, nil); err != nil {
		return err
	}
	return r.Approve(ctx, true)
}
func (r *HTTPRelay) Ticket() string {
	pair := r.Pairing()
	if pair.Code == "" {
		return ""
	}
	return (Ticket{Relay: r.base, Code: pair.Code, ExpiresAt: pair.ExpiresAt}).Link()
}
func (r *HTTPRelay) Get(ctx context.Context, id, token string) (rtc.SessionInfo, error) {
	if strings.ContainsAny(id, "/?#") || id == "" {
		return rtc.SessionInfo{}, errors.New("桌面会话身份无效")
	}
	if r.viewer {
		name, _ := os.Hostname()
		if err := connectivity.Request(ctx, r.client, "POST", r.base, "/desktop/v1/sessions/"+id+"/request", token, map[string]string{"label": name}, nil); err != nil {
			return rtc.SessionInfo{}, err
		}
	}
	wait, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		var status struct {
			rtc.SessionInfo
			Approved bool `json:"approved"`
		}
		if err := connectivity.Request(wait, r.client, "GET", r.base, "/desktop/v1/sessions/"+id, token, nil, &status); err != nil {
			return rtc.SessionInfo{}, err
		}
		if !r.viewer || status.Approved {
			if err := r.prepare(&status.SessionInfo, token); err != nil {
				return rtc.SessionInfo{}, err
			}
			return status.SessionInfo, nil
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-timer.C:
		case <-wait.Done():
			timer.Stop()
			return rtc.SessionInfo{}, errors.New("等待共享端确认超时或连接已取消")
		}
	}
}
func (r *HTTPRelay) Pending(ctx context.Context) (string, error) {
	r.mu.Lock()
	info := r.info
	r.mu.Unlock()
	var status struct {
		PendingViewer string `json:"pendingViewer"`
		Approved      bool   `json:"approved"`
	}
	err := connectivity.Request(ctx, r.client, "GET", r.base, "/desktop/v1/sessions/"+info.SessionID, info.HostToken, nil, &status)
	if status.Approved {
		return "", err
	}
	return status.PendingViewer, err
}
func (r *HTTPRelay) Approve(ctx context.Context, allow bool) error {
	r.mu.Lock()
	info := r.info
	r.mu.Unlock()
	return connectivity.Request(ctx, r.client, "POST", r.base, "/desktop/v1/sessions/"+info.SessionID+"/approve", info.HostToken, map[string]bool{"allow": allow}, nil)
}
func (r *HTTPRelay) Delete(ctx context.Context, id, token string) error {
	r.mu.Lock()
	pairing := r.pairing
	r.mu.Unlock()
	if pairing.ID != "" {
		_ = connectivity.Request(ctx, r.client, "POST", r.base, "/desktop/pairings/revoke", r.key, map[string]string{"id": pairing.ID}, nil)
	}
	return connectivity.Request(ctx, r.client, "DELETE", r.base, "/desktop/v1/sessions/"+id, token, nil, nil)
}
func (r *HTTPRelay) Signal(ctx context.Context, id, role, token string) (*rtc.SignalConn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, connectivity.WebSocketURL(r.base, "/desktop/v1/sessions/"+id+"/signal", url.Values{"role": {role}}), http.Header{"Authorization": {"Bearer " + token}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, errors.New("桌面信令连接失败")
	}
	conn.SetReadLimit(64 << 10)
	stopHeartbeat := connectivity.KeepAlive(conn)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return rtc.NewSignalConn(conn, func() { stop(); stopHeartbeat() }), nil
}
func (r *HTTPRelay) prepare(info *rtc.SessionInfo, token string) error {
	if info.Transport != "wss-tcp" || info.ICETransportPolicy != "relay" || len(info.ICEServers) == 0 {
		return errors.New("所选中继未提供 Vantry WSS/TURN 桌面通道")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return net.ErrClosed
	}
	if r.listener == nil {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		r.listener = listener
		r.wg.Add(1)
		go r.accept(listener, info.SessionID, token)
	}
	for i := range info.ICEServers {
		info.ICEServers[i].URLs = []string{"turn:" + r.listener.Addr().String() + "?transport=tcp"}
	}
	return nil
}
func (r *HTTPRelay) accept(listener net.Listener, id, token string) {
	defer r.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			_ = conn.Close()
			return
		}
		r.streams[conn] = struct{}{}
		r.wg.Add(1)
		r.mu.Unlock()
		go func() {
			defer r.wg.Done()
			defer func() { r.mu.Lock(); delete(r.streams, conn); r.mu.Unlock() }()
			r.forward(conn, id, token)
		}()
	}
}
func (r *HTTPRelay) forward(local net.Conn, id, token string) {
	defer local.Close()
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(r.ctx, connectivity.WebSocketURL(r.base, "/desktop/v1/sessions/"+id+"/turn", nil), http.Header{"Authorization": {"Bearer " + token}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return
	}
	stream := connectivity.NewStream(conn)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = stream.Close()
		return
	}
	r.streams[stream] = struct{}{}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.streams, stream); r.mu.Unlock() }()
	connectivity.Splice(r.ctx, local, stream)
}
func (r *HTTPRelay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.cancel()
	if r.listener != nil {
		_ = r.listener.Close()
	}
	for conn := range r.streams {
		_ = conn.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
}
