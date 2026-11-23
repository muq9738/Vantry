package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/attachments"
	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/internal/core"
	"github.com/muq9738/Vantry/internal/telemetry"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

type workbenchProvider struct{ requests chan provider.Request }

func (p workbenchProvider) Run(_ context.Context, request provider.Request, emit func(string) error) error {
	p.requests <- request
	return emit("completed")
}

func TestWorkbenchServicesThroughPrivateSocket(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "vantry-workbench-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	files := attachments.Store{Directory: filepath.Join(directory, "attachments")}
	executor := workbenchProvider{requests: make(chan provider.Request, 2)}
	service, err := core.Open(ctx, core.Options{DBPath: filepath.Join(directory, "state.sqlite"), Provider: executor, ValidateAttachments: files.Validate})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	config := Config{DataDir: directory}
	server, err := local.Listen(config.SocketPath(), &workbenchService{Service: service, config: config})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		_ = server.Close(context.Background())
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := local.Dial(config.SocketPath())
	defer client.Close()
	call := func(request api.WorkbenchRequest) api.WorkbenchResponse {
		t.Helper()
		response, err := client.Workbench(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	t.Run("attachment-submit-and-configuration", func(t *testing.T) {
		uploaded := call(api.WorkbenchRequest{Action: "attachment.import", Name: "说明.txt", Data: []byte("中文附件内容")})
		if uploaded.Attachment == nil {
			t.Fatal("upload returned no attachment")
		}
		project := filepath.Join(directory, "project")
		if err := os.Mkdir(project, 0700); err != nil {
			t.Fatal(err)
		}
		created := dispatchTest(t, client, api.Command{Version: 1, Kind: api.CreateThread, CommandID: "create", Title: "附件会话", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: project}})
		response := call(api.WorkbenchRequest{Action: "workspace", ThreadID: created.ThreadID})
		expected, err := filepath.EvalSymlinks(project)
		if err != nil {
			t.Fatal(err)
		}
		if response.Workspace != expected {
			t.Fatalf("terminal workspace=%q, want %q", response.Workspace, expected)
		}
		command := api.Command{Version: 1, Kind: api.SendMessage, CommandID: "send-with-attachment", ThreadID: created.ThreadID, Message: "请检查附件", Attachments: []api.Attachment{*uploaded.Attachment}}
		receipt := dispatchTest(t, client, command)
		var request provider.Request
		select {
		case request = <-executor.requests:
		case <-ctx.Done():
			t.Fatal("provider not invoked")
		}
		if request.RunID != receipt.RunID || !reflect.DeepEqual(request.Attachments, command.Attachments) {
			t.Fatalf("attachment request: %+v", request)
		}
		snap := awaitSnapshot(t, client, func(s api.Snapshot) bool { return len(s.Runs) == 1 && s.Runs[0].Status.Terminal() })
		if !reflect.DeepEqual(snap.Runs[0].Attachments, command.Attachments) {
			t.Fatal("committed run lost attachments")
		}
		if retry := dispatchTest(t, client, command); retry != receipt {
			t.Fatal("retry changed receipt")
		}
		pinned := true
		dispatchTest(t, client, api.Command{Version: 1, Kind: api.UpdateThread, CommandID: "rename", ThreadID: created.ThreadID, Title: "已重命名", Pinned: &pinned})
		snap, err = client.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !snap.Threads[0].Pinned || snap.Threads[0].Title != "已重命名" {
			t.Fatalf("thread mutation not visible: %+v", snap.Threads)
		}
		forged := *uploaded.Attachment
		forged.Name = "different.txt"
		if _, err := client.Dispatch(ctx, api.Command{Version: 1, Kind: api.SendMessage, CommandID: "bad-file", ThreadID: created.ThreadID, Message: "bad", Attachments: []api.Attachment{forged}}); api.CodeOf(err) != api.InvalidCommand {
			t.Fatalf("forged attachment accepted: %v", err)
		}
	})

	t.Run("plugin-discovery-execution-and-errors", func(t *testing.T) {
		directory := filepath.Join(config.DataDir, "plugins", "fixture")
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "plugin.json"), []byte(`{"id":"fixture","name":"Fixture","runtime":"native","navigation":[{"title":"Fixture","page":"home"}],"pages":{"home":{"vantryPanel":{"version":1,"widgets":[{"kind":"text","text":"中文草稿"}]}}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "index.ts"), []byte(`export function run(input:{message:string}) { return input.message.trim() }`), 0600); err != nil {
			t.Fatal(err)
		}
		list := call(api.WorkbenchRequest{Action: "plugins.list"})
		if len(list.Plugins) != 1 || list.Plugins[0].Error != "" {
			t.Fatalf("plugin list: %+v", list.Plugins)
		}
		result := call(api.WorkbenchRequest{Action: "plugins.run", PluginID: "fixture", Input: json.RawMessage(`{"message":" 中文草稿 "}`)})
		if !strings.Contains(result.Output, "中文草稿") {
			t.Fatalf("plugin output: %q", result.Output)
		}
		if err := os.WriteFile(filepath.Join(directory, "plugin.json"), []byte(`{broken`), 0600); err != nil {
			t.Fatal(err)
		}
		list = call(api.WorkbenchRequest{Action: "plugins.list"})
		if len(list.Plugins) != 1 || list.Plugins[0].Error == "" {
			t.Fatal("broken plugin disappeared instead of showing error")
		}
		if _, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "plugins.run", PluginID: "fixture"}); err == nil || !strings.Contains(err.Error(), "plugin.json") {
			t.Fatalf("plugin run failure: %v", err)
		}
	})

	t.Run("git-status-and-local-merge", func(t *testing.T) {
		repo := filepath.Join(directory, "repository")
		if err := os.Mkdir(repo, 0700); err != nil {
			t.Fatal(err)
		}
		git := func(args ...string) {
			t.Helper()
			command := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("git: %v %s", err, output)
			}
		}
		git("init", "-b", "main")
		git("config", "user.name", "Vantry Test")
		git("config", "user.email", "test@example.invalid")
		git("config", "commit.gpgsign", "false")
		if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base"), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", "base.txt")
		git("commit", "-m", "base")
		git("switch", "-c", "feature")
		if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new"), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", "new.txt")
		git("commit", "-m", "feature")
		git("switch", "main")
		thread := dispatchTest(t, client, api.Command{Version: 1, Kind: api.CreateThread, CommandID: "git-thread", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: repo}})
		state := call(api.WorkbenchRequest{Action: "git.status", ThreadID: thread.ThreadID})
		if state.Git == nil || state.Git.Branch != "main" {
			t.Fatalf("git state: %+v", state.Git)
		}
		merged := call(api.WorkbenchRequest{Action: "git.merge", ThreadID: thread.ThreadID, Branch: "feature"})
		if merged.Git == nil || merged.Git.Status != "" || merged.Output == "" {
			t.Fatalf("merge response: %+v", merged)
		}
		if _, err := os.Stat(filepath.Join(repo, "new.txt")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWorkbenchUsageSummaryIncludesRealTelemetry(t *testing.T) {
	directory := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	executor := workbenchProvider{requests: make(chan provider.Request, 2)}
	service, err := core.Open(ctx, core.Options{
		DBPath:       filepath.Join(directory, "state.sqlite"),
		Provider:     executor,
		TelemetryDir: directory,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	created, err := service.Dispatch(ctx, api.Command{
		Version:   1,
		Kind:      api.CreateThread,
		CommandID: "c1",
		Title:     "Usage test",
		Model:     "magpie/deepseek-v4",
	})
	if err != nil {
		t.Fatal(err)
	}
	sent, err := service.Dispatch(ctx, api.Command{
		Version:   1,
		Kind:      api.SendMessage,
		CommandID: "s1",
		ThreadID:  created.ThreadID,
		Message:   "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	<-executor.requests
	for {
		snap, err := service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Runs) == 1 && snap.Runs[0].Status.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := service.UsageLedger().Record(telemetry.RunMetrics{
		RunID:            sent.RunID,
		ThreadID:         created.ThreadID,
		ProviderID:       "magpie",
		Model:            "deepseek-v4",
		TurnCount:        2,
		InputTokens:      1000,
		OutputTokens:     500,
		CacheReadTokens:  1500,
		CacheWriteTokens: 0,
		TotalTokens:      3000,
		CostUSD:          0.015,
		TTFTMs:           450,
	}); err != nil {
		t.Fatal(err)
	}

	wb := &workbenchService{
		Service: service,
		config:  Config{DataDir: directory},
		ledger:  service.UsageLedger(),
	}
	resp, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "usage.summary"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Runs            int     `json:"runs"`
		InputTokens     int64   `json:"inputTokens"`
		OutputTokens    int64   `json:"outputTokens"`
		CacheReadTokens int64   `json:"cacheReadTokens"`
		TotalTokens     int64   `json:"totalTokens"`
		CacheHitRate    float64 `json:"cacheHitRate"`
		TotalCostUSD    float64 `json:"totalCostUsd"`
		AvgTTFTMs       int64   `json:"avgTtftMs"`
		Models          []struct {
			Model       string  `json:"model"`
			Runs        int     `json:"runs"`
			TotalTokens int64   `json:"totalTokens"`
			CostUSD     float64 `json:"costUsd"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(resp.Output), &got); err != nil {
		t.Fatalf("unmarshal usage.summary: %v (%s)", err, resp.Output)
	}
	if got.Runs != 1 || got.TotalTokens != 3000 || got.CacheReadTokens != 1500 || got.AvgTTFTMs != 450 {
		t.Fatalf("unexpected usage summary: %+v", got)
	}
	if got.CacheHitRate < 0.59 || got.CacheHitRate > 0.61 || got.TotalCostUSD < 0.014 {
		t.Fatalf("unexpected cacheHitRate/cost: %+v", got)
	}
	if len(got.Models) != 1 || got.Models[0].Model != "magpie/deepseek-v4" || got.Models[0].TotalTokens != 3000 {
		t.Fatalf("unexpected model breakdown: %+v", got.Models)
	}
}

func TestWorkbenchGitCheckpointsLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "user.name", "Vantry Test")
	runGit("config", "commit.gpgsign", "false")
	file := filepath.Join(repo, "hello.txt")
	if err := os.WriteFile(file, []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "hello.txt")
	runGit("commit", "-m", "init")

	wb := &workbenchService{config: Config{DataDir: t.TempDir()}}

	// Non-git directory lists empty checkpoints without error.
	nonGit := t.TempDir()
	nonResp, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "git.checkpoints.list", Workspace: nonGit})
	if err != nil || nonResp.Output != `{"checkpoints":[]}` {
		t.Fatalf("non-git list = %+v, %v", nonResp, err)
	}

	// Capture checkpoint on v1.
	capResp, err := wb.Workbench(ctx, api.WorkbenchRequest{
		Action: "git.checkpoints.capture", Workspace: repo, Name: "run-r1", Message: "Before turn: v1",
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !strings.Contains(capResp.Output, `"run-r1"`) {
		t.Fatalf("unexpected capture output: %s", capResp.Output)
	}

	// Modify working tree and list + diff.
	if err := os.WriteFile(file, []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listResp, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "git.checkpoints.list", Workspace: repo})
	if err != nil || !strings.Contains(listResp.Output, `"run-r1"`) {
		t.Fatalf("list = %+v, %v", listResp, err)
	}
	diffResp, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "git.checkpoints.diff", Workspace: repo, Name: "run-r1"})
	if err != nil || !strings.Contains(diffResp.Output, "-v1") || !strings.Contains(diffResp.Output, "+v2") {
		t.Fatalf("diff = %q, %v", diffResp.Output, err)
	}

	// Primary checkout refuses without explicit opt-in.
	if _, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "git.checkpoints.restore", Workspace: repo, Name: "run-r1"}); err == nil {
		t.Fatal("expected primary checkout restore without allowPrimaryCheckout to be refused")
	}

	// Restore with Amend=true (allowPrimaryCheckout) succeeds and creates backup.
	restResp, err := wb.Workbench(ctx, api.WorkbenchRequest{
		Action: "git.checkpoints.restore", Workspace: repo, Name: "run-r1", Amend: true,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(restResp.Output, "pre-restore-") {
		t.Fatalf("expected backup checkpoint in restore output: %s", restResp.Output)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "v1\n" {
		t.Fatalf("restored file = %q (%v), want v1", string(got), err)
	}
}

func TestWorkbenchSandboxGetAndSet(t *testing.T) {
	ctx := context.Background()
	wb := &workbenchService{config: Config{DataDir: t.TempDir()}}

	getResp, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "sandbox.get"})
	if err != nil {
		t.Fatalf("sandbox.get: %v", err)
	}
	var status struct {
		Mode        string `json:"mode"`
		Supported   bool   `json:"supported"`
		EnvOverride string `json:"envOverride"`
	}
	if err := json.Unmarshal([]byte(getResp.Output), &status); err != nil {
		t.Fatal(err)
	}
	if status.Mode != "off" {
		t.Fatalf("default sandbox mode = %q, want off", status.Mode)
	}
	if !status.Supported {
		return
	}

	setResp, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "sandbox.set", Message: "workspace-write"})
	if err != nil {
		t.Fatalf("sandbox.set workspace-write: %v", err)
	}
	if err := json.Unmarshal([]byte(setResp.Output), &status); err != nil {
		t.Fatal(err)
	}
	if status.Mode != "workspace-write" {
		t.Fatalf("after set mode = %q, want workspace-write", status.Mode)
	}

	// Verify persistence on disk and reset back to off.
	getAgain, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "sandbox.get"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(getAgain.Output, `"workspace-write"`) {
		t.Fatalf("persisted sandbox.get output = %s", getAgain.Output)
	}
	if _, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "sandbox.set", Message: "off"}); err != nil {
		t.Fatalf("reset sandbox off: %v", err)
	}
}

