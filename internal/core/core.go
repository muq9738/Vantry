// Package core implements Vantry's first orchestration core: a single-writer
// SQLite service that commits events, projections, command receipts and outbox
// effects in one transaction, then executes provider effects outside any
// transaction.
package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muq9738/Vantry/internal/telemetry"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

const (
	defaultEventRetention    = 5000
	defaultEventPruneInterval = 5 * time.Minute
)

// Core lifecycle: starting → recovering → ready → draining → stopped.
const (
	stateStarting int32 = iota
	stateRecovering
	stateReady
	stateDraining
	stateStopped
)

func stateName(state int32) string {
	switch state {
	case stateStarting:
		return "starting"
	case stateRecovering:
		return "recovering"
	case stateReady:
		return "ready"
	case stateDraining:
		return "draining"
	case stateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Options configures Open. The database file must not be shared with another
// live Core instance.
type Options struct {
	DBPath   string
	Provider provider.Provider

	// DefaultProviderID, DefaultModel and DefaultWorkspace resolve empty fields
	// of thread.create into a durable thread configuration. They are applied when
	// the entity is built, never by rewriting the command, so a later change of
	// the defaults cannot break the retry of an already accepted command.
	DefaultProviderID string
	DefaultModel      string
	DefaultWorkspace  *api.WorkspaceSpec

	// ValidateAttachments checks Core-owned attachment identities before they are
	// recorded or handed to a provider. Nil means attachments are unsupported.
	ValidateAttachments func([]api.Attachment) error

	// ForkSession optionally clones provider-side session state after thread.fork.
	ForkSession func(sourceThreadID, targetThreadID string, completedRuns int) error

	// RewindSession optionally truncates provider-side session state after thread.rewind.
	RewindSession func(threadID string, keepCompletedRuns int) error

	// TelemetryDir persists the token/cost usage ledger under <TelemetryDir>/usage-ledger.json.
	// Empty uses an in-memory ledger.
	TelemetryDir string

	// EventRetention keeps at least this many recent events when periodic
	// pruning runs. Zero defaults to defaultEventRetention; negative disables.
	EventRetention     int
	EventPruneInterval time.Duration
}

// Core is the concrete api.Service implementation. It is safe for concurrent
// use by multiple transport goroutines.
type Core struct {
	provider provider.Provider
	store    *store
	hub      *subHub

	// Defaults resolved into new threads; they never mutate a command payload.
	defaultProviderID   string
	defaultModel        string
	defaultWorkspace    *api.WorkspaceSpec
	validateAttachments func([]api.Attachment) error
	forkSession         func(string, string, int) error
	rewindSession       func(string, int) error

	catalogMu            sync.RWMutex
	modelCatalogProvider func(context.Context, string, string) (api.ModelCatalog, error)

	ledger             *telemetry.Ledger
	telemetryMu        sync.RWMutex
	liveTelemetry      map[string]*runTelemetryState
	eventRetention     int
	eventPruneInterval time.Duration

	state atomic.Int32

	// wmu is the single write coordinator: every committed write and its
	// post-commit publish happen inside one critical section, so commit order and
	// publish order are identical and SQLite never sees concurrent writers.
	wmu     sync.Mutex
	closing bool // guarded by wmu, set once by Close before the database closes

	// lifeMu lets Close wait for in-flight reads (including subscription replay)
	// before the database handle is closed.
	lifeMu sync.RWMutex

	workCtx    context.Context
	workCancel context.CancelFunc
	workWG     sync.WaitGroup
	wake       chan struct{}

	stopMu     sync.Mutex // serializes delivery of persistent provider stop intents
	runMu      sync.Mutex
	runCancels map[string]context.CancelFunc

	closeOnce sync.Once
	closeErr  error

	// Internal tuning knobs used by tests. They are not part of the api contract.
	subBufferSize     int
	shutdownGrace     time.Duration
	testHookPreCommit func(*sql.Tx) error
}

var _ api.Service = (*Core)(nil)

// Open prepares the database, migrates it, recovers old active runs and starts
// the effect worker. Open returns only once the instance is ready to accept
// commands; recovery never replays old user messages.
func Open(ctx context.Context, opts Options) (*Core, error) {
	if opts.Provider == nil {
		return nil, fmt.Errorf("core: Provider is required")
	}
	st, err := openStore(ctx, opts.DBPath)
	if err != nil {
		return nil, err
	}

	workCtx, workCancel := context.WithCancel(context.Background())
	ledger, err := telemetry.NewLedger(opts.TelemetryDir)
	if err != nil {
		ledger, _ = telemetry.NewLedger("")
	}
	retention := opts.EventRetention
	if retention == 0 {
		retention = defaultEventRetention
	}
	pruneInterval := opts.EventPruneInterval
	if pruneInterval <= 0 {
		pruneInterval = defaultEventPruneInterval
	}
	c := &Core{
		provider:            opts.Provider,
		defaultProviderID:   opts.DefaultProviderID,
		defaultModel:        opts.DefaultModel,
		defaultWorkspace:    opts.DefaultWorkspace,
		validateAttachments: opts.ValidateAttachments,
		forkSession:         opts.ForkSession,
		rewindSession:       opts.RewindSession,
		ledger:              ledger,
		liveTelemetry:       make(map[string]*runTelemetryState),
		eventRetention:      retention,
		eventPruneInterval:  pruneInterval,
		store:               st,
		workCtx:             workCtx,
		workCancel:          workCancel,
		wake:                make(chan struct{}, 1),
		runCancels:          make(map[string]context.CancelFunc),
		subBufferSize:       defaultSubscriberBuffer,
		shutdownGrace:       defaultShutdownGrace,
	}
	c.hub = newSubHub(c)
	c.state.Store(stateStarting)

	c.state.Store(stateRecovering)
	if err := c.recover(ctx); err != nil {
		workCancel()
		st.Close()
		return nil, err
	}

	c.state.Store(stateReady)
	c.workWG.Add(1)
	go func() {
		defer c.workWG.Done()
		c.workerLoop(workCtx)
	}()
	// Anything left pending from recovery (there is none in the first
	// milestone) would be picked up here without replaying user messages.
	c.notifyWorker()
	return c, nil
}

// Close drains the core. It stops accepting writes, cancels running providers,
// waits a bounded time for them, records still-active runs as interrupted and
// only then closes the database. A provider that ignores cancellation cannot
// keep Close waiting, and its late callbacks are rejected before they touch the
// closed handle.
func (c *Core) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.close() })
	return c.closeErr
}

