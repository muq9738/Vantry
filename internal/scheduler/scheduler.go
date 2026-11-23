// Package scheduler runs Vantry's local recurring prompts. The task list is
// persisted next to the Core data directory and every run is dispatched as an
// ordinary Core command, so a scheduled message goes through exactly the same
// durability and single-active-run rules as a message the user typed.
package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

const (
	// StoreFileName is the task file inside the scheduler data directory.
	StoreFileName = "scheduled-tasks.json"
	// storeVersion is the schema of that file, not a build identity.
	storeVersion = 1
	// defaultTick is the RunLoop interval when Options.Tick is unset.
	defaultTick = time.Second
)

// ScheduledTask is one persisted local schedule. ThreadID is optional: an empty
// value makes the scheduler allocate a dedicated thread for this task on its
// first run and remember it afterwards.
type ScheduledTask struct {
	ID              string             `json:"id"`
	Title           string             `json:"title"`
	ThreadID        string             `json:"threadId,omitempty"`
	Workspace       *api.WorkspaceSpec `json:"workspace,omitempty"`
	Model           string             `json:"model,omitempty"`
	Prompt          string             `json:"prompt"`
	IntervalMinutes int                `json:"intervalMinutes,omitempty"`
	DailyAt         string             `json:"dailyAt,omitempty"`
	Enabled         bool               `json:"enabled"`
	LastRunAt       time.Time          `json:"lastRunAt,omitempty"`
	NextRunAt       time.Time          `json:"nextRunAt,omitempty"`
	LastRunID       string             `json:"lastRunId,omitempty"`
	LastError       string             `json:"lastError,omitempty"`
}

// Outcome is the observable result of one dispatch attempt. Busy and Skipped
// mean the target thread already had an active run, so nothing was queued.
type Outcome struct {
	TaskID   string
	ThreadID string
	RunID    string
	Receipt  api.Receipt
	Started  bool
	Skipped  bool
	Busy     bool
	Err      error
}

// Options configures a Scheduler.
type Options struct {
	DataDir string
	Service api.Service
	// Now overrides the clock so scheduling can be verified deterministically.
	Now func() time.Time
	// Tick is the RunLoop interval; the default is one second.
	Tick time.Duration
	// Logf receives background dispatch diagnostics; nil discards them.
	Logf func(format string, args ...any)
}

// Scheduler owns the persisted task list. It is safe for concurrent use.
type Scheduler struct {
	dataDir string
	service api.Service
	now     func() time.Time
	tick    time.Duration
	logf    func(string, ...any)

	// sequence keeps generated identities unique even for two dispatches inside
	// the same clock tick.
	sequence atomic.Uint64

	mu       sync.Mutex
	tasks    []ScheduledTask
	inFlight map[string]bool
}

// Open loads the persisted tasks from opts.DataDir. A missing file is an empty
// schedule; an unreadable or unsupported one is an error, never a silent reset.
func Open(opts Options) (*Scheduler, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, errors.New("scheduler: 数据目录不能为空")
	}
	if opts.Service == nil {
		return nil, errors.New("scheduler: 需要 Core 服务")
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("scheduler: 创建数据目录：%w", err)
	}
	s := &Scheduler{
		dataDir:  opts.DataDir,
		service:  opts.Service,
		now:      opts.Now,
		tick:     opts.Tick,
		logf:     opts.Logf,
		inFlight: make(map[string]bool),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.tick <= 0 {
		s.tick = defaultTick
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	tasks, err := loadTasks(s.storePath())
	if err != nil {
		return nil, err
	}
	now := s.now()
	for position := range tasks {
		// A task that should run but has no slot is scheduled from now instead of
		// being silently skipped forever.
		if tasks[position].Enabled && tasks[position].NextRunAt.IsZero() {
			tasks[position].NextRunAt = nextRunAt(now, tasks[position])
		}
	}
	s.tasks = tasks
	return s, nil
}

// List returns a copy of the persisted tasks in stored order.
func (s *Scheduler) List() []ScheduledTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := make([]ScheduledTask, len(s.tasks))
	copy(tasks, s.tasks)
	return tasks
}

