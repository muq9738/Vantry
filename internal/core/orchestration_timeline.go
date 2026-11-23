package core

import (
	"context"
	"encoding/json"

	"github.com/muq9738/Vantry/pkg/api"
)

const threadReadByteBudget = 64 * 1024

func timelineSQL(activity bool) string {
	items := `SELECT r.id || ':user' AS item_id, r.id AS run_id, 'user' AS created_by,
		r.sender_thread_id, 'user_message' AS kind, r.status, '' AS title, r.input AS body,
		r.created_at AS item_time, r.created_at AS run_time, 0 AS phase, r.created_at AS updated_at
		FROM runs r JOIN run_order o ON o.id = r.id
		UNION ALL SELECT r.id || ':assistant', r.id, 'agent', '', 'assistant_message', r.status, '', r.output,
		r.updated_at, r.created_at, 5, r.updated_at FROM runs r JOIN run_order o ON o.id = r.id`
	if activity {
		items += `
		UNION ALL SELECT r.id || ':reasoning', r.id, 'agent', '', 'reasoning', r.status, '', r.reasoning,
			r.created_at, r.created_at, 1, r.updated_at FROM runs r JOIN run_order o ON o.id = r.id WHERE r.reasoning <> ''
		UNION ALL SELECT tool.id, tool.run_id, 'system', '', 'tool_call', tool.status, tool.name,
			CAST(tool.arguments AS TEXT) || char(10) || tool.output, tool.updated_at, o.created_at, 2, tool.updated_at
			FROM tool_calls tool JOIN run_order o ON o.id = tool.run_id
		UNION ALL SELECT request.id, request.run_id, 'system', '', 'approval_request', request.status, request.title,
			request.message || char(10) || request.options, request.created_at, o.created_at, 3, request.updated_at
			FROM provider_requests request JOIN run_order o ON o.id = request.run_id
		UNION ALL SELECT task.id, task.parent_run_id, 'agent', task.child_thread_id, 'subagent', task.status, task.title,
			task.result || char(10) || task.error, task.created_at, o.created_at, 4, task.updated_at
			FROM tasks task JOIN run_order o ON o.id = task.parent_run_id
		UNION ALL SELECT r.id || ':error', r.id, 'system', '', 'error', r.status, '', r.error,
			r.updated_at, r.created_at, 6, r.updated_at FROM runs r JOIN run_order o ON o.id = r.id WHERE r.error <> ''`
	}
	return `WITH thread_scope AS (SELECT ? AS id),
		run_order AS (SELECT id, created_at FROM runs WHERE thread_id = (SELECT id FROM thread_scope)),
		timeline AS (` + items + `), numbered AS (
		SELECT ROW_NUMBER() OVER (ORDER BY run_time, run_id, phase, item_time, item_id) - 1 AS position, * FROM timeline
	) `
}

func clipRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}

func timelineSize(result api.ThreadReadResult) int {
	raw, _ := json.Marshal(result)
	return len(raw)
}

