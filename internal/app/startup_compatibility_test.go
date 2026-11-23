package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestEnsureCoreDoesNotAttachAfterCompatibilityFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service api.Service
		want    error
	}{
		{"legacy without shutdown", &baseService{health: readyHealth()}, ErrLegacyCore},
		{"missing workbench without shutdown", &statusService{baseService: &baseService{health: readyHealth()}, status: compatibleStatus()}, ErrCoreCapabilitiesMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket, probe := startCapabilityServer(t, tc.service)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			client, err := EnsureCore(ctx, Config{DataDir: filepath.Dir(socket)}, false)
			if client != nil {
				_ = client.Close()
				t.Fatal("desktop attached to a Core after its capability check failed")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("EnsureCore error = %v, want %v", err, tc.want)
			}
			if _, err := probe.Health(ctx); err != nil {
				t.Fatalf("incompatible Core must remain available to its existing clients: %v", err)
			}
		})
	}
}
