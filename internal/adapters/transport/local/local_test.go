package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

type fakeService struct {
	healthFunc    func(context.Context) (api.Health, error)
	dispatchFunc  func(context.Context, api.Command) (api.Receipt, error)
	snapshotFunc  func(context.Context) (api.Snapshot, error)
	subscribeFunc func(context.Context, int64) (*api.Subscription, error)
}

func (f *fakeService) Health(ctx context.Context) (api.Health, error) {
	if f.healthFunc != nil {
		return f.healthFunc(ctx)
	}
	return api.Health{Version: api.ProtocolVersion, State: "ready"}, nil
}

func (f *fakeService) Dispatch(ctx context.Context, command api.Command) (api.Receipt, error) {
	if f.dispatchFunc != nil {
		return f.dispatchFunc(ctx, command)
	}
	return api.Receipt{}, errors.New("unexpected Dispatch call")
}

func (f *fakeService) Snapshot(ctx context.Context) (api.Snapshot, error) {
	if f.snapshotFunc != nil {
		return f.snapshotFunc(ctx)
	}
	return api.Snapshot{}, nil
}

func (f *fakeService) Subscribe(ctx context.Context, after int64) (*api.Subscription, error) {
	if f.subscribeFunc != nil {
		return f.subscribeFunc(ctx, after)
	}
	return nil, errors.New("unexpected Subscribe call")
}

type testServer struct {
	server   *Server
	socket   string
	cancel   context.CancelFunc
	done     chan error
	waitOnce sync.Once
	waitErr  error
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	// Keep Unix socket paths below Darwin's 104-byte sun_path limit.
	dir, err := os.MkdirTemp("/tmp", "vantry-local-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startTestServer(t *testing.T, service api.Service) *testServer {
	t.Helper()
	socket := filepath.Join(privateTempDir(t), "vantry.sock")
	server, err := Listen(socket, service)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	ts := &testServer{server: server, socket: socket, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		_ = server.Close(context.Background())
		ts.wait(t)
	})
	return ts
}

func (ts *testServer) wait(t *testing.T) {
	t.Helper()
	ts.waitOnce.Do(func() {
		select {
		case ts.waitErr = <-ts.done:
		case <-time.After(3 * time.Second):
			ts.waitErr = errors.New("Serve() did not stop")
		}
	})
	if ts.waitErr != nil {
		t.Errorf("Serve() error = %v", ts.waitErr)
	}
}

func startTestClient(t *testing.T, ts *testServer) *Client {
	t.Helper()
	client := Dial(ts.socket)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestClientServiceRoundTrips(t *testing.T) {
	wantCommand := api.Command{
		Version:   api.ProtocolVersion,
		CommandID: "command-1",
		Kind:      api.SendMessage,
		ThreadID:  "thread-1",
		Message:   "hello over UDS",
	}
	service := &fakeService{
		healthFunc: func(context.Context) (api.Health, error) {
			return api.Health{Version: api.ProtocolVersion, State: "ready"}, nil
		},
		snapshotFunc: func(context.Context) (api.Snapshot, error) {
			return api.Snapshot{
				Sequence: 9,
				Threads:  []api.Thread{{ID: "thread-1", Title: "test"}},
			}, nil
		},
		dispatchFunc: func(_ context.Context, got api.Command) (api.Receipt, error) {
			if got.CommandID != wantCommand.CommandID || got.Message != wantCommand.Message {
				t.Errorf("Dispatch() command = %#v, want %#v", got, wantCommand)
			}
			return api.Receipt{CommandID: got.CommandID, Accepted: true, ThreadID: got.ThreadID, RunID: "run-1", Sequence: 10}, nil
		},
	}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)
	ctx := context.Background()

	health, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if health.Version != api.ProtocolVersion || health.State != "ready" {
		t.Fatalf("Health() = %#v", health)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.Sequence != 9 || len(snapshot.Threads) != 1 || snapshot.Threads[0].ID != "thread-1" {
		t.Fatalf("Snapshot() = %#v", snapshot)
	}
	receipt, err := client.Dispatch(ctx, wantCommand)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if !receipt.Accepted || receipt.RunID != "run-1" || receipt.Sequence != 10 {
		t.Fatalf("Dispatch() = %#v", receipt)
	}
}

func TestClientMapsTypedErrors(t *testing.T) {
	codes := []api.ErrorCode{
		api.InvalidCommand,
		api.VersionMismatch,
		api.NotFound,
		api.Conflict,
		api.NotReady,
		api.ResyncRequired,
		api.InternalError,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			service := &fakeService{dispatchFunc: func(context.Context, api.Command) (api.Receipt, error) {
				return api.Receipt{}, &api.Error{Code: code, Message: "expected failure"}
			}}
			ts := startTestServer(t, service)
			client := startTestClient(t, ts)
			_, err := client.Dispatch(context.Background(), api.Command{Version: api.ProtocolVersion})
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("Dispatch() error = %T %v, want *api.Error", err, err)
			}
			if apiErr.Code != code || apiErr.Message != "expected failure" {
				t.Fatalf("Dispatch() error = %#v", apiErr)
			}
		})
	}
}

