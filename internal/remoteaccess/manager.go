package remoteaccess

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muq9738/Vantry/internal/connectivity"
	"github.com/muq9738/Vantry/internal/t3compat"
	"github.com/muq9738/Vantry/pkg/api"
)

type Status struct {
	Enabled                 bool           `json:"enabled"`
	Connected               bool           `json:"connected"`
	State                   string         `json:"state"`
	Error                   string         `json:"error,omitempty"`
	Name                    string         `json:"name"`
	Relay                   string         `json:"relay"`
	Relays                  []string       `json:"relays,omitempty"`
	PublicURL               string         `json:"publicUrl,omitempty"`
	RouteID                 string         `json:"routeId,omitempty"`
	ListenAddress           string         `json:"listenAddress,omitempty"`
	PairingLink             string         `json:"pairingLink,omitempty"`
	PairingCode             string         `json:"pairingCode,omitempty"`
	PairingState            string         `json:"pairingState,omitempty"`
	PairedWith              string         `json:"pairedWith,omitempty"`
	PairingExpiresAt        time.Time      `json:"pairingExpiresAt,omitempty"`
	RevokePendingID         string         `json:"revokePendingId,omitempty"`
	CertificateExpiresAt    time.Time      `json:"certificateExpiresAt,omitempty"`
	CertificateExpiringSoon bool           `json:"certificateExpiringSoon,omitempty"`
	T3PairingLink           string         `json:"t3PairingLink,omitempty"`
	Peers                   []PeerInfo     `json:"peers"`
	Clients                 []AccessClient `json:"clients"`
	Nearby                  []NearbyDevice `json:"nearby"`
	Direct                  []string       `json:"direct,omitempty"`
	RelayConnected          bool           `json:"relayConnected"`
	DiscoveryError          string         `json:"discoveryError,omitempty"`
}

type Options struct {
	DataDir    string
	SocketPath string
	// Kept only for callers migrating from old configurations. Native networking ignores them.
	ProtocolModule           string
	Node                     string
	DisableDiscovery         bool
	ListenAddress            string
	ListenInterface          string
	AllowInsecurePrivateHTTP bool
	T3Handler                http.Handler
	T3Pairing                func(context.Context, string) (string, error)
	T3Revoke                 func() error
	T3Disconnect             func()
	T3CancelPairings         func()
	T3Clients                func() []AccessClient
	T3RevokeClient           func(string) error
}

type Manager struct {
	mu               sync.Mutex
	root             context.Context
	opts             Options
	cfg              configuration
	state, failure   string
	generation       int
	bridges          []*connectivity.Host
	server           *http.Server
	t3Server         *http.Server
	advertiser       *mdnsService
	discovery        *mdnsService
	nearby           map[string]NearbyDevice
	checks           map[string]PeerInfo
	discoveryError   string
	pairings         connectivity.Pairings
	pairing          connectivity.Pairing
	pairLink, t3Link string
	revokePendingID  string
	pendingCodes     map[string]*pendingPairing
	redeemAttempts   map[string]redeemWindow
	requests         map[string]map[uint64]context.CancelFunc
	nextRequest      uint64
	refreshCancel    context.CancelFunc
	tlsCert          atomic.Pointer[tls.Certificate]
	mobileTLSCert    atomic.Pointer[tls.Certificate]
}

func New(ctx context.Context, opts Options) (*Manager, error) {
	cfg, err := loadConfig(opts.DataDir)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m := &Manager{
		root:           ctx,
		opts:           opts,
		cfg:            cfg,
		state:          "off",
		nearby:         map[string]NearbyDevice{},
		checks:         map[string]PeerInfo{},
		pendingCodes:   map[string]*pendingPairing{},
		redeemAttempts: map[string]redeemWindow{},
	}
	registerNearbySource(opts.DataDir, m)
	return m, nil
}

func (m *Manager) allowInsecurePrivate() bool {
	return m.opts.AllowInsecurePrivateHTTP || os.Getenv("VANTRY_ALLOW_INSECURE_LAN_RELAY") == "1"
}

func (m *Manager) Status() Status { m.mu.Lock(); defer m.mu.Unlock(); return m.statusLocked() }

func (m *Manager) bridgeStatusLocked() (state string, relayConnected bool, bridgeErr string) {
	if len(m.bridges) == 0 {
		return m.state, false, ""
	}
	var fallbackState string
	for _, b := range m.bridges {
		st := b.State()
		if st == "online" {
			relayConnected = true
			state = "online"
		}
		if fallbackState == "" {
			fallbackState = st
		} else if st == "denied" || st == "conflict" {
			fallbackState = st
		}
		if errText := b.LastError(); errText != "" && bridgeErr == "" {
			bridgeErr = errText
		}
	}
	if state == "" {
		state = fallbackState
	}
	if relayConnected {
		bridgeErr = ""
	}
	return state, relayConnected, bridgeErr
}

