package core

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func TestDelegateAndModelSwitchUseRequestedProvider(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	p := &routeProvider{fn: func(_ context.Context, req provider.Request, emit func(string) error) error {
		mu.Lock()
		seen = append(seen, req.ProviderID+" "+req.Model+" "+req.Message)
		mu.Unlock()
		return emit("ok")
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create")
	if _, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "model", Kind: api.SetThreadModel,
		ThreadID: threadID, ProviderID: "pi", Model: "openai/gpt-5",
	}); err != nil {
		t.Fatal(err)
	}
	sent := mustSend(t, c, "send", threadID, "parent")
	waitRunStatus(t, c, sent.RunID, api.RunCompleted)
	latest := latestAttempt(t, c, sent.RunID)
	if latest.ProviderID != "pi" || latest.Model != "openai/gpt-5" {
		t.Fatalf("run provider = %s %s", latest.ProviderID, latest.Model)
	}
	cmd := delegateCommand("child", sent.RunID, latest.AttemptID, "child work", "manual")
	cmd.ProviderID = "pi"
	cmd.Model = "anthropic/claude-sonnet-4"
	delegated := dispatchOrFail(t, c, cmd)
	waitRunStatus(t, c, delegated.RunID, api.RunCompleted)
	child := latestAttempt(t, c, delegated.RunID)
	if child.ProviderID != "pi" || child.Model != "anthropic/claude-sonnet-4" {
		t.Fatalf("delegated provider = %s %s", child.ProviderID, child.Model)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "pi openai/gpt-5 parent" || seen[1] != "pi anthropic/claude-sonnet-4 child work" {
		t.Fatalf("routes = %v", seen)
	}

	for _, retired := range []string{"agy", "qoder", "custom:review"} {
		_, err := c.Dispatch(context.Background(), api.Command{
			Version: api.ProtocolVersion, CommandID: "reject-" + retired, Kind: api.SetThreadModel,
			ThreadID: threadID, ProviderID: retired, Model: "default",
		})
		if err == nil || !strings.Contains(err.Error(), "unsupported provider") {
			t.Fatalf("expected unsupported provider error for %q, got %v", retired, err)
		}
	}

	// Historical retired ACP thread can still load and switch onto Pi.
	legacy, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "legacy-create", Kind: api.CreateThread,
		ThreadID: "th_legacy_acp", Title: "Legacy", ProviderID: "agy", Model: "gemini-4-hydrogen-medium",
	})
	if err != nil || !legacy.Accepted {
		t.Fatalf("legacy create = %+v, %v", legacy, err)
	}
	if _, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "legacy-switch", Kind: api.SetThreadModel,
		ThreadID: "th_legacy_acp", Model: "openai/gpt-5",
	}); err != nil {
		t.Fatalf("switch legacy thread onto Pi: %v", err)
	}
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var switched bool
	for _, th := range snap.Threads {
		if th.ID == "th_legacy_acp" {
			switched = th.ProviderID == "pi" && th.Model == "openai/gpt-5"
		}
	}
	if !switched {
		t.Fatal("retired ACP thread did not migrate onto Pi after model switch")
	}
}
