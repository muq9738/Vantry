package pi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/provider"
)

func TestFinderRuntimeResolvesExistingPiAndNode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	cli := filepath.Join(home, "runtime", "lib", "pi", "cli.js")
	node := filepath.Join(home, "runtime", "bin", "node")
	link := filepath.Join(home, ".local", "bin", "pi")
	for _, path := range []string{cli, node, link} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cli, []byte("#!/usr/bin/env node\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, []byte("#!/bin/sh\nprintf 'node-runtime-ok'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cli, link); err != nil {
		t.Fatal(err)
	}
	resolved := resolvePiExecutable("pi")
	command := exec.Command(resolved)
	command.Env = piEnvironment(resolved)
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "node-runtime-ok" {
		t.Fatalf("minimal GUI environment failed: %s %v", output, err)
	}
}

func TestConfiguredModelUsesOnlyPiModelSelection(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", directory)
	if err := os.WriteFile(filepath.Join(directory, "settings.json"), []byte(`{"defaultProvider":"example","defaultModel":"model/variant","unrelated":"ignored"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if model := ConfiguredModel(); model != "example/model/variant" {
		t.Fatalf("configured model=%q", model)
	}
}

func TestRuntimeEnvironmentDropsLauncherRepositoryRouting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	cli := filepath.Join(home, "runtime", "lib", "pi", "cli.js")
	node := filepath.Join(home, "runtime", "bin", "node")
	link := filepath.Join(home, ".local", "bin", "pi")
	for _, path := range []string{cli, node, link} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nprintf '%s|%s' \"${GIT_DIR-absent}\" \"${GIT_AUTHOR_NAME-absent}\"\n"
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, []byte("#!/bin/sh\nprintf 'node-runtime-ok'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cli, link); err != nil {
		t.Fatal(err)
	}
	decoy := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))
	t.Setenv("GIT_AUTHOR_NAME", "Vantry User")

	resolved := resolvePiExecutable("pi")
	command := exec.Command(resolved)
	command.Env = piEnvironment(resolved)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Pi subprocess failed: %s %v", output, err)
	}
	if string(output) != "absent|Vantry User" {
		t.Fatalf("Pi inherited launcher routing or lost user configuration: %q", output)
	}
	values := map[string]string{}
	for _, entry := range piEnvironment(resolved) {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		if value, ok := values[key]; ok {
			t.Fatalf("Pi environment kept %s=%s", key, value)
		}
	}
	if resolvedHome, err := filepath.EvalSymlinks(home); err != nil {
		t.Fatal(err)
	} else if values["PATH"] != filepath.Join(resolvedHome, "runtime", "bin")+":/usr/bin:/bin" {
		t.Fatalf("Pi environment lost the Finder Node lookup: %q", values["PATH"])
	}
}

func TestStopCancelsWorkerWhileWaitingForWorkspaceWriter(t *testing.T) {
	root := t.TempDir()
	calls := filepath.Join(root, "pi-calls")
	fake := writeFakePi(t, root, `printf '%s\n' '{"type":"agent_settled","aborted":false}'`, calls, filepath.Join(root, "args"))
	instance, request := newTestProvider(t, root, fake)
	lock, err := workspace.LockDirectory(context.Background(), instance.options.DefaultWorkspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Unlock(lock)
	defer instance.Stop(context.Background(), request)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- instance.RunEvents(ctx, request, func(provider.Event) error { return nil }) }()
	preparedPath := filepath.Join(root, "workspaces", request.ThreadID, "prepared.json")
	for {
		if _, err := os.Stat(preparedPath); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("worker did not reach workspace preparation")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := instance.Stop(ctx, request); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stop while waiting: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stopped worker stayed blocked behind a workspace writer")
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pi started while workspace remained locked: %v", err)
	}
}
