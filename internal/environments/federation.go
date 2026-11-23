// Package environments coordinates cross-host Vantry Core environments,
// including pooled peer connections, idempotent remote task delegation,
// cursor-resumable event watching, remote cancellation, and local task ledger
// persistence.
package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/adapters/transport/sshrelay"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	ledgerFileName            = "remote-tasks.json"
	ledgerFormatVersion       = 1
	defaultIdleTimeout        = 5 * time.Minute
	defaultHealthTimeout      = 2 * time.Second
	defaultReconnectBackoff   = 50 * time.Millisecond
	maxReconnectBackoff       = 2 * time.Second
	defaultReconcileInterval  = time.Second
	defaultDispatchMaxRetries = 2
)

// ErrFederatorClosed indicates the Federator has been shut down.
var ErrFederatorClosed = errors.New("environments: federator is closed")

// PeerTarget identifies a local or remote Vantry Core environment.
// When Host is empty, DefaultDial connects directly to SocketPath over a local
// Unix domain socket. When Host is non-empty, DefaultDial establishes a managed
// SSH socket forward via sshrelay.Connect.
type PeerTarget struct {
	// ConnectionID selects an existing authenticated access pairing. A custom
	// dialer resolves it; it is not a credential or a transport address.
	ConnectionID  string `json:"connectionId,omitempty"`
	EnvironmentID string `json:"environmentId"`
	Name          string `json:"name,omitempty"`
	Host          string `json:"host,omitempty"`
	SocketPath    string `json:"socketPath"`
}

// PeerClient is the transport contract for a connected peer Core.
// Both *local.Client and *sshrelay.Relay implement PeerClient.
type PeerClient interface {
	api.Service
	Close() error
}

// DialFunc establishes a transport client for target. The provided ctx governs
// the lifetime of managed tunnels such as sshrelay.Relay.
type DialFunc func(ctx context.Context, target PeerTarget) (PeerClient, error)

// Dialer abstracts peer connection establishment.
type Dialer interface {
	Dial(ctx context.Context, target PeerTarget) (PeerClient, error)
}

// Dial implements Dialer.
func (f DialFunc) Dial(ctx context.Context, target PeerTarget) (PeerClient, error) {
	return f(ctx, target)
}

// ValidateTarget checks whether target contains a usable transport address.
func ValidateTarget(target PeerTarget) error {
	host := strings.TrimSpace(target.Host)
	socket := strings.TrimSpace(target.SocketPath)
	if socket == "" && strings.TrimSpace(target.ConnectionID) != "" {
		return nil
	}
	if socket == "" {
		return &api.Error{
			Code:    api.InvalidCommand,
			Message: "peer target socketPath is required",
		}
	}
	if host != "" {
		if err := sshrelay.Validate(host, socket); err != nil {
			return &api.Error{
				Code:    api.InvalidCommand,
				Message: fmt.Sprintf("invalid SSH peer target: %v", err),
			}
		}
		return nil
	}
	if strings.ContainsAny(socket, "\r\n\x00") {
		return &api.Error{
			Code:    api.InvalidCommand,
			Message: "peer target socketPath contains invalid control characters",
		}
	}
	return nil
}

