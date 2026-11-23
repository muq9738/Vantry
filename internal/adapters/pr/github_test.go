package pr

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInspectBranchPRParsesFullMetadataAndMixedChecks(t *testing.T) {
	const rawJSON = `{
		"number": 108,
		"title": "feat: add PR watcher",
		"url": "https://github.com/acme/vantry/pull/108",
		"state": "open",
		"isDraft": false,
		"headRefName": "feat/pr-watcher",
		"baseRefName": "main",
		"headRefOid": "1111222233334444555566667777888899990000",
		"reviewDecision": "changes_requested",
		"mergeStateStatus": "clean",
		"updatedAt": "2026-04-20T09:30:00Z",
		"statusCheckRollup": [
			{"__typename": "CheckRun", "name": "go-test", "status": "COMPLETED", "conclusion": "SUCCESS"},
			{"__typename": "CheckRun", "name": "lint", "status": "COMPLETED", "conclusion": "FAILURE"},
			{"__typename": "CheckRun", "name": "optional-docs", "status": "COMPLETED", "conclusion": "SKIPPED"},
			{"__typename": "CheckRun", "name": "e2e-macos", "status": "IN_PROGRESS", "conclusion": ""},
			{"__typename": "StatusContext", "context": "ci/external-scan", "state": "ERROR"},
			{"__typename": "StatusContext", "context": "cla/check", "state": "PENDING"}
		],
		"comments": [
			{"id": "IC_1", "author": {"login": "alice"}, "body": "Initial note", "createdAt": "2026-04-20T08:00:00Z"},
			{"id": "IC_2", "author": {"login": "bob"}, "body": "Please check race detector", "createdAt": "2026-04-20T09:00:00Z"}
		],
		"reviews": [
			{"id": "PRR_1", "author": {"login": "carol"}, "body": "Nit in watcher transition", "state": "CHANGES_REQUESTED", "submittedAt": "2026-04-20T09:15:00Z"},
			{"id": "PRR_2", "author": {"login": "dave"}, "body": "", "state": "APPROVED", "submittedAt": "2026-04-20T09:20:00Z"}
		]
	}`

	var capturedDir string
	var capturedArgs []string
	client := NewClient(RunnerFunc(func(_ context.Context, dir string, args ...string) (string, string, error) {
		capturedDir = dir
		capturedArgs = append([]string(nil), args...)
		return rawJSON, "", nil
	}))

	info, err := client.InspectBranchPR(context.Background(), "/tmp/mock-workspace")
	if err != nil {
		t.Fatalf("InspectBranchPR failed: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil PRInfo")
	}
	if capturedDir != "/tmp/mock-workspace" || len(capturedArgs) < 4 || capturedArgs[0] != "pr" || capturedArgs[1] != "view" {
		t.Fatalf("unexpected runner invocation: dir=%q args=%v", capturedDir, capturedArgs)
	}
	if info.Number != 108 || info.State != "OPEN" || info.ReviewDecision != "CHANGES_REQUESTED" || info.MergeStateStatus != "CLEAN" {
		t.Fatalf("unexpected core fields: %+v", info)
	}
	if info.ChecksSummary.Total != 6 || info.ChecksSummary.Passed != 2 || info.ChecksSummary.Failed != 2 || info.ChecksSummary.Pending != 2 {
		t.Fatalf("unexpected ChecksSummary counts: %+v", info.ChecksSummary)
	}
	wantFailed := []string{"ci/external-scan", "lint"}
	if !slices.Equal(info.ChecksSummary.FailedNames, wantFailed) {
		t.Fatalf("expected FailedNames=%v, got %v", wantFailed, info.ChecksSummary.FailedNames)
	}
	// PRR_1 has the latest non-empty comment body (09:15 > 09:00; PRR_2 has empty body).
	if info.LatestCommentID != "PRR_1" || info.LatestCommentAuthor != "carol" || !strings.Contains(info.LatestCommentBody, "watcher transition") {
		t.Fatalf("unexpected latest comment: id=%q author=%q body=%q", info.LatestCommentID, info.LatestCommentAuthor, info.LatestCommentBody)
	}
}

func TestInspectBranchPRReturnsNilWhenNoPROrNoGitHubRemote(t *testing.T) {
	absentCases := []string{
		`no pull requests found for branch "feat/local-only"`,
		`none of the git remotes configured for this repository point to a known GitHub host`,
		`no git remotes found`,
		`could not determine base repository`,
		`could not determine current branch`,
	}
	for _, stderrMsg := range absentCases {
		client := NewClient(RunnerFunc(func(_ context.Context, _ string, _ ...string) (string, string, error) {
			return "", stderrMsg, errors.New("exit status 1")
		}))
		info, err := client.InspectBranchPR(context.Background(), "/tmp/workspace")
		if err != nil || info != nil {
			t.Fatalf("stderr %q should return (nil, nil), got info=%+v err=%v", stderrMsg, info, err)
		}
	}

	// Real auth/network failures must surface an error rather than pretending no PR exists.
	authClient := NewClient(RunnerFunc(func(_ context.Context, _ string, _ ...string) (string, string, error) {
		return "", "HTTP 401: Bad credentials (https://api.github.com/graphql)", errors.New("exit status 4")
	}))
	if info, err := authClient.InspectBranchPR(context.Background(), "/tmp/workspace"); err == nil || info != nil {
		t.Fatalf("expected auth error, got info=%+v err=%v", info, err)
	}
}