func (m *Manager) statusLocked() Status {
	state, relayConnected, bridgeErr := m.bridgeStatusLocked()
	errText := m.failure
	if errText == "" && bridgeErr != "" {
		errText = bridgeErr
	}
	s := Status{
		Enabled:         m.server != nil,
		Connected:       m.server != nil,
		State:           state,
		Error:           errText,
		Name:            m.cfg.Pair.Name,
		Relay:           m.cfg.Pair.Relay,
		Relays:          m.configuredRelayURLsLocked(),
		PublicURL:       m.cfg.PublicURL,
		RouteID:         m.cfg.Pair.Route,
		RevokePendingID: m.revokePendingID,
		Peers:           []PeerInfo{},
		Clients:         []AccessClient{},
		Nearby:          []NearbyDevice{},
		DiscoveryError:  m.discoveryError,
		RelayConnected:  relayConnected,
	}
	if m.cfg.Pair.Certificate != "" {
		if cert, err := parseAndValidateCert(m.cfg.Pair.Certificate, true); err == nil {
			s.CertificateExpiresAt = cert.NotAfter
			s.CertificateExpiringSoon = time.Until(cert.NotAfter) <= certRenewLeadWindow
		}
	}
	if s.Enabled {
		s.Direct = append([]string(nil), m.cfg.Pair.Direct...)
		s.ListenAddress = m.server.Addr
	}
	if s.Enabled && (m.pairing.ID != "" || m.revokePendingID != "") {
		s.PairingCode = m.pairing.Code
		s.PairingState = m.pairing.Status
		s.PairedWith = m.pairing.PairedWith
		if m.revokePendingID != "" {
			s.PairingState = "revoke_pending"
		} else if m.pairing.Code != "" {
			if p := m.pendingCodes[connectivity.NormalizeCode(m.pairing.Code)]; p != nil {
				s.PairingState = p.Status
				s.PairedWith = p.PairedWith
			}
		}
		if !m.pairing.ExpiresAt.IsZero() && !time.Now().Before(m.pairing.ExpiresAt) && s.PairingState == "waiting" {
			s.PairingState = "expired"
		}
		s.PairingExpiresAt = m.pairing.ExpiresAt
		s.PairingLink = m.pairLink
		s.T3PairingLink = m.t3Link
	}
	if m.opts.T3Clients != nil {
		s.Clients = append(s.Clients, m.opts.T3Clients()...)
	}
	for _, client := range m.cfg.Clients {
		client.Secret = ""
		s.Clients = append(s.Clients, client)
	}
	for _, peer := range m.cfg.Peers {
		info := PeerInfo{
			ID:       peer.ID,
			DeviceID: peer.Pair.DeviceID,
			Name:     peer.Pair.Name,
			Relay:    peer.Pair.Relay,
			Relays:   peer.Pair.relayList(),
			Direct:   append([]string(nil), peer.Pair.Direct...),
		}
		if device, ok := m.nearby[peer.Pair.DeviceID]; ok {
			info.Nearby = true
			info.Direct = append([]string(nil), device.Addresses...)
		}
		if checked, ok := m.checks[peer.ID]; ok {
			info.Reachable = checked.Reachable
			info.Mode = checked.Mode
			info.LatencyMs = checked.LatencyMs
			info.HandshakeMs = checked.HandshakeMs
			info.RTTMs = checked.RTTMs
			info.ErrorCode = checked.ErrorCode
			info.LastError = checked.LastError
		}
		s.Peers = append(s.Peers, info)
	}
	for id, device := range m.nearby {
		if id != m.cfg.Pair.DeviceID {
			s.Nearby = append(s.Nearby, device)
		}
	}
	sort.Slice(s.Nearby, func(i, j int) bool { return s.Nearby[i].Name < s.Nearby[j].Name })
	return s
}

func (m *Manager) Restore() {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if cfg.Enabled {
		_, _ = m.Start(m.root, cfg.Pair.Name, cfg.Pair.Relay, cfg.RegistrationKey)
	}
}

func interfaceBindIP(ifaceName string) (string, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return "", err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil || ip.To4() == nil || !validAdvertisedIP(ip) {
			continue
		}
		return ip.String(), nil
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil || !validAdvertisedIP(ip) {
			continue
		}
		return ip.String(), nil
	}
	return "", errors.New("所选网络接口没有可用的直连地址")
}

