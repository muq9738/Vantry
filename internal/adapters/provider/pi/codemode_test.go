package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeCodemodeDiscoveryStructuredQueriesAndGuards(t *testing.T) {
	piExecutable := resolvePiExecutable("pi")
	loader, ok := findPiExtensionLoader(piExecutable)
	if !ok {
		t.Skip("installed Pi SDK is required for native QuickJS acceptance")
	}
	sdkEntry := filepath.Clean(filepath.Join(filepath.Dir(loader), "..", "..", "index.js"))
	if _, err := os.Stat(sdkEntry); err != nil {
		t.Skip("installed Pi SDK entry is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "vantry-codemode-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	toolsPath, approvalPath := filepath.Join(root, "vantry_tools.ts"), filepath.Join(root, "approval.ts")
	if err := os.WriteFile(toolsPath, vantryToolsExtension, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(approvalPath, approvalExtension, 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("testdata", "codemode_contract.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "node", script, sdkEntry, toolsPath, approvalPath, root)
	command.Dir, command.Env = root, piEnvironment(piExecutable)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native codemode contract: %v\n%s", err, output)
	}
	var summary struct {
		Status                  string `json:"status"`
		RegisteredVantryTools   int    `json:"registeredVantryTools"`
		DeclaredVantryTools     int    `json:"declaredVantryTools"`
		FilteredResultBytes     int    `json:"filteredResultBytes"`
		UnfilteredFixtureBytes  int    `json:"unfilteredFixtureBytes"`
		CancellationReachedCore bool   `json:"cancellationReachedCore"`
		ToolDeclarationBytes    struct {
			Baseline int `json:"baseline"`
			Compact  int `json:"compact"`
		} `json:"toolDeclarationBytes"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &summary); err != nil {
		t.Fatalf("summary: %v\n%s", err, output)
	}
	if summary.Status != "passed" || !summary.CancellationReachedCore || summary.FilteredResultBytes >= summary.UnfilteredFixtureBytes {
		t.Fatalf("native contract summary: %+v", summary)
	}
	// The adapter must keep the real nested events, including the parent call
	// identity, so Core can persist the same audited tools as direct calls.
	raw, err := os.ReadFile(filepath.Join(root, "native-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	state := piStreamState{}
	starts, ends := 0, 0
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		event, err := state.consume(line)
		if err != nil {
			t.Fatal(err)
		}
		var nested struct {
			ParentToolCallID string `json:"parentToolCallId"`
			ToolCallID       string `json:"toolCallId"`
		}
		if err := json.Unmarshal(event.Data, &nested); err != nil || nested.ParentToolCallID == "" || !strings.HasPrefix(nested.ToolCallID, nested.ParentToolCallID+"/") {
			t.Fatalf("lost nested tool identity: %s, %v", event.Data, err)
		}
		if event.Type == "tool_execution_start" {
			starts++
		} else if event.Type == "tool_execution_end" {
			ends++
		}
	}
	if starts < 7 || starts != ends {
		t.Fatalf("nested lifecycle events: starts=%d ends=%d", starts, ends)
	}
	t.Logf("tool declarations: %d -> %d bytes; Vantry declarations: %d/%d; filtered result: %d/%d bytes",
		summary.ToolDeclarationBytes.Baseline, summary.ToolDeclarationBytes.Compact,
		summary.DeclaredVantryTools, summary.RegisteredVantryTools, summary.FilteredResultBytes, summary.UnfilteredFixtureBytes)
}

func TestPiRPCExplicitBuiltinsSurviveDisabledUserExtensions(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	piExecutable := resolvePiExecutable("pi")
	if _, err := exec.LookPath(piExecutable); err != nil {
		t.Skip("installed Pi not available")
	}
	root := t.TempDir()
	tools := filepath.Join(root, "vantry_tools.ts")
	probe := filepath.Join(root, "probe.ts")
	if err := os.WriteFile(tools, vantryToolsExtension, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, []byte(`export default function(pi) {
  pi.on("session_start", (_event, ctx) => {
    ctx.ui.notify(JSON.stringify({vantryCodemodeProbe:true, active:pi.getActiveTools(), callable:pi.getAllTools().filter(t=>t.name.startsWith("vantry_")).map(t=>({name:t.name,exposure:t.exposure}))}), "info");
  });
}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, piExecutable, "--offline", "--mode", "rpc", "--no-session", "--no-extensions",
		"--extension", "builtin:mcp", "--extension", "builtin:codemode", "--extension", "builtin:tool-search", "--tools", "+codemode,+tool_search", "--extension", tools, "--extension", probe)
	command.Dir, command.Env = root, piEnvironment(piExecutable)
	command.Stdin = strings.NewReader("{\"id\":\"probe-state\",\"type\":\"get_state\"}\n")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("Pi RPC: %v\n%s", err, stderr.String())
	}
	var active []string
	ready := false
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var record rpcRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record.Type == "response" && record.ID == "probe-state" && record.Success {
			ready = true
		}
		if record.Type != "extension_ui_request" || record.Method != "notify" {
			continue
		}
		var text string
		if json.Unmarshal(record.Message, &text) != nil {
			continue
		}
		var probe struct {
			VantryCodemodeProbe bool     `json:"vantryCodemodeProbe"`
			Active              []string `json:"active"`
		}
		if json.Unmarshal([]byte(text), &probe) == nil && probe.VantryCodemodeProbe {
			active = probe.Active
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	contains := func(name string) bool {
		for _, item := range active {
			if item == name {
				return true
			}
		}
		return false
	}
	if !ready || !contains("codemode") || !contains("tool_search") || !contains("vantry_request_secret") || !contains("vantry_terminal_run") || contains("vantry_delegate_task") || contains("vantry_thread_read") {
		t.Fatalf("built-in/discovery loadout did not survive --no-extensions: ready=%v active=%v stderr=%s", ready, active, stderr.String())
	}
}