// DefaultDial connects to target over a local Unix socket when Host is empty,
// or over a managed SSH tunnel when Host is non-empty.
func DefaultDial(ctx context.Context, target PeerTarget) (PeerClient, error) {
	if ctx == nil {
		return nil, errors.New("environments: nil context")
	}
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	host := strings.TrimSpace(target.Host)
	socket := strings.TrimSpace(target.SocketPath)
	if host != "" {
		return sshrelay.Connect(ctx, host, socket)
	}

	client := local.Dial(socket)
	probeCtx, cancel := context.WithTimeout(ctx, defaultHealthTimeout)
	health, err := client.Health(probeCtx)
	cancel()
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := checkPeerHealth(health); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func checkPeerHealth(health api.Health) error {
	if health.Version != api.ProtocolVersion {
		return &api.Error{
			Code:    api.VersionMismatch,
			Message: fmt.Sprintf("peer protocol version %d does not match local version %d", health.Version, api.ProtocolVersion),
		}
	}
	if health.State != "ready" {
		return &api.Error{
			Code:    api.NotReady,
			Message: fmt.Sprintf("peer core state is %q", health.State),
		}
	}
	return nil
}

// FederatorOptions configures a Federator instance.
type FederatorOptions struct {
	// DataDir is the directory holding remote-tasks.json (mode 0600).
	// When empty, task state is tracked in memory only.
	DataDir string

	// Dial overrides DefaultDial when set.
	Dial DialFunc

	// Dialer overrides DefaultDial when Dial is nil.
	Dialer Dialer

	// IdleTimeout evicts pooled connections that have been idle longer than this
	// duration. Zero uses 5 minutes; negative disables idle eviction.
	IdleTimeout time.Duration

	// HealthTimeout bounds health checks when reusing a pooled connection.
	// Zero uses 2 seconds.
	HealthTimeout time.Duration

	// ReconnectBackoff is the initial wait before redialing an interrupted
	// subscription or transient transport failure. Zero uses 50ms.
	ReconnectBackoff time.Duration

	// ReconcileInterval controls periodic Snapshot fallback checks while a
	// subscription is open. Zero uses 1s.
	ReconcileInterval time.Duration
}

// RemoteTaskSpec describes a task to delegate to a peer Core environment.
type RemoteTaskSpec struct {
	// LocalTaskID is the stable local task key stored in remote-tasks.json.
	// If empty, RemoteTaskID or CommandID is used.
	LocalTaskID string `json:"localTaskId,omitempty"`

	// RemoteTaskID is the stable remote task identifier across retries.
	// Defaults to LocalTaskID when omitted.
	RemoteTaskID string `json:"remoteTaskId,omitempty"`

	// CommandID is the caller-supplied idempotent command base. thread.create
	// and message.send derive deterministic sub-command IDs from this base so
	// retries never duplicate threads or prompts.
	CommandID string `json:"commandId,omitempty"`

	// RemoteThreadID optionally pins the created thread ID on the peer Core.
	// Defaults to a deterministic ID derived from RemoteTaskID.
	RemoteThreadID string `json:"remoteThreadId,omitempty"`

	Title           string             `json:"title,omitempty"`
	Prompt          string             `json:"prompt"`
	Attachments     []api.Attachment   `json:"attachments,omitempty"`
	Workspace       *api.WorkspaceSpec `json:"workspace,omitempty"`
	ProviderID      string             `json:"providerId,omitempty"`
	Model           string             `json:"model,omitempty"`
	ApprovalPolicy  string             `json:"approvalPolicy,omitempty"`
	InteractionMode string             `json:"interactionMode,omitempty"`
	// ExistingThread sends a later local run to the same remote session without
	// creating it again. The original local thread ID namespaces that session.
	ExistingThread bool `json:"existingThread,omitempty"`
}

// RemoteTaskHandle records the durable mapping between a local task identity
// and its execution on a remote Core.
type RemoteTaskHandle struct {
	LocalTaskID     string        `json:"localTaskId"`
	RemoteTaskID    string        `json:"remoteTaskId,omitempty"`
	CommandID       string        `json:"commandId"`
	Target          PeerTarget    `json:"target"`
	RemoteThreadID  string        `json:"remoteThreadId"`
	RemoteRunID     string        `json:"remoteRunId"`
	Status          api.RunStatus `json:"status"`
	InitialSequence int64         `json:"initialSequence,omitempty"`
	Cursor          int64         `json:"cursor,omitempty"`
	Output          string        `json:"output,omitempty"`
	Reasoning       string        `json:"reasoning,omitempty"`
	Error           string        `json:"error,omitempty"`
	CreatedAt       time.Time     `json:"createdAt"`
	UpdatedAt       time.Time     `json:"updatedAt"`
}

func (h *RemoteTaskHandle) clone() *RemoteTaskHandle {
	if h == nil {
		return nil
	}
	cp := *h
	return &cp
}

// RemoteTaskResult is the terminal outcome returned by WaitRemote.
type RemoteTaskResult struct {
	Status         api.RunStatus `json:"status"`
	Output         string        `json:"output"`
	Reasoning      string        `json:"reasoning,omitempty"`
	Error          string        `json:"error,omitempty"`
	RemoteThreadID string        `json:"remoteThreadId"`
	RemoteRunID    string        `json:"remoteRunId"`
}

// RemoteStreamDelta carries an incremental update observed while watching a
// remote run.
type RemoteStreamDelta struct {
	Sequence       int64
	OutputDelta    string
	ReasoningDelta string
	Status         api.RunStatus
}

type peerKey struct {
	connectionID  string
	environmentID string
	host          string
	socketPath    string
}

func makePeerKey(target PeerTarget) peerKey {
	socket := strings.TrimSpace(target.SocketPath)
	if socket != "" {
		socket = filepath.Clean(socket)
	}
	return peerKey{
		connectionID:  strings.TrimSpace(target.ConnectionID),
		environmentID: strings.TrimSpace(target.EnvironmentID),
		host:          strings.TrimSpace(target.Host),
		socketPath:    socket,
	}
}

func normalizeTarget(target PeerTarget) PeerTarget {
	socket := strings.TrimSpace(target.SocketPath)
	if socket != "" {
		socket = filepath.Clean(socket)
	}
	return PeerTarget{
		ConnectionID:  strings.TrimSpace(target.ConnectionID),
		EnvironmentID: strings.TrimSpace(target.EnvironmentID),
		Name:          strings.TrimSpace(target.Name),
		Host:          strings.TrimSpace(target.Host),
		SocketPath:    socket,
	}
}

func samePeerAddress(a, b PeerTarget) bool {
	return makePeerKey(a) == makePeerKey(b)
}

type peerSlot struct {
	mu       sync.Mutex
	connID   uint64
	client   PeerClient
	cancel   context.CancelFunc
	lastUsed time.Time
}

type ledgerDiskPayload struct {
	Version int                          `json:"version"`
	Tasks   map[string]*RemoteTaskHandle `json:"tasks"`
}

// Federator manages pooled peer connections and coordinates delegated remote
// tasks across Vantry Core environments.
type Federator struct {
	dialer            Dialer
	dataDir           string
	ledgerPath        string
	idleTimeout       time.Duration
	healthTimeout     time.Duration
	reconnectBackoff  time.Duration
	reconcileInterval time.Duration

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	poolMu     sync.Mutex
	closed     bool
	peers      map[peerKey]*peerSlot
	nextConnID atomic.Uint64

	ledgerMu sync.RWMutex
	tasks    map[string]*RemoteTaskHandle

	closeOnce sync.Once
	closeErr  error
}

// NewFederator initializes a Federator and loads any persisted remote task
// ledger from opts.DataDir/remote-tasks.json.
func NewFederator(opts FederatorOptions) (*Federator, error) {
	var dialer Dialer
	switch {
	case opts.Dial != nil:
		dialer = opts.Dial
	case opts.Dialer != nil:
		dialer = opts.Dialer
	default:
		dialer = DialFunc(DefaultDial)
	}

	idleTimeout := opts.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = defaultIdleTimeout
	}
	healthTimeout := opts.HealthTimeout
	if healthTimeout <= 0 {
		healthTimeout = defaultHealthTimeout
	}
	reconnectBackoff := opts.ReconnectBackoff
	if reconnectBackoff <= 0 {
		reconnectBackoff = defaultReconnectBackoff
	}
	reconcileInterval := opts.ReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = defaultReconcileInterval
	}

	var (
		dataDir    string
		ledgerPath string
	)
	if trimmed := strings.TrimSpace(opts.DataDir); trimmed != "" {
		absDir, err := filepath.Abs(trimmed)
		if err != nil {
			return nil, fmt.Errorf("environments: resolve dataDir: %w", err)
		}
		if err := os.MkdirAll(absDir, 0o700); err != nil {
			return nil, fmt.Errorf("environments: create dataDir: %w", err)
		}
		dataDir = absDir
		ledgerPath = filepath.Join(absDir, ledgerFileName)
	}

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	f := &Federator{
		dialer:            dialer,
		dataDir:           dataDir,
		ledgerPath:        ledgerPath,
		idleTimeout:       idleTimeout,
		healthTimeout:     healthTimeout,
		reconnectBackoff:  reconnectBackoff,
		reconcileInterval: reconcileInterval,
		lifecycleCtx:      lifecycleCtx,
		lifecycleCancel:   lifecycleCancel,
		peers:             make(map[peerKey]*peerSlot),
		tasks:             make(map[string]*RemoteTaskHandle),
	}

	if ledgerPath != "" {
		if err := f.loadLedger(); err != nil {
			lifecycleCancel()
			return nil, err
		}
	}
	return f, nil
}

// LedgerPath returns the path to remote-tasks.json, or "" when persistence is disabled.
func (f *Federator) LedgerPath() string {
	return f.ledgerPath
}

// GetTask returns a copy of the tracked RemoteTaskHandle for localTaskID.
func (f *Federator) GetTask(localTaskID string) (*RemoteTaskHandle, bool) {
	id := strings.TrimSpace(localTaskID)
	if id == "" {
		return nil, false
	}
	f.ledgerMu.RLock()
	defer f.ledgerMu.RUnlock()
	rec, ok := f.tasks[id]
	if !ok {
		return nil, false
	}
	return rec.clone(), true
}