func TestCommandVersionMismatchDoesNotReachService(t *testing.T) {
	called := false
	service := &fakeService{dispatchFunc: func(context.Context, api.Command) (api.Receipt, error) {
		called = true
		return api.Receipt{}, nil
	}}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)
	_, err := client.Dispatch(context.Background(), api.Command{Version: api.ProtocolVersion + 1})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.VersionMismatch {
		t.Fatalf("Dispatch() error = %v, want version_mismatch", err)
	}
	if called {
		t.Fatal("Dispatch reached backend with mismatched version")
	}
}

func TestEventStreamAndTypedStreamError(t *testing.T) {
	events := make(chan api.Event)
	errorsIn := make(chan error)
	backendClosed := make(chan struct{})
	var closeOnce sync.Once
	service := &fakeService{subscribeFunc: func(_ context.Context, after int64) (*api.Subscription, error) {
		if after != 41 {
			t.Errorf("Subscribe() after = %d, want 41", after)
		}
		return &api.Subscription{
			Events: events,
			Errors: errorsIn,
			Close: func() {
				closeOnce.Do(func() { close(backendClosed) })
			},
		}, nil
	}}
	ts := startTestServer(t, service)
	client := startTestClient(t, ts)
	subscription, err := client.Subscribe(context.Background(), 41)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer subscription.Close()

	wantEvent := api.Event{Sequence: 42, ID: "event-42", Type: "run.output", RunID: "run-1", Data: json.RawMessage(`{"delta":"hello"}`)}
	events <- wantEvent
	select {
	case got := <-subscription.Events:
		if got.Sequence != 42 || got.ID != "event-42" || string(got.Data) != string(wantEvent.Data) {
			t.Fatalf("event = %#v, want %#v", got, wantEvent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	errorsIn <- &api.Error{Code: api.ResyncRequired, Message: "client fell behind"}
	select {
	case err := <-subscription.Errors:
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ResyncRequired || apiErr.Message != "client fell behind" {
			t.Fatalf("stream error = %#v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream error")
	}
	waitClosed(t, backendClosed, "backend subscription after stream error")
}

func TestSubscriptionCancellationReleasesBackend(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel func(context.CancelFunc, *api.Subscription, *Client)
	}{
		{
			name: "request context",
			cancel: func(cancel context.CancelFunc, _ *api.Subscription, _ *Client) {
				cancel()
			},
		},
		{
			name: "subscription close",
			cancel: func(_ context.CancelFunc, subscription *api.Subscription, _ *Client) {
				subscription.Close()
				subscription.Close()
			},
		},
		{
			name: "client close",
			cancel: func(_ context.CancelFunc, _ *api.Subscription, client *Client) {
				_ = client.Close()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			backendClosed := make(chan struct{})
			var closeOnce sync.Once
			service := &fakeService{subscribeFunc: func(context.Context, int64) (*api.Subscription, error) {
				return &api.Subscription{
					Events: make(chan api.Event),
					Errors: make(chan error),
					Close: func() {
						closeOnce.Do(func() { close(backendClosed) })
					},
				}, nil
			}}
			ts := startTestServer(t, service)
			client := Dial(ts.socket)
			ctx, cancel := context.WithCancel(context.Background())
			subscription, err := client.Subscribe(ctx, 0)
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			test.cancel(cancel, subscription, client)
			waitClosed(t, backendClosed, "backend subscription")
			_ = client.Close()
			cancel()
		})
	}
}

func TestMalformedRequests(t *testing.T) {
	ts := startTestServer(t, &fakeService{})
	tests := []struct {
		name       string
		method     string
		target     string
		body       string
		wantStatus int
		wantCode   api.ErrorCode
	}{
		{name: "malformed JSON", method: http.MethodPost, target: "/v1/commands", body: `{"version":`, wantStatus: http.StatusBadRequest, wantCode: api.InvalidCommand},
		{name: "null command", method: http.MethodPost, target: "/v1/commands", body: `null`, wantStatus: http.StatusBadRequest, wantCode: api.InvalidCommand},
		{name: "unknown field", method: http.MethodPost, target: "/v1/commands", body: `{"version":1,"unknown":true}`, wantStatus: http.StatusBadRequest, wantCode: api.InvalidCommand},
		{name: "multiple JSON values", method: http.MethodPost, target: "/v1/commands", body: `{"version":1} {"version":1}`, wantStatus: http.StatusBadRequest, wantCode: api.InvalidCommand},
		{name: "malformed cursor", method: http.MethodGet, target: "/v1/events?after=nope", wantStatus: http.StatusBadRequest, wantCode: api.InvalidCommand},
		{name: "negative cursor", method: http.MethodGet, target: "/v1/events?after=-1", wantStatus: http.StatusBadRequest, wantCode: api.InvalidCommand},
		{name: "missing cursor", method: http.MethodGet, target: "/v1/events", wantStatus: http.StatusBadRequest, wantCode: api.InvalidCommand},
		{name: "duplicate cursor", method: http.MethodGet, target: "/v1/events?after=0&after=1", wantStatus: http.StatusBadRequest, wantCode: api.InvalidCommand},
		{name: "malformed query", method: http.MethodGet, target: "/v1/health?after=0;bad=1", wantStatus: http.StatusBadRequest, wantCode: api.InvalidCommand},
		{name: "unknown path", method: http.MethodGet, target: "/v1/unknown", wantStatus: http.StatusNotFound, wantCode: api.NotFound},
		{name: "unknown method", method: http.MethodPost, target: "/v1/health", wantStatus: http.StatusMethodNotAllowed, wantCode: api.InvalidCommand},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp, body := rawHTTPRequest(t, ts.socket, test.method, test.target, test.body)
			defer resp.Body.Close()
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, test.wantStatus, body)
			}
			var apiErr api.Error
			if err := json.Unmarshal(body, &apiErr); err != nil {
				t.Fatalf("decode error body %q: %v", body, err)
			}
			if apiErr.Code != test.wantCode || apiErr.Message == "" {
				t.Fatalf("error body = %#v", apiErr)
			}
		})
	}
}

