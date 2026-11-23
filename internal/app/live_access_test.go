package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

// Opt-in acceptance against a paired, real host. Setup is external; this test
// never installs a service or uses SSH. It creates one empty test conversation.
func TestLiveRelayRemoteAccess(t *testing.T) {
	dir := os.Getenv("VANTRY_LIVE_ACCESS_DATA_DIR")
	if dir == "" {
		t.Skip("set VANTRY_LIVE_ACCESS_DATA_DIR, VANTRY_LIVE_ACCESS_PEER and VANTRY_LIVE_ACCESS_WORKSPACE")
	}
	peer, workspace := os.Getenv("VANTRY_LIVE_ACCESS_PEER"), os.Getenv("VANTRY_LIVE_ACCESS_WORKSPACE")
	module, _ := filepath.Abs("../../resources/remote-access/node_modules/alma-relay/shared.mjs")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mode := os.Getenv("VANTRY_LIVE_ACCESS_MODE")
	client, err := remoteaccess.ConnectWithMode(ctx, remoteaccess.Options{DataDir: dir, ProtocolModule: module}, peer, mode)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if mode != "" && mode != "auto" && client.Mode() != mode {
		t.Fatalf("expected %s, actually used %s", mode, client.Mode())
	}
	t.Logf("actual transport: %s", client.Mode())
	if _, err := VerifyCoreCapabilities(ctx, client, RequiredDesktopCapabilities()); err != nil {
		t.Fatal(err)
	}
	read, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "files.read", Workspace: workspace, Paths: []string{"sample.txt"}})
	if err != nil || read.Output == "" {
		t.Fatalf("file read through relay: %v", err)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := client.Subscribe(ctx, snapshot.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	created, err := client.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, CommandID: "live-access-" + time.Now().Format("150405.000000000"), Kind: api.CreateThread, Title: "Mac mini 远程访问验收", ProviderID: "controlled", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: workspace}})
	if err != nil || created.ThreadID == "" {
		t.Fatalf("create remote conversation: %v", err)
	}
	select {
	case <-sub.Events:
	case <-ctx.Done():
		t.Fatal("remote event did not arrive")
	}
	if _, err := client.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, CommandID: "delete-" + created.ThreadID, Kind: api.DeleteThread, ThreadID: created.ThreadID}); err != nil {
		t.Fatalf("remove empty test conversation: %v", err)
	}
	t.Log("Mac mini capability, file read, conversation creation and event stream passed without SSH transport")
}