// ListTasks returns copies of all tracked remote tasks ordered by CreatedAt then LocalTaskID.
func (f *Federator) ListTasks() []*RemoteTaskHandle {
	f.ledgerMu.RLock()
	out := make([]*RemoteTaskHandle, 0, len(f.tasks))
	for _, rec := range f.tasks {
		out = append(out, rec.clone())
	}
	f.ledgerMu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].LocalTaskID < out[j].LocalTaskID
	})
	return out
}

// ActiveTasks returns copies of tracked remote tasks whose Status is not yet terminal.
func (f *Federator) ActiveTasks() []*RemoteTaskHandle {
	all := f.ListTasks()
	active := make([]*RemoteTaskHandle, 0, len(all))
	for _, rec := range all {
		if !rec.Status.Terminal() {
			active = append(active, rec)
		}
	}
	return active
}

// Close shuts down the Federator and releases all pooled peer connections and SSH tunnels.
func (f *Federator) Close() error {
	f.closeOnce.Do(func() {
		f.poolMu.Lock()
		f.closed = true
		f.lifecycleCancel()
		slots := make([]*peerSlot, 0, len(f.peers))
		for k, slot := range f.peers {
			slots = append(slots, slot)
			delete(f.peers, k)
		}
		f.poolMu.Unlock()

		var errs []error
		for _, slot := range slots {
			slot.mu.Lock()
			client := slot.client
			cancel := slot.cancel
			slot.client = nil
			slot.cancel = nil
			slot.connID = 0
			slot.mu.Unlock()

			if cancel != nil {
				cancel()
			}
			if client != nil {
				if err := client.Close(); err != nil {
					errs = append(errs, err)
				}
			}
		}
		f.closeErr = errors.Join(errs...)
	})
	return f.closeErr
}

func (f *Federator) isClosed() bool {
	f.poolMu.Lock()
	closed := f.closed
	f.poolMu.Unlock()
	return closed
}

func (f *Federator) getSlot(key peerKey) (*peerSlot, error) {
	f.poolMu.Lock()
	defer f.poolMu.Unlock()
	if f.closed {
		return nil, ErrFederatorClosed
	}
	slot, ok := f.peers[key]
	if !ok {
		slot = &peerSlot{}
		f.peers[key] = slot
	}
	return slot, nil
}

