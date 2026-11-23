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
	"github.com/muq9738/Vantry/internal/environments"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestEndToEndWorkbenchAndCoreIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dataDir, err := os.MkdirTemp("/tmp", "vt-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, "hello.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seed an environment catalog entry and a workspace-reading plugin.
	envCat, err := environments.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := envCat.Upsert(environments.EnvironmentEntry{
		ID: "gpu-dev", Name: "GPU Build Box", Host: "dev@gpu-box", SocketPath: "/var/run/vantry/core.sock",
	}); err != nil {
		t.Fatal(err)
	}

	pluginDir := filepath.Join(dataDir, "plugins", "ws-reader")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{
		"id": "ws-reader",
		"name": "Workspace Reader",
		"runtime": "process",
		"command": ["sh", "run.sh"]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "run.sh"), []byte("cat -- '"+strings.ReplaceAll(filepath.Join(wsDir, "hello.go"), "'", "'\\''")+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	coreCtx, stopCore := context.WithCancel(ctx)
	coreDone := make(chan error, 1)
	cfg := Config{
		DataDir:    dataDir,
		ProviderID: "controlled",
		Delay:      5 * time.Millisecond,
		Workspace:  &api.WorkspaceSpec{Kind: "directory", Path: wsDir},
	}
	go func() { coreDone <- RunCore(coreCtx, cfg) }()
	defer func() {
		stopCore()
		<-coreDone
	}()

	// Wait until socket is ready and EnsureCore succeeds via capability handshake.
	var client *local.Client
	var lastErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := EnsureCore(ctx, cfg, true)
		lastErr = err
		if err == nil {
			client = c
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if client == nil {
		t.Fatalf("EnsureCore attach failed: %v", lastErr)
	}
	defer client.Close()

	// 1. Verify capability status endpoint.
	status, err := client.ServerStatus(ctx)
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	if len(status.Capabilities) == 0 {
		t.Fatalf("expected capabilities, got %+v", status)
	}

	// 2. Verify files.list, files.read, files.search, environments.list, and async workspace plugin.
	listResp, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "files.list", Workspace: wsDir})
	if err != nil || !strings.Contains(listResp.Output, "hello.go") {
		t.Fatalf("files.list = %+v, err=%v", listResp, err)
	}
	readResp, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "files.read", Workspace: wsDir, Paths: []string{"hello.go"}})
	if err != nil || !strings.Contains(readResp.Output, "package main") {
		t.Fatalf("files.read = %+v, err=%v", readResp, err)
	}
	searchResp, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "files.search", Workspace: wsDir, Message: "hello"})
	if err != nil || !strings.Contains(searchResp.Output, "hello.go") {
		t.Fatalf("files.search = %+v, err=%v", searchResp, err)
	}
	envResp, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "environments.list"})
	if err != nil || !strings.Contains(envResp.Output, "GPU Build Box") {
		t.Fatalf("environments.list = %+v, err=%v", envResp, err)
	}
	plugResp, err := client.Workbench(ctx, api.WorkbenchRequest{
		Action: "plugins.run", PluginID: "ws-reader", Workspace: wsDir, Input: json.RawMessage(`{}`),
	})
	if err != nil || !strings.Contains(plugResp.Output, "package main") {
		t.Fatalf("plugins.run = %+v, err=%v", plugResp, err)
	}

	// 3. Create thread with auto-accept-edits + plan mode, run one turn, seed Pi session, fork via RunID, and test desktop queue aliases.
	createRec, err := client.Dispatch(ctx, api.Command{
		Version:         api.ProtocolVersion,
		CommandID:       "cmd-create-1",
		Kind:            api.CreateThread,
		Title:           "源会话",
		ApprovalPolicy:  "auto-accept-edits",
		InteractionMode: "plan",
		Workspace:       &api.WorkspaceSpec{Kind: "directory", Path: wsDir},
	})
	if err != nil || !createRec.Accepted {
		t.Fatalf("create thread: %+v, err=%v", createRec, err)
	}
	sendRec, err := client.Dispatch(ctx, api.Command{
		Version:   api.ProtocolVersion,
		CommandID: "cmd-send-1",
		Kind:      api.SendMessage,
		ThreadID:  createRec.ThreadID,
		Message:   "规划第一步",
	})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}

	// Wait for controlled run to complete.
	for {
		detail, err := client.ThreadDetail(ctx, api.ThreadDetailQuery{ThreadID: createRec.ThreadID, LimitRuns: 10})
		if err != nil {
			t.Fatalf("ThreadDetail: %v", err)
		}
		if len(detail.Runs) == 1 && detail.Runs[0].Status == api.RunCompleted {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Seed a valid Pi session JSONL file for source thread to verify ForkSession hook clones it.
	sessionsDir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionLines := strings.Join([]string{
		`{"type":"session","version":3,"id":"sess-1","timestamp":"2026-11-14T00:00:00Z","cwd":"/tmp"}`,
		`{"type":"message","id":"m1","parentId":null,"timestamp":"2026-11-14T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"规划第一步"}]}}`,
		`{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-11-14T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"已完成规划"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(sessionsDir, createRec.ThreadID+".jsonl"), []byte(sessionLines), 0o600); err != nil {
		t.Fatal(err)
	}

	// Fork using RunID (Desktop UI style).
	forkRec, err := client.Dispatch(ctx, api.Command{
		Version:   api.ProtocolVersion,
		CommandID: "cmd-fork-1",
		Kind:      api.ForkThread,
		ThreadID:  createRec.ThreadID,
		RunID:     sendRec.RunID,
	})
	if err != nil || forkRec.ThreadID == "" || forkRec.ThreadID == createRec.ThreadID {
		t.Fatalf("fork thread: %+v, err=%v", forkRec, err)
	}
	clonedBytes, err := os.ReadFile(filepath.Join(sessionsDir, forkRec.ThreadID+".jsonl"))
	if err != nil || !strings.Contains(string(clonedBytes), "已完成规划") {
		t.Fatalf("expected cloned Pi session file, err=%v content=%q", err, string(clonedBytes))
	}

	// Delete forked thread cleanly.
	if _, err := client.Dispatch(ctx, api.Command{
		Version:   api.ProtocolVersion,
		CommandID: "cmd-del-fork",
		Kind:      api.DeleteThread,
		ThreadID:  forkRec.ThreadID,
	}); err != nil {
		t.Fatalf("delete forked thread: %v", err)
	}
}