func (c *Core) close() error {
	c.state.Store(stateDraining)

	// Stop claiming new effects.
	c.workCancel()
	c.cancelAllRuns()

	grace := c.shutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	done := make(chan struct{})
	go func() {
		c.workWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		// The provider ignored cancellation. Close continues: the run is resolved as
		// interrupted below, so a wedged provider cannot keep Close waiting.
	}

	// Record whatever is still active; this is the same resolution a restart
	// would apply, so a graceful stop and a crash converge on the same state.
	settleCtx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	err := c.interruptActiveRuns(settleCtx)
	cancel()

	// Close subscriptions before the database handle disappears.
	c.hub.closeAll()

	c.lifeMu.Lock()
	c.wmu.Lock()
	c.closing = true
	c.wmu.Unlock()
	dbErr := c.store.Close()
	c.state.Store(stateStopped)
	c.lifeMu.Unlock()

	switch {
	case err != nil && !errors.Is(err, errStoreClosing):
		return err
	case dbErr != nil:
		return fmt.Errorf("core: close database: %w", dbErr)
	}
	return nil
}

// Health reports the lifecycle state. It stays answerable after Close so a
// transport can distinguish "stopped" from "unreachable".
// Health reports protocol compatibility and readiness.
func (c *Core) UsageLedger() *telemetry.Ledger { return c.ledger }

func (c *Core) Health(context.Context) (api.Health, error) {
	return api.Health{Version: api.ProtocolVersion, State: stateName(c.state.Load())}, nil
}