func (f *Federator) acquireClient(ctx context.Context, target PeerTarget, forceRedial bool) (PeerClient, uint64, error) {
	if ctx == nil {
		return nil, 0, errors.New("environments: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := ValidateTarget(target); err != nil {
		return nil, 0, err
	}

	normalized := normalizeTarget(target)
	key := makePeerKey(normalized)
	slot, err := f.getSlot(key)
	if err != nil {
		return nil, 0, err
	}

	slot.mu.Lock()
	defer slot.mu.Unlock()

	if f.isClosed() {
		return nil, 0, ErrFederatorClosed
	}

	now := time.Now()
	if slot.client != nil {
		expired := f.idleTimeout > 0 && !slot.lastUsed.IsZero() && now.Sub(slot.lastUsed) > f.idleTimeout
		if !forceRedial && !expired && f.probeClientHealthy(ctx, slot.client) {
			slot.lastUsed = time.Now()
			return slot.client, slot.connID, nil
		}
		f.closeSlotLocked(slot)
	}

	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	// Tie the pooled connection lifetime to the Federator, while still allowing
	// caller ctx cancellation to abort an in-progress dial immediately.
	connCtx, connCancel := context.WithCancel(f.lifecycleCtx)
	abortOnCallerCancel := context.AfterFunc(ctx, connCancel)

	client, dialErr := f.dialer.Dial(connCtx, normalized)
	stopped := abortOnCallerCancel()
	if dialErr != nil {
		connCancel()
		if !stopped && ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if f.isClosed() {
			return nil, 0, ErrFederatorClosed
		}
		return nil, 0, dialErr
	}
	if !stopped || ctx.Err() != nil || connCtx.Err() != nil {
		connCancel()
		_ = client.Close()
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		return nil, 0, ErrFederatorClosed
	}
	if f.isClosed() {
		connCancel()
		_ = client.Close()
		return nil, 0, ErrFederatorClosed
	}

	if !f.probeClientHealthy(ctx, client) {
		probeCtx, cancel := context.WithTimeout(ctx, f.healthTimeout)
		health, hErr := client.Health(probeCtx)
		cancel()
		connCancel()
		_ = client.Close()
		if hErr != nil {
			return nil, 0, hErr
		}
		return nil, 0, checkPeerHealth(health)
	}

	id := f.nextConnID.Add(1)
	slot.connID = id
	slot.client = client
	slot.cancel = connCancel
	slot.lastUsed = time.Now()
	return client, id, nil
}

func (f *Federator) probeClientHealthy(ctx context.Context, client PeerClient) bool {
	if relay, ok := client.(*sshrelay.Relay); ok {
		select {
		case <-relay.Done():
			return false
		default:
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, f.healthTimeout)
	defer cancel()
	health, err := client.Health(probeCtx)
	if err != nil {
		return false
	}
	return checkPeerHealth(health) == nil
}

func (f *Federator) evictClient(key peerKey, connID uint64) {
	if connID == 0 {
		return
	}
	f.poolMu.Lock()
	slot, ok := f.peers[key]
	f.poolMu.Unlock()
	if !ok {
		return
	}

	slot.mu.Lock()
	if slot.connID != connID {
		slot.mu.Unlock()
		return
	}
	client := slot.client
	cancel := slot.cancel
	slot.client = nil
	slot.cancel = nil
	slot.connID = 0
	slot.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if client != nil {
		_ = client.Close()
	}
}

func (f *Federator) closeSlotLocked(slot *peerSlot) {
	client := slot.client
	cancel := slot.cancel
	slot.client = nil
	slot.cancel = nil
	slot.connID = 0
	if cancel != nil {
		cancel()
	}
	if client != nil {
		_ = client.Close()
	}
}

func (f *Federator) touchClient(key peerKey, connID uint64) {
	f.poolMu.Lock()
	slot, ok := f.peers[key]
	f.poolMu.Unlock()
	if !ok {
		return
	}
	slot.mu.Lock()
	if slot.connID == connID && slot.client != nil {
		slot.lastUsed = time.Now()
	}
	slot.mu.Unlock()
}

// Client returns the pooled authenticated client. The Federator owns its
// lifetime; callers must not close it. All callers still choose a linked target.
func (f *Federator) Client(ctx context.Context, target PeerTarget) (PeerClient, error) {
	client, _, err := f.acquireClient(ctx, target, false)
	return client, err
}

// DelegateRemote creates a remote thread and dispatches the task message on target
// using deterministic idempotent CommandIDs derived from spec. Retrying the same
// RemoteTaskID / CommandID after a network drop returns the original remote
// thread and run without duplicating either.
func (f *Federator) DelegateRemote(ctx context.Context, target PeerTarget, spec RemoteTaskSpec) (*RemoteTaskHandle, error) {
	if ctx == nil {
		return nil, errors.New("environments: nil context")
	}
	if f.isClosed() {
		return nil, ErrFederatorClosed
	}
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}

	normalizedTarget := normalizeTarget(target)
	localTaskID := firstNonEmpty(
		strings.TrimSpace(spec.LocalTaskID),
		strings.TrimSpace(spec.RemoteTaskID),
		strings.TrimSpace(spec.CommandID),
	)
	if localTaskID == "" {
		return nil, &api.Error{
			Code:    api.InvalidCommand,
			Message: "localTaskId, remoteTaskId, or commandId is required",
		}
	}
	if strings.TrimSpace(spec.Prompt) == "" {
		return nil, &api.Error{
			Code:    api.InvalidCommand,
			Message: "prompt is required",
		}
	}

	remoteTaskID := firstNonEmpty(strings.TrimSpace(spec.RemoteTaskID), localTaskID)
	commandBase := firstNonEmpty(strings.TrimSpace(spec.CommandID), "fed:"+remoteTaskID)
	remoteThreadID := firstNonEmpty(strings.TrimSpace(spec.RemoteThreadID), "fed-thread:"+remoteTaskID)

	if existing, ok := f.GetTask(localTaskID); ok {
		if existing.CommandID != "" && existing.CommandID != commandBase {
			return nil, &api.Error{
				Code:    api.Conflict,
				Message: fmt.Sprintf("localTaskId %q already bound to commandId %q", localTaskID, existing.CommandID),
			}
		}
		if (existing.Target.SocketPath != "" || existing.Target.ConnectionID != "") && !samePeerAddress(existing.Target, normalizedTarget) {
			return nil, &api.Error{
				Code:    api.Conflict,
				Message: fmt.Sprintf("localTaskId %q already bound to another peer target", localTaskID),
			}
		}
		if strings.TrimSpace(spec.RemoteThreadID) != "" && existing.RemoteThreadID != "" && existing.RemoteThreadID != remoteThreadID {
			return nil, &api.Error{
				Code:    api.Conflict,
				Message: fmt.Sprintf("localTaskId %q already bound to remoteThreadId %q", localTaskID, existing.RemoteThreadID),
			}
		}
		if existing.RemoteThreadID != "" {
			remoteThreadID = existing.RemoteThreadID
		}
	}

	createCmd := api.Command{
		Version:         api.ProtocolVersion,
		CommandID:       commandBase + ":thread.create",
		Kind:            api.CreateThread,
		ThreadID:        remoteThreadID,
		Title:           spec.Title,
		ProviderID:      spec.ProviderID,
		Model:           spec.Model,
		Workspace:       cloneWorkspace(spec.Workspace),
		ApprovalPolicy:  spec.ApprovalPolicy,
		InteractionMode: spec.InteractionMode,
	}

	key := makePeerKey(normalizedTarget)
	var lastErr error
	forceRedial := false

	for attempt := 0; attempt <= defaultDispatchMaxRetries; attempt++ {
		if attempt > 0 {
			if err := f.waitBackoff(ctx, attempt-1); err != nil {
				return nil, err
			}
		}

		client, connID, err := f.acquireClient(ctx, normalizedTarget, forceRedial)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil || errors.Is(err, ErrFederatorClosed) || isDeterministicAPIError(err) {
				return nil, err
			}
			forceRedial = true
			continue
		}
		forceRedial = false

		resolvedThreadID := remoteThreadID
		if !spec.ExistingThread {
			createReceipt, err := client.Dispatch(ctx, createCmd)
			if err != nil {
				lastErr = err
				if ctx.Err() != nil || isDeterministicAPIError(err) {
					return nil, err
				}
				f.evictClient(key, connID)
				forceRedial = true
				continue
			}
			resolvedThreadID = firstNonEmpty(createReceipt.ThreadID, remoteThreadID)
		} else if spec.Model != "" {
			_, err := client.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, Kind: api.SetThreadModel,
				CommandID: commandBase + ":thread.model", ThreadID: resolvedThreadID, Model: spec.Model, ProviderID: spec.ProviderID})
			if err != nil {
				lastErr = err
				if ctx.Err() != nil || isDeterministicAPIError(err) {
					return nil, err
				}
				f.evictClient(key, connID)
				forceRedial = true
				continue
			}
		}
		sendCmd := api.Command{
			Version:         api.ProtocolVersion,
			CommandID:       commandBase + ":message.send",
			Kind:            api.SendMessage,
			ThreadID:        resolvedThreadID,
			Message:         spec.Prompt,
			Attachments:     cloneAttachments(spec.Attachments),
			InteractionMode: spec.InteractionMode,
		}

		sendReceipt, err := client.Dispatch(ctx, sendCmd)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil || isDeterministicAPIError(err) {
				return nil, err
			}
			f.evictClient(key, connID)
			forceRedial = true
			continue
		}
		f.touchClient(key, connID)

		handle, err := f.recordDelegatedTask(localTaskID, remoteTaskID, commandBase, normalizedTarget, resolvedThreadID, sendReceipt)
		if err != nil {
			return nil, err
		}
		return handle, nil
	}

	return nil, lastErr
}

