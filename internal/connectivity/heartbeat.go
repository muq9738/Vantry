package connectivity

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// KeepAlive detects a lost control/signaling connection without an extra process.
// The caller must keep reading WebSocket messages and close the socket before stop.
func KeepAlive(conn *websocket.Conn) func() {
	return keepAliveWithPong(conn, nil)
}

func keepAliveWithPong(conn *websocket.Conn, onPong func()) func() {
	_ = conn.SetReadDeadline(time.Now().Add(65 * time.Second))
	conn.SetPongHandler(func(string) error {
		if onPong != nil {
			onPong()
		}
		return conn.SetReadDeadline(time.Now().Add(65 * time.Second))
	})
	done := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(exited)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					_ = conn.Close()
					return
				}
			case <-done:
				return
			}
		}
	}()
	return func() { once.Do(func() { close(done) }); <-exited }
}
