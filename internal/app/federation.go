package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/muq9738/Vantry/internal/environments"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

// dialEnvironment uses the same saved pairing as the IDE machine switcher.
// SSH catalog entries remain supported, without inventing a second credential
// store or letting an agent supply arbitrary network addresses.
func (s *workbenchService) dialEnvironment(ctx context.Context, target environments.PeerTarget) (environments.PeerClient, error) {
	if target.ConnectionID != "" {
		return remoteaccess.Connect(ctx, remoteaccess.Options{DataDir: s.config.DataDir}, target.ConnectionID)
	}
	return environments.DefaultDial(ctx, target)
}

func (s *workbenchService) resolveEnvironmentTarget(ctx context.Context, id string) (environments.PeerTarget, error) {
	if s.federator == nil {
		return environments.PeerTarget{}, &api.Error{Code: api.NotReady, Message: "当前 Core 未启用跨环境编排"}
	}
	manager, err := s.accessManager()
	if err != nil {
		return environments.PeerTarget{}, err
	}
	peers := manager.Status().Peers
	// Connection IDs are stable, secret-free handles already used by the IDE.
	for _, peer := range peers {
		if id == peer.ID || id == peer.DeviceID {
			return environments.PeerTarget{EnvironmentID: id, ConnectionID: peer.ID, Name: peer.Name}, nil
		}
	}
	catalog, err := environments.New(s.config.DataDir)
	if err != nil {
		return environments.PeerTarget{}, err
	}
	entries, err := catalog.Load()
	if err != nil {
		return environments.PeerTarget{}, err
	}
	for _, entry := range entries {
		if entry.ID == id {
			if strings.HasPrefix(entry.Socket(), "~") {
				return environments.PeerTarget{}, &api.Error{Code: api.InvalidCommand, Message: "SSH 编排目标需配置远端 Core socket 的绝对路径"}
			}
			return environments.PeerTarget{EnvironmentID: entry.ID, Name: entry.Name, Host: entry.Host, SocketPath: entry.Socket()}, nil
		}
	}
	// A caller may use the actual Core environment ID learned when opening its
	// IDE session. Resolve it against saved pairings, never against local paths.
	for _, peer := range peers {
		target := environments.PeerTarget{EnvironmentID: peer.ID, ConnectionID: peer.ID, Name: peer.Name}
		client, connectErr := s.federator.Client(ctx, target)
		if connectErr != nil {
			continue
		}
		info, infoErr := peerEnvironmentInfo(ctx, client)
		if infoErr == nil && info.ID == id {
			target.EnvironmentID = id
			return target, nil
		}
	}
	return environments.PeerTarget{}, &api.Error{Code: api.NotFound, Message: "目标执行环境未连接；请先在机器连接中添加配对，或配置已有 SSH 环境"}
}

func peerEnvironmentInfo(ctx context.Context, client environments.PeerClient) (api.EnvironmentInfo, error) {
	workbench, ok := client.(api.WorkbenchService)
	if !ok {
		return api.EnvironmentInfo{}, &api.Error{Code: api.NotFound, Message: "目标 Core 不提供执行环境信息"}
	}
	response, err := workbench.Workbench(ctx, api.WorkbenchRequest{Action: "environment.info"})
	if err != nil {
		return api.EnvironmentInfo{}, err
	}
	var info api.EnvironmentInfo
	if json.Unmarshal([]byte(response.Output), &info) != nil || info.ID == "" {
		return info, errors.New("目标 Core 返回的执行环境信息无效")
	}
	return info, nil
}