func (f *Federator) recordDelegatedTask(
	localTaskID string,
	remoteTaskID string,
	commandBase string,
	target PeerTarget,
	remoteThreadID string,
	sendReceipt api.Receipt,
) (*RemoteTaskHandle, error) {
	now := time.Now().UTC()
	f.ledgerMu.Lock()
	defer f.ledgerMu.Unlock()

	rec, exists := f.tasks[localTaskID]
	if !exists {
		rec = &RemoteTaskHandle{
			LocalTaskID:     localTaskID,
			RemoteTaskID:    remoteTaskID,
			CommandID:       commandBase,
			Target:          target,
			RemoteThreadID:  remoteThreadID,
			RemoteRunID:     sendReceipt.RunID,
			Status:          api.RunQueued,
			InitialSequence: sendReceipt.Sequence,
			Cursor:          sendReceipt.Sequence,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
	} else {
		rec.RemoteTaskID = firstNonEmpty(rec.RemoteTaskID, remoteTaskID)
		rec.CommandID = firstNonEmpty(rec.CommandID, commandBase)
		rec.Target = target
		rec.RemoteThreadID = firstNonEmpty(sendReceipt.ThreadID, remoteThreadID, rec.RemoteThreadID)
		rec.RemoteRunID = firstNonEmpty(sendReceipt.RunID, rec.RemoteRunID)
		if rec.InitialSequence == 0 && sendReceipt.Sequence > 0 {
			rec.InitialSequence = sendReceipt.Sequence
		}
		if rec.Cursor == 0 && sendReceipt.Sequence > 0 {
			rec.Cursor = sendReceipt.Sequence
		}
		if rec.Status == "" {
			rec.Status = api.RunQueued
		}
		rec.UpdatedAt = now
	}
	f.tasks[localTaskID] = rec
	if err := f.saveLedgerLocked(); err != nil {
		return nil, err
	}
	return rec.clone(), nil
}

// WaitRemote streams events from target for handle's RemoteRunID, reconnecting
// from the latest Sequence cursor if interrupted and reconciling against
// Snapshot until the run reaches a terminal status.
func (f *Federator) WaitRemote(ctx context.Context, target PeerTarget, handle *RemoteTaskHandle) (RemoteTaskResult, error) {
	return f.WatchRemote(ctx, target, handle, nil)
}

// WatchRemote behaves like WaitRemote and additionally invokes onDelta (when
// non-nil) as incremental output, reasoning, or status updates arrive.
func (f *Federator) WatchRemote(
	ctx context.Context,
	target PeerTarget,
	handle *RemoteTaskHandle,
	onDelta func(RemoteStreamDelta),
) (RemoteTaskResult, error) {
	if ctx == nil {
		return RemoteTaskResult{}, errors.New("environments: nil context")
	}
	if f.isClosed() {
		return RemoteTaskResult{}, ErrFederatorClosed
	}

	resolved, resolvedTarget, err := f.resolveHandleAndTarget(target, handle)
	if err != nil {
		return RemoteTaskResult{}, err
	}
	if resolved.RemoteRunID == "" {
		return RemoteTaskResult{}, &api.Error{
			Code:    api.InvalidCommand,
			Message: "remoteRunId is required to wait on a remote task",
		}
	}

	key := makePeerKey(resolvedTarget)
	cursor := resolved.InitialSequence
	if cursor < 0 {
		cursor = 0
	}

	var (
		outputBuf    strings.Builder
		reasoningBuf strings.Builder
		forceRedial  bool
		needResync   bool
		attempt      int
	)

	if resolved.Cursor > cursor && (resolved.Output != "" || resolved.Reasoning != "") {
		cursor = resolved.Cursor
		outputBuf.WriteString(resolved.Output)
		reasoningBuf.WriteString(resolved.Reasoning)
	}

	for {
		if err := ctx.Err(); err != nil {
			f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
			return RemoteTaskResult{}, err
		}
		if f.isClosed() {
			f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
			return RemoteTaskResult{}, ErrFederatorClosed
		}

		client, connID, err := f.acquireClient(ctx, resolvedTarget, forceRedial)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrFederatorClosed) || isDeterministicAPIError(err) {
				f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
				return RemoteTaskResult{}, err
			}
			forceRedial = true
			if waitErr := f.waitBackoff(ctx, attempt); waitErr != nil {
				f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
				return RemoteTaskResult{}, waitErr
			}
			attempt++
			continue
		}
		forceRedial = false

		if needResync {
			snap, snapErr := client.Snapshot(ctx)
			if snapErr != nil {
				if ctx.Err() != nil || isDeterministicAPIError(snapErr) {
					return RemoteTaskResult{}, snapErr
				}
				f.evictClient(key, connID)
				forceRedial = true
				if waitErr := f.waitBackoff(ctx, attempt); waitErr != nil {
					return RemoteTaskResult{}, waitErr
				}
				attempt++
				continue
			}
			run, found := findRun(snap, resolved.RemoteRunID)
			if !found {
				return RemoteTaskResult{}, &api.Error{
					Code:    api.NotFound,
					Message: fmt.Sprintf("remote run %q not found", resolved.RemoteRunID),
				}
			}
			if resolved.RemoteThreadID == "" {
				resolved.RemoteThreadID = run.ThreadID
			}
			if run.Status.Terminal() {
				res := mergeTerminalResult(resolved, run, outputBuf.String(), reasoningBuf.String())
				f.persistTerminal(resolved.LocalTaskID, resolvedTarget, resolved, snap.Sequence, res)
				return res, nil
			}
			outputBuf.Reset()
			outputBuf.WriteString(run.Output)
			reasoningBuf.Reset()
			reasoningBuf.WriteString(run.Reasoning)
			cursor = snap.Sequence
			needResync = false
		}

		subCtx, subCancel := context.WithCancel(ctx)
		sub, err := client.Subscribe(subCtx, cursor)
		if err != nil {
			subCancel()
			if isResyncError(err) {
				needResync = true
				continue
			}
			if ctx.Err() != nil || isDeterministicAPIError(err) {
				f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
				return RemoteTaskResult{}, err
			}
			f.evictClient(key, connID)
			forceRedial = true
			if waitErr := f.waitBackoff(ctx, attempt); waitErr != nil {
				f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
				return RemoteTaskResult{}, waitErr
			}
			attempt++
			continue
		}
		attempt = 0

		// Perform an initial Snapshot check after opening the subscription so a
		// missing run fails immediately and an already-terminal run beyond or at
		// cursor cannot hang.
		var terminalSnapshot *api.Run
		var terminalSnapSeq int64
		snap, snapErr := client.Snapshot(subCtx)
		if snapErr != nil {
			sub.Close()
			subCancel()
			if ctx.Err() != nil || isDeterministicAPIError(snapErr) {
				f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
				return RemoteTaskResult{}, snapErr
			}
			f.evictClient(key, connID)
			forceRedial = true
			if waitErr := f.waitBackoff(ctx, attempt); waitErr != nil {
				f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
				return RemoteTaskResult{}, waitErr
			}
			attempt++
			continue
		}
		run, found := findRun(snap, resolved.RemoteRunID)
		if !found {
			sub.Close()
			subCancel()
			return RemoteTaskResult{}, &api.Error{
				Code:    api.NotFound,
				Message: fmt.Sprintf("remote run %q not found", resolved.RemoteRunID),
			}
		}
		if resolved.RemoteThreadID == "" {
			resolved.RemoteThreadID = run.ThreadID
		}
		if run.Status.Terminal() {
			if cursor >= snap.Sequence {
				sub.Close()
				subCancel()
				res := mergeTerminalResult(resolved, run, outputBuf.String(), reasoningBuf.String())
				f.persistTerminal(resolved.LocalTaskID, resolvedTarget, resolved, max(cursor, snap.Sequence), res)
				return res, nil
			}
			rCopy := run
			terminalSnapshot = &rCopy
			terminalSnapSeq = snap.Sequence
		} else {
			f.persistStatusAndProgress(resolved.LocalTaskID, resolvedTarget, resolved, run.Status, cursor, outputBuf.String(), reasoningBuf.String())
		}

		res, done, resync, fatalErr := f.consumeSubscription(
			ctx,
			subCtx,
			client,
			sub,
			resolved,
			resolvedTarget,
			&cursor,
			&outputBuf,
			&reasoningBuf,
			terminalSnapshot,
			terminalSnapSeq,
			onDelta,
		)
		sub.Close()
		subCancel()

		if done {
			f.touchClient(key, connID)
			return res, nil
		}
		if fatalErr != nil {
			f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
			return RemoteTaskResult{}, fatalErr
		}
		if resync {
			needResync = true
			continue
		}

		// Stream disconnected before observing a terminal event; evict the broken
		// client and redial from the latest sequence cursor.
		f.persistProgress(resolved.LocalTaskID, resolvedTarget, resolved, cursor, outputBuf.String(), reasoningBuf.String())
		f.evictClient(key, connID)
		forceRedial = true
		if waitErr := f.waitBackoff(ctx, attempt); waitErr != nil {
			return RemoteTaskResult{}, waitErr
		}
		attempt++
	}
}

