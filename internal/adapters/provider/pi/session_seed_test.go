package pi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCloneSessionFileMissingOrEmptySourceIsNoop(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := CloneSessionFile(dataDir, "thread-src", "thread-dst", 0); err != nil {
		t.Fatalf("missing source session returned error: %v", err)
	}
	sessionsDir := filepath.Join(dataDir, "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("sessions dir = %v, err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir, "thread-dst.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source created unexpected target file: %v", err)
	}

	// Empty source file is also a safe no-op.
	if err := os.WriteFile(filepath.Join(sessionsDir, "thread-empty.jsonl"), []byte("  \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CloneSessionFile(dataDir, "thread-empty", "thread-dst-2", 2); err != nil {
		t.Fatalf("empty source session returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir, "thread-dst-2.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty source created unexpected target file: %v", err)
	}
}

func TestCloneSessionFileValidationAndCorruptionErrors(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		dir   string
		src   string
		dst   string
		turns int
	}{
		{"empty-dir", "", "th-1", "th-2", 0},
		{"traversal-src", dataDir, "../th-1", "th-2", 0},
		{"traversal-dst", dataDir, "th-1", "../th-2", 0},
		{"same-thread", dataDir, "th-1", "th-1", 0},
	}
	for _, tc := range cases {
		if err := CloneSessionFile(tc.dir, tc.src, tc.dst, tc.turns); err == nil {
			t.Fatalf("%s unexpectedly succeeded", tc.name)
		}
	}

	sessionsDir := filepath.Join(dataDir, "sessions")
	if err := makePrivateDir(sessionsDir); err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(sessionsDir, "corrupt-src.jsonl")
	if err := os.WriteFile(corruptPath, []byte("{\"type\":\"session\",\"version\":3,\"id\":\"s1\"}\n{broken-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CloneSessionFile(dataDir, "corrupt-src", "corrupt-dst", 0); err == nil {
		t.Fatal("corrupt JSONL line was not rejected")
	}
}

func TestCloneSessionFileBranchPruningTruncationAndRealPiCompatibility(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionsDir := filepath.Join(dataDir, "sessions")
	if err := makePrivateDir(sessionsDir); err != nil {
		t.Fatal(err)
	}

	largePayload := strings.Repeat("large-context-line-", 8000) // ~152KB > 64KB scanner buffer
	records := []map[string]any{
		{
			"type":      "session",
			"version":   3,
			"id":        "orig-session-1",
			"timestamp": "2026-04-01T10:00:00.000Z",
			"cwd":       "/tmp/project",
		},
		{
			"type":      "message",
			"id":        "e1",
			"parentId":  nil,
			"timestamp": "2026-04-01T10:00:01.000Z",
			"message": map[string]any{
				"role":      "system",
				"content":   "",
				"sections":  map[string]any{"preamble": "You are Vantry."},
				"timestamp": 1775037601000,
			},
		},
		{
			"type":      "model_change",
			"id":        "e2",
			"parentId":  "e1",
			"timestamp": "2026-04-01T10:00:02.000Z",
			"provider":  "magpie",
			"modelId":   "deepseek-v4",
		},
		{
			"type":      "message",
			"id":        "e3",
			"parentId":  "e2",
			"timestamp": "2026-04-01T10:00:03.000Z",
			"message": map[string]any{
				"role":      "user",
				"content":   "Inspect repository",
				"timestamp": 1775037603000,
			},
		},
		// Abandoned side branch from e3 (must be pruned when cloning active branch)
		{
			"type":      "message",
			"id":        "e_abandoned",
			"parentId":  "e3",
			"timestamp": "2026-04-01T10:00:03.500Z",
			"message": map[string]any{
				"role":       "assistant",
				"content":    []map[string]any{{"type": "text", "text": "abandoned reply"}},
				"provider":   "magpie",
				"model":      "deepseek-v4",
				"stopReason": "stop",
				"timestamp":  1775037603500,
			},
		},
		// Active branch continues from e3: Assistant Turn #1 (tool call)
		{
			"type":      "message",
			"id":        "e4",
			"parentId":  "e3",
			"timestamp": "2026-04-01T10:00:04.000Z",
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "toolCall", "id": "call_1", "name": "read", "arguments": map[string]any{"path": "README.md"}},
				},
				"provider":   "magpie",
				"model":      "deepseek-v4",
				"stopReason": "toolUse",
				"timestamp":  1775037604000,
			},
		},
		// Trailing toolResult + usage belonging to Assistant Turn #1
		{
			"type":      "message",
			"id":        "e5",
			"parentId":  "e4",
			"timestamp": "2026-04-01T10:00:05.000Z",
			"message": map[string]any{
				"role":       "toolResult",
				"toolCallId": "call_1",
				"toolName":   "read",
				"content":    []map[string]any{{"type": "text", "text": "README contents"}},
				"isError":    false,
				"timestamp":  1775037605000,
			},
		},
		{
			"type":      "usage",
			"id":        "e6",
			"parentId":  "e5",
			"timestamp": "2026-04-01T10:00:05.500Z",
			"kind":      "cache_warm",
			"provider":  "magpie",
			"model":     "deepseek-v4",
			"usage":     map[string]any{"totalTokens": 100},
		},
		// Assistant Turn #2 (final answer with >128KB line)
		{
			"type":      "message",
			"id":        "e7",
			"parentId":  "e6",
			"timestamp": "2026-04-01T10:00:06.000Z",
			"message": map[string]any{
				"role":       "assistant",
				"content":    []map[string]any{{"type": "text", "text": largePayload}},
				"provider":   "magpie",
				"model":      "deepseek-v4",
				"stopReason": "stop",
				"timestamp":  1775037606000,
			},
		},
		// Second user turn + Assistant Turn #3
		{
			"type":      "message",
			"id":        "e8",
			"parentId":  "e7",
			"timestamp": "2026-04-01T10:00:07.000Z",
			"message": map[string]any{
				"role":      "user",
				"content":   "Follow-up question",
				"timestamp": 1775037607000,
			},
		},
		{
			"type":      "message",
			"id":        "e9",
			"parentId":  "e8",
			"timestamp": "2026-04-01T10:00:08.000Z",
			"message": map[string]any{
				"role":       "assistant",
				"content":    []map[string]any{{"type": "text", "text": "Follow-up answer"}},
				"provider":   "magpie",
				"model":      "deepseek-v4",
				"stopReason": "stop",
				"timestamp":  1775037608000,
			},
		},
		// Unanswered trailing user message #3
		{
			"type":      "message",
			"id":        "e10",
			"parentId":  "e9",
			"timestamp": "2026-04-01T10:00:09.000Z",
			"message": map[string]any{
				"role":      "user",
				"content":   "Pending unanswered prompt",
				"timestamp": 1775037609000,
			},
		},
	}

	sourcePath := filepath.Join(sessionsDir, "th-source.jsonl")
	var buf strings.Builder
	for _, rec := range records {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(sourcePath, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	readEntryIDs := func(targetThread string) (map[string]any, []string) {
		t.Helper()
		targetPath := filepath.Join(sessionsDir, targetThread+".jsonl")
		info, err := os.Stat(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("target session mode = %o, want 0600", info.Mode().Perm())
		}
		lines := strings.Split(strings.TrimSpace(readFile(t, targetPath)), "\n")
		var header map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, line := range lines[1:] {
			var entry struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, entry.ID)
		}
		return header, ids
	}

	// 1. Full active branch clone (maxAssistantTurns <= 0): prunes e_abandoned, keeps e1..e10
	if err := CloneSessionFile(dataDir, "th-source", "th-full", 0); err != nil {
		t.Fatalf("full clone: %v", err)
	}
	header, fullIDs := readEntryIDs("th-full")
	if header["type"] != "session" || header["id"] == "orig-session-1" || header["parentSession"] != sourcePath || header["cwd"] != "/tmp/project" {
		t.Fatalf("unexpected cloned header: %+v", header)
	}
	wantFull := []string{"e1", "e2", "e3", "e4", "e5", "e6", "e7", "e8", "e9", "e10"}
	if strings.Join(fullIDs, ",") != strings.Join(wantFull, ",") {
		t.Fatalf("full clone IDs = %v, want %v", fullIDs, wantFull)
	}

	// 2. Truncate at 1 assistant turn: keeps e4 AND its trailing toolResult (e5) + usage (e6), no dangling toolCall!
	if err := CloneSessionFile(dataDir, "th-source", "th-turn1", 1); err != nil {
		t.Fatalf("turn-1 clone: %v", err)
	}
	_, turn1IDs := readEntryIDs("th-turn1")
	wantTurn1 := []string{"e1", "e2", "e3", "e4", "e5", "e6"}
	if strings.Join(turn1IDs, ",") != strings.Join(wantTurn1, ",") {
		t.Fatalf("turn-1 clone IDs = %v, want %v", turn1IDs, wantTurn1)
	}

	// 3. Truncate at 2 assistant turns: stops cleanly after e7, excludes e8..e10
	if err := CloneSessionFile(dataDir, "th-source", "th-turn2", 2); err != nil {
		t.Fatalf("turn-2 clone: %v", err)
	}
	_, turn2IDs := readEntryIDs("th-turn2")
	wantTurn2 := []string{"e1", "e2", "e3", "e4", "e5", "e6", "e7"}
	if strings.Join(turn2IDs, ",") != strings.Join(wantTurn2, ",") {
		t.Fatalf("turn-2 clone IDs = %v, want %v", turn2IDs, wantTurn2)
	}

	// 4. Truncate at 3 assistant turns: excludes trailing unanswered user message e10
	if err := CloneSessionFile(dataDir, "th-source", "th-turn3", 3); err != nil {
		t.Fatalf("turn-3 clone: %v", err)
	}
	_, turn3IDs := readEntryIDs("th-turn3")
	wantTurn3 := []string{"e1", "e2", "e3", "e4", "e5", "e6", "e7", "e8", "e9"}
	if strings.Join(turn3IDs, ",") != strings.Join(wantTurn3, ",") {
		t.Fatalf("turn-3 clone IDs = %v, want %v", turn3IDs, wantTurn3)
	}

	// 5. Verify with Pi's real SessionManager if installed on this machine
	piExecutable := resolvePiExecutable("pi")
	if _, err := exec.LookPath(piExecutable); err != nil {
		return
	}
	sessionManagerJS := filepath.Join(filepath.Dir(piExecutable), "..", "core", "session-manager.js")
	if _, err := os.Stat(sessionManagerJS); err != nil {
		sessionManagerJS = filepath.Join(filepath.Dir(piExecutable), "core", "session-manager.js")
		if _, err := os.Stat(sessionManagerJS); err != nil {
			return
		}
	}

	verifyScript := `
import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";

const { SessionManager, assertValidSessionId } = await import(pathToFileURL(process.argv[1]).href);
for (const [sessionFile, expectedMsgCount] of [
  [process.argv[2], 8], // system, user, assistant(toolCall), toolResult, assistant, user, assistant, user
  [process.argv[3], 4], // system, user, assistant(toolCall), toolResult
  [process.argv[4], 5], // system, user, assistant(toolCall), toolResult, assistant
  [process.argv[5], 7], // system, user, assistant(toolCall), toolResult, assistant, user, assistant
]) {
  const sm = SessionManager.open(sessionFile);
  assertValidSessionId(sm.getSessionId());
  const ctx = sm.buildSessionContext();
  assert.equal(ctx.messages.length, expectedMsgCount, "unexpected message count for " + sessionFile);
}
console.log("pi-session-manager-ok");
`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	node := exec.CommandContext(
		ctx,
		"node", "--input-type=module", "-e", verifyScript,
		sessionManagerJS,
		filepath.Join(sessionsDir, "th-full.jsonl"),
		filepath.Join(sessionsDir, "th-turn1.jsonl"),
		filepath.Join(sessionsDir, "th-turn2.jsonl"),
		filepath.Join(sessionsDir, "th-turn3.jsonl"),
	)
	node.Env = piEnvironment(piExecutable)
	out, err := node.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "pi-session-manager-ok") {
		t.Fatalf("Pi SessionManager failed to load cloned sessions: %v\n%s", err, out)
	}
}

