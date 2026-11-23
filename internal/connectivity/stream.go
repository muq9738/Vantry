// Package connectivity contains Vantry's native network transports. It does not
// start Node, SSH, a browser, or an external reverse proxy.
package connectivity

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	streamPingInterval     = 20 * time.Second
	streamPongTimeout      = 65 * time.Second
	streamPingWriteTimeout = 5 * time.Second
)

// Stream carries a byte stream in binary WebSocket messages. One reader and one
// writer may run concurrently, as required by net/http and TLS.
type Stream struct {
	conn         *websocket.Conn
	read         io.Reader
	writes       sync.Mutex
	once         sync.Once
	done         chan struct{}
	lastActivity atomic.Int64
}

func NewStream(conn *websocket.Conn) *Stream {
	conn.SetReadLimit(1 << 20)
	s := &Stream{
		conn: conn,
		done: make(chan struct{}),
	}
	s.lastActivity.Store(time.Now().UnixNano())
	// Track Pong via activity timestamp instead of SetReadDeadline so TLS/HTTP
	// deadline resets (SetReadDeadline(time.Time{})) cannot disable keepalive.
	conn.SetPongHandler(func(string) error {
		s.lastActivity.Store(time.Now().UnixNano())
		return nil
	})
	pingInterval := streamPingInterval
	pongTimeout := streamPongTimeout
	writeTimeout := streamPingWriteTimeout
	if pingInterval > 0 {
		go s.keepAlive(pingInterval, pongTimeout, writeTimeout)
	}
	return s
}

func (s *Stream) keepAlive(pingInterval, pongTimeout, writeTimeout time.Duration) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if pongTimeout > 0 && time.Since(time.Unix(0, s.lastActivity.Load())) > pongTimeout {
				_ = s.Close()
				return
			}
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				_ = s.Close()
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *Stream) Read(p []byte) (int, error) {
	for {
		if s.read == nil {
			kind, reader, err := s.conn.NextReader()
			if err != nil {
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				continue
			}
			s.lastActivity.Store(time.Now().UnixNano())
			s.read = reader
		}
		n, err := s.read.Read(p)
		if n > 0 {
			s.lastActivity.Store(time.Now().UnixNano())
		}
		if err == io.EOF {
			s.read = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}
func (s *Stream) Write(p []byte) (int, error) {
	s.writes.Lock()
	defer s.writes.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), 64<<10)
		if err := s.conn.WriteMessage(websocket.BinaryMessage, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
func (s *Stream) Close() error {
	var err error
	s.once.Do(func() {
		if s.done != nil {
			close(s.done)
		}
		err = s.conn.Close()
	})
	return err
}
func (s *Stream) LocalAddr() net.Addr  { return s.conn.LocalAddr() }
func (s *Stream) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }
func (s *Stream) SetDeadline(t time.Time) error {
	_ = s.SetReadDeadline(t)
	return s.SetWriteDeadline(t)
}
func (s *Stream) SetReadDeadline(t time.Time) error  { return s.conn.SetReadDeadline(t) }
func (s *Stream) SetWriteDeadline(t time.Time) error { return s.conn.SetWriteDeadline(t) }

func Splice(ctx context.Context, a, b net.Conn) {
	stop := context.AfterFunc(ctx, func() { _ = a.Close(); _ = b.Close() })
	defer stop()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(a, b); _ = a.Close(); _ = b.Close(); close(done) }()
	_, _ = io.Copy(b, a)
	_ = a.Close()
	_ = b.Close()
	<-done
}
