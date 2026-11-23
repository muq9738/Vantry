package t3compat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/muq9738/Vantry/internal/adapters/provider/controlled"
	"github.com/muq9738/Vantry/internal/core"
	"github.com/muq9738/Vantry/pkg/api"
)

func testServer(t *testing.T) (*Server, *core.Core, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	workspace := &api.WorkspaceSpec{Kind: "directory", Path: dir}
	service, err := core.Open(context.Background(), core.Options{DBPath: filepath.Join(dir, "state.sqlite"), Provider: controlled.Provider{Delay: 20 * time.Millisecond}, DefaultProviderID: "pi", DefaultModel: "test-model", DefaultWorkspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	service.SetModelCatalogProvider(func(context.Context, string, string) (api.ModelCatalog, error) {
		return api.ModelCatalog{Models: []api.ModelInfo{{ID: "test-model", Provider: "example", Name: "Test model"}}, Default: "example/test-model"}, nil
	})
	if _, err = service.Dispatch(context.Background(), api.Command{Version: 1, CommandID: "seed", Kind: api.CreateThread, ThreadID: "seed", Title: "Existing thread"}); err != nil {
		t.Fatal(err)
	}
	server, err := New(service, Options{DataDir: dir, EnvironmentID: "vantry-test", Name: "Test Vantry", Version: "test", Provider: "pi", Model: "test-model", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return server, service, httpServer
}
func pairClient(t *testing.T, s *Server, base string) string {
	t.Helper()
	link, err := s.PairingLink(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(link)
	fragment, _ := url.ParseQuery(parsed.Fragment)
	form := url.Values{"grant_type": {tokenGrant}, "subject_token": {fragment.Get("token")}, "subject_token_type": {bootstrapType}, "requested_token_type": {accessType}}
	resp, err := http.PostForm(base+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil || result.AccessToken == "" {
		t.Fatal("invalid token exchange")
	}
	resp2, err := http.PostForm(base+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatal("bootstrap credential reused")
	}
	return result.AccessToken
}
func openRPC(t *testing.T, base, token string) *websocket.Conn {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/api/auth/websocket-ticket/rpc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Ticket string `json:"ticket"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil || result.Ticket == "" {
		t.Fatal("invalid ticket")
	}
	endpoint := strings.Replace(base, "http", "ws", 1) + "/ws?wsTicket=" + url.QueryEscape(result.Ticket)
	conn, reply, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if reply != nil {
		_ = reply.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if reused, reply, err := websocket.DefaultDialer.Dial(endpoint, nil); err == nil {
		_ = reused.Close()
		t.Fatal("RPC ticket reused")
	} else if reply != nil {
		_ = reply.Body.Close()
	}
	return conn
}
func sendRPC(t *testing.T, conn *websocket.Conn, id, method string, payload Object) {
	t.Helper()
	if err := conn.WriteJSON(Object{"_tag": "Request", "id": id, "tag": method, "payload": payload, "headers": []any{}}); err != nil {
		t.Fatal(err)
	}
}
func responseRPC(t *testing.T, conn *websocket.Conn, id string) Object {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var response Object
		if err := conn.ReadJSON(&response); err != nil {
			t.Fatal(err)
		}
		if response["requestId"] == id {
			return response
		}
	}
}
func TestT3PairingRPCStreamingCommandsAndReconnect(t *testing.T) {
	server, service, httpServer := testServer(t)
	token := pairClient(t, server, httpServer.URL)
	conn := openRPC(t, httpServer.URL, token)
	sendRPC(t, conn, "config", "subscribeServerConfig", Object{})
	response := responseRPC(t, conn, "config")
	if response["_tag"] != "Chunk" {
		t.Fatal(response)
	}
	config := response["values"].([]any)[0].(map[string]any)
	if config["type"] != "snapshot" {
		t.Fatal(config)
	}
	sendRPC(t, conn, "thread", "orchestration.subscribeThread", Object{"threadId": "seed"})
	response = responseRPC(t, conn, "thread")
	if response["values"].([]any)[0].(map[string]any)["kind"] != "snapshot" {
		t.Fatal(response)
	}
	command := Object{"type": "message.dispatch", "commandId": "send", "threadId": "seed", "messageId": "message", "text": "native protocol", "attachments": []any{}, "dispatchMode": Object{"type": "start_immediately"}}
	sendRPC(t, conn, "send", "orchestration.dispatchCommand", command)
	if response = responseRPC(t, conn, "send"); object(response["exit"])["_tag"] != "Success" {
		t.Fatal(response)
	}
	sendRPC(t, conn, "retry", "orchestration.dispatchCommand", command)
	if response = responseRPC(t, conn, "retry"); object(response["exit"])["_tag"] != "Success" {
		t.Fatal(response)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		snapshot, err := service.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Runs) == 1 && snapshot.Runs[0].Status.Terminal() {
			if !strings.Contains(snapshot.Runs[0].Output, "native protocol") {
				t.Fatal(snapshot.Runs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real Core run did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = conn.Close()
	reconnected := openRPC(t, httpServer.URL, token)
	sendRPC(t, reconnected, "resume", "orchestration.subscribeThread", Object{"threadId": "seed", "afterSequence": 0})
	response = responseRPC(t, reconnected, "resume")
	projection := object(response["values"].([]any)[0])["projection"].(map[string]any)
	if len(projection["runs"].([]any)) != 1 || len(projection["messages"].([]any)) != 2 {
		t.Fatal("reconnect lost or duplicated history")
	}
	if err := server.Revoke(server.Clients()[0].ID); err != nil {
		t.Fatal(err)
	}
	_ = reconnected.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := reconnected.ReadMessage(); err != nil {
			break
		}
	}
	req, _ := http.NewRequest("GET", httpServer.URL+"/api/orchestration/shell", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("revoked client still authorized")
	}
}
func TestReadOnlyT3ClientCannotDispatch(t *testing.T) {
	server, service, httpServer := testServer(t)
	token := pairClient(t, server, httpServer.URL)
	server.mu.Lock()
	server.auth.Clients[0].Scopes = "orchestration:read"
	server.mu.Unlock()
	conn := openRPC(t, httpServer.URL, token)
	sendRPC(t, conn, "read-only", "orchestration.dispatchCommand", Object{"type": "message.dispatch", "commandId": "denied-send", "threadId": "seed", "messageId": "message", "text": "must not execute", "attachments": []any{}, "dispatchMode": Object{"type": "start_immediately"}})
	response := responseRPC(t, conn, "read-only")
	if object(response["exit"])["_tag"] != "Failure" {
		t.Fatal("read-only client operated a run", response)
	}
	snapshot, err := service.Snapshot(context.Background())
	if err != nil || len(snapshot.Runs) != 0 {
		t.Fatal(snapshot, err)
	}
}
func TestT3ProjectIdentityPreservesExecutionEnvironment(t *testing.T) {
	server, service, _ := testServer(t)
	ctx := context.Background()
	_, err := service.Dispatch(ctx, api.Command{Version: 1, CommandID: "foreign-create", Kind: api.CreateThread, ThreadID: "foreign", Title: "Foreign", Workspace: &api.WorkspaceSpec{EnvironmentID: "other-environment", Kind: "directory", Path: server.opts.Workspace.Path}})
	if err != nil {
		t.Fatal(err)
	}
	shell, err := server.shell(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, raw := range shell["threads"].([]any) {
		row := raw.(Object)
		ids[row["id"].(string)] = row["projectId"].(string)
	}
	if ids["seed"] == ids["foreign"] {
		t.Fatal("same path on different environments shared a project identity")
	}
	_, err = server.dispatch(ctx, Object{"type": "thread.create", "commandId": "wrong-environment", "threadId": "wrong", "projectId": ids["foreign"], "title": "Do not relocate", "modelSelection": selection("pi", "test-model"), "runtimeMode": "auto"})
	if err == nil {
		t.Fatal("foreign workspace was reinterpreted as a local directory")
	}
	snapshot, _ := service.Snapshot(ctx)
	if len(snapshot.Threads) != 2 {
		t.Fatal("rejected creation changed Core state")
	}
}

func TestT3ProjectionFixtures(t *testing.T) {
	server, service, _ := testServer(t)
	ctx := context.Background()
	_, err := service.Dispatch(ctx, api.Command{Version: 1, CommandID: "fixture-send", Kind: api.SendMessage, ThreadID: "seed", Message: "Fixture"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		snapshot, _ := service.Snapshot(ctx)
		if len(snapshot.Runs) > 0 && snapshot.Runs[0].Status.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture run timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run := snapshot.Runs[0]
	snapshot.Requests = []api.ProviderRequest{{ID: "confirm", ThreadID: "seed", RunID: run.ID, AttemptID: run.AttemptID, Method: "confirm", Title: "Allow edit", Message: "Write sample.txt", Status: "pending", CreatedAt: time.Now(), UpdatedAt: time.Now()}, {ID: "question", ThreadID: "seed", RunID: run.ID, AttemptID: run.AttemptID, Method: "select", Title: "Choose", Options: []string{"One", "Two"}, Status: "pending", CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	snapshot.Inputs = []api.Input{{ID: "queued-input", ThreadID: "seed", Kind: "queue", Message: "Queued", Status: "queued", Position: 0, CreatedAt: time.Now(), UpdatedAt: time.Now()}, {ID: "steer-input", ThreadID: "seed", RunID: run.ID, Kind: "steer", Message: "Steer", Status: "delivered", CreatedAt: time.Now(), UpdatedAt: time.Now()}}
	snapshot.Tools = []api.ToolCall{{ID: "tool", ThreadID: "seed", RunID: run.ID, Name: "read", Arguments: json.RawMessage(`{"path":"sample.txt"}`), Status: "completed", Output: "sample", UpdatedAt: time.Now()}}
	projection, err := server.project(snapshot, "seed")
	if err != nil {
		t.Fatal(err)
	}
	config, err := server.config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if target := os.Getenv("VANTRY_T3_FIXTURES"); target != "" {
		data, _ := json.Marshal(Object{"config": config, "shell": server.shellSnapshot(snapshot), "projection": projection})
		if err = os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigratedT3MobileSessionTokenVerifiesAndRevokes(t *testing.T) {
	dir := t.TempDir()
	key := []byte("0123456789abcdef0123456789abcdef")
	exp := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	migratedScopes := "orchestration:read orchestration:operate filesystem:read filesystem:write terminal:read terminal:operate preview:operate diagnostics:read relay:read"
	state := authState{
		EnvironmentID:    "97f2e6ff-063e-4607-b405-20c7a643a337",
		LegacySigningKey: key,
		LegacySessions: []LegacySession{{
			ID:        "3cadbbf1-4370-4a7c-9049-94cdfb53224a",
			Name:      "手机 Studio 国内新域名 App 重试",
			Scopes:    migratedScopes,
			CreatedAt: time.Now().Add(-24 * time.Hour).UTC(),
			ExpiresAt: exp,
		}},
	}
	rawState, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "t3-access.json"), rawState, 0600); err != nil {
		t.Fatal(err)
	}

	base, service, ts := testServer(t)
	defer ts.Close()
	srv, err := New(service, Options{DataDir: dir, EnvironmentID: "vantry-new", Name: "Studio", Version: "0.1.18", Provider: "pi", Model: "test-model", Workspace: base.opts.Workspace})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.descriptor()["environmentId"]; got != "97f2e6ff-063e-4607-b405-20c7a643a337" {
		t.Fatalf("expected migrated environmentId, got %v", got)
	}

	claimsJSON, _ := json.Marshal(map[string]any{
		"v":      1,
		"kind":   "session",
		"sid":    "3cadbbf1-4370-4a7c-9049-94cdfb53224a",
		"role":   "owner",
		"method": "bearer-access-token",
		"exp":    exp.Unix(),
		"label":  "手机 Studio 国内新域名 App 重试",
		"scopes": []string{"orchestration:read", "orchestration:operate"},
	})
	payloadB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payloadB64))
	token := payloadB64 + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if client, ok := srv.authenticated(token); !ok || client.ID != "3cadbbf1-4370-4a7c-9049-94cdfb53224a" {
		t.Fatalf("migrated T3 mobile token rejected: ok=%v client=%+v", ok, client)
	}
	if len(srv.Clients()) != 1 || srv.Clients()[0].Name != "手机 Studio 国内新域名 App 重试" {
		t.Fatalf("migrated session missing from Clients(): %+v", srv.Clients())
	}

	// Verify GET /api/auth/session preserves all 9 migrated scopes in both scopes and permissions.
	migHTTP := httptest.NewServer(srv)
	defer migHTTP.Close()
	sessReq, _ := http.NewRequest("GET", migHTTP.URL+"/api/auth/session", nil)
	sessReq.Header.Set("Authorization", "Bearer "+token)
	sessResp, err := http.DefaultClient.Do(sessReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sessResp.Body.Close()
	var sessBody map[string]any
	if err := json.NewDecoder(sessResp.Body).Decode(&sessBody); err != nil {
		t.Fatal(err)
	}
	perms, _ := sessBody["permissions"].([]any)
	scList, _ := sessBody["scopes"].([]any)
	if len(perms) != 9 || len(scList) != 4 {
		t.Fatalf("expected 4 legacy scopes & 9 permissions, got scopes=%v permissions=%v", scList, perms)
	}
	for _, sc := range scList {
		if !legacyOnlyT3Permissions[sc.(string)] {
			t.Fatalf("unexpected non-legacy scope in scopes array: %v", scList)
		}
	}
	expStr, _ := sessBody["expiresAt"].(string)
	if !strings.HasSuffix(expStr, "Z") {
		t.Fatalf("expected UTC Z expiresAt, got %v", sessBody["expiresAt"])
	}
	hasFSRead := false
	for _, p := range perms {
		if p == "filesystem:read" {
			hasFSRead = true
		}
	}
	if !hasFSRead {
		t.Fatalf("expected filesystem:read in permissions to prevent legacy-permission banner, got %v", perms)
	}

	// Tampered signature must fail.
	if _, ok := srv.authenticated(payloadB64 + ".AAAA"); ok {
		t.Fatal("tampered signature accepted")
	}

	// Revoking the migrated device invalidates the token immediately.
	if err := srv.Revoke("3cadbbf1-4370-4a7c-9049-94cdfb53224a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.authenticated(token); ok {
		t.Fatal("revoked migrated token still accepted")
	}
}

func TestT3MobileExpandedScopesExchangeAndBrowserSessionCookie(t *testing.T) {
	server, _, httpServer := testServer(t)

	// 1. T3 0.1.10 iOS client sends all 16 scopes during token exchange.
	link, err := server.PairingLink(context.Background(), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(link)
	fragment, _ := url.ParseQuery(parsed.Fragment)
	mobileScopes := "orchestration:read orchestration:operate settings:write providers:manage environment:maintain preview:operate diagnostics:read terminal:read terminal:operate source-control:write filesystem:read filesystem:write relay:read access:read access:write relay:write"
	form := url.Values{
		"grant_type":           {tokenGrant},
		"subject_token":        {fragment.Get("token")},
		"subject_token_type":   {bootstrapType},
		"requested_token_type": {accessType},
		"client_label":         {"T3 Code Mobile"},
		"scope":                {mobileScopes},
	}
	resp, err := http.PostForm(httpServer.URL+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("T3 mobile multi-scope exchange failed with %s", resp.Status)
	}
	var tokenRes struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenRes); err != nil || tokenRes.AccessToken == "" {
		t.Fatalf("decode mobile token exchange: %v %+v", err, tokenRes)
	}
	if tokenRes.Scope != mobileScopes {
		t.Fatalf("expected all valid mobile scopes preserved, got %q", tokenRes.Scope)
	}

	// 1b. Unauthenticated GET /api/auth/session omits permissions.
	unauthResp, err := http.Get(httpServer.URL + "/api/auth/session")
	if err != nil {
		t.Fatal(err)
	}
	var unauthBody map[string]any
	_ = json.NewDecoder(unauthResp.Body).Decode(&unauthBody)
	_ = unauthResp.Body.Close()
	if _, hasPerms := unauthBody["permissions"]; hasPerms {
		t.Fatalf("unauthenticated /api/auth/session should omit permissions, got %+v", unauthBody)
	}

	// 1c. Authenticated GET /api/auth/session with 16-scope bearer token returns all 16 scopes & permissions.
	bearerSessReq, _ := http.NewRequest("GET", httpServer.URL+"/api/auth/session", nil)
	bearerSessReq.Header.Set("Authorization", "Bearer "+tokenRes.AccessToken)
	bearerSessResp, err := http.DefaultClient.Do(bearerSessReq)
	if err != nil {
		t.Fatal(err)
	}
	var bearerSessBody map[string]any
	_ = json.NewDecoder(bearerSessResp.Body).Decode(&bearerSessBody)
	_ = bearerSessResp.Body.Close()
	if bPerms, _ := bearerSessBody["permissions"].([]any); len(bPerms) != 16 {
		t.Fatalf("expected 16 permissions on expanded bearer session, got %v", bPerms)
	}
	bScopes, _ := bearerSessBody["scopes"].([]any)
	if len(bScopes) != 7 {
		t.Fatalf("expected 7 legacy-compatible scopes on 16-scope session, got %v", bScopes)
	}
	for _, sc := range bScopes {
		if !legacyOnlyT3Permissions[sc.(string)] {
			t.Fatalf("expected only legacyOnlyT3Permissions in scopes, got %v", bScopes)
		}
	}

	// 2. Unknown or non-orchestration-only scopes are still rejected with 400.
	for _, badScope := range []string{"unknown:admin", "filesystem:read"} {
		link2, _ := server.PairingLink(context.Background(), httpServer.URL)
		u2, _ := url.Parse(link2)
		f2, _ := url.ParseQuery(u2.Fragment)
		badResp, err := http.PostForm(httpServer.URL+"/oauth/token", url.Values{
			"grant_type":           {tokenGrant},
			"subject_token":        {f2.Get("token")},
			"subject_token_type":   {bootstrapType},
			"requested_token_type": {accessType},
			"scope":                {badScope},
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = badResp.Body.Close()
		if badResp.StatusCode != 400 {
			t.Fatalf("scope %q expected 400, got %d", badScope, badResp.StatusCode)
		}
	}

	// 3. CORS preflight & unauthenticated Origin probe (without Cookie header) must still allow credentials.
	preflight, _ := http.NewRequest("OPTIONS", httpServer.URL+"/api/auth/session", nil)
	preflight.Header.Set("Origin", "https://studio.example.com")
	preResp, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	_ = preResp.Body.Close()
	if preResp.StatusCode != 204 || preResp.Header.Get("Access-Control-Allow-Credentials") != "true" || preResp.Header.Get("Access-Control-Allow-Origin") != "https://studio.example.com" {
		t.Fatalf("CORS preflight missing credentials headers: status=%d headers=%+v", preResp.StatusCode, preResp.Header)
	}

	// 4. POST /api/auth/browser-session exchanges one-time pairing token into HttpOnly cookie + media ticket works.
	browserLink, err := server.PairingLink(context.Background(), httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	bu, _ := url.Parse(browserLink)
	bf, _ := url.ParseQuery(bu.Fragment)
	bsReq, _ := http.NewRequest("POST", httpServer.URL+"/api/auth/browser-session", strings.NewReader(`{"credential":"`+bf.Get("token")+`","label":"iPhone Safari"}`))
	bsReq.Header.Set("Content-Type", "application/json")
	bsReq.Header.Set("Origin", "https://studio.example.com")
	bsReq.Header.Set("X-Forwarded-Proto", "https")
	bsResp, err := http.DefaultClient.Do(bsReq)
	if err != nil {
		t.Fatal(err)
	}
	defer bsResp.Body.Close()
	if bsResp.StatusCode != 200 {
		t.Fatalf("POST /api/auth/browser-session status = %d", bsResp.StatusCode)
	}
	var bsBody map[string]any
	if err := json.NewDecoder(bsResp.Body).Decode(&bsBody); err != nil {
		t.Fatal(err)
	}
	if _, hasPerms := bsBody["permissions"]; hasPerms {
		t.Fatalf("2-scope browser-session should omit permissions to avoid legacy-permission warning, got %+v", bsBody)
	}
	cookies := bsResp.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].Value == "" {
		t.Fatalf("expected Secure HttpOnly session cookie, got %+v", cookies)
	}

	// 4b. 2-scope cookie session on GET /api/auth/session also omits permissions.
	twoScopeReq, _ := http.NewRequest("GET", httpServer.URL+"/api/auth/session", nil)
	twoScopeReq.AddCookie(cookies[0])
	twoScopeResp, err := http.DefaultClient.Do(twoScopeReq)
	if err != nil {
		t.Fatal(err)
	}
	var twoScopeBody map[string]any
	_ = json.NewDecoder(twoScopeResp.Body).Decode(&twoScopeBody)
	_ = twoScopeResp.Body.Close()
	if _, hasPerms := twoScopeBody["permissions"]; hasPerms {
		t.Fatalf("2-scope session should omit permissions on GET /api/auth/session, got %+v", twoScopeBody)
	}

	rpcTicketReq, _ := http.NewRequest("POST", httpServer.URL+"/api/auth/websocket-ticket/rpc", nil)
	rpcTicketReq.Header.Set("Authorization", "Bearer "+tokenRes.AccessToken)
	rpcTicketResp, err := http.DefaultClient.Do(rpcTicketReq)
	if err != nil {
		t.Fatal(err)
	}
	var rpcTicketBody map[string]any
	_ = json.NewDecoder(rpcTicketResp.Body).Decode(&rpcTicketBody)
	_ = rpcTicketResp.Body.Close()
	if rpcTicketResp.StatusCode != 200 {
		t.Fatalf("POST /api/auth/websocket-ticket/rpc status = %d", rpcTicketResp.StatusCode)
	}
	if exp, _ := rpcTicketBody["expiresAt"].(string); !strings.HasSuffix(exp, "Z") {
		t.Fatalf("expected UTC Z websocket-ticket expiresAt, got %v", rpcTicketBody["expiresAt"])
	}

	ticketReq, _ := http.NewRequest("POST", httpServer.URL+"/api/auth/websocket-ticket/media", nil)
	ticketReq.AddCookie(cookies[0])
	ticketResp, err := http.DefaultClient.Do(ticketReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = ticketResp.Body.Close()
	if ticketResp.StatusCode != 200 {
		t.Fatalf("POST /api/auth/websocket-ticket/media status = %d", ticketResp.StatusCode)
	}

	// 5. Browser/WebView session cookie authentication & descriptor advertisement.
	req, _ := http.NewRequest("GET", httpServer.URL+"/api/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: "t3_session_97f2e6ff", Value: tokenRes.AccessToken})
	req.Header.Set("Origin", "https://studio.example.com")
	sessResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer sessResp.Body.Close()
	if sessResp.StatusCode != 200 {
		t.Fatalf("cookie session status = %d", sessResp.StatusCode)
	}
	if sessResp.Header.Get("Access-Control-Allow-Credentials") != "true" || sessResp.Header.Get("Access-Control-Allow-Origin") != "https://studio.example.com" {
		t.Fatalf("CORS credentials headers missing for cookie session: %+v", sessResp.Header)
	}
	var sessBody map[string]any
	if err := json.NewDecoder(sessResp.Body).Decode(&sessBody); err != nil {
		t.Fatal(err)
	}
	if sessBody["authenticated"] != true || sessBody["sessionMethod"] != "browser-session-cookie" {
		t.Fatalf("unexpected cookie session body: %+v", sessBody)
	}
	authObj := sessBody["auth"].(map[string]any)
	methods, _ := json.Marshal(authObj["sessionMethods"])
	if !strings.Contains(string(methods), "browser-session-cookie") || !strings.Contains(string(methods), "bearer-access-token") {
		t.Fatalf("authDescriptor missing sessionMethods: %s", methods)
	}
}

func TestT3StartupRPCsProbeLifecycleAndActivity(t *testing.T) {
	server, _, httpServer := testServer(t)
	token := pairClient(t, server, httpServer.URL)
	conn := openRPC(t, httpServer.URL, token)

	// Direct Bearer /ws upgrade without wsTicket must also succeed.
	bearerHeaders := http.Header{"Authorization": {"Bearer " + token}}
	bearerWS, reply, err := websocket.DefaultDialer.Dial(strings.Replace(httpServer.URL, "http", "ws", 1)+"/ws", bearerHeaders)
	if reply != nil {
		_ = reply.Body.Close()
	}
	if err != nil {
		t.Fatalf("direct Bearer /ws upgrade failed: %v", err)
	}
	sendRPC(t, bearerWS, "direct-probe", "server.probe", Object{})
	if directRes := responseRPC(t, bearerWS, "direct-probe"); object(directRes["exit"])["_tag"] != "Success" {
		_ = bearerWS.Close()
		t.Fatalf("direct Bearer /ws server.probe failed: %+v", directRes)
	}
	_ = bearerWS.Close()

	// Harmless Effect RPC control frames and unknown _tag frames must not drop the socket.
	for _, tag := range []string{"Pong", "Eof", "ResendPreviousSeq", "FutureControlFrame"} {
		if err := conn.WriteJSON(Object{"_tag": tag}); err != nil {
			t.Fatal(err)
		}
	}
	// Null payload on Request must decode cleanly without closing the socket.
	if err := conn.WriteJSON(Object{"_tag": "Request", "id": "null-payload", "tag": "server.probe", "payload": nil, "headers": []any{}}); err != nil {
		t.Fatal(err)
	}
	if nullRes := responseRPC(t, conn, "null-payload"); object(nullRes["exit"])["_tag"] != "Success" {
		t.Fatalf("null payload server.probe failed: %+v", nullRes)
	}

	sendRPC(t, conn, "probe", "server.probe", Object{})
	probeRes := responseRPC(t, conn, "probe")
	if object(probeRes["exit"])["_tag"] != "Success" {
		t.Fatalf("server.probe failed: %+v", probeRes)
	}

	sendRPC(t, conn, "activity", "server.reportClientActivity", Object{"focused": true})
	actRes := responseRPC(t, conn, "activity")
	if object(actRes["exit"])["_tag"] != "Success" {
		t.Fatalf("server.reportClientActivity failed: %+v", actRes)
	}

	// Verify subscribeServerConfig provider auth, capabilities, and supportedRuntimeModes.
	sendRPC(t, conn, "cfg", "subscribeServerConfig", Object{})
	cfgChunk := responseRPC(t, conn, "cfg")
	if cfgChunk["_tag"] != "Chunk" {
		t.Fatalf("expected cfg Chunk, got %+v", cfgChunk)
	}
	cfgObj := object(object(cfgChunk["values"].([]any)[0])["config"])
	providers, _ := cfgObj["providers"].([]any)
	if len(providers) == 0 {
		t.Fatalf("expected at least 1 provider in config, got %+v", cfgObj)
	}
	prov := object(providers[0])
	if object(prov["auth"])["status"] != "authenticated" {
		t.Fatalf("expected authenticated provider status, got %+v", prov["auth"])
	}
	modelsList, _ := prov["models"].([]any)
	if len(modelsList) == 0 || object(object(modelsList[0])["capabilities"])["optionDescriptors"] == nil {
		t.Fatalf("expected non-nil capabilities.optionDescriptors on model, got %+v", modelsList)
	}
	for _, m := range prov["supportedRuntimeModes"].([]any) {
		if m == "auto" {
			t.Fatalf("did not expect 'auto' in pi supportedRuntimeModes: %v", prov["supportedRuntimeModes"])
		}
	}
	piSettings := object(object(object(cfgObj["settings"])["providerInstances"])["pi"])
	if piSettings["enabled"] != true || piSettings["driver"] != "pi" {
		t.Fatalf("expected settings.providerInstances.pi.enabled=true, got %+v", cfgObj["settings"])
	}

	sendRPC(t, conn, "lifecycle", "subscribeServerLifecycle", Object{})
	welcome := responseRPC(t, conn, "lifecycle")
	if welcome["_tag"] != "Chunk" {
		t.Fatalf("expected welcome Chunk, got %+v", welcome)
	}
	wVals := welcome["values"].([]any)
	if len(wVals) == 0 || object(wVals[0])["type"] != "welcome" || object(object(wVals[0])["payload"])["bootstrapStatus"] != "complete" {
		t.Fatalf("unexpected lifecycle welcome payload: %+v", welcome)
	}
	ready := responseRPC(t, conn, "lifecycle")
	if ready["_tag"] != "Chunk" {
		t.Fatalf("expected ready Chunk, got %+v", ready)
	}
	rVals := ready["values"].([]any)
	if len(rVals) == 0 || object(rVals[0])["type"] != "ready" || object(object(rVals[0])["payload"])["environment"] == nil {
		t.Fatalf("unexpected lifecycle ready payload: %+v", ready)
	}

	// Verify startup streams added for T3 0.1.10 mobile.
	for _, tc := range []struct {
		id     string
		method string
		check  func(any)
	}{
		{"clones", "subscribeProjectClones", func(v any) {
			if _, ok := v.([]any); !ok {
				t.Fatalf("subscribeProjectClones expected array, got %#v", v)
			}
		}},
		{"vcs-branch", "subscribeVcsBranch", func(v any) {
			if object(v)["isRepo"] != false {
				t.Fatalf("subscribeVcsBranch expected isRepo=false, got %#v", v)
			}
		}},
		{"vcs-status", "subscribeVcsStatus", func(v any) {
			if object(v)["_tag"] != "snapshot" {
				t.Fatalf("subscribeVcsStatus expected _tag=snapshot, got %#v", v)
			}
		}},
		{"terminals", "subscribeTerminalMetadata", func(v any) {
			if object(v)["type"] != "snapshot" {
				t.Fatalf("subscribeTerminalMetadata expected type=snapshot, got %#v", v)
			}
		}},
		{"discovered", "subscribeDiscoveredLocalServers", func(v any) {
			if _, ok := object(v)["servers"].([]any); !ok {
				t.Fatalf("subscribeDiscoveredLocalServers expected servers array, got %#v", v)
			}
		}},
		{"tasks", "scheduledTasks.subscribe", func(v any) {
			if _, ok := object(v)["tasks"].([]any); !ok {
				t.Fatalf("scheduledTasks.subscribe expected tasks array, got %#v", v)
			}
		}},
	} {
		sendRPC(t, conn, tc.id, tc.method, Object{})
		resp := responseRPC(t, conn, tc.id)
		if resp["_tag"] != "Chunk" {
			t.Fatalf("%s expected Chunk, got %+v", tc.method, resp)
		}
		vals, _ := resp["values"].([]any)
		if len(vals) == 0 {
			t.Fatalf("%s returned empty values: %+v", tc.method, resp)
		}
		tc.check(vals[0])
	}

	// Verify unary startup RPCs added for T3 0.1.10 mobile.
	for _, tc := range []struct {
		id      string
		method  string
		payload Object
	}{
		{"peers", "peerLinks.list", Object{}},
		{"older", "orchestration.getOlderSettledThreads", Object{"limit": 20}},
		{"refs", "vcs.listRefs", Object{"cwd": "/tmp"}},
		{"vcs-refresh", "vcs.refreshStatus", Object{"cwd": "/tmp"}},
		{"prev-list", "preview.list", Object{"threadId": "seed"}},
		{"scm", "server.discoverSourceControl", Object{}},
		{"settings", "server.getSettings", Object{}},
		{"refresh-prov", "server.refreshProviders", Object{"refreshModels": true}},
		{"fs-meta", "filesystem.getMetadata", Object{"paths": []string{"a", "b"}}},
	} {
		sendRPC(t, conn, tc.id, tc.method, tc.payload)
		resp := responseRPC(t, conn, tc.id)
		if resp["_tag"] != "Exit" || object(resp["exit"])["_tag"] != "Success" {
			t.Fatalf("%s failed: %+v", tc.method, resp)
		}
	}

	// Verify orchestration.launchThread accepts T3 0.1.10 workspaceStrategy {type: "root"} and modelSelection.options.
	shell, err := server.shell(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	projID := object(shell["projects"].([]any)[0])["id"].(string)
	sendRPC(t, conn, "launch-root", "orchestration.launchThread", Object{
		"commandId":         "launch-root-1",
		"projectId":         projID,
		"title":             "Mobile task",
		"runtimeMode":       "full-access",
		"interactionMode":   "default",
		"modelSelection":    Object{"instanceId": "pi", "model": "test-model", "options": []any{Object{"id": "thinking", "value": "high"}}},
		"workspaceStrategy": Object{"type": "root", "branch": "main"},
		"initialMessage":    Object{"text": "hello from mobile", "attachments": []any{}},
	})
	launchRes := responseRPC(t, conn, "launch-root")
	if launchRes["_tag"] != "Exit" || object(launchRes["exit"])["_tag"] != "Success" {
		t.Fatalf("orchestration.launchThread with root strategy failed: %+v", launchRes)
	}

	// Verify thread.settle / thread.unsettle / thread.snooze / thread.unsnooze / thread.visit.
	sendRPC(t, conn, "settle", "orchestration.dispatchCommand", Object{"type": "thread.settle", "commandId": "settle-1", "threadId": "seed"})
	if r := responseRPC(t, conn, "settle"); object(r["exit"])["_tag"] != "Success" {
		t.Fatalf("thread.settle failed: %+v", r)
	}
	settledShell, err := server.shell(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(settledShell["archivedThreads"].([]any)) != 1 {
		t.Fatalf("expected settled thread in archivedThreads, got %+v", settledShell)
	}
	settledRow := object(settledShell["archivedThreads"].([]any)[0])
	if settledRow["settledOverride"] != "settled" || settledRow["settledAt"] == nil {
		t.Fatalf("expected settledOverride=settled and settledAt, got %+v", settledRow)
	}

	sendRPC(t, conn, "unsettle", "orchestration.dispatchCommand", Object{"type": "thread.unsettle", "commandId": "unsettle-1", "threadId": "seed", "reason": "user"})
	if r := responseRPC(t, conn, "unsettle"); object(r["exit"])["_tag"] != "Success" {
		t.Fatalf("thread.unsettle failed: %+v", r)
	}
	sendRPC(t, conn, "snooze", "orchestration.dispatchCommand", Object{"type": "thread.snooze", "commandId": "snooze-1", "threadId": "seed", "snoozedUntil": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
	if r := responseRPC(t, conn, "snooze"); object(r["exit"])["_tag"] != "Success" {
		t.Fatalf("thread.snooze failed: %+v", r)
	}
	sendRPC(t, conn, "unsnooze", "orchestration.dispatchCommand", Object{"type": "thread.unsnooze", "commandId": "unsnooze-1", "threadId": "seed", "reason": "user"})
	if r := responseRPC(t, conn, "unsnooze"); object(r["exit"])["_tag"] != "Success" {
		t.Fatalf("thread.unsnooze failed: %+v", r)
	}
	sendRPC(t, conn, "visit", "orchestration.dispatchCommand", Object{"type": "thread.visit", "commandId": "visit-1", "threadId": "seed", "visitedAt": time.Now().UTC().Format(time.RFC3339)})
	if r := responseRPC(t, conn, "visit"); object(r["exit"])["_tag"] != "Success" {
		t.Fatalf("thread.visit failed: %+v", r)
	}
}

func TestFilesystemBrowseAndDirectoryLaunch(t *testing.T) {
	server, _, httpServer := testServer(t)
	token := pairClient(t, server, httpServer.URL)
	conn := openRPC(t, httpServer.URL, token)
	defer conn.Close()

	root := t.TempDir()
	alpha := filepath.Join(root, "alpha-git")
	beta := filepath.Join(root, "beta-dir")
	hidden := filepath.Join(root, ".hidden-dir")
	for _, dir := range []string{filepath.Join(alpha, ".git"), beta, hidden} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	sendRPC(t, conn, "browse-default", "filesystem.browse", Object{"path": root})
	resp := responseRPC(t, conn, "browse-default")
	exit := object(resp["exit"])
	if exit["_tag"] != "Success" {
		t.Fatalf("filesystem.browse failed: %+v", resp)
	}
	val := object(exit["value"])
	entries, _ := val["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("expected 2 non-hidden directories, got %+v", entries)
	}
	first := object(entries[0])
	if first["name"] != "alpha-git" || first["isGitRepo"] != true || first["isDirectory"] != true {
		t.Fatalf("unexpected first browse entry: %+v", first)
	}
	second := object(entries[1])
	if second["name"] != "beta-dir" || second["isGitRepo"] != false {
		t.Fatalf("unexpected second browse entry: %+v", second)
	}

	sendRPC(t, conn, "browse-hidden", "filesystem.browse", Object{"path": root, "showHidden": true})
	respHidden := responseRPC(t, conn, "browse-hidden")
	entriesHidden, _ := object(object(respHidden["exit"])["value"])["entries"].([]any)
	if len(entriesHidden) != 3 {
		t.Fatalf("expected 3 entries with showHidden=true, got %+v", entriesHidden)
	}

	sendRPC(t, conn, "launch-dir", "orchestration.launchThread", Object{
		"commandId":         "launch-dir-1",
		"projectId":         "",
		"title":             "Brand new folder chat",
		"workspaceStrategy": Object{"type": "directory", "path": alpha},
		"workspacePath":     alpha,
		"initialMessage":    Object{"text": "Hello in alpha-git", "attachments": []any{}},
	})
	launchResp := responseRPC(t, conn, "launch-dir")
	if object(launchResp["exit"])["_tag"] != "Success" {
		t.Fatalf("launchThread with directory strategy failed: %+v", launchResp)
	}
	projThread := object(object(object(launchResp["exit"])["value"])["projection"])["thread"]
	if object(projThread)["worktreePath"] != nil && object(projThread)["worktreePath"] != alpha {
		// directory launches store path on Workspace; verify via shell workspaceRoot
	}
	shell, err := server.shell(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundProject := false
	for _, item := range shell["projects"].([]any) {
		if object(item)["workspaceRoot"] == alpha {
			foundProject = true
		}
	}
	if !foundProject {
		t.Fatalf("expected launched directory %q to appear in shell projects, got %+v", alpha, shell["projects"])
	}

	foreignID := "workspace_" + base64.RawURLEncoding.EncodeToString([]byte("environment:remote-host:/tmp/work"))
	sendRPC(t, conn, "launch-foreign", "orchestration.launchThread", Object{
		"commandId":         "launch-foreign-1",
		"projectId":         foreignID,
		"title":             "Should fail",
		"workspaceStrategy": Object{"type": "project"},
		"initialMessage":    Object{"text": "Rejected", "attachments": []any{}},
	})
	foreignResp := responseRPC(t, conn, "launch-foreign")
	if object(foreignResp["exit"])["_tag"] != "Failure" {
		t.Fatalf("expected foreign environment projectId to be rejected, got %+v", foreignResp)
	}

	sendRPC(t, conn, "launch-foreign-path", "orchestration.launchThread", Object{
		"commandId":         "launch-foreign-path-1",
		"projectId":         "",
		"title":             "Should fail",
		"workspaceStrategy": Object{"type": "directory", "path": "environment:remote-host:/tmp/work"},
		"initialMessage":    Object{"text": "Rejected", "attachments": []any{}},
	})
	foreignPathResp := responseRPC(t, conn, "launch-foreign-path")
	if object(foreignPathResp["exit"])["_tag"] != "Failure" {
		t.Fatalf("expected foreign environment workspacePath to be rejected, got %+v", foreignPathResp)
	}
}

func TestScratchThreadLaunchAndProjection(t *testing.T) {
	server, _, httpServer := testServer(t)
	token := pairClient(t, server, httpServer.URL)
	conn := openRPC(t, httpServer.URL, token)

	sendRPC(t, conn, "launch-scratch-1", "orchestration.launchThread", Object{
		"commandId":         "cmd-scratch-1",
		"title":             "Quick scratch chat",
		"workspaceStrategy": Object{"type": "scratch"},
	})
	resp1 := responseRPC(t, conn, "launch-scratch-1")
	if object(resp1["exit"])["_tag"] != "Success" {
		t.Fatalf("launchThread scratch failed: %+v", resp1)
	}
	val1 := object(object(resp1["exit"])["value"])
	threadID1 := text(val1, "threadId")
	expectedDir1 := filepath.Join(server.opts.DataDir, "workspaces", threadID1, "chat")
	if info, err := os.Stat(expectedDir1); err != nil || !info.IsDir() {
		t.Fatalf("expected scratch directory %s to be created, stat err=%v", expectedDir1, err)
	}

	sendRPC(t, conn, "launch-scratch-2", "orchestration.launchThread", Object{
		"commandId":         "cmd-scratch-2",
		"title":             "Second no-project chat",
		"workspaceStrategy": Object{"type": "none"},
	})
	resp2 := responseRPC(t, conn, "launch-scratch-2")
	if object(resp2["exit"])["_tag"] != "Success" {
		t.Fatalf("launchThread none failed: %+v", resp2)
	}
	val2 := object(object(resp2["exit"])["value"])
	threadID2 := text(val2, "threadId")
	expectedDir2 := filepath.Join(server.opts.DataDir, "workspaces", threadID2, "chat")
	if info, err := os.Stat(expectedDir2); err != nil || !info.IsDir() {
		t.Fatalf("expected scratch directory %s to be created, stat err=%v", expectedDir2, err)
	}

	shell, err := server.shell(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scratchProjectID := projectID(scratchProjectIdentity)
	scratchCount := 0
	for _, raw := range shell["projects"].([]any) {
		proj := object(raw)
		if text(proj, "title") == "chat" {
			t.Fatalf("scratch thread polluted projects with fake 'chat' project: %+v", shell["projects"])
		}
		if text(proj, "id") == scratchProjectID {
			scratchCount++
			if text(proj, "title") != "No project" || proj["isScratch"] != true {
				t.Fatalf("unexpected scratch project row: %+v", proj)
			}
		}
	}
	if scratchCount != 1 {
		t.Fatalf("expected exactly 1 scratch project row, got %d in %+v", scratchCount, shell["projects"])
	}
	matchedThreads := 0
	for _, raw := range shell["threads"].([]any) {
		th := object(raw)
		if id := text(th, "id"); id == threadID1 || id == threadID2 {
			matchedThreads++
			if text(th, "projectId") != scratchProjectID {
				t.Fatalf("scratch thread %s has projectId=%q, want %q", id, text(th, "projectId"), scratchProjectID)
			}
		}
	}
	if matchedThreads != 2 {
		t.Fatalf("expected both scratch threads in shell snapshot, got %d", matchedThreads)
	}
}

func TestFilesystemReadFileRPC(t *testing.T) {
	server, _, httpServer := testServer(t)
	token := pairClient(t, server, httpServer.URL)
	conn := openRPC(t, httpServer.URL, token)

	root := t.TempDir()
	mdPath := filepath.Join(root, "NOTES.md")
	mdContent := "# Hello Vantry\nLine two\nLine three\n"
	if err := os.WriteFile(mdPath, []byte(mdContent), 0o600); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(root, "sub")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codePath := filepath.Join(subDir, "main.go")
	if err := os.WriteFile(codePath, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}
	imgPath := filepath.Join(root, "pixel.png")
	if err := os.WriteFile(imgPath, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(root, "blob.dat")
	if err := os.WriteFile(binPath, []byte{0x00, 0xff, 0xfe, 0x01, 0x02}, 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Markdown file read
	sendRPC(t, conn, "read-md", "filesystem.readFile", Object{"path": mdPath})
	resp := responseRPC(t, conn, "read-md")
	if object(resp["exit"])["_tag"] != "Success" {
		t.Fatalf("read markdown failed: %+v", resp)
	}
	val := object(object(resp["exit"])["value"])
	if val["kind"] != "markdown" || val["encoding"] != "utf8" || val["content"] != mdContent || val["truncated"] != false || val["isDirectory"] != false {
		t.Fatalf("unexpected markdown response: %+v", val)
	}
	if int(val["lineCount"].(float64)) != 3 {
		t.Fatalf("expected 3 lines, got %v", val["lineCount"])
	}

	// 2. Relative path with cwd
	sendRPC(t, conn, "read-rel", "filesystem.readFile", Object{"path": "sub/main.go", "cwd": root})
	respRel := responseRPC(t, conn, "read-rel")
	valRel := object(object(respRel["exit"])["value"])
	if object(respRel["exit"])["_tag"] != "Success" || valRel["kind"] != "code" || valRel["language"] != "go" {
		t.Fatalf("unexpected relative code read: %+v", respRel)
	}

	// 3. Truncation with maxBytes
	sendRPC(t, conn, "read-trunc", "filesystem.readFile", Object{"path": mdPath, "maxBytes": 10})
	respTrunc := responseRPC(t, conn, "read-trunc")
	valTrunc := object(object(respTrunc["exit"])["value"])
	if valTrunc["truncated"] != true || len(text(valTrunc, "content")) != 10 {
		t.Fatalf("expected truncated 10-byte content, got %+v", valTrunc)
	}

	// 4. Image base64
	sendRPC(t, conn, "read-img", "filesystem.readFile", Object{"path": imgPath})
	respImg := responseRPC(t, conn, "read-img")
	valImg := object(object(respImg["exit"])["value"])
	if valImg["kind"] != "image" || valImg["encoding"] != "base64" || valImg["mimeType"] != "image/png" {
		t.Fatalf("unexpected image response: %+v", valImg)
	}
	decoded, err := base64.StdEncoding.DecodeString(text(valImg, "content"))
	if err != nil || string(decoded) != string(pngBytes) {
		t.Fatalf("base64 image mismatch: err=%v len=%d", err, len(decoded))
	}

	// 5. Binary file
	sendRPC(t, conn, "read-bin", "filesystem.readFile", Object{"path": binPath})
	respBin := responseRPC(t, conn, "read-bin")
	valBin := object(object(respBin["exit"])["value"])
	if valBin["kind"] != "binary" || valBin["encoding"] != "none" || valBin["content"] != "" {
		t.Fatalf("unexpected binary response: %+v", valBin)
	}

	// 6. Directory
	sendRPC(t, conn, "read-dir", "filesystem.readFile", Object{"path": subDir})
	respDir := responseRPC(t, conn, "read-dir")
	valDir := object(object(respDir["exit"])["value"])
	if valDir["isDirectory"] != true || valDir["kind"] != "directory" {
		t.Fatalf("unexpected directory response: %+v", valDir)
	}

	// 7. Missing file
	sendRPC(t, conn, "read-missing", "filesystem.readFile", Object{"path": filepath.Join(root, "does-not-exist.txt")})
	respMissing := responseRPC(t, conn, "read-missing")
	if object(respMissing["exit"])["_tag"] != "Failure" {
		t.Fatalf("expected failure for missing file, got %+v", respMissing)
	}
}
