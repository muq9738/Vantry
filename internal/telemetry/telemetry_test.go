package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func TestExtractTurnUsageFormats(t *testing.T) {
	t.Run("pi_message_end_lossless", func(t *testing.T) {
		raw := []byte(`{
			"type": "message_end",
			"message": {
				"role": "assistant",
				"provider": "anthropic",
				"model": "claude-sonnet-4-6",
				"responseModel": "claude-sonnet-4-6-20260219",
				"responseId": "msg_0199abc",
				"stopReason": "stop",
				"usage": {
					"input": 1420,
					"output": 530,
					"cacheRead": 12800,
					"cacheWrite": 640,
					"cacheWrite1h": 320,
					"reasoning": 180,
					"totalTokens": 15390,
					"cost": {
						"input": 0.00426,
						"output": 0.00795,
						"cacheRead": 0.00384,
						"cacheWrite": 0.0024,
						"total": 0.01845
					}
				}
			}
		}`)

		u, ok := ExtractTurnUsage(raw)
		if !ok {
			t.Fatal("expected Pi message_end usage to be extracted")
		}
		if u.InputTokens != 1420 || u.OutputTokens != 530 || u.CacheReadTokens != 12800 || u.CacheWriteTokens != 640 {
			t.Fatalf("unexpected token counts: %+v", u)
		}
		if u.CacheWrite1h != 320 || u.ReasoningTokens != 180 || u.TotalTokens != 15390 {
			t.Fatalf("unexpected extended token counts: %+v", u)
		}
		if math.Abs(u.Cost.Total-0.01845) > 1e-9 || u.CostEstimated {
			t.Fatalf("unexpected cost breakdown: %+v (estimated=%v)", u.Cost, u.CostEstimated)
		}
		if u.Provider != "anthropic" || u.Model != "claude-sonnet-4-6" || u.ResponseID != "msg_0199abc" || u.StopReason != "stop" {
			t.Fatalf("unexpected message metadata: %+v", u)
		}
	})

	t.Run("pi_user_message_ignored", func(t *testing.T) {
		raw := []byte(`{"type":"message_end","message":{"role":"user","content":"Review this repository","timestamp":1733234401000}}`)
		if _, ok := ExtractTurnUsage(raw); ok {
			t.Fatal("user message_end without usage must not produce TurnUsage")
		}
	})

	t.Run("claude_generic_usage_with_estimated_cost", func(t *testing.T) {
		raw := []byte(`{
			"type": "message_delta",
			"model": "anthropic/claude-sonnet-4-6",
			"usage": {
				"input_tokens": 2000,
				"output_tokens": 500,
				"cache_read_input_tokens": 5000,
				"cache_creation_input_tokens": 1000
			}
		}`)
		u, ok := ExtractTurnUsage(raw)
		if !ok {
			t.Fatal("expected generic Claude usage to be extracted")
		}
		if u.InputTokens != 2000 || u.OutputTokens != 500 || u.CacheReadTokens != 5000 || u.CacheWriteTokens != 1000 {
			t.Fatalf("unexpected generic Claude tokens: %+v", u)
		}
		if u.TotalTokens != 8500 {
			t.Fatalf("TotalTokens = %d, want 8500", u.TotalTokens)
		}
		if !u.CostEstimated || u.Cost.Total <= 0 {
			t.Fatalf("expected estimated positive cost for known model, got %+v (estimated=%v)", u.Cost, u.CostEstimated)
		}
	})

	t.Run("openai_prompt_completion_details", func(t *testing.T) {
		raw := []byte(`{
			"model": "openai/gpt-5",
			"usage": {
				"prompt_tokens": 1200,
				"completion_tokens": 400,
				"total_tokens": 1600,
				"prompt_tokens_details": {"cached_tokens": 800},
				"completion_tokens_details": {"reasoning_tokens": 150}
			}
		}`)
		u, ok := ExtractTurnUsage(raw)
		if !ok {
			t.Fatal("expected OpenAI usage to be extracted")
		}
		if u.InputTokens != 1200 || u.OutputTokens != 400 || u.CacheReadTokens != 800 || u.ReasoningTokens != 150 || u.TotalTokens != 1600 {
			t.Fatalf("unexpected OpenAI usage: %+v", u)
		}
	})
}

