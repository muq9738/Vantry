package workspace

import (
	"context"

	"github.com/muq9738/Vantry/pkg/api"
)

// Resolve is the MacBook query name for the same existing-workspace contract.
// Neither entry point prepares a new checkout or falls back to the main repo.
func Resolve(ctx context.Context, dataDir string, spec api.WorkspaceSpec, owner string) (string, error) {
	return Existing(ctx, dataDir, spec, owner)
}
