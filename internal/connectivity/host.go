package connectivity

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const maxHostTunnels = 128

var hostStableResetWindow = 30 * time.Second

type RelayError struct {
	Stage      string
	StatusCode int
	Message    string
	Cause      error
}

func (e *RelayError) Error() string { return e.Message }
func (e *RelayError) Unwrap() error { return e.Cause }

func WebSocketURL(base, path string, query url.Values) string {
	u, _ := url.Parse(strings.TrimRight(base, "/") + path)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.RawQuery = query.Encode()
	return u.String()
}

type Host struct {
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	control   *websocket.Conn
	conns     map[net.Conn]struct{}
	tunnels   int
	state     string
	lastError string
	instance  string
	wg        sync.WaitGroup
	once      sync.Once
}

// StartHost reconnects in process. backend maps a channel to a local address;
// neither a peer nor the relay can select arbitrary destinations on the host.
func StartHost(ctx context.Context, base, route, key string, backend func(context.Context, string) (net.Conn, error)) *Host {
	return StartHostWithInstance(ctx, base, route, key, Token(), backend)
}

// StartHostWithInstance lets a stable host identity pass an instance tag so the
// relay can probe and replace its own stale half-open control socket quickly.
func StartHostWithInstance(ctx context.Context, base, route, key, instance string, backend func(context.Context, string) (net.Conn, error)) *Host {
	if instance == "" {
		instance = Token()
	}
	life, cancel := context.WithCancel(ctx)
	h := &Host{ctx: life, cancel: cancel, conns: map[net.Conn]struct{}{}, state: "connecting", instance: instance}
	h.wg.Add(1)
	go func() { defer h.wg.Done(); h.run(base, route, key, backend) }()
	return h
}
func (h *Host) State() string     { h.mu.Lock(); defer h.mu.Unlock(); return h.state }
func (h *Host) LastError() string { h.mu.Lock(); defer h.mu.Unlock(); return h.lastError }
func (h *Host) setStatus(state, lastError string) {
	h.mu.Lock()
	h.state = state
	h.lastError = lastError
	h.mu.Unlock()
}
func (h *Host) releaseTunnel() {
	h.mu.Lock()
	if h.tunnels > 0 {
		h.tunnels--
	}
	h.mu.Unlock()
}
func (h *Host) Close() {
	h.once.Do(func() {
		h.cancel()
		h.mu.Lock()
		if h.control != nil {
			_ = h.control.Close()
		}
		for c := range h.conns {
			_ = c.Close()
		}
		h.mu.Unlock()
		h.wg.Wait()
	})
}
func jitteredBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	half := base / 2
	if half <= 0 {
		return base
	}
	return base*3/4 + time.Duration(rand.Int64N(int64(half)))
}
func (h *Host) run(base, route, key string, backend func(context.Context, string) (net.Conn, error)) {
	retry := 500 * time.Millisecond
	for h.ctx.Err() == nil {
		dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		query := url.Values{"route": {route}, "instance": {h.instance}}
		conn, resp, err := dialer.DialContext(h.ctx, WebSocketURL(base, "/relay/host", query), http.Header{"Authorization": {"Bearer " + key}})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			connectedAt := time.Now()
			h.mu.Lock()
			if h.ctx.Err() != nil {
				h.mu.Unlock()
				_ = conn.Close()
				return
			}
			h.control = conn
			h.state = "online"
			h.lastError = ""
			h.mu.Unlock()
			conn.SetReadLimit(4096)
			stopHeartbeat := KeepAlive(conn)
			for {
				var open Open
				if readErr := conn.ReadJSON(&open); readErr != nil {
					err = readErr
					break
				}
				if open.ID == "" || (open.Channel != "core" && open.Channel != "stream" && open.Channel != "t3" && open.Channel != "http") {
					continue
				}
				h.mu.Lock()
				if h.ctx.Err() != nil || h.tunnels >= maxHostTunnels {
					h.mu.Unlock()
					continue
				}
				h.tunnels++
				h.wg.Add(1)
				h.mu.Unlock()
				go func() {
					defer h.wg.Done()
					defer h.releaseTunnel()
					h.attach(base, route, key, open, backend)
				}()
			}
			_ = conn.Close()
			stopHeartbeat()
			h.mu.Lock()
			if h.control == conn {
				h.control = nil
			}
			h.mu.Unlock()
			if time.Since(connectedAt) >= hostStableResetWindow {
				retry = 500 * time.Millisecond
			}
		}
		if resp != nil && (resp.StatusCode == 401 || resp.StatusCode == 403) {
			h.setStatus("denied", fmt.Sprintf("中继拒绝主机注册（HTTP %d）", resp.StatusCode))
			return
		}
		if resp != nil && resp.StatusCode == http.StatusConflict {
			h.setStatus("conflict", "中继路由已被另一连接占用（HTTP 409）")
		} else {
			errText := ""
			if err != nil {
				errText = err.Error()
			} else if resp != nil {
				errText = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			h.setStatus("reconnecting", errText)
		}
		timer := time.NewTimer(jitteredBackoff(retry))
		select {
		case <-timer.C:
		case <-h.ctx.Done():
			timer.Stop()
			return
		}
		retry = min(15*time.Second, retry*2)
	}
}
func (h *Host) attach(base, route, key string, open Open, backend func(context.Context, string) (net.Conn, error)) {
	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	upstream, err := backend(ctx, open.Channel)
	if err != nil {
		cancel()
		return
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, WebSocketURL(base, "/relay/attach", url.Values{"route": {route}, "id": {open.ID}}), http.Header{"Authorization": {"Bearer " + key}})
	cancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		_ = upstream.Close()
		return
	}
	stream := NewStream(conn)
	h.mu.Lock()
	if h.ctx.Err() != nil {
		h.mu.Unlock()
		_ = upstream.Close()
		_ = stream.Close()
		return
	}
	h.conns[upstream] = struct{}{}
	h.conns[stream] = struct{}{}
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.conns, upstream); delete(h.conns, stream); h.mu.Unlock() }()
	Splice(h.ctx, stream, upstream)
}
func DialTunnel(ctx context.Context, base, route string) (net.Conn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 12 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, WebSocketURL(base, "/relay/connect", url.Values{"route": {route}}), nil)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if err != nil {
		msg := "中继连接失败，请检查地址及主机在线状态"
		if status == http.StatusTooManyRequests {
			msg = "中继连接过于频繁或已达容量上限，请稍后重试"
		} else if status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
			msg = "目标主机离线或中继转发超时"
		}
		return nil, &RelayError{Stage: "relay_dial", StatusCode: status, Message: msg, Cause: err}
	}
	return NewStream(conn), nil
}
