package desktop

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// TestEffortPickerShowsOnlySupportedLevelsAndStaysOpen verifies that:
// 1. Only thinking levels actually supported by the active model are rendered
//    (no dead "超高" button when a model does not support xhigh).
// 2. Clicking a supported level (including "超高") dispatches SetThreadModel
//    immediately without closing the model popover and without starting any
//    continuous animation frames.
func TestEffortPickerShowsOnlySupportedLevelsAndStaysOpen(t *testing.T) {
	w := nativeReviewWorkbench()
	base := w.defaults.Model
	w.threads[0].Model = base + ":low"
	w.acceptThinking(base, []string{"low", "medium", "high", "max"})

	w.widgets.modelPickerOpen = true
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Frame()

	// Model does not support xhigh: "超高" must not be shown.
	if _, ok := tt.AccessByRole(ui.RoleRadio, "超高"); ok {
		t.Fatal("unsupported xhigh level should not be rendered as a radio option")
	}

	// Now enable xhigh on the model and verify clicking it works and keeps
	// the popover open.
	w.acceptThinking(base, []string{"low", "medium", "high", "xhigh", "max"})
	tt.Frame()

	mustClick(t, tt, "超高")
	tt.Frame()

	if !w.widgets.modelPickerOpen {
		t.Fatal("picking effort inside the model popover closed modelPickerOpen")
	}
	commands := f_dispatched(w)
	if len(commands) != 1 || commands[0].Model != base+":xhigh" {
		t.Fatalf("dispatched = %+v, want SetThreadModel to %s:xhigh", commands, base)
	}
	if got := w.displayedEffortLevel(); got != "xhigh" {
		t.Fatalf("displayedEffortLevel = %q, want xhigh", got)
	}
}

// TestTokenDisplaySeparatesFreshFromCachedAndShowsRetries verifies that cached
// prompt reads are separated from new uncached tokens in the header and footer,
// and that Pi auto-retries are surfaced in the status footer and popover.
func TestTokenDisplaySeparatesFreshFromCachedAndShowsRetries(t *testing.T) {
	w := designReviewWorkbench()
	w.runs = []api.Run{{
		ID:       "r-cache",
		ThreadID: w.selected,
		Status:   api.RunCompleted,
		Model:    w.threads[0].Model,
		Telemetry: &api.RunTelemetry{
			TurnCount:         2,
			RetryCount:        2,
			InputTokens:       578,
			OutputTokens:      388,
			CacheReadTokens:   151680,
			TotalTokens:       152646,
			LastContextTokens: 152646,
			CostUSD:           0.009,
		},
	}}

	tt := ui.NewTester(w.view, 1440, 900)
	tt.Frame()

	joined := strings.Join(tt.Texts(), " | ")
	if !strings.Contains(joined, "新增 966") {
		t.Fatalf("expected fresh uncached token count '新增 966' in UI, got: %s", joined)
	}
	if !strings.Contains(joined, "缓存 152k (99%)") {
		t.Fatalf("expected separated cache read badge in footer, got: %s", joined)
	}
	if !strings.Contains(joined, "重试 2 次") {
		t.Fatalf("expected retry count '重试 2 次' in status footer, got: %s", joined)
	}

	mustClick(t, tt, "会话概览与令牌统计")
	tt.Frame()
	pop := strings.Join(tt.Texts(), " | ")
	if !strings.Contains(pop, "自动重试") {
		t.Fatalf("expected '自动重试' tile in stats popover, got: %s", pop)
	}
}