type streamOutputPayload struct {
	Delta  string `json:"delta"`
	Output string `json:"output,omitempty"`
}

type streamReasoningPayload struct {
	ReasoningDelta string `json:"reasoningDelta"`
}

type streamRunPayload struct {
	Run api.Run `json:"run"`
}

func (f *Federator) consumeSubscription(
	callerCtx context.Context,
	subCtx context.Context,
	client PeerClient,
	sub *api.Subscription,
	resolved *RemoteTaskHandle,
	target PeerTarget,
	cursor *int64,
	outputBuf *strings.Builder,
	reasoningBuf *strings.Builder,
	initialTerminal *api.Run,
	initialTerminalSeq int64,
	onDelta func(RemoteStreamDelta),
) (result RemoteTaskResult, done bool, needResync bool, fatalErr error) {
	events := sub.Events
	errs := sub.Errors

	ticker := time.NewTicker(f.reconcileInterval)
	defer ticker.Stop()

	staleTerminalPolls := 0
	var seenTerminal *api.Run
	seenTerminalSeq := initialTerminalSeq
	if initialTerminal != nil {
		cp := *initialTerminal
		seenTerminal = &cp
	}

	for {
		if events == nil && errs == nil {
			if seenTerminal != nil && *cursor >= seenTerminalSeq {
				res := mergeTerminalResult(resolved, *seenTerminal, outputBuf.String(), reasoningBuf.String())
				f.persistTerminal(resolved.LocalTaskID, target, resolved, max(*cursor, seenTerminalSeq), res)
				return res, true, false, nil
			}
			return RemoteTaskResult{}, false, false, nil
		}

		select {
		case <-callerCtx.Done():
			return RemoteTaskResult{}, false, false, callerCtx.Err()

		case <-f.lifecycleCtx.Done():
			return RemoteTaskResult{}, false, false, ErrFederatorClosed

		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.Sequence <= *cursor {
				continue
			}
			*cursor = ev.Sequence
			staleTerminalPolls = 0

			if ev.RunID != resolved.RemoteRunID {
				continue
			}
			switch ev.Type {
			case "run.output":
				var payload streamOutputPayload
				if json.Unmarshal(ev.Data, &payload) == nil && payload.Delta != "" {
					outputBuf.WriteString(payload.Delta)
					if onDelta != nil {
						onDelta(RemoteStreamDelta{
							Sequence:    ev.Sequence,
							OutputDelta: payload.Delta,
						})
					}
				}

			case "run.reasoning":
				var payload streamReasoningPayload
				if json.Unmarshal(ev.Data, &payload) == nil && payload.ReasoningDelta != "" {
					reasoningBuf.WriteString(payload.ReasoningDelta)
					if onDelta != nil {
						onDelta(RemoteStreamDelta{
							Sequence:       ev.Sequence,
							ReasoningDelta: payload.ReasoningDelta,
						})
					}
				}

			case "run.queued", "run.started", "run.waiting_children":
				var payload streamRunPayload
				if json.Unmarshal(ev.Data, &payload) == nil && payload.Run.ID == resolved.RemoteRunID {
					if resolved.RemoteThreadID == "" {
						resolved.RemoteThreadID = payload.Run.ThreadID
					}
					f.persistStatusAndProgress(resolved.LocalTaskID, target, resolved, payload.Run.Status, *cursor, outputBuf.String(), reasoningBuf.String())
					if onDelta != nil {
						onDelta(RemoteStreamDelta{
							Sequence: ev.Sequence,
							Status:   payload.Run.Status,
						})
					}
				}

			case "run.completed", "run.failed", "run.cancelled", "run.interrupted":
				var payload streamRunPayload
				if json.Unmarshal(ev.Data, &payload) == nil && payload.Run.ID == resolved.RemoteRunID {
					res := mergeTerminalResult(resolved, payload.Run, outputBuf.String(), reasoningBuf.String())
					f.persistTerminal(resolved.LocalTaskID, target, resolved, *cursor, res)
					if onDelta != nil {
						onDelta(RemoteStreamDelta{
							Sequence: ev.Sequence,
							Status:   res.Status,
						})
					}
					return res, true, false, nil
				}
			}

		case streamErr, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if streamErr == nil {
				continue
			}
			if callerCtx.Err() != nil {
				return RemoteTaskResult{}, false, false, callerCtx.Err()
			}
			if f.isClosed() {
				return RemoteTaskResult{}, false, false, ErrFederatorClosed
			}
			if isResyncError(streamErr) {
				return RemoteTaskResult{}, false, true, nil
			}
			if isDeterministicAPIError(streamErr) {
				return RemoteTaskResult{}, false, false, streamErr
			}
			return RemoteTaskResult{}, false, false, nil

		case <-ticker.C:
			snapCtx, cancel := context.WithTimeout(subCtx, f.healthTimeout)
			snap, err := client.Snapshot(snapCtx)
			cancel()
			if err != nil {
				if callerCtx.Err() != nil {
					return RemoteTaskResult{}, false, false, callerCtx.Err()
				}
				if isDeterministicAPIError(err) {
					return RemoteTaskResult{}, false, false, err
				}
				// Snapshot failed on this connection; trigger reconnect.
				return RemoteTaskResult{}, false, false, nil
			}
			run, found := findRun(snap, resolved.RemoteRunID)
			if !found {
				return RemoteTaskResult{}, false, false, &api.Error{
					Code:    api.NotFound,
					Message: fmt.Sprintf("remote run %q not found", resolved.RemoteRunID),
				}
			}
			if run.Status.Terminal() {
				cp := run
				seenTerminal = &cp
				seenTerminalSeq = snap.Sequence
				staleTerminalPolls++
				if *cursor >= snap.Sequence || staleTerminalPolls >= 2 {
					res := mergeTerminalResult(resolved, run, outputBuf.String(), reasoningBuf.String())
					f.persistTerminal(resolved.LocalTaskID, target, resolved, max(*cursor, snap.Sequence), res)
					return res, true, false, nil
				}
			}
		}
	}
}

