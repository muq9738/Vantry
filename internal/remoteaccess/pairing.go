package remoteaccess

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/connectivity"
)

type pendingPairing struct {
	ID         string
	Code       string
	ExpiresAt  time.Time
	RemoteID   string
	Status     string
	PairedWith string
}

type redeemWindow struct {
	start time.Time
	count int
}

func (m *Manager) configuredRelayURLsLocked() []string {
	var relays []string
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			for _, existing := range relays {
				if existing == raw {
					return
				}
			}
			relays = append(relays, raw)
		}
	}
	add(m.cfg.Pair.Relay)
	for _, ep := range m.cfg.Relays {
		add(ep.URL)
	}
	return relays
}

func (m *Manager) prunePendingLocked(now time.Time) {
	for code, item := range m.pendingCodes {
		if !now.Before(item.ExpiresAt.Add(5 * time.Minute)) {
			delete(m.pendingCodes, code)
		}
	}
}

func (m *Manager) allowRedeemAttempt(remoteAddr string) bool {
	ip := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		ip = host
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.redeemAttempts == nil {
		m.redeemAttempts = map[string]redeemWindow{}
	}
	now := time.Now()
	w, known := m.redeemAttempts[ip]
	if !known && len(m.redeemAttempts) >= 4096 {
		for k, item := range m.redeemAttempts {
			if now.Sub(item.start) > time.Minute {
				delete(m.redeemAttempts, k)
			}
		}
		if len(m.redeemAttempts) >= 4096 {
			return false
		}
	}
	if now.Sub(w.start) > time.Minute {
		w = redeemWindow{start: now}
	}
	w.count++
	m.redeemAttempts[ip] = w
	if len(m.redeemAttempts) > 4096 {
		for k, item := range m.redeemAttempts {
			if now.Sub(item.start) > time.Minute {
				delete(m.redeemAttempts, k)
			}
		}
	}
	return w.count <= 20
}

