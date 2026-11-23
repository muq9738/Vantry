package pr

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// WakeReason categorizes why a watched Pull Request woke its bound thread.
type WakeReason string

const (
	WakeReasonChecksFailed     WakeReason = "checks_failed"
	WakeReasonChangesRequested WakeReason = "changes_requested"
	WakeReasonNewComment       WakeReason = "new_comment"
	WakeReasonMergeConflict    WakeReason = "merge_conflict"
	WakeReasonMerged           WakeReason = "pr_merged"
)

// WakeTrigger describes an actionable PR state transition to deliver to a thread.
type WakeTrigger struct {
	ThreadID        string       `json:"threadId,omitempty"`
	PRNumber        int          `json:"prNumber"`
	PRURL           string       `json:"prUrl"`
	PrimaryReason   WakeReason   `json:"primaryReason"`
	Reasons         []WakeReason `json:"reasons"`
	ShouldSettle    bool         `json:"shouldSettle"`
	NewFailedChecks []string     `json:"newFailedChecks,omitempty"`
	LatestCommentID string       `json:"latestCommentId,omitempty"`
	WakePrompt      string       `json:"wakePrompt"`
}

// WatchSnapshot stores the last notified/observed business fields for a watched PR.
type WatchSnapshot struct {
	ThreadID         string    `json:"threadId"`
	PRNumber         int       `json:"prNumber"`
	PRURL            string    `json:"prUrl,omitempty"`
	HeadRefOID       string    `json:"headRefOid,omitempty"`
	LastState        string    `json:"lastState"`
	FailedChecks     []string  `json:"failedChecks,omitempty"`
	ReviewDecision   string    `json:"reviewDecision,omitempty"`
	MergeStateStatus string    `json:"mergeStateStatus,omitempty"`
	LatestCommentID  string    `json:"latestCommentId,omitempty"`
	Settled          bool      `json:"settled,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt,omitempty"`
}

// EvaluateTransition compares previous and current PR business snapshots and
// returns a WakeTrigger only when a newly actionable transition occurred.
func EvaluateTransition(prev, curr *PRInfo) *WakeTrigger {
	if curr == nil {
		return nil
	}
	if prev != nil && prev.Number > 0 && curr.Number > 0 && prev.Number != curr.Number {
		prev = nil
	}

	currState := strings.ToUpper(strings.TrimSpace(curr.State))
	prevState := ""
	if prev != nil {
		prevState = strings.ToUpper(strings.TrimSpace(prev.State))
	}

	// 1. Merged PR settles the watching thread once.
	if currState == "MERGED" {
		if prevState == "MERGED" {
			return nil
		}
		return &WakeTrigger{
			PRNumber:      curr.Number,
			PRURL:         curr.URL,
			PrimaryReason: WakeReasonMerged,
			Reasons:       []WakeReason{WakeReasonMerged},
			ShouldSettle:  true,
			WakePrompt:    formatMergedWakePrompt(curr),
		}
	}

	// Closed (unmerged) PRs do not trigger CI/review/conflict wakeups.
	if currState == "CLOSED" {
		return nil
	}

	var (
		reasons         []WakeReason
		newFailedChecks []string
		commentID       string
	)

	// 2. Newly failed status checks (or failures on a newly pushed commit).
	currFailed := normalizeStringSet(curr.ChecksSummary.FailedNames)
	var prevFailed []string
	headAdvanced := false
	if prev != nil {
		prevFailed = normalizeStringSet(prev.ChecksSummary.FailedNames)
		headAdvanced = prev.HeadRefOID != "" && curr.HeadRefOID != "" && prev.HeadRefOID != curr.HeadRefOID
	}
	if prev == nil || headAdvanced {
		newFailedChecks = currFailed
	} else {
		for _, name := range currFailed {
			if !slices.Contains(prevFailed, name) {
				newFailedChecks = append(newFailedChecks, name)
			}
		}
	}
	unnamedFailureIncrease := len(currFailed) == 0 && curr.ChecksSummary.Failed > 0 &&
		(prev == nil || headAdvanced || curr.ChecksSummary.Failed > prev.ChecksSummary.Failed)
	if len(newFailedChecks) > 0 || unnamedFailureIncrease {
		reasons = append(reasons, WakeReasonChecksFailed)
	}

	// 3. Newly entered CHANGES_REQUESTED review state.
	currReview := strings.ToUpper(strings.TrimSpace(curr.ReviewDecision))
	prevReview := ""
	if prev != nil {
		prevReview = strings.ToUpper(strings.TrimSpace(prev.ReviewDecision))
	}
	if currReview == "CHANGES_REQUESTED" && prevReview != "CHANGES_REQUESTED" {
		reasons = append(reasons, WakeReasonChangesRequested)
	}

	// 4. New PR comment or review comment body.
	currComment := strings.TrimSpace(curr.LatestCommentID)
	prevComment := ""
	if prev != nil {
		prevComment = strings.TrimSpace(prev.LatestCommentID)
	}
	if currComment != "" && currComment != prevComment {
		reasons = append(reasons, WakeReasonNewComment)
		commentID = currComment
	}

	// 5. Newly detected merge conflict (DIRTY).
	currMerge := strings.ToUpper(strings.TrimSpace(curr.MergeStateStatus))
	prevMerge := ""
	if prev != nil {
		prevMerge = strings.ToUpper(strings.TrimSpace(prev.MergeStateStatus))
	}
	if currMerge == "DIRTY" && prevMerge != "DIRTY" {
		reasons = append(reasons, WakeReasonMergeConflict)
	}

	if len(reasons) == 0 {
		return nil
	}

	return &WakeTrigger{
		PRNumber:        curr.Number,
		PRURL:           curr.URL,
		PrimaryReason:   reasons[0],
		Reasons:         reasons,
		ShouldSettle:    false,
		NewFailedChecks: newFailedChecks,
		LatestCommentID: commentID,
		WakePrompt:      formatActionWakePrompt(curr, reasons, newFailedChecks),
	}
}

