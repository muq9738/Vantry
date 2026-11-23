package app

import (
	"context"
	"github.com/muq9738/Vantry/pkg/api"
)

func (s *workbenchService) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	response, err := s.Workbench(ctx, api.WorkbenchRequest{Action: "models"})
	if err != nil {
		return nil, err
	}
	if response.Catalog == nil {
		return nil, &api.Error{Code: api.NotReady, Message: "模型目录尚未就绪"}
	}
	models := append([]api.ModelInfo(nil), response.Catalog.Models...)
	for i := range models {
		models[i].ID = models[i].Key()
	}
	return models, nil
}

func (s *workbenchService) InspectWorkspace(ctx context.Context, threadID string) (api.WorkspaceChanges, error) {
	if service, ok := s.Service.(api.WorkbenchQueries); ok {
		return service.InspectWorkspace(ctx, threadID)
	}
	return api.WorkspaceChanges{}, &api.Error{Code: api.InvalidCommand, Message: "当前服务未提供工作区查询"}
}

func (s *workbenchService) OpenTerminal(ctx context.Context, threadID string) error {
	if service, ok := s.Service.(api.WorkbenchQueries); ok {
		return service.OpenTerminal(ctx, threadID)
	}
	return &api.Error{Code: api.InvalidCommand, Message: "当前服务未提供终端"}
}
