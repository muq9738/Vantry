package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// drainStopDelay lets a drain acceptance reach the caller before the listener
// and the socket disappear, so that caller can start a matching Core.
const drainStopDelay = 50 * time.Millisecond

// Server serves one api.Service on a private Unix domain socket.
type Server struct {
	socketPath string
	socketInfo os.FileInfo
	service    api.Service
	listener   *net.UnixListener
	httpServer *http.Server

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	mu          sync.Mutex
	closed      bool
	serving     bool
	connections map[net.Conn]struct{}
	closeOnce   sync.Once
}

// Listen creates socketPath in a private parent directory. It never removes an
// existing path; stale-socket recovery belongs to the process startup layer.
func Listen(socketPath string, service api.Service) (*Server, error) {
	if service == nil {
		return nil, errors.New("local: nil service")
	}
	if socketPath == "" {
		return nil, errors.New("local: empty socket path")
	}
	absolutePath, err := filepath.Abs(socketPath)
	if err != nil {
		return nil, fmt.Errorf("local: resolve socket path: %w", err)
	}
	absolutePath = filepath.Clean(absolutePath)
	if err := checkPrivateParent(filepath.Dir(absolutePath)); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(absolutePath); err == nil {
		return nil, fmt.Errorf("local: socket path already exists: %s", absolutePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("local: inspect socket path: %w", err)
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: absolutePath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("local: listen on Unix socket: %w", err)
	}
	// The Server performs an identity-checked removal instead of allowing
	// net.UnixListener to unlink whatever later appears at socketPath.
	listener.SetUnlinkOnClose(false)
	cleanup := func() {
		_ = listener.Close()
		info, statErr := os.Lstat(absolutePath)
		if statErr == nil && info.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(absolutePath)
		}
	}

	if err := os.Chmod(absolutePath, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("local: set socket permissions: %w", err)
	}
	socketInfo, err := os.Lstat(absolutePath)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("local: inspect created socket: %w", err)
	}
	if socketInfo.Mode()&os.ModeSocket == 0 {
		cleanup()
		return nil, fmt.Errorf("local: created path is not a socket: %s", absolutePath)
	}

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	s := &Server{
		socketPath:      absolutePath,
		socketInfo:      socketInfo,
		service:         service,
		listener:        listener,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		connections:     make(map[net.Conn]struct{}),
	}
	s.httpServer = &http.Server{
		Handler: compressResponses(http.HandlerFunc(s.handle)),
		BaseContext: func(net.Listener) context.Context {
			return lifecycleCtx
		},
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			s.trackConnection(conn)
			return ctx
		},
		ConnState: func(conn net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				s.untrackConnection(conn)
			}
		},
	}
	return s, nil
}

// Serve runs until the listener fails, Close is called, or ctx is canceled.
// Cancellation also closes active requests and subscriptions.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if s.serving {
		s.mu.Unlock()
		return errors.New("local: Serve called more than once")
	}
	s.serving = true
	s.mu.Unlock()

	stopCancellation := context.AfterFunc(ctx, func() {
		_ = s.Close(context.Background())
	})
	defer stopCancellation()

	err := s.httpServer.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

// Close stops the listener, cancels all request contexts, closes active
// connections, and removes only the socket originally created by Listen.
func (s *Server) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.lifecycleCancel()
		_ = s.listener.Close()
		s.closeConnections()
		s.removeOwnSocket()
	})
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) trackConnection(conn net.Conn) {
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.connections[conn] = struct{}{}
	}
	s.mu.Unlock()
	if closed {
		_ = conn.Close()
	}
}

func (s *Server) untrackConnection(conn net.Conn) {
	s.mu.Lock()
	delete(s.connections, conn)
	s.mu.Unlock()
}

