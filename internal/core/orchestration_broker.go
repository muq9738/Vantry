package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

var _ api.OrchestrationBroker = (*Core)(nil)
var _ api.ThreadReader = (*Core)(nil)
var _ api.StatusReporter = (*Core)(nil)

func (c *Core) SetModelCatalogProvider(fn func(context.Context, string, string) (api.ModelCatalog, error)) {
	c.catalogMu.Lock()
	c.modelCatalogProvider = fn
	c.catalogMu.Unlock()
}

func splitThinking(model string) (string, string) {
	if index := strings.LastIndexByte(model, ':'); index >= 0 {
		switch model[index+1:] {
		case "off", "minimal", "low", "medium", "high", "xhigh", "max":
			return model[:index], model[index+1:]
		}
	}
	return model, ""
}

func orchestrationModel(model, thinking, inherited string) string {
	model = firstNonEmpty(model, inherited)
	if thinking != "" {
		model, _ = splitThinking(model)
		model += ":" + thinking
	}
	return model
}

func workspacePath(spec *api.WorkspaceSpec) string {
	if spec == nil {
		return ""
	}
	return firstNonEmpty(spec.Path, spec.Repository)
}

func (c *Core) OrchestratorCapabilities(ctx context.Context, query api.OrchestratorCapabilitiesQuery) (api.OrchestratorCapabilities, error) {
	thread := threadRecord{ProviderID: c.defaultProviderID, Model: c.defaultModel, Workspace: c.defaultWorkspace,
		ApprovalPolicy: approvalAuto, InteractionMode: interactionDefault}
	if err := c.orchestrationRead(ctx, func(t *txn) error {
		if query.CallerThreadID == "" {
			return nil
		}
		var err error
		thread, err = requiredThread(t, query.CallerThreadID)
		return err
	}); err != nil {
		return api.OrchestratorCapabilities{}, err
	}
	model, thinking := splitThinking(thread.Model)
	result := api.OrchestratorCapabilities{ParentThreadID: thread.ID,
		InheritedProviderID: thread.ProviderID, InheritedModel: model, InheritedThinking: thinking,
		ApprovalPolicy: thread.ApprovalPolicy, InteractionMode: thread.InteractionMode,
		Models: []api.OrchestratorModelCapability{}, MaxBatchThreads: 20}
	for _, policy := range []string{approvalAsk, approvalAcceptEdits, approvalAuto} {
		if enforceCeiling(thread, policy, thread.InteractionMode) == nil {
			result.ApprovalPolicies = append(result.ApprovalPolicies, policy)
		}
	}
	for _, mode := range []string{"plan", interactionDefault} {
		if enforceCeiling(thread, thread.ApprovalPolicy, mode) == nil {
			result.InteractionModes = append(result.InteractionModes, mode)
		}
	}
	c.catalogMu.RLock()
	catalogFn := c.modelCatalogProvider
	c.catalogMu.RUnlock()
	catalog := api.ModelCatalog{}
	if catalogFn != nil {
		var err error
		// The catalog is keyed by provider/model only; a thinking suffix such as
		// ":high" is an option of the model, not part of its catalog key.
		catalog, err = catalogFn(ctx, workspacePath(thread.Workspace), model)
		if err != nil {
			return result, err
		}
	} else if model != "" {
		catalog.Models = []api.ModelInfo{{ID: model, Name: model}}
		if thinking != "" {
			catalog.ThinkingLevels = []string{thinking}
		}
	}
	for _, entry := range catalog.Models {
		id := entry.Key()
		if query.Search != "" && !strings.Contains(strings.ToLower(id+" "+entry.Name), strings.ToLower(query.Search)) {
			continue
		}
		capability := api.OrchestratorModelCapability{ID: id, Label: entry.Name}
		if len(catalog.ThinkingLevels) > 0 {
			capability.Options = []api.OrchestratorModelOption{{ID: "thinking", Values: append([]string(nil), catalog.ThinkingLevels...), Default: thinking}}
		}
		result.Models = append(result.Models, capability)
	}
	return result, nil
}

func brokerCommandID(kind, caller, clientRequestID string) string {
	if clientRequestID != "" {
		return kind + ":" + caller + ":" + clientRequestID
	}
	return newID(kind)
}

