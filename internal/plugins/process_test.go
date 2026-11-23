package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessPluginShellAndCompiledGoBinaryProtocol(t *testing.T) {
	root := t.TempDir()
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "README.md"), "workspace-hello")

	// 1. Shell script executed via interpreter ["sh", "run.sh"]
	shDir := filepath.Join(root, "shell-echo")
	if err := os.MkdirAll(shDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(shDir, "plugin.json"), `{
		"id": "shell-echo",
		"name": "Shell Echo",
		"description": "Shell process plugin",
		"runtime": "process",
		"command": ["sh", "run.sh"]
	}`)
	writeFile(t, filepath.Join(shDir, "run.sh"), `#!/bin/sh
cat
`)

	// 2. Compiled Go binary executed directly via ["./plugin-bin"]
	binDir := filepath.Join(root, "go-bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	writeFile(t, filepath.Join(srcDir, "go.mod"), "module testplugin\n\ngo 1.24\n")
	writeFile(t, filepath.Join(srcDir, "main.go"), `package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func main() {
	var req struct {
		Version   int             `+"`json:\"version\"`"+`
		PluginID  string          `+"`json:\"pluginId\"`"+`
		Workspace string          `+"`json:\"workspace\"`"+`
		Input     json.RawMessage `+"`json:\"input\"`"+`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		os.Stderr.WriteString("decode error: " + err.Error())
		os.Exit(2)
	}
	readme, err := os.ReadFile(filepath.Join(req.Workspace, "README.md"))
	if err != nil {
		os.Stderr.WriteString("read workspace: " + err.Error())
		os.Exit(3)
	}
	resp := map[string]any{
		"vantryPanel": map[string]any{
			"version": 1,
			"title":   req.PluginID,
			"widgets": []any{
				map[string]any{"kind": "badge", "label": "v" + string(rune('0'+req.Version)), "tone": "success"},
				map[string]any{"kind": "text", "text": string(readme)},
			},
		},
	}
	_ = json.NewEncoder(os.Stdout).Encode(resp)
}
`)
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	buildCmd := exec.Command(goBin, "build", "-o", filepath.Join(binDir, "plugin-bin"), ".")
	buildCmd.Dir = srcDir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build test plugin binary: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(binDir, "plugin.json"), `{
		"id": "go-bin",
		"name": "Go Binary Plugin",
		"runtime": "process",
		"command": ["./plugin-bin"],
		"timeoutMs": 5000,
		"permissions": ["workspace:read"]
	}`)

	host := Host{Directory: root, WorkspaceDir: workspace}
	list, err := host.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List: %+v %v", list, err)
	}
	for _, item := range list {
		if item.Error != "" {
			t.Fatalf("unexpected list error on %s: %s", item.ID, item.Error)
		}
	}

	// Verify exact stdin envelope via shell-echo.
	echoOut, err := host.Run(context.Background(), "shell-echo", json.RawMessage(`{"task":"inspect","n":7}`))
	if err != nil {
		t.Fatalf("shell-echo Run: %v", err)
	}
	var received struct {
		Version   int            `json:"version"`
		PluginID  string         `json:"pluginId"`
		Workspace string         `json:"workspace"`
		Input     map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(echoOut), &received); err != nil {
		t.Fatalf("unmarshal echoed stdin: %v (raw=%q)", err, echoOut)
	}
	if received.Version != 1 || received.PluginID != "shell-echo" || received.Workspace != workspace || received.Input["task"] != "inspect" {
		t.Fatalf("unexpected stdin envelope: %+v", received)
	}

	// Verify compiled Go binary plugin + declarative panel schema integration.
	binOut, err := host.Run(context.Background(), "go-bin", nil)
	if err != nil {
		t.Fatalf("go-bin Run: %v", err)
	}
	panel, ok := ParsePanelOutput(binOut)
	if !ok || panel.Title != "go-bin" || len(panel.Widgets) != 2 || panel.Widgets[1].Text != "workspace-hello" {
		t.Fatalf("unexpected panel output from binary plugin: ok=%v panel=%+v raw=%s", ok, panel, binOut)
	}
}

