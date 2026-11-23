package t3compat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/connectivity"
)

// ErrDesktopNotSharing indicates that desktop sharing has not been enabled on
// the target computer, independent of whether LAN or relay transport was requested.
var ErrDesktopNotSharing = errors.New("desktop sharing is not active")

// DesktopTarget stays inside Core. The relay's viewer credential is never
// returned to a mobile client; every request uses its existing device session.
type DesktopTarget struct {
	Relay, SessionID, Token string
	Authorize               func(context.Context, string) error
	Direct, RelayAvailable  bool
	Transport               http.RoundTripper
}

type desktopRequest struct {
	token  string
	cancel context.CancelFunc
}

func (s *Server) cancelDesktopRequestsLocked(token string) {
	for request := range s.desktopRequests {
		if token == "" || request.token == token {
			request.cancel()
		}
	}
}

func desktopActionName(path string) string {
	switch {
	case path == "/api/desktop/session":
		return "session"
	case strings.HasSuffix(path, "/request"):
		return "request"
	case strings.HasSuffix(path, "/signal"):
		return "signal"
	case strings.HasSuffix(path, "/turn"):
		return "turn"
	default:
		return "status"
	}
}

type desktopStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *desktopStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *desktopStatusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *desktopStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *desktopStatusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *desktopStatusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack unsupported")
	}
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return hijacker.Hijack()
}

