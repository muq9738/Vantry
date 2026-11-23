package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// Two integration branches published schemaVersion = 3 with different contents,
// and one of them then added a v4 steering table. These fixtures are the real
// DDL of each branch, applied to the same v2 seed, so the merged Core is proven
// to open both databases without dropping history, without re-running an
// accepted command and without reviving an unconfirmed control.

// macbookV3 is origin/main's v3: approval policy, durable inputs, tool calls and
// provider dialogs, plus the input/request identity of a receipt.
const macbookV3 = `
ALTER TABLE threads ADD COLUMN approval_policy TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE runs ADD COLUMN approval_policy TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE command_receipts ADD COLUMN input_id TEXT NOT NULL DEFAULT '';
ALTER TABLE command_receipts ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
CREATE TABLE inputs (
	id         TEXT PRIMARY KEY,
	thread_id  TEXT NOT NULL,
	run_id     TEXT NOT NULL DEFAULT '',
	attempt_id TEXT NOT NULL DEFAULT '',
	kind       TEXT NOT NULL,
	message    TEXT NOT NULL,
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX inputs_by_thread ON inputs(thread_id, kind, status, created_at, id);
CREATE INDEX inputs_by_attempt ON inputs(attempt_id, kind, status);
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

// localV3 is the local branch's v3: thread management flags, run attachments,
// the receipt message identity and its own queue table.
const localV3 = `
ALTER TABLE threads ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
ALTER TABLE threads ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]';
ALTER TABLE command_receipts ADD COLUMN message_id TEXT NOT NULL DEFAULT '';
CREATE TABLE queued_messages (
 id TEXT PRIMARY KEY,
 thread_id TEXT NOT NULL REFERENCES threads(id),
 message TEXT NOT NULL,
 attachments TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL,
 run_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX messages_pending ON queued_messages(status, created_at, id);
`

// localV4 is the local branch's steering projection.
const localV4 = `
CREATE TABLE steering_messages (
 id TEXT PRIMARY KEY,
 command_id TEXT NOT NULL UNIQUE,
 thread_id TEXT NOT NULL REFERENCES threads(id),
 run_id TEXT NOT NULL REFERENCES runs(id),
 attempt_id TEXT NOT NULL REFERENCES attempts(id),
 message TEXT NOT NULL,
 attachments TEXT NOT NULL,
 status TEXT NOT NULL,
 disposition TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX steering_by_status ON steering_messages(status, created_at, id);
`

// recordingProvider is a plain provider that also accepts controls, so the test
// can prove which handoffs Core actually attempted.
type recordingProvider struct {
	*funcProvider
	mu       sync.Mutex
	controls []provider.Control
}

func newRecordingProvider() *recordingProvider {
	return &recordingProvider{funcProvider: &funcProvider{}}
}

func (p *recordingProvider) Control(_ context.Context, _ provider.Request, control provider.Control) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.controls = append(p.controls, control)
	return nil
}

func (p *recordingProvider) controlCalls() []provider.Control {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Control(nil), p.controls...)
}

type legacyDatabase struct {
	path    string
	version int
}

// buildLegacyDatabase creates a real database of one branch: the shared v2 seed,
// then that branch's own migrations, then its history.
func buildLegacyDatabase(t *testing.T, version int, branch []string, seed func(*sql.DB)) legacyDatabase {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range append([]string{migrationV1, migrationV2}, branch...) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("apply branch schema: %v", err)
		}
	}
	if seed != nil {
		seed(db)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatal(err)
	}
	return legacyDatabase{path: path, version: version}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func insertLegacyThread(t *testing.T, db *sql.DB, threadID, title string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO threads (id, title, created_at, provider_id, model) VALUES (?, ?, ?, 'test', 'test/model')`,
		threadID, title, time.Now().Add(-time.Hour).UnixNano())
}

// insertLegacyRun records one finished run with its attempt. A completed run is
// history: no migration may resurrect it or repeat it.
func insertLegacyRun(t *testing.T, db *sql.DB, threadID, runID, attemptID, input, output string) {
	t.Helper()
	now := time.Now().Add(-30 * time.Minute).UnixNano()
	mustExec(t, db, `INSERT INTO runs
		(id, thread_id, attempt_id, active_attempt_id, status, input, output, error, created_at, updated_at,
		 provider_id, model, workspace, parent_task_id)
		VALUES (?, ?, ?, '', 'completed', ?, ?, '', ?, ?, 'test', 'test/model', '', '')`,
		runID, threadID, attemptID, input, output, now, now)
	mustExec(t, db, `INSERT INTO attempts (id, run_id, ordinal, status, created_at, updated_at, input, provider_cursor)
		VALUES (?, ?, 1, 'completed', ?, ?, ?, 1)`, attemptID, runID, now, now, input)
}

