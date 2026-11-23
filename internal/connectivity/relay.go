package connectivity

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	hostConflictProbeTimeout = 1500 * time.Millisecond
	hostStaleProbeAge        = 25 * time.Second
)

type HostCredential struct {
	Key    string   `json:"key"`
	Routes []string `json:"routes"`
}

type RelayConfig struct {
	// RegistrationKey is read from a private configuration file, never a CLI flag.
	RegistrationKey string `json:"registrationKey"`
	// HostCredentials optionally isolates hosts to specific registration keys and routes.
	HostCredentials []HostCredential `json:"hostCredentials,omitempty"`
	// Routes binds public HTTP hostnames to device route IDs for unmodified T3 clients.
	Routes map[string]string `json:"routes"`
	// HTTPUpstreams retains existing T3/FRP HTTP and WebSocket entrances during migration.
	HTTPUpstreams          []HTTPUpstream `json:"httpUpstreams,omitempty"`
	MaxHosts               int            `json:"maxHosts,omitempty"`
	MaxConnections         int            `json:"maxConnections,omitempty"`
	MaxConnectionsPerRoute int            `json:"maxConnectionsPerRoute,omitempty"`
	MaxPendingPerRoute     int            `json:"maxPendingPerRoute,omitempty"`
	TrustedProxies         []string       `json:"trustedProxies,omitempty"`
}

type HTTPUpstream struct {
	Host   string `json:"host"`
	Prefix string `json:"prefix"`
	Target string `json:"target"`
}

type Open struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
}
type relayHost struct {
	conn         *websocket.Conn
	writes       sync.Mutex
	instance     string
	lastActivity atomic.Int64
	pongCh       chan struct{}
	closed       chan struct{}
}
type pendingStream struct {
	route string
	ready chan net.Conn
}
type attemptWindow struct {
	start time.Time
	count int
}

type Relay struct {
	cfg             RelayConfig
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	hosts           map[string]*relayHost
	pending         map[string]pendingStream
	streams         map[net.Conn]struct{}
	routePending    map[string]int
	routeStreams    map[string]int
	attempts        map[string]attemptWindow
	connectAttempts map[string]attemptWindow
	trustedProxies  []netip.Prefix
	pairings        Pairings
	transports      map[tunnelTarget]*http.Transport
}