// Retrying an accepted broker request uses the stored command configuration,
// even if its caller subsequently changed model or workspace settings.
func (c *Core) priorBrokerCommand(ctx context.Context, id string, req any) (*api.Command, error) {
	var command *api.Command
	err := c.orchestrationRead(ctx, func(t *txn) error {
		var payload string
		err := t.queryRow(`SELECT payload FROM command_receipts WHERE command_id = ?`, id).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var prior api.Command
		if err := json.Unmarshal([]byte(payload), &prior); err != nil {
			return wrapInternal("read broker command", err)
		}
		if prior.Value != brokerRequestValue(req) {
			return errorf(api.Conflict, "clientRequestId was already used with different content")
		}
		command = &prior
		return nil
	})
	return command, err
}

func brokerRequestValue(req any) string {
	raw, _ := json.Marshal(req)
	return string(raw)
}

func (c *Core) DelegateTask(ctx context.Context, req api.DelegateTaskRequest) (api.TaskDetail, error) {
	mode, isolation := firstNonEmpty(req.Mode, "async"), firstNonEmpty(req.Isolation, "shared")
	if mode != "async" && mode != "wait" || isolation != "shared" && isolation != "worktree" || req.TimeoutMs < 0 {
		return api.TaskDetail{}, errorf(api.InvalidCommand, "unsupported delegation mode or isolation")
	}
	commandID := brokerCommandID("delegate", req.CallerThreadID, req.ClientRequestID)
	prior, err := c.priorBrokerCommand(ctx, commandID, req)
	if err != nil {
		return api.TaskDetail{}, err
	}
	if prior != nil {
		receipt, err := c.Dispatch(ctx, *prior)
		if err != nil {
			return api.TaskDetail{}, err
		}
		if mode == "wait" {
			return c.awaitDelegatedTask(ctx, receipt.TaskID, req.TimeoutMs)
		}
		return c.taskDetail(ctx, receipt.TaskID)
	}
	var parent runRecord
	var thread threadRecord
	if err := c.orchestrationRead(ctx, func(t *txn) error {
		var err error
		parent, err = loadRun(t, req.ParentRunID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "parent run %q not found", req.ParentRunID)
		}
		if err != nil {
			return err
		}
		if req.CallerThreadID != "" && req.CallerThreadID != parent.ThreadID {
			return errorf(api.Conflict, "parent run does not belong to caller thread")
		}
		thread, err = requiredThread(t, parent.ThreadID)
		return err
	}); err != nil {
		return api.TaskDetail{}, err
	}
	workspace := firstWorkspace(req.Workspace, thread.Workspace)
	providerID, inheritedModel := req.ProviderID, thread.Model
	if providerID != "" && providerID != thread.ProviderID {
		inheritedModel = ""
	}
	if req.ResolvedTarget != nil {
		workspace, providerID, inheritedModel = req.ResolvedTarget.Workspace, req.ResolvedTarget.ProviderID, req.ResolvedTarget.Model
	}
	if req.EnvironmentID != "" {
		// The app resolves and validates the linked target before dispatch. A
		// remote workspace is opaque here: never run local git on its paths.
		if req.ResolvedTarget == nil || workspace == nil || workspace.EnvironmentID != req.EnvironmentID {
			return api.TaskDetail{}, errorf(api.InvalidCommand, "remote delegation requires a resolved target workspace")
		}
	} else if workspace != nil && workspace.EnvironmentID != "" {
		return api.TaskDetail{}, errorf(api.InvalidCommand, "environmentId is required for a remote workspace")
	}
	if isolation == "worktree" && req.EnvironmentID == "" {
		path := workspacePath(workspace)
		if path == "" {
			return api.TaskDetail{}, errorf(api.InvalidCommand, "worktree isolation requires a git workspace with a commit")
		}
		root, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return api.TaskDetail{}, errorf(api.InvalidCommand, "worktree isolation requires a valid git repository")
		}
		repository := strings.TrimSpace(string(root))
		if err := exec.CommandContext(ctx, "git", "-C", repository, "rev-parse", "--verify", "HEAD").Run(); err != nil {
			return api.TaskDetail{}, errorf(api.InvalidCommand, "worktree isolation requires a git repository with a commit")
		}
		workspace = &api.WorkspaceSpec{Kind: "worktree", Repository: repository, BaseRef: "HEAD"}
	}
	policy := req.ResumePolicy
	if mode == "wait" && policy == "" {
		policy = resumeManual
	}
	message := req.Task
	if req.Role != "" {
		message = "[Role: " + req.Role + "]\n\n" + message
	}
	if strings.TrimSpace(req.Task) == "" {
		return api.TaskDetail{}, errorf(api.InvalidCommand, "task is required")
	}
	receipt, err := c.Dispatch(ctx, api.Command{Version: api.ProtocolVersion,
		CommandID: commandID, Kind: api.DelegateTask, Value: brokerRequestValue(req),
		ThreadID: parent.ThreadID, ParentRunID: req.ParentRunID, ParentAttemptID: req.ParentAttemptID,
		Title: req.Title, Message: message, ProviderID: providerID, Model: orchestrationModel(req.Model, req.Thinking, inheritedModel),
		Workspace: workspace, ApprovalPolicy: req.ApprovalPolicy, InteractionMode: req.InteractionMode, ResumePolicy: policy})
	if err != nil {
		return api.TaskDetail{}, err
	}
	if mode == "wait" {
		return c.awaitDelegatedTask(ctx, receipt.TaskID, req.TimeoutMs)
	}
	return c.taskDetail(ctx, receipt.TaskID)
}