func (c *Core) ReadThread(ctx context.Context, query api.ThreadReadQuery) (api.ThreadReadResult, error) {
	result := api.ThreadReadResult{Items: []api.ThreadTimelineItem{}, RecentRuns: []api.ThreadRunSummary{}}
	view := firstNonEmpty(query.View, "messages")
	if view != "messages" && view != "activity" {
		return result, errorf(api.InvalidCommand, "view must be messages or activity")
	}
	if query.Limit < 0 || query.RunLimit < 0 || query.MaxCharsPerItem < 0 || query.TextOffset < 0 ||
		query.AfterPosition != nil && *query.AfterPosition < 0 || query.BeforePosition != nil && *query.BeforePosition < 0 {
		return result, errorf(api.InvalidCommand, "timeline limits and positions must not be negative")
	}
	if query.AfterPosition != nil && query.BeforePosition != nil && *query.AfterPosition >= *query.BeforePosition {
		return result, errorf(api.InvalidCommand, "afterPosition must precede beforePosition")
	}
	limit, chars := boundedLimit(query.Limit, 20, 100), boundedLimit(query.MaxCharsPerItem, 4000, 30000)
	newestFirst := query.AfterPosition == nil
	var observed []api.TaskDetail
	err := c.orchestrationRead(ctx, func(t *txn) error {
		thread, err := requiredThread(t, query.ThreadID)
		if err != nil {
			return err
		}
		result.Thread, err = threadListItem(t, thread.ID)
		if err != nil {
			return err
		}
		result.Thread.Title = clipRunes(result.Thread.Title, 256)
		project := firstNonEmpty(thread.ProjectPath, api.WorkspaceProjectPath(thread.Workspace))
		result.Thread.Link = api.FormatThreadReference(thread.ID, result.Thread.Title, project)
		result.Thread.Model = clipRunes(result.Thread.Model, 512)
		result.Thread.WorkspacePath = clipRunes(result.Thread.WorkspacePath, 4096)
		result.Thread.Branch = clipRunes(result.Thread.Branch, 256)
		runRows, err := t.query(`SELECT id, ordinal, status, model, sender_thread_id, created_at, updated_at FROM (
			SELECT id, ROW_NUMBER() OVER (ORDER BY created_at, id) AS ordinal, status,
				substr(model, 1, 512) AS model, sender_thread_id, created_at, updated_at FROM runs WHERE thread_id = ?
			) ORDER BY ordinal DESC LIMIT ?`, thread.ID, boundedLimit(query.RunLimit, 10, 100))
		if err != nil {
			return err
		}
		for runRows.Next() {
			var run api.ThreadRunSummary
			var created, updated int64
			if err := runRows.Scan(&run.RunID, &run.Ordinal, &run.Status, &run.Model, &run.SenderThreadID, &created, &updated); err != nil {
				runRows.Close()
				return err
			}
			run.CreatedAt, run.UpdatedAt = fromNanos(created), fromNanos(updated)
			result.RecentRuns = append(result.RecentRuns, run)
		}
		err = runRows.Err()
		runRows.Close()
		if err != nil {
			return err
		}
		for i, j := 0, len(result.RecentRuns)-1; i < j; i, j = i+1, j-1 {
			result.RecentRuns[i], result.RecentRuns[j] = result.RecentRuns[j], result.RecentRuns[i]
		}
		for len(result.RecentRuns) > 0 && timelineSize(result) > threadReadByteBudget/2 {
			result.RecentRuns = result.RecentRuns[1:]
		}
		cte := timelineSQL(view == "activity")
		var total int
		if err := t.queryRow(cte+`SELECT COUNT(1) FROM numbered`, thread.ID).Scan(&total); err != nil {
			return err
		}
		filter, args := " WHERE 1 = 1", []any{thread.ID, query.TextOffset + 1, chars}
		if query.AfterPosition != nil {
			filter += " AND position > ?"
			args = append(args, *query.AfterPosition)
		}
		if query.BeforePosition != nil {
			filter += " AND position < ?"
			args = append(args, *query.BeforePosition)
		}
		if query.ItemID != "" {
			filter += " AND item_id = ?"
			args = append(args, query.ItemID)
		}
		order := " ASC"
		if newestFirst {
			order = " DESC"
		}
		args = append(args, limit)
		rows, err := t.query(cte+`SELECT position, item_id, run_id, created_by, sender_thread_id, kind,
			status, substr(title, 1, 256), substr(body, ?, ?), length(body), updated_at FROM numbered`+filter+` ORDER BY position`+order+` LIMIT ?`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item api.ThreadTimelineItem
			var length int
			var updated int64
			if err := rows.Scan(&item.Position, &item.ItemID, &item.RunID, &item.CreatedBy, &item.SenderThreadID,
				&item.Type, &item.Status, &item.Title, &item.Text, &length, &updated); err != nil {
				rows.Close()
				return err
			}
			item.UpdatedAt = fromNanos(updated)
			next := min(length, query.TextOffset+len([]rune(item.Text)))
			if next < length {
				item.TextTruncated, item.NextTextOffset = true, &next
			}
			result.Items = append(result.Items, item)
			if timelineSize(result) > threadReadByteBudget-512 {
				result.Items = result.Items[:len(result.Items)-1]
				if len(result.Items) > 0 {
					break
				}
				// Even a single escaped JSON string can exceed the budget. Reduce
				// only at rune boundaries and retain the precise continuation offset.
				runes := []rune(item.Text)
				low, high := 0, len(runes)
				for low < high {
					mid := (low + high + 1) / 2
					item.Text = string(runes[:mid])
					next := query.TextOffset + mid
					item.TextTruncated, item.NextTextOffset = true, &next
					result.Items = []api.ThreadTimelineItem{item}
					if timelineSize(result) <= threadReadByteBudget-512 {
						low = mid
					} else {
						high = mid - 1
					}
				}
				item.Text = string(runes[:low])
				next := query.TextOffset + low
				item.TextTruncated, item.NextTextOffset = true, &next
				result.Items = []api.ThreadTimelineItem{item}
				break
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if newestFirst {
			for i, j := 0, len(result.Items)-1; i < j; i, j = i+1, j-1 {
				result.Items[i], result.Items[j] = result.Items[j], result.Items[i]
			}
		}
		if len(result.Items) > 0 {
			first, last := result.Items[0].Position, result.Items[len(result.Items)-1].Position
			if first > 0 {
				result.EarlierPosition = &first
			}
			if last < total-1 {
				result.NextPosition = &last
			}
			result.HasMore = result.EarlierPosition != nil || result.NextPosition != nil
			if query.ItemID != "" {
				result.HasMore = result.Items[0].TextTruncated
			}
		}
		if query.CallerThreadID != "" && thread.ParentThreadID == query.CallerThreadID {
			for _, item := range result.Items {
				if item.Type != "assistant_message" || !api.RunStatus(item.Status).Terminal() {
					continue
				}
				ids, err := readRecords(t, `SELECT `+taskColumns+` FROM tasks WHERE child_run_id = ? AND parent_thread_id = ?`, scanTask, item.RunID, query.CallerThreadID)
				if err != nil {
					return err
				}
				for _, task := range ids {
					observed = append(observed, api.TaskDetail{TaskID: task.ID, ChildRunID: item.RunID, Status: api.RunStatus(item.Status)})
				}
			}
		}
		return nil
	})
	if err == nil && len(observed) > 0 {
		err = c.observeTaskResults(ctx, observed)
	}
	return result, err
}
