package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"

	_ "modernc.org/sqlite"
)

// schemaVersion is the last migration owned by this package. Migrations are
// applied in order on Open before any other database access happens.
//
// Two integration branches published schemaVersion=3 with different contents
// (one added approval policy, inputs, tools and provider requests; the other
// added pinned/archived threads, run attachments, queued messages and a v4
// steering table). The merged schema therefore never trusts user_version when
// deciding what a v3 database already contains: the seeding migrations below are
// version-gated, and everything after them is guarded by the tables and columns
// that actually exist.
const schemaVersion = 11

// SchemaVersionError prevents an older build from modifying a newer database.
type SchemaVersionError struct {
	Version   int
	Supported int
}

func (e *SchemaVersionError) Error() string {
	return fmt.Sprintf("core: database schema %d is newer than supported %d；请使用支持该数据库版本的 Vantry，或恢复升级前的数据库备份；重新安装旧版应用不会降级数据库", e.Version, e.Supported)
}

// migrationV1 is the complete first-milestone schema. Subscriptions replay the
// retained event log; core_metadata records the historical pruning boundary.
const migrationV1 = `
CREATE TABLE threads (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE runs (
	id                TEXT PRIMARY KEY,
	thread_id         TEXT NOT NULL REFERENCES threads(id),
	attempt_id        TEXT NOT NULL,
	active_attempt_id TEXT NOT NULL,
	status            TEXT NOT NULL,
	input             TEXT NOT NULL,
	output            TEXT NOT NULL DEFAULT '',
	error             TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL
);

-- A thread has at most one queued/running run. This is the durable backstop for
-- the in-transaction check done by message.send.
CREATE UNIQUE INDEX runs_one_active_per_thread
	ON runs(thread_id) WHERE status IN ('queued', 'running');
CREATE INDEX runs_by_thread ON runs(thread_id, created_at, id);

CREATE TABLE attempts (
	id         TEXT PRIMARY KEY,
	run_id     TEXT NOT NULL REFERENCES runs(id),
	ordinal    INTEGER NOT NULL,
	status     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX attempts_ordinal ON attempts(run_id, ordinal);

CREATE TABLE command_receipts (
	command_id  TEXT PRIMARY KEY,
	kind        TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	thread_id   TEXT NOT NULL,
	run_id      TEXT NOT NULL DEFAULT '',
	sequence    INTEGER NOT NULL,
	accepted    INTEGER NOT NULL,
	created_at  INTEGER NOT NULL
);

CREATE TABLE events (
	sequence   INTEGER PRIMARY KEY AUTOINCREMENT,
	id         TEXT NOT NULL,
	type       TEXT NOT NULL,
	thread_id  TEXT NOT NULL,
	run_id     TEXT NOT NULL DEFAULT '',
	attempt_id TEXT NOT NULL DEFAULT '',
	timestamp  INTEGER NOT NULL,
	data       BLOB NOT NULL
);

CREATE TABLE effect_outbox (
	id            TEXT PRIMARY KEY,
	kind          TEXT NOT NULL,
	thread_id     TEXT NOT NULL,
	run_id        TEXT NOT NULL,
	attempt_id    TEXT NOT NULL,
	status        TEXT NOT NULL,
	attempt_count INTEGER NOT NULL DEFAULT 0,
	claim_token   TEXT NOT NULL DEFAULT '',
	lease_until   INTEGER NOT NULL DEFAULT 0,
	last_error    TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL
);
CREATE INDEX effect_outbox_by_status ON effect_outbox(status, created_at, id);
`