func TestCommandBodySizeLimit(t *testing.T) {
	ts := startTestServer(t, &fakeService{})
	body := fmt.Sprintf(`{"version":1,"message":%q}`, strings.Repeat("x", maxCommandBytes))
	resp, responseBody := rawHTTPRequest(t, ts.socket, http.MethodPost, "/v1/commands", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, responseBody)
	}
	var apiErr api.Error
	if err := json.Unmarshal(responseBody, &apiErr); err != nil {
		t.Fatal(err)
	}
	if apiErr.Code != api.InvalidCommand || !strings.Contains(apiErr.Message, "exceeds") {
		t.Fatalf("error = %#v, want explicit size error", apiErr)
	}
}

func TestListenSocketPermissions(t *testing.T) {
	ts := startTestServer(t, &fakeService{})
	info, err := os.Lstat(ts.socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("mode = %v, want socket", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket permissions = %04o, want 0600", got)
	}
}

func TestListenRejectsSharedParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(parent, "vantry.sock")
	if _, err := Listen(socket, &fakeService{}); err == nil {
		t.Fatal("Listen() succeeded with shared parent")
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket exists after rejected Listen: %v", err)
	}
}

func TestListenDoesNotRemoveExistingPath(t *testing.T) {
	socket := filepath.Join(privateTempDir(t), "vantry.sock")
	if err := os.WriteFile(socket, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(socket, &fakeService{}); err == nil {
		t.Fatal("Listen() succeeded with existing path")
	}
	content, err := os.ReadFile(socket)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep me" {
		t.Fatalf("existing path content = %q", content)
	}
}

func TestCloseRemovesOnlyOriginalSocket(t *testing.T) {
	t.Run("owned socket", func(t *testing.T) {
		socket := filepath.Join(privateTempDir(t), "vantry.sock")
		server, err := Listen(socket, &fakeService{})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := server.Close(context.Background()); err != nil {
			t.Fatalf("second Close() error = %v", err)
		}
		if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned socket still exists: %v", err)
		}
	})

	t.Run("replacement file", func(t *testing.T) {
		socket := filepath.Join(privateTempDir(t), "vantry.sock")
		server, err := Listen(socket, &fakeService{})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(socket); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(socket, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := server.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(socket)
		if err != nil {
			t.Fatalf("replacement was removed: %v", err)
		}
		if string(content) != "replacement" {
			t.Fatalf("replacement content = %q", content)
		}
	})
}

func TestServeCancellationStopsService(t *testing.T) {
	ts := startTestServer(t, &fakeService{})
	client := startTestClient(t, ts)
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts.cancel()
	ts.wait(t)
	if err := ts.server.Close(context.Background()); err != nil {
		t.Fatalf("Close() after cancellation error = %v", err)
	}
	if _, err := os.Lstat(ts.socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still exists after Serve cancellation: %v", err)
	}
}

