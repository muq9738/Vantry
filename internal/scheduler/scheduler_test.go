package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// fakeService records dispatched commands and answers from a fixed projection.
type fakeService struct {
	mu         sync.Mutex
	commands   []api.Command
	threads    []api.Thread
	runs       []api.Run
	sequence   int64
	dispatch   func(api.Command) (api.Receipt, error)
	onDispatch func()
}

func (s *fakeService) Health(context.Context) (api.Health, error) {
	return api.Health{Version: api.ProtocolVersion, State: "ready"}, nil
}

func (s *fakeService) Dispatch(_ context.Context, command api.Command) (api.Receipt, error) {
	if hook := s.dispatchHook(); hook != nil {
		hook()
	}
	s.mu.Lock()
	s.commands = append(s.commands, command)
	s.sequence++
	sequence := s.sequence
	s.mu.Unlock()
	// A dispatched run becomes active, so a second dispatch for the same thread
	// is refused exactly as Core refuses it.
	if command.Kind == api.SendMessage {
		s.mu.Lock()
		for _, run := range s.runs {
			if run.ThreadID == command.ThreadID && !run.Status.Terminal() {
				s.mu.Unlock()
				return api.Receipt{}, &api.Error{Code: api.Conflict, Message: "thread already has an active run"}
			}
		}
		s.runs = append(s.runs, api.Run{
			ID: "run-" + command.CommandID, ThreadID: command.ThreadID, Status: api.RunRunning,
		})
		s.mu.Unlock()
	}
	receipt := api.Receipt{CommandID: command.CommandID, Accepted: true, Sequence: sequence, ThreadID: command.ThreadID}
	switch command.Kind {
	case api.CreateThread:
		receipt.ThreadID = "thread-scheduled"
		s.mu.Lock()
		s.threads = append(s.threads, api.Thread{ID: receipt.ThreadID, Title: command.Title, Model: command.Model})
		s.mu.Unlock()
	case api.SendMessage:
		receipt.RunID = "run-" + command.CommandID
	}
	return receipt, nil
}

func (s *fakeService) dispatchHook() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDispatch
}

func (s *fakeService) Snapshot(context.Context) (api.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return api.Snapshot{
		Sequence: s.sequence,
		Threads:  append([]api.Thread{}, s.threads...),
		Runs:     append([]api.Run{}, s.runs...),
	}, nil
}

func (s *fakeService) Subscribe(context.Context, int64) (*api.Subscription, error) {
	return nil, errors.New("unexpected Subscribe call")
}

func (s *fakeService) dispatched() []api.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.Command{}, s.commands...)
}

func (s *fakeService) setRuns(runs ...api.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = runs
}

func (s *fakeService) setThreads(threads ...api.Thread) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads = threads
}

// readerService exposes the light projections, so the scheduler must not read a
// full snapshot to decide whether a thread is busy.
type readerService struct {
	*fakeService
	summariesCalls atomicCounter
}

func (s *readerService) ThreadSummaries(context.Context) (api.ThreadSummaries, error) {
	s.summariesCalls.add(1)
	snapshot, err := s.fakeService.Snapshot(context.Background())
	if err != nil {
		return api.ThreadSummaries{}, err
	}
	return api.BuildThreadSummaries(snapshot), nil
}

func (s *readerService) ThreadDetail(ctx context.Context, query api.ThreadDetailQuery) (api.ThreadDetail, error) {
	snapshot, err := s.fakeService.Snapshot(ctx)
	if err != nil {
		return api.ThreadDetail{}, err
	}
	return api.BuildThreadDetail(snapshot, query)
}

type atomicCounter struct {
	mu    sync.Mutex
	value int
}

func (c *atomicCounter) add(delta int) {
	c.mu.Lock()
	c.value += delta
	c.mu.Unlock()
}

