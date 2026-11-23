// Package pi implements the durable Pi provider. A detached Vantry worker owns
// each attempt; Core only starts it once and subsequently observes its journal.
package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/provider/controlled"
	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/internal/hostfs"
	"github.com/muq9738/Vantry/internal/hostprocess"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

const (
	ProviderPI         = "pi"
	ProviderControlled = "controlled"
)

type Options struct {
	DataDir              string
	Executable           string
	PiExecutable         string
	SocketPath           string
	DefaultModel         string
	DefaultProviderID    string
	DefaultWorkspace     *api.WorkspaceSpec
	ControlledDelay      time.Duration
	ResolveModel         func(string) (string, error)
	EnableUserExtensions bool
	EnableUserMCP        bool
	// EnableNativeSubagents keeps user-extension child tools (pi-subagents).
	// Off by default: Vantry delegation is the durable, visible child path.
	EnableNativeSubagents bool
	InteractionMode       string
	SandboxMode           string
}

type Provider struct {
	options         Options
	rawPiExecutable string
	controlled      controlled.Provider
}

type jobRequest struct {
	Request               provider.Request `json:"request"`
	DataDir               string           `json:"dataDir"`
	Executable            string           `json:"executable"`
	PiExecutable          string           `json:"piExecutable"`
	SocketPath            string           `json:"socketPath"`
	DefaultModel          string           `json:"defaultModel"`
	ExpectedModel         string           `json:"expectedModel,omitempty"`
	EnableUserExtensions  bool             `json:"enableUserExtensions,omitempty"`
	EnableUserMCP         bool             `json:"enableUserMCP,omitempty"`
	EnableNativeSubagents bool             `json:"enableNativeSubagents,omitempty"`
	InteractionMode       string           `json:"interactionMode,omitempty"`
	SandboxMode           string           `json:"sandboxMode,omitempty"`
}

