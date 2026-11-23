package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/muq9738/Vantry/pkg/api"
)

const (
	pathOrchestrationCapabilities       = "/v1/orchestration/capabilities"
	pathOrchestrationDelegate           = "/v1/orchestration/delegate"
	pathOrchestrationTaskStatus         = "/v1/orchestration/task-status"
	pathOrchestrationTaskCancel         = "/v1/orchestration/task-cancel"
	pathOrchestrationThreadsList        = "/v1/orchestration/threads/list"
	pathOrchestrationThreadsRead        = "/v1/orchestration/threads/read"
	pathOrchestrationThreadsLaunch      = "/v1/orchestration/threads/launch"
	pathOrchestrationThreadsCreateBatch = "/v1/orchestration/threads/create-batch"
	pathOrchestrationThreadsSend        = "/v1/orchestration/threads/send"
	pathOrchestrationThreadsWait        = "/v1/orchestration/threads/wait"
	pathOrchestrationThreadsSearch      = "/v1/orchestration/threads/search"
)

type searchThreadsResponse struct {
	Hits []api.SearchHit `json:"hits"`
}

func (s *Server) handleOrchestration(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case pathOrchestrationCapabilities:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.OrchestratorCapabilities)
	case pathOrchestrationDelegate:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.DelegateTask)
	case pathOrchestrationTaskStatus:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.TaskStatus)
	case pathOrchestrationTaskCancel:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.CancelTask)
	case pathOrchestrationThreadsList:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.ListThreads)
	case pathOrchestrationThreadsRead:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.ReadThread)
	case pathOrchestrationThreadsLaunch:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.LaunchThread)
	case pathOrchestrationThreadsCreateBatch:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.CreateThreads)
	case pathOrchestrationThreadsSend:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.SendToThread)
	case pathOrchestrationThreadsWait:
		handleBrokerCall(w, r, s.service, api.OrchestrationBroker.WaitThread)
	case pathOrchestrationThreadsSearch:
		s.handleOrchestrationSearch(w, r)
	default:
		writeError(w, &api.Error{Code: api.NotFound, Message: "unknown endpoint"})
	}
}

func handleBrokerCall[Req any, Resp any](
	w http.ResponseWriter,
	r *http.Request,
	service api.Service,
	call func(api.OrchestrationBroker, context.Context, Req) (Resp, error),
) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireNoQuery(w, r) {
		return
	}
	broker, ok := service.(api.OrchestrationBroker)
	if !ok {
		writeError(w, &api.Error{Code: api.NotFound, Message: "当前 Core 未提供会话编排接口"})
		return
	}
	request, ok := decodeOrchestrationBody[Req](w, r)
	if !ok {
		return
	}
	response, err := call(broker, r.Context(), *request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleOrchestrationSearch(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !requireNoQuery(w, r) {
		return
	}
	var searchFn func(context.Context, api.SearchQuery) ([]api.SearchHit, error)
	if searcher, ok := s.service.(interface {
		Search(context.Context, api.SearchQuery) ([]api.SearchHit, error)
	}); ok {
		searchFn = searcher.Search
	} else if searcher, ok := s.service.(interface {
		SearchThreads(context.Context, api.SearchQuery) ([]api.SearchHit, error)
	}); ok {
		searchFn = searcher.SearchThreads
	} else {
		writeError(w, &api.Error{Code: api.NotFound, Message: "当前 Core 未提供会话搜索接口"})
		return
	}

	query, ok := decodeOrchestrationBody[api.SearchQuery](w, r)
	if !ok {
		return
	}
	hits, err := searchFn(r.Context(), *query)
	if err != nil {
		writeError(w, err)
		return
	}
	if hits == nil {
		hits = []api.SearchHit{}
	}
	writeJSON(w, http.StatusOK, searchThreadsResponse{Hits: hits})
}

func decodeOrchestrationBody[Req any](w http.ResponseWriter, r *http.Request) (*Req, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCommandBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request *Req
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, invalid("request body exceeds %d bytes", maxCommandBytes))
		} else {
			writeError(w, invalid("decode request JSON: %v", err))
		}
		return nil, false
	}
	if request == nil {
		writeError(w, invalid("request body must be a JSON object"))
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			writeError(w, invalid("request body must contain exactly one JSON object"))
		} else {
			writeError(w, invalid("decode trailing request data: %v", err))
		}
		return nil, false
	}
	return request, true
}

