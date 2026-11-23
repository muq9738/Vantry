package desktoprelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/connectivity"
	remote "github.com/muq9738/vantry-plugin-remote-desktop"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

// Use the production RTC signaling path and its trickled ICE messages. Capture,
// decoding and OS input are deliberately excluded from this transport test.
func TestNativeRTCConnectAndReconnectThroughDesktopRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	key := connectivity.Token()
	server, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	pairings, err := connectivity.NewRelay(connectivity.RelayConfig{RegistrationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer pairings.Close()
	mux := http.NewServeMux()
	mux.Handle("/desktop/", server)
	mux.Handle("/desktop/pairings", http.StripPrefix("/desktop", pairings))
	mux.Handle("/desktop/pairings/", http.StripPrefix("/desktop", pairings))
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()
	hostRelay, err := NewClient(httpServer.URL, key, false)
	if err != nil {
		t.Fatal(err)
	}
	defer hostRelay.Close()
	host, err := rtc.StartHost(ctx, rtc.HostConfig{
		Relay: hostRelay, SSHHost: "test-relay", Quality: "balanced",
		InputSink: transportOnlyInput{},
		SourceFactory: func(context.Context, rtc.VideoConfig) (rtc.EncodedSource, error) {
			return transportOnlySource{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ticket, err := Redeem(ctx, "", hostRelay.Ticket())
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		viewerRelay, err := NewClient(ticket.Relay, "", true)
		if err != nil {
			t.Fatal(err)
		}
		approval := make(chan error, 1)
		go func() {
			for ctx.Err() == nil {
				pending, err := hostRelay.Pending(ctx)
				if err != nil {
					approval <- err
					return
				}
				if pending != "" {
					approval <- hostRelay.Approve(ctx, true)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			approval <- ctx.Err()
		}()
		viewer, err := rtc.Dial(ctx, rtc.ConnectConfig{
			Relay:  viewerRelay,
			Ticket: (rtc.Ticket{SSHHost: "test-relay", SessionID: ticket.SessionID, Token: ticket.Token}).String(),
			DecoderFactory: func(context.Context, func(rtc.DecodedFrame)) (rtc.FrameDecoder, error) {
				return transportOnlyDecoder{}, nil
			},
		})
		if err != nil {
			viewerRelay.Close()
			t.Fatalf("attempt %d: %v (host: %v)", attempt+1, err, host.Err())
		}
		if err := <-approval; err != nil {
			_ = viewer.Close()
			viewerRelay.Close()
			t.Fatal(err)
		}
		_, _, stats := viewer.LatestFrame()
		if !stats.Connected || stats.Transport != "wss-tcp" {
			t.Errorf("attempt %d did not establish native RTC over WSS", attempt+1)
		}
		_ = viewer.Close()
		viewerRelay.Close()
		// Wait for the relay to release this viewer before requesting approval
		// for another connection using the same redeemed credential.
		for ctx.Err() == nil {
			server.mu.Lock()
			room := server.rooms[ticket.SessionID]
			closed := room != nil && room.peers["viewer"] == nil && !room.approved
			server.mu.Unlock()
			if closed {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

type transportOnlySource struct{}

func (transportOnlySource) Next(ctx context.Context) (rtc.EncodedFrame, error) {
	<-ctx.Done()
	return rtc.EncodedFrame{}, ctx.Err()
}
func (transportOnlySource) RequestKeyFrame() error { return nil }
func (transportOnlySource) SetBitrate(int) error   { return nil }
func (transportOnlySource) Close() error           { return nil }

type transportOnlyDecoder struct{}

func (transportOnlyDecoder) Decode(context.Context, rtc.EncodedFrame) error { return nil }
func (transportOnlyDecoder) Close() error                                   { return nil }

type transportOnlyInput struct{}

func (transportOnlyInput) Mouse(context.Context, remote.MouseEvent) error { return nil }
func (transportOnlyInput) Wheel(context.Context, remote.WheelEvent) error { return nil }
func (transportOnlyInput) Key(context.Context, remote.KeyEvent) error     { return nil }
