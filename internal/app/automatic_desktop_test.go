package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/muq9738/Vantry/internal/t3compat"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

type routingHostFixture struct {
	closed atomic.Bool
	id     string
	err    error
}

func (h *routingHostFixture) Ticket() string    { return h.id }
func (h *routingHostFixture) SessionID() string { return h.id }
func (h *routingHostFixture) Connected() bool   { return false }
func (h *routingHostFixture) Err() error        { return h.err }
func (h *routingHostFixture) Close() error      { h.closed.Store(true); return nil }

func TestAutomaticDesktopDefersRelayAndClosesBothHosts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	local, remote := &routingHostFixture{id: "local"}, &routingHostFixture{id: "relay"}
	var attempts int
	host := &automaticDesktopHost{direct: local, ctx: ctx, cancel: cancel, create: func(context.Context) (rtcHost, error) { attempts++; return remote, nil }}
	if host.Ticket() != "local" || host.SessionID() != "local" || host.currentRelay() != nil || attempts != 0 {
		t.Fatal("LAN startup contacted fallback")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := host.relay(canceled); err == nil || attempts != 0 {
		t.Fatal("canceled connection started fallback")
	}
	for i := 0; i < 2; i++ {
		if got, err := host.relay(ctx); err != nil || got != remote {
			t.Fatal("relay setup failed")
		}
	}
	if attempts != 1 {
		t.Fatal("relay host was duplicated")
	}
	host.Close()
	if !local.closed.Load() || !remote.closed.Load() {
		t.Fatal("a desktop transport survived sharing shutdown")
	}
}

func TestViewerByeKeepsExplicitlyEnabledDesktopSharingAvailable(t *testing.T) {
	local := &routingHostFixture{id: "local", err: rtc.ErrClosed}
	bridge := &RemoteDesktopBridge{host: local}
	status, err := bridge.HostStatus(context.Background())
	if err != nil || !status.Hosting || status.Clients != 0 || local.closed.Load() {
		t.Fatal("a normally closed viewer disabled the host", status, err)
	}
	automatic := &automaticDesktopHost{direct: local}
	if automatic.Err() != nil {
		t.Fatal("a closed LAN viewer disabled relay fallback")
	}
	local.err = errors.New("capture source failed")
	if _, err := bridge.HostStatus(context.Background()); err == nil || automatic.Err() == nil {
		t.Fatal("a real host failure was hidden")
	}
}

func TestMobileDesktopTargetReportsNotSharingForRelaySelector(t *testing.T) {
	bridge := &RemoteDesktopBridge{}
	if _, err := bridge.mobileDesktopTarget(context.Background(), "relay"); !errors.Is(err, t3compat.ErrDesktopNotSharing) {
		t.Fatalf("expected ErrDesktopNotSharing when sharing is off, got %v", err)
	}
}
