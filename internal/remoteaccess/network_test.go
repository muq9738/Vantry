package remoteaccess

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerNegotiatesHTTP2AndProxyPreservesEncoding(t *testing.T) {
	// Keep the Unix socket path short on macOS.
	dir, err := os.MkdirTemp("/tmp", "vantry-network-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "core.sock"))
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 2)
	backend := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Accept-Encoding")
		if r.Header.Get("Authorization") != "" {
			t.Error("device credential reached local Core")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":1,"state":"ready"}`)
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = backend.Serve(listener) }()
	defer func() { _ = backend.Close(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manager, err := New(ctx, Options{DataDir: dir, SocketPath: listener.Addr().String(), DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if _, err = manager.Start(ctx, "test", "", ""); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	pair := manager.cfg.Pair
	manager.mu.Unlock()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(pair.Certificate))
	tr := &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "vantry.remote", MinVersion: tls.VersionTLS13}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	for _, encoding := range []string{"gzip", "identity"} {
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+pair.Direct[0]+"/v1/health", nil)
		req.Header.Set("Authorization", "Bearer "+pair.Secret)
		req.Header.Set("Accept-Encoding", encoding)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || resp.ProtoMajor != 2 {
			t.Fatal(resp.Status, resp.Proto)
		}
		if got := <-requests; got != encoding {
			t.Fatalf("encoding changed: %q -> %q", encoding, got)
		}
	}
}