func (m *Manager) ensureCertificateStateLocked() (tls.Certificate, error) {
	cert, err := parseAndValidateCert(m.cfg.Pair.Certificate, true)
	if err == nil && time.Now().After(cert.NotAfter) && m.cfg.NextCertificate != "" && m.cfg.NextPrivateKey != "" {
		if nextPair, pairErr := tls.X509KeyPair([]byte(m.cfg.NextCertificate), []byte(m.cfg.NextPrivateKey)); pairErr == nil {
			if nextX509, xErr := parseAndValidateCert(m.cfg.NextCertificate, false); xErr == nil {
				m.cfg.Pair.Certificate = m.cfg.NextCertificate
				m.cfg.PrivateKey = m.cfg.NextPrivateKey
				m.cfg.NextCertificate = ""
				m.cfg.NextPrivateKey = ""
				cert = nextX509
				_ = nextPair
			}
		}
	}
	if err == nil && !time.Now().After(cert.NotAfter) && time.Until(cert.NotAfter) <= certRenewLeadWindow && m.cfg.NextCertificate == "" {
		if nextCert, nextKey, genErr := generateCertPair(); genErr == nil {
			m.cfg.NextCertificate = nextCert
			m.cfg.NextPrivateKey = nextKey
		}
	}
	return tls.X509KeyPair([]byte(m.cfg.Pair.Certificate), []byte(m.cfg.PrivateKey))
}

func (m *Manager) RenewCertificate() (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.Pair.Version == 0 {
		return m.statusLocked(), errors.New("主机身份尚未初始化")
	}
	nextCert, nextKey, err := generateCertPair()
	if err != nil {
		return m.statusLocked(), err
	}
	m.cfg.NextCertificate = nextCert
	m.cfg.NextPrivateKey = nextKey
	if err := saveConfig(m.opts.DataDir, m.cfg); err != nil {
		return m.statusLocked(), err
	}
	return m.statusLocked(), nil
}

func (m *Manager) ActivateNextCertificate() (Status, error) {
	m.mu.Lock()
	if m.cfg.NextCertificate == "" || m.cfg.NextPrivateKey == "" {
		s := m.statusLocked()
		m.mu.Unlock()
		return s, errors.New("尚未准备新证书，请先执行证书续期")
	}
	pairCert, err := tls.X509KeyPair([]byte(m.cfg.NextCertificate), []byte(m.cfg.NextPrivateKey))
	if err != nil {
		s := m.statusLocked()
		m.mu.Unlock()
		return s, err
	}
	m.cfg.Pair.Certificate = m.cfg.NextCertificate
	m.cfg.PrivateKey = m.cfg.NextPrivateKey
	m.cfg.NextCertificate = ""
	m.cfg.NextPrivateKey = ""
	m.tlsCert.Store(&pairCert)
	if err := saveConfig(m.opts.DataDir, m.cfg); err != nil {
		s := m.statusLocked()
		m.mu.Unlock()
		return s, err
	}
	s := m.statusLocked()
	m.mu.Unlock()
	return s, nil
}

func (m *Manager) serveIdentity(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	var expiresAt time.Time
	if cert, err := parseAndValidateCert(m.cfg.Pair.Certificate, true); err == nil {
		expiresAt = cert.NotAfter
	}
	snap := identitySnapshot{
		DeviceID:        m.cfg.Pair.DeviceID,
		Certificate:     m.cfg.Pair.Certificate,
		NextCertificate: m.cfg.NextCertificate,
		ExpiresAt:       expiresAt,
		Direct:          append([]string(nil), m.cfg.Pair.Direct...),
		Relays:          m.configuredRelayURLsLocked(),
	}
	m.mu.Unlock()
	connectivity.JSON(w, http.StatusOK, snap)
}

