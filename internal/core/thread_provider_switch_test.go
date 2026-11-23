package core

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

// A thread created by the old desktop "offline demo" setting in a Pi Core
// must be able to choose a real model; a demo-only Core still refuses.
func TestDemoThreadJoinsCoreExecutorWhenChoosingModel(t *testing.T) {
	for _, tc := range []struct {
		name, coreProvider, wantProvider string
		wantErr                          bool
	}{
		{"pi-core", "pi", "pi", false},
		{"demo-core", "controlled", "controlled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Open(context.Background(), Options{DBPath: filepath.Join(t.TempDir(), "core.db"), Provider: newRecordingProvider(), DefaultProviderID: tc.coreProvider, DefaultModel: "default-model"})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx := context.Background()
			if _, err := c.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, CommandID: "create", Kind: api.CreateThread, ThreadID: "demo", ProviderID: "controlled", Model: "default-model"}); err != nil {
				t.Fatal(err)
			}
			for i, kind := range []api.CommandKind{api.SetThreadModel, api.UpdateThread} {
				model := []string{"magpie/gpt-6-sol", "magpie/gpt-6-sol:high"}[i]
				_, err := c.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, CommandID: "model-" + string(kind), Kind: kind, ThreadID: "demo", Model: model})
				if (err != nil) != tc.wantErr {
					t.Fatalf("%s error = %v, want error %v", kind, err, tc.wantErr)
				}
			}
			snap, err := c.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := snap.Threads[0].ProviderID; got != tc.wantProvider {
				t.Fatalf("provider = %q, want %q", got, tc.wantProvider)
			}
		})
	}
}