func TestInspectPRByURLValidatesAndCanonicalizesURL(t *testing.T) {
	var seenTarget string
	client := NewClient(RunnerFunc(func(_ context.Context, _ string, args ...string) (string, string, error) {
		seenTarget = args[2]
		return `{"number":42,"title":"fix","url":"https://github.com/org/repo/pull/42","state":"OPEN","updatedAt":"2026-04-20T10:00:00Z"}`, "", nil
	}))

	info, err := client.InspectPRByURL(context.Background(), "", "https://github.com/org/repo/pull/42/files")
	if err != nil || info == nil || info.Number != 42 {
		t.Fatalf("valid PR URL failed: info=%+v err=%v", info, err)
	}
	if seenTarget != "https://github.com/org/repo/pull/42" {
		t.Fatalf("expected canonical PR URL, got %q", seenTarget)
	}

	invalidURLs := []string{
		"",
		"http://github.com/org/repo/pull/42",
		"https://user@github.com/org/repo/pull/42",
		"https://github.com/org/repo/issues/42",
		"https://github.com/org/repo/pull/0",
		"https://github.com/org/repo/pull/not-a-number",
		"https://github.com/../repo/pull/42",
		"--repo=evil/repo",
		"https://localhost/org/repo/pull/42",
	}
	for _, raw := range invalidURLs {
		if _, err := client.InspectPRByURL(context.Background(), "", raw); err == nil {
			t.Errorf("InspectPRByURL accepted invalid URL %q", raw)
		}
	}
}