func (m *Manager) Start(ctx context.Context, name, relay, key string) (Status, error) {
	allowInsecure := m.allowInsecurePrivate()
	if strings.TrimSpace(relay) != "" {
		var err error
		relay, err = ValidateRelayWithOptions(relay, allowInsecure)
		if err != nil {
			return m.Status(), err
		}
	}
	if strings.TrimSpace(name) == "" {
		name, _ = os.Hostname()
	}
	m.mu.Lock()
	if key == "" && m.cfg.Pair.Relay == relay {
		key = m.cfg.RegistrationKey
	}
	revokeID, prevRelay, prevKey := m.stopLocked()
	if m.cfg.Pair.Version == 0 {
		pair, private, err := newIdentity(name, relay)
		if err != nil {
			m.mu.Unlock()
			_ = revokeRemotePairing(prevRelay, prevKey, revokeID)
			return m.Status(), err
		}
		m.cfg.Pair, m.cfg.PrivateKey = pair, private
	}
	if m.cfg.Pair.DeviceID == "" {
		m.cfg.Pair.DeviceID = randomID()
	}
	m.cfg.Pair.Name, m.cfg.Pair.Relay, m.cfg.RegistrationKey = name, relay, key
	certificate, err := m.ensureCertificateStateLocked()
	if err != nil {
		m.mu.Unlock()
		_ = revokeRemotePairing(prevRelay, prevKey, revokeID)
		return m.Status(), err
	}
	m.tlsCert.Store(&certificate)
	mobileCertificate, err := m.ensureMobileCertificateLocked()
	if err != nil {
		m.mu.Unlock()
		return m.Status(), err
	}
	m.mobileTLSCert.Store(&mobileCertificate)

	address := m.opts.ListenAddress
	if address == "" && m.opts.ListenInterface != "" {
		bindIP, ifaceErr := interfaceBindIP(m.opts.ListenInterface)
		if ifaceErr != nil {
			m.mu.Unlock()
			_ = revokeRemotePairing(prevRelay, prevKey, revokeID)
			return m.Status(), ifaceErr
		}
		address = net.JoinHostPort(bindIP, strconv.Itoa(m.cfg.DirectPort))
	}
	if address == "" {
		address = net.JoinHostPort("0.0.0.0", strconv.Itoa(m.cfg.DirectPort))
	}
	listener, err := net.Listen("tcp", address)
	if err != nil && m.opts.ListenAddress == "" && m.opts.ListenInterface == "" {
		listener, err = net.Listen("tcp", "0.0.0.0:0")
	}
	if err != nil {
		m.mu.Unlock()
		_ = revokeRemotePairing(prevRelay, prevKey, revokeID)
		return m.Status(), err
	}
	m.cfg.DirectPort = listener.Addr().(*net.TCPAddr).Port
	m.cfg.Pair.Direct = directAddresses(listener.Addr(), m.opts.ListenInterface)
	transport := &http.Transport{
		DisableCompression:  true,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     time.Minute,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", m.opts.SocketPath)
		},
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "vantry.local"})
	proxy.Transport = transport
	proxy.FlushInterval = -1
	server := &http.Server{
		Addr:              listener.Addr().String(),
		ReadHeaderTimeout: 10 * time.Second,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout:  20 * time.Second,
			PingTimeout:      10 * time.Second,
			WriteByteTimeout: 30 * time.Second,
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// Mobile requests retain their own scoped device authorization on the
			// existing encrypted LAN listener, exactly as on the relay ingress.
			if m.opts.T3Handler != nil && (strings.HasPrefix(req.URL.Path, "/api/") || req.URL.Path == "/.well-known/t3/environment" || req.URL.Path == "/oauth/token" || req.URL.Path == "/ws") {
				m.opts.T3Handler.ServeHTTP(w, req)
				return
			}
			if req.URL.Path == "/pairings/redeem" {
				m.redeemLocal(w, req)
				return
			}
			authorized, release, ok := m.authorizeRequest(req)
			if !ok {
				http.Error(w, "Unauthorized", 401)
				return
			}
			defer release()
			req = authorized
			if req.URL.Path == "/v1/access/identity" && req.Method == http.MethodGet {
				m.serveIdentity(w, req)
				return
			}
			if req.URL.Path == "/v1/workbench" {
				data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 16<<20))
				if err != nil {
					http.Error(w, "Request too large", 413)
					return
				}
				var command struct {
					Action string `json:"action"`
				}
				if json.Unmarshal(data, &command) != nil {
					http.Error(w, "Invalid request", 400)
					return
				}
				if strings.HasPrefix(command.Action, "rd.") || strings.HasPrefix(command.Action, "plugins.") {
					connectivity.JSON(w, 403, &api.Error{Code: api.InvalidCommand, Message: "桌面共享与插件授权需要在所属电脑本机管理"})
					return
				}
				req.Body = io.NopCloser(bytes.NewReader(data))
			}
			if strings.HasPrefix(req.URL.Path, "/v1/admin/") {
				http.Error(w, "Local operation", 403)
				return
			}
			req.Header.Del("Authorization")
			proxy.ServeHTTP(w, req)
		}),
	}
	server.TLSConfig = &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// Native peers explicitly use this SNI and keep their existing
			// Ed25519 identity. Apple clients address a paired IP directly.
			if hello.ServerName != "vantry.remote" {
				if mobile := m.mobileTLSCert.Load(); mobile != nil {
					return mobile, nil
				}
			}
			if current := m.tlsCert.Load(); current != nil {
				return current, nil
			}
			return nil, errors.New("missing host certificate")
		},
	}
	m.server = server
	go func() {
		// ServeTLS configures ALPN and HTTP/2; Serve on a TLS listener does not.
		_ = server.ServeTLS(listener, "", "")
		transport.CloseIdleConnections()
	}()
	m.state, m.failure = "direct", ""
	if !m.opts.DisableDiscovery {
		m.advertiser, err = advertiseMDNS(name, m.cfg.Pair.DeviceID, m.cfg.Pair.Direct, m.cfg.DirectPort)
		if err != nil {
			m.discoveryError = "局域网发现未启动，请检查本地网络权限。"
		}
	}
	var t3Address string
	if m.opts.T3Handler != nil {
		bindAddr := "127.0.0.1:0"
		if configured := strings.TrimSpace(m.cfg.T3Listen); configured != "" {
			bindAddr = configured
		}
		t3Listener, listenErr := net.Listen("tcp", bindAddr)
		if listenErr != nil && bindAddr != "127.0.0.1:0" {
			t3Listener, listenErr = net.Listen("tcp", "127.0.0.1:0")
		}
		if listenErr != nil {
			rID, rRelay, rKey := m.stopLocked()
			m.mu.Unlock()
			_ = revokeRemotePairing(prevRelay, prevKey, revokeID)
			_ = revokeRemotePairing(rRelay, rKey, rID)
			return m.Status(), listenErr
		}
		t3Address = t3Listener.Addr().String()
		m.t3Server = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: m.opts.T3Handler}
		go func(server *http.Server) { _ = server.Serve(t3Listener) }(m.t3Server)
	}

	tcpAddr := listener.Addr().(*net.TCPAddr)
	coreHost := "127.0.0.1"
	if tcpAddr.IP != nil && !tcpAddr.IP.IsUnspecified() {
		coreHost = tcpAddr.IP.String()
	} else if tcpAddr.IP != nil && tcpAddr.IP.To4() == nil {
		coreHost = "::1"
	}
	coreAddress := net.JoinHostPort(coreHost, strconv.Itoa(m.cfg.DirectPort))
	dialBackend := func(ctx context.Context, channel string) (net.Conn, error) {
		var backend string
		switch channel {
		case "core", "stream":
			backend = coreAddress
		case "t3", "http":
			backend = t3Address
			if backend == "" {
				return nil, errors.New("T3 adapter unavailable")
			}
		default:
			return nil, errors.New("remote channel is not configured")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", backend)
	}

	type relayTarget struct{ url, key string }
	var targets []relayTarget
	seenRelays := map[string]bool{}
	if relay != "" {
		seenRelays[relay] = true
		targets = append(targets, relayTarget{relay, key})
	}
	for _, ep := range m.cfg.Relays {
		v, rErr := ValidateRelayWithOptions(ep.URL, allowInsecure)
		if rErr != nil || seenRelays[v] {
			continue
		}
		seenRelays[v] = true
		rKey := ep.RegistrationKey
		if rKey == "" {
			rKey = key
		}
		targets = append(targets, relayTarget{v, rKey})
	}
	for _, target := range targets {
		bridge := connectivity.StartHostWithInstance(m.root, target.url, m.cfg.Pair.Route, target.key, m.cfg.Pair.DeviceID, dialBackend)
		m.bridges = append(m.bridges, bridge)
	}

	// Periodically refresh local interface addresses so DHCP/Wi-Fi changes
	// propagate to mDNS advertisements and newly issued pairing details.
	refreshCtx, refreshCancel := context.WithCancel(m.root)
	m.refreshCancel = refreshCancel
	go m.watchAddresses(refreshCtx, listener.Addr())

	m.cfg.Enabled = true
	if err := saveConfig(m.opts.DataDir, m.cfg); err != nil {
		rID, rRelay, rKey := m.stopLocked()
		m.mu.Unlock()
		_ = revokeRemotePairing(prevRelay, prevKey, revokeID)
		_ = revokeRemotePairing(rRelay, rKey, rID)
		return m.Status(), err
	}
	s := m.statusLocked()
	m.mu.Unlock()
	_ = revokeRemotePairing(prevRelay, prevKey, revokeID)
	return s, nil
}