// Snapshot returns the committed projection plus the event sequence it is
// consistent with. Subscribing at that sequence yields no gap.
func (c *Core) Snapshot(ctx context.Context) (api.Snapshot, error) {
	c.lifeMu.RLock()
	defer c.lifeMu.RUnlock()
	if c.state.Load() == stateStopped {
		return api.Snapshot{}, errorf(api.NotReady, "core is stopped")
	}

	tx, err := c.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.Snapshot{}, wrapInternal("begin snapshot", err)
	}
	defer tx.Rollback()

	var snapshot api.Snapshot
	if err := tx.QueryRowContext(ctx, `SELECT MAX(COALESCE(MAX(sequence), 0),
		(SELECT int_value FROM core_metadata WHERE key = 'pruned_through_sequence')) FROM events`).Scan(&snapshot.Sequence); err != nil {
		return api.Snapshot{}, wrapInternal("read event sequence", err)
	}

	threadRows, err := tx.QueryContext(ctx, `SELECT `+threadColumns+` FROM threads ORDER BY created_at, id`)
	if err != nil {
		return api.Snapshot{}, wrapInternal("read threads", err)
	}
	defer threadRows.Close()
	for threadRows.Next() {
		rec, err := scanThread(threadRows)
		if err != nil {
			return api.Snapshot{}, wrapInternal("scan thread", err)
		}
		snapshot.Threads = append(snapshot.Threads, rec.toAPI())
	}
	if err := threadRows.Err(); err != nil {
		return api.Snapshot{}, wrapInternal("read threads", err)
	}

	runRows, err := tx.QueryContext(ctx, `SELECT `+runColumns+` FROM runs ORDER BY created_at, id`)
	if err != nil {
		return api.Snapshot{}, wrapInternal("read runs", err)
	}
	defer runRows.Close()
	for runRows.Next() {
		rec, err := scanRun(runRows)
		if err != nil {
			return api.Snapshot{}, wrapInternal("scan run", err)
		}
		apiRun := rec.toAPI()
		apiRun.Telemetry = c.runTelemetrySnapshot(apiRun.ID)
		snapshot.Runs = append(snapshot.Runs, apiRun)
	}
	if err := runRows.Err(); err != nil {
		return api.Snapshot{}, wrapInternal("read runs", err)
	}

	taskRows, err := tx.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks ORDER BY created_at, id`)
	if err != nil {
		return api.Snapshot{}, wrapInternal("read tasks", err)
	}
	defer taskRows.Close()
	for taskRows.Next() {
		rec, err := scanTask(taskRows)
		if err != nil {
			return api.Snapshot{}, wrapInternal("scan task", err)
		}
		snapshot.Tasks = append(snapshot.Tasks, rec.toAPI())
	}
	if err := taskRows.Err(); err != nil {
		return api.Snapshot{}, wrapInternal("read tasks", err)
	}

	cohortRows, err := tx.QueryContext(ctx, `SELECT `+cohortColumns+` FROM cohorts
		ORDER BY created_at, parent_run_id, generation`)
	if err != nil {
		return api.Snapshot{}, wrapInternal("read cohorts", err)
	}
	defer cohortRows.Close()
	for cohortRows.Next() {
		rec, err := scanCohort(cohortRows)
		if err != nil {
			return api.Snapshot{}, wrapInternal("scan cohort", err)
		}
		snapshot.Cohorts = append(snapshot.Cohorts, rec.toAPI())
	}
	if err := cohortRows.Err(); err != nil {
		return api.Snapshot{}, wrapInternal("read cohorts", err)
	}
	inputRows, err := tx.QueryContext(ctx, `SELECT `+inputColumns+` FROM inputs ORDER BY created_at, id`)
	if err != nil {
		return api.Snapshot{}, wrapInternal("read inputs", err)
	}
	defer inputRows.Close()
	for inputRows.Next() {
		rec, err := scanInput(inputRows)
		if err != nil {
			return api.Snapshot{}, wrapInternal("scan input", err)
		}
		snapshot.Inputs = append(snapshot.Inputs, rec.toAPI())
		// Legacy views of the same authority. They add no state of their own.
		switch rec.Kind {
		case inputKindQueue:
			snapshot.Messages = append(snapshot.Messages, rec.toQueuedMessage())
		case inputKindSteer:
			snapshot.Steering = append(snapshot.Steering, rec.toSteeringMessage())
		}
	}
	if err := inputRows.Err(); err != nil {
		return api.Snapshot{}, wrapInternal("read inputs", err)
	}

	toolRows, err := tx.QueryContext(ctx, `SELECT `+toolColumns+` FROM tool_calls ORDER BY updated_at, id`)
	if err != nil {
		return api.Snapshot{}, wrapInternal("read tool calls", err)
	}
	defer toolRows.Close()
	for toolRows.Next() {
		rec, err := scanTool(toolRows)
		if err != nil {
			return api.Snapshot{}, wrapInternal("scan tool call", err)
		}
		snapshot.Tools = append(snapshot.Tools, rec.toAPI())
	}
	if err := toolRows.Err(); err != nil {
		return api.Snapshot{}, wrapInternal("read tool calls", err)
	}

	requestRows, err := tx.QueryContext(ctx, `SELECT `+requestColumns+` FROM provider_requests ORDER BY created_at, id`)
	if err != nil {
		return api.Snapshot{}, wrapInternal("read provider requests", err)
	}
	defer requestRows.Close()
	for requestRows.Next() {
		rec, err := scanRequest(requestRows)
		if err != nil {
			return api.Snapshot{}, wrapInternal("scan provider request", err)
		}
		snapshot.Requests = append(snapshot.Requests, rec.toAPI())
	}
	if err := requestRows.Err(); err != nil {
		return api.Snapshot{}, wrapInternal("read provider requests", err)
	}

	if snapshot.Threads == nil {
		snapshot.Threads = []api.Thread{}
	}
	if snapshot.Runs == nil {
		snapshot.Runs = []api.Run{}
	}
	if snapshot.Tasks == nil {
		snapshot.Tasks = []api.Task{}
	}
	if snapshot.Cohorts == nil {
		snapshot.Cohorts = []api.Cohort{}
	}
	if snapshot.Inputs == nil {
		snapshot.Inputs = []api.Input{}
	}
	if snapshot.Tools == nil {
		snapshot.Tools = []api.ToolCall{}
	}
	if snapshot.Requests == nil {
		snapshot.Requests = []api.ProviderRequest{}
	}
	return snapshot, nil
}

// write is the only path to a committed write. It holds the single write
// coordinator for the transaction, and publishes the events produced by that
// transaction after a successful commit but still inside the same critical
// section — so subscribers observe events in commit order with no window in
// which a committed event is missing from both the log and the live path.
func (c *Core) write(ctx context.Context, fn func(*txn) error) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closing {
		return errStoreClosing
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("core: write aborted: %w", err)
	}
	// The transaction runs on a non-cancellable context. database/sql aborts a
	// transaction whose context is cancelled asynchronously, which can race our
	// own Commit/Rollback on the reserved writer connection and leave the driver
	// with a half-open transaction. A write that already started is short and
	// bounded; the conditional checks inside it decide whether it may commit.
	txCtx := context.WithoutCancel(ctx)
	tx, err := c.store.writer.BeginTx(txCtx, nil)
	if err != nil {
		return wrapInternal("begin transaction", err)
	}
	defer tx.Rollback()

	t := &txn{ctx: txCtx, tx: tx}
	if err := fn(t); err != nil {
		return err
	}
	if hook := c.testHookPreCommit; hook != nil {
		if err := hook(tx); err != nil {
			return fmt.Errorf("core: injected failure: %w", err)
		}
	}
	// A command whose caller gave up must not commit: the receipt would claim an
	// acceptance the client never learned about. The transaction is short and
	// already open, so checking here (and rolling back) is enough.
	if err := ctx.Err(); err != nil {
		return wrapInternal("commit transaction", err)
	}
	if err := tx.Commit(); err != nil {
		return wrapInternal("commit transaction", err)
	}
	for _, ev := range t.events {
		c.hub.publish(ev)
	}
	return nil
}

func (c *Core) notifyWorker() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// watchRun registers the cancel function that provider cancellation uses. It
// returns false when the core is already draining, in which case the caller
// must not start provider work.
func (c *Core) watchRun(runID string, cancel context.CancelFunc) bool {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.state.Load() >= stateDraining {
		return false
	}
	c.runCancels[runID] = cancel
	return true
}

func (c *Core) unwatchRun(runID string) {
	c.runMu.Lock()
	delete(c.runCancels, runID)
	c.runMu.Unlock()
}

func (c *Core) cancelRun(runID string) {
	c.runMu.Lock()
	cancel := c.runCancels[runID]
	c.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Core) cancelAllRuns() {
	c.runMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.runCancels))
	for _, cancel := range c.runCancels {
		cancels = append(cancels, cancel)
	}
	c.runMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func errorf(code api.ErrorCode, format string, args ...any) *api.Error {
	return &api.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// wrapInternal keeps cancellation recognisable. A transaction that was aborted
// because its context was cancelled is not a provider failure and must not be
// reported as an internal error: callers decide between interrupted and failed
// by inspecting the returned error.
func wrapInternal(what string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &api.Error{Code: api.InternalError, Message: what + ": " + err.Error()}
}