func pendingThreadRequests(t *txn, threadID string) (bool, *api.PendingApprovalSummary, error) {
	rows, err := t.query(`SELECT id, method, title, run_id FROM provider_requests
		WHERE thread_id = ? AND status IN ('pending', 'responding') ORDER BY created_at, id`, threadID)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	question := false
	var approval *api.PendingApprovalSummary
	for rows.Next() {
		var item api.PendingApprovalSummary
		if err := rows.Scan(&item.RequestID, &item.Method, &item.Title, &item.RunID); err != nil {
			return false, nil, err
		}
		if item.Method == "confirm" {
			if approval == nil {
				approval = &item
			}
		} else if item.Method == "input" || item.Method == "select" || item.Method == "editor" {
			question = true
		}
	}
	return question, approval, rows.Err()
}

func buildTaskDetail(t *txn, task taskRecord) (api.TaskDetail, error) {
	detail := api.TaskDetail{TaskID: task.ID, ParentThreadID: task.ParentThreadID, ParentRunID: task.ParentRunID,
		ChildThreadID: task.ChildThreadID, ChildRunID: task.ChildRunID, Title: task.Title, Status: api.RunStatus(task.Status),
		DeliveryStatus: api.DeliveryStatus(task.DeliveryStatus), Summary: task.Result, Error: task.Error, UpdatedAt: task.UpdatedAt}
	thread, err := requiredThread(t, task.ChildThreadID)
	if err != nil {
		return detail, err
	}
	detail.Link, detail.Model = thread.link(), thread.Model
	if thread.Workspace != nil {
		detail.EnvironmentID = thread.Workspace.EnvironmentID
	}
	run, err := loadRun(t, task.ChildRunID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return detail, err
	}
	if err == nil {
		detail.ChildAttemptID = run.AttemptID
	}
	if err == nil && task.DeliveryStatus != deliveryDisposed {
		detail.Status, detail.UpdatedAt = api.RunStatus(run.Status), run.UpdatedAt
		if detail.Status.Terminal() {
			detail.Summary, detail.Error = run.Output, run.Error
		}
	}
	if err := t.queryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE parent_thread_id = ? AND status IN ('queued', 'running', 'waiting_children'))
		OR EXISTS(SELECT 1 FROM runs WHERE thread_id = ? AND id <> ? AND status IN ('queued', 'running', 'waiting_children'))`, thread.ID, thread.ID, task.ChildRunID).Scan(&detail.HasPendingChildRuns); err != nil {
		return detail, err
	}
	detail.HasPendingQuestion, detail.PendingApproval, err = pendingThreadRequests(t, thread.ID)
	if err != nil {
		return detail, err
	}
	detail.WorkState = "working"
	if detail.Status == api.RunWaiting || detail.HasPendingChildRuns {
		detail.WorkState = "waiting_for_children"
	} else if detail.Status.Terminal() {
		detail.WorkState = "result_available"
	}
	if (!detail.Status.Terminal() || detail.HasPendingChildRuns) && !detail.HasPendingQuestion && detail.PendingApproval == nil {
		detail.Next = api.OrchestratorRunningTaskNext
	}
	return detail, nil
}

func (c *Core) taskDetail(ctx context.Context, taskID string) (api.TaskDetail, error) {
	var detail api.TaskDetail
	err := c.orchestrationRead(ctx, func(t *txn) error {
		task, err := loadTask(t, taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return errorf(api.NotFound, "task %q not found", taskID)
		}
		if err != nil {
			return err
		}
		detail, err = buildTaskDetail(t, task)
		return err
	})
	return detail, err
}

func (c *Core) TaskStatus(ctx context.Context, query api.TaskStatusQuery) (api.TaskStatusResult, error) {
	result := api.TaskStatusResult{ThreadID: query.CallerThreadID, RunID: query.ParentRunID, Tasks: []api.TaskDetail{}, Cohorts: []api.Cohort{}}
	if query.TaskID == "" && query.ParentRunID == "" && query.CallerThreadID == "" {
		return result, errorf(api.InvalidCommand, "taskId, parentRunId or callerThreadId is required")
	}
	err := c.orchestrationRead(ctx, func(t *txn) error {
		filter, id := "parent_thread_id = ?", query.CallerThreadID
		if query.ParentRunID != "" {
			filter, id = "parent_run_id = ?", query.ParentRunID
		}
		if query.TaskID != "" {
			filter, id = "id = ?", query.TaskID
		}
		tasks, err := readRecords(t, `SELECT `+taskColumns+` FROM tasks WHERE `+filter+` ORDER BY created_at, id`, scanTask, id)
		if err != nil {
			return err
		}
		if query.TaskID != "" && len(tasks) == 0 {
			return errorf(api.NotFound, "task %q not found", query.TaskID)
		}
		seen := map[string]bool{}
		for _, task := range tasks {
			if query.ParentRunID != "" && task.ParentRunID != query.ParentRunID {
				return errorf(api.Conflict, "task belongs to a different parent run")
			}
			detail, err := buildTaskDetail(t, task)
			if err != nil {
				return err
			}
			result.Tasks = append(result.Tasks, detail)
			if seen[task.ParentRunID] {
				continue
			}
			seen[task.ParentRunID] = true
			cohorts, err := readRecords(t, `SELECT `+cohortColumns+` FROM cohorts WHERE parent_run_id = ? ORDER BY generation`, scanCohort, task.ParentRunID)
			if err != nil {
				return err
			}
			for _, cohort := range cohorts {
				result.Cohorts = append(result.Cohorts, cohort.toAPI())
			}
		}
		return nil
	})
	if err == nil && query.Acknowledge {
		for _, task := range result.Tasks {
			if query.CallerThreadID != "" && query.CallerThreadID != task.ParentThreadID {
				return result, errorf(api.Conflict, "only the parent thread may acknowledge a task")
			}
		}
		if err := c.observeTaskResults(ctx, result.Tasks); err != nil {
			return result, err
		}
		query.Acknowledge = false
		return c.TaskStatus(ctx, query)
	}
	return result, err
}

func (c *Core) observeTaskResults(ctx context.Context, observed []api.TaskDetail) error {
	if len(observed) == 0 {
		return nil
	}
	err := c.dispatchWrite(ctx, func(t *txn) error {
		for _, expected := range observed {
			task, err := loadTask(t, expected.TaskID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if task.ChildRunID != expected.ChildRunID || !expected.Status.Terminal() {
				continue
			}
			cohort, found, err := loadCohort(t, task.ParentRunID, task.Generation)
			if err != nil {
				return err
			}
			if !found || cohort.Status != deliveryDelivered {
				continue
			}
			tasks, err := loadCohortTasks(t, task.ParentRunID, task.Generation)
			if err != nil {
				return err
			}
			if err := c.acknowledgeCohort(t, cohort, tasks); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		c.notifyWorker()
	}
	return err
}

func (c *Core) CancelTask(ctx context.Context, req api.CancelTaskRequest) (api.CancelTaskResult, error) {
	_, err := c.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, Kind: api.CancelTask,
		CommandID: brokerCommandID("cancel", req.CallerThreadID, req.ClientRequestID), ThreadID: req.CallerThreadID,
		TaskID: req.TaskID, Reason: req.Reason})
	result := api.CancelTaskResult{TaskID: req.TaskID}
	if err == nil {
		err = c.orchestrationRead(ctx, func(t *txn) error {
			task, err := loadTask(t, req.TaskID)
			result.Status = task.Status
			return err
		})
	}
	return result, err
}

func (c *Core) LaunchThread(ctx context.Context, req api.ThreadLaunchRequest) (api.LaunchedThread, error) {
	commandID := brokerCommandID("launch", req.CallerThreadID, req.ClientRequestID)
	prior, err := c.priorBrokerCommand(ctx, commandID, req)
	if err != nil {
		return api.LaunchedThread{}, err
	}
	if prior != nil {
		return c.launchFromCommand(ctx, req, *prior)
	}
	var caller threadRecord
	if err := c.orchestrationRead(ctx, func(t *txn) error {
		if req.CallerThreadID == "" {
			return nil
		}
		var err error
		caller, err = requiredThread(t, req.CallerThreadID)
		if err == nil && req.CallerRunID != "" {
			run, runErr := loadRun(t, req.CallerRunID)
			if runErr != nil || run.ThreadID != caller.ID {
				return errorf(api.Conflict, "callerRunId does not belong to caller thread")
			}
		}
		return err
	}); err != nil {
		return api.LaunchedThread{}, err
	}
	policy := firstNonEmpty(req.ApprovalPolicy, caller.ApprovalPolicy, approvalAuto)
	mode := firstNonEmpty(req.InteractionMode, caller.InteractionMode, interactionDefault)
	if caller.ID != "" {
		if err := enforceCeiling(caller, policy, mode); err != nil {
			return api.LaunchedThread{}, err
		}
	}
	threadID := newID("th")
	workspace := firstWorkspace(req.Workspace, caller.Workspace)
	if req.Scratch {
		if req.Workspace != nil {
			return api.LaunchedThread{}, errorf(api.InvalidCommand, "scratch and workspace cannot both be specified")
		}
		workspace = &api.WorkspaceSpec{Kind: "directory", Path: filepath.Join(c.store.directory, "workspaces", threadID, "chat")}
	}
	command := api.Command{Version: api.ProtocolVersion, CommandID: commandID, Kind: api.CreateThread,
		ThreadID: threadID, Value: brokerRequestValue(req), Title: req.Title,
		Model: orchestrationModel(req.Model, req.Thinking, caller.Model), ProviderID: caller.ProviderID,
		ApprovalPolicy: policy, InteractionMode: mode, Workspace: workspace,
		CreatedByThreadID: req.CallerThreadID, CreatedByRunID: req.CallerRunID}
	return c.launchFromCommand(ctx, req, command)
}

func (c *Core) launchFromCommand(ctx context.Context, req api.ThreadLaunchRequest, command api.Command) (api.LaunchedThread, error) {
	receipt, err := c.Dispatch(ctx, command)
	if err != nil {
		return api.LaunchedThread{}, err
	}
	result := api.LaunchedThread{ThreadID: receipt.ThreadID}
	if req.Scratch {
		if err := os.MkdirAll(command.Workspace.Path, 0o700); err != nil {
			return result, wrapInternal("prepare scratch workspace", err)
		}
	}
	if strings.TrimSpace(req.Message) != "" {
		sent, err := c.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, CommandID: command.CommandID + ":message", Kind: api.SendMessage,
			ThreadID: receipt.ThreadID, SenderThreadID: req.CallerThreadID, Message: req.Message})
		if err != nil {
			return result, err
		}
		result.RunID = sent.RunID
	}
	err = c.orchestrationRead(ctx, func(t *txn) error {
		thread, err := requiredThread(t, receipt.ThreadID)
		if err != nil {
			return err
		}
		result.Title, result.Model, result.Link = thread.Title, thread.Model, thread.link()
		if result.RunID != "" {
			run, err := loadRun(t, result.RunID)
			if err != nil {
				return err
			}
			result.Status = api.RunStatus(run.Status)
		}
		return nil
	})
	if err == nil && req.NotifyOnComplete && req.CallerThreadID != "" && req.CallerThreadID != result.ThreadID && result.RunID != "" {
		if _, watchErr := c.registerThreadWatch(ctx, req.CallerThreadID, req.CallerRunID, result.ThreadID, result.RunID); watchErr != nil {
			return result, watchErr
		}
	}
	return result, err
}

func (c *Core) registerThreadWatch(ctx context.Context, watcherThreadID, watcherRunID, targetThreadID, targetRunID string) (bool, error) {
	if strings.TrimSpace(watcherThreadID) == "" || strings.TrimSpace(targetThreadID) == "" || watcherThreadID == targetThreadID {
		return false, nil
	}
	var started bool
	err := c.dispatchWrite(ctx, func(t *txn) error {
		watcher, err := loadThread(t, watcherThreadID)
		if err != nil || watcher.Archived {
			return nil
		}
		var existingID string
		err = t.queryRow(`SELECT id FROM thread_watches WHERE status = ? AND watcher_thread_id = ? AND target_thread_id = ? AND target_run_id = ? LIMIT 1`,
			watchPending, watcherThreadID, targetThreadID, targetRunID).Scan(&existingID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return wrapInternal("read thread watch", err)
		}
		now := time.Now().UTC().UnixNano()
		if _, err := t.exec(`INSERT INTO thread_watches
			(id, watcher_thread_id, watcher_run_id, target_thread_id, target_run_id, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			newID("watch"), watcherThreadID, watcherRunID, targetThreadID, targetRunID, watchPending, now, now); err != nil {
			return wrapInternal("insert thread watch", err)
		}
		if targetRunID != "" {
			run, runErr := loadRun(t, targetRunID)
			if runErr == nil && api.RunStatus(run.Status).Terminal() {
				ok, recErr := c.reconcileThreadWatches(t, run)
				if recErr != nil {
					return recErr
				}
				started = started || ok
			}
		}
		return nil
	})
	if err == nil && started {
		c.notifyWorker()
	}
	return started, err
}

