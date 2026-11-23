package app

import (
	"context"
	"fmt"

	"github.com/muq9738/Vantry/internal/adapters/workbench"
	"github.com/muq9738/Vantry/pkg/api"
)

func unwrapService[T any](service api.Service) (T, bool) {
	var zero T
	for service != nil {
		if target, ok := any(service).(T); ok {
			return target, true
		}
		wrapped, ok := service.(*workbench.Service)
		if !ok {
			break
		}
		service = wrapped.Service
	}
	return zero, false
}

func (s *workbenchService) broker() (api.OrchestrationBroker, error) {
	if broker, ok := unwrapService[api.OrchestrationBroker](s.Service); ok {
		return broker, nil
	}
	return nil, &api.Error{Code: api.NotFound, Message: "当前 Core 未提供跨会话编排接口"}
}

func (s *workbenchService) OrchestratorCapabilities(ctx context.Context, query api.OrchestratorCapabilitiesQuery) (api.OrchestratorCapabilities, error) {
	if query.EnvironmentID != "" {
		broker, _, err := s.remoteBroker(ctx, query.EnvironmentID)
		if err != nil {
			return api.OrchestratorCapabilities{}, err
		}
		query.EnvironmentID, query.CallerThreadID = "", ""
		return broker.OrchestratorCapabilities(ctx, query)
	}
	broker, err := s.broker()
	if err != nil {
		return api.OrchestratorCapabilities{}, err
	}
	return broker.OrchestratorCapabilities(ctx, query)
}

func (s *workbenchService) DelegateTask(ctx context.Context, req api.DelegateTaskRequest) (api.TaskDetail, error) {
	broker, err := s.broker()
	if err != nil {
		return api.TaskDetail{}, err
	}
	if req.EnvironmentID != "" {
		req, err = s.prepareRemoteDelegation(ctx, req)
		if err != nil {
			return api.TaskDetail{}, err
		}
	}
	detail, err := broker.DelegateTask(ctx, req)
	if req.EnvironmentID != "" {
		detail.EnvironmentID = req.EnvironmentID
	}
	return s.decorateRemoteTask(detail), err
}

func (s *workbenchService) TaskStatus(ctx context.Context, query api.TaskStatusQuery) (api.TaskStatusResult, error) {
	broker, err := s.broker()
	if err != nil {
		return api.TaskStatusResult{}, err
	}
	result, err := broker.TaskStatus(ctx, query)
	for i := range result.Tasks {
		result.Tasks[i] = s.decorateRemoteTask(result.Tasks[i])
	}
	return result, err
}

func (s *workbenchService) CancelTask(ctx context.Context, req api.CancelTaskRequest) (api.CancelTaskResult, error) {
	broker, err := s.broker()
	if err != nil {
		return api.CancelTaskResult{}, err
	}
	return broker.CancelTask(ctx, req)
}

func (s *workbenchService) ListThreads(ctx context.Context, query api.ThreadListQuery) (api.ThreadListResult, error) {
	if query.EnvironmentID != "" {
		broker, _, err := s.remoteBroker(ctx, query.EnvironmentID)
		if err != nil {
			return api.ThreadListResult{}, err
		}
		environmentID := query.EnvironmentID
		query.EnvironmentID, query.CallerThreadID = "", ""
		result, err := broker.ListThreads(ctx, query)
		for i := range result.Threads {
			result.Threads[i].EnvironmentID = environmentID
			result.Threads[i].Link = api.FormatEnvironmentThreadLink(environmentID, result.Threads[i].Link)
		}
		return result, err
	}
	broker, err := s.broker()
	if err != nil {
		return api.ThreadListResult{}, err
	}
	return broker.ListThreads(ctx, query)
}

func (s *workbenchService) ReadThread(ctx context.Context, query api.ThreadReadQuery) (api.ThreadReadResult, error) {
	if query.EnvironmentID != "" {
		broker, _, err := s.remoteBroker(ctx, query.EnvironmentID)
		if err != nil {
			return api.ThreadReadResult{}, err
		}
		environmentID := query.EnvironmentID
		query.EnvironmentID, query.CallerThreadID = "", ""
		result, err := broker.ReadThread(ctx, query)
		result.Thread.EnvironmentID = environmentID
		result.Thread.Link = api.FormatEnvironmentThreadLink(environmentID, result.Thread.Link)
		return result, err
	}
	broker, err := s.broker()
	if err != nil {
		return api.ThreadReadResult{}, err
	}
	return broker.ReadThread(ctx, query)
}

func (s *workbenchService) LaunchThread(ctx context.Context, req api.ThreadLaunchRequest) (api.LaunchedThread, error) {
	if req.EnvironmentID != "" {
		return s.launchRemoteThread(ctx, req)
	}
	broker, err := s.broker()
	if err != nil {
		return api.LaunchedThread{}, err
	}
	return broker.LaunchThread(ctx, req)
}

