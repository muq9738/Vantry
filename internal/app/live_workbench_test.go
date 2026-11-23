package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/sshrelay"
	"github.com/muq9738/Vantry/internal/desktop"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

// This opt-in test uses the same SSH transport as a remote workbench window.
// The remote fixture is a directory containing go.mod and sample.txt; no model
// request, user-file mutation, or remote installation happens inside the test.
func TestLiveSSHWorkbench(t *testing.T) {
	host := os.Getenv("VANTRY_LIVE_SSH_HOST")
	if host == "" {
		t.Skip("set VANTRY_LIVE_SSH_HOST, VANTRY_LIVE_SOCKET and VANTRY_LIVE_WORKSPACE")
	}
	socket, workspace := os.Getenv("VANTRY_LIVE_SOCKET"), os.Getenv("VANTRY_LIVE_WORKSPACE")
	if socket == "" || workspace == "" {
		t.Fatal("live SSH fixture paths are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := sshrelay.Connect(ctx, host, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := VerifyCoreCapabilities(ctx, client, RequiredDesktopCapabilities()); err != nil {
		t.Fatal(err)
	}
	for _, request := range []api.WorkbenchRequest{
		{Action: "files.list", Workspace: workspace},
		{Action: "files.read", Workspace: workspace, Paths: []string{"sample.txt"}},
		{Action: "plugins.list"},
	} {
		response, err := client.Workbench(ctx, request)
		if err != nil {
			t.Fatalf("%s over SSH: %v", request.Action, err)
		}
		switch request.Action {
		case "files.list":
			if !strings.Contains(response.Output, "sample.txt") {
				t.Fatal("remote fixture missing from file listing")
			}
		case "files.read":
			if !strings.Contains(response.Output, "Vantry") {
				t.Fatal("remote fixture content was not read")
			}
		case "plugins.list":
			if len(response.Plugins) != 0 {
				t.Fatal("bundled plugins missing")
			}
		}
		t.Logf("%s passed over real SSH", request.Action)
	}
}

func TestLiveRemoteDesktopFrames(t *testing.T) {
	statusPath := os.Getenv("VANTRY_LIVE_RD_STATUS_FILE")
	if statusPath == "" {
		t.Skip("set VANTRY_LIVE_RD_STATUS_FILE to a private host status JSON file")
	}
	raw, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	var status desktop.RemoteDesktopHostStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Hosting || status.ConnectURL == "" {
		t.Fatal("remote host is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	helper, err := remoteDesktopHelperPath()
	if err != nil {
		t.Fatal(err)
	}
	session, err := rtc.Dial(ctx, rtc.ConnectConfig{Ticket: status.ConnectURL, HelperPath: helper})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		frame, seq, stats := session.LatestFrame()
		if frame != nil && seq >= 2 {
			if frame.Bounds().Dx() < 1 || frame.Bounds().Dy() < 1 {
				t.Fatal("empty remote frame")
			}
			t.Logf("received %d frames from actual remote screen: %dx%d via %s", seq, frame.Bounds().Dx(), frame.Bounds().Dy(), stats.Mode)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for remote screen frames")
		case <-session.Done():
			t.Fatalf("remote screen stream ended: %v", session.Err())
		case <-ticker.C:
		}
	}
}