func TestProcessPluginPythonIfAvailable(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	pyDir := filepath.Join(root, "py-plugin")
	if err := os.MkdirAll(pyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pyDir, "plugin.json"), `{
		"id": "py-plugin",
		"name": "Python Plugin",
		"runtime": "process",
		"command": ["python3", "main.py"]
	}`)
	writeFile(t, filepath.Join(pyDir, "main.py"), `import json, sys
req = json.load(sys.stdin)
assert req["version"] == 1
assert req["pluginId"] == "py-plugin"
print(json.dumps({"greeting": "hello " + req["input"].get("who", "world")}))
`)
	host := Host{Directory: root}
	out, err := host.Run(context.Background(), "py-plugin", json.RawMessage(`{"who":"python"}`))
	if err != nil || out != `{"greeting": "hello python"}` {
		t.Fatalf("python plugin: out=%q err=%v", out, err)
	}
}

func TestProcessPluginListDoesNotExecuteAndValidatesCommandBoundaries(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	outsideFile := filepath.Join(outside, "outside.sh")
	writeFile(t, outsideFile, "#!/bin/sh\necho outside\n")
	if err := os.Chmod(outsideFile, 0o700); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "executed-on-list")
	validDir := filepath.Join(root, "valid-proc")
	if err := os.MkdirAll(validDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(validDir, "plugin.json"), `{"id":"valid-proc","name":"Valid","runtime":"process","command":["./run.sh"]}`)
	scriptPath := filepath.Join(validDir, "run.sh")
	writeFile(t, scriptPath, fmt.Sprintf("#!/bin/sh\ntouch %q\necho ok\n", marker))
	if err := os.Chmod(scriptPath, 0o700); err != nil {
		t.Fatal(err)
	}

	// Symlinked script inside plugin directory.
	symDir := filepath.Join(root, "sym-proc")
	if err := os.MkdirAll(symDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(symDir, "plugin.json"), `{"id":"sym-proc","name":"Sym","runtime":"process","command":["sh","link.sh"]}`)
	if err := os.Symlink(outsideFile, filepath.Join(symDir, "link.sh")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		id       string
		manifest string
		setup    func(dir string)
		wantErr  string
	}{
		{
			id:       "empty-cmd",
			manifest: `{"id":"empty-cmd","name":"Empty","runtime":"process","command":[]}`,
			wantErr:  "command",
		},
		{
			id:       "escape-script",
			manifest: `{"id":"escape-script","name":"Escape","runtime":"process","command":["sh","../outside.sh"]}`,
			wantErr:  "目录内",
		},
		{
			id:       "abs-exe",
			manifest: `{"id":"abs-exe","name":"AbsExe","runtime":"process","command":["/bin/echo","hi"]}`,
			wantErr:  "目录内",
		},
		{
			id:       "inline-eval",
			manifest: `{"id":"inline-eval","name":"Inline","runtime":"process","command":["sh","-c","echo pwned"]}`,
			wantErr:  "内联参数",
		},
		{
			id:       "not-exec",
			manifest: `{"id":"not-exec","name":"NotExec","runtime":"process","command":["./plain.sh"]}`,
			setup: func(dir string) {
				writeFile(t, filepath.Join(dir, "plain.sh"), "#!/bin/sh\necho hi\n")
			},
			wantErr: "可执行权限",
		},
	}

	for _, tc := range cases {
		dir := filepath.Join(root, tc.id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "plugin.json"), tc.manifest)
		if tc.setup != nil {
			tc.setup(dir)
		}
	}

	host := Host{Directory: root}
	list, err := host.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("List() unexpectedly executed process plugin")
	}

	byID := map[string]string{}
	for _, item := range list {
		byID[item.ID] = item.Error
	}
	if byID["valid-proc"] != "" {
		t.Fatalf("valid-proc should have no error, got %q", byID["valid-proc"])
	}
	if !strings.Contains(byID["sym-proc"], "符号链接") {
		t.Fatalf("sym-proc should report symlink error, got %q", byID["sym-proc"])
	}
	for _, tc := range cases {
		if !strings.Contains(byID[tc.id], tc.wantErr) {
			t.Fatalf("%s list diagnostic = %q; want %q", tc.id, byID[tc.id], tc.wantErr)
		}
		if _, err := host.Run(context.Background(), tc.id, nil); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s run err = %v; want %q", tc.id, err, tc.wantErr)
		}
	}
}