func TestTruncateSessionFileKeepsHeaderAndTurnsOrRemoves(t *testing.T) {
	dataDir := t.TempDir()
	sessionsDir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sessionsDir, "th-rw.jsonl")
	raw := strings.Join([]string{
		`{"type":"session","version":3,"id":"sess-rw","cwd":"/tmp/ws"}`,
		`{"type":"message","id":"e1","parentId":null,"message":{"role":"user","content":"turn1"}}`,
		`{"type":"message","id":"e2","parentId":"e1","message":{"role":"assistant","content":"ans1"}}`,
		`{"type":"message","id":"e3","parentId":"e2","message":{"role":"user","content":"turn2"}}`,
		`{"type":"message","id":"e4","parentId":"e3","message":{"role":"assistant","content":"ans2"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(target, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := TruncateSessionFile(dataDir, "th-rw", 1); err != nil {
		t.Fatalf("TruncateSessionFile(1): %v", err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(after)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + 2 entries after keep=1, got %d:\n%s", len(lines), string(after))
	}
	if !strings.Contains(lines[0], `"id":"sess-rw"`) || !strings.Contains(lines[2], `"id":"e2"`) {
		t.Fatalf("unexpected truncated lines: %v", lines)
	}

	if err := TruncateSessionFile(dataDir, "th-rw", 0); err != nil {
		t.Fatalf("TruncateSessionFile(0): %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected session file removed at keep=0, stat=%v", err)
	}
	// Missing file on keep=0 is also a clean no-op.
	if err := TruncateSessionFile(dataDir, "th-rw", 0); err != nil {
		t.Fatalf("second TruncateSessionFile(0): %v", err)
	}
}
