package t3compat

import (
	"context"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestVantryClientUsesT3CoreSessions(t *testing.T) {
	server, service, httpServer := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	link, err := server.PairingLink(ctx, httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := PairRemote(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	client := local.DialAdapter(&RemoteAdapter{Remote: remote})
	defer client.Close()
	if _, err = client.Health(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := client.ServerStatus(ctx)
	if err != nil || !status.HasCapability(api.CapabilityThreadDetail) || status.HasCapability(api.CapabilityRemoteDesktopPermissions) {
		t.Fatal(status, err)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil || len(snapshot.Threads) != 1 {
		t.Fatal(snapshot, err)
	}
	sub, err := client.Subscribe(ctx, snapshot.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receipt, err := client.Dispatch(ctx, api.Command{Version: 1, Kind: api.SendMessage, CommandID: "adapter-send", ThreadID: "seed", Message: "Vantry to T3"})
	if err != nil || !receipt.Accepted {
		t.Fatal(receipt, err)
	}
	select {
	case err := <-sub.Errors:
		if api.CodeOf(err) != api.ResyncRequired {
			t.Fatal("adapter failed to request an authoritative refresh", err)
		}
	case <-ctx.Done():
		t.Fatal("T3 changes did not reach the Vantry client")
	}
	for {
		detail, err := client.ThreadDetail(ctx, api.ThreadDetailQuery{ThreadID: "seed"})
		if err != nil {
			t.Fatal(err)
		}
		if len(detail.Runs) == 1 && detail.Runs[0].Status.Terminal() {
			if detail.Runs[0].Output == "" {
				t.Fatal("reply text lost")
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("T3 reply did not complete")
		case <-time.After(10 * time.Millisecond):
		}
	}
	summary, err := client.ThreadSummaries(ctx)
	if err != nil || len(summary.Threads) != 1 || summary.Threads[0].RunCount != 1 {
		t.Fatal(summary, err)
	}
	if _, err = client.ListModels(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Dispatch(ctx, api.Command{Version: 1, CommandID: "adapter-create", Kind: api.CreateThread, Title: "From Vantry"}); err != nil {
		t.Fatal(err)
	}
	source, err := service.Snapshot(ctx)
	if err != nil || len(source.Threads) != 2 || len(source.Runs) != 1 {
		t.Fatal("adapter created a separate task store", source, err)
	}
}