func NewRelay(cfg RelayConfig) (*Relay, error) {
	if cfg.RegistrationKey == "" && len(cfg.HostCredentials) == 0 {
		return nil, errors.New("中继 registrationKey 至少需要 32 个字符")
	}
	if cfg.RegistrationKey != "" && len(cfg.RegistrationKey) < 32 {
		return nil, errors.New("中继 registrationKey 至少需要 32 个字符")
	}
	seenKeys := map[string]bool{}
	if cfg.RegistrationKey != "" {
		seenKeys[cfg.RegistrationKey] = true
	}
	for _, cred := range cfg.HostCredentials {
		if len(cred.Key) < 32 || seenKeys[cred.Key] || len(cred.Routes) == 0 {
			return nil, errors.New("主机凭据无效或重复")
		}
		seenKeys[cred.Key] = true
		for _, route := range cred.Routes {
			if !validRoute(route) {
				return nil, errors.New("主机凭据包含无效路由")
			}
		}
	}
	var proxies []netip.Prefix
	for _, raw := range cfg.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if prefix, err := netip.ParsePrefix(raw); err == nil {
			proxies = append(proxies, prefix)
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, errors.New("可信代理地址无效")
		}
		addr = addr.Unmap()
		proxies = append(proxies, netip.PrefixFrom(addr, addr.BitLen()))
	}
	if cfg.MaxHosts <= 0 {
		cfg.MaxHosts = 64
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 256
	}
	if cfg.MaxConnectionsPerRoute <= 0 {
		cfg.MaxConnectionsPerRoute = min(cfg.MaxConnections, 64)
	}
	if cfg.MaxPendingPerRoute <= 0 {
		cfg.MaxPendingPerRoute = 16
	}
	for _, upstream := range cfg.HTTPUpstreams {
		u, err := url.Parse(upstream.Target)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || upstream.Host == "" || !strings.HasPrefix(upstream.Prefix, "/") {
			return nil, errors.New("T3 迁移入口配置无效")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Relay{
		cfg:             cfg,
		ctx:             ctx,
		cancel:          cancel,
		hosts:           map[string]*relayHost{},
		pending:         map[string]pendingStream{},
		streams:         map[net.Conn]struct{}{},
		routePending:    map[string]int{},
		routeStreams:    map[string]int{},
		attempts:        map[string]attemptWindow{},
		connectAttempts: map[string]attemptWindow{},
		trustedProxies:  proxies,
		transports:      map[tunnelTarget]*http.Transport{},
	}, nil
}
func (r *Relay) Close() {
	r.cancel()
	r.mu.Lock()
	for _, host := range r.hosts {
		_ = host.conn.Close()
	}
	for conn := range r.streams {
		_ = conn.Close()
	}
	pools := make([]*http.Transport, 0, len(r.transports))
	for _, pool := range r.transports {
		pools = append(pools, pool)
	}
	r.mu.Unlock()
	for _, pool := range pools {
		pool.CloseIdleConnections()
	}
}
func (r *Relay) authorizeHost(req *http.Request, route string) (string, bool) {
	auth := req.Header.Get("Authorization")
	if r.cfg.RegistrationKey != "" && subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+r.cfg.RegistrationKey)) == 1 {
		return r.cfg.RegistrationKey, true
	}
	for _, cred := range r.cfg.HostCredentials {
		if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+cred.Key)) == 1 {
			if slices.Contains(cred.Routes, route) {
				return "host:" + cred.Key, true
			}
			return "", false
		}
	}
	return "", false
}
func (r *Relay) authorizePairingOwner(req *http.Request) (string, bool) {
	auth := req.Header.Get("Authorization")
	if r.cfg.RegistrationKey != "" && subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+r.cfg.RegistrationKey)) == 1 {
		return r.cfg.RegistrationKey, true
	}
	for _, cred := range r.cfg.HostCredentials {
		if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+cred.Key)) == 1 {
			return "host:" + cred.Key, true
		}
	}
	return "", false
}
func (r *Relay) isTrustedProxy(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range r.trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
func (r *Relay) clientIP(req *http.Request) string {
	host := req.RemoteAddr
	if h, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		host = h
	}
	peer, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if !r.isTrustedProxy(peer) {
		return peer.String()
	}
	var chain []string
	for _, header := range req.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(header, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				chain = append(chain, trimmed)
			}
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(chain[i])
		if err != nil {
			break
		}
		addr = addr.Unmap()
		if !r.isTrustedProxy(addr) {
			return addr.String()
		}
	}
	if realIP := strings.TrimSpace(req.Header.Get("X-Real-IP")); realIP != "" {
		if addr, err := netip.ParseAddr(realIP); err == nil {
			return addr.Unmap().String()
		}
	}
	return peer.String()
}
func (r *Relay) allowRate(buckets map[string]attemptWindow, key string, limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	a, known := buckets[key]
	if !known && len(buckets) >= 4096 {
		for k, item := range buckets {
			if now.Sub(item.start) > time.Minute {
				delete(buckets, k)
			}
		}
		if len(buckets) >= 4096 {
			return false
		}
	}
	if now.Sub(a.start) > time.Minute {
		a = attemptWindow{start: now}
	}
	a.count++
	buckets[key] = a
	if len(buckets) > 4096 {
		for k, item := range buckets {
			if now.Sub(item.start) > time.Minute {
				delete(buckets, k)
			}
		}
	}
	return a.count <= limit
}
func upgrade(w http.ResponseWriter, req *http.Request) (*websocket.Conn, error) {
	return (&websocket.Upgrader{ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10, CheckOrigin: func(req *http.Request) bool {
		if req.Header.Get("Origin") == "" {
			return true
		}
		u, err := url.Parse(req.Header.Get("Origin"))
		return err == nil && strings.EqualFold(u.Host, req.Host)
	}}).Upgrade(w, req, nil)
}
func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func Decode(w http.ResponseWriter, req *http.Request, value any) error {
	return json.NewDecoder(http.MaxBytesReader(w, req.Body, 32<<10)).Decode(value)
}
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/healthz":
		JSON(w, 200, map[string]any{"service": "vantry-relay", "protocol": 1})
		return
	case "/relay/host":
		r.host(w, req)
		return
	case "/relay/attach":
		r.attach(w, req)
		return
	case "/relay/connect":
		r.connect(w, req)
		return
	case "/pairings", "/pairings/redeem", "/pairings/revoke":
		r.pair(w, req)
		return
	}
	hostname := req.Host
	if host, _, err := net.SplitHostPort(req.Host); err == nil {
		hostname = host
	}
	route := r.cfg.Routes[strings.ToLower(req.Host)]
	if route == "" {
		route = r.cfg.Routes[strings.ToLower(hostname)]
	}
	if route == "" {
		var selected *HTTPUpstream
		for i := range r.cfg.HTTPUpstreams {
			upstream := &r.cfg.HTTPUpstreams[i]
			if (strings.EqualFold(req.Host, upstream.Host) || !strings.Contains(upstream.Host, ":") && strings.EqualFold(hostname, upstream.Host)) && strings.HasPrefix(req.URL.Path, upstream.Prefix) && (selected == nil || len(upstream.Prefix) > len(selected.Prefix)) {
				selected = upstream
			}
		}
		if selected != nil {
			target, _ := url.Parse(selected.Target)
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.FlushInterval = -1
			proxy.ServeHTTP(w, req)
			return
		}
		http.NotFound(w, req)
		return
	}
	// TLS terminates here; the host's T3 adapter still authenticates every API
	// request and WebSocket ticket. This supports a root URL on each hostname.
	transport := r.tunnelTransport(route, "t3")
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "vantry-host"})
	proxy.Transport = transport
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "设备离线或连接中断", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, req)
}
func validRoute(route string) bool {
	if len(route) < 16 || len(route) > 128 {
		return false
	}
	for _, c := range route {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func (r *Relay) probeAndEvictHost(ctx context.Context, existing *relayHost) bool {
	select {
	case <-existing.pongCh:
	default:
	}
	timeout := hostConflictProbeTimeout
	if err := existing.conn.WriteControl(websocket.PingMessage, []byte("probe"), time.Now().Add(timeout)); err == nil {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-existing.pongCh:
			return true
		case <-existing.closed:
			return false
		case <-ctx.Done():
			return true
		case <-timer.C:
		}
	}
	_ = existing.conn.Close()
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-existing.closed:
	case <-timer.C:
	}
	return false
}
func (r *Relay) host(w http.ResponseWriter, req *http.Request) {
	route := req.URL.Query().Get("route")
	if _, ok := r.authorizeHost(req, route); !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !validRoute(route) {
		http.Error(w, "Invalid route", http.StatusBadRequest)
		return
	}
	instance := req.URL.Query().Get("instance")
	for {
		r.mu.Lock()
		existing := r.hosts[route]
		if existing == nil {
			if len(r.hosts) >= r.cfg.MaxHosts {
				r.mu.Unlock()
				http.Error(w, "Capacity", http.StatusServiceUnavailable)
				return
			}
			r.mu.Unlock()
			break
		}
		shouldProbe := (instance != "" && existing.instance == instance) ||
			time.Since(time.Unix(0, existing.lastActivity.Load())) > hostStaleProbeAge
		r.mu.Unlock()
		if !shouldProbe {
			http.Error(w, "Route already registered", http.StatusConflict)
			return
		}
		if r.probeAndEvictHost(req.Context(), existing) {
			http.Error(w, "Route already registered", http.StatusConflict)
			return
		}
	}
	conn, err := upgrade(w, req)
	if err != nil {
		return
	}
	host := &relayHost{
		conn:     conn,
		instance: instance,
		pongCh:   make(chan struct{}, 1),
		closed:   make(chan struct{}),
	}
	host.lastActivity.Store(time.Now().UnixNano())
	stopHeartbeat := keepAliveWithPong(conn, func() {
		host.lastActivity.Store(time.Now().UnixNano())
		select {
		case host.pongCh <- struct{}{}:
		default:
		}
	})
	defer stopHeartbeat()
	conn.SetReadLimit(4096)
	r.mu.Lock()
	if r.hosts[route] != nil || len(r.hosts) >= r.cfg.MaxHosts || r.ctx.Err() != nil {
		r.mu.Unlock()
		_ = conn.Close()
		close(host.closed)
		return
	}
	r.hosts[route] = host
	r.mu.Unlock()
	defer func() {
		_ = conn.Close()
		r.mu.Lock()
		if r.hosts[route] == host {
			delete(r.hosts, route)
		}
		r.mu.Unlock()
		close(host.closed)
	}()
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}
func (r *Relay) open(ctx context.Context, route, channel string) (net.Conn, error) {
	id := Token()
	ready := make(chan net.Conn)
	r.mu.Lock()
	host := r.hosts[route]
	if r.ctx.Err() != nil || host == nil {
		r.mu.Unlock()
		return nil, &RelayError{Stage: "relay_open", StatusCode: http.StatusBadGateway, Message: "设备离线或中继繁忙"}
	}
	if len(r.pending)+len(r.streams) >= r.cfg.MaxConnections ||
		r.routePending[route] >= r.cfg.MaxPendingPerRoute ||
		r.routePending[route]+r.routeStreams[route] >= r.cfg.MaxConnectionsPerRoute {
		r.mu.Unlock()
		return nil, &RelayError{Stage: "relay_open", StatusCode: http.StatusTooManyRequests, Message: "设备离线或中继繁忙"}
	}
	r.pending[id] = pendingStream{route, ready}
	r.routePending[route]++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, id)
		if r.routePending[route] > 1 {
			r.routePending[route]--
		} else {
			delete(r.routePending, route)
		}
		r.mu.Unlock()
	}()
	host.writes.Lock()
	_ = host.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := host.conn.WriteJSON(Open{id, channel})
	host.writes.Unlock()
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case conn := <-ready:
		return conn, nil
	case <-host.closed:
		return nil, &RelayError{Stage: "relay_open", StatusCode: http.StatusBadGateway, Message: "设备连接已断开"}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.ctx.Done():
		return nil, net.ErrClosed
	case <-timer.C:
		return nil, &RelayError{Stage: "relay_open", StatusCode: http.StatusGatewayTimeout, Message: "设备连接超时"}
	}
}
func (r *Relay) attach(w http.ResponseWriter, req *http.Request) {
	route := req.URL.Query().Get("route")
	if _, ok := r.authorizeHost(req, route); !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	pending, ok := r.pending[req.URL.Query().Get("id")]
	r.mu.Unlock()
	if !ok || pending.route != route {
		http.Error(w, "Expired stream", http.StatusNotFound)
		return
	}
	conn, err := upgrade(w, req)
	if err != nil {
		return
	}
	stream := NewStream(conn)
	owned := &trackedConn{Conn: stream, done: make(chan struct{}), onClose: func() {
		r.mu.Lock()
		delete(r.streams, stream)
		if r.routeStreams[route] > 1 {
			r.routeStreams[route]--
		} else {
			delete(r.routeStreams, route)
		}
		r.mu.Unlock()
	}}
	r.mu.Lock()
	r.streams[stream] = struct{}{}
	r.routeStreams[route]++
	r.mu.Unlock()
	defer owned.Close()
	select {
	case pending.ready <- owned:
	case <-req.Context().Done():
		return
	case <-r.ctx.Done():
		return
	case <-time.After(15 * time.Second):
		return
	}
	select {
	case <-owned.done:
	case <-req.Context().Done():
	case <-r.ctx.Done():
	}
}
func (r *Relay) connect(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !websocket.IsWebSocketUpgrade(req) {
		http.Error(w, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}
	route := req.URL.Query().Get("route")
	if !validRoute(route) {
		http.Error(w, "Invalid route", http.StatusBadRequest)
		return
	}
	if !r.allowRate(r.connectAttempts, r.clientIP(req), 60) {
		http.Error(w, "请稍后重试", http.StatusTooManyRequests)
		return
	}
	upstream, err := r.open(req.Context(), route, "core")
	if err != nil {
		status := http.StatusBadGateway
		var relayErr *RelayError
		if errors.As(err, &relayErr) && relayErr.StatusCode != 0 {
			status = relayErr.StatusCode
		}
		http.Error(w, "Host unavailable", status)
		return
	}
	defer upstream.Close()
	conn, err := upgrade(w, req)
	if err != nil {
		return
	}
	Splice(req.Context(), NewStream(conn), upstream)
}
func (r *Relay) pair(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && req.URL.Path == "/pairings" {
		owner, ok := r.authorizePairingOwner(req)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		entry, ok := r.pairings.Info(req.URL.Query().Get("id"), owner)
		if !ok {
			http.NotFound(w, req)
			return
		}
		JSON(w, 200, entry)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if req.URL.Path == "/pairings/redeem" {
		if !r.allowRate(r.attempts, r.clientIP(req), 20) {
			http.Error(w, "请稍后重试", http.StatusTooManyRequests)
			return
		}
		var input struct {
			Scope string `json:"scope"`
			Code  string `json:"code"`
			Name  string `json:"name"`
		}
		if Decode(w, req, &input) != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		entry, err := r.pairings.Redeem(input.Scope, input.Code, input.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		JSON(w, 200, entry)
		return
	}
	owner, ok := r.authorizePairingOwner(req)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if req.URL.Path == "/pairings/revoke" {
		var input struct {
			ID string `json:"id"`
		}
		if Decode(w, req, &input) != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		JSON(w, 200, map[string]bool{"revoked": r.pairings.Revoke(input.ID, owner)})
		return
	}
	var input Pairing
	if Decode(w, req, &input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	entry, err := r.pairings.Put(input.Scope, input.Name, input.Payload, time.Until(input.ExpiresAt), owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	entry.Payload = nil
	JSON(w, 201, entry)
}

type trackedConn struct {
	net.Conn
	once    sync.Once
	done    chan struct{}
	onClose func()
}

func (c *trackedConn) Close() error {
	var err error
	c.once.Do(func() { err = c.Conn.Close(); close(c.done); c.onClose() })
	return err
}
