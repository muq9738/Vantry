package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/muq9738/Vantry/internal/desktop"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

func TestCancelledEnableCleanupCannotStopALaterEnable(t *testing.T) {
	bridge := testRTCBridge(t)
	host := &fakeRTCHost{}
	starts := 0
	bridge.startHost = func(context.Context, rtc.HostConfig) (rtcHost, error) { starts++; return host, nil }
	service := &workbenchService{rd: bridge}
	call := func(action, owner string) desktop.RemoteDesktopHostStatus {
		t.Helper()
		response, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: action, Name: "qmt", Message: owner})
		if err != nil {
			t.Fatal(err)
		}
		var status desktop.RemoteDesktopHostStatus
		if json.Unmarshal([]byte(response.Output), &status) != nil {
			t.Fatal("host response invalid")
		}
		return status
	}
	if status := call("rd.host_start", "enable-A"); !status.Hosting || status.StartRequestID != "enable-A" {
		t.Fatal("first operation did not own its pending host")
	}
	if status := call("rd.host_start", "enable-B"); !status.Hosting || status.StartRequestID != "enable-B" || starts != 1 {
		t.Fatal("a new explicit enable did not adopt the current host")
	}
	if status := call("rd.host_stop", "enable-A"); !status.Hosting || status.StartRequestID != "enable-B" || host.closed.Load() != 0 {
		t.Fatal("late cancelled cleanup stopped a newer enable")
	}
	if status := call("rd.host_stop", "enable-B"); status.Hosting || host.closed.Load() != 1 {
		t.Fatal("the matching operation could not stop its host")
	}
}

func TestExplicitOffStillStopsAnyOwnedHost(t *testing.T) {
	bridge := testRTCBridge(t)
	host := &fakeRTCHost{}
	bridge.startHost = func(context.Context, rtc.HostConfig) (rtcHost, error) { return host, nil }
	if _, err := bridge.HostStartOwned(context.Background(), "qmt", "enable-owner"); err != nil {
		t.Fatal(err)
	}
	status, err := bridge.HostStop(context.Background())
	if err != nil || status.Hosting || host.closed.Load() != 1 {
		t.Fatal("explicit user off was incorrectly limited by cleanup ownership")
	}
}
