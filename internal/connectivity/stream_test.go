package connectivity

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStreamKeepAliveSurvivesDeadlinesAndClosesDeadPeer(t *testing.T) {
	origInterval, origTimeout, origWrite := streamPingInterval, streamPongTimeout, streamPingWriteTimeout
	streamPingInterval = 25 * time.Millisecond
	streamPongTimeout = 90 * time.Millisecond
	streamPingWriteTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		streamPingInterval = origInterval
		streamPongTimeout = origTimeout
		streamPingWriteTimeout = origWrite
	})

	t.Run("idle stream stays open despite SetReadDeadline reset", func(t *testing.T) {
		serverReadErr := make(chan error, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrade(w, r)
			if err != nil {
				return
			}
			defer conn.Close()
			s := NewStream(conn)
			defer s.Close()
			buf := make([]byte, 64)
			n, err := s.Read(buf)
			if err == nil && string(buf[:n]) == "ping-ok" {
				_, _ = s.Write([]byte("pong-ok"))
			}
			serverReadErr <- err
		}))
		defer srv.Close()

		conn, resp, err := websocket.DefaultDialer.Dial(strings.Replace(srv.URL, "http", "ws", 1), nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		client := NewStream(conn)
		defer client.Close()

		if err := client.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		readDone := make(chan string, 1)
		readErr := make(chan error, 1)
		go func() {
			buf := make([]byte, 64)
			n, err := client.Read(buf)
			if err != nil {
				readErr <- err
				return
			}
			readDone <- string(buf[:n])
		}()

		time.Sleep(160 * time.Millisecond)
		if _, err := client.Write([]byte("ping-ok")); err != nil {
			t.Fatalf("idle stream closed prematurely: %v", err)
		}
		select {
		case msg := <-readDone:
			if msg != "pong-ok" {
				t.Fatalf("unexpected reply %q", msg)
			}
		case err := <-readErr:
			t.Fatalf("client read failed: %v", err)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for reply on kept-alive stream")
		}
		if err := <-serverReadErr; err != nil && err != io.EOF {
			t.Fatalf("server stream error: %v", err)
		}
	})

	t.Run("silent peer without Pong closes automatically", func(t *testing.T) {
		releaseServer := make(chan struct{})
		defer close(releaseServer)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrade(w, r)
			if err != nil {
				return
			}
			defer conn.Close()
			<-releaseServer
		}))
		defer srv.Close()

		conn, resp, err := websocket.DefaultDialer.Dial(strings.Replace(srv.URL, "http", "ws", 1), nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		client := NewStream(conn)
		defer client.Close()

		done := make(chan error, 1)
		go func() {
			buf := make([]byte, 64)
			_, err := client.Read(buf)
			done <- err
		}()

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected blackholed stream to terminate with error")
			}
		case <-time.After(time.Second):
			t.Fatal("keepalive did not close silent peer stream")
		}
	})
}