// Create validates and stores a new task. The scheduler owns task identity, so a
// caller-supplied ID is replaced. Run history starts empty.
func (s *Scheduler) Create(task ScheduledTask) (ScheduledTask, error) {
	if err := task.validate(); err != nil {
		return ScheduledTask{}, err
	}
	now := s.now()
	task.ID = s.newTaskID(now)
	task.LastRunAt = time.Time{}
	task.LastRunID = ""
	task.LastError = ""
	if task.Enabled {
		task.NextRunAt = nextRunAt(now, task)
	} else {
		task.NextRunAt = time.Time{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]ScheduledTask, 0, len(s.tasks)+1)
	next = append(next, s.tasks...)
	next = append(next, task)
	if err := s.writeStore(next); err != nil {
		return ScheduledTask{}, err
	}
	s.tasks = next
	return task, nil
}

// Update replaces the configurable fields of a stored task. Run history
// (LastRunAt, LastRunID, LastError) is preserved, and an unchanged schedule keeps
// its pending slot.
func (s *Scheduler) Update(task ScheduledTask) (ScheduledTask, error) {
	if strings.TrimSpace(task.ID) == "" {
		return ScheduledTask{}, &api.Error{Code: api.InvalidCommand, Message: "更新定时任务需要 ID"}
	}
	if err := task.validate(); err != nil {
		return ScheduledTask{}, err
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	position := s.position(task.ID)
	if position < 0 {
		return ScheduledTask{}, notFound(task.ID)
	}
	stored := s.tasks[position]
	scheduleChanged := stored.IntervalMinutes != task.IntervalMinutes || stored.DailyAt != task.DailyAt
	stored.Title = task.Title
	stored.ThreadID = task.ThreadID
	stored.Workspace = task.Workspace
	stored.Model = task.Model
	stored.Prompt = task.Prompt
	stored.IntervalMinutes = task.IntervalMinutes
	stored.DailyAt = task.DailyAt
	stored.Enabled = task.Enabled
	switch {
	case !stored.Enabled:
		stored.NextRunAt = time.Time{}
	case scheduleChanged || stored.NextRunAt.IsZero():
		stored.NextRunAt = nextRunAt(now, stored)
	}
	next := make([]ScheduledTask, len(s.tasks))
	copy(next, s.tasks)
	next[position] = stored
	if err := s.writeStore(next); err != nil {
		return ScheduledTask{}, err
	}
	s.tasks = next
	return stored, nil
}

// Delete removes a task. It does not stop a run that already started.
func (s *Scheduler) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	position := s.position(id)
	if position < 0 {
		return notFound(id)
	}
	next := make([]ScheduledTask, 0, len(s.tasks)-1)
	next = append(next, s.tasks[:position]...)
	next = append(next, s.tasks[position+1:]...)
	if err := s.writeStore(next); err != nil {
		return err
	}
	s.tasks = next
	return nil
}

// TriggerNow dispatches a task immediately, regardless of its schedule. A task
// whose thread is busy is refused: a scheduled prompt must never stack a second
// run behind a live one.
func (s *Scheduler) TriggerNow(ctx context.Context, id string) (api.Receipt, error) {
	outcome := s.runOne(ctx, id)
	if outcome.Busy && outcome.Err == nil {
		return api.Receipt{}, &api.Error{Code: api.Conflict, Message: fmt.Sprintf(
			"目标会话 %s 仍有活动任务，本次触发已拒绝", outcome.ThreadID)}
	}
	// A dispatch that was accepted but could not be recorded returns its receipt
	// together with the persistence error: the run is already moving.
	return outcome.Receipt, outcome.Err
}

// RunDue dispatches every enabled task whose next run is due. A task whose thread
// still has an active run is skipped for this round and recorded as such.
func (s *Scheduler) RunDue(ctx context.Context) []Outcome {
	now := s.now()
	s.mu.Lock()
	due := make([]string, 0, len(s.tasks))
	for _, task := range s.tasks {
		if !task.Enabled || task.NextRunAt.IsZero() || task.NextRunAt.After(now) {
			continue
		}
		if s.inFlight[task.ID] {
			continue
		}
		due = append(due, task.ID)
	}
	s.mu.Unlock()

	outcomes := make([]Outcome, 0, len(due))
	for _, id := range due {
		if err := ctx.Err(); err != nil {
			break
		}
		outcomes = append(outcomes, s.runOne(ctx, id))
	}
	return outcomes
}