func (m *Manager) watchAddresses(ctx context.Context, listenAddr net.Addr) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.refreshDirectAddresses(listenAddr)
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) refreshDirectAddresses(listenAddr net.Addr) bool {
	updated := directAddresses(listenAddr, m.opts.ListenInterface)
	m.mu.Lock()
	if m.server == nil || slices.Equal(m.cfg.Pair.Direct, updated) {
		m.mu.Unlock()
		return false
	}
	m.cfg.Pair.Direct = updated
	advertiser := m.advertiser
	name, id, port := m.cfg.Pair.Name, m.cfg.Pair.DeviceID, m.cfg.DirectPort
	_ = saveConfig(m.opts.DataDir, m.cfg)
	m.mu.Unlock()
	if advertiser != nil {
		advertiser.update(name, id, updated, port)
	}
	return true
}

func (m *Manager) authorizeRequest(req *http.Request) (*http.Request, func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	header := req.Header.Get("Authorization")
	secret := m.cfg.Pair.Secret
	valid := len(secret) >= 32 && subtle.ConstantTimeCompare([]byte(header), []byte("Bearer "+secret)) == 1
	if !valid {
		for _, client := range m.cfg.Clients {
			if len(client.Secret) >= 32 && subtle.ConstantTimeCompare([]byte(header), []byte("Bearer "+client.Secret)) == 1 {
				valid = true
				secret = client.Secret
				break
			}
		}
	}
	if !valid {
		return req, nil, false
	}
	life, cancel := context.WithCancel(req.Context())
	m.nextRequest++
	id := m.nextRequest
	if m.requests == nil {
		m.requests = map[string]map[uint64]context.CancelFunc{}
	}
	if m.requests[secret] == nil {
		m.requests[secret] = map[uint64]context.CancelFunc{}
	}
	m.requests[secret][id] = cancel
	return req.WithContext(life), func() {
		cancel()
		m.mu.Lock()
		delete(m.requests[secret], id)
		if len(m.requests[secret]) == 0 {
			delete(m.requests, secret)
		}
		m.mu.Unlock()
	}, true
}