func (s *workbenchService) CreateThreads(ctx context.Context, req api.CreateThreadsRequest) (api.CreateThreadsResult, error) {
	broker, err := s.broker()
	if err != nil {
		return api.CreateThreadsResult{}, err
	}
	if len(req.Threads) == 0 || len(req.Threads) > 20 {
		return api.CreateThreadsResult{}, &api.Error{Code: api.InvalidCommand, Message: "create_threads requires between 1 and 20 threads"}
	}
	for _, spec := range req.Threads {
		if spec.EnvironmentID != "" {
			result := api.CreateThreadsResult{Threads: []api.LaunchedThread{}}
			for i, item := range req.Threads {
				requestID := ""
				if req.ClientRequestID != "" {
					requestID = fmt.Sprintf("%s:thread:%d", req.ClientRequestID, i)
				}
				thread, err := s.LaunchThread(ctx, api.ThreadLaunchRequest{ClientRequestID: requestID, CallerThreadID: req.CallerThreadID, CallerRunID: req.CallerRunID, ThreadLaunchSpec: item})
				if err != nil {
					return result, err
				}
				result.Threads = append(result.Threads, thread)
			}
			return result, nil
		}
	}
	return broker.CreateThreads(ctx, req)
}

func (s *workbenchService) SendToThread(ctx context.Context, req api.InterThreadSendRequest) (api.InterThreadSendResult, error) {
	if req.EnvironmentID != "" {
		broker, _, err := s.scopedRemoteThread(ctx, req.EnvironmentID, req.CallerThreadID, req.TargetThreadID)
		if err != nil {
			return api.InterThreadSendResult{}, err
		}
		environmentID := req.EnvironmentID
		req.ClientRequestID, err = s.remoteRequestID(req.CallerThreadID, req.ClientRequestID)
		if err != nil {
			return api.InterThreadSendResult{}, err
		}
		req.EnvironmentID, req.CallerThreadID = "", ""
		result, err := broker.SendToThread(ctx, req)
		result.EnvironmentID = environmentID
		result.Link = api.FormatEnvironmentThreadLink(environmentID, result.Link)
		return result, err
	}
	broker, err := s.broker()
	if err != nil {
		return api.InterThreadSendResult{}, err
	}
	return broker.SendToThread(ctx, req)
}

func (s *workbenchService) WaitThread(ctx context.Context, query api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
	if query.EnvironmentID != "" {
		broker, _, err := s.remoteBroker(ctx, query.EnvironmentID)
		if err != nil {
			return api.ThreadWaitResult{}, err
		}
		environmentID := query.EnvironmentID
		query.EnvironmentID, query.CallerThreadID = "", ""
		result, err := broker.WaitThread(ctx, query)
		result.EnvironmentID = environmentID
		result.Link = api.FormatEnvironmentThreadLink(environmentID, result.Link)
		return result, err
	}
	broker, err := s.broker()
	if err != nil {
		return api.ThreadWaitResult{}, err
	}
	return broker.WaitThread(ctx, query)
}

func (s *workbenchService) ThreadSummaries(ctx context.Context) (api.ThreadSummaries, error) {
	if reader, ok := unwrapService[api.ThreadReader](s.Service); ok {
		return reader.ThreadSummaries(ctx)
	}
	snapshot, err := s.Service.Snapshot(ctx)
	if err != nil {
		return api.ThreadSummaries{}, err
	}
	return api.BuildThreadSummaries(snapshot), nil
}

func (s *workbenchService) ThreadDetail(ctx context.Context, query api.ThreadDetailQuery) (api.ThreadDetail, error) {
	if reader, ok := unwrapService[api.ThreadReader](s.Service); ok {
		return reader.ThreadDetail(ctx, query)
	}
	snapshot, err := s.Service.Snapshot(ctx)
	if err != nil {
		return api.ThreadDetail{}, err
	}
	return api.BuildThreadDetail(snapshot, query)
}

func (s *workbenchService) Search(ctx context.Context, query api.SearchQuery) ([]api.SearchHit, error) {
	type threadSearcher interface {
		Search(context.Context, api.SearchQuery) ([]api.SearchHit, error)
	}
	type coreThreadSearcher interface {
		SearchThreads(context.Context, api.SearchQuery) ([]api.SearchHit, error)
	}
	if searcher, ok := unwrapService[threadSearcher](s.Service); ok {
		return searcher.Search(ctx, query)
	}
	if searcher, ok := unwrapService[coreThreadSearcher](s.Service); ok {
		return searcher.SearchThreads(ctx, query)
	}
	return nil, &api.Error{Code: api.NotFound, Message: "当前 Core 未提供会话搜索接口"}
}

func (s *workbenchService) SearchThreads(ctx context.Context, query api.SearchQuery) ([]api.SearchHit, error) {
	return s.Search(ctx, query)
}

var (
	_ api.OrchestrationBroker = (*workbenchService)(nil)
	_ api.ThreadReader        = (*workbenchService)(nil)
	_ api.StatusReporter      = (*workbenchService)(nil)
	_ api.AdminController     = (*workbenchService)(nil)
)
