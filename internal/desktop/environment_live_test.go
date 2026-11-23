package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

// This opt-in test sends one small real Agent prompt to the selected host.
// It exercises the same UI create/send handlers and actual peer transport.
func TestLiveEnvironmentAgentSession(t *testing.T) {
	data := os.Getenv("VANTRY_LIVE_ENV_DATA")
	peerID := os.Getenv("VANTRY_LIVE_ENV_PEER")
	if data == "" || peerID == "" {
		t.Skip("set VANTRY_LIVE_ENV_DATA and VANTRY_LIVE_ENV_PEER for real Agent acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	localClient := local.Dial(filepath.Join(data, "core.sock"))
	defer localClient.Close()
	before, err := localClient.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := newWorkbench(localClient)
	root.defaults = Options{ProviderID: "pi", StatePath: filepath.Join(t.TempDir(), "desktop-state.json")}
	root.applySnapshot(before)
	root.newSession()
	module, _ := filepath.Abs("../../resources/remote-access/node_modules/alma-relay/shared.mjs")
	root.defaults.ConnectEnvironment = func(ctx context.Context, id string) (*EnvironmentSession, error) {
		client, err := remoteaccess.Connect(ctx, remoteaccess.Options{DataDir: data, ProtocolModule: module}, id)
		if err != nil {
			return nil, err
		}
		response, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "environment.info"})
		if err != nil {
			_ = client.Close()
			return nil, err
		}
		var info api.EnvironmentInfo
		if err := json.Unmarshal([]byte(response.Output), &info); err != nil {
			_ = client.Close()
			return nil, err
		}
		return &EnvironmentSession{Service: client, Info: info, Close: client.Close, Mode: client.Mode}, nil
	}
	h := newEnvironmentHub(root, ctx)
	defer h.close()
	h.records = append(h.records, environmentRecord{key: "mini", connectionID: peerID, label: "Mac mini"})
	h.bind = func(target *workbench) {
		snapshot, err := target.service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		target.applySnapshot(snapshot)
		target.ensureModels(true)
	}
	h.choose(root, 1, true)
	target := h.current
	if target == root {
		t.Fatalf("environment connection failed: %s", h.failure)
	}
	if target.environmentInfo.Name == machineName() {
		t.Fatal("test did not connect to another machine")
	}
	if target.modelsErr != "" || len(target.models) == 0 {
		t.Fatalf("remote model directory: %s", target.modelsErr)
	}
	remoteWorkspace := os.Getenv("VANTRY_LIVE_REMOTE_WORKSPACE")
	if remoteWorkspace == "" {
		remoteWorkspace = filepath.Join(os.TempDir(), "vantry-remote-connect-workspace")
	}
	target.configureWorkspaceSpec(&api.WorkspaceSpec{Kind: "directory", Path: remoteWorkspace})
	marker := fmt.Sprintf("VANTRY_MINI_AGENT_OK_%d", time.Now().Unix())
	prompt := "仅回复 " + marker + "。不要调用工具，不要修改文件。"
	target.setDraft(target.selected, prompt)
	tt := ui.NewTester(func(c *ui.Context) { h.current.view(c) }, 1280, 820)
	mustClick(t, tt, "发送")
	var completed api.Run
	for ctx.Err() == nil {
		snapshot, err := target.service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		target.applySnapshot(snapshot)
		for _, run := range snapshot.Runs {
			if run.Input == prompt && run.Status.Terminal() {
				completed = run
				break
			}
		}
		if completed.ID != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if completed.ID == "" {
		t.Fatal("remote Agent did not finish within deadline")
	}
	if completed.Status != api.RunCompleted || !strings.Contains(completed.Output, marker) {
		t.Fatalf("remote Agent status=%s error=%s output=%q", completed.Status, completed.Error, completed.Output)
	}
	localAfter, err := localClient.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(localAfter.Runs) != len(before.Runs) || len(localAfter.Threads) != len(before.Threads) {
		t.Fatal("local Core executed the remote session")
	}
	h.choose(target, 0, false)
	if h.current != root {
		t.Fatal("could not return to local environment")
	}
	h.choose(root, 1, false)
	if h.current != target || target.selected == "" {
		t.Fatal("switching back lost remote conversation state")
	}
	tt.Frame()
	t.Logf("remoteEnvironment=%s host=%s model=%s remoteRun=%s completed; local Core counts unchanged", target.environmentKey, target.environmentInfo.Name, completed.Model, completed.ID)
	if path := os.Getenv("VANTRY_LIVE_ENV_RESULT"); path != "" {
		result, _ := json.MarshalIndent(map[string]any{"status": "passed", "environmentId": target.environmentKey, "host": target.environmentInfo.Name, "model": completed.Model, "runId": completed.ID, "threadId": completed.ThreadID, "output": completed.Output, "localRunCountUnchanged": true, "remoteModelCount": len(target.models), "uiMode": "same-view handlers with real services"}, "", "  ")
		if err := os.WriteFile(path, result, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