func (m *Manager) cancelRequestsLocked(secret string) {
	for _, cancel := range m.requests[secret] {
		cancel()
	}
	delete(m.requests, secret)
}

func (m *Manager) stopLocked() (revokeID, relay, key string) {
	m.generation++
	if m.refreshCancel != nil {
		m.refreshCancel()
		m.refreshCancel = nil
	}
	for secret := range m.requests {
		m.cancelRequestsLocked(secret)
	}
	if m.opts.T3Disconnect != nil {
		m.opts.T3Disconnect()
	}
	if m.opts.T3CancelPairings != nil {
		m.opts.T3CancelPairings()
	}
	for _, bridge := range m.bridges {
		bridge.Close()
	}
	m.bridges = nil
	if m.advertiser != nil {
		m.advertiser.close()
		m.advertiser = nil
	}
	if m.server != nil {
		_ = m.server.Close()
		m.server = nil
	}
	if m.t3Server != nil {
		_ = m.t3Server.Close()
		m.t3Server = nil
	}
	if m.pairing.Status == "waiting" || m.pairing.Status == "revoke_pending" {
		revokeID = m.pairing.ID
	}
	if revokeID == "" {
		revokeID = m.revokePendingID
	}
	relay, key = m.cfg.Pair.Relay, m.cfg.RegistrationKey
	for _, item := range m.pendingCodes {
		if item.Status == "waiting" {
			item.Status = "revoked"
		}
	}
	m.pairings.Revoke(m.pairing.ID, "local")
	m.pairing = connectivity.Pairing{}
	m.pairLink = ""
	m.t3Link = ""
	m.revokePendingID = ""
	m.state, m.failure = "off", ""
	return revokeID, relay, key
}

func (m *Manager) Configure(ctx context.Context, name, relay, key string) (Status, error) {
	allowInsecure := m.allowInsecurePrivate()
	if strings.TrimSpace(relay) != "" {
		var err error
		relay, err = ValidateRelayWithOptions(relay, allowInsecure)
		if err != nil {
			return m.Status(), err
		}
	}
	m.mu.Lock()
	if m.server != nil {
		m.mu.Unlock()
		return m.Start(ctx, name, relay, key)
	}
	if key == "" && m.cfg.Pair.Relay == relay {
		key = m.cfg.RegistrationKey
	}
	m.cfg.Pair.Name, m.cfg.Pair.Relay, m.cfg.RegistrationKey = name, relay, key
	err := saveConfig(m.opts.DataDir, m.cfg)
	s := m.statusLocked()
	m.mu.Unlock()
	return s, err
}

func (m *Manager) ConfigureRelays(ctx context.Context, endpoints []RelayEndpoint) (Status, error) {
	allowInsecure := m.allowInsecurePrivate()
	cleaned := make([]RelayEndpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		v, err := ValidateRelayWithOptions(ep.URL, allowInsecure)
		if err != nil {
			return m.Status(), err
		}
		cleaned = append(cleaned, RelayEndpoint{URL: v, RegistrationKey: ep.RegistrationKey})
	}
	m.mu.Lock()
	m.cfg.Relays = cleaned
	running := m.server != nil
	name, relay, key := m.cfg.Pair.Name, m.cfg.Pair.Relay, m.cfg.RegistrationKey
	if !running {
		err := saveConfig(m.opts.DataDir, m.cfg)
		s := m.statusLocked()
		m.mu.Unlock()
		return s, err
	}
	m.mu.Unlock()
	return m.Start(ctx, name, relay, key)
}

