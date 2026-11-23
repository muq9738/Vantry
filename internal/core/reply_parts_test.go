package core

import (
	"encoding/json"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

// TestReplyPartsKeepReasoningAndToolPlacement proves reasoning is stored beside
// the reply, never in it, each tool call records where it began in the reply,
// and output events carry only their delta.
func TestReplyPartsKeepReasoningAndToolPlacement(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	thinking := func(delta string) map[string]any {
		return map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": delta}}
	}
	p.add("thinking_delta", thinking("先读"))
	p.add("thinking_delta", thinking("文件"))
	p.add("text_delta", "先看代码。")
	p.add("tool_execution_start", map[string]any{"toolCallId": "call-1", "toolName": "read", "args": map[string]any{"path": "a.go"}})
	p.add("tool_execution_end", map[string]any{"toolCallId": "call-1", "toolName": "read", "result": "package a"})
	p.add("text_delta", "完成。")
	p.finish()
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)

	snap := snapshotOf(t, c)
	var run api.Run
	for _, r := range snap.Runs {
		if r.ID == sent.RunID {
			run = r
		}
	}
	if run.Output != "先看代码。完成。" || run.Reasoning != "先读文件" {
		t.Fatalf("output=%q reasoning=%q", run.Output, run.Reasoning)
	}
	tool, ok := findTool(snap, attemptID+":call-1")
	if !ok || tool.OutputOffset != len("先看代码。") {
		t.Fatalf("tool %+v ok=%v, want offset %d", tool, ok, len("先看代码。"))
	}
	deltas := 0
	for _, ev := range loadAllEvents(t, c) {
		if ev.Type != eventRunOutput && ev.Type != eventRunReasoning {
			continue
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if _, full := data["output"]; full {
			t.Fatalf("event %d repeats the whole reply: %s", ev.Sequence, ev.Data)
		}
		deltas++
	}
	if deltas != 4 {
		t.Fatalf("output and reasoning events = %d, want 4", deltas)
	}
}