func TestProcessPluginStdoutAndStderrBounds(t *testing.T) {
	root := t.TempDir()

	// 1. Oversized stdout (> 1 MiB) is stopped immediately and rejected.
	bigDir := filepath.Join(root, "big-stdout")
	if err := os.MkdirAll(bigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(bigDir, "plugin.json"), `{"id":"big-stdout","name":"Big","runtime":"process","command":["sh","big.sh"],"timeoutMs":5000}`)
	writeFile(t, filepath.Join(bigDir, "big.sh"), "#!/bin/sh\nhead -c 1100000 /dev/zero | tr '\\0' 'a'\n")

	// 2. Oversized stderr (> 8 KiB) with non-zero exit is bounded and truncated cleanly.
	errDir := filepath.Join(root, "big-stderr")
	if err := os.MkdirAll(errDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(errDir, "plugin.json"), `{"id":"big-stderr","name":"Err","runtime":"process","command":["sh","err.sh"]}`)
	writeFile(t, filepath.Join(errDir, "err.sh"), "#!/bin/sh\nprintf 'ERR_START:' >&2\nhead -c 20000 /dev/zero | tr '\\0' 'e' >&2\nexit 7\n")

	// 3. Large stderr (> 8 KiB) with exit 0 does not break pipe or fail valid stdout.
	noisyDir := filepath.Join(root, "noisy-ok")
	if err := os.MkdirAll(noisyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(noisyDir, "plugin.json"), `{"id":"noisy-ok","name":"Noisy","runtime":"process","command":["sh","noisy.sh"]}`)
	writeFile(t, filepath.Join(noisyDir, "noisy.sh"), "#!/bin/sh\nhead -c 24000 /dev/zero | tr '\\0' 'w' >&2\nprintf 'clean-stdout\\n'\n")

	host := Host{Directory: root}
	if _, err := host.Run(context.Background(), "big-stdout", nil); err == nil || !strings.Contains(err.Error(), "超过 1 MiB") {
		t.Fatalf("big-stdout expected 1 MiB error, got %v", err)
	}

	_, err := host.Run(context.Background(), "big-stderr", nil)
	if err == nil || !strings.Contains(err.Error(), "ERR_START:") || !strings.Contains(err.Error(), "[stderr 已截断]") {
		t.Fatalf("big-stderr diagnostic mismatch: %v", err)
	}
	if len(err.Error()) > maxProcessStderrBytes+512 {
		t.Fatalf("stderr diagnostic exceeded 8 KiB bound: len=%d", len(err.Error()))
	}

	out, err := host.Run(context.Background(), "noisy-ok", nil)
	if err != nil || out != "clean-stdout" {
		t.Fatalf("noisy-ok failed: out=%q err=%v", out, err)
	}
}

func TestProcessPluginTimeoutKillsProcessGroupAndGrandchildren(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	hangDir := filepath.Join(root, "tree-hang")
	if err := os.MkdirAll(hangDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(hangDir, "plugin.json"), `{
		"id": "tree-hang",
		"name": "Tree Hang",
		"runtime": "process",
		"command": ["sh", "hang.sh"],
		"timeoutMs": 120
	}`)
	// Spawn a background grandchild that ignores SIGTERM and keeps stdout/stderr open.
	writeFile(t, filepath.Join(hangDir, "hang.sh"), fmt.Sprintf(`#!/bin/sh
trap '' TERM
(
  trap '' TERM
  sleep 60
) &
child_pid=$!
printf '%%s' "$child_pid" > %q
wait "$child_pid"
`, pidFile))

	host := Host{Directory: root}
	started := time.Now()
	_, err := host.Run(context.Background(), "tree-hang", nil)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("process group kill blocked too long (%s), pipe held by grandchild?", elapsed)
	}

	rawPID, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("grandchild pid file was not written before timeout: %v", readErr)
	}
	grandchildPID, convErr := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if convErr != nil || grandchildPID <= 0 {
		t.Fatalf("invalid grandchild pid %q: %v", string(rawPID), convErr)
	}
	t.Cleanup(func() { _ = syscall.Kill(grandchildPID, syscall.SIGKILL) })

	if killErr := syscall.Kill(grandchildPID, 0); !errors.Is(killErr, syscall.ESRCH) {
		t.Fatalf("grandchild process %d survived process-group termination: err=%v", grandchildPID, killErr)
	}
}