func (m *Manager) SetPublicURL(raw string) (Status, error) {
	if raw != "" {
		validated, err := mobilePublicURL(raw)
		if err != nil {
			return m.Status(), err
		}
		raw = validated
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.PublicURL = raw
	return m.statusLocked(), saveConfig(m.opts.DataDir, m.cfg)
}

func (m *Manager) Stop() (Status, error) {
	m.mu.Lock()
	revokeID, relay, key := m.stopLocked()
	m.cfg.Enabled = false
	err := saveConfig(m.opts.DataDir, m.cfg)
	s := m.statusLocked()
	m.mu.Unlock()
	_ = revokeRemotePairing(relay, key, revokeID)
	return s, err
}

func (m *Manager) Close() {
	unregisterNearbySource(m.opts.DataDir, m)
	m.mu.Lock()
	revokeID, relay, key := m.stopLocked()
	if m.discovery != nil {
		m.discovery.close()
		m.discovery = nil
	}
	m.mu.Unlock()
	_ = revokeRemotePairing(relay, key, revokeID)
}

func (m *Manager) Pair(link string) (Status, error) { return m.PairWithContext(m.root, link, "") }

func (m *Manager) PairWithContext(ctx context.Context, input, relay string) (Status, error) {
	allowInsecure := m.allowInsecurePrivate()
	var pair Pair
	var err error
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(strings.ToUpper(trimmed), "DESK-") {
		return m.Status(), errors.New("这是桌面共享码，请到远程桌面插件连接")
	}
	if !strings.Contains(trimmed, "://") && !strings.HasPrefix(strings.ToUpper(trimmed), "SESSION-") {
		if relay == "" {
			m.mu.Lock()
			relay = m.cfg.Pair.Relay
			m.mu.Unlock()
		}
		base, validateErr := ValidateRelayWithOptions(relay, allowInsecure)
		if validateErr != nil {
			return m.Status(), validateErr
		}
		trimmed = base + "#token=" + url.QueryEscape(trimmed)
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") || strings.HasPrefix(trimmed, "ws://") || strings.HasPrefix(trimmed, "wss://") || strings.HasPrefix(trimmed, "t3code") {
		remote, pairErr := t3compat.PairRemote(ctx, trimmed)
		if pairErr != nil {
			return m.Status(), pairErr
		}
		pair = Pair{Version: 3, Name: remote.Name, Relay: remote.URL, Route: remote.EnvironmentID, DeviceID: remote.EnvironmentID, T3URL: remote.URL, T3Token: remote.Token}
	} else if strings.HasPrefix(strings.TrimSpace(input), "vantry://") {
		pair, err = parseLinkWithOptions(input, allowInsecure)
		if err != nil {
			return m.Status(), err
		}
		if pair.Version == 2 {
			pair, err = redeemLinkWithOptions(ctx, pair, m.opts)
			if err != nil {
				return m.Status(), err
			}
		}
	} else {
		if relay == "" {
			m.mu.Lock()
			relay = m.cfg.Pair.Relay
			m.mu.Unlock()
		}
		if relay, err = ValidateRelayWithOptions(relay, allowInsecure); err != nil {
			return m.Status(), err
		}
		var entry connectivity.Pairing
		name, _ := os.Hostname()
		err = connectivity.Request(ctx, nil, "POST", relay, "/pairings/redeem", "", map[string]string{"scope": connectivity.Session, "code": input, "name": name}, &entry)
		if err != nil {
			return m.Status(), err
		}
		if json.Unmarshal(entry.Payload, &pair) != nil {
			return m.Status(), errors.New("中继返回无效配对资料")
		}
		if pair.Version == 2 && pair.Secret == "" {
			if pair.Code == "" {
				pair.Code = connectivity.NormalizeCode(input)
			}
			if pair.ExpiresAt.IsZero() {
				pair.ExpiresAt = entry.ExpiresAt
			}
			if pair.Relay == "" {
				pair.Relay = relay
			}
			pair, err = parseLinkWithOptions(pair.Link(), allowInsecure)
			if err != nil {
				return m.Status(), err
			}
			pair, err = redeemLinkWithOptions(ctx, pair, m.opts)
			if err != nil {
				return m.Status(), err
			}
		} else {
			pair, err = parseLinkWithOptions(pair.Link(), allowInsecure)
			if err != nil {
				return m.Status(), err
			}
		}
	}
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, p := range m.cfg.Peers {
		if p.Pair.DeviceID == pair.DeviceID && pair.DeviceID != "" || p.Pair.Route == pair.Route && p.Pair.Relay == pair.Relay {
			m.cfg.Peers[i].Pair = pair
			return m.statusLocked(), saveConfig(m.opts.DataDir, m.cfg)
		}
	}
	m.cfg.Peers = append(m.cfg.Peers, Peer{ID: randomID(), Pair: pair})
	return m.statusLocked(), saveConfig(m.opts.DataDir, m.cfg)
}

type PeerUpdate struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Relay  string   `json:"relay"`
	Relays []string `json:"relays,omitempty"`
	Direct []string `json:"direct,omitempty"`
}