func (s *Server) closeConnections() {
	s.mu.Lock()
	connections := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (s *Server) removeOwnSocket() {
	info, err := os.Lstat(s.socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 || !os.SameFile(s.socketInfo, info) {
		return
	}
	_ = os.Remove(s.socketPath)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/health":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !requireNoQuery(w, r) {
			return
		}
		health, err := s.service.Health(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, health)
	case "/v1/snapshot":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if !requireNoQuery(w, r) {
			return
		}
		snapshot, err := s.service.Snapshot(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	case "/v1/status":
		s.handleStatus(w, r)
	case "/v1/admin/shutdown-if-idle":
		s.handleShutdownIfIdle(w, r)
	case "/v1/threads/summaries":
		s.handleThreadSummaries(w, r)
	case "/v1/threads/detail":
		s.handleThreadDetail(w, r)
	case "/v1/models", "/v1/workspace", "/v1/terminal":
		s.handleWorkbenchQuery(w, r)
	case "/v1/workbench":
		s.handleWorkbench(w, r)
	case pathOrchestrationCapabilities,
		pathOrchestrationDelegate,
		pathOrchestrationTaskStatus,
		pathOrchestrationTaskCancel,
		pathOrchestrationThreadsList,
		pathOrchestrationThreadsRead,
		pathOrchestrationThreadsLaunch,
		pathOrchestrationThreadsCreateBatch,
		pathOrchestrationThreadsSend,
		pathOrchestrationThreadsWait,
		pathOrchestrationThreadsSearch:
		s.handleOrchestration(w, r)
	case "/v1/commands":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if !requireNoQuery(w, r) {
			return
		}
		s.handleCommand(w, r)
	case "/v1/events":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.handleEvents(w, r)
	default:
		writeError(w, &api.Error{Code: api.NotFound, Message: "unknown endpoint"})
	}
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	apiErr := invalid("method %s is not allowed; use %s", r.Method, method)
	writeJSON(w, http.StatusMethodNotAllowed, apiErr)
	return false
}

func requireNoQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery == "" {
		return true
	}
	writeError(w, invalid("endpoint does not accept query parameters"))
	return false
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCommandBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var command *api.Command
	if err := decoder.Decode(&command); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, invalid("command body exceeds %d bytes", maxCommandBytes))
		} else {
			writeError(w, invalid("decode command JSON: %v", err))
		}
		return
	}
	if command == nil {
		writeError(w, invalid("command body must be a JSON object"))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			writeError(w, invalid("command body must contain exactly one JSON object"))
		} else {
			writeError(w, invalid("decode trailing command data: %v", err))
		}
		return
	}
	if command.Version != api.ProtocolVersion {
		writeError(w, &api.Error{
			Code:    api.VersionMismatch,
			Message: fmt.Sprintf("protocol version %d is required", api.ProtocolVersion),
		})
		return
	}
	receipt, err := s.service.Dispatch(r.Context(), *command)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if len(query) != 1 || len(query["after"]) != 1 {
		writeError(w, invalid("events requires exactly one after cursor"))
		return
	}
	after, err := strconv.ParseInt(query.Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeError(w, invalid("after must be a non-negative integer sequence"))
		return
	}
	subscription, err := s.service.Subscribe(r.Context(), after)
	if err != nil {
		writeError(w, err)
		return
	}
	if subscription == nil {
		writeError(w, errors.New("local: service returned a nil subscription"))
		return
	}
	if subscription.Close != nil {
		defer subscription.Close()
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, errors.New("local: streaming is unsupported"))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Bound network batching by bytes and latency, without changing NDJSON
	// records or their order. Local Unix clients retain immediate delivery.
	batch := w.Header().Get("Content-Encoding") == "gzip"
	var pending bytes.Buffer
	encoder := json.NewEncoder(&pending)
	var timer *time.Timer
	var tick <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	flush := func() error {
		if timer != nil {
			timer.Stop()
		}
		tick = nil
		if pending.Len() == 0 {
			return nil
		}
		control := http.NewResponseController(w)
		_ = control.SetWriteDeadline(time.Now().Add(15 * time.Second))
		defer control.SetWriteDeadline(time.Time{})
		if _, err := w.Write(pending.Bytes()); err != nil {
			return err
		}
		pending.Reset()
		return control.Flush()
	}
	events := subscription.Events
	errorsIn := subscription.Errors
	for {
		if events == nil && errorsIn == nil {
			_ = flush()
			return
		}
		select {
		case <-tick:
			if flush() != nil {
				return
			}
		case <-r.Context().Done():
			return
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if err := encoder.Encode(streamEnvelope{Event: &event}); err != nil {
				return
			}
			if !batch || pending.Len() >= 4<<10 {
				if flush() != nil {
					return
				}
			} else if tick == nil {
				if timer == nil {
					timer = time.NewTimer(8 * time.Millisecond)
				} else {
					timer.Reset(8 * time.Millisecond)
				}
				tick = timer.C
			}
		case err, ok := <-errorsIn:
			if !ok {
				errorsIn = nil
				continue
			}
			if err == nil {
				continue
			}
			apiErr := asAPIError(err)
			_ = encoder.Encode(streamEnvelope{Error: apiErr})
			_ = flush()
			return
		}
	}
}