func TestWorkbenchGitHunkAndWorktreeActions(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit(repo, "init", "-b", "main")
	runGit(repo, "config", "user.email", "test@example.invalid")
	runGit(repo, "config", "user.name", "Vantry Test")
	runGit(repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "app.txt"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(repo, "add", "app.txt")
	runGit(repo, "commit", "-m", "init")

	dataDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dataDir, 0o700)
	service, err := core.Open(ctx, core.Options{
		DBPath:   filepath.Join(dataDir, "state.sqlite"),
		Provider: workbenchProvider{requests: make(chan provider.Request, 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	wb := &workbenchService{Service: service, config: Config{DataDir: dataDir}}

	// Test git.hunk.stage on primary repo.
	if err := os.WriteFile(filepath.Join(repo, "app.txt"), []byte("one-mod\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hunkInput, _ := json.Marshal(map[string]any{
		"file": "app.txt", "staged": false, "hunkIndex": 0,
	})
	hunkResp, err := wb.Workbench(ctx, api.WorkbenchRequest{
		Action: "git.hunk.stage", Workspace: repo, Branch: "main", Input: hunkInput,
	})
	if err != nil {
		t.Fatalf("git.hunk.stage: %v", err)
	}
	if hunkResp.Git == nil || len(hunkResp.Git.Files) != 1 || hunkResp.Git.Files[0].Index != "M" {
		t.Fatalf("expected staged file after git.hunk.stage, got %+v", hunkResp.Git)
	}
	// Reset repo clean before worktree test.
	runGit(repo, "checkout", "HEAD", "--", "app.txt")

	// Test worktree status & merge_cleanup.
	wtSpec := api.WorkspaceSpec{Kind: "worktree", Repository: repo, BaseRef: "HEAD"}
	forked, err := service.Dispatch(ctx, api.Command{
		Version: api.ProtocolVersion, CommandID: "create-wt", Kind: api.CreateThread,
		Title: "wt", Workspace: &wtSpec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Prepare(ctx, dataDir, wtSpec, forked.ThreadID); err != nil {
		t.Fatalf("prepare worktree: %v", err)
	}
	wsResp, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "workspace", ThreadID: forked.ThreadID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsResp.Workspace, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(wsResp.Workspace, "add", "feature.txt")
	runGit(wsResp.Workspace, "commit", "-m", "add feature")

	wtStatusResp, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "git.worktree.status", ThreadID: forked.ThreadID})
	if err != nil {
		t.Fatalf("git.worktree.status: %v", err)
	}
	if !strings.Contains(wtStatusResp.Output, `"canMerge":true`) {
		t.Fatalf("expected canMerge:true, got %s", wtStatusResp.Output)
	}

	mergeResp, err := wb.Workbench(ctx, api.WorkbenchRequest{Action: "git.worktree.merge_cleanup", ThreadID: forked.ThreadID})
	if err != nil {
		t.Fatalf("git.worktree.merge_cleanup: %v", err)
	}
	if !strings.Contains(mergeResp.Output, `"cleanedUp":true`) {
		t.Fatalf("expected cleanedUp:true, got %s", mergeResp.Output)
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatalf("merged feature.txt missing from main repo: %v", err)
	}
}
