package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/internal/hostfs"
	"github.com/muq9738/Vantry/internal/hostprocess"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	SetupTimeout   = 2 * time.Minute
	MaxSetupOutput = 64 << 10
)

type SetupError struct {
	Err       error
	Output    string
	Truncated bool
}

func (e *SetupError) Error() string { return fmt.Sprintf("project setup: %v\n%s", e.Err, e.Output) }
func (e *SetupError) Unwrap() error { return e.Err }

type setupOutput struct {
	data      []byte
	truncated bool
}

func (b *setupOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := MaxSetupOutput - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}

type setupWorkspace struct {
	Owner     string            `json:"owner"`
	Spec      api.WorkspaceSpec `json:"spec"`
	Path      string            `json:"path"`
	CreatedAt time.Time         `json:"createdAt"`
}

type setupDone struct {
	ProjectID string         `json:"projectId"`
	Workspace setupWorkspace `json:"workspace"`
	DoneAt    time.Time      `json:"doneAt"`
}

// RunSetupScript runs initialization once for a successfully prepared managed
// worktree. Prepare (or HandoffToWorktree) must finish first. The marker lives
// beside prepared.json, outside the user's checkout. Failed scripts can retry.
// A caller must invoke this before starting the provider's directory writer.
func RunSetupScript(ctx context.Context, project Project, worktreePath string) error {
	ctx, cancel := context.WithTimeout(ctx, SetupTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if project.SetupScript == "" {
		return nil
	}
	if project.ID == "" {
		return errors.New("setup requires a registered project")
	}
	if err := validateEnv(project.Env); err != nil {
		return err
	}
	path, err := canonicalPath(worktreePath)
	if err != nil {
		return err
	}
	ownerDir := filepath.Dir(path)
	if filepath.Base(path) != "worktree" || filepath.Base(filepath.Dir(ownerDir)) != "workspaces" {
		return errors.New("setup requires a managed workspaces/<owner>/worktree directory")
	}
	// Cleanup also checks prepare.lock and the stable checkout inode. Hold
	// both across the command and marker write so it cannot remove a live setup.
	lock, err := setupLock(ctx, filepath.Join(ownerDir, "prepare.lock"))
	if err != nil {
		return err
	}
	defer workspace.Unlock(lock)
	data, err := os.ReadFile(filepath.Join(ownerDir, "prepared.json"))
	if err != nil {
		return fmt.Errorf("setup requires a prepared worktree: %w", err)
	}
	var record setupWorkspace
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	recordedPath, err := canonicalPath(record.Path)
	if err != nil {
		return err
	}
	if record.Owner != filepath.Base(ownerDir) || record.Spec.Kind != workspace.KindWorktree || recordedPath != path || record.CreatedAt.IsZero() {
		return errors.New("setup worktree ownership mismatch")
	}
	dataDir := filepath.Dir(filepath.Dir(ownerDir))
	if _, err := workspace.Existing(ctx, dataDir, record.Spec, record.Owner); err != nil {
		return err
	}
	projectRoot, err := canonicalPath(project.RootPath)
	if err != nil {
		return err
	}
	common, err := projectGit(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	projectCommon, err := projectGit(ctx, projectRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(common)) != strings.TrimSpace(string(projectCommon)) {
		return errors.New("setup worktree belongs to another project")
	}
	writer, err := workspace.LockDirectory(ctx, path)
	if err != nil {
		return err
	}
	defer workspace.Unlock(writer)
	marker := filepath.Join(ownerDir, "setup-done.json")
	if data, err := os.ReadFile(marker); err == nil {
		var done setupDone
		if err := json.Unmarshal(data, &done); err != nil {
			return fmt.Errorf("read setup marker: %w", err)
		}
		if done.ProjectID == project.ID && reflect.DeepEqual(done.Workspace, record) {
			return nil
		}
		// A different prepared generation is a newly created checkout. A
		// different project on this same generation is an ownership conflict.
		if reflect.DeepEqual(done.Workspace, record) {
			return errors.New("setup marker belongs to another project")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	command := hostprocess.NewContext(ctx, "sh", "-c", project.SetupScript)
	command.Dir = path
	command.Env = setupEnvironment(project.Env)
	defer command.Close()
	command.WaitDelay = 250 * time.Millisecond
	var output setupOutput
	// Sharing one writer makes os/exec serialize stdout/stderr copies.
	command.Stdout, command.Stderr = &output, &output
	err = command.Run()
	// Also reap background children whose output was redirected away from us.
	// Setup must finish before a provider can obtain the workspace lock.
	if command.Process != nil {
		_ = command.KillTree()
	}
	if err != nil || ctx.Err() != nil {
		return &SetupError{Err: errors.Join(ctx.Err(), err), Output: string(output.data), Truncated: output.truncated}
	}
	return atomicJSON(marker, setupDone{ProjectID: project.ID, Workspace: record, DoneAt: time.Now().UTC()})
}

// PrepareWorktree prepares an owned checkout and automatically initializes it
// before returning it to a provider. Repeated calls reuse the checkout and its
// successful setup marker. A setup failure returns the prepared spec for retry.
func (s *Store) PrepareWorktree(ctx context.Context, project Project, owner, branch, baseRef string) (api.WorkspaceSpec, error) {
	if baseRef == "" {
		baseRef = project.DefaultBaseBranch
	}
	spec, err := workspace.Prepare(ctx, s.dataDir, api.WorkspaceSpec{
		Kind: workspace.KindWorktree, Repository: project.RootPath, Branch: branch, BaseRef: baseRef,
	}, owner)
	if err != nil {
		return api.WorkspaceSpec{}, err
	}
	return spec, s.RunSetupScript(ctx, project, spec.Path)
}

func (s *Store) RunSetupScript(ctx context.Context, project Project, worktreePath string) error {
	path, err := canonicalPath(worktreePath)
	if err != nil {
		return err
	}
	if filepath.Dir(filepath.Dir(filepath.Dir(path))) != s.dataDir {
		return errors.New("setup worktree is outside this store's data directory")
	}
	return RunSetupScript(ctx, project, path)
}

func setupLock(ctx context.Context, path string) (*os.File, error) {
	file, err := hostfs.OpenNoFollow(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("setup lock is not a regular file")
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		err := hostfs.TryLock(file)
		if err == nil {
			return file, nil
		}
		if !hostfs.LockBusy(err) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func setupEnvironment(overrides map[string]string) []string {
	values := map[string]string{}
	for _, entry := range workspace.Environment() {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}
