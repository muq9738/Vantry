package t3compat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func desktopRequestForTest(t *testing.T, method, endpoint, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, endpoint, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

func TestPairedDesktopUsesSessionAuthorizationAndRevocationClosesTURN(t *testing.T) {
	s, _, endpoint := testServer(t)
	token := pairClient(t, s, endpoint.URL)
	const relayToken = "fixture-relay-viewer-credential"
	var relayed atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+relayToken || r.Header.Get("Cookie") != "" {
			t.Error("gateway did not isolate the relay credential")
			http.Error(w, "unauthorized", 401)
			return
		}
		relayed.Add(1)
		if r.URL.Path != "/desktop/v1/sessions/room-one/turn" {
			t.Error("unexpected relay path")
			http.NotFound(w, r)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if conn.WriteMessage(kind, data) != nil {
				return
			}
		}
	}))
	defer relay.Close()
	var approved atomic.Int32
	s.opts.Desktop = func(context.Context, string) (DesktopTarget, error) {
		return DesktopTarget{Relay: relay.URL, SessionID: "room-one", Token: relayToken, Authorize: func(context.Context, string) error {
			approved.Add(1)
			return nil
		}}, nil
	}
	status, body := desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/session", token)
	if status != 200 || !strings.Contains(body, "room-one") || strings.Contains(body, relayToken) || strings.Contains(body, token) {
		t.Fatalf("invalid desktop descriptor: status=%d", status)
	}
	status, _ = desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/v1/sessions/room-one/request", token)
	if status != 200 || approved.Load() != 1 {
		t.Fatal("paired device did not authorize without another code")
	}
	for _, path := range []string{"/api/desktop/v1/sessions/other/turn", "/api/desktop/v1/sessions/room-one/approve", "/api/desktop/v1/sessions/room-one/signal?role=host"} {
		status, _ = desktopRequestForTest(t, "GET", endpoint.URL+path, token)
		if status != 403 && status != 404 {
			t.Fatalf("unexpected desktop route authorized: %s", path)
		}
	}
	if relayed.Load() != 0 {
		t.Fatal("rejected routes reached the relay")
	}
	url := strings.Replace(endpoint.URL, "http", "ws", 1) + "/api/desktop/v1/sessions/room-one/turn"
	conn, response, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + token}})
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data := []byte{0, 255, 128, 0, 19}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, echoed, err := conn.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(data, echoed) {
		t.Fatal("desktop TURN bytes were not preserved")
	}
	if err := s.Revoke(s.Clients()[0].ID); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("revoking the paired phone left desktop transport open")
	}
	if status, _ = desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/session", token); status != 401 {
		t.Fatal("revoked device retained desktop access")
	}
}

func TestPairedDesktopRequiresOperatingAuthorizationAndActiveSharing(t *testing.T) {
	s, _, endpoint := testServer(t)
	token := pairClient(t, s, endpoint.URL)
	var calls atomic.Int32
	s.opts.Desktop = func(context.Context, string) (DesktopTarget, error) {
		calls.Add(1)
		return DesktopTarget{}, errors.New("sharing disabled")
	}
	for _, token := range []string{"", "invalid"} {
		if status, _ := desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/session", token); status != 401 {
			t.Fatal("unpaired client reached desktop access")
		}
	}
	s.mu.Lock()
	s.auth.Clients[0].Scopes = "orchestration:read"
	s.mu.Unlock()
	if status, _ := desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/session", token); status != 403 || calls.Load() != 0 {
		t.Fatal("read-only authorization reached desktop transport")
	}
	s.mu.Lock()
	s.auth.Clients[0].Scopes = scopes
	s.mu.Unlock()
	if status, body := desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/session", token); status != 409 || !strings.Contains(body, "desktop_not_sharing") {
		t.Fatal("disabled sharing was not reported")
	}
}

func TestPairedLANDesktopNeverOpensTURNOrExposesViewerCredential(t *testing.T) {
	s, _, endpoint := testServer(t)
	token := pairClient(t, s, endpoint.URL)
	var proxied atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if r.URL.Path != "/v1/sessions/local-room" || r.Header.Get("Authorization") != "Bearer local-viewer" {
			t.Error("incorrect local gateway request")
		}
		io.WriteString(w, `{"sessionId":"local-room","transport":"lan","iceTransportPolicy":"all","viewerToken":"local-viewer"}`)
	}))
	defer local.Close()
	var relayStarts atomic.Int32
	s.opts.Desktop = func(_ context.Context, selector string) (DesktopTarget, error) {
		if selector == "relay" {
			relayStarts.Add(1)
			return DesktopTarget{}, errors.New("relay offline")
		}
		return DesktopTarget{Relay: local.URL, SessionID: "local-room", Token: "local-viewer", Direct: true, RelayAvailable: true, Authorize: func(context.Context, string) error { return nil }}, nil
	}
	status, body := desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/session", token)
	if status != 200 || !strings.Contains(body, `"transport":"lan"`) || relayStarts.Load() != 0 {
		t.Fatal("default descriptor depended on the relay")
	}
	status, body = desktopRequestForTest(t, "GET", endpoint.URL+"/api/desktop/v1/sessions/local-room", token)
	if status != 200 || !strings.Contains(body, `"approved":true`) || !strings.Contains(body, `"relayAvailable":true`) || strings.Contains(body, "local-viewer") {
		t.Fatal("LAN metadata did not use the existing device authorization")
	}
	status, _ = desktopRequestForTest(t, "GET", endpoint.URL+"/api/desktop/v1/sessions/local-room/turn", token)
	if status != 404 || proxied.Load() != 1 {
		t.Fatal("LAN session opened a TURN route")
	}
	status, _ = desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/session?transport=relay", token)
	if status != 502 || relayStarts.Load() != 1 {
		t.Fatal("explicit fallback did not select the relay")
	}
}

func TestRelayDesktopSessionReportsNotSharingBeforeRelayUnavailable(t *testing.T) {
	s, _, endpoint := testServer(t)
	token := pairClient(t, s, endpoint.URL)
	var errToReturn = ErrDesktopNotSharing
	s.opts.Desktop = func(context.Context, string) (DesktopTarget, error) {
		return DesktopTarget{}, errToReturn
	}
	if status, body := desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/session?transport=relay", token); status != 409 || !strings.Contains(body, "desktop_not_sharing") {
		t.Fatalf("expected 409 desktop_not_sharing when sharing is disabled on relay request, got %d: %s", status, body)
	}
	errToReturn = errors.New("relay offline")
	if status, body := desktopRequestForTest(t, "POST", endpoint.URL+"/api/desktop/session?transport=relay", token); status != 502 || !strings.Contains(body, "desktop_relay_unavailable") {
		t.Fatalf("expected 502 desktop_relay_unavailable when relay fails while sharing is active, got %d: %s", status, body)
	}
}