func mergeTerminalResult(handle *RemoteTaskHandle, authoritative api.Run, streamedOutput, streamedReasoning string) RemoteTaskResult {
	output := streamedOutput
	if len(authoritative.Output) > len(output) {
		output = authoritative.Output
	}
	reasoning := streamedReasoning
	if len(authoritative.Reasoning) > len(reasoning) {
		reasoning = authoritative.Reasoning
	}
	return RemoteTaskResult{
		Status:         authoritative.Status,
		Output:         output,
		Reasoning:      reasoning,
		Error:          authoritative.Error,
		RemoteThreadID: firstNonEmpty(authoritative.ThreadID, handle.RemoteThreadID),
		RemoteRunID:    firstNonEmpty(authoritative.ID, handle.RemoteRunID),
	}
}

// CancelRemote sends an idempotent run.stop command to target to terminate the
// remote run associated with handle.
func (f *Federator) CancelRemote(ctx context.Context, target PeerTarget, handle *RemoteTaskHandle) error {
	if ctx == nil {
		return errors.New("environments: nil context")
	}
	if f.isClosed() {
		return ErrFederatorClosed
	}

	resolved, resolvedTarget, err := f.resolveHandleAndTarget(target, handle)
	if err != nil {
		return err
	}
	if resolved.RemoteRunID == "" {
		return &api.Error{
			Code:    api.InvalidCommand,
			Message: "remoteRunId is required to cancel a remote task",
		}
	}

	stopBase := firstNonEmpty(
		strings.TrimSpace(resolved.CommandID),
		prefixIfNonEmpty("fed:", strings.TrimSpace(resolved.RemoteTaskID)),
		prefixIfNonEmpty("fed:", strings.TrimSpace(resolved.LocalTaskID)),
		"fed-run:"+resolved.RemoteRunID,
	)
	stopCmd := api.Command{
		Version:   api.ProtocolVersion,
		CommandID: stopBase + ":run.stop",
		Kind:      api.StopRun,
		ThreadID:  resolved.RemoteThreadID,
		RunID:     resolved.RemoteRunID,
	}

	key := makePeerKey(resolvedTarget)
	var lastErr error
	forceRedial := false

	for attempt := 0; attempt <= defaultDispatchMaxRetries; attempt++ {
		if attempt > 0 {
			if err := f.waitBackoff(ctx, attempt-1); err != nil {
				return err
			}
		}

		client, connID, err := f.acquireClient(ctx, resolvedTarget, forceRedial)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil || errors.Is(err, ErrFederatorClosed) || isDeterministicAPIError(err) {
				return err
			}
			forceRedial = true
			continue
		}
		forceRedial = false

		receipt, err := client.Dispatch(ctx, stopCmd)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil || isDeterministicAPIError(err) {
				return err
			}
			f.evictClient(key, connID)
			forceRedial = true
			continue
		}
		f.touchClient(key, connID)

		if receipt.Sequence > 0 {
			f.persistStatusOnly(resolved.LocalTaskID, resolvedTarget, resolved, api.RunCancelled)
		} else {
			// Run was already terminal on the peer; sync its actual terminal state if needed.
			snapCtx, cancel := context.WithTimeout(ctx, f.healthTimeout)
			if snap, sErr := client.Snapshot(snapCtx); sErr == nil {
				if run, ok := findRun(snap, resolved.RemoteRunID); ok && run.Status.Terminal() {
					res := mergeTerminalResult(resolved, run, resolved.Output, resolved.Reasoning)
					f.persistTerminal(resolved.LocalTaskID, resolvedTarget, resolved, snap.Sequence, res)
				}
			}
			cancel()
		}
		return nil
	}

	return lastErr
}

func (f *Federator) resolveHandleAndTarget(target PeerTarget, handle *RemoteTaskHandle) (*RemoteTaskHandle, PeerTarget, error) {
	if handle == nil {
		return nil, PeerTarget{}, &api.Error{
			Code:    api.InvalidCommand,
			Message: "remote task handle is required",
		}
	}

	resolved := handle.clone()
	localID := strings.TrimSpace(resolved.LocalTaskID)
	if localID != "" {
		if stored, ok := f.GetTask(localID); ok {
			resolved.RemoteTaskID = firstNonEmpty(resolved.RemoteTaskID, stored.RemoteTaskID)
			resolved.CommandID = firstNonEmpty(resolved.CommandID, stored.CommandID)
			resolved.RemoteThreadID = firstNonEmpty(resolved.RemoteThreadID, stored.RemoteThreadID)
			resolved.RemoteRunID = firstNonEmpty(resolved.RemoteRunID, stored.RemoteRunID)
			if resolved.InitialSequence == 0 {
				resolved.InitialSequence = stored.InitialSequence
			}
			if resolved.Cursor == 0 {
				resolved.Cursor = stored.Cursor
			}
			if resolved.Output == "" {
				resolved.Output = stored.Output
			}
			if resolved.Reasoning == "" {
				resolved.Reasoning = stored.Reasoning
			}
			if resolved.Target.SocketPath == "" && resolved.Target.ConnectionID == "" {
				resolved.Target = stored.Target
			}
		}
	}

	effectiveTarget := target
	if strings.TrimSpace(effectiveTarget.SocketPath) == "" && strings.TrimSpace(effectiveTarget.ConnectionID) == "" {
		effectiveTarget = resolved.Target
	}
	if err := ValidateTarget(effectiveTarget); err != nil {
		return nil, PeerTarget{}, err
	}
	effectiveTarget = normalizeTarget(effectiveTarget)
	resolved.Target = effectiveTarget
	resolved.LocalTaskID = localID
	resolved.RemoteThreadID = strings.TrimSpace(resolved.RemoteThreadID)
	resolved.RemoteRunID = strings.TrimSpace(resolved.RemoteRunID)
	return resolved, effectiveTarget, nil
}

func (f *Federator) persistStatusOnly(
	localTaskID string,
	target PeerTarget,
	handle *RemoteTaskHandle,
	status api.RunStatus,
) {
	if strings.TrimSpace(localTaskID) == "" {
		return
	}
	f.ledgerMu.Lock()
	defer f.ledgerMu.Unlock()

	rec := f.ensureTaskRecordLocked(localTaskID, target, handle)
	if !rec.Status.Terminal() || status.Terminal() {
		rec.Status = status
	}
	rec.UpdatedAt = time.Now().UTC()
	_ = f.saveLedgerLocked()
}

