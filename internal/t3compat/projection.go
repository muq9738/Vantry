package t3compat

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func nonempty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
const scratchProjectIdentity = "vantry:scratch"

func projectID(path string) string {
	return "workspace_" + base64.RawURLEncoding.EncodeToString([]byte(path))
}
func (s *Server) isScratchWorkspacePath(path string) bool {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "" || cleaned == "." {
		return false
	}
	if filepath.Base(cleaned) != "chat" {
		return false
	}
	if s.opts.DataDir != "" {
		root := filepath.Clean(filepath.Join(s.opts.DataDir, "workspaces"))
		if filepath.Dir(filepath.Dir(cleaned)) == root {
			return true
		}
	}
	slash := filepath.ToSlash(cleaned)
	return strings.HasSuffix(slash, "/chat") && strings.Contains(slash, "/workspaces/")
}
func (s *Server) workspace(thread api.Thread) string {
	path := api.WorkspaceProjectPath(thread.Workspace)
	if path == "" {
		path = api.WorkspaceProjectPath(s.opts.Workspace)
	}
	return nonempty(path, s.opts.DataDir)
}
func (s *Server) projectIdentity(thread api.Thread) string {
	if thread.Workspace != nil && thread.Workspace.EnvironmentID != "" {
		path := s.workspace(thread)
		return "environment:" + thread.Workspace.EnvironmentID + "\n" + path
	}
	raw := api.WorkspaceProjectPath(thread.Workspace)
	if s.isScratchWorkspacePath(raw) {
		return scratchProjectIdentity
	}
	if raw == "" && s.opts.Workspace == nil {
		return scratchProjectIdentity
	}
	return s.workspace(thread)
}
func selection(provider, model string) Object {
	return Object{"instanceId": nonempty(provider, "pi"), "model": nonempty(model, "default")}
}
func (s *Server) thread(thread api.Thread) Object {
	parent := thread.ParentThreadID
	relationship := any(nil)
	if parent != "" {
		relationship = "subagent"
	}
	if thread.ForkedFromThreadID != "" {
		parent = thread.ForkedFromThreadID
		relationship = "fork"
	}
	archived := any(nil)
	settledOverride := any(nil)
	settledAt := any(nil)
	if thread.Archived {
		archived = utcISO(thread.CreatedAt)
		settledOverride = "settled"
		settledAt = archived
	}
	pinned := any(nil)
	if thread.Pinned {
		pinned = utcISO(thread.CreatedAt)
	}
	branch, worktree := any(nil), any(nil)
	if thread.Workspace != nil {
		branch = nullable(thread.Workspace.Branch)
		if thread.Workspace.Kind == "worktree" {
			worktree = nullable(thread.Workspace.Path)
		}
	}
	return Object{"id": thread.ID, "projectId": projectID(s.projectIdentity(thread)), "title": nonempty(thread.Title, "新会话"), "providerInstanceId": nonempty(thread.ProviderID, s.opts.Provider), "modelSelection": selection(nonempty(thread.ProviderID, s.opts.Provider), nonempty(thread.Model, s.opts.Model)), "runtimeMode": runtimeMode(thread.ApprovalPolicy), "interactionMode": nonempty(thread.InteractionMode, "default"), "branch": branch, "worktreePath": worktree,
		"createdBy": "user", "creationSource": "server", "lineage": Object{"parentThreadId": nullable(parent), "relationshipToParent": relationship, "rootThreadId": nonempty(parent, thread.ID)}, "forkedFrom": nil, "activeProviderThreadId": nil, "createdAt": utcISO(thread.CreatedAt), "updatedAt": utcISO(thread.CreatedAt), "archivedAt": archived, "settledOverride": settledOverride, "settledAt": settledAt, "lastVisitedAt": nil, "deletedAt": nil, "pinnedAt": pinned, "snoozedUntil": utcISONullable(thread.SnoozedUntil)}
}
func runStatus(status api.RunStatus) string {
	if status == api.RunWaiting {
		return "waiting"
	}
	return string(status)
}
func (s *Server) shell(ctx context.Context) (Object, error) {
	snapshot, err := s.service.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return s.shellSnapshot(snapshot), nil
}
func (s *Server) shellSnapshot(snapshot api.Snapshot) Object {
	projects := []any{}
	threads := []any{}
	archived := []any{}
	paths := map[string]time.Time{}
	roots := map[string]string{}
	if s.opts.Workspace != nil {
		path := api.WorkspaceProjectPath(s.opts.Workspace)
		if path != "" {
			identity := s.projectIdentity(api.Thread{Workspace: s.opts.Workspace})
			paths[identity] = time.Unix(0, 0).UTC()
			roots[identity] = path
		}
	}
	for _, thread := range snapshot.Threads {
		path := s.workspace(thread)
		identity := s.projectIdentity(thread)
		roots[identity] = path
		if created, ok := paths[identity]; !ok || created.Equal(time.Unix(0, 0)) {
			paths[identity] = thread.CreatedAt
		}
		row := s.thread(thread)
		row["status"] = "idle"
		row["latestRunId"] = nil
		row["activeRunId"] = nil
		row["pendingRuntimeRequest"] = nil
		row["latestVisibleMessage"] = nil
		row["latestUserMessageAt"] = nil
		row["hasActionableProposedPlan"] = false
		row["itemCount"] = 0
		row["visibleItemCount"] = 0
		var latest *api.Run
		count := 0
		for _, run := range snapshot.Runs {
			if run.ThreadID == thread.ID {
				count++
				if run.Output != "" {
					count++
				}
				if run.Reasoning != "" {
					count++
				}
				if latest == nil || run.CreatedAt.After(latest.CreatedAt) {
					copy := run
					latest = &copy
				}
			}
		}
		for _, tool := range snapshot.Tools {
			if tool.ThreadID == thread.ID {
				count++
			}
		}
		for _, request := range snapshot.Requests {
			if request.ThreadID == thread.ID {
				count++
			}
		}
		for _, input := range snapshot.Inputs {
			if input.ThreadID == thread.ID && (input.Kind == "queue" && input.Status == "queued" || input.Kind == "steer" && input.Status != "cancelled") {
				count++
			}
		}
		row["itemCount"], row["visibleItemCount"] = count, count
		if latest != nil {
			row["status"] = runStatus(latest.Status)
			row["latestRunId"] = latest.ID
			row["updatedAt"] = utcISO(latest.UpdatedAt)
			row["latestUserMessageAt"] = utcISO(latest.CreatedAt)
			if !latest.Status.Terminal() {
				row["activeRunId"] = latest.ID
			}
			role, text, id := "user", latest.Input, latest.ID+"/user"
			if latest.Output != "" {
				role, text, id = "assistant", latest.Output, latest.ID+"/assistant"
			}
			row["latestVisibleMessage"] = Object{"id": id, "role": role, "text": text, "updatedAt": utcISO(latest.UpdatedAt)}
		}
		for _, request := range snapshot.Requests {
			if request.ThreadID == thread.ID && request.Status == "pending" {
				kind := "user_input"
				if request.Method == "confirm" {
					kind = "permission"
				}
				row["pendingRuntimeRequest"] = Object{"id": request.ID, "kind": kind, "createdAt": utcISO(request.CreatedAt)}
			}
		}
		if thread.Archived {
			archived = append(archived, row)
		} else {
			threads = append(threads, row)
		}
	}
	keys := make([]string, 0, len(paths))
	for path := range paths {
		keys = append(keys, path)
	}
	sort.Strings(keys)
	for _, identity := range keys {
		created := paths[identity]
		path := roots[identity]
		createdISO := utcISO(created)
		if identity == scratchProjectIdentity {
			projects = append(projects, Object{
				"id":                    projectID(identity),
				"title":                 "No project",
				"workspaceRoot":         "",
				"isScratch":             true,
				"defaultModelSelection": selection(s.opts.Provider, s.opts.Model),
				"scripts":               []any{},
				"createdAt":             createdISO,
				"updatedAt":             createdISO,
			})
			continue
		}
		projects = append(projects, Object{"id": projectID(identity), "title": nonempty(filepath.Base(path), "Vantry"), "workspaceRoot": path, "defaultModelSelection": selection(s.opts.Provider, s.opts.Model), "scripts": []any{}, "createdAt": createdISO, "updatedAt": createdISO})
	}
	return Object{"schemaVersion": 2, "snapshotSequence": snapshot.Sequence, "projects": projects, "threads": threads, "archivedThreads": archived}
}
func (s *Server) projection(ctx context.Context, id string) (Object, int64, error) {
	snapshot, err := s.service.Snapshot(ctx)
	if err != nil {
		return nil, 0, err
	}
	projection, err := s.project(snapshot, id)
	return projection, snapshot.Sequence, err
}
func itemBase(id, thread, run string, ordinal int, status string, at time.Time) Object {
	atISO := utcISO(at)
	return Object{"id": id, "threadId": thread, "runId": nullable(run), "nodeId": nil, "providerThreadId": nil, "providerTurnId": nil, "nativeItemRef": nil, "parentItemId": nil, "ordinal": ordinal, "status": status, "title": nil, "startedAt": atISO, "completedAt": nil, "updatedAt": atISO}
}
func executionNode(id, thread, run, kind, status string, at time.Time) Object {
	if status == "queued" {
		status = "pending"
	}
	parent := any(nil)
	if id != run {
		parent = run
	}
	completed := any(nil)
	if status == "completed" || status == "failed" || status == "cancelled" || status == "interrupted" {
		completed = utcISO(at)
	}
	return Object{"id": id, "threadId": thread, "runId": run, "parentNodeId": parent, "rootNodeId": run, "kind": kind, "status": status, "countsForRun": id == run, "providerThreadId": nil, "providerTurnId": nil, "nativeItemRef": nil, "runtimeRequestId": nil, "checkpointScopeId": nil, "startedAt": nil, "completedAt": completed}
}
func (s *Server) project(snapshot api.Snapshot, id string) (Object, error) {
	var source *api.Thread
	for _, thread := range snapshot.Threads {
		if thread.ID == id {
			copy := thread
			source = &copy
			break
		}
	}
	if source == nil {
		return nil, &api.Error{Code: api.NotFound, Message: "会话不存在"}
	}
	projection := Object{"thread": s.thread(*source), "updatedAt": utcISO(source.CreatedAt)}
	for _, key := range []string{"runs", "attempts", "nodes", "subagents", "providerSessions", "providerThreads", "providerTurns", "runtimeRequests", "messages", "plans", "turnItems", "checkpointScopes", "checkpoints", "contextHandoffs", "contextTransfers", "visibleTurnItems"} {
		projection[key] = []any{}
	}
	appendRow := func(key string, value any) { projection[key] = append(projection[key].([]any), value) }
	addItem := func(item Object) {
		appendRow("turnItems", item)
		position := len(projection["visibleTurnItems"].([]any))
		appendRow("visibleTurnItems", Object{"position": position, "visibility": "local", "sourceThreadId": id, "sourceItemId": item["id"], "item": item})
	}
	ordinal := 0
	runs := []api.Run{}
	for _, run := range snapshot.Runs {
		if run.ThreadID == id {
			runs = append(runs, run)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt.Before(runs[j].CreatedAt) })
	for index, run := range runs {
		userMessageID := run.ID + "/user"
		for _, input := range snapshot.Inputs {
			if input.ThreadID == id && input.Kind == "queue" && input.RunID == run.ID {
				userMessageID = input.ID + "/user"
				break
			}
		}
		completed := any(nil)
		if run.Status.Terminal() {
			completed = utcISO(run.UpdatedAt)
		}
		appendRow("runs", Object{"id": run.ID, "threadId": id, "ordinal": index + 1, "providerInstanceId": nonempty(run.ProviderID, s.opts.Provider), "modelSelection": selection(nonempty(run.ProviderID, s.opts.Provider), nonempty(run.Model, s.opts.Model)), "providerThreadId": nil, "userMessageId": userMessageID, "rootNodeId": run.ID, "activeAttemptId": nil, "status": runStatus(run.Status), "requestedAt": utcISO(run.CreatedAt), "startedAt": utcISO(run.CreatedAt), "completedAt": completed, "checkpointId": nil, "contextHandoffId": nil})
		appendRow("nodes", executionNode(run.ID, id, run.ID, "root_turn", runStatus(run.Status), run.UpdatedAt))
		for _, role := range []string{"user", "assistant"} {
			text := run.Input
			if role == "assistant" {
				text = run.Output
				if text == "" {
					continue
				}
			}
			messageID := run.ID + "/" + role
			if role == "user" {
				messageID = userMessageID
			}
			// A submitted user message does not change as the assistant streams.
			// Keep its own timestamps stable instead of borrowing run activity.
			updatedAt := run.UpdatedAt
			if role == "user" {
				updatedAt = run.CreatedAt
			}
			streaming := role == "assistant" && !run.Status.Terminal()
			appendRow("messages", Object{"id": messageID, "threadId": id, "runId": run.ID, "nodeId": nil, "role": role, "text": text, "attachments": []any{}, "streaming": streaming, "createdAt": utcISO(run.CreatedAt), "updatedAt": utcISO(updatedAt), "createdBy": "user", "creationSource": "server"})
			status := "completed"
			if streaming {
				status = "running"
			}
			item := itemBase(messageID, id, run.ID, ordinal, status, updatedAt)
			ordinal++
			item["type"] = role + "_message"
			item["messageId"] = messageID
			item["text"] = text
			item["streaming"] = streaming
			if role == "user" {
				item["inputIntent"] = "turn_start"
				item["attachments"] = []any{}
				item["createdBy"] = "user"
				item["creationSource"] = "server"
			}
			if !streaming {
				item["completedAt"] = utcISO(updatedAt)
			}
			addItem(item)
		}
		if run.Reasoning != "" {
			item := itemBase(run.ID+"/reasoning", id, run.ID, ordinal, "completed", run.UpdatedAt)
			ordinal++
			item["type"] = "reasoning"
			item["text"] = run.Reasoning
			item["streaming"] = !run.Status.Terminal()
			addItem(item)
		}
		projection["updatedAt"] = utcISO(run.UpdatedAt)
	}
	for _, input := range snapshot.Inputs {
		if input.ThreadID != id {
			continue
		}
		queued := input.Kind == "queue" && input.Status == "queued"
		steer := input.Kind == "steer" && input.Status != "cancelled"
		if !queued && !steer {
			continue
		}
		runID := input.RunID
		messageID := input.ID + "/steer"
		intent := "steer"
		status := "completed"
		if input.Status == "pending" {
			status = "pending"
		}
		if input.Status == "failed" {
			status = "failed"
		}
		if input.Status == "unknown" {
			status = "waiting"
		}
		if queued {
			runID = "input/" + input.ID
			messageID = input.ID + "/user"
			intent = "queued_turn"
			status = "pending"
			provider, model := source.ProviderID, source.Model
			if input.Configuration != nil {
				provider, model = input.Configuration.ProviderID, input.Configuration.Model
			}
			appendRow("runs", Object{"id": runID, "threadId": id, "ordinal": len(projection["runs"].([]any)) + 1, "providerInstanceId": nonempty(provider, s.opts.Provider), "modelSelection": selection(nonempty(provider, s.opts.Provider), nonempty(model, s.opts.Model)), "providerThreadId": nil, "userMessageId": messageID, "rootNodeId": nil, "activeAttemptId": nil, "status": "queued", "queuePosition": input.Position + 1, "requestedAt": utcISO(input.CreatedAt), "startedAt": nil, "completedAt": nil, "checkpointId": nil, "contextHandoffId": nil})
		}
		appendRow("messages", Object{"id": messageID, "threadId": id, "runId": nullable(runID), "nodeId": nil, "role": "user", "text": input.Message, "attachments": []any{}, "streaming": false, "createdAt": utcISO(input.CreatedAt), "updatedAt": utcISO(input.UpdatedAt), "createdBy": "user", "creationSource": "server"})
		item := itemBase(messageID, id, runID, ordinal, status, input.UpdatedAt)
		ordinal++
		item["type"] = "user_message"
		item["messageId"] = messageID
		item["text"] = input.Message
		item["inputIntent"] = intent
		item["attachments"] = []any{}
		item["createdBy"] = "user"
		item["creationSource"] = "server"
		addItem(item)
	}
	for _, tool := range snapshot.Tools {
		if tool.ThreadID != id {
			continue
		}
		item := itemBase(tool.ID, id, tool.RunID, ordinal, tool.Status, tool.UpdatedAt)
		item["nodeId"] = tool.ID
		appendRow("nodes", executionNode(tool.ID, id, tool.RunID, "tool_call", tool.Status, tool.UpdatedAt))
		ordinal++
		item["type"] = "dynamic_tool"
		item["toolName"] = tool.Name
		item["input"] = tool.Arguments
		item["output"] = tool.Output
		addItem(item)
	}
	for _, request := range snapshot.Requests {
		if request.ThreadID != id {
			continue
		}
		status := request.Status
		if status == "responded" {
			status = "resolved"
		}
		if status == "responding" {
			status = "pending"
		}
		if status == "failed" {
			status = "expired"
		}
		kind := "user_input"
		if request.Method == "confirm" {
			kind = "permission"
		}
		appendRow("runtimeRequests", Object{"id": request.ID, "nodeId": request.ID, "providerTurnId": nil, "nativeRequestRef": nil, "kind": kind, "status": status, "responseCapability": Object{"type": "live", "providerSessionId": request.AttemptID}, "createdAt": utcISO(request.CreatedAt), "resolvedAt": nil})
		itemStatus := "completed"
		if status == "pending" {
			itemStatus = "waiting"
		}
		item := itemBase(request.ID, id, request.RunID, ordinal, itemStatus, request.UpdatedAt)
		ordinal++
		item["requestId"] = request.ID
		item["nodeId"] = request.ID
		nodeKind := "user_input_request"
		if request.Method == "confirm" {
			nodeKind = "approval_request"
		}
		node := executionNode(request.ID, id, request.RunID, nodeKind, itemStatus, request.UpdatedAt)
		node["runtimeRequestId"] = request.ID
		appendRow("nodes", node)
		if request.Method == "confirm" {
			item["type"] = "approval_request"
			item["requestKind"] = "permission"
			item["prompt"] = nonempty(request.Message, request.Title)
			item["reason"] = request.Title
			item["options"] = []any{Object{"decision": "accept", "label": "允许"}, Object{"decision": "decline", "label": "拒绝"}}
		} else {
			item["type"] = "user_input_request"
			options := []any{}
			for _, option := range request.Options {
				options = append(options, Object{"label": option, "description": option})
			}
			item["questions"] = []any{Object{"id": request.ID, "header": request.Title, "question": nonempty(request.Message, request.Title), "options": options, "multiSelect": false, "allowCustomAnswer": request.Method != "select"}}
		}
		addItem(item)
	}
	return projection, nil
}
func (s *Server) projectPath(ctx context.Context, id string) (string, error) {
	snapshot, err := s.service.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	for _, thread := range snapshot.Threads {
		if thread.Workspace != nil && thread.Workspace.EnvironmentID != "" && projectID(s.projectIdentity(thread)) == id {
			return "", errors.New("此项目属于另一执行环境，请连接所属环境后创建会话")
		}
	}
	if s.opts.Workspace != nil && s.opts.Workspace.EnvironmentID != "" && projectID(s.projectIdentity(api.Thread{Workspace: s.opts.Workspace})) == id {
		return "", errors.New("此项目属于另一执行环境，请连接所属环境后创建会话")
	}
	shell := s.shellSnapshot(snapshot)
	if err != nil {
		return "", err
	}
	for _, raw := range shell["projects"].([]any) {
		row := raw.(Object)
		if row["id"] == id {
			return row["workspaceRoot"].(string), nil
		}
	}
	return "", errors.New("找不到所属项目；请刷新后重试")
}
func (s *Server) settings() Object {
	provider := nonempty(s.opts.Provider, "pi")
	return Object{
		"providerInstances": Object{
			provider: Object{
				"driver":      "pi",
				"displayName": "Pi",
				"enabled":     true,
			},
		},
	}
}