func postOrchestrationJSON[Req any, Resp any](ctx context.Context, c *Client, path string, request Req) (Resp, error) {
	var zero Resp
	body, err := json.Marshal(request)
	if err != nil {
		return zero, fmt.Errorf("local: encode %s: %w", path, err)
	}
	if len(body) > maxCommandBytes {
		return zero, invalid("request body exceeds %d bytes", maxCommandBytes)
	}
	var response Resp
	if err := c.doJSON(ctx, http.MethodPost, path, body, &response); err != nil {
		return zero, err
	}
	return response, nil
}

func (c *Client) OrchestratorCapabilities(ctx context.Context, query api.OrchestratorCapabilitiesQuery) (api.OrchestratorCapabilities, error) {
	return postOrchestrationJSON[api.OrchestratorCapabilitiesQuery, api.OrchestratorCapabilities](ctx, c, pathOrchestrationCapabilities, query)
}

func (c *Client) DelegateTask(ctx context.Context, request api.DelegateTaskRequest) (api.TaskDetail, error) {
	return postOrchestrationJSON[api.DelegateTaskRequest, api.TaskDetail](ctx, c, pathOrchestrationDelegate, request)
}

func (c *Client) TaskStatus(ctx context.Context, query api.TaskStatusQuery) (api.TaskStatusResult, error) {
	return postOrchestrationJSON[api.TaskStatusQuery, api.TaskStatusResult](ctx, c, pathOrchestrationTaskStatus, query)
}

func (c *Client) CancelTask(ctx context.Context, request api.CancelTaskRequest) (api.CancelTaskResult, error) {
	return postOrchestrationJSON[api.CancelTaskRequest, api.CancelTaskResult](ctx, c, pathOrchestrationTaskCancel, request)
}

func (c *Client) ListThreads(ctx context.Context, query api.ThreadListQuery) (api.ThreadListResult, error) {
	return postOrchestrationJSON[api.ThreadListQuery, api.ThreadListResult](ctx, c, pathOrchestrationThreadsList, query)
}

func (c *Client) ReadThread(ctx context.Context, query api.ThreadReadQuery) (api.ThreadReadResult, error) {
	return postOrchestrationJSON[api.ThreadReadQuery, api.ThreadReadResult](ctx, c, pathOrchestrationThreadsRead, query)
}

func (c *Client) LaunchThread(ctx context.Context, request api.ThreadLaunchRequest) (api.LaunchedThread, error) {
	return postOrchestrationJSON[api.ThreadLaunchRequest, api.LaunchedThread](ctx, c, pathOrchestrationThreadsLaunch, request)
}

func (c *Client) CreateThreads(ctx context.Context, request api.CreateThreadsRequest) (api.CreateThreadsResult, error) {
	return postOrchestrationJSON[api.CreateThreadsRequest, api.CreateThreadsResult](ctx, c, pathOrchestrationThreadsCreateBatch, request)
}

func (c *Client) SendToThread(ctx context.Context, request api.InterThreadSendRequest) (api.InterThreadSendResult, error) {
	return postOrchestrationJSON[api.InterThreadSendRequest, api.InterThreadSendResult](ctx, c, pathOrchestrationThreadsSend, request)
}

func (c *Client) WaitThread(ctx context.Context, query api.ThreadWaitQuery) (api.ThreadWaitResult, error) {
	return postOrchestrationJSON[api.ThreadWaitQuery, api.ThreadWaitResult](ctx, c, pathOrchestrationThreadsWait, query)
}

func (c *Client) Search(ctx context.Context, query api.SearchQuery) ([]api.SearchHit, error) {
	response, err := postOrchestrationJSON[api.SearchQuery, searchThreadsResponse](ctx, c, pathOrchestrationThreadsSearch, query)
	if err != nil {
		return nil, err
	}
	if response.Hits == nil {
		return []api.SearchHit{}, nil
	}
	return response.Hits, nil
}

func (c *Client) SearchThreads(ctx context.Context, query api.SearchQuery) ([]api.SearchHit, error) {
	return c.Search(ctx, query)
}

var _ api.OrchestrationBroker = (*Client)(nil)
