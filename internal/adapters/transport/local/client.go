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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

var errClientClosed = errors.New("local: client is closed")

// Client accesses a Server through its Unix domain socket. It never dials TCP.
type Client struct {
	socketPath string
	httpClient *http.Client
	transport  *http.Transport

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	closeOnce       sync.Once
}

// Dial constructs a client. The socket is opened lazily by each operation.
func Dial(socketPath string) *Client {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath:      socketPath,
		transport:       transport,
		httpClient:      &http.Client{Transport: transport},
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
}

// DialNetwork reuses the Core protocol over an authenticated native transport.
// The supplied dialer returns an established TLS stream with h2/http/1.1 ALPN.
func DialNetwork(dial func(context.Context, string, string) (net.Conn, error), bearer string) *Client {
	life, cancel := context.WithCancel(context.Background())
	transport := &http.Transport{
		DialTLSContext:        dial,
		ForceAttemptHTTP2:     true,
		MaxConnsPerHost:       16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       5 * time.Minute,
		ResponseHeaderTimeout: 20 * time.Second,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout:  20 * time.Second,
			PingTimeout:      10 * time.Second,
			WriteByteTimeout: 30 * time.Second,
		},
	}
	return &Client{transport: transport, httpClient: &http.Client{Transport: bearerTransport{transport, bearer}}, lifecycleCtx: life, lifecycleCancel: cancel}
}

type bearerTransport struct {
	base   http.RoundTripper
	bearer string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request := req.Clone(req.Context())
	request.URL.Scheme = "https"
	request.Header.Set("Authorization", "Bearer "+t.bearer)
	return t.base.RoundTrip(request)
}

// DialAdapter exposes a foreign session protocol through the Core client API.
func DialAdapter(adapter http.RoundTripper) *Client {
	life, cancel := context.WithCancel(context.Background())
	return &Client{httpClient: &http.Client{Transport: adapter}, lifecycleCtx: life, lifecycleCancel: cancel}
}

// Close cancels active operations and subscriptions and closes idle sockets.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.lifecycleCancel()
		if c.transport != nil {
			c.transport.CloseIdleConnections()
		}
	})
	return nil
}

func (c *Client) Health(ctx context.Context) (api.Health, error) {
	var health api.Health
	if err := c.doJSON(ctx, http.MethodGet, "/v1/health", nil, &health); err != nil {
		return api.Health{}, err
	}
	return health, nil
}

func (c *Client) Snapshot(ctx context.Context) (api.Snapshot, error) {
	var snapshot api.Snapshot
	if err := c.doJSON(ctx, http.MethodGet, "/v1/snapshot", nil, &snapshot); err != nil {
		return api.Snapshot{}, err
	}
	return snapshot, nil
}

func (c *Client) Dispatch(ctx context.Context, command api.Command) (api.Receipt, error) {
	body, err := json.Marshal(command)
	if err != nil {
		return api.Receipt{}, fmt.Errorf("local: encode command: %w", err)
	}
	if len(body) > maxCommandBytes {
		return api.Receipt{}, invalid("command body exceeds %d bytes", maxCommandBytes)
	}
	var receipt api.Receipt
	if err := c.doJSON(ctx, http.MethodPost, "/v1/commands", body, &receipt); err != nil {
		return api.Receipt{}, err
	}
	return receipt, nil
}

// ServerStatus reads the connected Core's structured capabilities. A NotFound
// answer means the Core has no status endpoint: it is a legacy Core whose
// capabilities cannot be verified.
func (c *Client) ServerStatus(ctx context.Context) (api.ServerStatus, error) {
	var status api.ServerStatus
	err := c.doJSON(ctx, http.MethodGet, "/v1/status", nil, &status)
	if api.CodeOf(err) == api.NotFound {
		return api.ServerStatus{}, &api.Error{Code: api.NotFound, Message: "当前 Core 未提供 /v1/status 能力状态接口（旧版 Core）"}
	}
	return status, err
}

