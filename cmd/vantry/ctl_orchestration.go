package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// commandIdentity is the idempotency identity of a CLI-created command. A
// repeated --command-id replays the stored receipt instead of acting twice.
func commandIdentity(given string) string {
	if given != "" {
		return given
	}
	return "cmd_" + rand.Text()
}

// providedBool reports a filter as unset unless the operator named the flag, so
// --settled can stay unset while --settled=false narrows to live threads.
func providedBool(set *flag.FlagSet, name string, value bool) *bool {
	found := false
	set.Visit(func(visited *flag.Flag) { found = found || visited.Name == name })
	if !found {
		return nil
	}
	return &value
}

// providedInt keeps a cursor unset unless the flag was given: position 0 is a
// real timeline position and must stay distinguishable from "not requested".
func providedInt(set *flag.FlagSet, name string, value int) *int {
	found := false
	set.Visit(func(visited *flag.Flag) { found = found || visited.Name == name })
	if !found {
		return nil
	}
	return &value
}

// snoozeDeadline resolves exactly one of --duration or --until into the UTC
// deadline Core stores for the thread.
func snoozeDeadline(until string, duration time.Duration, now time.Time) (time.Time, error) {
	if (strings.TrimSpace(until) == "") == (duration == 0) {
		return time.Time{}, errors.New("snooze 需要 --duration 或 --until 之一")
	}
	if until != "" {
		deadline, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return time.Time{}, fmt.Errorf("until 需要 RFC3339 时间：%w", err)
		}
		return deadline.UTC(), nil
	}
	if duration < 0 {
		return time.Time{}, errors.New("duration 必须为正数")
	}
	return now.Add(duration).UTC(), nil
}