func TestWatcherTransitionsDeduplicationAndMergedSettle(t *testing.T) {
	now := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	base := &PRInfo{
		Number:           77,
		Title:            "feat: lane 4",
		URL:              "https://github.com/acme/vantry/pull/77",
		State:            "OPEN",
		HeadRefOID:       "commit_1",
		ReviewDecision:   "REVIEW_REQUIRED",
		MergeStateStatus: "CLEAN",
		LatestCommentID:  "IC_initial",
		ChecksSummary:    ChecksSummary{Total: 2, Passed: 2},
		UpdatedAt:        now,
	}

	watcher := NewWatcher(nil)
	watcher.Watch("thread-77", "/tmp/ws", base.URL, base)

	// 1. Unchanged baseline must not wake.
	if trig := watcher.Observe("thread-77", base); trig != nil {
		t.Fatalf("unchanged baseline triggered wakeup: %+v", trig)
	}

	// 2. First failed check triggers once; repeating same state (even reordered) does not repeat.
	failOne := *base
	failOne.ChecksSummary = ChecksSummary{
		Total:       2,
		Passed:      1,
		Failed:      1,
		FailedNames: []string{"unit-test"},
	}
	trig := watcher.Observe("thread-77", &failOne)
	if trig == nil || trig.PrimaryReason != WakeReasonChecksFailed || trig.ShouldSettle || !slices.Equal(trig.NewFailedChecks, []string{"unit-test"}) {
		t.Fatalf("expected checks_failed trigger for unit-test, got %+v", trig)
	}
	if !strings.Contains(trig.WakePrompt, "unit-test") {
		t.Fatalf("wake prompt missing check name: %s", trig.WakePrompt)
	}
	if dup := watcher.Observe("thread-77", &failOne); dup != nil {
		t.Fatalf("duplicate wakeup on identical failed check: %+v", dup)
	}

	// 3. Second check fails while unit-test remains failing -> wakes only for newly failed check "lint".
	failTwo := failOne
	failTwo.ChecksSummary = ChecksSummary{
		Total:       2,
		Failed:      2,
		FailedNames: []string{"unit-test", "lint"},
	}
	trig = watcher.Observe("thread-77", &failTwo)
	if trig == nil || !slices.Equal(trig.NewFailedChecks, []string{"lint"}) {
		t.Fatalf("expected only newly failed lint, got %+v", trig)
	}
	reordered := failTwo
	reordered.ChecksSummary.FailedNames = []string{"lint", "unit-test"}
	if dup := watcher.Observe("thread-77", &reordered); dup != nil {
		t.Fatalf("reordered FailedNames caused duplicate wakeup: %+v", dup)
	}

	// 4. Simultaneous CHANGES_REQUESTED + new comment + DIRTY merge conflict wakes once with all reasons.
	reviewAndConflict := reordered
	reviewAndConflict.ReviewDecision = "CHANGES_REQUESTED"
	reviewAndConflict.MergeStateStatus = "DIRTY"
	reviewAndConflict.LatestCommentID = "IC_new_review"
	reviewAndConflict.LatestCommentAuthor = "reviewer1"
	reviewAndConflict.LatestCommentBody = "Please rebase and fix lint"
	trig = watcher.Observe("thread-77", &reviewAndConflict)
	if trig == nil {
		t.Fatal("expected combined wakeup")
	}
	wantReasons := []WakeReason{WakeReasonChangesRequested, WakeReasonNewComment, WakeReasonMergeConflict}
	if !slices.Equal(trig.Reasons, wantReasons) {
		t.Fatalf("expected reasons %v, got %v", wantReasons, trig.Reasons)
	}
	if dup := watcher.Observe("thread-77", &reviewAndConflict); dup != nil {
		t.Fatalf("duplicate wakeup on unchanged review/comment/dirty state: %+v", dup)
	}

	// 5. Deleting IC_new_review so LatestCommentID falls back to already-seen IC_initial must NOT wake!
	commentDeleted := reviewAndConflict
	commentDeleted.LatestCommentID = "IC_initial"
	if dup := watcher.Observe("thread-77", &commentDeleted); dup != nil {
		t.Fatalf("reverting to previously seen comment ID caused false wakeup: %+v", dup)
	}

	// 6. Pushing a new commit (HeadRefOID changes) where lint fails again wakes for the new commit!
	newCommitFail := commentDeleted
	newCommitFail.HeadRefOID = "commit_2"
	newCommitFail.MergeStateStatus = "CLEAN"
	newCommitFail.ChecksSummary = ChecksSummary{
		Total:       2,
		Passed:      1,
		Failed:      1,
		FailedNames: []string{"lint"},
	}
	trig = watcher.Observe("thread-77", &newCommitFail)
	if trig == nil || !slices.Equal(trig.Reasons, []WakeReason{WakeReasonChecksFailed}) || !slices.Equal(trig.NewFailedChecks, []string{"lint"}) {
		t.Fatalf("expected failure on new commit_2 to wake, got %+v", trig)
	}

	// 7. PR merges -> triggers pr_merged with ShouldSettle=true once, then settles permanently.
	merged := newCommitFail
	merged.State = "MERGED"
	trig = watcher.Observe("thread-77", &merged)
	if trig == nil || trig.PrimaryReason != WakeReasonMerged || !trig.ShouldSettle || !strings.Contains(trig.WakePrompt, "MERGED") {
		t.Fatalf("expected ShouldSettle merged trigger, got %+v", trig)
	}
	snap, ok := watcher.Snapshot("thread-77")
	if !ok || !snap.Settled || snap.LastState != "MERGED" {
		t.Fatalf("expected settled snapshot, got %+v (ok=%v)", snap, ok)
	}
	if dup := watcher.Observe("thread-77", &merged); dup != nil {
		t.Fatalf("settled merged PR woke again: %+v", dup)
	}
}

func TestWatcherPollAllConcurrentSafety(t *testing.T) {
	var mu sync.Mutex
	stateByURL := map[string]string{
		"https://github.com/acme/vantry/pull/1": `{"number":1,"title":"one","url":"https://github.com/acme/vantry/pull/1","state":"OPEN","mergeStateStatus":"DIRTY","updatedAt":"2026-04-20T10:00:00Z"}`,
		"https://github.com/acme/vantry/pull/2": `{"number":2,"title":"two","url":"https://github.com/acme/vantry/pull/2","state":"MERGED","updatedAt":"2026-04-20T10:00:00Z"}`,
	}
	client := NewClient(RunnerFunc(func(_ context.Context, _ string, args ...string) (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		return stateByURL[args[2]], "", nil
	}))
	w := NewWatcher(client)
	w.Watch("t-1", "", "https://github.com/acme/vantry/pull/1", nil)
	w.Watch("t-2", "", "https://github.com/acme/vantry/pull/2", nil)

	triggers, err := w.PollAll(context.Background())
	if err != nil || len(triggers) != 2 {
		t.Fatalf("first PollAll: triggers=%+v err=%v", triggers, err)
	}
	if triggers[0].ThreadID != "t-1" || triggers[0].PrimaryReason != WakeReasonMergeConflict {
		t.Fatalf("unexpected t-1 trigger: %+v", triggers[0])
	}
	if triggers[1].ThreadID != "t-2" || !triggers[1].ShouldSettle {
		t.Fatalf("unexpected t-2 trigger: %+v", triggers[1])
	}

	// Second PollAll with identical remote state produces zero triggers.
	triggers, err = w.PollAll(context.Background())
	if err != nil || len(triggers) != 0 {
		t.Fatalf("second PollAll should produce 0 triggers, got %+v, err=%v", triggers, err)
	}
}