// migrationV2 adds task/cohort projections, the durable provider cursor, the
// per-attempt input, thread/run configuration and the command payload used to
// verify a repeated commandId against the actual command instead of a digest.
// Rows written by schema 1 keep an empty payload: they cannot be verified and a
// repeated commandId is refused instead of being re-executed.
const migrationV2 = `
ALTER TABLE threads ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'controlled';
ALTER TABLE threads ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN workspace TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN parent_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN parent_task_id TEXT NOT NULL DEFAULT '';

ALTER TABLE runs ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'controlled';
ALTER TABLE runs ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN workspace TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN parent_task_id TEXT NOT NULL DEFAULT '';

ALTER TABLE attempts ADD COLUMN input TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN provider_cursor INTEGER NOT NULL DEFAULT 0;

ALTER TABLE effect_outbox ADD COLUMN input TEXT NOT NULL DEFAULT '';

UPDATE attempts SET input = COALESCE((SELECT r.input FROM runs r WHERE r.id = attempts.run_id), '')
	WHERE input = '';
UPDATE effect_outbox SET input = COALESCE((SELECT r.input FROM runs r WHERE r.id = effect_outbox.run_id), '')
	WHERE input = '';

ALTER TABLE command_receipts ADD COLUMN payload TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN task_id TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE command_receipts ADD COLUMN delivery_message_id TEXT NOT NULL DEFAULT '';

DROP INDEX runs_one_active_per_thread;
CREATE UNIQUE INDEX runs_one_active_per_thread
	ON runs(thread_id) WHERE status IN ('queued', 'running', 'waiting_children');
CREATE INDEX runs_by_parent_task ON runs(parent_task_id) WHERE parent_task_id <> '';
CREATE INDEX threads_by_parent_task ON threads(parent_task_id) WHERE parent_task_id <> '';

CREATE TABLE tasks (
	id                  TEXT PRIMARY KEY,
	parent_thread_id    TEXT NOT NULL,
	parent_run_id       TEXT NOT NULL REFERENCES runs(id),
	parent_attempt_id   TEXT NOT NULL DEFAULT '',
	child_thread_id     TEXT NOT NULL,
	child_run_id        TEXT NOT NULL,
	title               TEXT NOT NULL,
	generation          INTEGER NOT NULL,
	status              TEXT NOT NULL,
	delivery_status     TEXT NOT NULL,
	delivery_message_id TEXT NOT NULL DEFAULT '',
	result              TEXT NOT NULL DEFAULT '',
	error               TEXT NOT NULL DEFAULT '',
	created_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL
);
CREATE INDEX tasks_by_parent_run ON tasks(parent_run_id, generation, created_at, id);
CREATE UNIQUE INDEX tasks_by_child_thread ON tasks(child_thread_id);
CREATE INDEX tasks_by_child_run ON tasks(child_run_id);

CREATE TABLE cohorts (
	parent_run_id           TEXT NOT NULL REFERENCES runs(id),
	generation              INTEGER NOT NULL,
	resume_policy           TEXT NOT NULL,
	delivery_message_id     TEXT NOT NULL,
	status                  TEXT NOT NULL,
	continuation_attempt_id TEXT NOT NULL DEFAULT '',
	created_at              INTEGER NOT NULL,
	updated_at              INTEGER NOT NULL,
	PRIMARY KEY (parent_run_id, generation)
);
CREATE UNIQUE INDEX cohorts_delivery_message ON cohorts(delivery_message_id);
CREATE INDEX cohorts_by_status ON cohorts(status, created_at);
`

// migrationV3 is the canonical merged v3 schema. Both branches' workbench
// projections live here: the durable input FIFO, real tool-call and dialog
// projections, the approval policy inherited by threads, runs and requests, the
// input/request/message identity of a command receipt, and the thread
// management flags with run attachments of the local branch.
const migrationV3 = `
ALTER TABLE threads ADD COLUMN approval_policy TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE threads ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN approval_policy TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE runs ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]';

ALTER TABLE command_receipts ADD COLUMN input_id TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN message_id TEXT NOT NULL DEFAULT '';

-- inputs is the single durable authority for queued and steering messages. A
-- queue input becomes a Run when the thread slot is free; a steer input is a
-- control intent addressed to exactly one attempt.
CREATE TABLE inputs (
	id          TEXT PRIMARY KEY,
	thread_id   TEXT NOT NULL,
	run_id      TEXT NOT NULL DEFAULT '',
	attempt_id  TEXT NOT NULL DEFAULT '',
	kind        TEXT NOT NULL,
	message     TEXT NOT NULL,
	attachments TEXT NOT NULL DEFAULT '[]',
	command_id  TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL,
	disposition TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX inputs_by_thread ON inputs(thread_id, kind, status, created_at, id);
CREATE INDEX inputs_by_attempt ON inputs(attempt_id, kind, status);

-- tool_calls mirrors real Pi tool executions; output is bounded display text.
CREATE TABLE tool_calls (
	id         TEXT PRIMARY KEY,
	thread_id  TEXT NOT NULL,
	run_id     TEXT NOT NULL,
	attempt_id TEXT NOT NULL,
	name       TEXT NOT NULL,
	arguments  BLOB NOT NULL DEFAULT '',
	output     TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX tool_calls_by_attempt ON tool_calls(attempt_id, status, updated_at);

-- provider_requests records Pi dialogs that block the model until answered.
CREATE TABLE provider_requests (
	id                  TEXT PRIMARY KEY,
	provider_request_id TEXT NOT NULL,
	thread_id           TEXT NOT NULL,
	run_id              TEXT NOT NULL,
	attempt_id          TEXT NOT NULL,
	method              TEXT NOT NULL,
	title               TEXT NOT NULL,
	message             TEXT NOT NULL DEFAULT '',
	options             TEXT NOT NULL DEFAULT '',
	prefill             TEXT NOT NULL DEFAULT '',
	status              TEXT NOT NULL,
	error               TEXT NOT NULL DEFAULT '',
	expires_at          INTEGER NOT NULL DEFAULT 0,
	created_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL
);
CREATE UNIQUE INDEX provider_requests_provider ON provider_requests(attempt_id, provider_request_id);
CREATE INDEX provider_requests_by_attempt ON provider_requests(attempt_id, status);
CREATE INDEX provider_requests_by_status ON provider_requests(status, expires_at);
`