func normalizeStringSet(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func formatMergedWakePrompt(curr *PRInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[PR Watcher] PR #%d 已合并 (MERGED)：%s\n", curr.Number, strings.TrimSpace(curr.Title))
	if curr.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", curr.URL)
	}
	b.WriteString("事件 / Event: pr_merged (ShouldSettle=true)\n")
	b.WriteString("请确认当前变更已合入目标分支，输出简明的收尾总结并归档结束当前会话任务（Please summarize the final outcome and settle this thread）。")
	return b.String()
}

func formatActionWakePrompt(curr *PRInfo, reasons []WakeReason, newFailed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[PR Watcher] PR #%d 检测到需要处理的新状态变化：%s\n", curr.Number, strings.TrimSpace(curr.Title))
	if curr.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", curr.URL)
	}
	reasonLabels := make([]string, 0, len(reasons))
	for _, r := range reasons {
		reasonLabels = append(reasonLabels, string(r))
	}
	fmt.Fprintf(&b, "触发原因 / Trigger Reasons: %s\n", strings.Join(reasonLabels, ", "))

	for _, r := range reasons {
		switch r {
		case WakeReasonChecksFailed:
			if len(newFailed) > 0 {
				fmt.Fprintf(&b, "- 新增失败检查 / Newly Failed Checks: %s\n", strings.Join(newFailed, ", "))
			} else {
				fmt.Fprintf(&b, "- 新增失败检查 / Newly Failed Checks: %d failed\n", curr.ChecksSummary.Failed)
			}
		case WakeReasonChangesRequested:
			b.WriteString("- 审查状态变为需要修改 / Review Decision: CHANGES_REQUESTED\n")
		case WakeReasonNewComment:
			if curr.LatestCommentAuthor != "" {
				fmt.Fprintf(&b, "- 收到新评论 / New Comment (%s by @%s)", curr.LatestCommentID, curr.LatestCommentAuthor)
			} else {
				fmt.Fprintf(&b, "- 收到新评论 / New Comment (%s)", curr.LatestCommentID)
			}
			if curr.LatestCommentBody != "" {
				fmt.Fprintf(&b, ": %s", curr.LatestCommentBody)
			}
			b.WriteByte('\n')
		case WakeReasonMergeConflict:
			b.WriteString("- 出现合并冲突 / Merge State: DIRTY (merge conflict with base branch)\n")
		}
	}
	b.WriteString("请针对上述变化检查具体原因并在当前工作区做最小必要修复与验证（Inspect the reported issues, apply minimal fixes, and verify locally）。")
	return b.String()
}

