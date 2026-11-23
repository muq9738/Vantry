package core

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

type unavailableStopProvider struct {
	*durableProvider
	available atomic.Bool
}

func (p *unavailableStopProvider) Stop(ctx context.Context, request provider.Request) error {
	if !p.available.Load() {
		return errors.New("temporary worker cancellation delivery failure")
	}
	return p.durableProvider.Stop(ctx, request)
}

func TestRestartRetriesCancellationCommittedBeforeWorkerDelivery(t *testing.T) {
	p := &unavailableStopProvider{durableProvider: &durableProvider{recoverable: true}}
	p.runEvents = func(ctx context.Context, _ provider.Request, _ func(provider.Event) error) error {
		<-ctx.Done()
		return provider.ErrDetached
	}
	path := filepath.Join(t.TempDir(), "state.sqlite")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "root")
	receipt := mustSend(t, first, "send", threadID, "working")
	waitRunStatus(t, first, receipt.RunID, api.RunRunning)
	dispatchOrFail(t, first, api.Command{Version: 1, CommandID: "stop", Kind: api.StopRun, RunID: receipt.RunID})
	var status string
	if err := first.store.db.QueryRow(`SELECT status FROM effect_outbox WHERE kind = ? AND run_id = ?`,
		effectKindProviderStop, receipt.RunID).Scan(&status); err != nil || status != outboxPending {
		t.Fatalf("failed external stop lost its durable retry: status=%q err=%v", status, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	p.available.Store(true)
	second := openCoreAt(t, path, p)
	defer second.Close()
	waitFor(t, "cancelled worker stop intent delivered after restart", func() bool { return p.stopCount() == 1 })
	waitFor(t, "stop outbox settled", func() bool {
		err := second.store.db.QueryRow(`SELECT status FROM effect_outbox WHERE kind = ? AND run_id = ?`,
			effectKindProviderStop, receipt.RunID).Scan(&status)
		return err == nil && status == outboxSettled
	})
	if len(p.requests()) != 1 {
		t.Fatal("restart executed a cancelled run again")
	}
	if got := latestAttempt(t, second, receipt.RunID).Status; got != api.RunCancelled {
		t.Fatalf("cancelled run resurrected: %s", got)
	}
}