// migrationV4 is the pre-merge local steering projection. It is kept so a
// database written by that branch keeps its history; the merged Core only reads
// it once, to project its rows into inputs.
const migrationV4 = `
CREATE TABLE IF NOT EXISTS steering_messages (
 id TEXT PRIMARY KEY,
 command_id TEXT NOT NULL UNIQUE,
 thread_id TEXT NOT NULL,
 run_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL,
 message TEXT NOT NULL,
 attachments TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL,
 disposition TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS steering_by_status ON steering_messages(status, created_at, id);
`

// migrationV5 holds the pre-merge local queue projection (queued_messages). It
// is likewise retained for traceability only.
const migrationV5 = `
CREATE TABLE IF NOT EXISTS queued_messages (
 id TEXT PRIMARY KEY,
 thread_id TEXT NOT NULL,
 message TEXT NOT NULL,
 attachments TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL,
 run_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_pending ON queued_messages(status, created_at, id);
`

// migrationV6 places a reply's parts in order: the reasoning the model showed
// and, for each tool call, the length of the reply when it started, so a client
// interleaves text and tool work as they happened. Rows from before it keep -1
// and show after the reply, as they did.
const migrationV6 = `
ALTER TABLE runs ADD COLUMN reasoning TEXT NOT NULL DEFAULT '';
ALTER TABLE tool_calls ADD COLUMN output_offset INTEGER NOT NULL DEFAULT -1;
`

const migrationV7 = `
ALTER TABLE inputs ADD COLUMN configuration TEXT NOT NULL DEFAULT '';
`

// A negative queue_order identifies historical rows that still need their FIFO
// position assigned, including legacy queue rows projected on a later open.
const migrationV8 = `
ALTER TABLE threads ADD COLUMN interaction_mode TEXT NOT NULL DEFAULT 'default';
ALTER TABLE threads ADD COLUMN forked_from_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN forked_from_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN interaction_mode TEXT NOT NULL DEFAULT 'default';
ALTER TABLE inputs ADD COLUMN queue_order INTEGER NOT NULL DEFAULT -1;
CREATE INDEX inputs_queue_order ON inputs(thread_id, kind, status, queue_order, created_at, id);
`

const migrationV9 = `
ALTER TABLE threads ADD COLUMN snoozed_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN created_by_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN created_by_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN sender_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE inputs ADD COLUMN sender_thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN delivery TEXT NOT NULL DEFAULT '';
`

const migrationV10 = `
ALTER TABLE threads ADD COLUMN project_path TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX project_coordinator_path ON threads(project_path) WHERE project_path <> '';
CREATE TABLE project_updates (
 id TEXT PRIMARY KEY,
 source_sequence INTEGER NOT NULL UNIQUE,
 thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
 source_thread_id TEXT NOT NULL,
 run_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL DEFAULT '',
 request_id TEXT NOT NULL DEFAULT '',
 title TEXT NOT NULL,
 status TEXT NOT NULL,
 summary TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX project_updates_thread ON project_updates(thread_id, source_sequence);
CREATE UNIQUE INDEX project_updates_milestone ON project_updates(thread_id, run_id, attempt_id, status, request_id);
`

const migrationV11 = `
CREATE TABLE thread_watches (
 id TEXT PRIMARY KEY,
 watcher_thread_id TEXT NOT NULL,
 watcher_run_id TEXT NOT NULL DEFAULT '',
 target_thread_id TEXT NOT NULL,
 target_run_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX thread_watches_by_target ON thread_watches(target_thread_id, target_run_id, status);
CREATE UNIQUE INDEX thread_watches_unique_pending ON thread_watches(watcher_thread_id, target_thread_id, target_run_id) WHERE status = 'pending';
`

// seedMigrations are the only sequential migrations. Everything from v3 on is
// applied by ensureUnifiedSchema, which inspects the real database.
var seedMigrations = []string{migrationV1, migrationV2}

