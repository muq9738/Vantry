package connectivity

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPairingPurposeExpirationReplayAndConcurrentRedemption(t *testing.T) {
	var store Pairings
	entry, err := store.Put(Session, "Host", json.RawMessage(`{"deviceId":"device"}`), time.Minute, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Redeem(Desktop, entry.Code); err == nil {
		t.Fatal("session code granted desktop access")
	}
	var wg sync.WaitGroup
	wins := make(chan Pairing, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if result, err := store.Redeem(Session, entry.Code, "Viewer"); err == nil {
				wins <- result
			}
		}()
	}
	wg.Wait()
	close(wins)
	if len(wins) != 1 {
		t.Fatalf("one-time code had %d successful redeemers", len(wins))
	}
	state, ok := store.Info(entry.ID, "owner")
	if !ok || state.Status != "paired" || state.PairedWith != "Viewer" || state.Payload != nil {
		t.Fatal(state, ok)
	}
	if _, ok = store.Info(entry.ID, "other-owner"); ok {
		t.Fatal("pair state crossed owner boundary")
	}
	expired, err := store.Put(Session, "Expired", json.RawMessage(`{}`), time.Minute, "owner")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	value := store.entries[expired.Code]
	value.ExpiresAt = time.Now().Add(-time.Second)
	store.entries[expired.Code] = value
	store.mu.Unlock()
	if _, err = store.Redeem(Session, expired.Code); err == nil {
		t.Fatal("expired code redeemed")
	}
	if state, ok = store.Info(expired.ID, "owner"); !ok || state.Status != "expired" {
		t.Fatal(state, ok)
	}
	if store.Revoke(entry.ID, "other-owner") {
		t.Fatal("foreign owner revoked a code")
	}
}
func TestNativeReverseTunnelServesRootHTTPAndWebSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/ws" {
			conn, err := upgrade(w, req)
			if err != nil {
				return
			}
			defer conn.Close()
			kind, data, err := conn.ReadMessage()
			if err == nil {
				_ = conn.WriteMessage(kind, data)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"path": req.URL.Path, "authorization": req.Header.Get("Authorization")})
	}))
	defer backend.Close()
	route, key := Token(), Token()
	relay, err := NewRelay(RelayConfig{RegistrationKey: key, Routes: map[string]string{"vantry.test": route}})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	gateway := httptest.NewServer(relay)
	defer gateway.Close()
	host := StartHost(ctx, gateway.URL, route, key, func(ctx context.Context, channel string) (net.Conn, error) {
		if channel != "t3" {
			t.Fatal("wrong channel")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(backend.URL, "http://"))
	})
	defer host.Close()
	for deadline := time.Now().Add(3 * time.Second); host.State() != "online"; {
		if time.Now().After(deadline) {
			t.Fatal(host.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", gateway.URL+"/.well-known/t3/environment", nil)
	req.Host = "vantry.test"
	req.Header.Set("Authorization", "Bearer test-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(data), "/.well-known/t3/environment") || !strings.Contains(string(data), "test-session") {
		t.Fatal(resp.Status, string(data))
	}
	conn, resp, err := websocket.DefaultDialer.Dial(strings.Replace(gateway.URL, "http", "ws", 1)+"/ws", http.Header{"Host": {"vantry.test"}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.WriteMessage(websocket.TextMessage, []byte("native-root-websocket")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err = conn.ReadMessage()
	if err != nil || string(data) != "native-root-websocket" {
		t.Fatal(string(data), err)
	}
}
func TestLegacyT3HTTPUpstreamKeepsRequestPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { _, _ = io.WriteString(w, req.URL.RequestURI()) }))
	defer backend.Close()
	relay, err := NewRelay(RelayConfig{RegistrationKey: Token(), HTTPUpstreams: []HTTPUpstream{{Host: "existing-t3.test", Prefix: "/", Target: backend.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	req := httptest.NewRequest("GET", "http://existing-t3.test/~!frp?transport=wss", nil)
	response := httptest.NewRecorder()
	relay.ServeHTTP(response, req)
	if response.Code != 200 || response.Body.String() != "/~!frp?transport=wss" {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestTunnelPoolReuseAndDestinationIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key, first, second := Token(), Token(), Token()
	relay, err := NewRelay(RelayConfig{
		RegistrationKey: key,
		Routes:          map[string]string{"first.test": first, "second.test": second},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	gateway := httptest.NewServer(relay)
	defer gateway.Close()

	var dials atomic.Int32
	for _, route := range []string{first, second} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_, _ = io.WriteString(w, route+"/"+req.Header.Get("Accept-Encoding"))
		}))
		defer backend.Close()
		host := StartHost(ctx, gateway.URL, route, key, func(ctx context.Context, _ string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(backend.URL, "http://"))
		})
		defer host.Close()
		for deadline := time.Now().Add(3 * time.Second); host.State() != "online"; {
			if time.Now().After(deadline) {
				t.Fatal(host.State())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	for _, tc := range []struct{ host, want string }{
		{"first.test", first + "/identity"},
		{"second.test", second + "/identity"},
	} {
		for range 4 {
			req, _ := http.NewRequestWithContext(ctx, "GET", gateway.URL+"/v1/snapshot", nil)
			req.Host = tc.host
			req.Header.Set("Accept-Encoding", "identity")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(data) != tc.want {
				t.Fatalf("got %q, want %q", string(data), tc.want)
			}
		}
	}
	if dials.Load() != 2 {
		t.Fatalf("8 requests opened %d tunnels, want 2 pooled destinations", dials.Load())
	}
}

func TestConnectRequiresWebSocketAndQuotasAndTakeoverAndHostIsolation(t *testing.T) {
	origProbe := hostConflictProbeTimeout
	hostConflictProbeTimeout = 60 * time.Millisecond
	t.Cleanup(func() { hostConflictProbeTimeout = origProbe })

	keyA, keyB := Token(), Token()
	routeA, routeB := Token(), Token()
	relay, err := NewRelay(RelayConfig{
		HostCredentials: []HostCredential{
			{Key: keyA, Routes: []string{routeA}},
			{Key: keyB, Routes: []string{routeB}},
		},
		MaxConnections:         20,
		MaxPendingPerRoute:     1,
		MaxConnectionsPerRoute: 2,
		TrustedProxies:         []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	gateway := httptest.NewServer(relay)
	defer gateway.Close()

	// Host A cannot register route B.
	wsBase := strings.Replace(gateway.URL, "http", "ws", 1)
	if conn, resp, err := websocket.DefaultDialer.Dial(wsBase+"/relay/host?route="+routeB, http.Header{"Authorization": {"Bearer " + keyA}}); err == nil {
		_ = conn.Close()
		t.Fatal("host A registered route B")
	} else if resp != nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var openedA atomic.Int32
	stall := make(chan struct{})
	defer close(stall)
	hostA := StartHostWithInstance(ctx, gateway.URL, routeA, keyA, "inst-a", func(ctx context.Context, _ string) (net.Conn, error) {
		openedA.Add(1)
		select {
		case <-stall:
		case <-ctx.Done():
		}
		return nil, context.Canceled
	})
	defer hostA.Close()
	for deadline := time.Now().Add(3 * time.Second); hostA.State() != "online"; {
		if time.Now().After(deadline) {
			t.Fatal(hostA.State())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Non-WS /relay/connect rejected before opening host.
	resp, err := http.Get(gateway.URL + "/relay/connect?route=" + routeA)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || openedA.Load() != 0 {
		t.Fatalf("non-ws connect opened host (%d, status=%d)", openedA.Load(), resp.StatusCode)
	}

	// Live same-instance duplicate rejected with 409.
	if conn, resp, err := websocket.DefaultDialer.Dial(wsBase+"/relay/host?route="+routeA+"&instance=inst-a", http.Header{"Authorization": {"Bearer " + keyA}}); err == nil {
		_ = conn.Close()
		t.Fatal("live host evicted")
	} else if resp != nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409, got %d", resp.StatusCode)
		}
	}

	// Route A saturation does not affect Route B.
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer backendB.Close()
	hostB := StartHostWithInstance(ctx, gateway.URL, routeB, keyB, "inst-b", func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(backendB.URL, "http://"))
	})
	defer hostB.Close()
	for deadline := time.Now().Add(3 * time.Second); hostB.State() != "online"; {
		if time.Now().After(deadline) {
			t.Fatal(hostB.State())
		}
		time.Sleep(5 * time.Millisecond)
	}
	var wg sync.WaitGroup
	pCtx, pCancel := context.WithCancel(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = relay.open(pCtx, routeA, "core")
	}()
	for deadline := time.Now().Add(2 * time.Second); ; {
		relay.mu.Lock()
		n := relay.routePending[routeA]
		relay.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("routeA pending not registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := DialTunnel(ctx, gateway.URL, routeA); err == nil {
		t.Fatal("routeA bypassed MaxPendingPerRoute")
	}
	if connB, err := DialTunnel(ctx, gateway.URL, routeB); err != nil {
		t.Fatalf("routeB blocked by routeA: %v", err)
	} else {
		_ = connB.Close()
	}
	pCancel()
	wg.Wait()

	// Trusted proxy XFF isolates per-client pairing rate limits.
	for range 20 {
		req, _ := http.NewRequest(http.MethodPost, gateway.URL+"/pairings/redeem", strings.NewReader(`{"scope":"session","code":"NONE"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "198.51.100.10")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
	}
	req1, _ := http.NewRequest(http.MethodPost, gateway.URL+"/pairings/redeem", strings.NewReader(`{"scope":"session","code":"NONE"}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Forwarded-For", "198.51.100.10")
	r1, _ := http.DefaultClient.Do(req1)
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for XFF client 1, got %d", r1.StatusCode)
	}
	req2, _ := http.NewRequest(http.MethodPost, gateway.URL+"/pairings/redeem", strings.NewReader(`{"scope":"session","code":"NONE"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Forwarded-For", "198.51.100.11")
	r2, _ := http.DefaultClient.Do(req2)
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusBadRequest {
		t.Fatalf("XFF client 2 shared rate limit with client 1: got %d", r2.StatusCode)
	}

	// Host pairing isolation.
	var created Pairing
	if err := Request(ctx, nil, "POST", gateway.URL, "/pairings", keyA, Pairing{
		Scope: Session, Name: "A", ExpiresAt: time.Now().Add(time.Minute), Payload: json.RawMessage(`{}`),
	}, &created); err != nil {
		t.Fatal(err)
	}
	if err := Request(ctx, nil, "GET", gateway.URL, "/pairings?id="+created.ID, keyB, nil, &Pairing{}); err == nil {
		t.Fatal("host B read host A pairing")
	}

	// Unresponsive half-open control connection with same instance is evicted.
	hostA.Close()
	stale, _, err := websocket.DefaultDialer.Dial(wsBase+"/relay/host?route="+routeA+"&instance=inst-a", http.Header{"Authorization": {"Bearer " + keyA}})
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	rep, rResp, err := websocket.DefaultDialer.Dial(wsBase+"/relay/host?route="+routeA+"&instance=inst-a", http.Header{"Authorization": {"Bearer " + keyA}})
	if rResp != nil && rResp.Body != nil {
		_ = rResp.Body.Close()
	}
	if err != nil {
		t.Fatalf("same-instance takeover failed: %v", err)
	}
	_ = rep.Close()
}
