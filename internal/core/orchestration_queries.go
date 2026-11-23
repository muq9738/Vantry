package core

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

// Every query in a broker response shares one WAL snapshot.
func (c *Core) orchestrationRead(ctx context.Context, fn func(*txn) error) error {
	c.lifeMu.RLock()
	defer c.lifeMu.RUnlock()
	if c.state.Load() == stateStopped {
		return errorf(api.NotReady, "core is stopped")
	}
	tx, err := c.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return wrapInternal("begin orchestration query", err)
	}
	defer tx.Rollback()
	if err := fn(&txn{ctx: ctx, tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func requiredThread(t *txn, id string) (threadRecord, error) {
	if strings.TrimSpace(id) == "" {
		return threadRecord{}, errorf(api.InvalidCommand, "threadId is required")
	}
	thread, err := loadThread(t, id)
	if errors.Is(err, sql.ErrNoRows) {
		return threadRecord{}, errorf(api.NotFound, "thread %q not found", id)
	}
	if err != nil {
		return threadRecord{}, wrapInternal("read thread", err)
	}
	return thread, nil
}

func readRecords[T any](t *txn, query string, scan func(scanner) (T, error), args ...any) ([]T, error) {
	rows, err := t.query(query, args...)
	if err != nil {
		return nil, wrapInternal("query orchestration records", err)
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		record, err := scan(rows)
		if err != nil {
			return nil, wrapInternal("scan orchestration record", err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func projectionSequence(t *txn) (int64, error) {
	var sequence int64
	err := t.queryRow(`SELECT MAX(COALESCE(MAX(sequence), 0),
		COALESCE((SELECT int_value FROM core_metadata WHERE key = 'pruned_through_sequence'), 0)) FROM events`).Scan(&sequence)
	return sequence, err
}

func (c *Core) ServerStatus(ctx context.Context) (api.ServerStatus, error) {
	status := api.ServerStatus{ProtocolVersion: api.ProtocolVersion, State: stateName(c.state.Load()),
		SchemaVersion: schemaVersion, PID: os.Getpid(), Capabilities: []string{api.CapabilityStatus,
			api.CapabilityThreadSummaries, api.CapabilityThreadDetail, api.CapabilityInterThreadOrchestration, api.CapabilityWorkbenchV3}}
	err := c.orchestrationRead(ctx, func(t *txn) error {
		return t.queryRow(`SELECT COUNT(1) FROM runs WHERE status IN ('queued', 'running', 'waiting_children')`).Scan(&status.ActiveRuns)
	})
	return status, err
}

const threadProjectionSQL = `WITH thread_projection AS (
	SELECT t.*,
		(SELECT COUNT(1) FROM runs WHERE thread_id = t.id) AS run_count,
		(SELECT COUNT(1) FROM runs WHERE thread_id = t.id AND status IN ('queued', 'running', 'waiting_children')) AS active_runs,
		(SELECT COUNT(1) FROM inputs i WHERE i.thread_id = t.id AND (i.status IN ('queued', 'pending')
			OR (i.status = 'started' AND EXISTS (SELECT 1 FROM runs r WHERE r.id = i.run_id AND r.status IN ('queued', 'running', 'waiting_children'))))) AS pending_inputs,
		(SELECT COUNT(1) FROM provider_requests WHERE thread_id = t.id AND status IN ('pending', 'responding')) AS pending_requests,
		(SELECT COUNT(1) FROM tasks WHERE parent_thread_id = t.id AND status IN ('queued', 'running', 'waiting_children')) AS delegated_live,
		COALESCE((SELECT id FROM runs WHERE thread_id = t.id ORDER BY status IN ('queued', 'running', 'waiting_children') DESC, created_at DESC, id DESC LIMIT 1), '') AS latest_run_id,
		COALESCE((SELECT status FROM runs WHERE thread_id = t.id ORDER BY status IN ('queued', 'running', 'waiting_children') DESC, created_at DESC, id DESC LIMIT 1), 'idle') AS latest_status,
		MAX(t.created_at,
			COALESCE((SELECT MAX(updated_at) FROM runs WHERE thread_id = t.id), 0),
			COALESCE((SELECT MAX(updated_at) FROM inputs WHERE thread_id = t.id), 0),
			COALESCE((SELECT MAX(updated_at) FROM tool_calls WHERE thread_id = t.id), 0),
			COALESCE((SELECT MAX(updated_at) FROM provider_requests WHERE thread_id = t.id), 0),
			COALESCE((SELECT MAX(updated_at) FROM tasks WHERE parent_thread_id = t.id OR child_thread_id = t.id), 0),
			COALESCE((SELECT MAX(timestamp) FROM events WHERE thread_id = t.id AND type = 'thread.updated'), 0)) AS updated_at
	FROM threads t
) `

const threadListColumns = `id, title, model, approval_policy, interaction_mode, workspace,
	parent_thread_id, created_by_thread_id, pinned, snoozed_until, run_count, active_runs,
	pending_inputs, pending_requests, delegated_live, latest_status, updated_at`

func scanThreadListItem(row scanner) (api.ThreadListItem, error) {
	var item api.ThreadListItem
	var workspace string
	var snoozed, updated int64
	var active, delegated int
	err := row.Scan(&item.ThreadID, &item.Title, &item.Model, &item.ApprovalPolicy, &item.InteractionMode,
		&workspace, &item.ParentThreadID, &item.CreatedByThreadID, &item.Pinned, &snoozed,
		&item.RunCount, &active, &item.PendingInputs, &item.PendingRequests, &delegated, &item.Status, &updated)
	if err != nil {
		return item, err
	}
	item.SnoozedUntil, item.Snoozed = optionalTime(snoozed), snoozed > nowNanos()
	item.Settled = active == 0 && item.PendingInputs == 0 && item.PendingRequests == 0 && delegated == 0
	item.UpdatedAt = fromNanos(updated)
	projectPath := ""
	if spec := decodeWorkspace(workspace); spec != nil {
		item.WorkspacePath = firstNonEmpty(spec.Path, spec.Repository)
		item.Branch, item.IsWorktree = spec.Branch, spec.Kind == "worktree"
		projectPath = api.WorkspaceProjectPath(spec)
	}
	item.Link = api.FormatThreadReference(item.ThreadID, item.Title, projectPath)
	return item, nil
}

func threadListItem(t *txn, id string) (api.ThreadListItem, error) {
	return scanThreadListItem(t.queryRow(threadProjectionSQL+`SELECT `+threadListColumns+` FROM thread_projection WHERE id = ?`, id))
}

func (c *Core) ListThreads(ctx context.Context, query api.ThreadListQuery) (api.ThreadListResult, error) {
	result := api.ThreadListResult{CurrentThreadID: query.CallerThreadID, Threads: []api.ThreadListItem{}}
	if query.Cursor < 0 || query.Limit < 0 {
		return result, errorf(api.InvalidCommand, "cursor and limit must not be negative")
	}
	limit := boundedLimit(query.Limit, 25, 100)
	err := c.orchestrationRead(ctx, func(t *txn) error {
		workspace := query.Workspace
		if !query.AllProjects && workspace == "" {
			spec := c.defaultWorkspace
			if query.CallerThreadID != "" {
				caller, err := requiredThread(t, query.CallerThreadID)
				if err != nil {
					return err
				}
				spec = caller.Workspace
			}
			if spec != nil {
				workspace = firstNonEmpty(spec.Path, spec.Repository)
			}
		}
		filter, args := " WHERE 1 = 1", []any{}
		if !query.IncludeSubagents {
			filter += " AND parent_thread_id = ''"
		}
		if !query.AllProjects && workspace != "" {
			filter += ` AND (json_extract(NULLIF(workspace, ''), '$.path') = ? OR json_extract(NULLIF(workspace, ''), '$.repository') = ?)`
			args = append(args, workspace, workspace)
		}
		if query.TitleContains != "" {
			filter += ` AND instr(lower(title), lower(?)) > 0`
			args = append(args, query.TitleContains)
		}
		if query.Settled != nil {
			filter += ` AND (active_runs = 0 AND pending_inputs = 0 AND pending_requests = 0 AND delegated_live = 0) = ?`
			args = append(args, *query.Settled)
		}
		if query.Snoozed != nil {
			filter += ` AND (snoozed_until > ?) = ?`
			args = append(args, nowNanos(), *query.Snoozed)
		}
		if len(query.Statuses) > 0 {
			filter += ` AND latest_status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(query.Statuses)), ",") + `)`
			for _, status := range query.Statuses {
				args = append(args, status)
			}
		}
		if err := t.queryRow(threadProjectionSQL+`SELECT COUNT(1) FROM thread_projection`+filter, args...).Scan(&result.Total); err != nil {
			return wrapInternal("count threads", err)
		}
		args = append(args, limit, query.Cursor)
		items, err := readRecords(t, threadProjectionSQL+`SELECT `+threadListColumns+` FROM thread_projection`+filter+` ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?`, scanThreadListItem, args...)
		if err != nil {
			return err
		}
		result.Threads = items
		if next := query.Cursor + len(items); next < result.Total {
			result.NextCursor = &next
		}
		return nil
	})
	return result, err
}

func boundedLimit(value, defaultValue, maximum int) int {
	if value <= 0 {
		return defaultValue
	}
	return min(value, maximum)
}

func (c *Core) ThreadSummaries(ctx context.Context) (api.ThreadSummaries, error) {
	result := api.ThreadSummaries{Threads: []api.ThreadSummary{}}
	err := c.orchestrationRead(ctx, func(t *txn) error {
		var err error
		result.Sequence, err = projectionSequence(t)
		if err != nil {
			return err
		}
		rows, err := t.query(threadProjectionSQL + `SELECT id, title, provider_id, model, workspace,
			parent_thread_id, parent_task_id, created_by_thread_id, pinned, archived, snoozed_until, approval_policy,
			created_at, updated_at, run_count, active_runs, pending_inputs, pending_requests, latest_run_id, latest_status, project_path
			FROM thread_projection ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item api.ThreadSummary
			var created, updated, snoozed int64
			var workspace string
			if err := rows.Scan(&item.ID, &item.Title, &item.ProviderID, &item.Model, &workspace, &item.ParentThreadID,
				&item.ParentTaskID, &item.CreatedByThreadID, &item.Pinned, &item.Archived, &snoozed, &item.ApprovalPolicy,
				&created, &updated, &item.RunCount, &item.ActiveRuns, &item.PendingInputs, &item.PendingRequests, &item.LatestRunID, &item.LatestStatus, &item.ProjectPath); err != nil {
				return err
			}
			item.Workspace, item.SnoozedUntil = decodeWorkspace(workspace), optionalTime(snoozed)
			item.CreatedAt, item.UpdatedAt = fromNanos(created), fromNanos(updated)
			if item.LatestStatus == "idle" {
				item.LatestStatus = ""
			}
			result.Threads = append(result.Threads, item)
		}
		return rows.Err()
	})
	return result, err
}

func (c *Core) ThreadDetail(ctx context.Context, query api.ThreadDetailQuery) (api.ThreadDetail, error) {
	result := api.ThreadDetail{Runs: []api.Run{}, Tasks: []api.Task{}, Inputs: []api.Input{}, Tools: []api.ToolCall{}, Requests: []api.ProviderRequest{}}
	if query.LimitRuns < 0 {
		return result, errorf(api.InvalidCommand, "limitRuns must not be negative")
	}
	err := c.orchestrationRead(ctx, func(t *txn) error {
		thread, err := requiredThread(t, query.ThreadID)
		if err != nil {
			return err
		}
		result.Thread = thread.toAPI()
		result.Sequence, err = projectionSequence(t)
		if err != nil {
			return err
		}
		if err := t.queryRow(`SELECT COUNT(1) FROM runs WHERE thread_id = ?`, thread.ID).Scan(&result.RunCount); err != nil {
			return err
		}
		limit := query.LimitRuns
		if limit == 0 {
			limit = result.RunCount
		}
		runs, err := readRecords(t, `SELECT `+runColumns+` FROM runs WHERE thread_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`, scanRun, thread.ID, limit)
		if err != nil {
			return err
		}
		for i := len(runs) - 1; i >= 0; i-- {
			result.Runs = append(result.Runs, runs[i].toAPI())
		}
		result.TruncatedRuns = len(runs) < result.RunCount
		tasks, err := readRecords(t, `SELECT `+taskColumns+` FROM tasks WHERE parent_thread_id = ? OR child_thread_id = ? ORDER BY created_at, id`, scanTask, thread.ID, thread.ID)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			result.Tasks = append(result.Tasks, task.toAPI())
		}
		inputs, err := readRecords(t, `SELECT `+inputColumns+` FROM inputs WHERE thread_id = ? ORDER BY created_at, id`, scanInput, thread.ID)
		if err != nil {
			return err
		}
		for _, input := range inputs {
			result.Inputs = append(result.Inputs, input.toAPI())
		}
		tools, err := readRecords(t, `SELECT `+toolColumns+` FROM tool_calls WHERE thread_id = ? ORDER BY updated_at, id`, scanTool, thread.ID)
		if err != nil {
			return err
		}
		for _, tool := range tools {
			result.Tools = append(result.Tools, tool.toAPI())
		}
		requests, err := readRecords(t, `SELECT `+requestColumns+` FROM provider_requests WHERE thread_id = ? ORDER BY created_at, id`, scanRequest, thread.ID)
		if err != nil {
			return err
		}
		for _, request := range requests {
			result.Requests = append(result.Requests, request.toAPI())
		}
		return nil
	})
	return result, err
}