// Event types committed by this package. Run events carry {run: ...}, task
// events carry {task: ...} and cohort events carry {cohort: ...}.
const (
	eventThreadCreated  = "thread.created"
	eventThreadDeleted  = "thread.deleted"
	eventThreadRewound  = "thread.rewound"
	eventRunQueued      = "run.queued"
	eventRunStarted     = "run.started"
	eventRunOutput      = "run.output"
	eventRunReasoning   = "run.reasoning"
	eventRunTelemetry   = "run.telemetry"
	eventRunWaiting     = "run.waiting_children"
	eventRunCompleted   = "run.completed"
	eventRunCancelled   = "run.cancelled"
	eventRunInterrupted = "run.interrupted"
	eventRunFailed      = "run.failed"
	eventTaskCreated    = "task.created"
	eventTaskUpdated    = "task.updated"
	eventCohortCreated  = "cohort.created"
	eventCohortUpdated  = "cohort.updated"
	eventInputUpdated   = "input.updated"
	eventToolUpdated    = "tool.updated"
	eventRequestUpdated = "request.updated"
)

// Run statuses stored in the projections mirror api.RunStatus.
const (
	statusQueued          = string(api.RunQueued)
	statusRunning         = string(api.RunRunning)
	statusWaitingChildren = string(api.RunWaiting)
	statusCompleted       = string(api.RunCompleted)
	statusCancelled       = string(api.RunCancelled)
	statusInterrupted     = string(api.RunInterrupted)
	statusFailed          = string(api.RunFailed)
)

// Delivery statuses and resume policies mirror pkg/api.
const (
	deliveryPending      = string(api.DeliveryPending)
	deliveryDelivered    = string(api.DeliveryDelivered)
	deliveryAcknowledged = string(api.DeliveryAcknowledged)
	deliveryDisposed     = string(api.DeliveryDisposed)

	resumeAuto   = "auto"
	resumeManual = "manual"
)

// Outbox statuses.
const (
	outboxPending   = "pending"
	outboxClaimed   = "claimed"
	outboxSettled   = "settled"
	outboxCancelled = "cancelled"
	outboxAbandoned = "abandoned"
)

const (
	effectKindProviderRun     = "provider.run"
	effectKindProviderStop    = "provider.stop"
	effectKindProviderControl = "provider.control"

	runColumns = `id, thread_id, attempt_id, active_attempt_id, status, input, output, error, created_at, updated_at,
		provider_id, model, workspace, parent_task_id, approval_policy, attachments, reasoning, interaction_mode, sender_thread_id`
	threadColumns = `id, title, created_at, provider_id, model, workspace, parent_thread_id, parent_task_id,
		approval_policy, pinned, archived, interaction_mode, forked_from_thread_id, forked_from_run_id,
		snoozed_until, created_by_thread_id, created_by_run_id, project_path`
)

// Approval policies. auto keeps the historical behaviour (no provider dialog);
// ask enables the explicit confirmation extension for bash/write/edit.
const (
	approvalAuto        = "auto"
	approvalAcceptEdits = "auto-accept-edits"
	approvalAsk         = "ask"
)

func normalizeApprovalPolicy(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", approvalAuto:
		return approvalAuto, nil
	case approvalAcceptEdits:
		return approvalAcceptEdits, nil
	case approvalAsk:
		return approvalAsk, nil
	default:
		return "", errorf(api.InvalidCommand, "approvalPolicy %q is not supported", value)
	}
}

const interactionDefault = "default"

func normalizeInteractionMode(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "", interactionDefault:
		return interactionDefault, nil
	case "plan":
		return "plan", nil
	default:
		return "", errorf(api.InvalidCommand, "interactionMode %q is not supported", value)
	}
}

// store owns the SQLite handle. All writes are serialised by Core through a
// single write critical section; readers use WAL snapshots.
type store struct {
	db        *sql.DB
	writer    *sql.Conn
	searchFTS bool
	directory string
}

func openStore(ctx context.Context, path string) (*store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("core: DBPath is required")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("core: create database directory: %w", err)
		}
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("core: resolve database path: %w", err)
	}
	// These settings belong to each physical SQLite connection. Passing them
	// through the driver's DSN applies them to new pooled handles as well.
	db, err := sql.Open("sqlite", sqliteDSN(absolutePath))
	if err != nil {
		return nil, fmt.Errorf("core: open database: %w", err)
	}
	// WAL keeps readers concurrent with the single writer. Busy timeout is the
	// safety net for the rare second handle (a crashed process still shutting
	// down), not the primary coordination mechanism.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	// Reject a newer database before changing persistent journal settings.
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("core: read schema version: %w", err)
	}
	if version > schemaVersion {
		db.Close()
		return nil, fmt.Errorf("数据库 %s：%w", absolutePath, &SchemaVersionError{Version: version, Supported: schemaVersion})
	}
	// WAL is persistent database state; connection-local settings are installed
	// above before any connection attempts this initialization.
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("core: enable WAL: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	if err := initCoreMetadata(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	searchFTS, err := initSearch(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	writer, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("core: reserve write connection: %w", err)
	}
	return &store{db: db, writer: writer, searchFTS: searchFTS, directory: filepath.Dir(absolutePath)}, nil
}