func (s *Server) config(ctx context.Context) (Object, error) {
	models := []api.ModelInfo{}
	if catalog, ok := s.service.(api.WorkbenchQueries); ok {
		var err error
		models, err = catalog.ListModels(ctx)
		if err != nil {
			return nil, err
		}
	}
	provider := nonempty(s.opts.Provider, "pi")
	entries := []any{}
	hasDefault := false
	for _, model := range models {
		isDefault := model.Key() == s.opts.Model
		if isDefault {
			hasDefault = true
		}
		entries = append(entries, Object{
			"slug":         model.Key(),
			"name":         nonempty(model.Name, model.ID),
			"isCustom":     false,
			"capabilities": Object{"optionDescriptors": []any{}},
			"isDefault":    isDefault,
		})
	}
	if len(entries) == 0 && s.opts.Model != "" {
		hasDefault = true
		entries = append(entries, Object{
			"slug":         s.opts.Model,
			"name":         s.opts.Model,
			"isCustom":     false,
			"capabilities": Object{"optionDescriptors": []any{}},
			"isDefault":    true,
		})
	}
	if len(entries) > 0 && !hasDefault {
		entries[0].(Object)["isDefault"] = true
	}
	providers := []any{Object{
		"instanceId":                provider,
		"driver":                    "pi",
		"displayName":               "Pi",
		"continuation":              Object{"groupKey": "pi:instance:" + provider},
		"showInteractionModeToggle": false,
		"reportsContextWindow":      true,
		"supportsTextGeneration":    true,
		"supportedRuntimeModes":     []string{"approval-required", "auto-accept-edits", "full-access"},
		"setup":                     Object{"canAuthenticate": false, "canInstall": false},
		"enabled":                   true,
		"installed":                 true,
		"version":                   nonempty(s.opts.Version, "0.1.0"),
		"status":                    "ready",
		"auth":                      Object{"status": "authenticated", "type": "pi"},
		"checkedAt":                 utcISO(time.Now()),
		"models":                    entries,
		"slashCommands":             []any{},
		"skills":                    []any{},
	}}
	cwd := nonempty(api.WorkspaceProjectPath(s.opts.Workspace), s.opts.DataDir)
	return Object{"environment": s.descriptor(), "auth": s.authDescriptor(), "cwd": cwd, "keybindingsConfigPath": filepath.Join(s.opts.DataDir, "keybindings.json"), "keybindings": []any{}, "issues": []any{}, "providers": providers, "availableEditors": []any{}, "settings": s.settings(), "observability": Object{"logsDirectoryPath": s.opts.DataDir, "localTracingEnabled": false, "otlpTracesEnabled": false, "otlpMetricsEnabled": false}, "serverTime": utcISO(time.Now())}, nil
}

func answerText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		parts := []string{}
		for _, item := range value {
			parts = append(parts, answerText(item))
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if answers, ok := value["answers"]; ok {
			return answerText(answers)
		}
	}
	return ""
}