// RunLoop keeps dispatching due tasks until ctx is cancelled. It returns nil on
// cancellation; scheduling state stays on disk, so a restart resumes from the
// stored NextRunAt.
func (s *Scheduler) RunLoop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("scheduler: nil context")
	}
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for _, outcome := range s.RunDue(ctx) {
				s.logf("定时任务 %s：会话=%s 运行=%s 跳过=%t 忙碌=%t 错误=%v",
					outcome.TaskID, outcome.ThreadID, outcome.RunID, outcome.Skipped, outcome.Busy, outcome.Err)
			}
		}
	}
}

func (s *Scheduler) runOne(ctx context.Context, id string) Outcome {
	s.mu.Lock()
	position := s.position(id)
	if position < 0 {
		s.mu.Unlock()
		return Outcome{TaskID: id, Err: notFound(id)}
	}
	if s.inFlight[id] {
		s.mu.Unlock()
		return Outcome{TaskID: id, Skipped: true, Err: &api.Error{
			Code: api.Conflict, Message: "该定时任务正在执行中，已跳过重复触发"}}
	}
	s.inFlight[id] = true
	task := s.tasks[position]
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inFlight, id)
		s.mu.Unlock()
	}()

	return s.dispatchTask(ctx, task)
}

func (s *Scheduler) dispatchTask(ctx context.Context, task ScheduledTask) Outcome {
	now := s.now()
	receipt, threadID, busy, err := s.send(ctx, task, now)
	outcome := Outcome{
		TaskID:   task.ID,
		ThreadID: threadID,
		RunID:    receipt.RunID,
		Receipt:  receipt,
		Busy:     busy,
		Skipped:  busy,
		Started:  err == nil && !busy,
		Err:      err,
	}
	if recordErr := s.record(task.ID, now, outcome); recordErr != nil && outcome.Err == nil {
		// The run was accepted; only recording its outcome failed. Keep Started so
		// a caller does not re-trigger work that already began.
		outcome.Err = recordErr
	}
	return outcome
}

// send resolves the target thread, refuses a busy one, and dispatches the prompt.
func (s *Scheduler) send(ctx context.Context, task ScheduledTask, now time.Time) (api.Receipt, string, bool, error) {
	threadID := task.ThreadID
	if threadID != "" {
		state, err := s.threadState(ctx, threadID)
		if err != nil {
			return api.Receipt{}, threadID, false, err
		}
		if !state.Found {
			// The thread was removed; a dedicated one is created again below.
			threadID = ""
		} else {
			if state.ActiveRuns > 0 {
				return api.Receipt{}, threadID, true, nil
			}
			if task.Model != "" && state.Model != "" && state.Model != task.Model {
				if err := s.applyModel(ctx, task, threadID, now); err != nil {
					return api.Receipt{}, threadID, false, err
				}
			}
		}
	}
	if threadID == "" {
		receipt, err := s.service.Dispatch(ctx, api.Command{
			Version:   api.ProtocolVersion,
			CommandID: s.commandID("create", task.ID, now),
			Kind:      api.CreateThread,
			Title:     task.Title,
			Model:     task.Model,
			Workspace: task.Workspace,
		})
		if err != nil {
			return api.Receipt{}, "", false, err
		}
		if receipt.ThreadID == "" {
			return api.Receipt{}, "", false, errors.New("Core 未返回新建会话 ID")
		}
		threadID = receipt.ThreadID
	}
	receipt, err := s.service.Dispatch(ctx, api.Command{
		Version:   api.ProtocolVersion,
		CommandID: s.commandID("send", task.ID, now),
		Kind:      api.SendMessage,
		ThreadID:  threadID,
		Message:   task.Prompt,
	})
	if err != nil {
		if api.CodeOf(err) == api.Conflict {
			// Another run won the thread's single slot between the check and the
			// command. That is a skip, never a second queued run.
			return api.Receipt{}, threadID, true, err
		}
		return api.Receipt{}, threadID, false, err
	}
	return receipt, threadID, false, nil
}