func (m *Manager) UpdatePeer(update PeerUpdate) (Status, error) {
	id := strings.TrimSpace(update.ID)
	if id == "" {
		return m.Status(), errors.New("缺少环境标识")
	}
	name := strings.TrimSpace(update.Name)
	if name == "" {
		return m.Status(), errors.New("请输入环境名称")
	}
	allowInsecure := m.allowInsecurePrivate()

	relay := strings.TrimSpace(update.Relay)
	if relay != "" {
		validated, err := ValidateRelayWithOptions(relay, allowInsecure)
		if err != nil {
			return m.Status(), err
		}
		relay = validated
	}

	var cleanedRelays []string
	seenRelays := map[string]bool{}
	if relay != "" {
		seenRelays[relay] = true
	}
	for _, raw := range update.Relays {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		validated, err := ValidateRelayWithOptions(trimmed, allowInsecure)
		if err != nil {
			return m.Status(), err
		}
		if relay == "" {
			relay = validated
			seenRelays[relay] = true
			continue
		}
		if !seenRelays[validated] {
			seenRelays[validated] = true
			cleanedRelays = append(cleanedRelays, validated)
		}
	}

	var cleanedDirect []string
	seenDirect := map[string]bool{}
	for _, raw := range update.Direct {
		addr := strings.TrimSpace(raw)
		if addr == "" {
			continue
		}
		host, portStr, err := net.SplitHostPort(addr)
		port, portErr := strconv.Atoi(portStr)
		if err != nil || strings.TrimSpace(host) == "" || portErr != nil || port < 1 || port > 65535 {
			return m.Status(), errors.New("直连地址格式无效（示例：192.168.31.118:52041）：" + addr)
		}
		if !seenDirect[addr] {
			seenDirect[addr] = true
			cleanedDirect = append(cleanedDirect, addr)
		}
	}

	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	idx := -1
	for i, peer := range m.cfg.Peers {
		if peer.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return m.statusLocked(), errors.New("找不到已配对环境")
	}

	prev := m.cfg.Peers[idx].Pair
	next := prev
	next.Name = name
	next.Relay = relay
	next.Relays = cleanedRelays
	next.Direct = cleanedDirect

	if next.Version == 3 || next.T3Token != "" {
		if relay == "" {
			return m.statusLocked(), errors.New("T3 环境需要保留服务地址")
		}
		u, _ := url.Parse(relay)
		if u != nil && u.Path != "" && u.Path != "/" {
			return m.statusLocked(), errors.New("T3 环境需要根路径地址")
		}
		next.T3URL = relay
	}
	if next.Relay == "" && len(next.Relays) == 0 && len(next.Direct) == 0 {
		return m.statusLocked(), errors.New("请至少保留一个中继地址或直连地址")
	}

	m.cfg.Peers[idx].Pair = next
	if err := saveConfig(m.opts.DataDir, m.cfg); err != nil {
		m.cfg.Peers[idx].Pair = prev
		return m.statusLocked(), err
	}
	delete(m.checks, id)
	return m.statusLocked(), nil
}

func (m *Manager) Remove(id string) (Status, error) {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, peer := range m.cfg.Peers {
		if peer.ID == id {
			m.cfg.Peers = append(m.cfg.Peers[:i:i], m.cfg.Peers[i+1:]...)
			return m.statusLocked(), saveConfig(m.opts.DataDir, m.cfg)
		}
	}
	return m.statusLocked(), errors.New("找不到已配对主机")
}

func (m *Manager) RevokeClient(id string) (Status, error) {
	if strings.HasPrefix(id, "t3:") && m.opts.T3RevokeClient != nil {
		err := m.opts.T3RevokeClient(strings.TrimPrefix(id, "t3:"))
		return m.Status(), err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, client := range m.cfg.Clients {
		if client.ID == id {
			prior := m.cfg.Clients
			m.cfg.Clients = append(append([]AccessClient(nil), prior[:i]...), prior[i+1:]...)
			if err := saveConfig(m.opts.DataDir, m.cfg); err != nil {
				m.cfg.Clients = prior
				return m.statusLocked(), err
			}
			m.cancelRequestsLocked(client.Secret)
			return m.statusLocked(), nil
		}
	}
	return m.statusLocked(), errors.New("找不到已授权设备")
}

func (m *Manager) Rotate(ctx context.Context) (Status, error) {
	m.mu.Lock()
	enabled := m.cfg.Enabled
	name, relay, key := m.cfg.Pair.Name, m.cfg.Pair.Relay, m.cfg.RegistrationKey
	revokeID, prevRelay, prevKey := m.stopLocked()
	m.cfg.Pair = Pair{Name: name, Relay: relay}
	m.cfg.PrivateKey = ""
	m.cfg.NextCertificate = ""
	m.cfg.NextPrivateKey = ""
	m.cfg.Clients = nil
	m.cfg.Enabled = false
	m.pendingCodes = map[string]*pendingPairing{}
	err := saveConfig(m.opts.DataDir, m.cfg)
	m.mu.Unlock()
	_ = revokeRemotePairing(prevRelay, prevKey, revokeID)
	if err == nil && m.opts.T3Revoke != nil {
		err = m.opts.T3Revoke()
	}
	if err != nil {
		return m.Status(), err
	}
	if enabled {
		return m.Start(ctx, name, relay, key)
	}
	return m.Status(), nil
}