// RequestDrainShutdown asks an idle Core to retire so a matching one can take
// the socket. A busy Core answers Conflict, which callers must report instead of
// retrying with a process kill.
func (c *Client) RequestDrainShutdown(ctx context.Context) error {
	var result struct {
		State string `json:"state"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/v1/admin/shutdown-if-idle", nil, &result)
	if api.CodeOf(err) == api.NotFound {
		return &api.Error{Code: api.NotFound, Message: "当前 Core 未提供空闲停机接口（旧版 Core）"}
	}
	return err
}

// ThreadSummaries lists conversations without their stored reply text.
func (c *Client) ThreadSummaries(ctx context.Context) (api.ThreadSummaries, error) {
	var summaries api.ThreadSummaries
	err := c.doJSON(ctx, http.MethodGet, "/v1/threads/summaries", nil, &summaries)
	if api.CodeOf(err) == api.NotFound {
		return api.ThreadSummaries{}, &api.Error{Code: api.VersionMismatch, Message: "当前 Core 尚未提供轻量会话摘要接口，请更新并重启本地服务"}
	}
	return summaries, err
}

// ThreadDetail reads one thread, optionally bounded to its most recent runs. A
// NotFound here is the thread lookup result; capability verification is what
// distinguishes a legacy Core that has no such endpoint at all.
func (c *Client) ThreadDetail(ctx context.Context, query api.ThreadDetailQuery) (api.ThreadDetail, error) {
	if strings.TrimSpace(query.ThreadID) == "" {
		return api.ThreadDetail{}, &api.Error{Code: api.InvalidCommand, Message: "threadId is required"}
	}
	if query.LimitRuns < 0 {
		return api.ThreadDetail{}, &api.Error{Code: api.InvalidCommand, Message: "limitRuns must not be negative"}
	}
	path := "/v1/threads/detail?thread=" + url.QueryEscape(query.ThreadID)
	if query.LimitRuns > 0 {
		path += "&limit=" + strconv.Itoa(query.LimitRuns)
	}
	var detail api.ThreadDetail
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &detail); err != nil {
		return api.ThreadDetail{}, err
	}
	return detail, nil
}

// GetJSON sends an authenticated GET request using the client's transport and
// decodes a single JSON response body into result.
func (c *Client) GetJSON(ctx context.Context, path string, result any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, result)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body []byte, result any) error {
	requestCtx, cancel, stopClientCancellation, err := c.requestContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	defer stopClientCancellation()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(requestCtx, method, "http://vantry.local"+path, reader)
	if err != nil {
		return fmt.Errorf("local: create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("local: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeHTTPError(resp)
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("local: decode %s response: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("local: decode %s response: multiple JSON values", path)
		}
		return fmt.Errorf("local: decode %s trailing response: %w", path, err)
	}
	return nil
}

func (c *Client) Subscribe(ctx context.Context, after int64) (*api.Subscription, error) {
	requestCtx, cancel, stopClientCancellation, err := c.requestContext(ctx)
	if err != nil {
		return nil, err
	}
	path := "/v1/events?after=" + url.QueryEscape(strconv.FormatInt(after, 10))
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://vantry.local"+path, nil)
	if err != nil {
		cancel()
		stopClientCancellation()
		return nil, fmt.Errorf("local: create subscription request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()
		stopClientCancellation()
		return nil, fmt.Errorf("local: subscribe: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		defer cancel()
		defer stopClientCancellation()
		return nil, decodeHTTPError(resp)
	}

	events := make(chan api.Event)
	errorsOut := make(chan error, 1)
	var closeOnce sync.Once
	closeSubscription := func() {
		closeOnce.Do(func() {
			cancel()
			_ = resp.Body.Close()
		})
	}
	subscription := &api.Subscription{
		Events: events,
		Errors: errorsOut,
		Close:  closeSubscription,
	}
	go c.readEvents(requestCtx, resp.Body, events, errorsOut, cancel, stopClientCancellation)
	return subscription, nil
}

func (c *Client) requestContext(ctx context.Context) (context.Context, context.CancelFunc, func() bool, error) {
	if ctx == nil {
		return nil, nil, nil, errors.New("local: nil context")
	}
	select {
	case <-c.lifecycleCtx.Done():
		return nil, nil, nil, errClientClosed
	default:
	}
	requestCtx, cancel := context.WithCancel(ctx)
	stopClientCancellation := context.AfterFunc(c.lifecycleCtx, cancel)
	return requestCtx, cancel, stopClientCancellation, nil
}

func (c *Client) readEvents(
	ctx context.Context,
	body io.ReadCloser,
	events chan<- api.Event,
	errorsOut chan<- error,
	cancel context.CancelFunc,
	stopClientCancellation func() bool,
) {
	defer close(events)
	defer close(errorsOut)
	defer body.Close()
	defer cancel()
	defer stopClientCancellation()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), maxEventBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			sendStreamError(ctx, errorsOut, errors.New("local: empty NDJSON line"))
			return
		}
		envelope, err := decodeStreamEnvelope(line)
		if err != nil {
			sendStreamError(ctx, errorsOut, err)
			return
		}
		if envelope.Error != nil {
			sendStreamError(ctx, errorsOut, envelope.Error)
			return
		}
		select {
		case events <- *envelope.Event:
		case <-ctx.Done():
			return
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		sendStreamError(ctx, errorsOut, fmt.Errorf("local: read event stream: %w", err))
	}
}

func decodeStreamEnvelope(line []byte) (streamEnvelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var envelope streamEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return streamEnvelope{}, fmt.Errorf("local: decode NDJSON envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return streamEnvelope{}, errors.New("local: NDJSON line contains trailing data")
	}
	if (envelope.Event == nil) == (envelope.Error == nil) {
		return streamEnvelope{}, errors.New("local: NDJSON line must contain exactly one of event or error")
	}
	return envelope, nil
}

func sendStreamError(ctx context.Context, errorsOut chan<- error, err error) {
	select {
	case errorsOut <- err:
	case <-ctx.Done():
	}
}

func decodeHTTPError(resp *http.Response) error {
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBytes))
	var apiErr api.Error
	if err := decoder.Decode(&apiErr); err != nil {
		return fmt.Errorf("local: HTTP %s: decode error response: %w", resp.Status, err)
	}
	if apiErr.Code == "" {
		return fmt.Errorf("local: HTTP %s: error response has no code", resp.Status)
	}
	return &apiErr
}

var (
	_ api.Service             = (*Client)(nil)
	_ api.StatusReporter      = (*Client)(nil)
	_ api.AdminController     = (*Client)(nil)
	_ api.ThreadReader        = (*Client)(nil)
	_ api.OrchestrationBroker = (*Client)(nil)
)