func (s *Server) serveDesktop(w http.ResponseWriter, req *http.Request) {
	started := time.Now()
	rec := &desktopStatusWriter{ResponseWriter: w}
	action := desktopActionName(req.URL.Path)
	transport := "-"
	sid := ""
	errKind := ""
	defer func() {
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf(
			"t3compat desktop method=%s action=%s transport=%s status=%d dur=%dms sid=%s err=%q",
			req.Method,
			action,
			transport,
			status,
			time.Since(started).Milliseconds(),
			sid,
			errKind,
		)
	}()

	token, method := extractRequestToken(req)
	// Native mobile access uses the paired device's bearer credential. Do not
	// turn an ambient browser session cookie into screen or input access.
	if method != "bearer-access-token" {
		errKind = "unauthorized_method"
		authError(rec)
		return
	}
	s.mu.Lock()
	client, ok := s.authenticatedLocked(token)
	if !ok {
		s.mu.Unlock()
		errKind = "invalid_token"
		authError(rec)
		return
	}
	if !strings.Contains(" "+nonempty(client.Scopes, scopes)+" ", " orchestration:operate ") {
		s.mu.Unlock()
		errKind = "missing_operate_scope"
		http.Error(rec, "Desktop access requires an operating device authorization", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithDeadline(req.Context(), client.ExpiresAt)
	lease := &desktopRequest{token: token, cancel: cancel}
	if s.desktopRequests == nil {
		s.desktopRequests = map[*desktopRequest]struct{}{}
	}
	s.desktopRequests[lease] = struct{}{}
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.desktopRequests, lease)
		s.mu.Unlock()
	}()
	if s.opts.Desktop == nil {
		errKind = "desktop_disabled"
		http.NotFound(rec, req)
		return
	}
	selector := "direct"
	if req.URL.Path == "/api/desktop/session" {
		if req.Method != http.MethodPost || (req.URL.RawQuery != "" && req.URL.RawQuery != "transport=relay") {
			errKind = "method_or_query"
			http.Error(rec, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if req.URL.RawQuery == "transport=relay" {
			selector = "relay"
		}
	} else {
		const prefix = "/api/desktop/v1/sessions/"
		if !strings.HasPrefix(req.URL.Path, prefix) {
			errKind = "invalid_prefix"
			http.NotFound(rec, req)
			return
		}
		selector = strings.SplitN(strings.TrimPrefix(req.URL.Path, prefix), "/", 2)[0]
		if selector == "" || selector == "direct" || selector == "relay" {
			errKind = "invalid_session_selector"
			http.NotFound(rec, req)
			return
		}
		sid = selector
	}
	target, err := s.opts.Desktop(ctx, selector)
	if err != nil {
		if req.URL.Path != "/api/desktop/session" {
			errKind = "session_not_found"
			http.NotFound(rec, req)
			return
		}
		if selector == "relay" && !errors.Is(err, ErrDesktopNotSharing) {
			errKind = "relay_unavailable"
			connectivity.JSON(rec, http.StatusBadGateway, Object{"code": "desktop_relay_unavailable", "message": "The fallback desktop relay is unavailable."})
			return
		}
		errKind = "not_sharing"
		connectivity.JSON(rec, http.StatusConflict, Object{"code": "desktop_not_sharing", "message": "Turn on desktop sharing in Vantry on this computer."})
		return
	}
	sid = target.SessionID
	if target.Direct {
		transport = "lan"
	} else {
		transport = "wss-tcp"
	}
	base, err := url.Parse(target.Relay)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || target.SessionID == "" || strings.ContainsAny(target.SessionID, "/?#%") || target.Token == "" || target.Authorize == nil {
		errKind = "invalid_target"
		http.Error(rec, "Desktop transport unavailable", http.StatusBadGateway)
		return
	}
	if req.URL.Path == "/api/desktop/session" {
		connectivity.JSON(rec, http.StatusOK, Object{
			"sessionId":      target.SessionID,
			"transport":      transport,
			"relayAvailable": target.RelayAvailable,
			"platform":       runtime.GOOS,
		})
		return
	}
	path := "/api/desktop/v1/sessions/" + target.SessionID
	suffix := strings.TrimPrefix(req.URL.Path, path)
	if !strings.HasPrefix(req.URL.Path, path) || (suffix != "" && suffix != "/request" && suffix != "/signal" && suffix != "/turn") {
		errKind = "session_mismatch"
		http.NotFound(rec, req)
		return
	}
	if suffix == "/request" {
		if req.Method != http.MethodPost || req.URL.RawQuery != "" {
			errKind = "method_or_query"
			http.Error(rec, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Trust the owning computer's stored device name, not a caller-supplied
		// label. The computer's sharing switch and OS permissions still apply.
		if err := target.Authorize(ctx, nonempty(client.Name, "Vantry for iPhone")); err != nil {
			errKind = "authorize_failed"
			http.Error(rec, "Desktop connection could not be authorized", http.StatusBadGateway)
			return
		}
		connectivity.JSON(rec, http.StatusOK, Object{"waiting": true})
		return
	}
	if req.Method != http.MethodGet || (suffix == "/signal" && req.URL.RawQuery != "role=viewer") || (suffix != "/signal" && req.URL.RawQuery != "") {
		errKind = "operation_not_allowed"
		http.Error(rec, "Desktop operation not allowed", http.StatusForbidden)
		return
	}
	if target.Direct && suffix == "/turn" {
		errKind = "turn_direct_not_applicable"
		http.NotFound(rec, req)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(base)
	if target.Transport != nil {
		proxy.Transport = target.Transport
	}
	proxy.FlushInterval = -1
	originalDirector := proxy.Director
	proxy.Director = func(out *http.Request) {
		out.URL.Path = strings.TrimPrefix(out.URL.Path, "/api")
		if target.Direct {
			out.URL.Path = strings.TrimPrefix(out.URL.Path, "/desktop")
		}
		out.URL.RawPath = ""
		originalDirector(out)
		out.Host = base.Host
		out.Header.Del("Cookie")
		out.Header.Del("Origin")
		out.Header.Set("Authorization", "Bearer "+target.Token)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		response.Header.Del("Set-Cookie")
		response.Header.Del("Location")
		if target.Direct && suffix == "" && response.StatusCode == http.StatusOK {
			var room Object
			if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&room); err != nil {
				return err
			}
			response.Body.Close()
			delete(room, "hostToken")
			delete(room, "viewerToken")
			room["approved"] = true
			room["relayAvailable"] = target.RelayAvailable
			data, err := json.Marshal(room)
			if err != nil {
				return err
			}
			response.Body = io.NopCloser(bytes.NewReader(data))
			response.ContentLength = int64(len(data))
			response.Header.Del("Content-Length")
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		if err != nil {
			errKind = "proxy_error"
		}
		http.Error(w, "Desktop transport unavailable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(rec, req.WithContext(ctx))
}