func (s *store) Close() error { return errors.Join(s.writer.Close(), s.db.Close()) }

func sqliteDSN(absolutePath string) string {
	return sqliteDSNFromSlashPath(filepath.ToSlash(absolutePath))
}

func sqliteDSNFromSlashPath(slashPath string) string {
	// Go's url.URL.String() formats a file URL whose Path does not start with
	// "/" (such as Windows "C:/Users/...") as "file://C:/Users/...". SQLite
	// then treats "C:" as a URI authority and fails sqlite3_open_v2 with
	// SQLITE_ERROR (1). Ensuring a leading "/" produces "file:///C:/Users/..."
	// with an empty authority, which SQLite accepts on all platforms.
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "synchronous(NORMAL)")
	dsn := url.URL{Scheme: "file", Path: slashPath, RawQuery: query.Encode()}
	return dsn.String()
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("core: begin migration: %w", err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("core: read schema version: %w", err)
	}
	if version > schemaVersion {
		return &SchemaVersionError{Version: version, Supported: schemaVersion}
	}
	for v := version; v < len(seedMigrations); v++ {
		if _, err := tx.ExecContext(ctx, seedMigrations[v]); err != nil {
			return fmt.Errorf("core: apply migration %d: %w", v+1, err)
		}
	}
	// From v3 on the recorded version is not evidence of the content: both
	// branches published v3, and one of them added a v4. Only what the database
	// actually contains decides which statements still have to run.
	for _, statement := range []string{migrationV3, migrationV4, migrationV5, migrationV6, migrationV7, migrationV8, migrationV9, migrationV10, migrationV11} {
		if err := ensureSchema(ctx, tx, statement); err != nil {
			return err
		}
	}
	if err := projectLegacyWorkbench(ctx, tx); err != nil {
		return err
	}
	// Assign only previously unpositioned rows. Existing explicit queue order
	// survives migration and every subsequent open.
	if _, err := tx.ExecContext(ctx, `WITH positions AS (
		SELECT pending.id, ROW_NUMBER() OVER (
			PARTITION BY pending.thread_id ORDER BY pending.created_at, pending.id) - 1
			+ COALESCE((SELECT MAX(existing.queue_order) + 1 FROM inputs existing
				WHERE existing.thread_id = pending.thread_id AND existing.kind = 'queue'
				AND existing.status = 'queued' AND existing.queue_order >= 0), 0) AS position
		FROM inputs pending WHERE pending.kind = 'queue' AND pending.status = 'queued' AND pending.queue_order < 0
	) UPDATE inputs SET queue_order = (SELECT position FROM positions WHERE positions.id = inputs.id)
		WHERE id IN (SELECT id FROM positions)`); err != nil {
		return fmt.Errorf("core: migrate queue order: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inputs SET queue_order = 0 WHERE queue_order < 0`); err != nil {
		return fmt.Errorf("core: initialize historical input positions: %w", err)
	}
	if version < schemaVersion {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("core: record schema version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("core: commit migration: %w", err)
	}
	return nil
}

// ensureSchema applies one DDL block idempotently. A table that already exists
// (for example from the other integration branch) is reconciled column by
// column instead of being recreated, so no stored row is ever dropped.
func ensureSchema(ctx context.Context, tx *sql.Tx, ddl string) error {
	for _, statement := range splitSQLStatements(ddl) {
		upper := strings.ToUpper(statement)
		switch {
		case strings.HasPrefix(upper, "CREATE TABLE"):
			if err := ensureTable(ctx, tx, statement); err != nil {
				return err
			}
		case strings.HasPrefix(upper, "CREATE UNIQUE INDEX"), strings.HasPrefix(upper, "CREATE INDEX"):
			if _, err := tx.ExecContext(ctx, withIfNotExists(statement)); err != nil {
				return fmt.Errorf("core: create index: %w", err)
			}
		case strings.HasPrefix(upper, "ALTER TABLE"):
			if err := ensureAlter(ctx, tx, statement); err != nil {
				return err
			}
		default:
			return fmt.Errorf("core: unsupported schema statement %q", firstStatementLine(statement))
		}
	}
	return nil
}

// splitSQLStatements drops comment lines and splits the canonical DDL into
// single executable statements.
func splitSQLStatements(ddl string) []string {
	lines := make([]string, 0, 16)
	for _, line := range strings.Split(ddl, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		lines = append(lines, line)
	}
	statements := make([]string, 0, 8)
	for _, raw := range strings.Split(strings.Join(lines, "\n"), ";") {
		statement := strings.TrimSpace(raw)
		if statement == "" {
			continue
		}
		statements = append(statements, statement)
	}
	return statements
}

func firstStatementLine(statement string) string {
	if index := strings.IndexByte(statement, '\n'); index >= 0 {
		return statement[:index]
	}
	return statement
}

func withIfNotExists(statement string) string {
	upper := strings.ToUpper(statement)
	if strings.Contains(upper, "IF NOT EXISTS") {
		return statement
	}
	if index := strings.Index(upper, "INDEX"); index >= 0 {
		return statement[:index+len("INDEX")] + " IF NOT EXISTS" + statement[index+len("INDEX"):]
	}
	return statement
}

func tableExists(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
		return false, fmt.Errorf("core: read schema for %s: %w", table, err)
	}
	return count > 0, nil
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, fmt.Errorf("core: read columns of %s: %w", table, err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var (
			position   int
			name       string
			kind       string
			notNull    int
			defaultVal any
			primaryKey int
		)
		if err := rows.Scan(&position, &name, &kind, &notNull, &defaultVal, &primaryKey); err != nil {
			return nil, fmt.Errorf("core: scan columns of %s: %w", table, err)
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func ensureColumn(ctx context.Context, tx *sql.Tx, table, column, definition string) error {
	columns, err := tableColumns(ctx, tx, table)
	if err != nil {
		return err
	}
	if columns[column] {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		return fmt.Errorf("core: add column %s.%s: %w", table, column, err)
	}
	return nil
}

// ensureTable creates a table when it is absent and otherwise adds every column
// the canonical definition has but the stored table does not.
func ensureTable(ctx context.Context, tx *sql.Tx, statement string) error {
	table, body, ok := parseCreateTable(statement)
	if !ok {
		return fmt.Errorf("core: unparsable table definition %q", firstStatementLine(statement))
	}
	exists, err := tableExists(ctx, tx, table)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, withIfNotExists(statement)); err != nil {
			return fmt.Errorf("core: create table %s: %w", table, err)
		}
		return nil
	}
	for _, column := range parseTableColumns(body) {
		if err := ensureColumn(ctx, tx, table, column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func ensureAlter(ctx context.Context, tx *sql.Tx, statement string) error {
	fields := strings.Fields(statement)
	// ALTER TABLE <table> ADD COLUMN <column> <definition...>
	if len(fields) < 6 || !strings.EqualFold(fields[3], "ADD") || !strings.EqualFold(fields[4], "COLUMN") {
		return fmt.Errorf("core: unsupported alter statement %q", firstStatementLine(statement))
	}
	table, column := fields[2], fields[5]
	definition := strings.TrimSpace(statement[strings.Index(statement, fields[5])+len(fields[5]):])
	return ensureColumn(ctx, tx, table, column, definition)
}

type columnDefinition struct {
	name       string
	definition string
}

var parseCreateTablePattern = regexp.MustCompile(`(?is)^CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\((.*)\)$`)

func parseCreateTable(statement string) (string, string, bool) {
	match := parseCreateTablePattern.FindStringSubmatch(strings.TrimSpace(statement))
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

// parseTableColumns keeps only real column definitions: table-level constraints
// (PRIMARY KEY (...), UNIQUE (...), FOREIGN KEY, CHECK) are not columns.
func parseTableColumns(body string) []columnDefinition {
	var (
		columns []columnDefinition
		depth   int
		start   int
	)
	segments := []string{}
	for index, char := range body {
		switch char {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				segments = append(segments, body[start:index])
				start = index + 1
			}
		}
	}
	segments = append(segments, body[start:])
	for _, segment := range segments {
		fields := strings.Fields(strings.TrimSpace(segment))
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "PRIMARY", "UNIQUE", "FOREIGN", "CHECK", "CONSTRAINT":
			continue
		}
		if strings.Contains(strings.ToUpper(segment), "PRIMARY KEY") {
			// The primary key of an existing table is never re-added.
			continue
		}
		columns = append(columns, columnDefinition{
			name:       fields[0],
			definition: strings.TrimSpace(strings.Join(fields[1:], " ")),
		})
	}
	return columns
}

// projectLegacyWorkbench copies the pre-merge local queue and steering rows into
// the unified inputs table. Identities, ownership, state and attachments are
// preserved, so an old receipt or client reference still resolves. A steering
// row without a confirmed send becomes failed (never sent) or unknown (sent,
// unconfirmed); none of them is ever retried.
func projectLegacyWorkbench(ctx context.Context, tx *sql.Tx) error {
	queueExists, err := tableExists(ctx, tx, "queued_messages")
	if err != nil {
		return err
	}
	if queueExists {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO inputs
			(id, thread_id, run_id, attempt_id, kind, message, attachments, command_id,
			 status, disposition, error, created_at, updated_at)
			SELECT q.id, q.thread_id, q.run_id,
				COALESCE((SELECT r.active_attempt_id FROM runs r WHERE r.id = q.run_id), ''),
				'queue', q.message, q.attachments,
				COALESCE((SELECT c.command_id FROM command_receipts c
					WHERE c.message_id = q.id OR c.input_id = q.id LIMIT 1), ''),
				CASE q.status WHEN 'pending' THEN 'queued' ELSE q.status END,
				'', '', q.created_at, q.created_at
			FROM queued_messages q`); err != nil {
			return fmt.Errorf("core: project queued messages: %w", err)
		}
	}
	steeringExists, err := tableExists(ctx, tx, "steering_messages")
	if err != nil {
		return err
	}
	if steeringExists {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO inputs
			(id, thread_id, run_id, attempt_id, kind, message, attachments, command_id,
			 status, disposition, error, created_at, updated_at)
			SELECT s.id, s.thread_id, s.run_id, s.attempt_id, 'steer', s.message, s.attachments,
				s.command_id,
				CASE s.status
					WHEN 'accepted' THEN 'delivered'
					WHEN 'rejected' THEN 'failed'
					WHEN 'sending' THEN 'unknown'
					WHEN 'pending' THEN 'failed'
					ELSE s.status END,
				s.disposition,
				CASE s.status
					WHEN 'pending' THEN 'steering was persisted before the send barrier and will not be retried'
					ELSE s.error END,
				s.created_at, s.updated_at
			FROM steering_messages s`); err != nil {
			return fmt.Errorf("core: project steering messages: %w", err)
		}
	}
	return nil
}

// runRecord is the projection row for a Run. Thread → Run → Attempt are kept as
// separate rows: a Run carries the identity of its current attempt and the
// attempt table keeps its own lifecycle.
type runRecord struct {
	ID              string
	ThreadID        string
	SenderThreadID  string
	AttemptID       string
	ActiveAttemptID string
	Status          string
	Input           string
	Output          string
	Reasoning       string
	Error           string
	ProviderID      string
	Model           string
	Workspace       *api.WorkspaceSpec
	ParentTaskID    string
	ApprovalPolicy  string
	InteractionMode string
	Attachments     []api.Attachment
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (r runRecord) toAPI() api.Run {
	return api.Run{
		ID:              r.ID,
		ThreadID:        r.ThreadID,
		SenderThreadID:  r.SenderThreadID,
		AttemptID:       r.AttemptID,
		Status:          api.RunStatus(r.Status),
		Input:           r.Input,
		Attachments:     r.Attachments,
		Output:          r.Output,
		Reasoning:       r.Reasoning,
		Error:           r.Error,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
		ProviderID:      r.ProviderID,
		Model:           r.Model,
		Workspace:       r.Workspace,
		ApprovalPolicy:  r.ApprovalPolicy,
		InteractionMode: firstNonEmpty(r.InteractionMode, interactionDefault),
	}
}

// active means provider work may still be attached to this run. A run waiting
// for its children is instead idempotently parked: it occupies the thread slot
// but has no active attempt.
func (r runRecord) active() bool {
	return r.Status == statusQueued || r.Status == statusRunning
}

// occupyingSlot reports whether this run holds the single active run slot of
// its thread, including the waiting_children state.
func (r runRecord) occupyingSlot() bool {
	return r.active() || r.Status == statusWaitingChildren
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRun(row scanner) (runRecord, error) {
	var (
		rec              runRecord
		created, updated int64
		workspace        string
		attachments      string
	)
	err := row.Scan(&rec.ID, &rec.ThreadID, &rec.AttemptID, &rec.ActiveAttemptID, &rec.Status,
		&rec.Input, &rec.Output, &rec.Error, &created, &updated,
		&rec.ProviderID, &rec.Model, &workspace, &rec.ParentTaskID, &rec.ApprovalPolicy,
		&attachments, &rec.Reasoning, &rec.InteractionMode, &rec.SenderThreadID)
	if err != nil {
		return runRecord{}, err
	}
	if attachments != "" && attachments != "[]" {
		if err := json.Unmarshal([]byte(attachments), &rec.Attachments); err != nil {
			return runRecord{}, err
		}
	}
	rec.CreatedAt = fromNanos(created)
	rec.UpdatedAt = fromNanos(updated)
	rec.Workspace = decodeWorkspace(workspace)
	return rec, nil
}

func loadRun(t *txn, runID string) (runRecord, error) {
	return scanRun(t.queryRow(`SELECT `+runColumns+` FROM runs WHERE id = ?`, runID))
}

// threadRecord is the projection row for a Thread. Configuration is resolved
// once on the thread and copied onto every run created from it.
type threadRecord struct {
	ProjectPath        string
	ID                 string
	Title              string
	ProviderID         string
	Model              string
	Workspace          *api.WorkspaceSpec
	ParentThreadID     string
	ParentTaskID       string
	CreatedByThreadID  string
	CreatedByRunID     string
	SnoozedUntil       *time.Time
	ApprovalPolicy     string
	InteractionMode    string
	ForkedFromThreadID string
	ForkedFromRunID    string
	Pinned             bool
	Archived           bool
	CreatedAt          time.Time
}

// link is the reference agents and the workbench copy: the label names the
// session, never the project its coordinator title is qualified with.
func (r threadRecord) link() string {
	return api.FormatThreadReference(r.ID, r.Title, firstNonEmpty(r.ProjectPath, api.WorkspaceProjectPath(r.Workspace)))
}

func (r threadRecord) toAPI() api.Thread {
	return api.Thread{
		ProjectPath:        r.ProjectPath,
		ID:                 r.ID,
		Title:              r.Title,
		CreatedAt:          r.CreatedAt,
		ProviderID:         r.ProviderID,
		Model:              r.Model,
		Workspace:          r.Workspace,
		ParentThreadID:     r.ParentThreadID,
		ParentTaskID:       r.ParentTaskID,
		CreatedByThreadID:  r.CreatedByThreadID,
		CreatedByRunID:     r.CreatedByRunID,
		SnoozedUntil:       r.SnoozedUntil,
		ApprovalPolicy:     r.ApprovalPolicy,
		Pinned:             r.Pinned,
		Archived:           r.Archived,
		InteractionMode:    firstNonEmpty(r.InteractionMode, interactionDefault),
		ForkedFromThreadID: r.ForkedFromThreadID,
		ForkedFromRunID:    r.ForkedFromRunID,
	}
}

func scanThread(row scanner) (threadRecord, error) {
	var (
		rec       threadRecord
		created   int64
		snoozed   int64
		workspace string
	)
	if err := row.Scan(&rec.ID, &rec.Title, &created, &rec.ProviderID, &rec.Model, &workspace,
		&rec.ParentThreadID, &rec.ParentTaskID, &rec.ApprovalPolicy,
		&rec.Pinned, &rec.Archived, &rec.InteractionMode, &rec.ForkedFromThreadID, &rec.ForkedFromRunID,
		&snoozed, &rec.CreatedByThreadID, &rec.CreatedByRunID, &rec.ProjectPath); err != nil {
		return threadRecord{}, err
	}
	rec.CreatedAt = fromNanos(created)
	rec.SnoozedUntil = optionalTime(snoozed)
	rec.Workspace = decodeWorkspace(workspace)
	return rec, nil
}

func optionalTime(n int64) *time.Time {
	if n <= 0 {
		return nil
	}
	at := fromNanos(n)
	return &at
}

func timeNanos(at *time.Time) int64 {
	if at == nil || at.IsZero() {
		return 0
	}
	return at.UTC().UnixNano()
}

func loadThread(t *txn, threadID string) (threadRecord, error) {
	return scanThread(t.queryRow(`SELECT `+threadColumns+` FROM threads WHERE id = ?`, threadID))
}

func encodeWorkspace(spec *api.WorkspaceSpec) (string, error) {
	if spec == nil {
		return "", nil
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("core: encode workspace: %w", err)
	}
	return string(raw), nil
}

func decodeWorkspace(raw string) *api.WorkspaceSpec {
	if raw == "" {
		return nil
	}
	var spec api.WorkspaceSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil
	}
	return &spec
}

// txn is the only way to write. It accumulates the events committed by a write
// so they can be published strictly after the commit succeeds and in commit
// order, without a second transaction.
type txn struct {
	ctx    context.Context
	tx     *sql.Tx
	events []api.Event
}

func (t *txn) exec(query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(t.ctx, query, args...)
}

func (t *txn) queryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(t.ctx, query, args...)
}

func (t *txn) query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(t.ctx, query, args...)
}

// event appends one event to the log and records it for post-commit publish.
func (t *txn) event(typ, threadID, runID, attemptID string, data any) (api.Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return api.Event{}, fmt.Errorf("core: encode event data: %w", err)
	}
	ev := api.Event{
		ID:        newID("evt"),
		Type:      typ,
		ThreadID:  threadID,
		RunID:     runID,
		AttemptID: attemptID,
		Timestamp: time.Now().UTC(),
		Data:      raw,
	}
	res, err := t.exec(`INSERT INTO events (id, type, thread_id, run_id, attempt_id, timestamp, data)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.Type, ev.ThreadID, ev.RunID, ev.AttemptID, ev.Timestamp.UnixNano(), raw)
	if err != nil {
		return api.Event{}, fmt.Errorf("core: append event: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return api.Event{}, fmt.Errorf("core: read event sequence: %w", err)
	}
	ev.Sequence = seq
	t.events = append(t.events, ev)
	return ev, nil
}

func newID(prefix string) string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("core: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

func nowNanos() int64 { return time.Now().UnixNano() }

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }
