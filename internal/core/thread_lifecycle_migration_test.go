package core

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestLifecycleAndQueueMigrateEveryPublishedSchema(t *testing.T) {
	for _, tc := range []struct {
		name       string
		version    int
		migrations []string
	}{
		{"v1", 1, []string{migrationV1}},
		{"v2", 2, []string{migrationV1, migrationV2}},
		{"macbook-v3", 3, []string{migrationV1, migrationV2, macbookV3}},
		{"local-v3", 3, []string{migrationV1, migrationV2, localV3}},
		{"local-v4", 4, []string{migrationV1, migrationV2, localV3, localV4}},
		{"unified-v7", 7, []string{migrationV1, migrationV2, migrationV3, migrationV4, migrationV5, migrationV6, migrationV7}},
		// A partially integrated schema may already have one new column even
		// though its recorded version is older. ensureColumn must reconcile it.
		{"partial-v8", 7, []string{migrationV1, migrationV2, migrationV3, migrationV4, migrationV5, migrationV6, migrationV7,
			`ALTER TABLE threads ADD COLUMN interaction_mode TEXT NOT NULL DEFAULT 'default';`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
			if err != nil {
				t.Fatal(err)
			}
			for _, ddl := range tc.migrations {
				mustExec(t, db, ddl)
			}
			mustExec(t, db, `INSERT INTO threads (id, title, created_at) VALUES ('source', 'history', 1)`)
			mustExec(t, db, `INSERT INTO runs
				(id, thread_id, attempt_id, active_attempt_id, status, input, output, created_at, updated_at)
				VALUES ('historical-run', 'source', 'historical-attempt', '', 'completed', 'old input', 'old output', 2, 3)`)
			mustExec(t, db, `INSERT INTO attempts (id, run_id, ordinal, status, created_at, updated_at)
				VALUES ('historical-attempt', 'historical-run', 1, 'completed', 2, 3)`)
			if tc.name == "local-v3" || tc.name == "local-v4" {
				mustExec(t, db, `INSERT INTO queued_messages (id, thread_id, message, status, run_id, created_at)
					VALUES ('legacy-queue', 'source', 'old queue', 'started', 'historical-run', 1)`)
			}
			if tc.name == "local-v4" {
				mustExec(t, db, `INSERT INTO steering_messages
					(id, command_id, thread_id, run_id, attempt_id, message, attachments, status, created_at, updated_at)
					VALUES ('legacy-steer', 'old-steer', 'source', 'historical-run', 'historical-attempt', 'old steer', '[]', 'accepted', 2, 3)`)
			}
			mustExec(t, db, fmt.Sprintf("PRAGMA user_version = %d", tc.version))
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			c := openCoreAt(t, path, emitProvider())
			pauseTestWorker(c)
			thread := lifecycleThread(t, c, "source")
			runs := lifecycleRuns(t, c, "source")
			if thread.InteractionMode != "default" || thread.ForkedFromThreadID != "" || len(runs) != 1 || runs[0].InteractionMode != "default" || runs[0].Output != "old output" {
				t.Fatalf("migrated projection: %+v %+v", thread, runs)
			}
			forkCmd := lifecycleCommand("fork", api.ForkThread, "source")
			fork := dispatchOrFail(t, c, forkCmd)
			if got := lifecycleRuns(t, c, fork.ThreadID); len(got) != 1 || got[0].Input != "old input" || got[0].Output != "old output" {
				t.Fatalf("migrated history could not fork: %+v", got)
			}
			deleteCmd := lifecycleCommand("delete", api.DeleteThread, "source")
			deleted := dispatchOrFail(t, c, deleteCmd)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			c = openCoreAt(t, path, emitProvider())
			defer c.Close()
			assertCommandReplay(t, c, forkCmd, fork)
			assertCommandReplay(t, c, deleteCmd, deleted)
			if countRows(t, c, "queued_messages") != 0 || countRows(t, c, "steering_messages") != 0 || countRows(t, c, "inputs") != 0 {
				t.Fatal("deletion retained or resurrected legacy dependants")
			}
			var version int
			if err := c.store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
				t.Fatalf("migration version: %d %v", version, err)
			}
			for _, column := range [][2]string{{"threads", "interaction_mode"}, {"threads", "forked_from_thread_id"}, {"threads", "forked_from_run_id"}, {"runs", "interaction_mode"}, {"inputs", "queue_order"}} {
				if !databaseColumn(t, path, column[0], column[1]) {
					t.Fatalf("missing %s.%s", column[0], column[1])
				}
			}
		})
	}
}