func (s *Scheduler) applyModel(ctx context.Context, task ScheduledTask, threadID string, now time.Time) error {
	_, err := s.service.Dispatch(ctx, api.Command{
		Version:   api.ProtocolVersion,
		CommandID: s.commandID("model", task.ID, now),
		Kind:      api.SetThreadModel,
		ThreadID:  threadID,
		Model:     task.Model,
	})
	return err
}

// record writes the dispatch result and advances the schedule. It is one atomic
// file replacement, so a crash leaves either the previous or the new list.
func (s *Scheduler) record(taskID string, now time.Time, outcome Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	position := s.position(taskID)
	if position < 0 {
		// The task was deleted while it ran; there is nothing to record.
		return nil
	}
	next := make([]ScheduledTask, len(s.tasks))
	copy(next, s.tasks)
	task := &next[position]
	if task.ThreadID == "" && outcome.ThreadID != "" {
		task.ThreadID = outcome.ThreadID
	}
	switch {
	case outcome.Busy:
		task.LastError = busyMessage(outcome)
		advance(task, now)
	case outcome.Err != nil:
		task.LastError = outcome.Err.Error()
		advance(task, now)
	default:
		task.LastRunAt = now
		task.LastRunID = outcome.RunID
		task.LastError = ""
		advance(task, now)
	}
	if err := s.writeStore(next); err != nil {
		return err
	}
	s.tasks = next
	return nil
}

// advance moves a due task to its next slot. A run that happened before the
// scheduled time — a manual trigger — keeps the pending slot.
func advance(task *ScheduledTask, now time.Time) {
	if !task.Enabled || task.NextRunAt.After(now) {
		return
	}
	task.NextRunAt = nextRunAt(now, *task)
}

func busyMessage(outcome Outcome) string {
	message := fmt.Sprintf("目标会话 %s 仍有活动任务，本轮已跳过", outcome.ThreadID)
	if outcome.Err != nil {
		message += "：" + outcome.Err.Error()
	}
	return message
}

// threadState is the minimum a dispatch decision needs about its target thread.
type threadState struct {
	Found      bool
	ActiveRuns int
	Model      string
}

// threadState reads the lightest projection the Core offers: per-thread summaries
// when available, otherwise one snapshot.
func (s *Scheduler) threadState(ctx context.Context, threadID string) (threadState, error) {
	if reader, ok := s.service.(api.ThreadReader); ok {
		summaries, err := reader.ThreadSummaries(ctx)
		if err != nil {
			return threadState{}, err
		}
		for _, summary := range summaries.Threads {
			if summary.ID == threadID {
				return threadState{Found: true, ActiveRuns: summary.ActiveRuns, Model: summary.Model}, nil
			}
		}
		return threadState{}, nil
	}
	snapshot, err := s.service.Snapshot(ctx)
	if err != nil {
		return threadState{}, err
	}
	state := threadState{}
	for _, thread := range snapshot.Threads {
		if thread.ID == threadID {
			state.Found = true
			state.Model = thread.Model
			break
		}
	}
	if state.Found {
		for _, run := range snapshot.Runs {
			if run.ThreadID == threadID && !run.Status.Terminal() {
				state.ActiveRuns++
			}
		}
	}
	return state, nil
}

func (s *Scheduler) storePath() string {
	return filepath.Join(s.dataDir, StoreFileName)
}

func (s *Scheduler) newTaskID(now time.Time) string {
	return fmt.Sprintf("task-%d-%d", now.UnixNano(), s.sequence.Add(1))
}

func (s *Scheduler) commandID(kind, taskID string, now time.Time) string {
	return fmt.Sprintf("sched-%s-%s-%d-%d", kind, taskID, now.UnixNano(), s.sequence.Add(1))
}

func (s *Scheduler) position(id string) int {
	for position := range s.tasks {
		if s.tasks[position].ID == id {
			return position
		}
	}
	return -1
}