func TestRunCollectorDeduplicatesTurnEndAndMeasuresLatency(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	collector := NewRunCollector(RunMetadata{
		RunID:      "run-101",
		AttemptID:  "attempt-1",
		ThreadID:   "thread-alpha",
		ProviderID: "anthropic",
		Model:      "anthropic/claude-sonnet-4-6",
	}, t0)

	collector.Observe(provider.Event{
		Sequence:   1,
		OccurredAt: t0.Add(50 * time.Millisecond),
		Type:       "agent_start",
		Data:       json.RawMessage(`{"type":"agent_start"}`),
	})
	// First token arrives at +320ms -> TTFTMs = 320.
	collector.Observe(provider.Event{
		Sequence:   2,
		OccurredAt: t0.Add(320 * time.Millisecond),
		Type:       "thinking_delta",
		Data:       json.RawMessage(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"analyzing"}}`),
	})
	collector.Observe(provider.Event{
		Sequence:   3,
		OccurredAt: t0.Add(600 * time.Millisecond),
		Type:       "text_delta",
		Delta:      "Checking files...",
		Data:       json.RawMessage(`{"type":"message_update","usage":{"input":500,"output":12,"cacheRead":1000,"cacheWrite":0,"totalTokens":1512,"cost":{"total":0.002}},"assistantMessageEvent":{"type":"text_delta","delta":"Checking files..."}}`),
	})

	turn1Msg := `{"type":"message_end","message":{"role":"assistant","responseId":"resp-1","stopReason":"toolUse","usage":{"input":500,"output":80,"cacheRead":1000,"cacheWrite":100,"totalTokens":1680,"cost":{"input":0.0015,"output":0.0012,"cacheRead":0.0003,"cacheWrite":0.000375,"total":0.003375}}}}`
	collector.Observe(provider.Event{
		Sequence:   4,
		OccurredAt: t0.Add(1100 * time.Millisecond),
		Type:       "message_end",
		Data:       json.RawMessage(turn1Msg),
	})
	// turn_end carries the exact same message; RunCollector must not double-count it.
	collector.Observe(provider.Event{
		Sequence:   5,
		OccurredAt: t0.Add(1110 * time.Millisecond),
		Type:       "turn_end",
		Data:       json.RawMessage(strings.Replace(turn1Msg, `"message_end"`, `"turn_end"`, 1)),
	})

	// Compaction usage also contributes to token/cost accounting without inflating assistant TurnCount.
	collector.Observe(provider.Event{
		Sequence:   6,
		OccurredAt: t0.Add(1800 * time.Millisecond),
		Type:       "compaction_end",
		Data:       json.RawMessage(`{"type":"compaction_end","reason":"threshold","result":{"tokensBefore":9000,"estimatedTokensAfter":2000,"usage":{"input":200,"output":100,"cacheRead":0,"cacheWrite":0,"totalTokens":300,"cost":{"total":0.0021}}}}`),
	})

	// Second assistant turn after tool execution.
	collector.Observe(provider.Event{
		Sequence:   7,
		OccurredAt: t0.Add(2000 * time.Millisecond),
		Type:       "turn_start",
		Data:       json.RawMessage(`{"type":"turn_start"}`),
	})
	collector.Observe(provider.Event{
		Sequence:   8,
		OccurredAt: t0.Add(2900 * time.Millisecond),
		Type:       "message_end",
		Data:       json.RawMessage(`{"type":"message_end","message":{"role":"assistant","responseId":"resp-2","stopReason":"stop","usage":{"input":300,"output":120,"cacheRead":1500,"cacheWrite":0,"totalTokens":1920,"cost":{"total":0.00315}}}}`),
	})

	metrics := collector.Finalize(t0.Add(3000 * time.Millisecond))
	if metrics.TTFTMs != 320 {
		t.Fatalf("TTFTMs = %d, want 320", metrics.TTFTMs)
	}
	if metrics.DurationMs != 3000 {
		t.Fatalf("DurationMs = %d, want 3000", metrics.DurationMs)
	}
	if metrics.TurnCount != 2 {
		t.Fatalf("TurnCount = %d, want 2", metrics.TurnCount)
	}
	if metrics.InputTokens != 1000 || metrics.OutputTokens != 300 || metrics.CacheReadTokens != 2500 || metrics.CacheWriteTokens != 100 || metrics.TotalTokens != 3900 {
		t.Fatalf("unexpected accumulated tokens: %+v", metrics)
	}
	wantCost := 0.003375 + 0.0021 + 0.00315
	if math.Abs(metrics.CostUSD-wantCost) > 1e-9 {
		t.Fatalf("CostUSD = %.8f, want %.8f", metrics.CostUSD, wantCost)
	}
}

func TestLedgerPersistenceMultiModelBreakdownAndConcurrency(t *testing.T) {
	dataDir := t.TempDir()
	ledger, err := NewLedger(dataDir)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}

	baseTime := time.Date(2026, 4, 1, 8, 0, 0, 0, time.UTC)

	records := []RunMetrics{
		{
			RunID:            "run-1",
			AttemptID:        "att-1",
			ThreadID:         "thread-1",
			ProviderID:       "anthropic",
			Model:            "anthropic/claude-opus-4-6",
			TurnCount:        2,
			InputTokens:      2000,
			OutputTokens:     800,
			CacheReadTokens:  6000,
			CacheWriteTokens: 500,
			Cost:             CostBreakdown{Input: 0.03, Output: 0.06, CacheRead: 0.009, CacheWrite: 0.009375, Total: 0.108375},
			TTFTMs:           450,
			DurationMs:       4200,
			StartedAt:        baseTime.Add(-2 * time.Hour),
			CompletedAt:      baseTime.Add(-2*time.Hour + 4200*time.Millisecond),
		},
		{
			// Continuation attempt on the same run-1 in thread-1.
			RunID:            "run-1",
			AttemptID:        "att-2",
			ThreadID:         "thread-1",
			ProviderID:       "anthropic",
			Model:            "anthropic/claude-opus-4-6",
			TurnCount:        1,
			InputTokens:      1000,
			OutputTokens:     400,
			CacheReadTokens:  7000,
			CacheWriteTokens: 0,
			Cost:             CostBreakdown{Input: 0.015, Output: 0.03, CacheRead: 0.0105, Total: 0.0555},
			TTFTMs:           350,
			DurationMs:       2800,
			StartedAt:        baseTime.Add(10 * time.Minute),
			CompletedAt:      baseTime.Add(10*time.Minute + 2800*time.Millisecond),
		},
		{
			RunID:            "run-2",
			AttemptID:        "att-1",
			ThreadID:         "thread-1",
			ProviderID:       "openai",
			Model:            "openai/gpt-5",
			TurnCount:        1,
			InputTokens:      1500,
			OutputTokens:     600,
			CacheReadTokens:  3000,
			CacheWriteTokens: 0,
			Cost:             CostBreakdown{Total: 0.00825},
			TTFTMs:           280,
			DurationMs:       1900,
			StartedAt:        baseTime.Add(20 * time.Minute),
			CompletedAt:      baseTime.Add(20*time.Minute + 1900*time.Millisecond),
		},
		{
			RunID:            "run-3",
			AttemptID:        "att-1",
			ThreadID:         "thread-2",
			ProviderID:       "deepseek",
			Model:            "deepseek/deepseek-chat",
			TurnCount:        3,
			InputTokens:      4000,
			OutputTokens:     1200,
			CacheReadTokens:  8000,
			CacheWriteTokens: 0,
			Cost:             CostBreakdown{Total: 0.00296},
			TTFTMs:           210,
			DurationMs:       3100,
			StartedAt:        baseTime.Add(30 * time.Minute),
			CompletedAt:      baseTime.Add(30*time.Minute + 3100*time.Millisecond),
		},
	}

	for _, r := range records {
		if err := ledger.Record(r); err != nil {
			t.Fatalf("Record(%s/%s): %v", r.RunID, r.AttemptID, err)
		}
	}

	// Verify persisted file exists at dataDir/usage-ledger.json and reloads cleanly.
	if _, err := os.Stat(filepath.Join(dataDir, "usage-ledger.json")); err != nil {
		t.Fatalf("persisted usage-ledger.json missing: %v", err)
	}
	reloaded, err := NewLedger(dataDir)
	if err != nil {
		t.Fatalf("reload NewLedger: %v", err)
	}

	thread1 := reloaded.ThreadSummary("thread-1")
	if thread1.RunCount != 2 || thread1.AttemptCount != 3 || thread1.TurnCount != 4 {
		t.Fatalf("unexpected thread-1 counts: %+v", thread1)
	}
	if thread1.InputTokens != 4500 || thread1.OutputTokens != 1800 || thread1.CacheReadTokens != 16000 || thread1.CacheHitTokens != 16000 {
		t.Fatalf("unexpected thread-1 tokens: %+v", thread1)
	}
	if thread1.AvgTTFTMs != (450+350+280)/3 {
		t.Fatalf("AvgTTFTMs = %d, want %d", thread1.AvgTTFTMs, (450+350+280)/3)
	}
	if thread1.CacheHitRate <= 0.70 || thread1.CacheHitRate >= 0.80 {
		t.Fatalf("unexpected CacheHitRate = %.4f", thread1.CacheHitRate)
	}

	// ModelBreakdown filtered by `since = baseTime` excludes the first historical attempt (-2h).
	recentModels := reloaded.ModelBreakdown(baseTime)
	if len(recentModels) != 3 {
		t.Fatalf("ModelBreakdown(since) len = %d, want 3: %+v", len(recentModels), recentModels)
	}
	if recentModels[0].Model != "anthropic/claude-opus-4-6" || math.Abs(recentModels[0].TotalCostUSD-0.0555) > 1e-9 {
		t.Fatalf("top model since baseTime = %+v", recentModels[0])
	}
	if recentModels[1].Model != "openai/gpt-5" || recentModels[2].Model != "deepseek/deepseek-chat" {
		t.Fatalf("unexpected model sort order: %+v", recentModels)
	}

	global := reloaded.GlobalTotals()
	if global.RunCount != 3 || global.AttemptCount != 4 {
		t.Fatalf("GlobalTotals counts = %+v", global)
	}
	wantGlobalCost := 0.108375 + 0.0555 + 0.00825 + 0.00296
	if math.Abs(global.TotalCostUSD-wantGlobalCost) > 1e-9 {
		t.Fatalf("GlobalTotals cost = %.8f, want %.8f", global.TotalCostUSD, wantGlobalCost)
	}

	// Concurrent read/write stress check under -race.
	memLedger, err := NewLedger("")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = memLedger.Record(RunMetrics{
					RunID:        fmt.Sprintf("run-%d-%d", g, i),
					ThreadID:     fmt.Sprintf("thread-%d", g%3),
					ProviderID:   "openai",
					Model:        "openai/gpt-5-mini",
					InputTokens:  100,
					OutputTokens: 50,
					TTFTMs:       120,
					DurationMs:   500,
				})
				_ = memLedger.ThreadSummary("thread-0")
				_ = memLedger.ModelBreakdown(time.Time{})
				_ = memLedger.GlobalTotals()
			}
		}()
	}
	wg.Wait()
	if got := memLedger.GlobalTotals().RunCount; got != 200 {
		t.Fatalf("concurrent RunCount = %d, want 200", got)
	}
}

func TestPhaseTrackerNestingCompactionRetryAndApproval(t *testing.T) {
	tracker := NewPhaseTracker()
	if snap := tracker.Snapshot(); snap.Phase != PhaseStarting {
		t.Fatalf("initial phase = %s, want %s", snap.Phase, PhaseStarting)
	}

	// 1. Agent starts -> Thinking -> Streaming.
	snap := tracker.Apply(provider.Event{Sequence: 1, Type: "agent_start", Data: json.RawMessage(`{"type":"agent_start"}`)})
	if snap.Phase != PhaseThinking {
		t.Fatalf("after agent_start = %s, want %s", snap.Phase, PhaseThinking)
	}
	snap = tracker.Apply(provider.Event{Sequence: 2, Type: "text_delta", Delta: "Let me inspect the repo."})
	if snap.Phase != PhaseStreaming {
		t.Fatalf("after text_delta = %s, want %s", snap.Phase, PhaseStreaming)
	}

	// 2. Nested tool execution + non-blocking notify + blocking approval dialog + restoration.
	snap = tracker.Apply(provider.Event{
		Sequence: 3,
		Type:     "tool_execution_start",
		Data:     json.RawMessage(`{"type":"tool_execution_start","toolCallId":"call-read","toolName":"read","args":{"path":"internal/core/worker.go"}}`),
	})
	if snap.Phase != PhaseExecutingTool || snap.ToolName != "read" || snap.ToolTarget != "internal/core/worker.go" {
		t.Fatalf("unexpected tool phase: %+v", snap)
	}

	snap = tracker.Apply(provider.Event{
		Sequence: 4,
		Type:     "tool_execution_start",
		Data:     json.RawMessage(`{"type":"tool_execution_start","toolCallId":"call-bash","toolName":"bash","args":{"command":"curl -H 'Authorization: Bearer supersecret123' https://example.com\necho done"}}`),
	})
	if snap.Phase != PhaseExecutingTool || snap.ToolName != "bash" || len(snap.ActiveTools) != 2 {
		t.Fatalf("expected nested bash tool on top: %+v", snap)
	}
	if strings.Contains(snap.ToolTarget, "supersecret123") || !strings.Contains(snap.ToolTarget, "[REDACTED]") {
		t.Fatalf("expected bearer token redaction in ToolTarget, got %q", snap.ToolTarget)
	}

	// Non-blocking notify must NOT switch to PhaseWaitingApproval.
	snap = tracker.Apply(provider.Event{
		Sequence: 5,
		Type:     "extension_ui_request",
		Data:     json.RawMessage(`{"type":"extension_ui_request","id":"ready","method":"notify","message":"vantry_approval_ready"}`),
	})
	if snap.Phase != PhaseExecutingTool {
		t.Fatalf("notify UI event should stay in PhaseExecutingTool, got %s", snap.Phase)
	}

	// Blocking confirm dialog enters PhaseWaitingApproval while keeping tool context.
	snap = tracker.Apply(provider.Event{
		Sequence: 6,
		Type:     "extension_ui_request",
		Data:     json.RawMessage(`{"type":"extension_ui_request","id":"pi:dialog/1","method":"confirm","title":"Allow bash command?"}`),
	})
	if snap.Phase != PhaseWaitingApproval || snap.Approval == nil || snap.Approval.Title != "Allow bash command?" {
		t.Fatalf("expected PhaseWaitingApproval, got %+v", snap)
	}

	// Core/worker control_result resolves the dialog -> restores PhaseExecutingTool (bash).
	snap = tracker.Apply(provider.Event{
		Sequence: 7,
		Type:     "control_result",
		Data:     json.RawMessage(`{"id":"attempt-1:pi:dialog/1","status":"delivered"}`),
	})
	if snap.Phase != PhaseExecutingTool || snap.ToolName != "bash" {
		t.Fatalf("expected restoration to bash PhaseExecutingTool after approval, got %+v", snap)
	}

	// Bash finishes -> restores outer active tool `read`.
	snap = tracker.Apply(provider.Event{
		Sequence: 8,
		Type:     "tool_execution_end",
		Data:     json.RawMessage(`{"type":"tool_execution_end","toolCallId":"call-bash","toolName":"bash","isError":false}`),
	})
	if snap.Phase != PhaseExecutingTool || snap.ToolName != "read" {
		t.Fatalf("expected restoration to outer read tool, got %+v", snap)
	}

	// Read finishes -> all tools done -> PhaseThinking.
	snap = tracker.Apply(provider.Event{
		Sequence: 9,
		Type:     "tool_execution_end",
		Data:     json.RawMessage(`{"type":"tool_execution_end","toolCallId":"call-read","toolName":"read","isError":false}`),
	})
	if snap.Phase != PhaseThinking {
		t.Fatalf("expected PhaseThinking after all tools finish, got %s", snap.Phase)
	}

	// 3. Context compaction with nested summarization retry -> restores PhaseCompacting -> PhaseThinking.
	snap = tracker.Apply(provider.Event{
		Sequence: 10,
		Type:     "compaction_start",
		Data:     json.RawMessage(`{"type":"compaction_start","reason":"threshold"}`),
	})
	if snap.Phase != PhaseCompacting || snap.Compaction == nil || snap.Compaction.Reason != "threshold" {
		t.Fatalf("expected PhaseCompacting(threshold), got %+v", snap)
	}

	snap = tracker.Apply(provider.Event{
		Sequence: 11,
		Type:     "summarization_retry_scheduled",
		Data:     json.RawMessage(`{"type":"summarization_retry_scheduled","attempt":1,"maxAttempts":3,"delayMs":1500,"errorMessage":"connection reset"}`),
	})
	if snap.Phase != PhaseRetrying || snap.RetryAttempt != 1 || snap.RetryWaitSeconds != 2 {
		t.Fatalf("expected nested PhaseRetrying during compaction, got %+v", snap)
	}

	snap = tracker.Apply(provider.Event{
		Sequence: 12,
		Type:     "summarization_retry_finished",
		Data:     json.RawMessage(`{"type":"summarization_retry_finished"}`),
	})
	if snap.Phase != PhaseCompacting {
		t.Fatalf("expected return to PhaseCompacting after summarization retry finished, got %s", snap.Phase)
	}

	snap = tracker.Apply(provider.Event{
		Sequence: 13,
		Type:     "compaction_end",
		Data:     json.RawMessage(`{"type":"compaction_end","reason":"threshold","result":{"tokensBefore":140000,"estimatedTokensAfter":28000},"aborted":false,"willRetry":false}`),
	})
	if snap.Phase != PhaseThinking {
		t.Fatalf("expected PhaseThinking after compaction_end, got %s", snap.Phase)
	}

	// 4. Transient error rewritten by pi/worker.go as Type:"error" followed by auto_retry_start -> auto_retry_end(success:true).
	snap = tracker.Apply(provider.Event{
		Sequence: 14,
		Type:     "error",
		Data:     json.RawMessage(`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"529 overloaded"}}`),
	})
	if snap.Phase != PhaseFailed || snap.Error != "529 overloaded" {
		t.Fatalf("expected immediate transient PhaseFailed before retry signal, got %+v", snap)
	}

	snap = tracker.Apply(provider.Event{
		Sequence: 15,
		Type:     "agent_end",
		Data:     json.RawMessage(`{"type":"agent_end","willRetry":true}`),
	})
	if snap.Phase != PhaseRetrying {
		t.Fatalf("expected agent_end(willRetry:true) to enter PhaseRetrying, got %s", snap.Phase)
	}

	snap = tracker.Apply(provider.Event{
		Sequence: 16,
		Type:     "auto_retry_start",
		Data:     json.RawMessage(`{"type":"auto_retry_start","attempt":2,"maxAttempts":3,"delayMs":4000,"errorMessage":"529 overloaded"}`),
	})
	if snap.Phase != PhaseRetrying || snap.RetryAttempt != 2 || snap.RetryMaxAttempts != 3 || snap.RetryWaitSeconds != 4 || snap.RetryReason != "529 overloaded" {
		t.Fatalf("unexpected retry snapshot: %+v", snap)
	}

	snap = tracker.Apply(provider.Event{
		Sequence: 17,
		Type:     "auto_retry_end",
		Data:     json.RawMessage(`{"type":"auto_retry_end","success":true,"attempt":2}`),
	})
	if snap.Phase != PhaseThinking || snap.Error != "" {
		t.Fatalf("expected clean recovery to PhaseThinking after auto_retry_end(success:true), got %+v", snap)
	}

	snap = tracker.Apply(provider.Event{
		Sequence: 18,
		Type:     "text_delta",
		Delta:    "All tasks delegated.",
	})
	if snap.Phase != PhaseStreaming {
		t.Fatalf("expected PhaseStreaming, got %s", snap.Phase)
	}

	// Park in waiting_children, then test final settlement in separate trackers for terminal states.
	snap = tracker.MarkWaitingChildren()
	if snap.Phase != PhaseWaitingChildren || !snap.Phase.Active() {
		t.Fatalf("expected active PhaseWaitingChildren, got %+v", snap)
	}
	snap = tracker.MarkCompleted()
	if snap.Phase != PhaseCompleted || !snap.Phase.Terminal() {
		t.Fatalf("expected terminal PhaseCompleted, got %+v", snap)
	}

	// Verify exhausted retry failure and aborted settlement.
	failedTracker := NewPhaseTracker()
	failedTracker.Apply(provider.Event{
		Sequence: 1,
		Type:     "auto_retry_start",
		Data:     json.RawMessage(`{"type":"auto_retry_start","attempt":3,"maxAttempts":3,"delayMs":8000,"errorMessage":"429 Too Many Requests"}`),
	})
	failedTracker.Apply(provider.Event{
		Sequence: 2,
		Type:     "error",
		Data:     json.RawMessage(`{"type":"auto_retry_end","success":false,"attempt":3,"finalError":"rate limit persisted after 3 retries"}`),
	})
	finalFail := failedTracker.Apply(provider.Event{
		Sequence: 3,
		Type:     "agent_settled",
		Data:     json.RawMessage(`{"type":"agent_settled","aborted":false}`),
	})
	if finalFail.Phase != PhaseFailed || finalFail.Error != "rate limit persisted after 3 retries" {
		t.Fatalf("expected final PhaseFailed, got %+v", finalFail)
	}

	cancelTracker := NewPhaseTracker()
	cancelSnap := cancelTracker.Apply(provider.Event{
		Sequence: 1,
		Type:     "agent_settled",
		Data:     json.RawMessage(`{"type":"agent_settled","aborted":true}`),
	})
	if cancelSnap.Phase != PhaseCancelled || !cancelSnap.Phase.Terminal() {
		t.Fatalf("expected PhaseCancelled, got %+v", cancelSnap)
	}
}

func TestRateLimitClassificationBackoffAndFallbackAdvisor(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	t.Run("classify_categories_and_retry_after", func(t *testing.T) {
		// 1. 429 RateLimited with HTTP-date Retry-After header.
		httpDate := now.Add(25 * time.Second).Format("Mon, 02 Jan 2006 15:04:05 GMT")
		d1 := ClassifyErrorWithHeaders(
			`HTTP 429 Too Many Requests: Rate limit reached for requests per minute`,
			map[string]string{"Retry-After": httpDate},
			now,
		)
		if d1.Kind != RateLimited || !d1.Retryable || d1.RetryAfter != 25*time.Second {
			t.Fatalf("unexpected 429 diagnosis: %+v", d1)
		}

		// 2. OpenAI inline text wait ("Please try again in 6m12s.") -> long wait triggers fallback hint.
		d2 := ClassifyErrorWithHeaders(
			`429 {"error":{"code":"rate_limit_exceeded","message":"Rate limit reached for gpt-5. Please try again in 18s."}}`,
			nil,
			now,
		)
		if d2.Kind != RateLimited || d2.RetryAfter != 18*time.Second {
			t.Fatalf("unexpected inline retry wait diagnosis: %+v", d2)
		}

		// 3. HTTP 429 that is actually QuotaExhausted / credits_required / insufficient_quota!
		for _, quotaErr := range []string{
			`429 {"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details."}}`,
			`{"status":402,"error":{"code":"credits_required","message":"Insufficient credits to run this model"}}`,
			`HTTP 400: {"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing."}}`,
		} {
			dq := ClassifyErrorWithHeaders(quotaErr, nil, now)
			if dq.Kind != QuotaExhausted || dq.Retryable || !dq.ShouldFallback || dq.Action != ActionSwitchFallback {
				t.Fatalf("expected non-retryable QuotaExhausted with fallback for %q, got %+v", quotaErr, dq)
			}
		}

		// 4. Gemini transient per-minute quota metric must be RateLimited, NOT permanent QuotaExhausted.
		dGemini := ClassifyErrorWithHeaders(
			`429 Quota exceeded for quota metric 'Generate Content API requests per minute' and limit 'RequestsPerMinutePerProject'. retryDelay: "12s"`,
			nil,
			now,
		)
		if dGemini.Kind != RateLimited || !dGemini.Retryable {
			t.Fatalf("expected per-minute quota metric to classify as RateLimited, got %+v", dGemini)
		}

		// 5. Overloaded (529 / 503).
		dOver := ClassifyError(`529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		if dOver.Kind != Overloaded || !dOver.Retryable {
			t.Fatalf("expected Overloaded, got %+v", dOver)
		}

		// 6. ContextOverflow.
		dCtx := ClassifyError(`400 {"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 200000 tokens, however your prompt is too long (215000 tokens)."}}`)
		if dCtx.Kind != ContextOverflow || dCtx.Retryable || dCtx.Action != ActionCompactContext {
			t.Fatalf("expected ContextOverflow, got %+v", dCtx)
		}

		// 7. Ordinary permanent error (ensure numbers like "503 files" do not false-trigger HTTP 503).
		dPerm := ClassifyError(`validation failed after scanning 503 files: invalid tool schema`)
		if dPerm.Kind != PermanentError || dPerm.Retryable || dPerm.ShouldFallback {
			t.Fatalf("expected PermanentError without false 503 match, got %+v", dPerm)
		}
	})

	t.Run("next_backoff_exponential_and_jitter_cap", func(t *testing.T) {
		cfg := BackoffConfig{
			BaseDelay:     2 * time.Second,
			MaxDelay:      60 * time.Second,
			MaxJitter:     2 * time.Second,
			MaxRetryAfter: 5 * time.Minute,
			Rand:          func() float64 { return 0.5 },
		}
		b1 := cfg.Compute(1, 0) // 2s + 0.5 * 500ms = 2.25s
		b2 := cfg.Compute(2, 0) // 4s + 0.5 * 1s = 4.5s
		b3 := cfg.Compute(3, 0) // 8s + 0.5 * 2s = 9s
		b10 := cfg.Compute(10, 0)
		if b1 != 2250*time.Millisecond || b2 != 4500*time.Millisecond || b3 != 9*time.Second {
			t.Fatalf("unexpected backoff progression: b1=%v b2=%v b3=%v", b1, b2, b3)
		}
		if b10 > 60*time.Second || b10 < 58*time.Second {
			t.Fatalf("expected ceiling backoff within [58s, 60s], got %v", b10)
		}

		// Respect explicit server Retry-After floor + bounded jitter.
		withRetryAfter := cfg.Compute(1, 15*time.Second)
		if withRetryAfter < 15*time.Second || withRetryAfter > 17*time.Second {
			t.Fatalf("expected Retry-After floor 15s + <=2s jitter, got %v", withRetryAfter)
		}

		// Smoke check package-level NextBackoff across extreme attempts.
		for attempt := 0; attempt <= 50; attempt++ {
			got := NextBackoff(attempt, 0)
			if got < DefaultBaseBackoff || got > DefaultMaxBackoff {
				t.Fatalf("NextBackoff(%d, 0) = %v out of [%v, %v]", attempt, got, DefaultBaseBackoff, DefaultMaxBackoff)
			}
		}
	})

	t.Run("fallback_advisor_markdown_catalog_and_no_blind_paid_retry", func(t *testing.T) {
		mappingMarkdown := `
# Quota Fallback Mapping
| 主模型 | 备用模型 |
| --- | --- |
| anthropic/claude-opus-4-6 | openai/gpt-5, google/gemini-2.5-pro |
| anthropic/claude-sonnet-4-6 | openai/gpt-4o, deepseek/deepseek-chat |
`
		catalog := []api.ModelInfo{
			{ID: "claude-opus-4-6", Provider: "anthropic", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 200000},
			{ID: "claude-sonnet-4-6", Provider: "anthropic", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 200000},
			{ID: "gpt-5", Provider: "openai", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 400000},
			{ID: "gemini-2.5-pro", Provider: "google", Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 1000000},
			{ID: "gpt-5-mini", Provider: "openai", Reasoning: true, Input: []string{"text"}, ContextWindow: 200000},
		}

		advisor := NewFallbackAdvisor(
			WithCatalog(catalog),
			WithMaxRetries(2),
			WithBackoff(BackoffConfig{Rand: func() float64 { return 0 }}),
		)
		if err := advisor.LoadMappingText(mappingMarkdown); err != nil {
			t.Fatalf("LoadMappingText: %v", err)
		}

		// Case 1: credits_required on Opus -> Attempt 1 MUST NOT retry; immediately switch to openai/gpt-5!
		dec1 := advisor.Advise(RecoveryQuery{
			PrimaryModel: "anthropic/claude-opus-4-6",
			Attempt:      1,
			ErrorText:    `402 {"error":{"code":"credits_required","message":"Account credits exhausted"}}`,
			Now:          now,
		})
		if dec1.Action != ActionSwitchFallback || dec1.Wait != 0 || dec1.FallbackModel != "openai/gpt-5" {
			t.Fatalf("expected immediate switch to openai/gpt-5 without wait, got %+v", dec1)
		}

		// Case 2: Suppose openai/gpt-5 also hits insufficient_quota -> next recommendation skips both cooled-down models and picks google/gemini-2.5-pro!
		dec2 := advisor.Advise(RecoveryQuery{
			PrimaryModel: "openai/gpt-5",
			Attempt:      1,
			ErrorText:    `429 {"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`,
			TriedModels:  []string{"anthropic/claude-opus-4-6"},
			Now:          now.Add(time.Second),
		})
		if dec2.Action != ActionSwitchFallback || dec2.FallbackModel != "google/gemini-2.5-pro" {
			t.Fatalf("expected peer-tier cross-provider fallback google/gemini-2.5-pro, got %+v", dec2)
		}

		// Case 3: Transient 429 on gemini-2.5-pro retries up to MaxRetries=2, then switches or aborts when all flagships are exhausted.
		dec3Retry := advisor.Advise(RecoveryQuery{
			PrimaryModel: "google/gemini-2.5-pro",
			Attempt:      1,
			ErrorText:    `429 Too Many Requests: Retry-After: 5s`,
			Now:          now.Add(2 * time.Second),
		})
		if dec3Retry.Action != ActionRetryBackoff || dec3Retry.Wait != 5*time.Second {
			t.Fatalf("expected 5s retry backoff on attempt 1, got %+v", dec3Retry)
		}

		// Attempt 3 (> MaxRetries=2) escalates to remaining available catalog model rather than looping forever.
		dec3Escalate := advisor.Advise(RecoveryQuery{
			PrimaryModel: "google/gemini-2.5-pro",
			Attempt:      3,
			ErrorText:    `429 Too Many Requests`,
			TriedModels:  []string{"anthropic/claude-opus-4-6", "openai/gpt-5"},
			Now:          now.Add(15 * time.Second),
		})
		if dec3Escalate.Action != ActionSwitchFallback || dec3Escalate.FallbackModel == "" {
			t.Fatalf("expected fallback escalation after exceeding retries, got %+v", dec3Escalate)
		}
	})
}

