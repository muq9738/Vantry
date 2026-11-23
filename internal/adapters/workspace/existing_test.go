package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestExistingUsesPreparedWorktreeAndRejectsDifferentIntent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initRepository(t, repository)
	dataDir := filepath.Join(root, "data")
	spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: repository}
	ready, err := Prepare(ctx, dataDir, spec, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ready.Path, "dirty.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, request := range []api.WorkspaceSpec{spec, ready} {
		path, err := Existing(ctx, dataDir, request, "thread-1")
		if err != nil || !samePath(path, ready.Path) || samePath(path, repository) {
			t.Fatalf("existing = %q, %v; prepared = %q", path, err, ready.Path)
		}
	}
	for _, change := range []struct {
		name string
		edit func(*api.WorkspaceSpec)
	}{
		{"path", func(s *api.WorkspaceSpec) { s.Path = repository }},
		{"branch", func(s *api.WorkspaceSpec) { s.Branch = "main" }},
		{"base", func(s *api.WorkspaceSpec) { s.BaseRef = "main" }},
		{"repository", func(s *api.WorkspaceSpec) { s.Repository = root }},
		{"kind", func(s *api.WorkspaceSpec) { *s = api.WorkspaceSpec{Kind: KindDirectory, Path: repository} }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := spec
			change.edit(&changed)
			if path, err := Existing(ctx, dataDir, changed, "thread-1"); err == nil || path != "" {
				t.Fatalf("different intent accepted: %q, %v", path, err)
			}
		})
	}
}

func TestExistingRejectsPrimaryCheckoutAsAnIsolatedWorktree(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initRepository(t, repository)
	spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: repository, Path: repository, Branch: "main"}
	dataDir := filepath.Join(root, "data")
	if _, err := Prepare(context.Background(), dataDir, spec, "thread-1"); err == nil {
		t.Fatal("preparation accepted primary checkout as isolated")
	}
	// Construct a malformed historical record directly: current preparation
	// must already reject this request before a ready record can be written.
	record := prepared{Owner: "thread-1", Spec: spec, Path: spec.Path}
	if err := writeJSON(filepath.Join(dataDir, "workspaces", "thread-1", "prepared.json"), record); err != nil {
		t.Fatal(err)
	}
	if path, err := Existing(context.Background(), dataDir, spec, "thread-1"); err == nil || path != "" {
		t.Fatalf("primary checkout accepted as isolated: %q, %v", path, err)
	}
}

func TestExistingDirectoryAndCancelledRequest(t *testing.T) {
	root := t.TempDir()
	spec := api.WorkspaceSpec{Kind: KindDirectory, Path: root}
	dataDir := filepath.Join(root, "data")
	path, err := Existing(context.Background(), dataDir, spec, "thread-1")
	if err != nil || !samePath(path, root) {
		t.Fatalf("existing directory: %q, %v", path, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if path, err := Existing(ctx, dataDir, spec, "thread-1"); path != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled directory lookup: %q, %v", path, err)
	}
}

func TestExistingNeverCreatesOrFallsBackAfterMissingPreparation(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initRepository(t, repository)
	dataDir := filepath.Join(root, "data")
	spec := api.WorkspaceSpec{Kind: KindWorktree, Repository: repository}
	if path, err := Existing(context.Background(), dataDir, spec, "thread-1"); err == nil || path != "" {
		t.Fatalf("unprepared worktree fell back: %q, %v", path, err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("Existing created workspace state: %v", err)
	}
	ready, err := Prepare(context.Background(), dataDir, spec, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(ready.Path); err != nil {
		t.Fatal(err)
	}
	if path, err := Existing(context.Background(), dataDir, spec, "thread-1"); err == nil || path != "" {
		t.Fatalf("missing worktree fell back: %q, %v", path, err)
	}
}