type watchTarget struct {
	threadID      string
	workspacePath string
	prURL         string
	hasBaseline   bool
	snapshot      WatchSnapshot
	seenComments  map[string]struct{}
}

// Watcher tracks PR business snapshots per ThreadID and prevents duplicate wakeups
// for unchanged PR states (modeled after the pull-request watch lifecycle).
type Watcher struct {
	mu      sync.Mutex
	client  *Client
	targets map[string]*watchTarget
}

// NewWatcher creates a stateful PR Watcher backed by client (or defaultClient if nil).
func NewWatcher(client *Client) *Watcher {
	if client == nil {
		client = defaultClient
	}
	return &Watcher{
		client:  client,
		targets: make(map[string]*watchTarget),
	}
}

// Watch binds or updates a watched PR for threadID. Passing a non-nil baseline
// seeds the initial snapshot so pre-existing states do not fire immediate wakeups.
func (w *Watcher) Watch(threadID, workspacePath, prURL string, baseline *PRInfo) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	target := &watchTarget{
		threadID:      threadID,
		workspacePath: strings.TrimSpace(workspacePath),
		prURL:         strings.TrimSpace(prURL),
		seenComments:  make(map[string]struct{}),
	}
	if baseline != nil {
		target.hasBaseline = true
		target.snapshot = snapshotFromPRInfo(threadID, baseline)
		target.snapshot.Settled = strings.EqualFold(baseline.State, "MERGED")
		if target.snapshot.LatestCommentID != "" {
			target.seenComments[target.snapshot.LatestCommentID] = struct{}{}
		}
	}
	w.targets[threadID] = target
}

// Unwatch removes PR tracking for threadID.
func (w *Watcher) Unwatch(threadID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.targets, strings.TrimSpace(threadID))
}

// Snapshot returns a copy of the latest tracked business snapshot for threadID.
func (w *Watcher) Snapshot(threadID string) (WatchSnapshot, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	target, ok := w.targets[strings.TrimSpace(threadID)]
	if !ok || !target.hasBaseline {
		return WatchSnapshot{}, false
	}
	return cloneSnapshot(target.snapshot), true
}

