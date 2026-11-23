// Package projects stores registered projects independently of their checkouts.
package projects

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/internal/hostprocess"
)

var (
	ErrNotFound = errors.New("project not found")
	ErrConflict = errors.New("project already registered")
)

type Project struct {
	ID                 string            `json:"id"`
	Title              string            `json:"title"`
	RootPath           string            `json:"rootPath"`
	DefaultBaseBranch  string            `json:"defaultBaseBranch"`
	SetupScript        string            `json:"setupScript,omitempty"`
	DevCommand         string            `json:"devCommand,omitempty"`
	CustomInstructions string            `json:"customInstructions,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	CreatedAt          time.Time         `json:"createdAt"`
	UpdatedAt          time.Time         `json:"updatedAt"`
}

type CreateOptions struct {
	ID                 string
	Title              string
	DefaultBaseBranch  string
	SetupScript        string
	DevCommand         string
	CustomInstructions string
	Env                map[string]string
	InitGitIfMissing   bool
}

// UpdateOptions uses pointers so an empty value can clear a setting. RootPath,
// ID and CreatedAt are stable; adopting another checkout creates another project.
type UpdateOptions struct {
	Title              *string
	DefaultBaseBranch  *string
	SetupScript        *string
	DevCommand         *string
	CustomInstructions *string
	Env                *map[string]string
}

type Store struct{ dataDir string }

func NewStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("project data directory is required")
	}
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := prepareStoreDirectory(absolute); err != nil {
		return nil, err
	}
	absolute, err = canonicalPath(absolute)
	if err != nil {
		return nil, err
	}
	return &Store{dataDir: absolute}, nil
}

func (s *Store) List() ([]Project, error) {
	lock, err := workspace.LockDirectory(context.Background(), s.dataDir)
	if err != nil {
		return nil, err
	}
	defer workspace.Unlock(lock)
	return s.load()
}

func (s *Store) load() ([]Project, error) {
	data, err := os.ReadFile(filepath.Join(s.dataDir, "projects.json"))
	if errors.Is(err, os.ErrNotExist) {
		return []Project{}, nil
	}
	if err != nil {
		return nil, err
	}
	var projects []Project
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("read project registry: %w", err)
	}
	if projects == nil {
		projects = []Project{}
	}
	return projects, nil
}

// CreateOrAdopt registers a directory without modifying it unless initialization
// was requested. Re-adopting the same canonical path returns its existing entry.
func (s *Store) CreateOrAdopt(rootPath string, opts CreateOptions) (Project, error) {
	if rootPath == "" {
		return Project{}, errors.New("project root path is required")
	}
	if err := validateEnv(opts.Env); err != nil {
		return Project{}, err
	}
	lock, err := workspace.LockDirectory(context.Background(), s.dataDir)
	if err != nil {
		return Project{}, err
	}
	defer workspace.Unlock(lock)
	projects, err := s.load()
	if err != nil {
		return Project{}, err
	}
	if opts.InitGitIfMissing {
		if err := os.MkdirAll(rootPath, 0o755); err != nil {
			return Project{}, err
		}
	}
	rootPath, err = canonicalPath(rootPath)
	if err != nil {
		return Project{}, err
	}
	info, err := os.Stat(rootPath)
	if err != nil {
		return Project{}, err
	}
	if !info.IsDir() {
		return Project{}, errors.New("project root must be a directory")
	}
	for _, project := range projects {
		if project.RootPath == rootPath {
			return project, nil
		}
		if opts.ID != "" && project.ID == opts.ID {
			return Project{}, fmt.Errorf("%w: %s", ErrConflict, opts.ID)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if opts.InitGitIfMissing {
		if _, err := projectGit(ctx, rootPath, "rev-parse", "--show-toplevel"); err != nil {
			if err := initializeGit(ctx, rootPath, opts); err != nil {
				return Project{}, err
			}
		}
	}
	if opts.DefaultBaseBranch == "" {
		if branch, err := projectGit(ctx, rootPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
			opts.DefaultBaseBranch = strings.TrimSpace(string(branch))
		} else if branch, err := projectGit(ctx, rootPath, "symbolic-ref", "--short", "HEAD"); err == nil {
			opts.DefaultBaseBranch = strings.TrimSpace(string(branch))
		} else {
			opts.DefaultBaseBranch = "main"
		}
	}
	if opts.Title == "" {
		opts.Title = filepath.Base(rootPath)
	}
	if opts.ID == "" {
		opts.ID = "project_" + rand.Text()
	}
	now := time.Now().UTC()
	project := Project{
		ID: opts.ID, Title: opts.Title, RootPath: rootPath, DefaultBaseBranch: opts.DefaultBaseBranch,
		SetupScript: opts.SetupScript, DevCommand: opts.DevCommand, CustomInstructions: opts.CustomInstructions,
		Env: cloneEnv(opts.Env), CreatedAt: now, UpdatedAt: now,
	}
	projects = append(projects, project)
	if err := atomicJSON(filepath.Join(s.dataDir, "projects.json"), projects); err != nil {
		return Project{}, err
	}
	return project, nil
}

func (s *Store) Update(id string, opts UpdateOptions) (Project, error) {
	if opts.Env != nil {
		if err := validateEnv(*opts.Env); err != nil {
			return Project{}, err
		}
	}
	lock, err := workspace.LockDirectory(context.Background(), s.dataDir)
	if err != nil {
		return Project{}, err
	}
	defer workspace.Unlock(lock)
	projects, err := s.load()
	if err != nil {
		return Project{}, err
	}
	for i := range projects {
		project := &projects[i]
		if project.ID != id {
			continue
		}
		for _, field := range []struct{ target, value *string }{
			{&project.Title, opts.Title}, {&project.DefaultBaseBranch, opts.DefaultBaseBranch},
			{&project.SetupScript, opts.SetupScript}, {&project.DevCommand, opts.DevCommand},
			{&project.CustomInstructions, opts.CustomInstructions},
		} {
			if field.value != nil {
				*field.target = *field.value
			}
		}
		if opts.Env != nil {
			project.Env = cloneEnv(*opts.Env)
		}
		project.UpdatedAt = time.Now().UTC()
		if err := atomicJSON(filepath.Join(s.dataDir, "projects.json"), projects); err != nil {
			return Project{}, err
		}
		return *project, nil
	}
	return Project{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// Delete removes only the registration. It never removes the project directory.
func (s *Store) Delete(id string) error {
	lock, err := workspace.LockDirectory(context.Background(), s.dataDir)
	if err != nil {
		return err
	}
	defer workspace.Unlock(lock)
	projects, err := s.load()
	if err != nil {
		return err
	}
	for i, project := range projects {
		if project.ID == id {
			projects = append(projects[:i], projects[i+1:]...)
			return atomicJSON(filepath.Join(s.dataDir, "projects.json"), projects)
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

// FindByPath matches nested project directories first, then linked worktrees by
// Git's common directory. It does not depend on where a linked worktree lives.
func (s *Store) FindByPath(workspacePath string) (Project, error) {
	path, err := canonicalPath(workspacePath)
	if err != nil {
		return Project{}, err
	}
	projects, err := s.List()
	if err != nil {
		return Project{}, err
	}
	sort.SliceStable(projects, func(i, j int) bool { return len(projects[i].RootPath) > len(projects[j].RootPath) })
	for _, project := range projects {
		relative, err := filepath.Rel(project.RootPath, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return project, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	common, err := projectGit(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err == nil {
		for _, project := range projects {
			root, err := projectGit(ctx, project.RootPath, "rev-parse", "--show-toplevel")
			if err != nil || strings.TrimSpace(string(root)) != project.RootPath {
				continue // A registered subdirectory does not own the whole repo.
			}
			candidate, err := projectGit(ctx, project.RootPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
			if err == nil && strings.TrimSpace(string(candidate)) == strings.TrimSpace(string(common)) {
				return project, nil
			}
		}
	}
	return Project{}, fmt.Errorf("%w for path %s", ErrNotFound, path)
}

func initializeGit(ctx context.Context, path string, opts CreateOptions) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("Git initialization requires an empty directory; existing files were preserved")
	}
	branch := opts.DefaultBaseBranch
	if branch == "" {
		branch = "main"
	}
	if _, err := projectGit(ctx, path, "check-ref-format", "--branch", branch); err != nil {
		return err
	}
	if _, err := projectGit(ctx, path, "init", "--initial-branch="+branch); err != nil {
		return err
	}
	title := opts.Title
	if title == "" {
		title = filepath.Base(path)
	}
	for name, content := range map[string]string{
		".gitignore": ".DS_Store\n.env\n.env.local\nnode_modules/\n",
		"README.md":  "# " + title + "\n",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	if _, err := projectGit(ctx, path, "add", "--", ".gitignore", "README.md"); err != nil {
		return err
	}
	args := []string{}
	for key, fallback := range map[string]string{"user.name": "Vantry", "user.email": "vantry@localhost"} {
		if value, err := projectGit(ctx, path, "config", "--get", key); err != nil || strings.TrimSpace(string(value)) == "" {
			args = append(args, "-c", key+"="+fallback)
		}
	}
	_, err = projectGit(ctx, path, append(args, "commit", "-m", "Initial commit")...)
	return err
}

func canonicalPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(filepath.Clean(absolute))
}

func cloneEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	result := make(map[string]string, len(env))
	for key, value := range env {
		result[key] = value
	}
	return result
}

func validateEnv(env map[string]string) error {
	for key, value := range env {
		if key == "" || strings.ContainsRune(value, 0) {
			return errors.New("invalid project environment")
		}
		for i, r := range key {
			if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(i > 0 && r >= '0' && r <= '9') {
				return fmt.Errorf("invalid project environment key %q", key)
			}
		}
	}
	return nil
}

func projectGit(ctx context.Context, directory string, args ...string) ([]byte, error) {
	command := hostprocess.NewContext(ctx, "git", append([]string{"-C", directory}, args...)...)
	command.Env = workspace.GitEnvironment()
	defer command.Close()
	command.WaitDelay = 250 * time.Millisecond
	output, err := command.CombinedOutput()
	if command.Process != nil {
		_ = command.KillTree()
	}
	if err != nil || ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
		if len(output) > 4096 {
			output = output[:4096]
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, output)
	}
	return output, nil
}

func atomicJSON(path string, value any) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