func TestLegacyQueueOrderBackfillAndExplicitOrderSurviveReopen(t *testing.T) {
	for _, branch := range []struct {
		name string
		ddl  []string
	}{
		{"local-v4", []string{localV3, localV4}},
		{"unified-v7", []string{migrationV3, migrationV4, migrationV5, migrationV6, migrationV7}},
	} {
		t.Run(branch.name, func(t *testing.T) {
			fixture := buildLegacyDatabase(t, 4, branch.ddl, func(db *sql.DB) {
				insertLegacyThread(t, db, "thread", "queue")
				// Archived historical queues stay idle through Core.Open.
				mustExec(t, db, `UPDATE threads SET archived = 1 WHERE id = 'thread'`)
				// Same timestamp: the business ID is the stable FIFO tie-breaker.
				for _, id := range []string{"b", "a", "c"} {
					mustExec(t, db, `INSERT INTO queued_messages (id, thread_id, message, status, created_at)
						VALUES (?, 'thread', ?, 'pending', 1)`, id, id)
				}
			})
			c := openCoreAt(t, fixture.path, emitProvider())
			pauseTestWorker(c)
			assertQueueIDs(t, c, "thread", "a", "b", "c")
			cmd := lifecycleCommand("reorder", api.ReorderInput, "thread")
			cmd.InputID, cmd.Position = "c", 0
			receipt := dispatchOrFail(t, c, cmd)
			assertQueueIDs(t, c, "thread", "c", "a", "b")
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			c = openCoreAt(t, fixture.path, emitProvider())
			defer c.Close()
			pauseTestWorker(c)
			assertQueueIDs(t, c, "thread", "c", "a", "b")
			assertCommandReplay(t, c, cmd, receipt)
			// Starting one legacy input uses its historical defaults and the
			// persisted reorder, while keeping the rest of the queue contiguous.
			if err := c.write(context.Background(), func(tx *txn) error {
				if _, err := tx.exec(`UPDATE threads SET archived = 0, interaction_mode = 'plan' WHERE id = 'thread'`); err != nil {
					return err
				}
				_, err := c.startQueuedInput(tx, "thread")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			input := waitInputStatus(t, c, "c", inputStarted)
			run, ok := findRun(snapshotOf(t, c), input.RunID)
			if !ok || run.InteractionMode != "plan" || input.Configuration != nil {
				t.Fatalf("legacy nil configuration lost start-time defaults: %+v %+v", input, run)
			}
			assertQueueIDs(t, c, "thread", "a", "b")
		})
	}
}

func TestV7InputConfigurationWithoutModeRetainsDefault(t *testing.T) {
	fixture := buildLegacyDatabase(t, 7, []string{migrationV3, migrationV4, migrationV5, migrationV6, migrationV7}, func(db *sql.DB) {
		insertLegacyThread(t, db, "thread", "queue")
		mustExec(t, db, `UPDATE threads SET archived = 1 WHERE id = 'thread'`)
		mustExec(t, db, `INSERT INTO inputs (id, thread_id, kind, message, status, created_at, updated_at, configuration)
			VALUES ('input', 'thread', 'queue', 'old queued input', 'queued', ?, ?, '{"providerId":"test","model":"old","approvalPolicy":"auto"}')`,
			time.Now().UnixNano(), time.Now().UnixNano())
	})
	c := openCoreAt(t, fixture.path, emitProvider())
	defer c.Close()
	pauseTestWorker(c)
	if err := c.write(context.Background(), func(tx *txn) error {
		if _, err := tx.exec(`UPDATE threads SET archived = 0, interaction_mode = 'plan' WHERE id = 'thread'`); err != nil {
			return err
		}
		_, err := c.startQueuedInput(tx, "thread")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	input := waitInputStatus(t, c, "input", inputStarted)
	run, ok := findRun(snapshotOf(t, c), input.RunID)
	if !ok || run.InteractionMode != "default" || run.Model != "old" {
		t.Fatalf("v7 captured configuration changed semantics: %+v", run)
	}
}
