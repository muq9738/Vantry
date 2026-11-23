package t3compat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func text(object Object, key string) string { value, _ := object[key].(string); return value }
func object(value any) Object {
	result, _ := value.(map[string]any)
	if result == nil {
		return Object{}
	}
	return result
}
func policy(mode string) (string, error) {
	switch mode {
	case "", "auto", "full-access":
		return "auto", nil
	case "approval-required":
		return "ask", nil
	case "auto-accept-edits":
		return mode, nil
	default:
		return "", errors.New("Vantry 暂不支持此运行权限模式")
	}
}
func runtimeMode(value string) string {
	if value == "ask" {
		return "approval-required"
	}
	return nonempty(value, "auto")
}
func (s *Server) dispatch(ctx context.Context, wire Object) (Object, error) {
	kind := text(wire, "type")
	command := api.Command{Version: api.ProtocolVersion, CommandID: text(wire, "commandId"), ThreadID: text(wire, "threadId"), RunID: text(wire, "runId")}
	if command.CommandID == "" {
		return nil, errors.New("commandId 不能为空")
	}
	if attachments, ok := wire["attachments"].([]any); ok && len(attachments) > 0 {
		return nil, errors.New("此连接尚不支持附件发送")
	}
	if context, ok := wire["context"].([]any); ok && len(context) > 0 {
		return nil, errors.New("此连接尚不支持附加消息上下文")
	}
	model := object(wire["modelSelection"])
	if len(model) > 0 {
		command.ProviderID = nonempty(text(model, "instanceId"), text(model, "provider"))
		command.Model = text(model, "model")
		if command.ProviderID != "" && command.ProviderID != s.opts.Provider {
			return nil, errors.New("模型执行器不属于本机 Vantry 配置")
		}
	}
	var err error
	switch kind {
	case "thread.create":
		command.Kind = api.CreateThread
		command.Title = text(wire, "title")
		command.InteractionMode = text(wire, "interactionMode")
		command.ApprovalPolicy, err = policy(text(wire, "runtimeMode"))
		if err != nil {
			return nil, err
		}
		path, err := s.resolveCreateWorkspacePath(ctx, wire)
		if err != nil {
			return nil, err
		}
		if wt := text(wire, "worktreePath"); wt != "" {
			command.Workspace = &api.WorkspaceSpec{Kind: "worktree", Path: wt, Repository: path, Branch: text(wire, "branch")}
		} else {
			command.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: path, Branch: text(wire, "branch")}
		}
	case "message.dispatch":
		command.Message = text(wire, "text")
		command.MessageID = text(wire, "messageId")
		mode := object(wire["dispatchMode"])
		switch text(mode, "type") {
		case "steer_active":
			command.Kind = api.SteerRun
			command.RunID = text(mode, "targetRunId")
		case "queue_after_active":
			command.Kind = api.QueueMessage
		case "start_immediately", "defer_start", "":
			command.Kind = api.SendMessage
		default:
			return nil, errors.New("尚不支持此消息投递方式")
		}
	case "queued-run.cancel", "queued-run.edit", "queued-message.promote-to-steer":
		queued := text(wire, "runId")
		if kind == "queued-message.promote-to-steer" {
			queued = text(wire, "queuedRunId")
		}
		if !strings.HasPrefix(queued, "input/") {
			return nil, errors.New("该运行不属于待处理输入队列")
		}
		command.InputID = strings.TrimPrefix(queued, "input/")
		command.RunID = ""
		if kind == "queued-run.cancel" {
			command.Kind = api.CancelInput
		} else if kind == "queued-run.edit" {
			command.Kind = api.EditInput
			command.Message = text(wire, "text")
		} else {
			command.Kind = api.PromoteInputToSteer
			command.RunID = text(wire, "targetRunId")
		}
	case "thread.model-selection.set":
		command.Kind = api.SetThreadModel
	case "thread.runtime-mode.set":
		command.Kind = api.UpdateThread
		command.ApprovalPolicy, err = policy(text(wire, "runtimeMode"))
		if err != nil {
			return nil, err
		}
	case "thread.interaction-mode.set":
		command.Kind = api.UpdateThread
		command.InteractionMode = text(wire, "interactionMode")
	case "thread.metadata.update":
		command.Kind = api.UpdateThread
		command.Title = text(wire, "title")
	case "thread.archive", "thread.unarchive", "thread.settle", "thread.auto-settle", "thread.unsettle":
		command.Kind = api.UpdateThread
		value := kind == "thread.archive" || kind == "thread.settle" || kind == "thread.auto-settle"
		command.Archived = &value
	case "thread.snooze":
		command.Kind = api.UpdateThread
		rawUntil := text(wire, "snoozedUntil")
		until, parseErr := time.Parse(time.RFC3339Nano, rawUntil)
		if parseErr != nil {
			until, parseErr = time.Parse(time.RFC3339, rawUntil)
		}
		if parseErr != nil {
			return nil, errors.New("无效的延后时间格式")
		}
		until = until.UTC()
		command.SnoozedUntil = &until
	case "thread.unsnooze":
		command.Kind = api.UpdateThread
		zero := time.Time{}
		command.SnoozedUntil = &zero
	case "thread.visit", "thread.mark-unread", "thread.auto-settle.set", "thread.pin.reorder", "thread.active.reorder":
		snapshot, err := s.service.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		return Object{"sequence": snapshot.Sequence}, nil
	case "thread.pin", "thread.unpin":
		command.Kind = api.UpdateThread
		value := kind == "thread.pin"
		command.Pinned = &value
	case "thread.delete":
		command.Kind = api.DeleteThread
	case "run.interrupt", "thread.stop":
		command.Kind = api.StopRun
		if command.RunID == "" {
			snapshot, err := s.service.Snapshot(ctx)
			if err != nil {
				return nil, err
			}
			for _, run := range snapshot.Runs {
				if run.ThreadID == command.ThreadID && !run.Status.Terminal() {
					command.RunID = run.ID
					break
				}
			}
			if command.RunID == "" {
				return Object{"sequence": snapshot.Sequence}, nil
			}
		}
	case "runtime-request.respond":
		command.Kind = api.RespondRequest
		command.RequestID = text(wire, "requestId")
		snapshot, err := s.service.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		var request *api.ProviderRequest
		for _, item := range snapshot.Requests {
			if item.ID == command.RequestID && item.ThreadID == command.ThreadID {
				copy := item
				request = &copy
				break
			}
		}
		if request == nil {
			return nil, errors.New("请求不属于此会话或已不存在")
		}
		command.RunID = request.RunID
		if request.Method == "confirm" {
			decision := text(wire, "decision")
			switch decision {
			case "accept":
				command.Confirmed = true
			case "decline":
				command.Confirmed = false
			case "cancel":
				command.Cancelled = true
			default:
				return nil, errors.New("只支持本次允许、拒绝或取消；不能扩大为永久授权")
			}
		} else {
			answers := object(wire["answers"])
			if value, ok := answers[request.ID]; ok {
				command.Value = answerText(value)
			} else if len(answers) == 1 {
				for _, value := range answers {
					command.Value = answerText(value)
				}
			} else {
				return nil, errors.New("请回答此请求对应的问题")
			}
		}
	default:
		return nil, fmt.Errorf("Vantry 尚不支持 T3 操作 %q", kind)
	}
	receipt, err := s.service.Dispatch(ctx, command)
	if err != nil {
		return nil, err
	}
	return Object{"sequence": receipt.Sequence}, nil
}
func (s *Server) launch(ctx context.Context, input Object) (Object, error) {
	commandID := text(input, "commandId")
	if commandID == "" {
		return nil, errors.New("commandId 不能为空")
	}
	threadID := text(input, "threadId")
	if threadID == "" {
		threadID = "t3_" + commandID
	}
	resumed := false
	if reuse, _ := input["reuseExistingThread"].(bool); reuse && threadID != "" {
		if snap, err := s.service.Snapshot(ctx); err == nil {
			for _, th := range snap.Threads {
				if th.ID == threadID {
					resumed = true
					break
				}
			}
		}
	}
	if !resumed {
		strategy := object(input["workspaceStrategy"])
		kind := text(strategy, "type")
		switch kind {
		case "", "root", "project", "local", "directory", "existing_worktree", "worktree", "scratch", "none":
		default:
			return nil, errors.New("目前只支持在已有项目目录或工作树启动会话")
		}
		create := Object{}
		for key, value := range input {
			create[key] = value
		}
		create["type"] = "thread.create"
		create["threadId"] = threadID
		create["commandId"] = commandID + "/create"
		if b := text(strategy, "branch"); b != "" && text(create, "branch") == "" {
			create["branch"] = b
		}
		if p := text(strategy, "path"); p != "" && text(create, "workspacePath") == "" {
			create["workspacePath"] = p
		}
		if kind == "existing_worktree" {
			if wt := text(strategy, "worktreePath"); wt != "" {
				create["worktreePath"] = wt
			}
		}
		if _, err := s.dispatch(ctx, create); err != nil {
			return nil, err
		}
	}
	if initial := object(input["initialMessage"]); len(initial) > 0 {
		message := Object{"type": "message.dispatch", "commandId": commandID + "/message", "threadId": threadID, "messageId": nonempty(text(initial, "messageId"), commandID+"/message"), "text": text(initial, "text"), "attachments": initial["attachments"], "dispatchMode": Object{"type": "start_immediately"}}
		if _, err := s.dispatch(ctx, message); err != nil {
			return nil, err
		}
	}
	projection, sequence, err := s.projection(ctx, threadID)
	if err != nil {
		return nil, err
	}
	return Object{"threadId": threadID, "projection": projection, "resumed": resumed, "sequence": sequence}, nil
}
func (s *Server) resolveCreateWorkspacePath(ctx context.Context, wire Object) (string, error) {
	strategy := object(wire["workspaceStrategy"])
	strategyType := text(strategy, "type")
	noProject, _ := wire["noProject"].(bool)
	if strategyType == "scratch" || strategyType == "none" || noProject || text(wire, "projectId") == projectID(scratchProjectIdentity) {
		return s.ensureScratchWorkspacePath(text(wire, "threadId"), text(wire, "commandId"))
	}
	explicit := nonempty(text(wire, "workspacePath"), nonempty(text(wire, "workspaceRoot"), text(strategy, "path")))
	if explicit == "" {
		return s.projectPath(ctx, text(wire, "projectId"))
	}
	if strings.ContainsRune(explicit, 0) || strings.Contains(explicit, "environment:") {
		return "", errors.New("不支持在该目录启动会话")
	}
	home, _ := os.UserHomeDir()
	trimmed := strings.TrimSpace(explicit)
	if trimmed == "~" && home != "" {
		trimmed = home
	} else if (strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, "~\\")) && home != "" {
		trimmed = filepath.Join(home, trimmed[2:])
	}
	cleaned := filepath.Clean(trimmed)
	if !filepath.IsAbs(cleaned) {
		return "", errors.New("所选目录路径必须是绝对路径")
	}
	info, err := os.Stat(cleaned)
	if err != nil || !info.IsDir() {
		return "", errors.New("所选目录不存在或不是有效文件夹")
	}
	return cleaned, nil
}

func (s *Server) ensureScratchWorkspacePath(threadID, fallbackID string) (string, error) {
	if s.opts.DataDir == "" {
		return "", errors.New("未配置数据目录，无法创建无项目会话")
	}
	id := strings.TrimSpace(threadID)
	if id == "" {
		id = strings.TrimSpace(fallbackID)
	}
	if id == "" {
		id = "scratch"
	}
	safe := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(id)
	if safe == "" || safe == "." {
		safe = "scratch"
	}
	dir := filepath.Join(s.opts.DataDir, "workspaces", safe, "chat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("无法创建临时会话目录：%w", err)
	}
	return dir, nil
}

func unsupportedMethod(method string) error {
	return fmt.Errorf("Vantry 尚未提供此 T3 接口：%s", strings.TrimSpace(method))
}