func (s *workbenchService) remoteBroker(ctx context.Context, id string) (api.OrchestrationBroker, environments.PeerClient, error) {
	target, err := s.resolveEnvironmentTarget(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	client, err := s.federator.Client(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	status, ok := client.(api.StatusReporter)
	if !ok {
		return nil, nil, &api.Error{Code: api.NotFound, Message: "目标 Core 没有能力报告"}
	}
	report, err := status.ServerStatus(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !report.HasCapability(api.CapabilityInterThreadOrchestration) {
		return nil, nil, &api.Error{Code: api.NotReady, Message: "目标 Core 未提供跨会话编排，请更新目标服务"}
	}
	broker, ok := client.(api.OrchestrationBroker)
	if !ok {
		return nil, nil, &api.Error{Code: api.NotFound, Message: "目标连接不支持会话编排"}
	}
	return broker, client, nil
}

func (s *workbenchService) callerThread(ctx context.Context, id string) (*api.Thread, error) {
	if id == "" {
		return nil, nil
	}
	reader, ok := unwrapService[api.ThreadReader](s.Service)
	if !ok {
		return nil, &api.Error{Code: api.NotReady, Message: "当前 Core 不提供会话详情"}
	}
	detail, err := reader.ThreadDetail(ctx, api.ThreadDetailQuery{ThreadID: id, LimitRuns: 1})
	if err != nil {
		return nil, err
	}
	return &detail.Thread, nil
}

func remotePermissionCheck(caller *api.Thread, approval, mode string) error {
	if caller == nil {
		return nil
	}
	rank := func(policy string) int {
		switch policy {
		case "ask", "approval-required":
			return 0
		case "auto-accept-edits":
			return 1
		default:
			return 2
		}
	}
	if rank(approval) > rank(caller.ApprovalPolicy) || caller.InteractionMode == "plan" && mode != "plan" {
		return &api.Error{Code: api.Conflict, Message: "远端会话的权限或交互模式高于发起会话；请使用受限的远端会话"}
	}
	return nil
}

func (s *workbenchService) prepareRemoteDelegation(ctx context.Context, req api.DelegateTaskRequest) (api.DelegateTaskRequest, error) {
	_, client, err := s.remoteBroker(ctx, req.EnvironmentID)
	if err != nil {
		return req, err
	}
	info, err := peerEnvironmentInfo(ctx, client)
	if err != nil {
		return req, err
	}
	caller, err := s.callerThread(ctx, req.CallerThreadID)
	if err != nil {
		return req, err
	}
	policy, mode := req.ApprovalPolicy, req.InteractionMode
	if caller != nil {
		policy, mode = firstRemote(policy, caller.ApprovalPolicy), firstRemote(mode, caller.InteractionMode)
	}
	if err := remotePermissionCheck(caller, policy, mode); err != nil {
		return req, err
	}
	providerID, model := firstRemote(req.ProviderID, info.ProviderID), firstRemote(req.Model, info.DefaultModel)
	if req.Model == "" && caller != nil && caller.Model != "" && caller.Model != model {
		return req, &api.Error{Code: api.Conflict, Message: "目标环境的默认模型与父会话不同；请查看目标能力并显式选择 model，不能隐式切换模型"}
	}
	if model == "" && providerID != "controlled" {
		return req, &api.Error{Code: api.NotReady, Message: "目标环境未配置默认模型，请显式选择目标模型"}
	}
	workspace := req.Workspace
	if workspace == nil {
		workspace = info.Workspace
	}
	if workspace == nil {
		return req, &api.Error{Code: api.InvalidCommand, Message: "远端任务需要目标环境的工作区，请显式指定目标路径"}
	}
	copy := *workspace
	if copy.EnvironmentID != "" && copy.EnvironmentID != req.EnvironmentID {
		return req, &api.Error{Code: api.Conflict, Message: "工作区不属于目标环境"}
	}
	copy.EnvironmentID = req.EnvironmentID
	if copy.Kind != "directory" && copy.Kind != "worktree" {
		return req, &api.Error{Code: api.InvalidCommand, Message: "远端任务仅支持 directory 或 worktree 工作区"}
	}
	if copy.Path != "" && !filepath.IsAbs(copy.Path) || copy.Repository != "" && !filepath.IsAbs(copy.Repository) {
		return req, &api.Error{Code: api.InvalidCommand, Message: "远端工作区必须是目标机器上的绝对路径"}
	}
	if req.Isolation == "worktree" {
		copy.Kind, copy.Repository, copy.Path, copy.Branch = "worktree", firstRemote(copy.Repository, copy.Path), "", ""
	}
	req.ResolvedTarget = &api.ResolvedDelegateTarget{ProviderID: providerID, Model: model, Workspace: &copy}
	return req, nil
}

func (s *workbenchService) remoteRequestID(callerID, requestID string) (string, error) {
	if requestID == "" {
		return "", nil
	}
	info, err := s.environmentInfo()
	if err != nil {
		return "", err
	}
	return "remote:" + info.ID + ":" + callerID + ":" + requestID, nil
}

func (s *workbenchService) launchRemoteThread(ctx context.Context, req api.ThreadLaunchRequest) (api.LaunchedThread, error) {
	broker, _, err := s.remoteBroker(ctx, req.EnvironmentID)
	if err != nil {
		return api.LaunchedThread{}, err
	}
	caller, err := s.callerThread(ctx, req.CallerThreadID)
	if err != nil {
		return api.LaunchedThread{}, err
	}
	if caller != nil {
		if req.ApprovalPolicy == "" {
			req.ApprovalPolicy = caller.ApprovalPolicy
		}
		if req.InteractionMode == "" {
			req.InteractionMode = caller.InteractionMode
		}
	}
	if err := remotePermissionCheck(caller, req.ApprovalPolicy, req.InteractionMode); err != nil {
		return api.LaunchedThread{}, err
	}
	if req.Workspace != nil && req.Workspace.EnvironmentID != "" {
		return api.LaunchedThread{}, &api.Error{Code: api.InvalidCommand, Message: "远端会话工作区不能再指定其他执行环境"}
	}
	req.ClientRequestID, err = s.remoteRequestID(req.CallerThreadID, req.ClientRequestID)
	if err != nil {
		return api.LaunchedThread{}, err
	}
	environmentID := req.EnvironmentID
	req.EnvironmentID, req.CallerThreadID, req.CallerRunID = "", "", ""
	result, err := broker.LaunchThread(ctx, req)
	result.EnvironmentID = environmentID
	result.Link = api.FormatEnvironmentThreadLink(environmentID, result.Link)
	return result, err
}

func firstRemote(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (s *workbenchService) decorateRemoteTask(detail api.TaskDetail) api.TaskDetail {
	if s.federator == nil {
		return detail
	}
	if handle, ok := s.federator.GetTask(detail.ChildAttemptID); ok {
		detail.EnvironmentID, detail.RemoteThreadID, detail.RemoteRunID = handle.Target.EnvironmentID, handle.RemoteThreadID, handle.RemoteRunID
	}
	return detail
}

func (s *workbenchService) orchestrationEnvironments(ctx context.Context) (api.WorkbenchResponse, error) {
	type entry struct {
		ID        string `json:"environmentId"`
		Name      string `json:"name"`
		Transport string `json:"transport"`
	}
	out := []entry{}
	manager, err := s.accessManager()
	if err != nil {
		return api.WorkbenchResponse{}, err
	}
	for _, peer := range manager.Status().Peers {
		out = append(out, entry{ID: peer.ID, Name: peer.Name, Transport: "paired"})
	}
	catalog, err := environments.New(s.config.DataDir)
	if err != nil {
		return api.WorkbenchResponse{}, err
	}
	entries, err := catalog.Load()
	if err != nil {
		return api.WorkbenchResponse{}, err
	}
	for _, peer := range entries {
		out = append(out, entry{ID: peer.ID, Name: peer.Name, Transport: "ssh"})
	}
	return environmentOutput(map[string]any{"environments": out})
}

func (s *workbenchService) scopedRemoteThread(ctx context.Context, environmentID, callerID, threadID string) (api.OrchestrationBroker, api.ThreadReadResult, error) {
	broker, _, err := s.remoteBroker(ctx, environmentID)
	if err != nil {
		return nil, api.ThreadReadResult{}, err
	}
	caller, err := s.callerThread(ctx, callerID)
	if err != nil {
		return nil, api.ThreadReadResult{}, err
	}
	thread, err := broker.ReadThread(ctx, api.ThreadReadQuery{ThreadID: threadID, Limit: 1, RunLimit: 1, MaxCharsPerItem: 1})
	if err != nil {
		return nil, thread, err
	}
	if err := remotePermissionCheck(caller, thread.Thread.ApprovalPolicy, thread.Thread.InteractionMode); err != nil {
		return nil, thread, err
	}
	return broker, thread, nil
}