type startedMarker struct {
	AttemptID string    `json:"attemptId"`
	PID       int       `json:"pid,omitempty"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type stopMarker struct {
	ThreadID  string    `json:"threadId"`
	RunID     string    `json:"runId"`
	AttemptID string    `json:"attemptId"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type jobResult struct {
	AttemptID    string    `json:"attemptId"`
	Status       string    `json:"status"`
	Error        string    `json:"error,omitempty"`
	LastSequence int64     `json:"lastSequence"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func New(options Options) *Provider {
	rawPiExecutable := options.PiExecutable
	options.PiExecutable = ResolveExecutable(rawPiExecutable, options.DataDir)
	if options.DefaultModel == "" {
		options.DefaultModel = ConfiguredModel()
	}
	if options.DefaultWorkspace != nil {
		copy := *options.DefaultWorkspace
		options.DefaultWorkspace = &copy
	}
	return &Provider{options: options, rawPiExecutable: rawPiExecutable, controlled: controlled.Provider{Delay: options.ControlledDelay}}
}

// Run retains the original streaming port. Durable Core uses RunEvents so tool
// and failure records are retained alongside assistant text deltas.
func (p *Provider) Run(ctx context.Context, request provider.Request, emit func(string) error) error {
	if p.route(request) == ProviderControlled {
		return p.controlled.Run(ctx, request, emit)
	}
	return p.RunEvents(ctx, request, func(event provider.Event) error {
		if event.Delta == "" {
			return nil
		}
		return emit(event.Delta)
	})
}

func (p *Provider) RunEvents(ctx context.Context, request provider.Request, emit func(provider.Event) error) error {
	route := p.route(request)
	if route != ProviderPI && route != ProviderControlled {
		return fmt.Errorf("unsupported provider %q", route)
	}
	if route == ProviderControlled {
		sequence := int64(0)
		return p.controlled.Run(ctx, request, func(delta string) error {
			sequence++
			if sequence <= request.AfterSequence {
				return nil
			}
			return emit(provider.Event{Sequence: sequence, Delta: delta, Type: "text_delta"})
		})
	}
	if request.AfterSequence < 0 {
		return errors.New("provider event cursor cannot be negative")
	}
	job, jobDir, err := p.ensureJob(ctx, request)
	if err != nil {
		return err
	}
	_ = job
	return p.observe(ctx, jobDir, request.AttemptID, request.AfterSequence, emit)
}

func (p *Provider) Recoverable(request provider.Request) bool {
	return p.route(request) == ProviderPI
}

func (p *Provider) SupportsControl(request provider.Request) bool {
	return p.Recoverable(request)
}

// Stop records explicit cancellation independently of observer lifetime.
// Cancelling RunEvents' context never calls Stop and never signals the worker.
func (p *Provider) Stop(ctx context.Context, request provider.Request) error {
	route := p.route(request)
	if route == ProviderControlled {
		return nil
	}
	if route != ProviderPI {
		return fmt.Errorf("unsupported provider %q", route)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(request.AttemptID); err != nil {
		return fmt.Errorf("invalid attempt ID: %w", err)
	}
	dataDir, err := p.dataDir()
	if err != nil {
		return err
	}
	jobDir := filepath.Join(dataDir, "jobs", request.AttemptID)
	job, found, err := readJobRequest(jobDir)
	if err != nil {
		return err
	}
	if !found {
		// Stop may win the race with the first RunEvents call. Persist the
		// cancellation without launching a worker; the later request journal
		// finalization will observe it before any prompt can be submitted.
		if err := makePrivateDir(jobDir); err != nil {
			return err
		}
		marker := stopMarker{ThreadID: request.ThreadID, RunID: request.RunID, AttemptID: request.AttemptID, UpdatedAt: time.Now().UTC()}
		return writeJSONFile(filepath.Join(jobDir, "stop.json"), marker)
	}
	if job.Request.AttemptID != request.AttemptID ||
		(request.RunID != "" && job.Request.RunID != request.RunID) ||
		(request.ThreadID != "" && job.Request.ThreadID != request.ThreadID) {
		return errors.New("stop request does not match the persisted attempt")
	}
	marker := stopMarker{ThreadID: job.Request.ThreadID, RunID: job.Request.RunID, AttemptID: request.AttemptID, UpdatedAt: time.Now().UTC()}
	if err := writeJSONFile(filepath.Join(jobDir, "stop.json"), marker); err != nil {
		return err
	}
	// If the worker itself died, persistent cancellation still cleans up its
	// independently running Pi group instead of leaving a marker nobody reads.
	return finalizeExitedWorker(jobDir, request.AttemptID, true)
}

func (p *Provider) route(request provider.Request) string {
	if request.ProviderID != "" {
		return request.ProviderID
	}
	if p.options.DefaultProviderID != "" {
		return p.options.DefaultProviderID
	}
	return ProviderPI
}

func (p *Provider) dataDir() (string, error) {
	if p.options.DataDir == "" {
		return "", errors.New("pi provider data directory is required")
	}
	absolute, err := filepath.Abs(p.options.DataDir)
	if err != nil {
		return "", err
	}
	if err := makePrivateDir(absolute); err != nil {
		return "", err
	}
	return absolute, nil
}

func (p *Provider) normalizeRequest(request provider.Request) (provider.Request, error) {
	if err := validID(request.ThreadID); err != nil {
		return provider.Request{}, fmt.Errorf("invalid thread ID: %w", err)
	}
	if err := validID(request.RunID); err != nil {
		return provider.Request{}, fmt.Errorf("invalid run ID: %w", err)
	}
	if err := validID(request.AttemptID); err != nil {
		return provider.Request{}, fmt.Errorf("invalid attempt ID: %w", err)
	}
	if request.Message == "" {
		return provider.Request{}, errors.New("pi prompt cannot be empty")
	}
	request.ProviderID = p.route(request)
	policy, err := normalizeApprovalPolicyValue(request.ApprovalPolicy)
	if err != nil {
		return provider.Request{}, err
	}
	request.ApprovalPolicy = policy
	if request.Model == "" {
		request.Model = p.options.DefaultModel
	}
	if request.Model == "" {
		request.Model = ConfiguredModel()
	}
	if request.Model == "" {
		return provider.Request{}, errors.New("pi model is required")
	}
	if request.Workspace == nil && p.options.DefaultWorkspace != nil {
		copy := *p.options.DefaultWorkspace
		request.Workspace = &copy
	}
	if request.Workspace == nil {
		dataDir, err := p.dataDir()
		if err != nil {
			return provider.Request{}, err
		}
		chat, err := workspace.ChatSpec(dataDir, request.ThreadID)
		if err != nil {
			return provider.Request{}, err
		}
		request.Workspace = &chat
	}
	copy := *request.Workspace
	if copy.Path != "" {
		copy.Path, err = filepath.Abs(copy.Path)
		if err != nil {
			return provider.Request{}, err
		}
	}
	if copy.Repository != "" {
		copy.Repository, err = filepath.Abs(copy.Repository)
		if err != nil {
			return provider.Request{}, err
		}
	}
	request.Workspace = &copy
	request.AfterSequence = 0
	return request, nil
}

func (p *Provider) ensureJob(ctx context.Context, request provider.Request) (jobRequest, string, error) {
	normalized, err := p.normalizeRequest(request)
	if err != nil {
		return jobRequest{}, "", err
	}
	dataDir, err := p.dataDir()
	if err != nil {
		return jobRequest{}, "", err
	}
	jobsDir := filepath.Join(dataDir, "jobs")
	if err := makePrivateDir(jobsDir); err != nil {
		return jobRequest{}, "", err
	}
	jobDir := filepath.Join(jobsDir, normalized.AttemptID)
	if err := makePrivateDir(jobDir); err != nil {
		return jobRequest{}, "", err
	}
	lock, err := lockFileContext(ctx, filepath.Join(jobDir, "start.lock"))
	if err != nil {
		return jobRequest{}, "", err
	}
	defer unlock(lock)

	job, found, err := readJobRequest(jobDir)
	if err != nil {
		return jobRequest{}, "", err
	}
	if found {
		// An accepted job owns its resolved defaults. Reattachment must not
		// move it to a newly configured model or workspace after a restart.
		if request.Model == "" {
			normalized.Model = job.Request.Model
		}
		if request.Workspace == nil {
			normalized.Workspace = job.Request.Workspace
		}
		// The interaction mode is resolved (explicit, env or "default") and
		// persisted when the job is accepted; a reattach that omits it keeps it.
		if request.InteractionMode == "" {
			normalized.InteractionMode = job.Request.InteractionMode
		}
		if !sameRequest(job.Request, normalized) {
			return jobRequest{}, "", errors.New("provider attempt request conflicts with its persisted request")
		}
		resultExists, err := fileExists(filepath.Join(jobDir, "result.json"))
		if err != nil {
			return jobRequest{}, "", err
		}
		if !resultExists {
			// A crash after request.json but before started.json could not have
			// launched Pi. It is safe to finish this one-time launch window.
			if err := startDetachedWorker(jobDir, normalized.AttemptID, job.Executable); err != nil {
				return jobRequest{}, "", err
			}
		}
		return job, jobDir, nil
	}
	expectedModel := ""
	if p.options.ResolveModel != nil && normalized.ProviderID == ProviderPI {
		expectedModel, err = p.options.ResolveModel(normalized.Model)
		if err != nil {
			return jobRequest{}, "", err
		}
	}
	if request.Workspace == nil && p.options.DefaultWorkspace == nil {
		if err := workspace.PrepareChatDirectory(dataDir, request.ThreadID); err != nil {
			return jobRequest{}, "", err
		}
	}
	executable := p.options.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return jobRequest{}, "", err
		}
	} else {
		executable, err = filepath.Abs(executable)
		if err != nil {
			return jobRequest{}, "", err
		}
	}
	rawExe := p.rawPiExecutable
	if rawExe == "" {
		rawExe = p.options.PiExecutable
	}
	piExecutable := ResolveExecutable(rawExe, dataDir)
	if piExecutable == "" {
		piExecutable = "pi"
	} else if strings.ContainsRune(piExecutable, filepath.Separator) {
		piExecutable, err = filepath.Abs(piExecutable)
		if err != nil {
			return jobRequest{}, "", err
		}
	}
	socketPath := p.options.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(dataDir, "core.sock")
	}
	interactionMode, err := normalizeInteractionModeValue(normalized.InteractionMode)
	if err != nil {
		return jobRequest{}, "", err
	}
	if interactionMode == "" {
		interactionMode, err = normalizeInteractionModeValue(p.options.InteractionMode)
		if err != nil {
			return jobRequest{}, "", err
		}
	}
	if interactionMode == "" {
		interactionMode, err = normalizeInteractionModeValue(os.Getenv(EnvInteractionMode))
		if err != nil {
			return jobRequest{}, "", err
		}
	}
	normalized.InteractionMode = interactionMode
	sandboxMode := resolveEffectiveSandboxMode(dataDir, p.options.SandboxMode)
	job = jobRequest{
		Request:               normalized,
		DataDir:               dataDir,
		Executable:            executable,
		PiExecutable:          piExecutable,
		SocketPath:            socketPath,
		DefaultModel:          p.options.DefaultModel,
		ExpectedModel:         expectedModel,
		EnableUserExtensions:  resolveBoolEnvOverride(EnvUserExtensions, p.options.EnableUserExtensions),
		EnableUserMCP:         resolveBoolEnvOverride(EnvUserMCP, p.options.EnableUserMCP),
		EnableNativeSubagents: resolveBoolEnvOverride(EnvNativeSubagents, p.options.EnableNativeSubagents),
		InteractionMode:       interactionMode,
		SandboxMode:           string(sandboxMode),
	}
	if err := writeJSONFile(filepath.Join(jobDir, "request.json"), job); err != nil {
		return jobRequest{}, "", fmt.Errorf("persist provider request: %w", err)
	}
	if _, err := os.Stat(filepath.Join(jobDir, "stop.json")); err == nil {
		result := jobResult{AttemptID: normalized.AttemptID, Status: "cancelled", UpdatedAt: time.Now().UTC()}
		return job, jobDir, writeJSONFile(filepath.Join(jobDir, "result.json"), result)
	} else if !errors.Is(err, os.ErrNotExist) {
		return jobRequest{}, "", err
	}
	if err := startDetachedWorker(jobDir, normalized.AttemptID, executable); err != nil {
		return jobRequest{}, "", err
	}
	return job, jobDir, nil
}

func sameRequest(left, right provider.Request) bool {
	left.AfterSequence, right.AfterSequence = 0, 0
	// An omitted policy has always meant auto, including persisted attempts
	// created before explicit approval policies were exposed by Core.
	if left.ApprovalPolicy == "" {
		left.ApprovalPolicy = "auto"
	}
	if right.ApprovalPolicy == "" {
		right.ApprovalPolicy = "auto"
	}
	return reflect.DeepEqual(left, right)
}

func startDetachedWorker(jobDir, attemptID, executable string) error {
	startedPath := filepath.Join(jobDir, "started.json")
	found, err := fileExists(startedPath)
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	// The launch intent precedes exec. If Core dies in this window, recovery
	// fails the attempt rather than risking a second prompt submission.
	started := startedMarker{AttemptID: attemptID, State: "launching", UpdatedAt: time.Now().UTC()}
	if err := writeJSONFile(startedPath, started); err != nil {
		return fmt.Errorf("persist worker launch intent: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(jobDir, "worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	command := exec.Command(executable, "provider-worker", "--job-dir", jobDir)
	command.Dir = jobDir
	command.Stdout = logFile
	command.Stderr = logFile
	hostprocess.ConfigureDetached(command)
	if err := command.Start(); err != nil {
		result := jobResult{AttemptID: attemptID, Status: "failed", Error: fmt.Sprintf("start provider worker: %v", err), UpdatedAt: time.Now().UTC()}
		_ = writeJSONFile(filepath.Join(jobDir, "result.json"), result)
		return fmt.Errorf("start provider worker: %w", err)
	}
	started.PID = command.Process.Pid
	started.State = "started"
	started.UpdatedAt = time.Now().UTC()
	if err := writeJSONFile(startedPath, started); err != nil {
		// The worker owns execution now. Never start another one to repair a
		// marker write failure; its own lock and result remain authoritative.
		// The pre-exec intent remains sufficient for crash-safe attachment.
		_ = command.Process.Release()
		return nil
	}
	// Release disowns the process handle without waiting. The worker remains in
	// its own session and publishes its own durable result.
	_ = command.Process.Release()
	return nil
}

func readJobRequest(jobDir string) (jobRequest, bool, error) {
	var job jobRequest
	found, err := readJSONFile(filepath.Join(jobDir, "request.json"), &job)
	return job, found, err
}

func validID(value string) error {
	if value == "" || value == "." || value == ".." {
		return errors.New("identifier is empty or reserved")
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("unsafe identifier character %q", r)
	}
	return nil
}

func readJSONFile(path string, target any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	return true, nil
}

func writeJSONFile(path string, value any) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
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
	// Persist both file contents and replacement metadata before publishing.
	return hostfs.RenameDurable(temporary, path)
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func lockFileContext(ctx context.Context, path string) (*os.File, error) {
	file, err := openProviderLock(path)
	if err != nil {
		return nil, err
	}
	for {
		err = hostfs.TryLock(file)
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

func tryLock(path string) (*os.File, bool, error) {
	file, err := openProviderLock(path)
	if err != nil {
		return nil, false, err
	}
	if err := hostfs.TryLock(file); err != nil {
		_ = file.Close()
		if hostfs.LockBusy(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return file, true, nil
}

func unlock(file *os.File) {
	if file == nil {
		return
	}
	_ = hostfs.Unlock(file)
	_ = file.Close()
}

func resultError(result jobResult) error {
	switch result.Status {
	case "completed":
		return nil
	case "cancelled":
		return context.Canceled
	default:
		if strings.TrimSpace(result.Error) == "" {
			return errors.New("pi provider worker failed without an error message")
		}
		return errors.New(result.Error)
	}
}

var _ provider.Durable = (*Provider)(nil)
