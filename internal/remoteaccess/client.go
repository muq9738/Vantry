package remoteaccess

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/connectivity"
	"github.com/muq9738/Vantry/internal/t3compat"
	"github.com/muq9738/Vantry/pkg/api"
)

var (
	directStaggerDelay   = 100 * time.Millisecond
	directAttemptTimeout = 1500 * time.Millisecond
	autoRelayGraceDelay  = 1200 * time.Millisecond
	relayStaggerDelay    = 500 * time.Millisecond
	maxDirectCandidates  = 8
	maxDirectConcurrent  = 8
)

type ConnectError struct {
	Stage   string `json:"stage"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}

func (e *ConnectError) Error() string { return e.Message }
func (e *ConnectError) Unwrap() error { return e.Cause }

func classifyErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var connErr *ConnectError
	if errors.As(err, &connErr) && connErr.Code != "" {
		return connErr.Code
	}
	var invalidCert x509.CertificateInvalidError
	if errors.As(err, &invalidCert) {
		if invalidCert.Reason == x509.Expired {
			return "cert_expired"
		}
		return "cert_invalid"
	}
	var unknownAuth x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	if errors.As(err, &unknownAuth) || errors.As(err, &hostnameErr) {
		return "cert_mismatch"
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Code == api.Conflict {
		return "conflict"
	}
	var relayErr *connectivity.RelayError
	if errors.As(err, &relayErr) {
		switch relayErr.StatusCode {
		case 429:
			return "relay_busy"
		case 502, 503:
			return "host_offline"
		case 504:
			return "timeout"
		}
		return "relay_failed"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "refused"
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "401") || strings.Contains(lower, "unauthorized"):
		return "unauthorized"
	case strings.Contains(lower, "certificate has expired") || strings.Contains(lower, "已过期"):
		return "cert_expired"
	case strings.Contains(lower, "certificate signed by unknown authority") || strings.Contains(lower, "不匹配"):
		return "cert_mismatch"
	case strings.Contains(lower, "connection refused"):
		return "refused"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return "timeout"
	default:
		return "connect_failed"
	}
}

func friendlyConnectError(stage string, err error) *ConnectError {
	if err == nil {
		return nil
	}
	var existing *ConnectError
	if errors.As(err, &existing) {
		return existing
	}
	code := classifyErrorCode(err)
	var msg string
	switch code {
	case "cert_expired":
		msg = "主机证书已过期，请在主机端续期或重新配对"
	case "cert_mismatch", "cert_invalid":
		msg = "主机身份证书校验失败，请确认设备或重新配对"
	case "unauthorized":
		msg = "设备授权无效或已被主机撤销（401），请重新配对"
	case "relay_busy":
		msg = "中继连接繁忙或已达限流上限，请稍后重试"
	case "host_offline":
		msg = "目标主机离线或未在中继上线"
	case "dns":
		msg = "域名解析失败，请检查地址和网络"
	case "timeout":
		msg = "连接目标设备超时，请确认设备在线或网络可达"
	case "refused":
		msg = "目标端口拒绝连接，请确认主机已开启会话访问"
	default:
		msg = "无法连接设备，请确认主机已开启会话访问及中继入口正确"
	}
	return &ConnectError{Stage: stage, Code: code, Message: msg, Cause: err}
}

var (
	nearbySourcesMu sync.RWMutex
	nearbySources   = map[string]map[*Manager]struct{}{}
	configWriteMu   sync.Mutex
)

func normalizedDataDir(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Clean(dir)
}

func registerNearbySource(dir string, m *Manager) {
	key := normalizedDataDir(dir)
	if key == "" || m == nil {
		return
	}
	nearbySourcesMu.Lock()
	defer nearbySourcesMu.Unlock()
	if nearbySources[key] == nil {
		nearbySources[key] = map[*Manager]struct{}{}
	}
	nearbySources[key][m] = struct{}{}
}

func unregisterNearbySource(dir string, m *Manager) {
	key := normalizedDataDir(dir)
	if key == "" || m == nil {
		return
	}
	nearbySourcesMu.Lock()
	defer nearbySourcesMu.Unlock()
	set := nearbySources[key]
	if set == nil {
		return
	}
	delete(set, m)
	if len(set) == 0 {
		delete(nearbySources, key)
	}
}

func lookupNearbyDirect(dir, deviceID string) []string {
	key := normalizedDataDir(dir)
	if key == "" || deviceID == "" {
		return nil
	}
	nearbySourcesMu.RLock()
	var managers []*Manager
	for m := range nearbySources[key] {
		managers = append(managers, m)
	}
	nearbySourcesMu.RUnlock()
	for _, m := range managers {
		m.mu.Lock()
		device, ok := m.nearby[deviceID]
		m.mu.Unlock()
		if ok && len(device.Addresses) > 0 {
			return append([]string(nil), device.Addresses...)
		}
	}
	return nil
}

// Connection closes only its own requests; the remote Core owns the tasks.
type Connection struct {
	*local.Client
	Name        string
	Protocol    string
	mu          sync.Mutex
	mode        string
	lastDirect  string
	relayURL    string
	handshakeMs int
	rttMs       int
	lastError   string
	errorCode   string
}

func (c *Connection) Mode() string { c.mu.Lock(); defer c.mu.Unlock(); return c.mode }
func (c *Connection) HandshakeMs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handshakeMs
}
func (c *Connection) RTTMs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rttMs
}
func (c *Connection) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError
}
func (c *Connection) ErrorCode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errorCode
}

type dialOutcome struct {
	conn   net.Conn
	mode   string
	target string
	err    error
}

func mergeDirectCandidates(lastDirect string, groups ...[]string) []string {
	var out []string
	add := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" || slices.Contains(out, addr) || len(out) >= maxDirectCandidates {
			return
		}
		out = append(out, addr)
	}
	add(lastDirect)
	for _, group := range groups {
		for _, addr := range group {
			add(addr)
		}
	}
	return out
}

func dialDirectRace(ctx context.Context, addresses []string, secure func(context.Context, net.Conn) (net.Conn, error)) (net.Conn, string, error) {
	if len(addresses) == 0 {
		return nil, "", &ConnectError{Stage: "direct_dial", Code: "no_addresses", Message: "无可用直连地址"}
	}
	if len(addresses) > maxDirectCandidates {
		addresses = addresses[:maxDirectCandidates]
	}
	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	results := make(chan dialOutcome, len(addresses))
	sem := make(chan struct{}, maxDirectConcurrent)
	var wg sync.WaitGroup
	launched := make(chan struct{})

	go func() {
		defer close(launched)
		for i, addr := range addresses {
			if i > 0 {
				timer := time.NewTimer(directStaggerDelay)
				select {
				case <-timer.C:
				case <-raceCtx.Done():
					timer.Stop()
					return
				}
			}
			select {
			case sem <- struct{}{}:
			case <-raceCtx.Done():
				return
			}
			wg.Add(1)
			go func(address string) {
				defer wg.Done()
				defer func() { <-sem }()
				probe, cancel := context.WithTimeout(raceCtx, directAttemptTimeout)
				defer cancel()
				raw, err := (&net.Dialer{}).DialContext(probe, "tcp", address)
				var conn net.Conn
				if err == nil {
					conn, err = secure(probe, raw)
				}
				if err != nil {
					results <- dialOutcome{mode: "direct", target: address, err: err}
					return
				}
				select {
				case results <- dialOutcome{conn: conn, mode: "direct", target: address}:
				case <-raceCtx.Done():
					_ = conn.Close()
				}
			}(addr)
		}
	}()

	go func() {
		<-launched
		wg.Wait()
		close(results)
	}()

	var lastErr error
	for res := range results {
		if res.conn != nil {
			raceCancel()
			go func() {
				for lost := range results {
					if lost.conn != nil {
						_ = lost.conn.Close()
					}
				}
			}()
			return res.conn, res.target, nil
		}
		if res.err != nil {
			lastErr = res.err
		}
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	if lastErr == nil {
		lastErr = errors.New("无法直连设备")
	}
	return nil, "", lastErr
}

func dialRelayRace(ctx context.Context, relays []string, route string, secure func(context.Context, net.Conn) (net.Conn, error)) (net.Conn, string, error) {
	if len(relays) == 0 {
		return nil, "", &ConnectError{Stage: "relay_dial", Code: "no_relays", Message: "未配置中继地址"}
	}
	if len(relays) == 1 {
		raw, err := connectivity.DialTunnel(ctx, relays[0], route)
		if err != nil {
			return nil, relays[0], err
		}
		conn, err := secure(ctx, raw)
		if err != nil {
			return nil, relays[0], err
		}
		return conn, relays[0], nil
	}
	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()
	results := make(chan dialOutcome, len(relays))
	var wg sync.WaitGroup
	launched := make(chan struct{})
	go func() {
		defer close(launched)
		for i, relayURL := range relays {
			if i > 0 {
				timer := time.NewTimer(relayStaggerDelay)
				select {
				case <-timer.C:
				case <-raceCtx.Done():
					timer.Stop()
					return
				}
			}
			wg.Add(1)
			go func(base string) {
				defer wg.Done()
				raw, err := connectivity.DialTunnel(raceCtx, base, route)
				var conn net.Conn
				if err == nil {
					conn, err = secure(raceCtx, raw)
				}
				if err != nil {
					results <- dialOutcome{mode: "relay", target: base, err: err}
					return
				}
				select {
				case results <- dialOutcome{conn: conn, mode: "relay", target: base}:
				case <-raceCtx.Done():
					_ = conn.Close()
				}
			}(relayURL)
		}
	}()
	go func() {
		<-launched
		wg.Wait()
		close(results)
	}()
	var lastErr error
	for res := range results {
		if res.conn != nil {
			raceCancel()
			go func() {
				for lost := range results {
					if lost.conn != nil {
						_ = lost.conn.Close()
					}
				}
			}()
			return res.conn, res.target, nil
		}
		if res.err != nil {
			lastErr = res.err
		}
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return nil, "", lastErr
}

func dialHostTLS(ctx context.Context, mode string, addresses, relays []string, route string, secure func(context.Context, net.Conn) (net.Conn, error)) (net.Conn, string, string, error) {
	switch mode {
	case "direct":
		conn, target, err := dialDirectRace(ctx, addresses, secure)
		return conn, "direct", target, err
	case "relay":
		conn, target, err := dialRelayRace(ctx, relays, route, secure)
		return conn, "relay", target, err
	}
	if len(relays) == 0 {
		conn, target, err := dialDirectRace(ctx, addresses, secure)
		return conn, "direct", target, err
	}
	if len(addresses) == 0 {
		conn, target, err := dialRelayRace(ctx, relays, route, secure)
		return conn, "relay", target, err
	}

	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	directCh := make(chan dialOutcome, 1)
	go func() {
		conn, target, err := dialDirectRace(raceCtx, addresses, secure)
		directCh <- dialOutcome{conn: conn, mode: "direct", target: target, err: err}
	}()

	grace := time.NewTimer(autoRelayGraceDelay)
	var directErr error
	directDone := false

	select {
	case res := <-directCh:
		grace.Stop()
		if res.conn != nil {
			raceCancel()
			return res.conn, "direct", res.target, nil
		}
		directErr = res.err
		directDone = true
	case <-grace.C:
	case <-ctx.Done():
		grace.Stop()
		raceCancel()
		go func() {
			if d := <-directCh; d.conn != nil {
				_ = d.conn.Close()
			}
		}()
		return nil, "", "", ctx.Err()
	}

	relayCh := make(chan dialOutcome, 1)
	go func() {
		conn, target, err := dialRelayRace(raceCtx, relays, route, secure)
		relayCh <- dialOutcome{conn: conn, mode: "relay", target: target, err: err}
	}()

	if directDone {
		res := <-relayCh
		if res.conn != nil {
			raceCancel()
			return res.conn, "relay", res.target, nil
		}
		if res.err != nil {
			return nil, "", "", res.err
		}
		return nil, "", "", directErr
	}

	var relayErr error
	relayDone := false
	for !directDone || !relayDone {
		select {
		case res := <-directCh:
			directDone = true
			if res.conn != nil {
				raceCancel()
				if !relayDone {
					go func() {
						if r := <-relayCh; r.conn != nil {
							_ = r.conn.Close()
						}
					}()
				}
				return res.conn, "direct", res.target, nil
			}
			directErr = res.err
		case res := <-relayCh:
			relayDone = true
			if res.conn != nil {
				raceCancel()
				if !directDone {
					go func() {
						if d := <-directCh; d.conn != nil {
							_ = d.conn.Close()
						}
					}()
				}
				return res.conn, "relay", res.target, nil
			}
			relayErr = res.err
		case <-ctx.Done():
			raceCancel()
			if !directDone {
				go func() {
					if d := <-directCh; d.conn != nil {
						_ = d.conn.Close()
					}
				}()
			}
			if !relayDone {
				go func() {
					if r := <-relayCh; r.conn != nil {
						_ = r.conn.Close()
					}
				}()
			}
			return nil, "", "", ctx.Err()
		}
	}
	if relayErr != nil {
		return nil, "", "", relayErr
	}
	return nil, "", "", directErr
}

func Connect(ctx context.Context, opts Options, id string) (*Connection, error) {
	return ConnectWithMode(ctx, opts, id, "auto")
}

func findPeer(cfg configuration, id string) (Pair, bool) {
	for _, peer := range cfg.Peers {
		if peer.ID == id {
			return peer.Pair, true
		}
	}
	return Pair{}, false
}

func ConnectWithMode(ctx context.Context, opts Options, id, mode string) (*Connection, error) {
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "direct" && mode != "relay" {
		return nil, errors.New("连接方式无效")
	}
	cfg, err := loadConfig(opts.DataDir)
	if err != nil {
		return nil, err
	}
	pair, ok := findPeer(cfg, id)
	if !ok || pair.Version == 0 {
		return nil, errors.New("找不到配对设备，请重新添加配对码或链接")
	}
	if pair.Version == 3 {
		adapter := &t3compat.RemoteAdapter{Remote: t3compat.Remote{URL: pair.T3URL, Token: pair.T3Token, EnvironmentID: pair.DeviceID, Name: pair.Name}}
		connection := &Connection{Client: local.DialAdapter(adapter), Name: pair.Name, Protocol: "t3", mode: "t3"}
		start := time.Now()
		if _, err := connection.Health(ctx); err != nil {
			_ = connection.Close()
			return nil, friendlyConnectError("t3_health", err)
		}
		connection.mu.Lock()
		connection.rttMs = int(time.Since(start).Milliseconds())
		connection.mu.Unlock()
		return connection, nil
	}
	if _, err := certPoolFromPair(pair); err != nil {
		return nil, err
	}
	connection := &Connection{Name: pair.Name}
	sessionCache := tls.NewLRUClientSessionCache(8)
	var dialErrMu sync.Mutex
	var lastDialErr error

	connection.Client = local.DialNetwork(func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		dialStart := time.Now()
		currentPair := pair
		if latestCfg, loadErr := loadConfig(opts.DataDir); loadErr == nil {
			if latest, found := findPeer(latestCfg, id); found && latest.Version != 0 {
				currentPair = latest
			}
		}
		roots, poolErr := certPoolFromPair(currentPair)
		if poolErr != nil {
			return nil, poolErr
		}
		tlsConfig := &tls.Config{
			RootCAs:            roots,
			ServerName:         "vantry.remote",
			MinVersion:         tls.VersionTLS13,
			NextProtos:         []string{"h2", "http/1.1"},
			ClientSessionCache: sessionCache,
		}
		secure := func(sCtx context.Context, raw net.Conn) (net.Conn, error) {
			conn := tls.Client(raw, tlsConfig)
			life, cancel := context.WithTimeout(sCtx, 12*time.Second)
			defer cancel()
			if err := conn.HandshakeContext(life); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return conn, nil
		}
		connection.mu.Lock()
		prevDirect := connection.lastDirect
		connection.mu.Unlock()
		candidates := mergeDirectCandidates(prevDirect, lookupNearbyDirect(opts.DataDir, currentPair.DeviceID), currentPair.Direct)
		conn, chosenMode, target, err := dialHostTLS(dialCtx, mode, candidates, currentPair.relayList(), currentPair.Route, secure)
		if err != nil {
			dialErrMu.Lock()
			lastDialErr = err
			dialErrMu.Unlock()
			classified := friendlyConnectError("dial", err)
			connection.mu.Lock()
			connection.lastError = classified.Message
			connection.errorCode = classified.Code
			connection.mu.Unlock()
			return nil, classified
		}
		handshakeMs := int(time.Since(dialStart).Milliseconds())
		connection.mu.Lock()
		connection.mode = chosenMode
		connection.handshakeMs = handshakeMs
		connection.lastError = ""
		connection.errorCode = ""
		if chosenMode == "direct" {
			connection.lastDirect = target
			connection.relayURL = ""
		} else {
			connection.relayURL = target
		}
		connection.mu.Unlock()
		return conn, nil
	}, pair.Secret)

	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	healthStart := time.Now()
	if _, err := connection.Health(probe); err != nil {
		_ = connection.Close()
		dialErrMu.Lock()
		dErr := lastDialErr
		dialErrMu.Unlock()
		if dErr != nil && classifyErrorCode(err) == "connect_failed" {
			err = dErr
		}
		return nil, friendlyConnectError("health", err)
	}
	totalMs := int(time.Since(healthStart).Milliseconds())
	connection.mu.Lock()
	connection.rttMs = max(0, totalMs-connection.handshakeMs)
	connection.mu.Unlock()

	syncPeerIdentity(ctx, opts.DataDir, id, connection)
	return connection, nil
}

type identitySnapshot struct {
	DeviceID        string    `json:"deviceId"`
	Certificate     string    `json:"certificate"`
	NextCertificate string    `json:"nextCertificate,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt"`
	Direct          []string  `json:"direct,omitempty"`
	Relays          []string  `json:"relays,omitempty"`
}

func syncPeerIdentity(ctx context.Context, dataDir, peerID string, conn *Connection) {
	if dataDir == "" || conn == nil || conn.Client == nil {
		return
	}
	syncCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var snap identitySnapshot
	if err := conn.GetJSON(syncCtx, "/v1/access/identity", &snap); err != nil {
		return
	}
	if _, err := parseAndValidateCert(snap.Certificate, false); err != nil {
		return
	}
	if snap.NextCertificate != "" {
		if _, err := parseAndValidateCert(snap.NextCertificate, false); err != nil {
			snap.NextCertificate = ""
		}
	}
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	cfg, err := loadConfig(dataDir)
	if err != nil {
		return
	}
	changed := false
	for i, peer := range cfg.Peers {
		if peer.ID != peerID {
			continue
		}
		if peer.Pair.DeviceID != "" && snap.DeviceID != "" && peer.Pair.DeviceID != snap.DeviceID {
			return
		}
		// Only accept an active certificate update if it matches the existing cert
		// or the previously authenticated NextCertificate staged inside TLS.
		if snap.Certificate != peer.Pair.Certificate {
			if peer.Pair.NextCertificate != "" && snap.Certificate == peer.Pair.NextCertificate {
				cfg.Peers[i].Pair.Certificate = snap.Certificate
				changed = true
			}
		}
		if snap.NextCertificate != peer.Pair.NextCertificate {
			cfg.Peers[i].Pair.NextCertificate = snap.NextCertificate
			changed = true
		}
		if len(snap.Direct) > 0 && !slices.Equal(cfg.Peers[i].Pair.Direct, snap.Direct) {
			cfg.Peers[i].Pair.Direct = append([]string(nil), snap.Direct...)
			changed = true
		}
		if len(snap.Relays) > 0 && !slices.Equal(cfg.Peers[i].Pair.Relays, snap.Relays) {
			cfg.Peers[i].Pair.Relays = append([]string(nil), snap.Relays...)
			changed = true
		}
		break
	}
	if changed {
		_ = saveConfig(dataDir, cfg)
	}
}