// handleStatus reports the connected Core's structured capabilities. A service
// that cannot report them answers NotFound, which is how a client recognises a
// legacy Core instead of silently attaching to it.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !requireNoQuery(w, r) {
		return
	}
	reporter, ok := s.service.(api.StatusReporter)
	if !ok {
		writeError(w, &api.Error{Code: api.NotFound, Message: "当前 Core 未提供能力状态接口"})
		return
	}
	status, err := reporter.ServerStatus(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleShutdownIfIdle asks the Core to retire itself. The Core refuses with
// Conflict while a run is active; this transport never interrupts that work.
func (s *Server) handleShutdownIfIdle(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireNoQuery(w, r) {
		return
	}
	controller, ok := s.service.(api.AdminController)
	if !ok {
		writeError(w, &api.Error{Code: api.NotFound, Message: "当前 Core 未提供空闲停机接口"})
		return
	}
	if err := controller.RequestDrainShutdown(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		State string `json:"state"`
	}{State: "draining"})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	s.stopAfterDrain()
}

// stopAfterDrain retires this listener once the acceptance has been written.
// The Core has already stopped accepting work, and a matching Core can only
// listen on the socket after it is gone.
func (s *Server) stopAfterDrain() {
	go func() {
		time.Sleep(drainStopDelay)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Close(shutdownCtx)
	}()
}

func (s *Server) handleThreadSummaries(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !requireNoQuery(w, r) {
		return
	}
	summaries, err := s.threadSummaries(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summaries)
}

// threadSummaries lets a backend serve the projection itself. Otherwise the
// transport trims one snapshot, so the bounded payload still applies to a Core
// that only knows the full-snapshot contract.
func (s *Server) threadSummaries(ctx context.Context) (api.ThreadSummaries, error) {
	if reader, ok := s.service.(api.ThreadReader); ok {
		return reader.ThreadSummaries(ctx)
	}
	snapshot, err := s.service.Snapshot(ctx)
	if err != nil {
		return api.ThreadSummaries{}, err
	}
	return api.BuildThreadSummaries(snapshot), nil
}

func (s *Server) handleThreadDetail(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	values := r.URL.Query()
	if len(values["thread"]) != 1 || strings.TrimSpace(values.Get("thread")) == "" {
		writeError(w, invalid("threads/detail requires exactly one non-empty thread"))
		return
	}
	if len(values["limit"]) > 1 {
		writeError(w, invalid("threads/detail accepts at most one limit"))
		return
	}
	for name := range values {
		if name != "thread" && name != "limit" {
			writeError(w, invalid("unknown query parameter %q", name))
			return
		}
	}
	query := api.ThreadDetailQuery{ThreadID: strings.TrimSpace(values.Get("thread"))}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 {
			writeError(w, invalid("limit must be a non-negative integer"))
			return
		}
		query.LimitRuns = limit
	}
	detail, err := s.threadDetail(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) threadDetail(ctx context.Context, query api.ThreadDetailQuery) (api.ThreadDetail, error) {
	if reader, ok := s.service.(api.ThreadReader); ok {
		return reader.ThreadDetail(ctx, query)
	}
	snapshot, err := s.service.Snapshot(ctx)
	if err != nil {
		return api.ThreadDetail{}, err
	}
	return api.BuildThreadDetail(snapshot, query)
}