// WatchedThreads returns sorted ThreadIDs currently registered with the Watcher.
func (w *Watcher) WatchedThreads() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]string, 0, len(w.targets))
	for id := range w.targets {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Observe evaluates curr against threadID's last tracked snapshot, updates the
// snapshot, and returns a WakeTrigger only if a new wake-worthy transition occurred.
func (w *Watcher) Observe(threadID string, curr *PRInfo) *WakeTrigger {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" || curr == nil {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	target, exists := w.targets[threadID]
	if !exists {
		target = &watchTarget{
			threadID:     threadID,
			seenComments: make(map[string]struct{}),
		}
		w.targets[threadID] = target
	}

	// Reset tracking if the thread switched to a different PR number.
	if target.hasBaseline && target.snapshot.PRNumber > 0 && curr.Number > 0 && target.snapshot.PRNumber != curr.Number {
		target.hasBaseline = false
		target.snapshot = WatchSnapshot{}
		target.seenComments = make(map[string]struct{})
	}

	if target.hasBaseline && target.snapshot.Settled {
		return nil
	}

	var prev *PRInfo
	if target.hasBaseline {
		p := prInfoFromSnapshot(target.snapshot)
		// Prevent a deleted newest comment from re-triggering on an older already-seen comment ID.
		if cid := strings.TrimSpace(curr.LatestCommentID); cid != "" {
			if _, alreadySeen := target.seenComments[cid]; alreadySeen {
				p.LatestCommentID = cid
			}
		}
		prev = &p
	}

	trigger := EvaluateTransition(prev, curr)

	nextSnap := snapshotFromPRInfo(threadID, curr)
	if nextSnap.LatestCommentID == "" && target.hasBaseline {
		nextSnap.LatestCommentID = target.snapshot.LatestCommentID
	}
	if cid := strings.TrimSpace(curr.LatestCommentID); cid != "" {
		target.seenComments[cid] = struct{}{}
	}
	if trigger != nil && trigger.ShouldSettle {
		nextSnap.Settled = true
	}
	target.hasBaseline = true
	target.snapshot = nextSnap

	if trigger != nil {
		trigger.ThreadID = threadID
	}
	return trigger
}

// PollThread queries the live PR status for threadID and runs Observe.
func (w *Watcher) PollThread(ctx context.Context, threadID string) (*WakeTrigger, *PRInfo, error) {
	threadID = strings.TrimSpace(threadID)
	w.mu.Lock()
	target, ok := w.targets[threadID]
	if !ok {
		w.mu.Unlock()
		return nil, nil, fmt.Errorf("线程 %q 未注册 PR 巡检", threadID)
	}
	if target.hasBaseline && target.snapshot.Settled {
		w.mu.Unlock()
		return nil, nil, nil
	}
	workspacePath := target.workspacePath
	prURL := target.prURL
	client := w.client
	w.mu.Unlock()

	var (
		info *PRInfo
		err  error
	)
	if prURL != "" {
		info, err = client.InspectPRByURL(ctx, workspacePath, prURL)
	} else {
		info, err = client.InspectBranchPR(ctx, workspacePath)
	}
	if err != nil || info == nil {
		return nil, info, err
	}
	trigger := w.Observe(threadID, info)
	return trigger, info, nil
}

// PollAll polls all unsettled watched threads in deterministic order.
func (w *Watcher) PollAll(ctx context.Context) ([]WakeTrigger, error) {
	ids := w.WatchedThreads()
	var (
		triggers []WakeTrigger
		errs     []error
	)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		trigger, _, err := w.PollThread(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if trigger != nil {
			triggers = append(triggers, *trigger)
		}
	}
	return triggers, errors.Join(errs...)
}

func snapshotFromPRInfo(threadID string, info *PRInfo) WatchSnapshot {
	return WatchSnapshot{
		ThreadID:         threadID,
		PRNumber:         info.Number,
		PRURL:            info.URL,
		HeadRefOID:       strings.TrimSpace(info.HeadRefOID),
		LastState:        strings.ToUpper(strings.TrimSpace(info.State)),
		FailedChecks:     normalizeStringSet(info.ChecksSummary.FailedNames),
		ReviewDecision:   strings.ToUpper(strings.TrimSpace(info.ReviewDecision)),
		MergeStateStatus: strings.ToUpper(strings.TrimSpace(info.MergeStateStatus)),
		LatestCommentID:  strings.TrimSpace(info.LatestCommentID),
		UpdatedAt:        info.UpdatedAt,
	}
}

func prInfoFromSnapshot(snap WatchSnapshot) PRInfo {
	failed := cloneStrings(snap.FailedChecks)
	return PRInfo{
		Number:           snap.PRNumber,
		URL:              snap.PRURL,
		State:            snap.LastState,
		HeadRefOID:       snap.HeadRefOID,
		ReviewDecision:   snap.ReviewDecision,
		MergeStateStatus: snap.MergeStateStatus,
		ChecksSummary: ChecksSummary{
			Failed:      len(failed),
			FailedNames: failed,
		},
		LatestCommentID: snap.LatestCommentID,
		UpdatedAt:       snap.UpdatedAt,
	}
}

func cloneSnapshot(snap WatchSnapshot) WatchSnapshot {
	snap.FailedChecks = cloneStrings(snap.FailedChecks)
	return snap
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
