package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

// dispatchUpdateThread is the thread-management command: rename, pin, archive
// and (while the thread is idle) model or interaction-mode changes. It complements thread.model,
// which is the transactional "subsequent messages only" switch and never
// touches an execution that is already running.
//
// A model, interaction-mode or archive change is refused while the thread owns an active
// run or an unstarted queue item, so no accepted configuration can be silently
// applied to work that was created under the previous one.
func (c *Core) dispatchUpdateThread(ctx context.Context, cmd api.Command) (api.Receipt, error) {
	if strings.TrimSpace(cmd.ThreadID) == "" {
		return api.Receipt{}, errorf(api.InvalidCommand, "threadId is required for %s", api.UpdateThread)
	}

	var receipt api.Receipt
	err := c.dispatchWrite(ctx, func(t *txn) error {
		prior, found, err := lookupReceipt(t, cmd)
		if err != nil {
			return err
		}
		if found {
			receipt = prior
			return nil
		}

		thread, err := loadThread(t, cmd.ThreadID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "thread %q not found", cmd.ThreadID)
		}
		if err != nil {
			return wrapInternal("read thread", err)
		}

		title := thread.Title
		if cmd.Title != "" {
			title = strings.TrimSpace(cmd.Title)
			if title == "" {
				return errorf(api.InvalidCommand, "thread title must not be empty")
			}
		}
		model := thread.Model
		providerID := thread.ProviderID
		mode := thread.InteractionMode
		if cmd.Model != "" || cmd.InteractionMode != "" || cmd.Archived != nil && *cmd.Archived {
			active, err := activeRunID(t, thread.ID)
			if err != nil {
				return err
			}
			if active != "" {
				return errorf(api.Conflict,
					"thread %q has active run %q; the model, interaction mode and archive state cannot change until it settles",
					thread.ID, active)
			}
			queued, err := countQueuedInputs(t, thread.ID)
			if err != nil {
				return err
			}
			if queued > 0 {
				return errorf(api.Conflict,
					"thread %q still has %d queued message(s); cancel them before changing its model, interaction mode or archive state",
					thread.ID, queued)
			}
			if cmd.Model != "" {
				providerID, err = c.switchedProvider(thread, cmd.Model, cmd.ProviderID)
				if err != nil {
					return err
				}
				model = cmd.Model
			}
			if cmd.InteractionMode != "" {
				mode, err = normalizeInteractionMode(cmd.InteractionMode)
				if err != nil {
					return err
				}
			}
		}
		pinned := thread.Pinned
		if cmd.Pinned != nil {
			pinned = *cmd.Pinned
		}
		archived := thread.Archived
		if cmd.Archived != nil {
			archived = *cmd.Archived
		}
		snoozedUntil := timeNanos(thread.SnoozedUntil)
		if cmd.SnoozedUntil != nil {
			snoozedUntil = timeNanos(cmd.SnoozedUntil)
			thread.SnoozedUntil = optionalTime(snoozedUntil)
		}
		if _, err := t.exec(`UPDATE threads SET title = ?, model = ?, provider_id = ?, pinned = ?, archived = ?, interaction_mode = ?, snoozed_until = ? WHERE id = ?`,
			title, model, providerID, pinned, archived, mode, snoozedUntil, thread.ID); err != nil {
			return wrapInternal("update thread", err)
		}
		thread.Title, thread.Model, thread.ProviderID, thread.Pinned, thread.Archived = title, model, providerID, pinned, archived
		thread.InteractionMode = mode
		ev, err := t.event(eventThreadUpdated, thread.ID, "", "", threadEventData{Thread: thread.toAPI()})
		if err != nil {
			return err
		}
		receipt = api.Receipt{
			CommandID: cmd.CommandID,
			Accepted:  true,
			ThreadID:  thread.ID,
			Sequence:  ev.Sequence,
		}
		return insertReceipt(t, cmd, receipt)
	})
	if err != nil {
		return api.Receipt{}, err
	}
	return receipt, nil
}

// countQueuedInputs counts messages that have not started yet. A cancelled or
// already started input no longer blocks a configuration change.
func countQueuedInputs(t *txn, threadID string) (int, error) {
	var count int
	if err := t.queryRow(`SELECT COUNT(1) FROM inputs WHERE thread_id = ? AND kind = ? AND status = ?`,
		threadID, inputKindQueue, inputQueued).Scan(&count); err != nil {
		return 0, wrapInternal("count queued inputs", err)
	}
	return count, nil
}
