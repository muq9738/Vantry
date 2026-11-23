package app

import (
	"context"
	"errors"
	"image"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/desktop"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

type fakeRTCViewer struct {
	closed atomic.Int32
	once   sync.Once
	done   chan struct{}
	stats  rtc.Stats
}

func newFakeRTCViewer() *fakeRTCViewer {
	return &fakeRTCViewer{done: make(chan struct{}), stats: rtc.Stats{Connected: true, Mode: "relay-webrtc", Transport: "ssh-tcp", Codec: "h264", Hardware: true, FPS: 30, FirstFrameMs: 120, FrameAgeP95Ms: 45, ClockSynchronized: true}}
}
func (*fakeRTCViewer) OnFrame(func()) {}
func (v *fakeRTCViewer) LatestFrame() (*image.RGBA, uint64, rtc.Stats) {
	return image.NewRGBA(image.Rect(0, 0, 2, 2)), 1, v.stats
}
func (*fakeRTCViewer) SendMouse(string, float64, float64, int) error { return nil }
func (*fakeRTCViewer) SendWheel(float64, float64) error              { return nil }
func (*fakeRTCViewer) SendKey(string, int, string, uint32) error     { return nil }
func (*fakeRTCViewer) SetQuality(string) error                       { return nil }
func (v *fakeRTCViewer) Done() <-chan struct{}                       { return v.done }
func (*fakeRTCViewer) Err() error                                    { return nil }
func (v *fakeRTCViewer) Close() error {
	v.once.Do(func() { v.closed.Add(1); close(v.done) })
	return nil
}

type fakeRTCHost struct{ closed atomic.Int32 }

func (*fakeRTCHost) Ticket() string    { return "private-test-ticket" }
func (*fakeRTCHost) SessionID() string { return "test-session" }
func (*fakeRTCHost) Connected() bool   { return true }
func (*fakeRTCHost) Err() error        { return nil }
func (h *fakeRTCHost) Close() error    { h.closed.Add(1); return nil }

func testRTCBridge(t *testing.T) *RemoteDesktopBridge {
	t.Helper()
	b := NewRemoteDesktopDriver(context.Background())
	b.helper = func() (string, error) { return "test-native-helper", nil }
	t.Cleanup(func() { _ = b.Close() })
	return b
}
func testRTCTicket() string {
	return (rtc.Ticket{SSHHost: "qmt", SessionID: "test-session-12345", Token: strings.Repeat("t", 40)}).String()
}

func TestRTCViewerMapsTransportAndClosesWithView(t *testing.T) {
	b := testRTCBridge(t)
	viewer := newFakeRTCViewer()
	calls := 0
	b.dial = func(_ context.Context, cfg rtc.ConnectConfig) (rtcViewer, error) {
		calls++
		if cfg.HelperPath != "test-native-helper" || cfg.Ticket != testRTCTicket() {
			t.Fatal("RTC dial lost helper or ticket")
		}
		return viewer, nil
	}
	if err := b.Connect(context.Background(), desktop.RemoteDesktopConnect{Target: testRTCTicket(), Quality: "low", RelaySSHHost: "own-relay", RelayOnly: true}, nil); err != nil {
		t.Fatal(err)
	}
	_, _, stats := b.LatestFrame()
	if calls != 1 || stats.Mode != "relay-webrtc" || stats.Transport != "ssh-tcp" || !stats.Hardware || stats.FirstFrameMs != 120 || stats.FrameAgeP95Ms != 45 {
		t.Fatal("RTC measured stats were lost in the UI adapter")
	}
	_ = b.Close()
	_ = b.Close()
	if viewer.closed.Load() != 1 {
		t.Fatal("closing a view did not release exactly its viewer")
	}
}
func TestRTCViewerRejectsLegacyRouteWithoutFallback(t *testing.T) {
	b := testRTCBridge(t)
	calls := 0
	b.dial = func(context.Context, rtc.ConnectConfig) (rtcViewer, error) {
		calls++
		return nil, errors.New("unexpected dial")
	}
	for _, target := range []string{"test-machine", "vantry-rd://127.0.0.1:38789"} {
		if err := b.Connect(context.Background(), desktop.RemoteDesktopConnect{Target: target}, nil); err == nil {
			t.Fatal("legacy non-RTC route was accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid ticket fell back to another transport")
	}
}
func TestRTCViewerLateDialCannotSurviveDisconnect(t *testing.T) {
	b := testRTCBridge(t)
	viewer := newFakeRTCViewer()
	started, release := make(chan struct{}), make(chan struct{})
	b.dial = func(context.Context, rtc.ConnectConfig) (rtcViewer, error) {
		close(started)
		<-release
		return viewer, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- b.Connect(context.Background(), desktop.RemoteDesktopConnect{Target: testRTCTicket(), RelaySSHHost: "own-relay", RelayOnly: true}, nil)
	}()
	<-started
	_ = b.Disconnect()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("late dial did not report cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("late dial stuck")
	}
	if viewer.closed.Load() != 1 || b.viewer() != nil {
		t.Fatal("disconnected view retained a late RTC session")
	}
}
func TestRTCHostLifetimeBelongsToCoreNotRequest(t *testing.T) {
	b := testRTCBridge(t)
	host := &fakeRTCHost{}
	var life context.Context
	b.startHost = func(ctx context.Context, cfg rtc.HostConfig) (rtcHost, error) {
		life = ctx
		if cfg.SSHHost != "" || cfg.HelperPath != "test-native-helper" {
			t.Fatal("default host did not use LAN and the target Core helper")
		}
		return host, nil
	}
	request, cancel := context.WithCancel(context.Background())
	status, err := b.HostStart(request, "")
	if err != nil || !status.Hosting || status.Clients != 1 {
		t.Fatal("host did not start")
	}
	cancel()
	if life.Err() != nil {
		t.Fatal("finishing the request stopped the Core's host")
	}
	_, _ = b.HostStop(context.Background())
	if life.Err() == nil || host.closed.Load() != 1 {
		t.Fatal("Core host stop did not release capture")
	}
}
func TestRTCCancelledHostStartupCanRetry(t *testing.T) {
	b := testRTCBridge(t)
	started := make(chan struct{})
	b.startHost = func(ctx context.Context, _ rtc.HostConfig) (rtcHost, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	request, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := b.HostStart(request, "qmt"); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("startup cancellation missing")
		}
	case <-time.After(time.Second):
		t.Fatal("startup did not cancel")
	}
	b.startHost = func(context.Context, rtc.HostConfig) (rtcHost, error) { return &fakeRTCHost{}, nil }
	if _, err := b.HostStart(context.Background(), "qmt"); err != nil {
		t.Fatal("cancelled startup permanently blocked retries")
	}
}