func (c *atomicCounter) load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func fixedClock(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

func newDataDir(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "data")
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func openTestScheduler(t *testing.T, service api.Service, now func() time.Time) *Scheduler {
	t.Helper()
	scheduler, err := Open(Options{DataDir: newDataDir(t), Service: service, Now: now})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return scheduler
}

func TestRunDueDispatchesDueTaskAndAdvancesSchedule(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	service := &fakeService{}
	service.setThreads(api.Thread{ID: "thread-a", Title: "每日巡检", Model: "pi/sonnet"})
	scheduler := openTestScheduler(t, service, fixedClock(now))

	task, err := scheduler.Create(ScheduledTask{
		Title: "每日巡检", ThreadID: "thread-a", Model: "pi/sonnet",
		Prompt: "汇总今天的仓库状态", IntervalMinutes: 15, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if task.ID == "" || task.NextRunAt.IsZero() {
		t.Fatalf("created task = %+v", task)
	}

	// Nothing is due before the stored slot.
	if outcomes := scheduler.RunDue(context.Background()); len(outcomes) != 0 {
		t.Fatalf("premature outcomes = %+v", outcomes)
	}

	scheduler.now = fixedClock(now.Add(16 * time.Minute))
	outcomes := scheduler.RunDue(context.Background())
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	outcome := outcomes[0]
	if !outcome.Started || outcome.Busy || outcome.Err != nil {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if outcome.Receipt.RunID == "" || outcome.RunID != outcome.Receipt.RunID {
		t.Fatalf("run identity = %+v", outcome)
	}

	commands := service.dispatched()
	if len(commands) != 1 {
		t.Fatalf("commands = %+v", commands)
	}
	if commands[0].Kind != api.SendMessage || commands[0].ThreadID != "thread-a" || commands[0].Message != "汇总今天的仓库状态" {
		t.Fatalf("command = %+v", commands[0])
	}
	if commands[0].Version != api.ProtocolVersion || strings.TrimSpace(commands[0].CommandID) == "" {
		t.Fatalf("command envelope = %+v", commands[0])
	}

	stored := scheduler.List()
	if stored[0].LastRunID != outcome.RunID {
		t.Fatalf("LastRunID = %q, want %q", stored[0].LastRunID, outcome.RunID)
	}
	if !stored[0].LastRunAt.Equal(now.Add(16 * time.Minute)) {
		t.Fatalf("LastRunAt = %s", stored[0].LastRunAt)
	}
	if want := now.Add(31 * time.Minute); !stored[0].NextRunAt.Equal(want) {
		t.Fatalf("NextRunAt = %s, want %s", stored[0].NextRunAt, want)
	}
	if stored[0].LastError != "" {
		t.Fatalf("LastError = %q", stored[0].LastError)
	}
}

func TestRunDueSkipsBusyThreadWithoutStacking(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	service := &fakeService{}
	service.setThreads(api.Thread{ID: "thread-a", Title: "巡检"})
	service.setRuns(api.Run{ID: "run-live", ThreadID: "thread-a", Status: api.RunRunning})
	scheduler := openTestScheduler(t, service, fixedClock(now))

	if _, err := scheduler.Create(ScheduledTask{
		Title: "巡检", ThreadID: "thread-a", Prompt: "继续", IntervalMinutes: 15, Enabled: true,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	scheduler.now = fixedClock(now.Add(16 * time.Minute))
	outcomes := scheduler.RunDue(context.Background())
	if len(outcomes) != 1 || !outcomes[0].Skipped || !outcomes[0].Busy || outcomes[0].Started {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	if commands := service.dispatched(); len(commands) != 0 {
		t.Fatalf("busy thread received commands: %+v", commands)
	}
	stored := scheduler.List()[0]
	if !strings.Contains(stored.LastError, "活动任务") || !strings.Contains(stored.LastError, "thread-a") {
		t.Fatalf("LastError = %q", stored.LastError)
	}
	if want := now.Add(31 * time.Minute); !stored.NextRunAt.Equal(want) {
		t.Fatalf("NextRunAt = %s, want %s (skip must not retry immediately)", stored.NextRunAt, want)
	}
	if !stored.LastRunAt.IsZero() {
		t.Fatalf("skipped run was recorded as executed: %+v", stored)
	}
}

func TestRunDueUsesThreadSummariesInsteadOfSnapshot(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	service := &readerService{fakeService: &fakeService{}}
	service.setThreads(api.Thread{ID: "thread-a", Title: "巡检"})
	service.setRuns(api.Run{ID: "run-live", ThreadID: "thread-a", Status: api.RunWaiting})
	scheduler := openTestScheduler(t, service, fixedClock(now))

	if _, err := scheduler.Create(ScheduledTask{
		Title: "巡检", ThreadID: "thread-a", Prompt: "继续", IntervalMinutes: 5, Enabled: true,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	scheduler.now = fixedClock(now.Add(6 * time.Minute))
	outcomes := scheduler.RunDue(context.Background())
	if len(outcomes) != 1 || !outcomes[0].Busy {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	if calls := service.summariesCalls.load(); calls == 0 {
		t.Fatal("scheduler did not use the lightweight thread summaries")
	}
}

func TestRunDueCreatesDedicatedThreadAndRemembersIt(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	service := &fakeService{}
	scheduler := openTestScheduler(t, service, fixedClock(now))
	workspace := &api.WorkspaceSpec{Kind: "worktree", Repository: "github.com/muq9738/Vantry", Branch: "main"}

	if _, err := scheduler.Create(ScheduledTask{
		Title: "夜间构建", Workspace: workspace, Model: "pi/sonnet",
		Prompt: "跑一次完整测试", IntervalMinutes: 60, Enabled: true,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	scheduler.now = fixedClock(now.Add(61 * time.Minute))
	outcomes := scheduler.RunDue(context.Background())
	if len(outcomes) != 1 || !outcomes[0].Started || outcomes[0].Err != nil {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	commands := service.dispatched()
	if len(commands) != 2 {
		t.Fatalf("commands = %+v", commands)
	}
	if commands[0].Kind != api.CreateThread || commands[0].Title != "夜间构建" || commands[0].Model != "pi/sonnet" {
		t.Fatalf("create command = %+v", commands[0])
	}
	if commands[0].Workspace == nil || commands[0].Workspace.Branch != "main" {
		t.Fatalf("create workspace = %+v", commands[0].Workspace)
	}
	if commands[1].Kind != api.SendMessage || commands[1].ThreadID != "thread-scheduled" {
		t.Fatalf("send command = %+v", commands[1])
	}
	stored := scheduler.List()[0]
	if stored.ThreadID != "thread-scheduled" {
		t.Fatalf("dedicated thread was not remembered: %+v", stored)
	}

	// The next run reuses the remembered thread instead of creating another one.
	service.setRuns()
	scheduler.now = fixedClock(now.Add(122 * time.Minute))
	if outcomes := scheduler.RunDue(context.Background()); len(outcomes) != 1 || !outcomes[0].Started {
		t.Fatalf("second outcomes = %+v", outcomes)
	}
	commands = service.dispatched()
	if len(commands) != 3 || commands[2].Kind != api.SendMessage || commands[2].ThreadID != "thread-scheduled" {
		t.Fatalf("second run commands = %+v", commands)
	}
}

func TestTriggerNowRefusesBusyThread(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	service := &fakeService{}
	service.setThreads(api.Thread{ID: "thread-a", Title: "巡检"})
	service.setRuns(api.Run{ID: "run-live", ThreadID: "thread-a", Status: api.RunRunning})
	scheduler := openTestScheduler(t, service, fixedClock(now))

	task, err := scheduler.Create(ScheduledTask{
		Title: "巡检", ThreadID: "thread-a", Prompt: "继续", IntervalMinutes: 15, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := scheduler.TriggerNow(context.Background(), task.ID); api.CodeOf(err) != api.Conflict {
		t.Fatalf("error = %v, want api.Conflict", err)
	}
	if commands := service.dispatched(); len(commands) != 0 {
		t.Fatalf("busy thread received commands: %+v", commands)
	}
	if _, err := scheduler.TriggerNow(context.Background(), "missing"); api.CodeOf(err) != api.NotFound {
		t.Fatalf("missing task error = %v, want api.NotFound", err)
	}
}

func TestConcurrentRunDueDispatchesOnce(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	release := make(chan struct{})
	service := &fakeService{}
	service.setThreads(api.Thread{ID: "thread-a", Title: "巡检"})
	service.onDispatch = func() { <-release }
	scheduler := openTestScheduler(t, service, fixedClock(now))

	if _, err := scheduler.Create(ScheduledTask{
		Title: "巡检", ThreadID: "thread-a", Prompt: "继续", IntervalMinutes: 15, Enabled: true,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	scheduler.now = fixedClock(now.Add(16 * time.Minute))

	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		scheduler.RunDue(context.Background())
	}()
	<-started
	// Wait until the first dispatch is inside the fake, then run a second pass.
	waitForInFlight(t, scheduler)
	second := scheduler.RunDue(context.Background())
	close(release)
	wg.Wait()
	for _, outcome := range second {
		if outcome.Started {
			t.Fatalf("second pass started a duplicate run: %+v", outcome)
		}
	}
	if commands := service.dispatched(); len(commands) != 1 {
		t.Fatalf("commands = %+v", commands)
	}
}

func TestPersistenceSurvivesReopenAndStaysPrivate(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	service := &fakeService{}
	directory := newDataDir(t)
	scheduler, err := Open(Options{DataDir: directory, Service: service, Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	created, err := scheduler.Create(ScheduledTask{
		Title: "每日汇总", Prompt: "总结", DailyAt: "08:00", Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if want := time.Date(2026, 3, 5, 8, 0, 0, 0, time.UTC); !created.NextRunAt.Equal(want) {
		t.Fatalf("NextRunAt = %s, want %s", created.NextRunAt, want)
	}

	info, err := os.Stat(filepath.Join(directory, StoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("task file mode = %04o, want 0600", mode)
	}
	payload, err := os.ReadFile(filepath.Join(directory, StoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	var stored storeFile
	if err := json.Unmarshal(payload, &stored); err != nil {
		t.Fatalf("task file is not JSON: %v", err)
	}
	if stored.Version != storeVersion || len(stored.Tasks) != 1 {
		t.Fatalf("stored file = %+v", stored)
	}

	reopened, err := Open(Options{DataDir: directory, Service: service, Now: fixedClock(now.Add(time.Hour))})
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	tasks := reopened.List()
	if len(tasks) != 1 || tasks[0].ID != created.ID || tasks[0].Prompt != "总结" {
		t.Fatalf("reopened tasks = %+v", tasks)
	}
}

func TestUpdatePreservesRunHistoryAndToSchedule(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	service := &fakeService{}
	service.setThreads(api.Thread{ID: "thread-a", Title: "巡检"})
	scheduler := openTestScheduler(t, service, fixedClock(now))

	created, err := scheduler.Create(ScheduledTask{
		Title: "巡检", ThreadID: "thread-a", Prompt: "继续", IntervalMinutes: 15, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	scheduler.now = fixedClock(now.Add(16 * time.Minute))
	if outcomes := scheduler.RunDue(context.Background()); len(outcomes) != 1 || !outcomes[0].Started {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	ran := scheduler.List()[0]

	updated, err := scheduler.Update(ScheduledTask{
		ID: ran.ID, Title: "巡检（夜间）", ThreadID: "thread-a", Prompt: "新的提示词", IntervalMinutes: 15, Enabled: false,
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.LastRunID != ran.LastRunID || !updated.LastRunAt.Equal(ran.LastRunAt) {
		t.Fatalf("run history lost: %+v", updated)
	}
	if !updated.NextRunAt.IsZero() {
		t.Fatalf("disabled task still has a slot: %+v", updated)
	}
	if updated.Title != "巡检（夜间）" || updated.Prompt != "新的提示词" {
		t.Fatalf("update ignored configuration: %+v", updated)
	}
	if _, err := scheduler.Update(ScheduledTask{ID: "missing", Title: "x", Prompt: "y", IntervalMinutes: 1}); api.CodeOf(err) != api.NotFound {
		t.Fatalf("missing update error = %v", err)
	}
	if err := scheduler.Delete(created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if remaining := scheduler.List(); len(remaining) != 0 {
		t.Fatalf("tasks after delete = %+v", remaining)
	}
	if err := scheduler.Delete(created.ID); api.CodeOf(err) != api.NotFound {
		t.Fatalf("second delete error = %v", err)
	}
}

func TestScheduleValidation(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	scheduler := openTestScheduler(t, &fakeService{}, fixedClock(now))

	cases := []ScheduledTask{
		{Prompt: "缺少标题", IntervalMinutes: 5},
		{Title: "缺少提示词", IntervalMinutes: 5},
		{Title: "缺少时间", Prompt: "x"},
		{Title: "负间隔", Prompt: "x", IntervalMinutes: -1},
		{Title: "错误时间", Prompt: "x", DailyAt: "8:00"},
		{Title: "越界时间", Prompt: "x", DailyAt: "24:00"},
	}
	for _, task := range cases {
		if _, err := scheduler.Create(task); api.CodeOf(err) != api.InvalidCommand {
			t.Fatalf("Create(%+v) error = %v, want api.InvalidCommand", task, err)
		}
	}
	if tasks := scheduler.List(); len(tasks) != 0 {
		t.Fatalf("invalid tasks were stored: %+v", tasks)
	}
}

func TestRunLoopDispatchesDueTaskUntilCancelled(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 30, 0, 0, time.UTC)
	service := &fakeService{}
	service.setThreads(api.Thread{ID: "thread-a", Title: "巡检"})
	scheduler, err := Open(Options{
		DataDir: newDataDir(t), Service: service, Now: fixedClock(now), Tick: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := scheduler.Create(ScheduledTask{
		Title: "巡检", ThreadID: "thread-a", Prompt: "立即执行", IntervalMinutes: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	scheduler.now = fixedClock(now.Add(2 * time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.RunLoop(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(service.dispatched()) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("RunLoop did not dispatch the due task")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunLoop() error = %v", err)
	}
	dispatched := service.dispatched()
	if len(dispatched) != 1 {
		t.Fatalf("RunLoop dispatched %d commands", len(dispatched))
	}
	if dispatched[0].Message != "立即执行" {
		t.Fatalf("command = %+v", dispatched[0])
	}
}

func waitForInFlight(t *testing.T, scheduler *Scheduler) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		scheduler.mu.Lock()
		pending := len(scheduler.inFlight)
		scheduler.mu.Unlock()
		if pending == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scheduled dispatch never became in flight")
}
