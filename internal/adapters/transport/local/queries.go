package local

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

func (s *Server) handleWorkbenchQuery(w http.ResponseWriter, r *http.Request) {
	method := http.MethodGet
	if r.URL.Path == "/v1/terminal" {
		method = http.MethodPost
	}
	if !requireMethod(w, r, method) {
		return
	}
	service, ok := s.service.(api.WorkbenchQueries)
	if !ok {
		writeError(w, &api.Error{Code: api.VersionMismatch, Message: "当前 Core 未提供模型与工作区查询，请更新并重启本地服务"})
		return
	}
	if r.URL.Path == "/v1/models" {
		if !requireNoQuery(w, r) {
			return
		}
		models, err := service.ListModels(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, models)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["thread"]) != 1 || strings.TrimSpace(query.Get("thread")) == "" {
		writeError(w, invalid("endpoint requires exactly one non-empty thread"))
		return
	}
	if r.URL.Path == "/v1/workspace" {
		changes, err := service.InspectWorkspace(r.Context(), query.Get("thread"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, changes)
		return
	}
	if err := service.OpenTerminal(r.Context(), query.Get("thread")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Opened bool `json:"opened"`
	}{Opened: true})
}

func (c *Client) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	var models []api.ModelInfo
	err := c.doJSON(ctx, http.MethodGet, "/v1/models", nil, &models)
	if api.CodeOf(err) == api.NotFound {
		return nil, &api.Error{Code: api.VersionMismatch, Message: "当前 Core 尚未提供模型目录接口，请更新并重启本地服务"}
	}
	return models, err
}

func (c *Client) InspectWorkspace(ctx context.Context, threadID string) (api.WorkspaceChanges, error) {
	var changes api.WorkspaceChanges
	err := c.doJSON(ctx, http.MethodGet, "/v1/workspace?thread="+url.QueryEscape(threadID), nil, &changes)
	return changes, err
}

func (c *Client) OpenTerminal(ctx context.Context, threadID string) error {
	var result struct {
		Opened bool `json:"opened"`
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/terminal?thread="+url.QueryEscape(threadID), nil, &result)
}

var _ api.WorkbenchQueries = (*Client)(nil)