func (c *Core) CreateThreads(ctx context.Context, req api.CreateThreadsRequest) (api.CreateThreadsResult, error) {
	result := api.CreateThreadsResult{Threads: []api.LaunchedThread{}}
	if len(req.Threads) == 0 || len(req.Threads) > 20 {
		return result, errorf(api.InvalidCommand, "create_threads requires between 1 and 20 threads")
	}
	for i, spec := range req.Threads {
		clientID := ""
		if req.ClientRequestID != "" {
			clientID = fmt.Sprintf("%s:thread:%d", req.ClientRequestID, i)
		}
		thread, err := c.LaunchThread(ctx, api.ThreadLaunchRequest{ClientRequestID: clientID,
			CallerThreadID: req.CallerThreadID, CallerRunID: req.CallerRunID, ThreadLaunchSpec: spec})
		if err != nil {
			return result, err
		}
		result.Threads = append(result.Threads, thread)
	}
	return result, nil
}

func (c *Core) SendToThread(ctx context.Context, req api.InterThreadSendRequest) (api.InterThreadSendResult, error) {
	receipt, err := c.Dispatch(ctx, api.Command{Version: api.ProtocolVersion, Kind: api.InterThreadSend,
		CommandID: brokerCommandID("send", req.CallerThreadID, req.ClientRequestID), SenderThreadID: req.CallerThreadID,
		TargetThreadID: req.TargetThreadID, Message: req.Message, DeliveryMode: req.Mode, FallbackToQueue: true})
	if err != nil {
		return api.InterThreadSendResult{}, err
	}
	result := api.InterThreadSendResult{ThreadID: receipt.ThreadID, RunID: receipt.RunID, InputID: receipt.InputID,
		TaskID: receipt.TaskID, Delivery: receipt.Delivery, Sequence: receipt.Sequence}
	err = c.orchestrationRead(ctx, func(t *txn) error {
		thread, err := requiredThread(t, receipt.ThreadID)
		if err != nil {
			return err
		}
		result.Link = thread.link()
		if receipt.RunID != "" {
			run, err := loadRun(t, receipt.RunID)
			if err != nil {
				return err
			}
			result.Status = api.RunStatus(run.Status)
		}
		return nil
	})
	if err == nil && req.NotifyOnComplete && req.CallerThreadID != "" && req.CallerThreadID != result.ThreadID {
		if _, watchErr := c.registerThreadWatch(ctx, req.CallerThreadID, "", result.ThreadID, result.RunID); watchErr != nil {
			return result, watchErr
		}
	}
	return result, err
}
