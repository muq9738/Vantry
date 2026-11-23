package local

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/muq9738/Vantry/pkg/api"
)

const maxWorkbenchBytes = 12 << 20

func (c *Client) Workbench(ctx context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	var response api.WorkbenchResponse
	raw, err := json.Marshal(request)
	if err != nil {
		return response, err
	}
	if len(raw) > maxWorkbenchBytes {
		return response, invalid("工作台请求过大")
	}
	err = c.doJSON(ctx, http.MethodPost, "/v1/workbench", raw, &response)
	return response, err
}

func (s *Server) handleWorkbench(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) || !requireNoQuery(w, r) {
		return
	}
	service, ok := s.service.(api.WorkbenchService)
	if !ok {
		writeError(w, &api.Error{Code: api.NotFound, Message: "Core 不支持工作台操作，请更新并重启 Core"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkbenchBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request *api.WorkbenchRequest
	if err := decoder.Decode(&request); err != nil || request == nil {
		writeError(w, invalid("工作台请求 JSON 无效"))
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeError(w, invalid("请求必须只有一个 JSON 对象"))
		return
	}
	response, err := service.Workbench(r.Context(), *request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}