func writeReceipt(t *testing.T, db *sql.DB, cmd api.Command, receipt api.Receipt, extraColumn, extraValue string) {
	t.Helper()
	columns := `command_id, kind, fingerprint, thread_id, run_id, sequence, accepted, created_at,
		payload, task_id, generation, delivery_message_id`
	values := []any{cmd.CommandID, string(cmd.Kind), "", receipt.ThreadID, receipt.RunID, receipt.Sequence,
		1, time.Now().UnixNano(), commandPayload(cmd), "", 0, ""}
	if extraColumn != "" {
		columns += ", " + extraColumn
		values = append(values, extraValue)
	}
	placeholders := ""
	for i := range values {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += "?"
	}
	mustExec(t, db, `INSERT INTO command_receipts (`+columns+`) VALUES (`+placeholders+`)`, values...)
}

func databaseColumn(t *testing.T, path, table, column string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func databaseTableCount(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(1) FROM ` + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// TestLocalV4UpgradeKeepsHistoryAndUnifiesInputs opens a database written by the
// local branch (v3 queue table + v4 steering table) and proves that its business
// identities, states and attachments move into the unified inputs table without
// re-running anything and without turning an unconfirmed steer into a resend.
func TestLocalV4UpgradeKeepsHistoryAndUnifiesInputs(t *testing.T) {
	const (
		threadID  = "th_legacy"
		doneRun   = "run_done"
		doneAtt   = "att_done"
		queuedRun = "run_queued"
		oldAtt    = "att_old"
	)
	keep := api.Attachment{ID: "att_keep", Name: "note.txt", MIME: "text/plain", Size: 4}
	queuedAttachment := api.Attachment{ID: "att_q", Name: "queued.txt", MIME: "text/plain", Size: 6}
	sendCommand := api.Command{
		Version: api.ProtocolVersion, CommandID: "cmd-historic-send", Kind: api.SendMessage,
		ThreadID: threadID, Message: "historic",
	}
	historicReceipt := api.Receipt{
		CommandID: sendCommand.CommandID, Accepted: true, ThreadID: threadID, RunID: doneRun, Sequence: 4,
	}

	database := buildLegacyDatabase(t, 4, []string{localV3, localV4}, func(db *sql.DB) {
		insertLegacyThread(t, db, threadID, "历史会话")
		mustExec(t, db, `UPDATE threads SET pinned = 1 WHERE id = ?`, threadID)
		insertLegacyRun(t, db, threadID, doneRun, doneAtt, "historic", "historic output")
		insertLegacyRun(t, db, threadID, queuedRun, "att_queued_run", "earlier", "earlier output")
		attachmentJSON, _ := json.Marshal([]api.Attachment{keep})
		mustExec(t, db, `UPDATE runs SET attachments = ? WHERE id = ?`, string(attachmentJSON), queuedRun)
		queuedJSON, _ := json.Marshal([]api.Attachment{queuedAttachment})
		mustExec(t, db, `INSERT INTO queued_messages (id, thread_id, message, attachments, status, run_id, created_at)
			VALUES ('msg_pending', ?, 'legacy queued', ?, 'pending', '', ?)`,
			threadID, string(queuedJSON), time.Now().Add(-20*time.Minute).UnixNano())
		mustExec(t, db, `INSERT INTO queued_messages (id, thread_id, message, attachments, status, run_id, created_at)
			VALUES ('msg_started', ?, 'legacy started', '[]', 'started', ?, ?)`,
			threadID, queuedRun, time.Now().Add(-25*time.Minute).UnixNano())
		mustExec(t, db, `INSERT INTO steering_messages
			(id, command_id, thread_id, run_id, attempt_id, message, attachments, status, disposition, error, created_at, updated_at)
			VALUES ('msg_unconfirmed', 'cmd-unconfirmed', ?, ?, ?, 'unconfirmed steer', '[]', 'sending', '', '', ?, ?)`,
			threadID, doneRun, doneAtt, time.Now().Add(-10*time.Minute).UnixNano(), time.Now().Add(-10*time.Minute).UnixNano())
		mustExec(t, db, `INSERT INTO steering_messages
			(id, command_id, thread_id, run_id, attempt_id, message, attachments, status, disposition, error, created_at, updated_at)
			VALUES ('msg_acknowledged', 'cmd-acknowledged', ?, ?, ?, 'accepted steer', '[]', 'accepted', 'queued', '', ?, ?)`,
			threadID, doneRun, doneAtt, time.Now().Add(-9*time.Minute).UnixNano(), time.Now().Add(-9*time.Minute).UnixNano())
		writeReceipt(t, db, sendCommand, historicReceipt, "message_id", "")
	})

	capture := &funcProvider{}
	c := openCoreAt(t, database.path, capture)
	defer c.Close()

	// Nothing the branch recorded may be lost.
	snapshot := snapshotOf(t, c)
	if len(snapshot.Threads) != 1 || snapshot.Threads[0].Title != "历史会话" || !snapshot.Threads[0].Pinned {
		t.Fatalf("thread history lost: %+v", snapshot.Threads)
	}
	done, ok := findRun(snapshot, doneRun)
	if !ok || done.Output != "historic output" || done.Status != api.RunCompleted {
		t.Fatalf("completed run lost: %+v", done)
	}
	earlier, ok := findRun(snapshot, queuedRun)
	if !ok || len(earlier.Attachments) != 1 || earlier.Attachments[0].ID != keep.ID {
		t.Fatalf("run attachments lost: %+v", earlier)
	}

	// The legacy queue row is projected with its own identity, state and
	// attachments, and is the single scheduling authority from now on.
	run := waitForRun(t, c, "queue input to start", func(snap api.Snapshot) bool {
		input, found := findInput(snap, "msg_pending")
		return found && input.Status == inputStarted && input.RunID != ""
	}, func() bool { return capture.callCount() == 1 })
	if run.Message != "legacy queued" || len(run.Attachments) != 1 || run.Attachments[0].ID != queuedAttachment.ID {
		t.Fatalf("queued message changed while migrating: %+v", run)
	}
	started, ok := findInput(snapshotOf(t, c), "msg_started")
	if !ok || started.Status != inputStarted || started.RunID != queuedRun {
		t.Fatalf("started queue row changed: %+v", started)
	}
	unconfirmed, ok := findInput(snapshotOf(t, c), "msg_unconfirmed")
	if !ok || unconfirmed.Status != inputUnknown {
		t.Fatalf("unconfirmed steer must stay unknown, not pending: %+v", unconfirmed)
	}
	acknowledged, ok := findInput(snapshotOf(t, c), "msg_acknowledged")
	if !ok || acknowledged.Status != inputDelivered {
		t.Fatalf("acknowledged steer lost: %+v", acknowledged)
	}
	var acknowledgedLegacy bool
	for _, message := range snapshot.Steering {
		if message.ID == "msg_acknowledged" {
			acknowledgedLegacy = message.Status == api.SteeringAccepted && message.Disposition == "queued"
		}
	}
	if !acknowledgedLegacy {
		t.Fatalf("legacy steering view lost its disposition: %+v", snapshot.Steering)
	}
	// The legacy table is gone from the scheduler but kept for traceability.
	if count := databaseTableCount(t, database.path, "queued_messages"); count != 2 {
		t.Fatalf("legacy queue table lost rows: %d", count)
	}
	if count := databaseTableCount(t, database.path, "steering_messages"); count != 2 {
		t.Fatalf("legacy steering table lost rows: %d", count)
	}
	// The legacy views are derived from the same rows.
	if len(snapshot.Messages) != 2 || len(snapshot.Steering) != 2 {
		t.Fatalf("legacy projections: messages=%+v steering=%+v", snapshot.Messages, snapshot.Steering)
	}

	// A stored commandId still resolves to its original receipt and never runs
	// the historic message a second time.
	runsBefore := len(snapshotOf(t, c).Runs)
	replayed, err := c.Dispatch(context.Background(), sendCommand)
	if err != nil {
		t.Fatalf("historic receipt replay: %v", err)
	}
	if replayed.RunID != doneRun || replayed.Sequence != historicReceipt.Sequence {
		t.Fatalf("historic receipt changed: %+v", replayed)
	}
	if len(snapshotOf(t, c).Runs) != runsBefore {
		t.Fatal("replayed command started another run")
	}
	if capture.callCount() != 1 {
		t.Fatalf("historic message was submitted again: %d provider calls", capture.callCount())
	}
}

// TestMacBookV3UpgradeAddsWorkbenchState opens the other branch's v3 database
// and proves that its inputs, tool calls, dialogs and approval policy survive
// next to the merged columns, and that a persisted control is never resent.
func TestMacBookV3UpgradeAddsWorkbenchState(t *testing.T) {
	const (
		threadID = "th_mac"
		runID    = "run_mac"
		attempt  = "att_mac"
	)
	database := buildLegacyDatabase(t, 3, []string{macbookV3}, func(db *sql.DB) {
		insertLegacyThread(t, db, threadID, "MacBook 会话")
		mustExec(t, db, `UPDATE threads SET approval_policy = 'ask' WHERE id = ?`, threadID)
		insertLegacyRun(t, db, threadID, runID, attempt, "historic mac", "mac output")
		mustExec(t, db, `UPDATE runs SET approval_policy = 'ask' WHERE id = ?`, runID)
		mustExec(t, db, `INSERT INTO inputs (id, thread_id, run_id, attempt_id, kind, message, status, error, created_at, updated_at)
			VALUES ('inp_pending', ?, '', '', 'queue', 'mac queued', 'queued', '', ?, ?)`,
			threadID, time.Now().Add(-5*time.Minute).UnixNano(), time.Now().Add(-5*time.Minute).UnixNano())
		mustExec(t, db, `INSERT INTO inputs (id, thread_id, run_id, attempt_id, kind, message, status, error, created_at, updated_at)
			VALUES ('inp_steer', ?, ?, ?, 'steer', 'mac steer', 'pending', '', ?, ?)`,
			threadID, runID, attempt, time.Now().Add(-4*time.Minute).UnixNano(), time.Now().Add(-4*time.Minute).UnixNano())
		// A persisted steer is always paired with its durable handoff intent in
		// the same transaction; the fixture keeps that invariant.
		handoff, err := json.Marshal(controlIntent{
			Request: provider.Request{ThreadID: threadID, RunID: runID, AttemptID: attempt},
			Control: provider.Control{ID: "inp_steer", Kind: "steer", Message: "mac steer"},
		})
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `INSERT INTO effect_outbox
			(id, kind, thread_id, run_id, attempt_id, status, attempt_count, claim_token, lease_until,
			 last_error, created_at, updated_at, input)
			VALUES ('eff_steer', ?, ?, ?, ?, 'pending', 0, '', 0, '', ?, ?, ?)`,
			effectKindProviderControl, threadID, runID, attempt, time.Now().UnixNano(), time.Now().UnixNano(),
			string(handoff))
		mustExec(t, db, `INSERT INTO tool_calls (id, thread_id, run_id, attempt_id, name, arguments, output, status, updated_at)
			VALUES (?, ?, ?, ?, 'bash', '{"command":"true"}', 'done', 'running', ?)`,
			attempt+":tool-1", threadID, runID, attempt, time.Now().UnixNano())
		mustExec(t, db, `INSERT INTO provider_requests
			(id, provider_request_id, thread_id, run_id, attempt_id, method, title, message, options, prefill,
			 status, error, expires_at, created_at, updated_at)
			VALUES (?, 'pi:dialog/1', ?, ?, ?, 'confirm', 'Allow bash?', '', '[]', '', 'pending', '', 0, ?, ?)`,
			attempt+":pi:dialog/1", threadID, runID, attempt, time.Now().UnixNano(), time.Now().UnixNano())
	})

	recorder := newRecordingProvider()
	c := openCoreAt(t, database.path, recorder)
	defer c.Close()

	snapshot := snapshotOf(t, c)
	thread, ok := findThreadByID(snapshot, threadID)
	if !ok || thread.ApprovalPolicy != approvalAsk {
		t.Fatalf("approval policy lost: %+v", thread)
	}
	if len(snapshot.Tools) != 1 || snapshot.Tools[0].Status != toolRunning {
		t.Fatalf("tool projection lost: %+v", snapshot.Tools)
	}
	if len(snapshot.Requests) != 1 || snapshot.Requests[0].Status != requestPending {
		t.Fatalf("dialog projection lost: %+v", snapshot.Requests)
	}
	// The merged columns exist next to the branch's own ones.
	for _, column := range []string{"attachments", "command_id", "disposition"} {
		if !databaseColumn(t, database.path, "inputs", column) {
			t.Fatalf("merged inputs column %s missing", column)
		}
	}
	for _, check := range []struct{ table, column string }{
		{"threads", "pinned"}, {"threads", "archived"},
		{"runs", "attachments"}, {"command_receipts", "message_id"},
	} {
		if !databaseColumn(t, database.path, check.table, check.column) {
			t.Fatalf("merged column %s.%s missing", check.table, check.column)
		}
	}

	// The queued input runs once under the inherited approval policy.
	request := waitForRun(t, c, "queued input to run", func(api.Snapshot) bool { return true },
		func() bool { return recorder.callCount() == 1 })
	if request.Message != "mac queued" || request.ApprovalPolicy != approvalAsk {
		t.Fatalf("queued request lost its configuration: %+v", request)
	}
	// A steer persisted by the old process is resolved without a handoff: it is
	// never resent and never left pending forever.
	steer := waitForStatus(t, c, "inp_steer", func(status string) bool {
		return status == inputFailed || status == inputUnknown
	})
	if steer.RunID != runID || steer.AttemptID != attempt {
		t.Fatalf("steer identity changed: %+v", steer)
	}
	controls := recorder.controlCalls()
	if len(controls) != 0 {
		t.Fatalf("unconfirmed control was resent: %+v", controls)
	}
	if count := databaseTableCount(t, database.path, "inputs"); count != 2 {
		t.Fatalf("inputs table lost rows: %d", count)
	}
}

// TestFreshAndV2DatabasesReachTheUnifiedSchema covers the two remaining inputs
// to the migration: a brand new file and a database that stopped at the shared
// v2 seed.
func TestFreshAndV2DatabasesReachTheUnifiedSchema(t *testing.T) {
	required := []struct{ table, column string }{
		{"threads", "approval_policy"}, {"threads", "pinned"}, {"threads", "archived"},
		{"runs", "approval_policy"}, {"runs", "attachments"},
		{"command_receipts", "input_id"}, {"command_receipts", "request_id"}, {"command_receipts", "message_id"},
		{"inputs", "attachments"}, {"inputs", "command_id"}, {"inputs", "disposition"},
		{"tool_calls", "output"}, {"provider_requests", "expires_at"},
	}
	check := func(t *testing.T, path string) {
		t.Helper()
		for _, column := range required {
			if !databaseColumn(t, path, column.table, column.column) {
				t.Fatalf("fresh schema missing %s.%s", column.table, column.column)
			}
		}
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var version int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != schemaVersion {
			t.Fatalf("recorded schema version %d, want %d", version, schemaVersion)
		}
	}

	t.Run("fresh", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fresh.sqlite")
		c := openCoreAt(t, path, &funcProvider{})
		defer c.Close()
		check(t, path)
	})

	t.Run("version-two", func(t *testing.T) {
		database := buildLegacyDatabase(t, 2, nil, func(db *sql.DB) {
			insertLegacyThread(t, db, "th_v2", "v2 会话")
			insertLegacyRun(t, db, "th_v2", "run_v2", "att_v2", "old input", "old output")
		})
		capture := &funcProvider{}
		c := openCoreAt(t, database.path, capture)
		defer c.Close()
		run, ok := findRun(snapshotOf(t, c), "run_v2")
		if !ok || run.Output != "old output" || run.Status != api.RunCompleted {
			t.Fatalf("v2 history lost: %+v", run)
		}
		if capture.callCount() != 0 {
			t.Fatal("v2 upgrade re-ran a finished run")
		}
		check(t, database.path)
	})
}

func findThreadByID(snapshot api.Snapshot, threadID string) (api.Thread, bool) {
	for _, thread := range snapshot.Threads {
		if thread.ID == threadID {
			return thread, true
		}
	}
	return api.Thread{}, false
}

// waitForRun waits for a durable state and for the provider request it produced.
func waitForRun(t *testing.T, c *Core, what string, cond func(api.Snapshot) bool, called func() bool) provider.Request {
	t.Helper()
	waitFor(t, what, func() bool { return cond(snapshotOf(t, c)) && called() })
	capture, ok := c.provider.(interface{ requestAt(int) provider.Request })
	if !ok {
		t.Fatalf("provider %T cannot report requests", c.provider)
	}
	return capture.requestAt(0)
}

func waitForStatus(t *testing.T, c *Core, inputID string, accept func(string) bool) api.Input {
	t.Helper()
	var input api.Input
	waitFor(t, "input "+inputID+" to resolve", func() bool {
		found, ok := findInput(snapshotOf(t, c), inputID)
		if !ok {
			return false
		}
		input = found
		return accept(found.Status)
	})
	return input
}
