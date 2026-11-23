package remoteaccess

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMobileTLSKeepsNativeIdentityAndRoutesMobileAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager, err := New(ctx, Options{DataDir: t.TempDir(), DisableDiscovery: true, ListenAddress: "0.0.0.0:0", T3Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"environmentId":"fixture"}`)) })})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if _, err = manager.Start(ctx, "fixture", "", ""); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	native := manager.cfg.Pair.Certificate
	mobile := manager.cfg.MobileCertificate
	addresses := manager.cfg.Pair.Direct
	manager.mu.Unlock()
	block, _ := pem.Decode([]byte(mobile))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || cert.VerifyHostname("vantry.mobile") != nil {
		t.Fatal("mobile TLS identity is not an Apple-compatible ECDSA certificate")
	}
	for _, entry := range []struct{ name, certificate string }{{"vantry.remote", native}, {"vantry.mobile", mobile}} {
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM([]byte(entry.certificate))
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: entry.name, MinVersion: tls.VersionTLS13}}
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		_, port, _ := net.SplitHostPort(addresses[0])
		response, err := client.Get("https://127.0.0.1:" + port + "/.well-known/t3/environment")
		if err != nil {
			transport.CloseIdleConnections()
			t.Fatal(err)
		}
		response.Body.Close()
		transport.CloseIdleConnections()
		if response.StatusCode != 200 {
			t.Fatal("mobile discovery did not reach the scoped adapter")
		}
	}
	// Optional cross-runtime check uses the exact Foundation delegate shipped in
	// VantryKit, against this temporary server with no production credentials.
	if probe := os.Getenv("VANTRY_APPLE_TLS_PROBE"); probe != "" {
		data, _ := json.Marshal(manager.MobileRoutes())
		file := filepath.Join(t.TempDir(), "public-routes.json")
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.CommandContext(ctx, probe, file).CombinedOutput()
		if err != nil || !strings.Contains(string(output), "HTTP status 200") {
			t.Fatalf("Apple URLSession TLS check: %s (%v)", output, err)
		}
		t.Log(strings.TrimSpace(string(output)))
	}
	if manager.cfg.Pair.Certificate != native {
		t.Fatal("mobile TLS replaced the native computer identity")
	}
}

func TestPairingSurvivesUnavailableRelayWithoutPollingItsLocalCode(t *testing.T) {
	var calls atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "offline", 503) }))
	defer relay.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mobileBase string
	manager, err := New(ctx, Options{DataDir: t.TempDir(), DisableDiscovery: true, ListenAddress: "0.0.0.0:0", T3Pairing: func(_ context.Context, base string) (string, error) {
		mobileBase = base
		return base + "/#token=fixture", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if _, err = manager.Start(ctx, "fixture", "", ""); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.cfg.Pair.Relay = relay.URL
	manager.mu.Unlock()
	status, err := manager.GenerateCode(ctx)
	if err != nil || status.T3PairingLink == "" || mobileBase == "" {
		t.Fatal("offline relay prevented local QR pairing", err)
	}
	manager.RefreshPairing(ctx)
	if _, err = manager.RevokeCode(ctx); err != nil {
		t.Fatal("local pairing revocation depended on the relay", err)
	}
	if calls.Load() != 1 {
		t.Fatal("a local code was polled or revoked on the offline relay")
	}
}