func TestDisconnectReleasesBackendSubscription(t *testing.T) {
	backendClosed := make(chan struct{})
	var closeOnce sync.Once
	service := &fakeService{subscribeFunc: func(context.Context, int64) (*api.Subscription, error) {
		return &api.Subscription{
			Events: make(chan api.Event),
			Errors: make(chan error),
			Close: func() {
				closeOnce.Do(func() { close(backendClosed) })
			},
		}, nil
	}}
	ts := startTestServer(t, service)
	conn, resp := rawStreamingRequest(t, ts.socket)
	_ = conn.Close()
	_ = resp.Body.Close()
	waitClosed(t, backendClosed, "backend subscription after disconnect")
}

func TestCloseUnblocksSlowEventWriter(t *testing.T) {
	backendClosed := make(chan struct{})
	events := make(chan api.Event)
	var closeOnce sync.Once
	service := &fakeService{subscribeFunc: func(context.Context, int64) (*api.Subscription, error) {
		return &api.Subscription{
			Events: events,
			Errors: make(chan error),
			Close: func() {
				closeOnce.Do(func() { close(backendClosed) })
			},
		}, nil
	}}
	ts := startTestServer(t, service)
	_, resp := rawStreamingRequest(t, ts.socket)
	defer resp.Body.Close()

	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		event := api.Event{Sequence: 1, Data: json.RawMessage(fmt.Sprintf(`{"data":%q}`, strings.Repeat("x", 256*1024)))}
		for {
			select {
			case events <- event:
			case <-backendClosed:
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ts.server.Close(ctx); err != nil {
		t.Fatalf("Close() with blocked writer error = %v", err)
	}
	waitClosed(t, backendClosed, "blocked backend subscription")
	waitClosed(t, producerDone, "event producer")
	ts.wait(t)
}

func rawHTTPRequest(t *testing.T, socket, method, target, body string) (*http.Response, []byte) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{
		Method: method,
		URL:    &url.URL{Path: target},
		Host:   "vantry.local",
		Header: make(http.Header),
	}
	if targetURL, err := url.ParseRequestURI(target); err == nil {
		req.URL = targetURL
	}
	if body != "" {
		req.Body = io.NopCloser(strings.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, responseBody
}

func rawStreamingRequest(t *testing.T, socket string) (net.Conn, *http.Response) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://vantry.local/v1/events?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = conn.Close()
		t.Fatalf("subscription status = %d, body = %s", resp.StatusCode, body)
	}
	return conn, resp
}

func waitClosed(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestNDJSONEnvelopeValidation(t *testing.T) {
	validLines := [][]byte{
		[]byte(`{"event":{"sequence":1}}`),
		[]byte(`{"error":{"code":"resync_required","message":"retry"}}`),
	}
	for _, line := range validLines {
		if _, err := decodeStreamEnvelope(line); err != nil {
			t.Errorf("decodeStreamEnvelope(%s) error = %v", line, err)
		}
	}
	invalidLines := [][]byte{
		[]byte(`{}`),
		[]byte(`{"event":{},"error":{}}`),
		[]byte(`not-json`),
		[]byte(`{"event":{}} {"event":{}}`),
	}
	for _, line := range invalidLines {
		if _, err := decodeStreamEnvelope(line); err == nil {
			t.Errorf("decodeStreamEnvelope(%s) succeeded", line)
		}
	}
}

func TestClientUsesUnixSocket(t *testing.T) {
	ts := startTestServer(t, &fakeService{})
	client := startTestClient(t, ts)
	address := client.transport.DialContext
	conn, err := address(context.Background(), "tcp", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("Unix DialContext() error = %v", err)
	}
	if conn.RemoteAddr().Network() != "unix" {
		t.Fatalf("network = %s, want unix", conn.RemoteAddr().Network())
	}
	_ = conn.Close()
}

func TestNDJSONEventShape(t *testing.T) {
	events := make(chan api.Event, 1)
	errorsIn := make(chan error)
	service := &fakeService{subscribeFunc: func(context.Context, int64) (*api.Subscription, error) {
		return &api.Subscription{Events: events, Errors: errorsIn, Close: func() {}}, nil
	}}
	ts := startTestServer(t, service)
	events <- api.Event{Sequence: 7}
	conn, resp := rawStreamingRequest(t, ts.socket)
	defer resp.Body.Close()
	defer conn.Close()
	line, err := bufio.NewReader(resp.Body).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &object); err != nil {
		t.Fatal(err)
	}
	if len(object) != 1 || object["event"] == nil {
		t.Fatalf("NDJSON line = %s", line)
	}
}