// storeFile is the on-disk layout. Version is a schema version of this file.
type storeFile struct {
	Version int             `json:"version"`
	Tasks   []ScheduledTask `json:"tasks"`
}

func (s *Scheduler) writeStore(tasks []ScheduledTask) error {
	if tasks == nil {
		tasks = []ScheduledTask{}
	}
	payload, err := json.MarshalIndent(storeFile{Version: storeVersion, Tasks: tasks}, "", "  ")
	if err != nil {
		return fmt.Errorf("scheduler: 编码定时任务：%w", err)
	}
	payload = append(payload, '\n')
	temp, err := os.CreateTemp(s.dataDir, ".scheduled-tasks-*.json")
	if err != nil {
		return fmt.Errorf("scheduler: 创建临时任务文件：%w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("scheduler: 设置任务文件权限：%w", err)
	}
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return fmt.Errorf("scheduler: 写入任务文件：%w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("scheduler: 同步任务文件：%w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("scheduler: 关闭任务文件：%w", err)
	}
	if err := os.Rename(tempName, s.storePath()); err != nil {
		return fmt.Errorf("scheduler: 替换任务文件：%w", err)
	}
	return nil
}

func loadTasks(path string) ([]ScheduledTask, error) {
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []ScheduledTask{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scheduler: 读取 %s：%w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var stored storeFile
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("scheduler: 解析 %s：%w", path, err)
	}
	if stored.Version != storeVersion {
		return nil, fmt.Errorf("scheduler: 不支持的定时任务文件版本 %d（需要 %d）", stored.Version, storeVersion)
	}
	if stored.Tasks == nil {
		return []ScheduledTask{}, nil
	}
	return stored.Tasks, nil
}

func notFound(id string) error {
	return &api.Error{Code: api.NotFound, Message: fmt.Sprintf("未找到定时任务 %q", id)}
}

// validate rejects a schedule that could never fire or could not be dispatched.
func (task ScheduledTask) validate() error {
	if strings.TrimSpace(task.Title) == "" {
		return &api.Error{Code: api.InvalidCommand, Message: "定时任务需要标题"}
	}
	if strings.TrimSpace(task.Prompt) == "" {
		return &api.Error{Code: api.InvalidCommand, Message: "定时任务需要提示词"}
	}
	if task.IntervalMinutes < 0 {
		return &api.Error{Code: api.InvalidCommand, Message: "intervalMinutes 不能为负数"}
	}
	if task.IntervalMinutes == 0 && strings.TrimSpace(task.DailyAt) == "" {
		return &api.Error{Code: api.InvalidCommand, Message: "定时任务需要执行间隔（分钟）或每日时间（HH:MM）"}
	}
	if task.DailyAt != "" {
		if _, _, err := parseDailyAt(task.DailyAt); err != nil {
			return err
		}
	}
	return nil
}

// nextRunAt resolves the next slot strictly after now. DailyAt takes precedence
// over IntervalMinutes when both are set.
func nextRunAt(now time.Time, task ScheduledTask) time.Time {
	if task.DailyAt != "" {
		hour, minute, err := parseDailyAt(task.DailyAt)
		if err != nil {
			return time.Time{}
		}
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		return next
	}
	return now.Add(time.Duration(task.IntervalMinutes) * time.Minute)
}

func parseDailyAt(value string) (int, int, error) {
	invalid := &api.Error{Code: api.InvalidCommand, Message: fmt.Sprintf("dailyAt 必须是 HH:MM：%q", value)}
	parts := strings.Split(value, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, 0, invalid
	}
	hour, err := parseTwoDigits(parts[0])
	if err != nil {
		return 0, 0, invalid
	}
	minute, err := parseTwoDigits(parts[1])
	if err != nil {
		return 0, 0, invalid
	}
	if hour > 23 || minute > 59 {
		return 0, 0, invalid
	}
	return hour, minute, nil
}

func parseTwoDigits(value string) (int, error) {
	if value[0] < '0' || value[0] > '9' || value[1] < '0' || value[1] > '9' {
		return 0, errors.New("not a two digit number")
	}
	return int(value[0]-'0')*10 + int(value[1]-'0'), nil
}
