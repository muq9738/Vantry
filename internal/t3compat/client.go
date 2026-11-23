package t3compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/muq9738/Vantry/pkg/api"
)

type Remote struct {
	URL           string
	Token         string
	EnvironmentID string
	Name          string
}

func PairRemote(ctx context.Context, input string) (Remote, error) {
	u, err := url.Parse(strings.TrimSpace(input))
	if err != nil {
		return Remote{}, errors.New("T3 配对链接无效")
	}
	if strings.HasPrefix(u.Scheme, "t3code") {
		nested := u.Query().Get("pairingUrl")
		u, err = url.Parse(nested)
		if err != nil {
			return Remote{}, errors.New("T3 配对链接无效")
		}
	}
	fragment, _ := url.ParseQuery(u.Fragment)
	credential := nonempty(fragment.Get("token"), u.Query().Get("token"))
	if host := u.Query().Get("host"); host != "" {
		u, err = url.Parse(host)
		if err != nil {
			return Remote{}, errors.New("T3 主机地址无效")
		}
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
	}
	if credential == "" || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return Remote{}, errors.New("请粘贴包含主机地址和一次性凭据的 T3 配对链接")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsPrivate() && !ip.IsLoopback()) {
			return Remote{}, errors.New("公网 T3 环境需要 HTTPS")
		}
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	base := u.String()
	description := Object{}
	if err = remoteHTTP(ctx, "GET", base, "/.well-known/t3/environment", "", nil, &description); err != nil {
		return Remote{}, err
	}
	if number(description["orchestrationProtocolVersion"]) != 2 {
		return Remote{}, errors.New("该 T3 环境未提供会话协议 2")
	}
	form := url.Values{"grant_type": {tokenGrant}, "subject_token": {credential}, "subject_token_type": {bootstrapType}, "requested_token_type": {accessType}, "client_label": {"Vantry desktop"}, "client_os": {runtime.GOOS}, "client_device_type": {"desktop"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := remoteHTTPClient().Do(req)
	if err != nil {
		return Remote{}, errors.New("无法连接 T3 环境")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Remote{}, fmt.Errorf("T3 配对失败（HTTP %d），请确认代码尚未使用或过期", resp.StatusCode)
	}
	var result struct {
		Token string `json:"access_token"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) != nil || result.Token == "" {
		return Remote{}, errors.New("T3 返回的设备授权无效")
	}
	return Remote{URL: base, Token: result.Token, EnvironmentID: text(description, "environmentId"), Name: text(description, "label")}, nil
}
func remoteHTTPClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func remoteHTTP(ctx context.Context, method, base, path, token string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("x-t3-orchestration-protocol", "2")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := remoteHTTPClient().Do(req)
	if err != nil {
		return errors.New("无法连接 T3 环境")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("T3 环境拒绝请求（HTTP %d）", resp.StatusCode)
	}
	if output != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(output)
	}
	return nil
}
func number(value any) int64 {
	switch value := value.(type) {
	case float64:
		return int64(value)
	case int:
		return int64(value)
	case int64:
		return value
	}
	return 0
}
func date(value any) time.Time {
	result, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(value))
	return result
}
func rows(value any) []any { result, _ := value.([]any); return result }

type RemoteAdapter struct{ Remote }

func (a *RemoteAdapter) rpc(ctx context.Context, method string, input Object) (any, error) {
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	if err := remoteHTTP(ctx, "POST", a.URL, "/api/auth/websocket-ticket/rpc", a.Token, nil, &ticket); err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, EnableCompression: true}
	endpoint := strings.Replace(a.URL, "http", "ws", 1) + "/ws?wsTicket=" + url.QueryEscape(ticket.Ticket) + "&orchestrationProtocol=2"
	conn, resp, err := dialer.DialContext(ctx, endpoint, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, errors.New("T3 会话通道连接失败")
	}
	defer conn.Close()
	conn.SetReadLimit(64 << 20)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err = conn.WriteJSON(Object{"_tag": "Request", "id": "0", "tag": method, "payload": input, "headers": []any{}}); err != nil {
		return nil, err
	}
	for {
		_, data, readErr := conn.ReadMessage()
		if readErr != nil {
			return nil, errors.New("T3 会话通道已断开")
		}
		var frames []json.RawMessage
		if len(data) > 0 && data[0] == '[' {
			if json.Unmarshal(data, &frames) != nil {
				return nil, errors.New("T3 会话数据无效")
			}
		} else {
			frames = []json.RawMessage{data}
		}
		for _, frame := range frames {
			var response Object
			if json.Unmarshal(frame, &response) != nil {
				return nil, errors.New("T3 会话数据无效")
			}
			if response["_tag"] == "Ping" {
				_ = conn.WriteJSON(Object{"_tag": "Pong"})
				continue
			}
			if response["_tag"] == "Exit" {
				exit := object(response["exit"])
				if exit["_tag"] == "Success" {
					return exit["value"], nil
				}
				for _, cause := range rows(exit["cause"]) {
					entry := object(cause)
					if message := text(object(entry["error"]), "message"); message != "" {
						return nil, errors.New(message)
					}
					if message := text(object(entry["defect"]), "message"); message != "" {
						return nil, errors.New(message)
					}
				}
				return nil, errors.New("T3 未能完成此操作")
			}
		}
	}
}
func (a *RemoteAdapter) shell(ctx context.Context) (Object, error) {
	var result Object
	err := remoteHTTP(ctx, "GET", a.URL, "/api/orchestration/shell", a.Token, nil, &result)
	return result, err
}
func (a *RemoteAdapter) detail(ctx context.Context, id string) (Object, int64, error) {
	var result Object
	err := remoteHTTP(ctx, "GET", a.URL, "/api/orchestration/threads/"+url.PathEscape(id), a.Token, nil, &result)
	return object(result["projection"]), number(result["snapshotSequence"]), err
}
func remoteThread(row Object, projects map[string]string) api.Thread {
	model := object(row["modelSelection"])
	policy, _ := policy(text(row, "runtimeMode"))
	thread := api.Thread{ID: text(row, "id"), Title: text(row, "title"), ProviderID: text(row, "providerInstanceId"), Model: text(model, "instanceId") + "/" + text(model, "model"), CreatedAt: date(row["createdAt"]), ApprovalPolicy: policy, InteractionMode: text(row, "interactionMode"), Archived: row["archivedAt"] != nil, Pinned: row["pinnedAt"] != nil, ParentThreadID: text(object(row["lineage"]), "parentThreadId")}
	if path := projects[text(row, "projectId")]; path != "" {
		thread.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: path}
	}
	if path := text(row, "worktreePath"); path != "" {
		thread.Workspace = &api.WorkspaceSpec{Kind: "worktree", Path: path, Repository: projects[text(row, "projectId")], Branch: text(row, "branch")}
	}
	return thread
}
func (a *RemoteAdapter) snapshot(ctx context.Context) (api.Snapshot, error) {
	shell, err := a.shell(ctx)
	if err != nil {
		return api.Snapshot{}, err
	}
	snapshot := api.Snapshot{Sequence: number(shell["snapshotSequence"]), Threads: []api.Thread{}, Runs: []api.Run{}, Tasks: []api.Task{}, Cohorts: []api.Cohort{}}
	projects := map[string]string{}
	for _, raw := range rows(shell["projects"]) {
		project := object(raw)
		projects[text(project, "id")] = text(project, "workspaceRoot")
	}
	for _, raw := range append(rows(shell["threads"]), rows(shell["archivedThreads"])...) {
		thread := remoteThread(object(raw), projects)
		snapshot.Threads = append(snapshot.Threads, thread)
		projection, sequence, err := a.detail(ctx, thread.ID)
		if err != nil {
			return api.Snapshot{}, err
		}
		_ = sequence // Keep the shell cursor: concurrent changes must trigger a resync.
		snapshot = appendProjection(snapshot, thread, projection)
	}
	return snapshot, nil
}
func appendProjection(snapshot api.Snapshot, thread api.Thread, projection Object) api.Snapshot {
	for _, raw := range rows(projection["runs"]) {
		row := object(raw)
		status := api.RunStatus(text(row, "status"))
		if status == "waiting" {
			status = api.RunWaiting
		}
		run := api.Run{ID: text(row, "id"), ThreadID: thread.ID, AttemptID: text(row, "activeAttemptId"), Status: status, ProviderID: text(row, "providerInstanceId"), Model: text(object(row["modelSelection"]), "model"), CreatedAt: date(row["requestedAt"]), UpdatedAt: date(row["completedAt"]), Workspace: thread.Workspace}
		if run.UpdatedAt.IsZero() {
			run.UpdatedAt = date(projection["updatedAt"])
		}
		for _, rawMessage := range rows(projection["messages"]) {
			message := object(rawMessage)
			if text(message, "runId") != run.ID {
				continue
			}
			if text(message, "role") == "user" {
				run.Input = text(message, "text")
			} else if text(message, "role") == "assistant" {
				run.Output += text(message, "text")
			}
		}
		snapshot.Runs = append(snapshot.Runs, run)
	}
	for _, raw := range rows(projection["runtimeRequests"]) {
		row := object(raw)
		if text(row, "status") != "pending" {
			continue
		}
		request := api.ProviderRequest{ID: text(row, "id"), ThreadID: thread.ID, Status: "pending", Method: "input", CreatedAt: date(row["createdAt"])}
		for _, rawItem := range rows(projection["turnItems"]) {
			item := object(rawItem)
			if text(item, "requestId") != request.ID {
				continue
			}
			request.RunID = text(item, "runId")
			request.Title = nonempty(text(item, "reason"), "T3 请求")
			request.Message = text(item, "prompt")
			if text(item, "type") == "approval_request" {
				request.Method = "confirm"
			} else if questions := rows(item["questions"]); len(questions) > 0 {
				question := object(questions[0])
				request.Message = text(question, "question")
				for _, option := range rows(question["options"]) {
					request.Options = append(request.Options, text(object(option), "label"))
				}
				if len(request.Options) > 0 {
					request.Method = "select"
				}
				if len(questions) > 1 {
					request.Method = "editor"
					request.Options = nil
					answers := Object{}
					questionTexts := []string{}
					for _, rawQuestion := range questions {
						question := object(rawQuestion)
						answers[text(question, "id")] = Object{"answers": []string{}}
						questionTexts = append(questionTexts, text(question, "question"))
					}
					data, _ := json.MarshalIndent(answers, "", "  ")
					request.Prefill = string(data)
					request.Message = strings.Join(questionTexts, "\n")
				}
			}
		}
		snapshot.Requests = append(snapshot.Requests, request)
	}
	return snapshot
}
func (a *RemoteAdapter) dispatch(ctx context.Context, command api.Command) (api.Receipt, error) {
	if command.CommandID == "" {
		return api.Receipt{}, errors.New("commandId 不能为空")
	}
	wire := Object{"commandId": command.CommandID, "threadId": command.ThreadID}
	if len(command.Attachments) > 0 {
		return api.Receipt{}, errors.New("T3 适配连接尚不支持附件发送")
	}
	switch command.Kind {
	case api.CreateThread:
		shell, err := a.shell(ctx)
		if err != nil {
			return api.Receipt{}, err
		}
		project := ""
		for _, raw := range rows(shell["projects"]) {
			row := object(raw)
			if command.Workspace == nil || text(row, "workspaceRoot") == api.WorkspaceProjectPath(command.Workspace) {
				project = text(row, "id")
				break
			}
		}
		if project == "" {
			return api.Receipt{}, errors.New("请先在 T3 环境创建或选择所属项目")
		}
		if command.ThreadID == "" {
			command.ThreadID = "vantry_" + command.CommandID
		}
		wire["threadId"] = command.ThreadID
		wire["type"] = "thread.create"
		wire["projectId"] = project
		wire["title"] = nonempty(command.Title, "Vantry 会话")
		wire["runtimeMode"] = runtimeMode(command.ApprovalPolicy)
		wire["interactionMode"] = nonempty(command.InteractionMode, "default")
		wire["branch"] = nil
		wire["worktreePath"] = nil
		wire["createdBy"] = "user"
		wire["creationSource"] = "web"
		config, err := a.rpc(ctx, "server.getConfig", Object{})
		if err != nil {
			return api.Receipt{}, err
		}
		providers := rows(object(config)["providers"])
		if len(providers) == 0 {
			return api.Receipt{}, errors.New("T3 未提供可用模型执行器")
		}
		provider := object(providers[0])
		model := command.Model
		if model == "" {
			models := rows(provider["models"])
			if len(models) == 0 {
				return api.Receipt{}, errors.New("T3 没有可用模型")
			}
			model = text(object(models[0]), "slug")
		}
		instance := nonempty(command.ProviderID, text(provider, "instanceId"))
		model = strings.TrimPrefix(model, instance+"/")
		wire["modelSelection"] = selection(instance, model)
	case api.SendMessage, api.QueueMessage, api.SteerRun:
		wire["type"] = "message.dispatch"
		wire["text"] = command.Message
		wire["messageId"] = nonempty(command.MessageID, command.CommandID+"/message")
		wire["attachments"] = []any{}
		wire["createdBy"] = "user"
		wire["creationSource"] = "web"
		mode := "start_immediately"
		if command.Kind == api.QueueMessage {
			mode = "queue_after_active"
		}
		dispatch := Object{"type": mode}
		if command.Kind == api.SteerRun {
			dispatch = Object{"type": "steer_active", "targetRunId": command.RunID}
		}
		wire["dispatchMode"] = dispatch
	case api.StopRun:
		wire["type"] = "run.interrupt"
		wire["runId"] = command.RunID
	case api.SetThreadModel:
		wire["type"] = "thread.model-selection.set"
		wire["modelSelection"] = selection(command.ProviderID, strings.TrimPrefix(command.Model, command.ProviderID+"/"))
	case api.RespondRequest:
		wire["type"] = "runtime-request.respond"
		wire["requestId"] = command.RequestID
		answer, err := a.response(ctx, command)
		if err != nil {
			return api.Receipt{}, err
		}
		for key, value := range answer {
			wire[key] = value
		}
	case api.UpdateThread:
		if command.Pinned != nil {
			wire["type"] = "thread.unpin"
			if *command.Pinned {
				wire["type"] = "thread.pin"
			}
		} else if command.Archived != nil {
			wire["type"] = "thread.unarchive"
			if *command.Archived {
				wire["type"] = "thread.archive"
			}
		} else if command.Title != "" {
			wire["type"] = "thread.metadata.update"
			wire["title"] = command.Title
		} else {
			return api.Receipt{}, errors.New("此 T3 会话更新尚未支持")
		}
	case api.DeleteThread:
		wire["type"] = "thread.delete"
	default:
		return api.Receipt{}, errors.New("此会话操作暂不支持 T3 环境")
	}
	result, err := a.rpc(ctx, "orchestration.dispatchCommand", wire)
	if err != nil {
		return api.Receipt{}, err
	}
	return api.Receipt{CommandID: command.CommandID, Accepted: true, ThreadID: command.ThreadID, RunID: command.RunID, Sequence: number(object(result)["sequence"])}, nil
}
func adapterResponse(req *http.Request, value any, err error) *http.Response {
	status := 200
	if err != nil {
		status = 400
		value = &api.Error{Code: api.InvalidCommand, Message: err.Error()}
	}
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(data)), Request: req}
}
func (a *RemoteAdapter) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	var value any
	var err error
	switch req.URL.Path {
	case "/v1/health":
		var descriptor Object
		err = remoteHTTP(ctx, "GET", a.URL, "/.well-known/t3/environment", "", nil, &descriptor)
		if err == nil && text(descriptor, "environmentId") != a.EnvironmentID {
			err = errors.New("T3 环境身份与配对记录不同，请重新配对")
		}
		value = api.Health{Version: 1, State: "ready"}
	case "/v1/status":
		value = api.ServerStatus{ProtocolVersion: 1, State: "ready", SchemaVersion: api.StatusSchemaVersion, Capabilities: []string{api.CapabilityStatus, api.CapabilityThreadSummaries, api.CapabilityThreadDetail}}
	case "/v1/snapshot":
		value, err = a.snapshot(ctx)
	case "/v1/threads/summaries":
		var snapshot api.Snapshot
		snapshot, err = a.snapshot(ctx)
		if err == nil {
			value = api.BuildThreadSummaries(snapshot)
		}
	case "/v1/threads/detail":
		var snapshot api.Snapshot
		snapshot, err = a.snapshot(ctx)
		if err == nil {
			limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
			value, err = api.BuildThreadDetail(snapshot, api.ThreadDetailQuery{ThreadID: req.URL.Query().Get("thread"), LimitRuns: limit})
		}
	case "/v1/commands":
		var command api.Command
		err = json.NewDecoder(req.Body).Decode(&command)
		if err == nil {
			value, err = a.dispatch(ctx, command)
		}
	case "/v1/events":
		return a.events(req), nil
	case "/v1/models":
		value, err = a.models(ctx)
	case "/v1/workbench":
		var request api.WorkbenchRequest
		err = json.NewDecoder(req.Body).Decode(&request)
		if err == nil {
			value, err = a.workbench(ctx, request)
		}
	default:
		err = errors.New("此功能未通过 T3 会话适配提供")
	}
	return adapterResponse(req, value, err), nil
}
func (a *RemoteAdapter) models(ctx context.Context) ([]api.ModelInfo, error) {
	config, err := a.rpc(ctx, "server.getConfig", Object{})
	if err != nil {
		return nil, err
	}
	models := []api.ModelInfo{}
	for _, raw := range rows(object(config)["providers"]) {
		provider := object(raw)
		for _, rawModel := range rows(provider["models"]) {
			model := object(rawModel)
			models = append(models, api.ModelInfo{ID: text(model, "slug"), Provider: text(provider, "instanceId"), Name: text(model, "name")})
		}
	}
	return models, nil
}
func (a *RemoteAdapter) workbench(ctx context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	switch request.Action {
	case "models":
		models, err := a.models(ctx)
		return api.WorkbenchResponse{Catalog: &api.ModelCatalog{Models: models}}, err
	case "plugins.list":
		return api.WorkbenchResponse{Plugins: []api.PluginInfo{}}, nil
	case "environment.info":
		data, _ := json.Marshal(api.EnvironmentInfo{ID: a.EnvironmentID, Name: a.Name})
		return api.WorkbenchResponse{Output: string(data)}, nil
	default:
		return api.WorkbenchResponse{}, errors.New("此工作台功能未通过 T3 会话适配提供")
	}
}
func (a *RemoteAdapter) events(req *http.Request) *http.Response {
	reader, writer := io.Pipe()
	after, _ := strconv.ParseInt(req.URL.Query().Get("after"), 10, 64)
	go func() {
		defer writer.Close()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				shell, err := a.shell(req.Context())
				if err != nil {
					_ = json.NewEncoder(writer).Encode(Object{"error": &api.Error{Code: api.ResyncRequired, Message: "T3 连接中断，请重新同步"}})
					return
				}
				if number(shell["snapshotSequence"]) != after {
					_ = json.NewEncoder(writer).Encode(Object{"error": &api.Error{Code: api.ResyncRequired, Message: "T3 会话已更新，请刷新投影"}})
					return
				}
			case <-req.Context().Done():
				return
			}
		}
	}()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/x-ndjson"}}, Body: reader, Request: req}
}
