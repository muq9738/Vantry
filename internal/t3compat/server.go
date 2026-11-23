// Package t3compat adapts the T3 orchestration protocol at the network boundary.
// All conversation state and execution remain owned by Vantry Core.
package t3compat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/muq9738/Vantry/internal/connectivity"
	"github.com/muq9738/Vantry/pkg/api"
)

type Object = map[string]any

const scopes = "orchestration:read orchestration:operate"
const tokenGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
const bootstrapType = "urn:t3:params:oauth:token-type:environment-bootstrap"
const accessType = "urn:ietf:params:oauth:token-type:access_token"

type Options struct {
	DataDir, EnvironmentID, Name, Version, Provider, Model string
	Workspace                                              *api.WorkspaceSpec
	Desktop                                                func(context.Context, string) (DesktopTarget, error)
	ConnectionRoutes                                       func() Object
}
type Client struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
	Scopes    string    `json:"scopes,omitempty"`
}
type LegacySession struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Scopes    string    `json:"scopes,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type authState struct {
	EnvironmentID    string          `json:"environmentId,omitempty"`
	LegacySigningKey []byte          `json:"legacySigningKey,omitempty"`
	LegacySessions   []LegacySession `json:"legacySessions,omitempty"`
	Clients          []Client        `json:"clients"`
}
type credential struct {
	code    string
	expires time.Time
}
type ticket struct {
	token   string
	expires time.Time
}
type Server struct {
	service         api.Service
	opts            Options
	mu              sync.Mutex
	auth            authState
	codes           map[string]credential
	tickets         map[string]ticket
	sockets         map[*websocket.Conn]string
	desktopRequests map[*desktopRequest]struct{}
}

func New(service api.Service, opts Options) (*Server, error) {
	s := &Server{service: service, opts: opts, codes: map[string]credential{}, tickets: map[string]ticket{}, sockets: map[*websocket.Conn]string{}}
	data, err := os.ReadFile(filepath.Join(opts.DataDir, "t3-access.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) > 0 {
		if err = json.Unmarshal(data, &s.auth); err != nil {
			return nil, err
		}
	}
	return s, nil
}
func (s *Server) saveLocked() error {
	if err := os.MkdirAll(s.opts.DataDir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(s.auth)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.opts.DataDir, ".t3-access-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), filepath.Join(s.opts.DataDir, "t3-access.json"))
}
func (s *Server) PairingLink(ctx context.Context, base string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("T3 配对需要根路径 HTTP/HTTPS 入口")
	}
	code := connectivity.Code(connectivity.Session)
	s.mu.Lock()
	s.codes = map[string]credential{code: {code: code, expires: time.Now().Add(5 * time.Minute)}}
	s.mu.Unlock()
	u.Path = "/"
	fragment := url.Values{"token": {code}}
	if s.opts.ConnectionRoutes != nil {
		if routes := s.opts.ConnectionRoutes(); len(routes) > 0 {
			data, err := json.Marshal(routes)
			if err != nil {
				return "", err
			}
			fragment.Set("routes", base64.RawURLEncoding.EncodeToString(data))
			fragment.Set("environment", s.environmentID())
		}
	}
	u.Fragment = fragment.Encode()
	return u.String(), nil
}
func (s *Server) RevokePairings() { s.mu.Lock(); s.codes = map[string]credential{}; s.mu.Unlock() }
func (s *Server) Disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.sockets {
		_ = conn.Close()
	}
	s.cancelDesktopRequestsLocked("")
}
func (s *Server) RevokeAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prior := s.auth
	s.auth = authState{EnvironmentID: prior.EnvironmentID}
	if err := s.saveLocked(); err != nil {
		s.auth = prior
		return err
	}
	s.codes = map[string]credential{}
	s.tickets = map[string]ticket{}
	s.cancelDesktopRequestsLocked("")
	for conn := range s.sockets {
		_ = conn.Close()
	}
	return nil
}
func (s *Server) Clients() []Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	clients := append([]Client(nil), s.auth.Clients...)
	for i := range clients {
		clients[i].Token = ""
	}
	now := time.Now()
	for _, legacy := range s.auth.LegacySessions {
		if now.Before(legacy.ExpiresAt) {
			clients = append(clients, Client{
				ID:        legacy.ID,
				Name:      legacy.Name,
				CreatedAt: legacy.CreatedAt,
				ExpiresAt: legacy.ExpiresAt,
				Scopes:    legacy.Scopes,
			})
		}
	}
	return clients
}
func (s *Server) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	priorClients := append([]Client(nil), s.auth.Clients...)
	priorLegacy := append([]LegacySession(nil), s.auth.LegacySessions...)
	token := ""
	revokedSID := ""
	for i, client := range s.auth.Clients {
		if client.ID == id {
			token = client.Token
			s.auth.Clients = append(s.auth.Clients[:i:i], s.auth.Clients[i+1:]...)
			break
		}
	}
	if token == "" {
		for i, legacy := range s.auth.LegacySessions {
			if legacy.ID == id {
				revokedSID = legacy.ID
				s.auth.LegacySessions = append(s.auth.LegacySessions[:i:i], s.auth.LegacySessions[i+1:]...)
				break
			}
		}
	}
	if token == "" && revokedSID == "" {
		return errors.New("找不到已授权的 T3 设备")
	}
	if err := s.saveLocked(); err != nil {
		s.auth.Clients = priorClients
		s.auth.LegacySessions = priorLegacy
		return err
	}
	for request := range s.desktopRequests {
		if (token != "" && request.token == token) || (revokedSID != "" && s.tokenMatchesLegacySIDLocked(request.token, revokedSID)) {
			request.cancel()
		}
	}
	for conn, key := range s.sockets {
		if (token != "" && key == token) || (revokedSID != "" && s.tokenMatchesLegacySIDLocked(key, revokedSID)) {
			_ = conn.Close()
		}
	}
	return nil
}

type legacySessionClaims struct {
	Kind   string   `json:"kind"`
	SID    string   `json:"sid"`
	Exp    int64    `json:"exp"`
	Label  string   `json:"label"`
	Scopes []string `json:"scopes"`
}

func (s *Server) verifyLegacyTokenLocked(token string) (legacySessionClaims, bool) {
	if len(s.auth.LegacySigningKey) < 16 || token == "" {
		return legacySessionClaims{}, false
	}
	payloadB64, sigB64, ok := strings.Cut(token, ".")
	if !ok || payloadB64 == "" || sigB64 == "" || strings.Contains(sigB64, ".") {
		return legacySessionClaims{}, false
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return legacySessionClaims{}, false
	}
	mac := hmac.New(sha256.New, s.auth.LegacySigningKey)
	_, _ = mac.Write([]byte(payloadB64))
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return legacySessionClaims{}, false
	}
	rawClaims, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return legacySessionClaims{}, false
	}
	var claims legacySessionClaims
	if json.Unmarshal(rawClaims, &claims) != nil || claims.SID == "" || (claims.Kind != "" && claims.Kind != "session") {
		return legacySessionClaims{}, false
	}
	if claims.Exp > 0 {
		var expTime time.Time
		if claims.Exp > 1e12 {
			expTime = time.UnixMilli(claims.Exp)
		} else {
			expTime = time.Unix(claims.Exp, 0)
		}
		if !time.Now().Before(expTime) {
			return legacySessionClaims{}, false
		}
	}
	return claims, true
}

func (s *Server) tokenMatchesLegacySIDLocked(token, sid string) bool {
	claims, ok := s.verifyLegacyTokenLocked(token)
	return ok && claims.SID == sid
}

var knownT3Scopes = map[string]bool{
	"orchestration:read":    true,
	"orchestration:operate": true,
	"settings:write":        true,
	"providers:manage":      true,
	"environment:maintain":  true,
	"preview:operate":       true,
	"diagnostics:read":      true,
	"terminal:read":         true,
	"terminal:operate":      true,
	"filesystem:read":       true,
	"filesystem:write":      true,
	"review:write":          true,
	"source-control:write":  true,
	"access:read":           true,
	"access:write":          true,
	"relay:read":            true,
	"relay:write":           true,
}

var legacyOnlyT3Permissions = map[string]bool{
	"orchestration:read":    true,
	"orchestration:operate": true,
	"terminal:operate":      true,
	"review:write":          true,
	"access:read":           true,
	"access:write":          true,
	"relay:read":            true,
	"relay:write":           true,
}

func sessionScopesAndPermissions(raw string) ([]string, []string) {
	var allScopes []string
	var legacyList []string
	seen := map[string]bool{}
	hasFineGrained := false
	for _, sc := range strings.Fields(raw) {
		if !knownT3Scopes[sc] || seen[sc] {
			continue
		}
		seen[sc] = true
		allScopes = append(allScopes, sc)
		if legacyOnlyT3Permissions[sc] {
			legacyList = append(legacyList, sc)
		} else {
			hasFineGrained = true
		}
	}
	if len(allScopes) == 0 {
		allScopes = []string{"orchestration:read", "orchestration:operate"}
		legacyList = []string{"orchestration:read", "orchestration:operate"}
	} else if len(legacyList) == 0 {
		legacyList = []string{"orchestration:read", "orchestration:operate"}
	}
	if !hasFineGrained {
		return legacyList, nil
	}
	return legacyList, allScopes
}

func negotiateExchangeScopes(wanted string) (string, bool) {
	wanted = strings.TrimSpace(wanted)
	if wanted == "" {
		return scopes, true
	}
	var granted []string
	seen := map[string]bool{}
	hasOrchestration := false
	for _, sc := range strings.Fields(wanted) {
		if !knownT3Scopes[sc] {
			return "", false
		}
		if sc == "orchestration:read" || sc == "orchestration:operate" {
			hasOrchestration = true
		}
		if !seen[sc] {
			seen[sc] = true
			granted = append(granted, sc)
		}
	}
	if !hasOrchestration || len(granted) == 0 {
		return "", false
	}
	return strings.Join(granted, " "), true
}

func extractRequestToken(req *http.Request) (string, string) {
	if auth := strings.TrimSpace(req.Header.Get("Authorization")); strings.HasPrefix(auth, "Bearer ") {
		if tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")); tok != "" {
			return tok, "bearer-access-token"
		}
	}
	for _, cookie := range req.Cookies() {
		if cookie.Name == "vantry_session" || strings.HasPrefix(cookie.Name, "t3_session") {
			if tok := strings.TrimSpace(cookie.Value); tok != "" {
				return tok, "browser-session-cookie"
			}
		}
	}
	return "", ""
}

func (s *Server) authenticated(token string) (Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authenticatedLocked(token)
}

func (s *Server) authenticatedLocked(token string) (Client, bool) {
	if token == "" {
		return Client{}, false
	}
	now := time.Now()
	for _, client := range s.auth.Clients {
		if client.Token == token && now.Before(client.ExpiresAt) {
			return client, true
		}
	}
	if claims, ok := s.verifyLegacyTokenLocked(token); ok {
		for _, legacy := range s.auth.LegacySessions {
			if legacy.ID == claims.SID && now.Before(legacy.ExpiresAt) {
				sc := legacy.Scopes
				if sc == "" && len(claims.Scopes) > 0 {
					sc = strings.Join(claims.Scopes, " ")
				}
				return Client{
					ID:        legacy.ID,
					Name:      nonempty(legacy.Name, claims.Label),
					Token:     token,
					CreatedAt: legacy.CreatedAt,
					ExpiresAt: legacy.ExpiresAt,
					Scopes:    nonempty(sc, scopes),
				}, true
			}
		}
	}
	return Client{}, false
}
func utcISO(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

func utcISONullable(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return utcISO(*t)
}

func (s *Server) environmentID() string {
	s.mu.Lock()
	envID := s.auth.EnvironmentID
	s.mu.Unlock()
	if envID == "" {
		return s.opts.EnvironmentID
	}
	return envID
}

func (s *Server) descriptor() Object {
	envID := s.environmentID()
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	if arch != "arm64" && arch != "x64" {
		arch = "other"
	}
	result := Object{"environmentId": envID, "label": s.opts.Name, "platform": Object{"os": runtime.GOOS, "arch": arch}, "serverVersion": s.opts.Version, "orchestrationProtocolVersion": 2,
		"capabilities": Object{"remoteDesktop": s.opts.Desktop != nil, "repositoryIdentity": false, "connectionProbe": true, "attachmentUploads": false, "questionAttachments": false, "pullRequests": false, "threadStop": true, "threadPinning": true, "threadSettlement": true, "threadSnooze": true, "agentActivityPublishing": false}}
	if s.opts.ConnectionRoutes != nil {
		result["connectionRoutes"] = s.opts.ConnectionRoutes()
	}
	return result
}
func (s *Server) sessionCookieName() string {
	s.mu.Lock()
	envID := strings.TrimSpace(s.auth.EnvironmentID)
	s.mu.Unlock()
	if envID != "" {
		sum := sha256.Sum256([]byte(envID))
		return "t3_session_" + hex.EncodeToString(sum[:])[:12]
	}
	return "vantry_session"
}
func (s *Server) authDescriptor() Object {
	return Object{"policy": "remote-reachable", "bootstrapMethods": []string{"one-time-token"}, "sessionMethods": []string{"bearer-access-token", "browser-session-cookie"}, "sessionCookieName": s.sessionCookieName()}
}
func authError(w http.ResponseWriter) {
	connectivity.JSON(w, 401, Object{"_tag": "EnvironmentAuthInvalidError", "code": "auth_invalid", "reason": "invalid_credential", "traceId": connectivity.Token()})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if origin := strings.TrimSpace(req.Header.Get("Origin")); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Vary", "Origin")
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, DPoP, x-t3-orchestration-protocol")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Cache-Control", "no-store")
	if req.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}
	path := req.URL.Path
	if strings.HasPrefix(path, "/api/desktop/") {
		s.serveDesktop(w, req)
		return
	}
	if path == "/ws" {
		s.socket(w, req)
		return
	}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	token, method := extractRequestToken(req)
	client, ok := s.authenticated(token)
	defer func() {
		sid := ""
		if ok {
			sid = client.ID
		}
		log.Printf("t3compat http method=%s path=%s status=%d auth=%t authMethod=%s sid=%s ua=%q", req.Method, path, rec.status, ok, method, sid, req.UserAgent())
	}()

	if path == "/.well-known/t3/environment" && req.Method == "GET" {
		connectivity.JSON(rec, 200, s.descriptor())
		return
	}
	if path == "/oauth/token" {
		s.exchange(rec, req)
		return
	}
	if path == "/api/auth/browser-session" {
		s.browserSession(rec, req)
		return
	}
	if path == "/api/auth/session" && req.Method == "GET" {
		result := Object{"authenticated": ok, "auth": s.authDescriptor()}
		if ok {
			if s.opts.ConnectionRoutes != nil {
				result["connectionRoutes"] = s.opts.ConnectionRoutes()
			}
			grantedScopes, grantedPerms := sessionScopesAndPermissions(client.Scopes)
			result["scopes"] = grantedScopes
			result["sessionMethod"] = nonempty(method, "bearer-access-token")
			result["expiresAt"] = utcISO(client.ExpiresAt)
			if len(grantedPerms) > 0 {
				result["permissions"] = grantedPerms
			}
		}
		connectivity.JSON(rec, 200, result)
		return
	}
	if !ok {
		authError(rec)
		return
	}
	if path == "/api/auth/websocket-ticket" || path == "/api/auth/websocket-ticket/rpc" || path == "/api/auth/websocket-ticket/media" {
		if req.Method != "POST" {
			http.Error(rec, "Method not allowed", 405)
			return
		}
		key := connectivity.Token()
		expiry := time.Now().UTC().Add(5 * time.Minute)
		s.mu.Lock()
		for id, item := range s.tickets {
			if !time.Now().Before(item.expires) {
				delete(s.tickets, id)
			}
		}
		if len(s.tickets) >= 1024 {
			s.mu.Unlock()
			http.Error(rec, "Capacity", 429)
			return
		}
		s.tickets[key] = ticket{token, expiry}
		s.mu.Unlock()
		connectivity.JSON(rec, 200, Object{"ticket": key, "expiresAt": utcISO(expiry)})
		return
	}
	if req.Method == "GET" && path == "/api/orchestration/shell" {
		snapshot, err := s.shell(req.Context())
		s.reply(rec, snapshot, err)
		return
	}
	if req.Method == "GET" && strings.HasPrefix(path, "/api/orchestration/threads/") {
		tail := strings.TrimPrefix(path, "/api/orchestration/threads/")
		id, suffix, _ := strings.Cut(tail, "/")
		projection, sequence, err := s.projection(req.Context(), id)
		if err != nil {
			s.reply(rec, nil, err)
			return
		}
		result := Object{"snapshotSequence": sequence, "projection": projection}
		switch suffix {
		case "":
		case "bounded":
			result["historyCursor"] = nil
			result["hasMoreHistory"] = false
			result["latestLocalTurnOrdinal"] = nil
		case "history":
			result = Object{"snapshotSequence": sequence, "items": []any{}, "nextCursor": nil, "hasMoreHistory": false}
		case "transcript":
			result = Object{"threadId": id, "title": projection["thread"].(Object)["title"], "updatedAt": projection["updatedAt"], "items": projection["visibleTurnItems"]}
		default:
			http.NotFound(rec, req)
			return
		}
		connectivity.JSON(rec, 200, result)
		return
	}
	http.NotFound(rec, req)
}
func (s *Server) reply(w http.ResponseWriter, value any, err error) {
	if err != nil {
		connectivity.JSON(w, 400, Object{"message": err.Error()})
		return
	}
	connectivity.JSON(w, 200, value)
}
func (s *Server) exchange(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, 8192)
	if req.ParseForm() != nil || req.PostForm.Get("grant_type") != tokenGrant || req.PostForm.Get("subject_token_type") != bootstrapType || req.PostForm.Get("requested_token_type") != accessType {
		http.Error(w, "Unsupported token exchange", 400)
		return
	}
	grantedScopes, ok := negotiateExchangeScopes(req.PostForm.Get("scope"))
	if !ok {
		http.Error(w, "Scope not supported", 400)
		return
	}
	code := connectivity.NormalizeCode(req.PostForm.Get("subject_token"))
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, ok := s.codes[code]
	if !ok || !time.Now().Before(credential.expires) {
		authError(w)
		return
	}
	if len(s.auth.Clients) >= 128 {
		http.Error(w, "请先撤销不再使用的设备", 429)
		return
	}
	now := time.Now().UTC()
	client := Client{ID: connectivity.Token(), Name: req.PostForm.Get("client_label"), Token: connectivity.Token(), ExpiresAt: now.AddDate(1, 0, 0), CreatedAt: now, Scopes: grantedScopes}
	if client.Name == "" {
		client.Name = "T3 客户端"
	}
	s.auth.Clients = append(s.auth.Clients, client)
	if err := s.saveLocked(); err != nil {
		s.auth.Clients = s.auth.Clients[:len(s.auth.Clients)-1]
		http.Error(w, "无法保存设备授权", 500)
		return
	}
	delete(s.codes, code)
	connectivity.JSON(w, 200, Object{"access_token": client.Token, "issued_token_type": accessType, "token_type": "Bearer", "expires_in": int(time.Until(client.ExpiresAt).Seconds()), "scope": client.Scopes})
}
func (s *Server) browserSession(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, 8192)
	var payload struct {
		Credential string `json:"credential"`
		Label      string `json:"label"`
	}
	if json.NewDecoder(req.Body).Decode(&payload) != nil || strings.TrimSpace(payload.Credential) == "" {
		http.Error(w, "Invalid browser session request", 400)
		return
	}
	code := connectivity.NormalizeCode(payload.Credential)
	cookieName := s.sessionCookieName()
	s.mu.Lock()
	credential, ok := s.codes[code]
	if !ok || !time.Now().Before(credential.expires) {
		s.mu.Unlock()
		authError(w)
		return
	}
	if len(s.auth.Clients) >= 128 {
		s.mu.Unlock()
		http.Error(w, "请先撤销不再使用的设备", 429)
		return
	}
	label := strings.TrimSpace(payload.Label)
	if label == "" {
		label = "T3 浏览器会话"
	}
	now := time.Now().UTC()
	client := Client{
		ID:        connectivity.Token(),
		Name:      label,
		Token:     connectivity.Token(),
		ExpiresAt: now.AddDate(0, 1, 0),
		CreatedAt: now,
		Scopes:    scopes,
	}
	s.auth.Clients = append(s.auth.Clients, client)
	if err := s.saveLocked(); err != nil {
		s.auth.Clients = s.auth.Clients[:len(s.auth.Clients)-1]
		s.mu.Unlock()
		http.Error(w, "无法保存设备授权", 500)
		return
	}
	delete(s.codes, code)
	s.mu.Unlock()

	secure := req.TLS != nil || strings.EqualFold(strings.TrimSpace(req.Header.Get("X-Forwarded-Proto")), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    client.Token,
		Path:     "/",
		Expires:  client.ExpiresAt,
		MaxAge:   int(time.Until(client.ExpiresAt).Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	grantedScopes, grantedPerms := sessionScopesAndPermissions(client.Scopes)
	res := Object{
		"authenticated": true,
		"scopes":        grantedScopes,
		"sessionMethod": "browser-session-cookie",
		"expiresAt":     utcISO(client.ExpiresAt),
	}
	if len(grantedPerms) > 0 {
		res["permissions"] = grantedPerms
	}
	connectivity.JSON(w, 200, res)
}
