package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

// This exercises RunCore's production wiring and the same saved authenticated
// pairing used by the IDE, with production dispatch and transport wiring.
func TestPairedEnvironmentDelegationProductionE2E(t *testing.T) {
	t.Chdir("../..")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("/tmp", "vt-fed-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	remoteConfig := Config{DataDir: filepath.Join(root, "remote"), ProviderID: "controlled", Model: "remote-default",
		Delay: 35 * time.Millisecond, Workspace: &api.WorkspaceSpec{Kind: "directory", Path: t.TempDir()}}
	localConfig := Config{DataDir: filepath.Join(root, "local"), ProviderID: "controlled", Model: "local-default",
		Delay: 5 * time.Millisecond, Workspace: &api.WorkspaceSpec{Kind: "directory", Path: t.TempDir()}}
	start := func(config Config) (*local.Client, func()) {
		t.Helper()
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- RunCore(runCtx, config) }()
		client := local.Dial(config.SocketPath())
		for i := 0; i < 150; i++ {
			if _, err := client.Health(ctx); err == nil {
				return client, func() {
					client.Close()
					stop()
					if err := <-done; err != nil {
						t.Error(err)
					}
				}
			}
			select {
			case err := <-done:
				stop()
				t.Fatalf("start core: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
		}
		stop()
		<-done
		t.Fatal("core startup timeout")
		return nil, nil
	}
	remote, stopRemote := start(remoteConfig)
	defer stopRemote()
	host, err := remoteaccess.New(ctx, remoteaccess.Options{DataDir: remoteConfig.DataDir, SocketPath: remoteConfig.SocketPath(), DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	_, err = host.Start(ctx, "Remote test host", "", "")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := remoteaccess.New(ctx, remoteaccess.Options{DataDir: localConfig.DataDir, DisableDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	exposure, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paired, err := controller.Pair(exposure.PairingLink)
	if err != nil {
		t.Fatal(err)
	}
	environmentID := paired.Peers[0].ID
	client, stopLocal := start(localConfig)
	defer func() {
		if stopLocal != nil {
			stopLocal()
		}
	}()
	status, err := client.ServerStatus(ctx)
	if err != nil || !status.HasCapability(api.CapabilityEnvironmentOrchestration) {
		t.Fatalf("unwired production Core: %+v %v", status, err)
	}
	parent, err := client.LaunchThread(ctx, api.ThreadLaunchRequest{ClientRequestID: "parent", ThreadLaunchSpec: api.ThreadLaunchSpec{Title: "Parent", Message: "prepare parent"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.WaitThread(ctx, api.ThreadWaitQuery{ThreadID: parent.ThreadID, RunID: parent.RunID, TimeoutMs: 5000}); err != nil {
		t.Fatal(err)
	}
	req := api.DelegateTaskRequest{ClientRequestID: "remote-child", CallerThreadID: parent.ThreadID, ParentRunID: parent.RunID,
		EnvironmentID: environmentID, Task: "produce remote evidence", Title: "Remote child"}
	if _, err := client.DelegateTask(ctx, req); api.CodeOf(err) != api.Conflict {
		t.Fatalf("different peer default must require an explicit model: %v", err)
	}
	req.Model = "remote-default"
	child, err := client.DelegateTask(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := client.DelegateTask(ctx, req)
	if err != nil || retry.TaskID != child.TaskID {
		t.Fatalf("retry duplicated task: %+v %v", retry, err)
	}
	// Detach the parent Core during real remote output, then reattach the same
	// durable Attempt. Remote work continues independently of the connection.
	for i := 0; i < 300; i++ {
		active, readErr := remote.Snapshot(ctx)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(active.Runs) == 1 && active.Runs[0].Status == api.RunRunning && active.Runs[0].Output != "" {
			break
		}
		if i == 299 {
			t.Fatal("remote output never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopLocal()
	stopLocal = nil
	client, stopLocal = start(localConfig)
	parentWait, err := client.WaitThread(ctx, api.ThreadWaitQuery{ThreadID: parent.ThreadID, RunID: parent.RunID, TimeoutMs: 10000, UntilDelegatedWorkSettled: true})
	if err != nil || parentWait.TimedOut || parentWait.Status != "completed" || strings.Count(parentWait.Summary, "produce remote evidence") != 1 {
		t.Fatalf("parent continuation: %+v %v", parentWait, err)
	}
	localSnap, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remoteSnap, err := remote.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remoteSnap.Threads) != 1 || len(remoteSnap.Runs) != 1 {
		t.Fatalf("remote execution duplicated: threads=%d runs=%d", len(remoteSnap.Threads), len(remoteSnap.Runs))
	}
	remoteRun := remoteSnap.Runs[0]
	if remoteRun.Model != "remote-default" || remoteRun.Workspace.Path != remoteConfig.Workspace.Path || remoteRun.Workspace.EnvironmentID != "" {
		t.Fatalf("local defaults leaked to peer: %+v", remoteRun)
	}
	if len(localSnap.Runs) != 2 {
		t.Fatalf("unexpected local runs: %+v", localSnap.Runs)
	}
	for _, run := range localSnap.Runs {
		if run.ID == child.ChildRunID && (run.Workspace.EnvironmentID != environmentID || run.Model != "remote-default") {
			t.Fatalf("proxy config: %+v", run)
		}
	}
	parentDetail, err := client.ReadThread(ctx, api.ThreadReadQuery{ThreadID: parent.ThreadID, View: "activity", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(parentDetail.RecentRuns) != 1 || parentDetail.RecentRuns[0].RunID != parent.RunID {
		t.Fatalf("parent identity changed: %+v", parentDetail.RecentRuns)
	}

	// A follow-up retains the remote session and becomes a new generation
	// of the original local task.
	follow, err := client.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, CommandID: "follow", Kind: api.FollowUpTask,
		TaskID: child.TaskID, Message: "check the follow-up edge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.WaitThread(ctx, api.ThreadWaitQuery{ThreadID: parent.ThreadID, RunID: parent.RunID, TimeoutMs: 10000, UntilDelegatedWorkSettled: true}); err != nil {
		t.Fatal(err)
	}
	remoteSnap, err = remote.Snapshot(ctx)
	if err != nil || len(remoteSnap.Threads) != 1 || len(remoteSnap.Runs) != 2 {
		t.Fatalf("followup lost session: %+v %v", remoteSnap, err)
	}
	details, err := client.TaskStatus(ctx, api.TaskStatusQuery{TaskID: child.TaskID})
	if err != nil || len(details.Tasks) != 1 || details.Tasks[0].RemoteThreadID == "" || details.Tasks[0].EnvironmentID != environmentID {
		t.Fatalf("remote task mapping: %+v %v", details, err)
	}
	if follow.Generation != 2 {
		t.Fatalf("followup generation: %+v", follow)
	}

	// Explicit scope prevents a same-named thread from being read or messaged
	// on the local Core. Unknown targets fail before creating a local run.
	_, err = client.ReadThread(ctx, api.ThreadReadQuery{EnvironmentID: environmentID, ThreadID: remoteRun.ThreadID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.ReadThread(ctx, api.ThreadReadQuery{ThreadID: remoteRun.ThreadID}); api.CodeOf(err) != api.NotFound {
		t.Fatalf("scope collapsed: %v", err)
	}
	if _, err = client.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, CommandID: "same-name", Kind: api.CreateThread, ThreadID: remoteRun.ThreadID, Title: "local namesake"}); err != nil {
		t.Fatal(err)
	}
	sendReq := api.InterThreadSendRequest{EnvironmentID: environmentID, CallerThreadID: parent.ThreadID, TargetThreadID: remoteRun.ThreadID, ClientRequestID: "scoped-message", Message: "message only to the remote namesake"}
	sentRemote, err := client.SendToThread(ctx, sendReq)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sentRemote.Link, "?environmentId="+environmentID) {
		t.Fatalf("remote link lost scope and could select local namesake: %q", sentRemote.Link)
	}
	retired, err := client.SendToThread(ctx, sendReq)
	if err != nil || retired.RunID != sentRemote.RunID {
		t.Fatalf("remote send retry changed identity: %+v %v", retired, err)
	}
	waitRemote, err := client.WaitThread(ctx, api.ThreadWaitQuery{EnvironmentID: environmentID, ThreadID: remoteRun.ThreadID, RunID: sentRemote.RunID, TimeoutMs: 5000})
	if err != nil || waitRemote.Status != "completed" {
		t.Fatalf("scoped remote wait: %+v %v", waitRemote, err)
	}
	namesake, err := client.ReadThread(ctx, api.ThreadReadQuery{ThreadID: remoteRun.ThreadID})
	if err != nil || len(namesake.RecentRuns) != 0 {
		t.Fatalf("remote message executed locally: %+v %v", namesake, err)
	}
	restricted, err := client.LaunchThread(ctx, api.ThreadLaunchRequest{ClientRequestID: "restricted", ThreadLaunchSpec: api.ThreadLaunchSpec{ApprovalPolicy: "ask", InteractionMode: "plan"}})
	if err != nil {
		t.Fatal(err)
	}
	sendReq.CallerThreadID, sendReq.ClientRequestID = restricted.ThreadID, "denied"
	if _, err = client.SendToThread(ctx, sendReq); api.CodeOf(err) != api.Conflict {
		t.Fatalf("remote send exceeded caller permissions: %v", err)
	}
	bad := req
	bad.ClientRequestID = "unknown"
	bad.EnvironmentID = "unlinked-host"
	if _, err = client.DelegateTask(ctx, bad); api.CodeOf(err) != api.NotFound {
		t.Fatalf("unknown target fell back: %v", err)
	}
	body, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "orchestration.environments"})
	if err != nil || !strings.Contains(body.Output, environmentID) {
		t.Fatalf("target discovery: %+v %v", body, err)
	}

	// Restart the local production Core: a settled task stays settled and its
	// original remote run is not dispatched a second time.
	stopLocal()
	stopLocal = nil
	client, stopLocal = start(localConfig)
	after, err := client.TaskStatus(ctx, api.TaskStatusQuery{TaskID: child.TaskID})
	if err != nil || after.Tasks[0].RemoteThreadID != remoteRun.ThreadID {
		t.Fatalf("restart mapping: %+v %v", after, err)
	}
	raw, _ := json.Marshal(map[string]any{"localTask": child.TaskID, "environment": environmentID, "remoteThread": remoteRun.ThreadID, "remoteRuns": len(remoteSnap.Runs), "parentRun": parent.RunID})
	t.Log(string(raw))
}