func TestDeriveTitleAndAsyncGenerator(t *testing.T) {
	t.Run("mixed_zh_en_with_code_block_long_path_and_pleasantries", func(t *testing.T) {
		userMsg := "你好！麻烦请帮我看一下 `/Users/demo/Develop/Vantry/internal/core/worker.go` 里的 **SQLite 事务锁竞争超时**问题，并发压测时偶发报错：\n```go\nfunc (c *Core) settleEffect(eff effect, err error) error {\n    return c.write(ctx, func(t *txn) error { return nil })\n}\n```\n谢谢！"
		title := DeriveTitle(userMsg, "")

		if !utf8.ValidString(title) {
			t.Fatalf("derived title is not valid UTF-8: %q", title)
		}
		runes := utf8.RuneCountInString(title)
		if runes < 18 || runes > 28 {
			t.Fatalf("derived title %q length = %d runes, want within [18, 28]", title, runes)
		}
		if strings.Contains(title, "你好") || strings.Contains(title, "帮我") || strings.Contains(title, "/Users/") || strings.Contains(title, "func") || strings.Contains(title, "`") || strings.Contains(title, "**") {
			t.Fatalf("derived title leaked pleasantries, long paths, markdown, or code: %q", title)
		}
		if !strings.Contains(title, "worker.go") || !strings.Contains(title, "SQLite") {
			t.Fatalf("derived title lost key technical anchors: %q", title)
		}
	})

	t.Run("english_word_boundary_safety", func(t *testing.T) {
		userMsg := "Hi there, could you please help me implement exponential backoff recovery for transient provider failures in the telemetry package?"
		title := DeriveTitle(userMsg, "")
		runes := utf8.RuneCountInString(title)
		if runes < 18 || runes > 28 {
			t.Fatalf("english title %q length = %d runes, want [18, 28]", title, runes)
		}
		// Must not cut mid-word ("recovery" or "backoff" must remain whole words).
		if strings.HasSuffix(title, "recov") || strings.HasSuffix(title, "backo") || strings.Contains(title, "please") {
			t.Fatalf("english title broke word boundary or kept pleasantry: %q", title)
		}
	})

	t.Run("code_only_prompt_falls_back_to_assistant_reply", func(t *testing.T) {
		userMsg := "```\nError: SQLITE_BUSY: database is locked\n    at Database.exec (/app/db.js:42:9)\n```"
		assistantReply := "好的，这个报错是因为 SQLite 并发写入未开启 WAL 模式与忙碌等待超时，建议在连接初始化时配置 PRAGMA。"
		title := DeriveTitle(userMsg, assistantReply)
		runes := utf8.RuneCountInString(title)
		if runes < 18 || runes > 28 {
			t.Fatalf("fallback title %q length = %d runes, want [18, 28]", title, runes)
		}
		if strings.Contains(title, "好的") || !strings.Contains(title, "SQLite") {
			t.Fatalf("unexpected assistant-fallback title: %q", title)
		}
	})

	t.Run("async_summarizer_refinement_and_fallback", func(t *testing.T) {
		var called atomic.Int32
		summarizer := func(ctx context.Context, prompt string) (string, error) {
			called.Add(1)
			if !strings.Contains(prompt, "User:") {
				return "", errors.New("malformed prompt")
			}
			return "标题：「修复 worker.go 的 SQLite 锁竞争问题」\n附加说明", nil
		}

		gen := NewTitleGenerator(summarizer, WithSummarizerTimeout(2*time.Second))
		var (
			mu       sync.Mutex
			asyncGot string
		)
		immediate := gen.GenerateAsync(
			context.Background(),
			"请帮我排查 /a/b/c/worker.go 报错",
			"好的，我来排查 SQLite 锁竞争。",
			func(title string) {
				mu.Lock()
				asyncGot = title
				mu.Unlock()
			},
		)
		if immediate == "" {
			t.Fatal("GenerateAsync must return immediate heuristic title synchronously")
		}
		gen.Wait()

		mu.Lock()
		got := asyncGot
		mu.Unlock()
		if called.Load() != 1 {
			t.Fatalf("summarizer call count = %d, want 1", called.Load())
		}
		if got != "修复 worker.go 的 SQLite 锁竞争问题" {
			t.Fatalf("async refined title = %q, want %q", got, "修复 worker.go 的 SQLite 锁竞争问题")
		}

		// Failing summarizer gracefully falls back to heuristic title without callback noise.
		failingGen := NewTitleGenerator(func(ctx context.Context, prompt string) (string, error) {
			return "", context.DeadlineExceeded
		})
		fallbackTitle, usedModel, err := failingGen.Regenerate(context.Background(), "请帮我实现 429 自动回退顾问", "")
		if err == nil || usedModel || fallbackTitle != "实现 429 自动回退顾问" {
			t.Fatalf("unexpected failing Regenerate result: title=%q usedModel=%v err=%v", fallbackTitle, usedModel, err)
		}
	})

	t.Run("xml_injection_windows_path_github_url_and_empty_inputs", func(t *testing.T) {
		msg := "<system-reminder>internal hook</system-reminder>\nAct as the implementation sub-agent for this task.\n请帮我修复 https://github.com/muq9738/Vantry/issues/42 中 `C:\\Users\\dev\\Vantry\\cmd\\vantry\\main.go` 启动崩溃的问题"
		title := DeriveTitle(msg, "")
		if strings.Contains(title, "system-reminder") || strings.Contains(title, "Act as") || strings.Contains(title, "C:\\") {
			t.Fatalf("expected noise/path stripping, got %q", title)
		}
		if !strings.Contains(title, "#42") || !strings.Contains(title, "main.go") {
			t.Fatalf("expected condensed #42 and main.go in %q", title)
		}

		// Code-only without assistant reply falls back to first meaningful code line.
		codeOnly := DeriveTitle("```go\npanic(\"database schema mismatch in store.go\")\n```", "")
		if !strings.Contains(codeOnly, "panic") {
			t.Fatalf("expected code-block fallback, got %q", codeOnly)
		}

		if empty := DeriveTitle("   ", ""); empty != DefaultThreadTitle {
			t.Fatalf("empty DeriveTitle = %q, want %q", empty, DefaultThreadTitle)
		}
	})
}

