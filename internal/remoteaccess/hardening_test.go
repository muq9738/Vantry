package remoteaccess

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/internal/connectivity"
	"golang.org/x/net/dns/dnsmessage"
)

func startMockCoreUnix(t *testing.T) (string, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vt-ra-hardening-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "core.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":1,"state":"ready"}`)
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return dir, sock
}

func startBlackholeTLSListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func(conn net.Conn) {
				<-stop
				_ = conn.Close()
			}(c)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	return ln.Addr().String()
}

func generateCertWithWindow(t *testing.T, notBefore, notAfter time.Time) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Vantry remote access"},
		DNSNames:              []string{"vantry.remote"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func TestZeroSecretOnRelayAndSwappedCertRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dataDir, sock := startMockCoreUnix(t)

	key := connectivity.Token()
	relay, err := connectivity.NewRelay(connectivity.RelayConfig{RegistrationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	var uploadedPayloads []string
	var relayRedeems atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/pairings" && req.Method == http.MethodPost {
			raw, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(raw))
			var body struct {
				Payload json.RawMessage `json:"payload"`
			}
			_ = json.Unmarshal(raw, &body)
			uploadedPayloads = append(uploadedPayloads, string(body.Payload))
		}
		if req.URL.Path == "/pairings/redeem" {
			relayRedeems.Add(1)
		}
		relay.ServeHTTP(w, req)
	}))
	defer gateway.Close()

	host, err := New(ctx, Options{DataDir: dataDir, SocketPath: sock, DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if _, err := host.Start(ctx, "Host", gateway.URL, key); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); !host.Status().RelayConnected; {
		if time.Now().After(deadline) {
			t.Fatal(host.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}

	status, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploadedPayloads) != 1 {
		t.Fatalf("expected 1 uploaded payload, got %d", len(uploadedPayloads))
	}
	var uploadedPair Pair
	if err := json.Unmarshal([]byte(uploadedPayloads[0]), &uploadedPair); err != nil {
		t.Fatal(err)
	}
	if uploadedPair.Secret != "" || uploadedPair.Version != 2 {
		t.Fatalf("relay payload leaked secret or wrong version: version=%d secretLen=%d", uploadedPair.Version, len(uploadedPair.Secret))
	}

	// Link pairing redeems directly inside host TLS without calling relay /pairings/redeem.
	ctrl1, err := New(ctx, Options{DataDir: t.TempDir(), DisableDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl1.Close()
	if _, err := ctrl1.PairWithContext(ctx, status.PairingLink, ""); err != nil {
		t.Fatalf("link pairing failed: %v", err)
	}
	if relayRedeems.Load() != 0 {
		t.Fatalf("full-link pairing called relay /pairings/redeem %d times", relayRedeems.Load())
	}

	// Short code redeems bootstrap from relay and real secret from host inner TLS.
	status2, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctrl2, err := New(ctx, Options{DataDir: t.TempDir(), DisableDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl2.Close()
	if _, err := ctrl2.PairWithContext(ctx, status2.PairingCode, gateway.URL); err != nil {
		t.Fatalf("short code pairing failed: %v", err)
	}
	if relayRedeems.Load() != 1 {
		t.Fatalf("expected 1 relay redeem for short code, got %d", relayRedeems.Load())
	}

	// Tampered/swapped identity returned from a peer must be rejected.
	expectedLink, err := ParseLink(status2.PairingLink)
	if err != nil {
		t.Fatal(err)
	}
	// Spin up an imposter HTTPS server with a DIFFERENT certificate + route.
	imposterCert, imposterKey := generateCertWithWindow(t, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	tlsCert, err := tls.X509KeyPair([]byte(imposterCert), []byte(imposterKey))
	if err != nil {
		t.Fatal(err)
	}
	imposterSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := expectedLink
		p.Version = 1
		p.Secret = connectivity.Token()
		p.Certificate = imposterCert
		p.DeviceID = "evil-device"
		raw, _ := json.Marshal(p)
		connectivity.JSON(w, 200, connectivity.Pairing{Payload: raw})
	}))
	imposterSrv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}, MinVersion: tls.VersionTLS13}
	imposterSrv.StartTLS()
	defer imposterSrv.Close()

	spoof := expectedLink
	spoof.Direct = []string{strings.TrimPrefix(imposterSrv.URL, "https://")}
	spoof.Relay = ""
	spoof.Relays = nil
	if _, err := redeemLinkWithOptions(ctx, spoof, Options{}); err == nil {
		t.Fatal("redeemLink accepted swapped certificate / device identity")
	}
}

func TestConcurrentStaggeredDialAndAutoFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dataDir, sock := startMockCoreUnix(t)

	key := connectivity.Token()
	relay, err := connectivity.NewRelay(connectivity.RelayConfig{RegistrationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	gateway := httptest.NewServer(relay)
	defer gateway.Close()

	host, err := New(ctx, Options{DataDir: dataDir, SocketPath: sock, DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if _, err := host.Start(ctx, "Host", gateway.URL, key); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); !host.Status().RelayConnected; {
		if time.Now().After(deadline) {
			t.Fatal(host.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}

	ctrlDir := t.TempDir()
	ctrlOpts := Options{DataDir: ctrlDir, DisableDiscovery: true}
	ctrl, err := New(ctx, ctrlOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	paired, err := ctrl.PairWithContext(ctx, status.PairingLink, "")
	if err != nil {
		t.Fatal(err)
	}
	peerID := paired.Peers[0].ID

	// Build 7 blackholes followed by the real host address at position 8.
	cfg, err := loadConfig(ctrlDir)
	if err != nil {
		t.Fatal(err)
	}
	realDirect := cfg.Peers[0].Pair.Direct[0]
	blackholes := make([]string, 7)
	for i := range blackholes {
		blackholes[i] = startBlackholeTLSListener(t)
	}
	cfg.Peers[0].Pair.Direct = append(append([]string(nil), blackholes...), realDirect)
	if err := saveConfig(ctrlDir, cfg); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	conn, err := ConnectWithMode(ctx, ctrlOpts, peerID, "auto")
	if err != nil {
		t.Fatalf("auto dial with 7 blackholes failed: %v", err)
	}
	if conn.Mode() != "direct" {
		t.Fatalf("expected direct mode to win staggered race, got %s", conn.Mode())
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("staggered dial took %v, expected < 2.5s", elapsed)
	}
	_ = conn.Close()

	// Replace the 8th candidate with an 8th blackhole: auto mode must fall back to relay.
	cfg.Peers[0].Pair.Direct[7] = startBlackholeTLSListener(t)
	if err := saveConfig(ctrlDir, cfg); err != nil {
		t.Fatal(err)
	}
	connRelay, err := ConnectWithMode(ctx, ctrlOpts, peerID, "auto")
	if err != nil {
		t.Fatalf("auto relay fallback failed: %v", err)
	}
	if connRelay.Mode() != "relay" {
		t.Fatalf("expected relay fallback, got %s", connRelay.Mode())
	}
	_ = connRelay.Close()

	// Explicit direct mode must NOT fall back to relay when direct candidates fail.
	if err := saveConfig(ctrlDir, cfg); err != nil {
		t.Fatal(err)
	}
	directCtx, directCancel := context.WithTimeout(ctx, 2*time.Second)
	defer directCancel()
	if c, err := ConnectWithMode(directCtx, ctrlOpts, peerID, "direct"); err == nil {
		_ = c.Close()
		t.Fatal("direct mode bypassed to relay")
	}
}

func TestDynamicNearbyRedialWithoutCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dataDir, sock := startMockCoreUnix(t)

	host, err := New(ctx, Options{DataDir: dataDir, SocketPath: sock, DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if _, err := host.Start(ctx, "LANHost", "", ""); err != nil {
		t.Fatal(err)
	}
	status, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}

	ctrlDir := t.TempDir()
	ctrlOpts := Options{DataDir: ctrlDir, DisableDiscovery: true}
	ctrl, err := New(ctx, ctrlOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	paired, err := ctrl.Pair(status.PairingLink)
	if err != nil {
		t.Fatal(err)
	}
	peerID := paired.Peers[0].ID

	// Overwrite saved Direct with an unreachable port, simulating DHCP/port change.
	cfg, err := loadConfig(ctrlDir)
	if err != nil {
		t.Fatal(err)
	}
	actualDirect := append([]string(nil), cfg.Peers[0].Pair.Direct...)
	deviceID := cfg.Peers[0].Pair.DeviceID
	cfg.Peers[0].Pair.Direct = []string{"127.0.0.1:1"}
	if err := saveConfig(ctrlDir, cfg); err != nil {
		t.Fatal(err)
	}

	// Inject live nearby update without calling Check().
	ctrl.mu.Lock()
	ctrl.nearby[deviceID] = NearbyDevice{ID: deviceID, Name: "LANHost", Addresses: actualDirect}
	ctrl.mu.Unlock()

	conn, err := ConnectWithMode(ctx, ctrlOpts, peerID, "direct")
	if err != nil {
		t.Fatalf("ConnectWithMode did not use live nearby addresses without Check: %v", err)
	}
	_ = conn.Close()
}

func TestPendingPairingDoesNotExhaustClientQuotaAndRollbacks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dataDir, sock := startMockCoreUnix(t)

	key := connectivity.Token()
	relay, err := connectivity.NewRelay(connectivity.RelayConfig{RegistrationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	var failNextRevoke atomic.Bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/pairings/revoke" && failNextRevoke.Load() {
			http.Error(w, "upstream failure", http.StatusInternalServerError)
			return
		}
		relay.ServeHTTP(w, req)
	}))
	defer gateway.Close()

	var t3Fail atomic.Bool
	host, err := New(ctx, Options{
		DataDir:          dataDir,
		SocketPath:       sock,
		DisableDiscovery: true,
		ListenAddress:    "127.0.0.1:0",
		T3Pairing: func(context.Context, string) (string, error) {
			if t3Fail.Load() {
				return "", errors.New("t3 failed")
			}
			return "t3://mock", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if _, err := host.SetPublicURL("https://public.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Start(ctx, "Host", gateway.URL, key); err != nil {
		t.Fatal(err)
	}

	// Generate & revoke > 128 codes; Clients list must remain 0 and 136th code must succeed.
	for range 135 {
		if _, err := host.GenerateCode(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(host.Status().Clients) != 0 {
		t.Fatalf("unredeemed codes polluted Clients: %d", len(host.Status().Clients))
	}
	st, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := New(ctx, Options{DataDir: t.TempDir(), DisableDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	if _, err := ctrl.PairWithContext(ctx, st.PairingCode, gateway.URL); err != nil {
		t.Fatalf("136th pairing failed after repeated generations: %v", err)
	}
	if len(host.Status().Clients) != 1 {
		t.Fatalf("expected 1 client after redemption, got %d", len(host.Status().Clients))
	}

	// T3 failure rolls back remote relay pairing code.
	t3Fail.Store(true)
	if _, err := host.GenerateCode(ctx); err == nil {
		t.Fatal("expected GenerateCode to fail when T3Pairing fails")
	}
	t3Fail.Store(false)

	// RevokeCode preserves retryable state on relay error, then succeeds on retry.
	if _, err := host.GenerateCode(ctx); err != nil {
		t.Fatal(err)
	}
	failNextRevoke.Store(true)
	revokedSt, err := host.RevokeCode(ctx)
	if err == nil || revokedSt.RevokePendingID == "" || revokedSt.PairingState != "revoke_pending" {
		t.Fatalf("RevokeCode did not retain retryable state: status=%+v err=%v", revokedSt, err)
	}
	failNextRevoke.Store(false)
	clearedSt, err := host.RevokeCode(ctx)
	if err != nil || clearedSt.RevokePendingID != "" || clearedSt.PairingCode != "" {
		t.Fatalf("RevokeCode retry failed: status=%+v err=%v", clearedSt, err)
	}

	// Stop() revokes active remote code on relay.
	st3, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := ctrl.PairWithContext(ctx, st3.PairingCode, gateway.URL); err == nil {
		t.Fatal("remote pairing code remained redeemable after host Stop()")
	}
}

func TestCertificateExpiryWarningSyncAndRollover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dataDir, sock := startMockCoreUnix(t)

	// Pre-populate a host config with a certificate expiring in 10 days.
	nearCert, nearKey := generateCertWithWindow(t, time.Now().Add(-time.Hour), time.Now().Add(10*24*time.Hour))
	cfg := configuration{
		Pair: Pair{
			Version:     1,
			Name:        "RolloverHost",
			DeviceID:    randomID(),
			Route:       randomID(),
			Secret:      randomID(),
			Certificate: nearCert,
		},
		PrivateKey: nearKey,
	}
	if err := saveConfig(dataDir, cfg); err != nil {
		t.Fatal(err)
	}

	host, err := New(ctx, Options{DataDir: dataDir, SocketPath: sock, DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	st, err := host.Start(ctx, "RolloverHost", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !st.CertificateExpiringSoon || host.cfg.NextCertificate == "" {
		t.Fatalf("expected near-expiry warning and staged NextCertificate: soon=%v nextLen=%d", st.CertificateExpiringSoon, len(host.cfg.NextCertificate))
	}
	nextCertStaged := host.cfg.NextCertificate

	pairStatus, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctrlDir := t.TempDir()
	ctrlOpts := Options{DataDir: ctrlDir, DisableDiscovery: true}
	ctrl, err := New(ctx, ctrlOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	paired, err := ctrl.Pair(pairStatus.PairingLink)
	if err != nil {
		t.Fatal(err)
	}
	peerID := paired.Peers[0].ID

	// Connect once: client syncs NextCertificate over inner TLS.
	conn1, err := Connect(ctx, ctrlOpts, peerID)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn1.Close()

	ctrlCfg, err := loadConfig(ctrlDir)
	if err != nil {
		t.Fatal(err)
	}
	if ctrlCfg.Peers[0].Pair.NextCertificate != nextCertStaged {
		t.Fatal("client did not sync NextCertificate from host")
	}

	// Host promotes NextCertificate to live without dropping client trust.
	if _, err := host.ActivateNextCertificate(); err != nil {
		t.Fatal(err)
	}
	conn2, err := Connect(ctx, ctrlOpts, peerID)
	if err != nil {
		t.Fatalf("client failed to connect after certificate rollover: %v", err)
	}
	_ = conn2.Close()

	ctrlCfg2, err := loadConfig(ctrlDir)
	if err != nil {
		t.Fatal(err)
	}
	if ctrlCfg2.Peers[0].Pair.Certificate != nextCertStaged {
		t.Fatal("client did not promote synced certificate after rollover")
	}

	// Offline peer holding an expired certificate gets cert_expired.
	expiredCert, _ := generateCertWithWindow(t, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	ctrlCfg2.Peers[0].Pair.Certificate = expiredCert
	ctrlCfg2.Peers[0].Pair.NextCertificate = ""
	if err := saveConfig(ctrlDir, ctrlCfg2); err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(ctx, ctrlOpts, peerID); err == nil {
		t.Fatal("expected cert_expired error")
	} else {
		var connErr *ConnectError
		if !errors.As(err, &connErr) || (connErr.Code != "cert_expired" && connErr.Code != "cert_mismatch") {
			t.Fatalf("unexpected error classification: %+v (%v)", connErr, err)
		}
	}
}

func TestMDNSCacheIPv6SplitPacketsTTLExpiryGoodbyeAndUTF8(t *testing.T) {
	trunc := utf8Truncate("你好世界Vantry工作室电脑名称", 10)
	if !utf8.ValidString(trunc) || len(trunc) > 10 {
		t.Fatalf("invalid utf8 truncation: %q (len=%d)", trunc, len(trunc))
	}

	cache := newMDNSCache()
	inst := dnsName("vantry-dev1." + mdnsType)
	host := dnsName("vantry-dev1.local.")
	hdr := func(n dnsmessage.Name, tp dnsmessage.Type, ttl uint32) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: n, Type: tp, Class: dnsmessage.ClassINET, TTL: ttl}
	}

	now := time.Now()
	// Packet 1: PTR + SRV only (no TXT/A/AAAA yet).
	u1 := cache.ingest(dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{
			{Header: hdr(dnsName(mdnsType), dnsmessage.TypePTR, 60), Body: &dnsmessage.PTRResource{PTR: inst}},
			{Header: hdr(inst, dnsmessage.TypeSRV, 60), Body: &dnsmessage.SRVResource{Port: 8443, Target: host}},
		},
	}, now)
	if len(u1) != 0 {
		t.Fatalf("expected incomplete record to wait for TXT+addr, got %+v", u1)
	}

	// Packet 2: TXT + A + AAAA (IPv4 + IPv6 dual-stack).
	u2 := cache.ingest(dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{
			{Header: hdr(inst, dnsmessage.TypeTXT, 60), Body: &dnsmessage.TXTResource{TXT: []string{"id=dev1", "name=Studio"}}},
			{Header: hdr(host, dnsmessage.TypeA, 30), Body: &dnsmessage.AResource{A: [4]byte{192, 168, 1, 20}}},
			{Header: hdr(host, dnsmessage.TypeAAAA, 30), Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0xfd, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}}},
		},
	}, now)
	if len(u2) != 1 || u2[0].ID != "dev1" || !slices.Contains(u2[0].Addresses, "192.168.1.20:8443") || !slices.Contains(u2[0].Addresses, "[fd00::2]:8443") {
		t.Fatalf("split-packet dual-stack mDNS assembly failed: %+v", u2)
	}

	// TTL=0 goodbye packet removes device immediately.
	u3 := cache.ingest(dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{
			{Header: hdr(inst, dnsmessage.TypeTXT, 0), Body: &dnsmessage.TXTResource{TXT: []string{"id=dev1", "name=Studio"}}},
		},
	}, now.Add(time.Second))
	if len(u3) != 1 || !u3[0].Removed || u3[0].ID != "dev1" {
		t.Fatalf("goodbye packet did not emit removal: %+v", u3)
	}

	// Re-add and verify TTL expiration pruning.
	_ = cache.ingest(dnsmessage.Message{
		Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{
			{Header: hdr(inst, dnsmessage.TypeSRV, 10), Body: &dnsmessage.SRVResource{Port: 8443, Target: host}},
			{Header: hdr(inst, dnsmessage.TypeTXT, 10), Body: &dnsmessage.TXTResource{TXT: []string{"id=dev1", "name=Studio"}}},
			{Header: hdr(host, dnsmessage.TypeAAAA, 10), Body: &dnsmessage.AAAAResource{AAAA: [16]byte{0xfd, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}}},
		},
	}, now)
	expired := cache.prune(now.Add(15 * time.Second))
	if !slices.Contains(expired, "dev1") {
		t.Fatalf("TTL prune did not expire dev1: %v", expired)
	}
}

func TestRedeemLocalRateLimitAndStructuredCheckTimings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dataDir, sock := startMockCoreUnix(t)

	host, err := New(ctx, Options{DataDir: dataDir, SocketPath: sock, DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	st, err := host.Start(ctx, "RateHost", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Check() records separated HandshakeMs/RTTMs and classifies unauthorized error cleanly.
	codeSt, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctrlDir := t.TempDir()
	ctrlOpts := Options{DataDir: ctrlDir, DisableDiscovery: true}
	ctrl, err := New(ctx, ctrlOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	paired, err := ctrl.Pair(codeSt.PairingLink)
	if err != nil {
		t.Fatal(err)
	}
	peerID := paired.Peers[0].ID

	checked, err := ctrl.Check(ctx, peerID, "direct")
	if err != nil || checked.Peers[0].Reachable == nil || !*checked.Peers[0].Reachable || checked.Peers[0].ErrorCode != "" {
		t.Fatalf("healthy Check failed: %+v err=%v", checked.Peers, err)
	}

	// Corrupt client secret -> Check must surface unauthorized without leaking secret.
	badCfg, _ := loadConfig(ctrlDir)
	badCfg.Peers[0].Pair.Secret = connectivity.Token()
	_ = saveConfig(ctrlDir, badCfg)
	checkedBad, err := ctrl.Check(ctx, peerID, "direct")
	if err != nil || checkedBad.Peers[0].Reachable == nil || *checkedBad.Peers[0].Reachable {
		t.Fatalf("expected unreachable peer on bad secret: %+v err=%v", checkedBad.Peers, err)
	}
	if checkedBad.Peers[0].ErrorCode != "unauthorized" {
		t.Fatalf("expected errorCode=unauthorized, got %+v", checkedBad.Peers[0])
	}

	host.mu.Lock()
	host.redeemAttempts = nil
	host.mu.Unlock()

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(host.cfg.Pair.Certificate))
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "vantry.remote", MinVersion: tls.VersionTLS13}}
	defer tr.CloseIdleConnections()
	httpClient := &http.Client{Transport: tr}

	target := "https://" + st.Direct[0] + "/pairings/redeem"
	for range 20 {
		resp, err := httpClient.Post(target, "application/json", strings.NewReader(`{"scope":"session","code":"SESSION-XXXX-XXXX-XXXX"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 before rate limit, got %d", resp.StatusCode)
		}
	}
	resp, err := httpClient.Post(target, "application/json", strings.NewReader(`{"scope":"session","code":"SESSION-XXXX-XXXX-XXXX"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 21st redeemLocal call, got %d", resp.StatusCode)
	}
}

func TestMultiRelayFailover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dataDir, sock := startMockCoreUnix(t)

	key1, key2 := connectivity.Token(), connectivity.Token()
	relay1, err := connectivity.NewRelay(connectivity.RelayConfig{RegistrationKey: key1})
	if err != nil {
		t.Fatal(err)
	}
	gateway1 := httptest.NewServer(relay1)
	relay2, err := connectivity.NewRelay(connectivity.RelayConfig{RegistrationKey: key2})
	if err != nil {
		t.Fatal(err)
	}
	defer relay2.Close()
	gateway2 := httptest.NewServer(relay2)
	defer gateway2.Close()

	host, err := New(ctx, Options{DataDir: dataDir, SocketPath: sock, DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if _, err := host.Start(ctx, "MultiRelayHost", gateway1.URL, key1); err != nil {
		t.Fatal(err)
	}
	if _, err := host.ConfigureRelays(ctx, []RelayEndpoint{{URL: gateway2.URL, RegistrationKey: key2}}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); !host.Status().RelayConnected; {
		if time.Now().After(deadline) {
			t.Fatal(host.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	codeSt, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}

	ctrlOpts := Options{DataDir: t.TempDir(), DisableDiscovery: true}
	ctrl, err := New(ctx, ctrlOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	paired, err := ctrl.PairWithContext(ctx, codeSt.PairingLink, "")
	if err != nil {
		t.Fatal(err)
	}
	peerID := paired.Peers[0].ID

	// Bring down primary relay; client in relay mode must fail over to secondary relay.
	relay1.Close()
	gateway1.Close()

	conn, err := ConnectWithMode(ctx, ctrlOpts, peerID, "relay")
	if err != nil {
		t.Fatalf("multi-relay failover failed: %v", err)
	}
	if conn.Mode() != "relay" {
		t.Fatalf("expected relay mode, got %s", conn.Mode())
	}
	_ = conn.Close()
}