func (m *Manager) RefreshPairing(ctx context.Context) Status {
	m.mu.Lock()
	entry, relay, key := m.pairing, m.cfg.Pair.Relay, m.cfg.RegistrationKey
	if p := m.pendingCodes[connectivity.NormalizeCode(entry.Code)]; p != nil && p.RemoteID == "" {
		s := m.statusLocked()
		m.mu.Unlock()
		return s
	}
	if entry.Code != "" {
		if p := m.pendingCodes[connectivity.NormalizeCode(entry.Code)]; p != nil && p.Status == "paired" {
			m.pairing.Status = "paired"
			m.pairing.PairedWith = p.PairedWith
			s := m.statusLocked()
			m.mu.Unlock()
			return s
		}
	}
	m.mu.Unlock()
	if relay == "" || entry.ID == "" || entry.Status != "waiting" || !time.Now().Before(entry.ExpiresAt) {
		return m.Status()
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var current connectivity.Pairing
	err := connectivity.Request(probe, nil, "GET", relay, "/pairings?id="+entry.ID, key, nil, &current)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pairing.ID == entry.ID {
		if p := m.pendingCodes[connectivity.NormalizeCode(entry.Code)]; p != nil && p.Status == "paired" {
			m.pairing.Status = "paired"
			m.pairing.PairedWith = p.PairedWith
			m.failure = ""
		} else if err != nil {
			m.failure = err.Error()
		} else {
			m.pairing.Status = current.Status
			m.pairing.PairedWith = current.PairedWith
			m.failure = ""
		}
	}
	return m.statusLocked()
}

func revokeRemotePairing(relay, key, id string) error {
	if relay == "" || id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return connectivity.Request(ctx, nil, "POST", relay, "/pairings/revoke", key, map[string]string{"id": id}, nil)
}

func (m *Manager) GenerateCode(ctx context.Context) (Status, error) {
	m.mu.Lock()
	if m.server == nil {
		m.mu.Unlock()
		return m.Status(), errors.New("请先开启本机会话访问")
	}
	if len(m.cfg.Clients) >= 128 {
		m.mu.Unlock()
		return m.Status(), errors.New("请先撤销不再使用的会话设备")
	}
	generation := m.generation
	pair := m.cfg.Pair
	key := m.cfg.RegistrationKey
	publicURL := m.cfg.PublicURL
	relays := m.configuredRelayURLsLocked()
	nextCert := m.cfg.NextCertificate
	old := m.pairing
	oldRemoteID := old.ID
	if p := m.pendingCodes[connectivity.NormalizeCode(old.Code)]; p != nil {
		oldRemoteID = p.RemoteID
	}
	m.mu.Unlock()
	if publicURL != "" {
		if _, err := mobilePublicURL(publicURL); err != nil {
			return m.Status(), err
		}
	}

	// Never upload a long-lived secret to the relay. Only public routing and
	// identity verification material are placed in the bootstrap payload.
	bootstrap := Pair{
		Version:         2,
		Name:            pair.Name,
		Relay:           pair.Relay,
		Relays:          relays,
		Route:           pair.Route,
		DeviceID:        pair.DeviceID,
		Certificate:     pair.Certificate,
		NextCertificate: nextCert,
		Direct:          append([]string(nil), pair.Direct...),
	}
	payload, _ := json.Marshal(bootstrap)

	var entry connectivity.Pairing
	var remoteID string
	var err error
	if pair.Relay != "" {
		reqEntry := connectivity.Pairing{
			Scope:     connectivity.Session,
			Name:      pair.Name,
			ExpiresAt: time.Now().Add(5 * time.Minute),
			Payload:   payload,
		}
		requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = connectivity.Request(requestCtx, nil, "POST", pair.Relay, "/pairings", key, reqEntry, &entry)
		cancel()
		if err == nil {
			remoteID = entry.ID
		} else {
			if ctx.Err() != nil {
				return m.Status(), ctx.Err()
			}
			entry, err = m.pairings.Put(connectivity.Session, pair.Name, payload, 5*time.Minute, "local")
			if err != nil {
				return m.Status(), err
			}
		}
	} else {
		entry, err = m.pairings.Put(connectivity.Session, pair.Name, payload, 5*time.Minute, "local")
		if err != nil {
			return m.Status(), err
		}
	}

	bootstrap.Code = entry.Code
	bootstrap.ExpiresAt = entry.ExpiresAt

	rollbackRemote := func() {
		if remoteID != "" {
			_ = revokeRemotePairing(pair.Relay, key, remoteID)
		} else {
			m.pairings.Revoke(entry.ID, "local")
		}
	}

	var t3Link string
	if m.opts.T3Pairing != nil {
		mobileBase := publicURL
		if mobileBase == "" {
			if direct, ok := m.MobileRoutes()["direct"].([]string); ok && len(direct) > 0 {
				mobileBase = direct[0]
			}
		}
		if mobileBase == "" {
			rollbackRemote()
			return m.Status(), errors.New("No computer address is available")
		}
		t3Link, err = m.opts.T3Pairing(ctx, mobileBase)
		if err != nil {
			rollbackRemote()
			return m.Status(), err
		}
	}

	m.mu.Lock()
	if m.generation != generation || m.server == nil {
		m.mu.Unlock()
		rollbackRemote()
		if m.opts.T3CancelPairings != nil {
			m.opts.T3CancelPairings()
		}
		return m.Status(), context.Canceled
	}
	if len(m.cfg.Clients) >= 128 {
		m.mu.Unlock()
		rollbackRemote()
		if m.opts.T3CancelPairings != nil {
			m.opts.T3CancelPairings()
		}
		return m.Status(), errors.New("请先撤销不再使用的会话设备")
	}
	if m.pendingCodes == nil {
		m.pendingCodes = map[string]*pendingPairing{}
	}
	m.prunePendingLocked(time.Now())
	if old.Code != "" {
		if prev, ok := m.pendingCodes[connectivity.NormalizeCode(old.Code)]; ok && prev.Status == "waiting" {
			prev.Status = "revoked"
		}
	}
	m.pairings.Revoke(old.ID, "local")
	normCode := connectivity.NormalizeCode(entry.Code)
	m.pendingCodes[normCode] = &pendingPairing{
		ID:        entry.ID,
		Code:      normCode,
		ExpiresAt: entry.ExpiresAt,
		RemoteID:  remoteID,
		Status:    "waiting",
	}
	m.pairing = entry
	m.pairLink = bootstrap.Link()
	m.t3Link = t3Link
	m.revokePendingID = ""
	status := m.statusLocked()
	m.mu.Unlock()

	if oldRemoteID != "" && oldRemoteID != remoteID && pair.Relay != "" && old.Status == "waiting" {
		_ = revokeRemotePairing(pair.Relay, key, oldRemoteID)
	}
	return status, nil
}

func (m *Manager) RevokeCode(ctx context.Context) (Status, error) {
	if m.opts.T3CancelPairings != nil {
		m.opts.T3CancelPairings()
	}
	m.mu.Lock()
	entry := m.pairing
	targetID := entry.ID
	if targetID == "" {
		targetID = m.revokePendingID
	}
	relay, key := m.cfg.Pair.Relay, m.cfg.RegistrationKey
	m.pairings.Revoke(entry.ID, "local")
	if entry.Code != "" {
		if p := m.pendingCodes[connectivity.NormalizeCode(entry.Code)]; p != nil {
			p.Status = "revoked"
			targetID = p.RemoteID
		}
	}
	m.mu.Unlock()

	if relay != "" && targetID != "" {
		if err := connectivity.Request(ctx, nil, "POST", relay, "/pairings/revoke", key, map[string]string{"id": targetID}, nil); err != nil {
			m.mu.Lock()
			m.revokePendingID = targetID
			if m.pairing.ID == "" {
				m.pairing.ID = targetID
			}
			m.pairing.Status = "revoke_pending"
			m.failure = err.Error()
			s := m.statusLocked()
			m.mu.Unlock()
			return s, err
		}
	}

	m.mu.Lock()
	m.pairing = connectivity.Pairing{}
	m.pairLink = ""
	m.t3Link = ""
	m.revokePendingID = ""
	m.failure = ""
	s := m.statusLocked()
	m.mu.Unlock()
	return s, nil
}

func nonemptyClientName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "配对设备"
	}
	if len(name) > 128 {
		return name[:128]
	}
	return name
}