func TestEdgeCasesCoverage(t *testing.T) {
	t.Run("ledger_idempotent_upsert_get_run_and_interrupted_collector", func(t *testing.T) {
		t0 := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
		col := NewRunCollector(RunMetadata{RunID: "run-int", AttemptID: "att-1", ThreadID: "th-int"}, t0)
		col.Observe(provider.Event{
			Sequence:   1,
			OccurredAt: t0.Add(150 * time.Millisecond),
			Data:       json.RawMessage(`{"type":"message_update","usage":{"input":300,"output":45,"cacheRead":0,"cacheWrite":0,"totalTokens":345,"cost":{"total":0.0012}},"assistantMessageEvent":{"type":"text_delta","delta":"partial"}}`),
		})
		liveSnap := col.Metrics(t0.Add(400 * time.Millisecond))
		if liveSnap.TotalTokens != 345 || liveSnap.ProviderID != "" {
			t.Fatalf("unexpected live snapshot: %+v", liveSnap)
		}
		final := col.Finalize(t0.Add(800 * time.Millisecond))
		if final.TTFT() != 150*time.Millisecond || final.Duration() != 800*time.Millisecond || final.TotalTokens != 345 {
			t.Fatalf("unexpected interrupted Finalize metrics: %+v", final)
		}

		dir := t.TempDir()
		ledger, err := NewLedger(dir)
		if err != nil {
			t.Fatal(err)
		}
		if ledger.Path() == "" {
			t.Fatal("expected non-empty ledger path")
		}
		if err := ledger.Record(final); err != nil {
			t.Fatal(err)
		}
		// Idempotent update of same RunID+AttemptID must overwrite rather than duplicate.
		final.OutputTokens = 90
		final.TotalTokens = 390
		if err := ledger.Record(final); err != nil {
			t.Fatal(err)
		}
		if err := ledger.Save(); err != nil {
			t.Fatal(err)
		}
		if len(ledger.Records()) != 1 {
			t.Fatalf("Records len = %d, want 1", len(ledger.Records()))
		}
		gotRun, ok := ledger.GetRun("run-int")
		if !ok || gotRun.TotalTokens != 390 {
			t.Fatalf("GetRun = %+v, ok=%v", gotRun, ok)
		}
	})

	t.Run("phase_tracker_apply_json_dedup_and_manual_controls", func(t *testing.T) {
		tr := NewPhaseTracker()
		if _, err := tr.ApplyJSON([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"hmm"}}`)); err != nil {
			t.Fatal(err)
		}
		if tr.Snapshot().Phase != PhaseThinking {
			t.Fatalf("phase = %s, want thinking", tr.Snapshot().Phase)
		}

		// Tool update + ffgrep target + auto-clearing dialog on tool_execution_end.
		_, _ = tr.ApplyJSON([]byte(`{"type":"tool_execution_start","toolCallId":"grep-1","toolName":"ffgrep","args":{"pattern":"RunWorker","path":"internal/"}}`))
		if s := tr.Snapshot(); s.Phase != PhaseExecutingTool || s.ToolTarget != "RunWorker in internal/" {
			t.Fatalf("unexpected ffgrep snapshot: %+v", s)
		}
		_, _ = tr.ApplyJSON([]byte(`{"type":"tool_execution_update","toolCallId":"grep-1","toolName":"ffgrep","args":{"pattern":"settleEffect","path":"internal/core/"}}`))
		if s := tr.Snapshot(); s.ToolTarget != "settleEffect in internal/core/" {
			t.Fatalf("expected updated ToolTarget, got %q", s.ToolTarget)
		}
		_, _ = tr.ApplyJSON([]byte(`{"type":"extension_ui_request","id":"dlg-9","method":"select","params":{"title":"Choose action"}}`))
		if tr.Snapshot().Phase != PhaseWaitingApproval {
			t.Fatal("expected PhaseWaitingApproval")
		}
		// Explicit ResolveApproval restores PhaseExecutingTool.
		if s := tr.ResolveApproval("dlg-9"); s.Phase != PhaseExecutingTool {
			t.Fatalf("expected PhaseExecutingTool after ResolveApproval, got %s", s.Phase)
		}
		// Duplicate positive sequence is ignored.
		tr.Apply(provider.Event{Sequence: 10, Type: "tool_execution_end", Data: json.RawMessage(`{"toolCallId":"grep-1"}`)})
		dup := tr.Apply(provider.Event{Sequence: 10, Type: "text_delta", Delta: "stale"})
		if dup.Phase != PhaseThinking {
			t.Fatalf("duplicate sequence altered phase to %s", dup.Phase)
		}

		if s := tr.MarkFailed("fatal crash"); s.Phase != PhaseFailed || s.Error != "fatal crash" {
			t.Fatalf("MarkFailed = %+v", s)
		}
		if s := NewPhaseTracker().MarkCancelled(); s.Phase != PhaseCancelled {
			t.Fatalf("MarkCancelled = %+v", s)
		}
	})

	t.Run("json_mapping_file_chinese_retry_and_cooldown_clear", func(t *testing.T) {
		dir := t.TempDir()
		mapFile := filepath.Join(dir, "fallbacks.json")
		content := `{"routes": {"anthropic/*": ["openai/gpt-5-mini", "deepseek/deepseek-chat"]}}`
		if err := os.WriteFile(mapFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		adv := NewFallbackAdvisor(WithCooldowns(30*time.Second, 10*time.Minute))
		if err := adv.LoadMappingFile(mapFile); err != nil {
			t.Fatalf("LoadMappingFile: %v", err)
		}

		diag := ClassifyErrorWithHeaders(
			"请求过于频繁，请在 12 秒后重试",
			map[string]string{"X-RateLimit-Reset-Requests": "15s"},
			time.Now().UTC(),
		)
		if diag.Kind != RateLimited || diag.RetryAfter != 15*time.Second {
			t.Fatalf("unexpected Chinese/header rate-limit diagnosis: %+v", diag)
		}

		now := time.Now().UTC()
		adv.MarkUnavailable("openai/gpt-5-mini", diag, now)
		best, all := adv.Recommend("anthropic/claude-haiku-4-5", QuotaExhausted, nil, now)
		if best != "deepseek/deepseek-chat" || len(all) != 1 {
			t.Fatalf("expected cooled-down model skipped, got best=%q all=%v", best, all)
		}
		adv.ClearCooldown("openai/gpt-5-mini")
		bestAfter, _ := adv.Recommend("anthropic/claude-haiku-4-5", QuotaExhausted, nil, now)
		if bestAfter != "openai/gpt-5-mini" {
			t.Fatalf("expected openai/gpt-5-mini restored after ClearCooldown, got %q", bestAfter)
		}
	})
}

func TestLastContextTokensTracksLatestTurnAndPersistsInLedger(t *testing.T) {
	t0 := time.Now().UTC()
	collector := NewRunCollector(RunMetadata{
		RunID:      "run-ctx-1",
		AttemptID:  "att-1",
		ThreadID:   "th-1",
		ProviderID: "pi",
		Model:      "magpie/deepseek-v4",
	}, t0)

	collector.Observe(provider.Event{
		Sequence:   1,
		OccurredAt: t0.Add(10 * time.Millisecond),
		Type:       "message_end",
		Data:       json.RawMessage(`{"type":"message_end","message":{"role":"assistant","usage":{"input":1000,"output":200,"cacheRead":300,"totalTokens":1500}}}`),
	})
	snap1 := collector.Metrics(t0.Add(20 * time.Millisecond))
	if snap1.LastContextTokens != 1500 || snap1.TotalTokens != 1500 {
		t.Fatalf("after turn 1: %+v", snap1)
	}

	collector.Observe(provider.Event{
		Sequence:   2,
		OccurredAt: t0.Add(30 * time.Millisecond),
		Type:       "message_end",
		Data:       json.RawMessage(`{"type":"message_end","message":{"role":"assistant","usage":{"input":800,"output":150,"cacheRead":1200,"cacheWrite":50,"totalTokens":2200}}}`),
	})
	// Pending streaming update overrides last context until committed/cleared.
	collector.Observe(provider.Event{
		Sequence:   3,
		OccurredAt: t0.Add(40 * time.Millisecond),
		Type:       "message_update",
		Data:       json.RawMessage(`{"type":"message_update","usage":{"input":900,"output":250,"cacheRead":1200}}`),
	})
	live := collector.Metrics(t0.Add(45 * time.Millisecond))
	if live.LastContextTokens != 2350 {
		t.Fatalf("live LastContextTokens = %d, want 2350", live.LastContextTokens)
	}

	fin := collector.Finalize(t0.Add(50 * time.Millisecond))
	if fin.LastContextTokens != 2350 {
		t.Fatalf("finalized LastContextTokens = %d, want 2350", fin.LastContextTokens)
	}

	dir := t.TempDir()
	ledger, err := NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(fin); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.GetRun("run-ctx-1")
	if !ok || got.LastContextTokens != 2350 || got.TotalTokens != 6050 {
		t.Fatalf("reloaded GetRun = %+v, ok=%v", got, ok)
	}
}

func TestRunCollectorRecordsAutoRetryCountInLedger(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	col := NewRunCollector(RunMetadata{
		RunID:      "run-retry-1",
		AttemptID:  "att-retry-1",
		ThreadID:   "th-retry-1",
		ProviderID: "pi",
		Model:      "example-provider/claude-opus-5.5",
	}, t0)

	col.Observe(provider.Event{Type: "auto_retry_start", OccurredAt: t0.Add(time.Second), Data: json.RawMessage(`{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000}`)})
	col.Observe(provider.Event{Type: "auto_retry_start", OccurredAt: t0.Add(4 * time.Second), Data: json.RawMessage(`{"type":"auto_retry_start","attempt":2,"maxAttempts":3,"delayMs":4000}`)})
	col.Observe(provider.Event{Type: "auto_retry_end", OccurredAt: t0.Add(6 * time.Second), Data: json.RawMessage(`{"type":"auto_retry_end","success":true,"attempt":2}`)})
	col.Observe(provider.Event{Type: "message_end", OccurredAt: t0.Add(8 * time.Second), Data: json.RawMessage(`{"type":"message_end","message":{"role":"assistant","usage":{"input":578,"output":388,"cacheRead":151680,"totalTokens":152646}}}`)})

	fin := col.Finalize(t0.Add(9 * time.Second))
	if fin.RetryCount != 2 {
		t.Fatalf("RetryCount = %d, want 2", fin.RetryCount)
	}

	dir := t.TempDir()
	ledger, err := NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(fin); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.GetRun("run-retry-1")
	if !ok || got.RetryCount != 2 {
		t.Fatalf("reloaded GetRun = %+v, ok=%v, want RetryCount=2", got, ok)
	}
	if summary := reloaded.ThreadSummary("th-retry-1"); summary.RetryCount != 2 {
		t.Fatalf("ThreadSummary.RetryCount = %d, want 2", summary.RetryCount)
	}
}