// listThreads prefers Core's authoritative broker and otherwise rebuilds the
// same listing shape from the committed snapshot, so the CLI works against any
// api.Service.
func listThreads(ctx context.Context, service api.Service, query api.ThreadListQuery) (api.ThreadListResult, error) {
	if broker, ok := service.(api.OrchestrationBroker); ok {
		return broker.ListThreads(ctx, query)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil {
		return api.ThreadListResult{}, err
	}
	return snapshotThreadList(snapshot, query, time.Now()), nil
}

// readThread prefers Core's paginated timeline and otherwise formats the
// committed runs of the thread from the snapshot.
func readThread(ctx context.Context, service api.Service, query api.ThreadReadQuery) (api.ThreadReadResult, error) {
	if broker, ok := service.(api.OrchestrationBroker); ok {
		return broker.ReadThread(ctx, query)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil {
		return api.ThreadReadResult{}, err
	}
	return snapshotThreadRead(snapshot, query, time.Now())
}

// sendToThread delivers an inter-session message. Core's broker owns delivery
// semantics when it is available; otherwise the durable command is dispatched
// so a plain api.Service still accepts the send.
func sendToThread(ctx context.Context, service api.Service, request api.InterThreadSendRequest) (any, error) {
	if broker, ok := service.(api.OrchestrationBroker); ok {
		return broker.SendToThread(ctx, request)
	}
	command := api.Command{
		Version: api.ProtocolVersion, CommandID: request.ClientRequestID, Kind: api.InterThreadSend,
		ThreadID: request.TargetThreadID, TargetThreadID: request.TargetThreadID,
		SenderThreadID: request.CallerThreadID, Message: request.Message, DeliveryMode: request.Mode,
	}
	return service.Dispatch(ctx, command)
}

// waitThread blocks until the target run is terminal. The broker owns the
// event-driven wake-up; the fallback watches the same committed run events over
// a subscription and never polls.
func waitThread(ctx context.Context, service api.Service, query api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
	if broker, ok := service.(api.OrchestrationBroker); ok {
		return broker.WaitThread(ctx, query)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil {
		return api.ThreadWaitResult{}, err
	}
	thread, found := snapshotThread(snapshot, query.ThreadID)
	if !found {
		return api.ThreadWaitResult{}, fmt.Errorf("找不到线程 %q", query.ThreadID)
	}
	runs := sortedRuns(snapshot.Runs, thread.ID)
	result := api.ThreadWaitResult{ThreadID: thread.ID, Link: api.ThreadReference(thread)}
	target, live := query.RunID, true
	if target == "" {
		if run, ok := latestLiveRun(runs); ok {
			target = run.ID
		} else {
			live = false
		}
	}
	if target == "" || !live {
		// Nothing is running: report the settled state instead of waiting.
		if run, ok := latestRun(runs); ok {
			result = describeThreadRun(result, run)
		} else {
			result.Status = "idle"
		}
		return result, nil
	}
	exists := false
	for _, run := range runs {
		if run.ID != target {
			continue
		}
		exists = true
		if run.Status.Terminal() {
			return describeThreadRun(result, run), nil
		}
	}
	if !exists {
		return api.ThreadWaitResult{}, fmt.Errorf("线程 %q 没有运行 %q", thread.ID, target)
	}
	result.RunID, result.Status = target, string(api.RunRunning)
	subscription, err := service.Subscribe(ctx, snapshot.Sequence)
	if err != nil {
		return api.ThreadWaitResult{}, err
	}
	defer subscription.Close()
	timeout := time.Duration(query.TimeoutMs) * time.Millisecond
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	events, failures := subscription.Events, subscription.Errors
	for events != nil || failures != nil {
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return api.ThreadWaitResult{}, ctx.Err()
			}
			result.TimedOut = true
			result.Next = "线程仍在运行；稍后重新 thread-wait 或使用 thread-read 查看进度。"
			return result, nil
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event.ThreadID != thread.ID || event.RunID != target {
				continue
			}
			status, terminal := terminalRunStatus(event.Type)
			if !terminal {
				continue
			}
			result.RunID, result.Status = target, string(status)
			applyRunEventData(&result, event.Data)
			return result, nil
		case err, ok := <-failures:
			if !ok {
				failures = nil
				continue
			}
			if err != nil {
				return api.ThreadWaitResult{}, err
			}
		}
	}
	return api.ThreadWaitResult{}, errors.New("Core 事件连接已关闭；重新查询快照后重试")
}

// terminalRunStatus maps a committed run event to the run status it announced.
func terminalRunStatus(eventType string) (api.RunStatus, bool) {
	switch eventType {
	case "run.completed":
		return api.RunCompleted, true
	case "run.cancelled":
		return api.RunCancelled, true
	case "run.interrupted":
		return api.RunInterrupted, true
	case "run.failed":
		return api.RunFailed, true
	}
	return "", false
}

// applyRunEventData copies the settled run's outcome out of its event payload.
func applyRunEventData(result *api.ThreadWaitResult, data json.RawMessage) {
	var payload struct {
		Run struct {
			ID     string `json:"id"`
			Output string `json:"output"`
			Error  string `json:"error"`
		} `json:"run"`
	}
	if len(data) == 0 || json.Unmarshal(data, &payload) != nil {
		return
	}
	if payload.Run.ID != "" {
		result.RunID = payload.Run.ID
	}
	result.Summary, result.Error = summaryText(payload.Run.Output), payload.Run.Error
}

// describeThreadRun reports one committed run as a wait result.
func describeThreadRun(result api.ThreadWaitResult, run api.Run) api.ThreadWaitResult {
	result.RunID, result.Status = run.ID, string(run.Status)
	result.Summary, result.Error = summaryText(run.Output), run.Error
	return result
}

// summaryText keeps the CLI result readable: the reply body is trimmed and
// bounded, and the authoritative text stays in the thread history.
func summaryText(text string) string {
	trimmed := strings.TrimSpace(text)
	const limit = 1000
	runes := []rune(trimmed)
	if len(runes) <= limit {
		return trimmed
	}
	return string(runes[:limit]) + "…"
}

// snapshotThreadList reconstructs Core's thread listing from a snapshot. A
// thread counts as settled when it is archived or owns no live run, which is
// the same grouping the desktop shelves use.
func snapshotThreadList(snapshot api.Snapshot, query api.ThreadListQuery, now time.Time) api.ThreadListResult {
	runs := groupThreadRuns(snapshot.Runs)
	inputs := countPendingInputs(snapshot.Inputs)
	requests := countPendingRequests(snapshot.Requests)
	items := make([]api.ThreadListItem, 0, len(snapshot.Threads))
	for _, thread := range snapshot.Threads {
		item := snapshotThreadItem(thread, runs[thread.ID], inputs[thread.ID], requests[thread.ID], now)
		if query.Settled != nil && item.Settled != *query.Settled {
			continue
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	total := len(items)
	if query.Limit > 0 && len(items) > query.Limit {
		items = items[:query.Limit]
	}
	return api.ThreadListResult{CurrentThreadID: query.CallerThreadID, Threads: items, Total: total}
}

// snapshotThreadRead formats the committed runs of one thread. The snapshot
// carries no timeline positions, so only the run summaries are reconstructed.
func snapshotThreadRead(snapshot api.Snapshot, query api.ThreadReadQuery, now time.Time) (api.ThreadReadResult, error) {
	thread, found := snapshotThread(snapshot, query.ThreadID)
	if !found {
		return api.ThreadReadResult{}, fmt.Errorf("找不到线程 %q", query.ThreadID)
	}
	runs := sortedRuns(snapshot.Runs, thread.ID)
	limit := query.Limit
	if limit <= 0 {
		limit = 5
	}
	summaries := make([]api.ThreadRunSummary, 0, limit)
	for index := len(runs) - 1; index >= 0 && len(summaries) < limit; index-- {
		run := runs[index]
		summaries = append(summaries, api.ThreadRunSummary{
			RunID: run.ID, Ordinal: index + 1, Status: run.Status, Model: run.Model,
			SenderThreadID: run.SenderThreadID, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
		})
	}
	item := snapshotThreadItem(thread, runs, countPendingInputs(snapshot.Inputs)[thread.ID],
		countPendingRequests(snapshot.Requests)[thread.ID], now)
	return api.ThreadReadResult{Thread: item, RecentRuns: summaries}, nil
}

func snapshotThread(snapshot api.Snapshot, threadID string) (api.Thread, bool) {
	for _, thread := range snapshot.Threads {
		if thread.ID == threadID {
			return thread, true
		}
	}
	return api.Thread{}, false
}

// snapshotThreadItem projects one snapshot thread into the listing shape.
func snapshotThreadItem(thread api.Thread, runs []api.Run, pendingInputs, pendingRequests int, now time.Time) api.ThreadListItem {
	item := api.ThreadListItem{
		ThreadID: thread.ID, Title: thread.Title, Link: api.ThreadReference(thread),
		Status: "idle", Model: thread.Model, ApprovalPolicy: thread.ApprovalPolicy,
		InteractionMode: thread.InteractionMode, ParentThreadID: thread.ParentThreadID,
		CreatedByThreadID: thread.CreatedByThreadID, Pinned: thread.Pinned,
		RunCount: len(runs), PendingInputs: pendingInputs, PendingRequests: pendingRequests,
		UpdatedAt: thread.CreatedAt,
	}
	if thread.Workspace != nil {
		item.WorkspacePath, item.Branch = thread.Workspace.Path, thread.Workspace.Branch
		item.IsWorktree = thread.Workspace.Kind == "worktree"
	}
	if thread.SnoozedUntil != nil {
		item.SnoozedUntil = thread.SnoozedUntil
		item.Snoozed = thread.SnoozedUntil.After(now)
	}
	live := false
	for _, run := range runs {
		if run.UpdatedAt.After(item.UpdatedAt) {
			item.UpdatedAt = run.UpdatedAt
		}
		live = live || !run.Status.Terminal()
	}
	if run, ok := latestRun(runs); ok {
		item.Status = string(run.Status)
	}
	item.Settled = thread.Archived || !live
	return item
}

// groupThreadRuns indexes runs per thread in creation order.
func groupThreadRuns(runs []api.Run) map[string][]api.Run {
	grouped := make(map[string][]api.Run, len(runs))
	for _, run := range runs {
		grouped[run.ThreadID] = append(grouped[run.ThreadID], run)
	}
	for threadID := range grouped {
		sort.SliceStable(grouped[threadID], func(i, j int) bool {
			left, right := grouped[threadID][i], grouped[threadID][j]
			if !left.CreatedAt.Equal(right.CreatedAt) {
				return left.CreatedAt.Before(right.CreatedAt)
			}
			return left.ID < right.ID
		})
	}
	return grouped
}

// sortedRuns returns one thread's runs in creation order.
func sortedRuns(runs []api.Run, threadID string) []api.Run {
	owned := make([]api.Run, 0, len(runs))
	for _, run := range runs {
		if run.ThreadID == threadID {
			owned = append(owned, run)
		}
	}
	sort.SliceStable(owned, func(i, j int) bool {
		if !owned[i].CreatedAt.Equal(owned[j].CreatedAt) {
			return owned[i].CreatedAt.Before(owned[j].CreatedAt)
		}
		return owned[i].ID < owned[j].ID
	})
	return owned
}

func latestRun(runs []api.Run) (api.Run, bool) {
	if len(runs) == 0 {
		return api.Run{}, false
	}
	return runs[len(runs)-1], true
}

func latestLiveRun(runs []api.Run) (api.Run, bool) {
	for index := len(runs) - 1; index >= 0; index-- {
		if !runs[index].Status.Terminal() {
			return runs[index], true
		}
	}
	return api.Run{}, false
}

func countPendingInputs(inputs []api.Input) map[string]int {
	counts := make(map[string]int, len(inputs))
	for _, input := range inputs {
		if input.Status == "queued" || input.Status == "pending" {
			counts[input.ThreadID]++
		}
	}
	return counts
}

func countPendingRequests(requests []api.ProviderRequest) map[string]int {
	counts := make(map[string]int, len(requests))
	for _, request := range requests {
		if request.Status == "pending" || request.Status == "responding" {
			counts[request.ThreadID]++
		}
	}
	return counts
}