func (f *Federator) persistStatusAndProgress(
	localTaskID string,
	target PeerTarget,
	handle *RemoteTaskHandle,
	status api.RunStatus,
	cursor int64,
	output string,
	reasoning string,
) {
	if strings.TrimSpace(localTaskID) == "" {
		return
	}
	f.ledgerMu.Lock()
	defer f.ledgerMu.Unlock()

	rec := f.ensureTaskRecordLocked(localTaskID, target, handle)
	if status != "" && (!rec.Status.Terminal() || status.Terminal()) {
		rec.Status = status
	}
	if cursor >= rec.Cursor {
		rec.Cursor = cursor
		rec.Output = output
		rec.Reasoning = reasoning
	}
	rec.UpdatedAt = time.Now().UTC()
	_ = f.saveLedgerLocked()
}

func (f *Federator) persistProgress(
	localTaskID string,
	target PeerTarget,
	handle *RemoteTaskHandle,
	cursor int64,
	output string,
	reasoning string,
) {
	f.persistStatusAndProgress(localTaskID, target, handle, "", cursor, output, reasoning)
}

func (f *Federator) persistTerminal(
	localTaskID string,
	target PeerTarget,
	handle *RemoteTaskHandle,
	cursor int64,
	res RemoteTaskResult,
) {
	if strings.TrimSpace(localTaskID) == "" {
		return
	}
	f.ledgerMu.Lock()
	defer f.ledgerMu.Unlock()

	rec := f.ensureTaskRecordLocked(localTaskID, target, handle)
	rec.Status = res.Status
	rec.RemoteThreadID = firstNonEmpty(res.RemoteThreadID, rec.RemoteThreadID)
	rec.RemoteRunID = firstNonEmpty(res.RemoteRunID, rec.RemoteRunID)
	rec.Output = res.Output
	rec.Reasoning = res.Reasoning
	rec.Error = res.Error
	if cursor > rec.Cursor {
		rec.Cursor = cursor
	}
	rec.UpdatedAt = time.Now().UTC()
	_ = f.saveLedgerLocked()
}

func (f *Federator) ensureTaskRecordLocked(localTaskID string, target PeerTarget, seed *RemoteTaskHandle) *RemoteTaskHandle {
	rec, ok := f.tasks[localTaskID]
	now := time.Now().UTC()
	if !ok {
		rec = &RemoteTaskHandle{
			LocalTaskID:     localTaskID,
			RemoteTaskID:    seed.RemoteTaskID,
			CommandID:       seed.CommandID,
			Target:          target,
			RemoteThreadID:  seed.RemoteThreadID,
			RemoteRunID:     seed.RemoteRunID,
			Status:          firstStatus(seed.Status, api.RunQueued),
			InitialSequence: seed.InitialSequence,
			Cursor:          seed.Cursor,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		f.tasks[localTaskID] = rec
		return rec
	}
	if target.SocketPath != "" {
		rec.Target = target
	}
	rec.RemoteThreadID = firstNonEmpty(rec.RemoteThreadID, seed.RemoteThreadID)
	rec.RemoteRunID = firstNonEmpty(rec.RemoteRunID, seed.RemoteRunID)
	rec.CommandID = firstNonEmpty(rec.CommandID, seed.CommandID)
	rec.RemoteTaskID = firstNonEmpty(rec.RemoteTaskID, seed.RemoteTaskID)
	return rec
}

func (f *Federator) loadLedger() error {
	if f.ledgerPath == "" {
		return nil
	}
	raw, err := os.ReadFile(f.ledgerPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("environments: read %s: %w", f.ledgerPath, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}

	var disk ledgerDiskPayload
	if err := json.Unmarshal(raw, &disk); err != nil {
		return fmt.Errorf("environments: decode %s: %w", f.ledgerPath, err)
	}
	for id, rec := range disk.Tasks {
		if rec == nil {
			continue
		}
		cp := rec.clone()
		cp.LocalTaskID = firstNonEmpty(strings.TrimSpace(cp.LocalTaskID), strings.TrimSpace(id))
		if cp.LocalTaskID == "" {
			continue
		}
		cp.Target = normalizeTarget(cp.Target)
		f.tasks[cp.LocalTaskID] = cp
	}
	return nil
}

func (f *Federator) saveLedgerLocked() error {
	if f.ledgerPath == "" {
		return nil
	}
	payload := ledgerDiskPayload{
		Version: ledgerFormatVersion,
		Tasks:   f.tasks,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("environments: encode remote task ledger: %w", err)
	}
	encoded = append(encoded, '\n')

	tmpFile, err := os.CreateTemp(f.dataDir, ".remote-tasks-*.tmp")
	if err != nil {
		return fmt.Errorf("environments: create temp ledger: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	if err := tmpFile.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("environments: chmod temp ledger: %w", err)
	}
	if _, err := tmpFile.Write(encoded); err != nil {
		cleanup()
		return fmt.Errorf("environments: write temp ledger: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("environments: sync temp ledger: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("environments: close temp ledger: %w", err)
	}
	if err := os.Rename(tmpPath, f.ledgerPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("environments: commit ledger %s: %w", f.ledgerPath, err)
	}
	if err := os.Chmod(f.ledgerPath, 0o600); err != nil {
		return fmt.Errorf("environments: chmod ledger %s: %w", f.ledgerPath, err)
	}
	return nil
}

func (f *Federator) waitBackoff(ctx context.Context, attempt int) error {
	delay := f.reconnectBackoff
	for i := 0; i < attempt && delay < maxReconnectBackoff; i++ {
		delay *= 2
		if delay > maxReconnectBackoff {
			delay = maxReconnectBackoff
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.lifecycleCtx.Done():
		return ErrFederatorClosed
	case <-timer.C:
		return nil
	}
}

func findRun(snap api.Snapshot, runID string) (api.Run, bool) {
	for _, r := range snap.Runs {
		if r.ID == runID {
			return r, true
		}
	}
	return api.Run{}, false
}

func isResyncError(err error) bool {
	var apiErr *api.Error
	return errors.As(err, &apiErr) && apiErr.Code == api.ResyncRequired
}

func isDeterministicAPIError(err error) bool {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case api.InvalidCommand, api.VersionMismatch, api.Conflict, api.NotFound:
		return true
	default:
		return false
	}
}

func cloneWorkspace(ws *api.WorkspaceSpec) *api.WorkspaceSpec {
	if ws == nil {
		return nil
	}
	cp := *ws
	return &cp
}

func cloneAttachments(in []api.Attachment) []api.Attachment {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.Attachment, len(in))
	copy(out, in)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstStatus(values ...api.RunStatus) api.RunStatus {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func prefixIfNonEmpty(prefix, val string) string {
	if val == "" {
		return ""
	}
	return prefix + val
}
