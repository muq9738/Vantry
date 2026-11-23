package core

import (
	"context"
	"encoding/json"

	"github.com/muq9738/Vantry/pkg/provider"
)

// Stop is an external effect too: a crash after run.cancelled commits must not
// strand an independent worker before its cancellation marker was written.
func (c *Core) stopProvider(request provider.Request) {
	durable, ok := c.provider.(provider.Durable)
	if !ok {
		return
	}
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	c.lifeMu.RLock()
	if c.state.Load() == stateStopped {
		c.lifeMu.RUnlock()
		return
	}
	var pending int
	err := c.store.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM effect_outbox
		WHERE kind = ? AND run_id = ? AND attempt_id = ? AND status = ?`,
		effectKindProviderStop, request.RunID, request.AttemptID, outboxPending).Scan(&pending)
	c.lifeMu.RUnlock()
	if err != nil || pending == 0 {
		return
	}
	stopErr := durable.Stop(ctx, request)
	status, reason := outboxSettled, ""
	if stopErr != nil {
		status, reason = outboxPending, stopErr.Error()
	}
	_ = c.write(ctx, func(t *txn) error {
		if _, err := t.exec(`UPDATE effect_outbox SET status = ?, last_error = ?,
			attempt_count = attempt_count + 1, updated_at = ?
			WHERE kind = ? AND run_id = ? AND attempt_id = ? AND status = ?`,
			status, reason, nowNanos(), effectKindProviderStop, request.RunID, request.AttemptID, outboxPending); err != nil {
			return err
		}
		// A deleted session retains only undelivered stop intents. Successful
		// delivery releases that final outbox row without restoring projections.
		_, err := t.exec(`DELETE FROM effect_outbox WHERE kind = ? AND run_id = ? AND attempt_id = ?
			AND status = ? AND NOT EXISTS (SELECT 1 FROM threads WHERE id = effect_outbox.thread_id)`,
			effectKindProviderStop, request.RunID, request.AttemptID, outboxSettled)
		return err
	})
}

func (c *Core) drainStopEffects(ctx context.Context) {
	if _, ok := c.provider.(provider.Durable); !ok {
		return
	}
	rows, err := c.store.db.QueryContext(ctx, `SELECT input FROM effect_outbox
		WHERE kind = ? AND status = ? ORDER BY created_at, id LIMIT 32`, effectKindProviderStop, outboxPending)
	if err != nil {
		return
	}
	var requests []provider.Request
	for rows.Next() {
		var payload string
		if rows.Scan(&payload) != nil {
			break
		}
		var request provider.Request
		if json.Unmarshal([]byte(payload), &request) == nil {
			requests = append(requests, request)
		}
	}
	_ = rows.Close()
	for _, request := range requests {
		if ctx.Err() != nil {
			return
		}
		c.stopProvider(request)
	}
}
