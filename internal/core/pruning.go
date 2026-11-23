package core

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/muq9738/Vantry/pkg/api"
)

// initCoreMetadata is independent of projection migrations so existing schema
// versions gain retention metadata without competing for a migration number.
func initCoreMetadata(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS core_metadata (
			key TEXT PRIMARY KEY,
			int_value INTEGER,
			text_value TEXT
		);
		INSERT OR IGNORE INTO core_metadata(key, int_value)
		VALUES ('pruned_through_sequence', 0);`)
	if err != nil {
		return fmt.Errorf("core: initialize retention metadata: %w", err)
	}
	return nil
}

// PruneEvents removes the historical prefix covered by durable projections,
// retaining the most recent keepRecent sequence positions. This includes old
// events for active runs: their committed state already lives in projections.
// Neither projections nor command receipts are removed. A zero retention is
// allowed; the metadata preserves the high-water even when the log is empty.
func (c *Core) PruneEvents(ctx context.Context, keepRecent int) (prunedCount int64, prunedThrough int64, err error) {
	if keepRecent < 0 {
		return 0, 0, errorf(api.InvalidCommand, "keepRecent must not be negative")
	}
	var count, through int64
	err = c.write(ctx, func(t *txn) error {
		if state := c.state.Load(); state >= stateDraining {
			return errorf(api.NotReady, "core is %s", stateName(state))
		}
		if err := t.queryRow(`SELECT int_value FROM core_metadata
			WHERE key = 'pruned_through_sequence'`).Scan(&through); err != nil {
			return wrapInternal("read pruning boundary", err)
		}
		var highWater int64
		if err := t.queryRow(`SELECT MAX(COALESCE(MAX(sequence), 0), ?) FROM events`, through).Scan(&highWater); err != nil {
			return wrapInternal("read event high-water", err)
		}
		cutoff := highWater - int64(keepRecent)
		if cutoff <= through {
			return nil
		}
		var deletedThrough int64
		if err := t.queryRow(`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE sequence <= ?`, cutoff).Scan(&deletedThrough); err != nil {
			return wrapInternal("read pruning cutoff", err)
		}
		if deletedThrough <= through {
			return nil
		}
		result, err := t.exec(`DELETE FROM events WHERE sequence <= ?`, deletedThrough)
		if err != nil {
			return wrapInternal("prune events", err)
		}
		count, err = result.RowsAffected()
		if err != nil {
			return wrapInternal("count pruned events", err)
		}
		through = deletedThrough
		if _, err := t.exec(`UPDATE core_metadata SET int_value = ?
			WHERE key = 'pruned_through_sequence'`, through); err != nil {
			return wrapInternal("save pruning boundary", err)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return count, through, nil
}

// hasPrunableEvents cheaply checks with a read query whether the event log has
// grown past keepRecent since the last pruning boundary.
func (c *Core) hasPrunableEvents(ctx context.Context, keepRecent int) bool {
	if keepRecent < 0 {
		return false
	}
	c.lifeMu.RLock()
	defer c.lifeMu.RUnlock()
	if c.state.Load() == stateStopped {
		return false
	}
	var highWater, through int64
	if err := c.store.db.QueryRowContext(ctx, `SELECT
		MAX(COALESCE(MAX(sequence), 0), COALESCE((SELECT int_value FROM core_metadata WHERE key = 'pruned_through_sequence'), 0)),
		COALESCE((SELECT int_value FROM core_metadata WHERE key = 'pruned_through_sequence'), 0)
		FROM events`).Scan(&highWater, &through); err != nil {
		return false
	}
	return highWater-int64(keepRecent) > through
}