func (m *Manager) redeemLocal(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.allowRedeemAttempt(req.RemoteAddr) {
		http.Error(w, "请稍后重试", http.StatusTooManyRequests)
		return
	}
	var input struct {
		Code  string `json:"code"`
		Scope string `json:"scope"`
		Name  string `json:"name"`
	}
	if connectivity.Decode(w, req, &input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if input.Scope != "" && input.Scope != connectivity.Session {
		http.Error(w, "配对码用途不符：会话配对码与桌面共享码不能混用", http.StatusBadRequest)
		return
	}
	code := connectivity.NormalizeCode(input.Code)

	m.mu.Lock()
	m.prunePendingLocked(time.Now())
	pending := m.pendingCodes[code]
	if pending == nil || pending.Status != "waiting" || !time.Now().Before(pending.ExpiresAt) {
		m.mu.Unlock()
		http.Error(w, "配对码无效、已兑换或已过期，请重新生成", http.StatusBadRequest)
		return
	}
	if len(m.cfg.Clients) >= 128 {
		m.mu.Unlock()
		http.Error(w, "请先撤销不再使用的会话设备", http.StatusBadRequest)
		return
	}
	client := AccessClient{
		ID:        randomID(),
		Name:      nonemptyClientName(input.Name),
		CreatedAt: time.Now(),
		Secret:    randomID(),
	}
	nextCfg := m.cfg
	nextCfg.Clients = append(append([]AccessClient(nil), m.cfg.Clients...), client)
	if err := saveConfig(m.opts.DataDir, nextCfg); err != nil {
		m.mu.Unlock()
		http.Error(w, "保存授权设备失败", http.StatusInternalServerError)
		return
	}
	m.cfg = nextCfg
	pending.Status = "paired"
	pending.PairedWith = client.Name
	if m.pairing.ID == pending.ID {
		m.pairing.Status = "paired"
		m.pairing.PairedWith = client.Name
	}
	issuedPair := m.cfg.Pair
	issuedPair.Version = 1
	issuedPair.Secret = client.Secret
	issuedPair.Relays = m.configuredRelayURLsLocked()
	issuedPair.NextCertificate = m.cfg.NextCertificate
	issuedPair.Code = ""
	issuedPair.ExpiresAt = time.Time{}
	remoteID := pending.RemoteID
	relayURL := m.cfg.Pair.Relay
	regKey := m.cfg.RegistrationKey
	m.mu.Unlock()

	if relayURL != "" && remoteID != "" {
		go func() {
			_ = revokeRemotePairing(relayURL, regKey, remoteID)
		}()
	}

	payload, _ := json.Marshal(issuedPair)
	connectivity.JSON(w, http.StatusOK, connectivity.Pairing{
		ID:         pending.ID,
		Code:       pending.Code,
		Scope:      connectivity.Session,
		Name:       issuedPair.Name,
		ExpiresAt:  pending.ExpiresAt,
		Payload:    payload,
		Status:     "paired",
		PairedWith: client.Name,
	})
}

func redeemLink(ctx context.Context, pair Pair) (Pair, error) {
	return redeemLinkWithOptions(ctx, pair, Options{})
}

func redeemLinkWithOptions(ctx context.Context, expected Pair, opts Options) (Pair, error) {
	allowInsecure := opts.AllowInsecurePrivateHTTP || os.Getenv("VANTRY_ALLOW_INSECURE_LAN_RELAY") == "1"
	roots, err := certPoolFromPair(expected)
	if err != nil {
		return Pair{}, err
	}
	tlsConfig := &tls.Config{
		RootCAs:    roots,
		ServerName: "vantry.remote",
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2", "http/1.1"},
	}
	candidates := mergeDirectCandidates("", lookupNearbyDirect(opts.DataDir, expected.DeviceID), expected.Direct)
	relays := expected.relayList()

	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	defer dialCancel()

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

	established, _, _, dialErr := dialHostTLS(dialCtx, "auto", candidates, relays, expected.Route, secure)
	if dialErr != nil {
		return Pair{}, errors.New("无法兑换配对码，请确认主机在线、地址可达且配对码未过期")
	}

	var dialedOnce bool
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			if dialedOnce {
				return nil, errors.New("redeem transport single-use")
			}
			dialedOnce = true
			return established, nil
		},
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport:     transport,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	name, _ := os.Hostname()
	input := map[string]string{"scope": connectivity.Session, "code": expected.Code, "name": name}
	var entry connectivity.Pairing
	if err := connectivity.Request(dialCtx, client, http.MethodPost, "https://vantry.remote", "/pairings/redeem", "", input, &entry); err != nil {
		_ = established.Close()
		return Pair{}, errors.New("无法兑换配对码，请确认主机在线、地址可达且配对码未过期")
	}

	var result Pair
	if json.Unmarshal(entry.Payload, &result) != nil {
		return Pair{}, errors.New("配对资料无效")
	}
	validated, err := parseLinkWithOptions(result.Link(), allowInsecure)
	if err != nil {
		return Pair{}, err
	}
	if validated.Version != 1 || len(validated.Secret) < 32 {
		return Pair{}, errors.New("配对资料缺少有效设备凭据")
	}
	if validated.Route != expected.Route {
		return Pair{}, errors.New("对端返回的路由身份与配对链接不匹配")
	}
	if expected.DeviceID != "" && validated.DeviceID != expected.DeviceID {
		return Pair{}, errors.New("对端返回的设备身份与配对链接不匹配")
	}
	certMatched := validated.Certificate == expected.Certificate ||
		(expected.NextCertificate != "" && validated.Certificate == expected.NextCertificate)
	if !certMatched {
		return Pair{}, errors.New("对端返回的主机证书与配对链接不匹配")
	}
	return validated, nil
}
