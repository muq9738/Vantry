package app

import (
	"context"
	"encoding/json"
	"strings"

	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/pkg/api"
)

func (s *workbenchService) effectivePiExecutable() string {
	return piprovider.ResolveExecutable(s.config.PiExecutable, s.config.DataDir)
}

func (s *workbenchService) activeRunCount(ctx context.Context) int {
	if s.Service == nil {
		return 0
	}
	snapshot, err := s.Service.Snapshot(ctx)
	if err != nil {
		return 0
	}
	return api.ActiveRunCount(snapshot)
}

func (s *workbenchService) piRuntimeAction(ctx context.Context, req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	var response api.WorkbenchResponse
	switch req.Action {
	case "pi.status":
		status := piprovider.InspectRuntimeWithOptions(ctx, s.config.PiExecutable, s.config.DataDir, false, s.activeRunCount(ctx))
		response.PiRuntime = &status
		return response, nil

	case "pi.check_update":
		status := piprovider.InspectRuntimeWithOptions(ctx, s.config.PiExecutable, s.config.DataDir, true, s.activeRunCount(ctx))
		response.PiRuntime = &status
		if status.Error != "" && status.LatestVersion == "" {
			return response, &api.Error{Code: api.InvalidCommand, Message: status.Error}
		}
		return response, nil

	case "pi.upgrade":
		var input api.PiUpgradeInput
		if len(req.Input) > 0 {
			if err := json.Unmarshal(req.Input, &input); err != nil {
				return response, &api.Error{Code: api.InvalidCommand, Message: "Pi 升级参数无效"}
			}
		}
		status, summary, err := piprovider.UpgradePiWithOptions(ctx, s.config.PiExecutable, s.config.DataDir, input, s.activeRunCount(ctx))
		response.PiRuntime = &status
		response.Output = summary
		if err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: err.Error()}
		}
		return response, nil

	case "pi.settings.set":
		var input api.PiRuntimeSettingsInput
		if len(req.Input) > 0 {
			if err := json.Unmarshal(req.Input, &input); err != nil {
				return response, &api.Error{Code: api.InvalidCommand, Message: "Pi 运行时设置参数无效"}
			}
		}
		current := piprovider.LoadRuntimeSettings(s.config.DataDir)
		if strings.TrimSpace(input.MirrorMode) != "" {
			current.MirrorMode = input.MirrorMode
		}
		// Explicit customPath / npmRegistry fields allow clearing back to empty when passed.
		var rawMap map[string]json.RawMessage
		if len(req.Input) > 0 && json.Unmarshal(req.Input, &rawMap) == nil {
			if _, ok := rawMap["customPath"]; ok {
				current.CustomPath = input.CustomPath
			}
			if _, ok := rawMap["npmRegistry"]; ok {
				current.NPMRegistry = input.NPMRegistry
			}
		}
		if _, err := piprovider.SaveRuntimeSettings(s.config.DataDir, current); err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: err.Error()}
		}
		status := piprovider.InspectRuntimeWithOptions(ctx, s.config.PiExecutable, s.config.DataDir, false, s.activeRunCount(ctx))
		response.PiRuntime = &status
		return response, nil

	case "pi.setup.save":
		var setupReq api.PiSetupRequest
		if err := json.Unmarshal(req.Input, &setupReq); err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: "模型配置参数无效"}
		}
		catalog, err := piprovider.SaveProviderSetup(ctx, s.effectivePiExecutable(), "", setupReq)
		if err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: err.Error()}
		}
		status := piprovider.InspectRuntimeWithOptions(ctx, s.config.PiExecutable, s.config.DataDir, false, s.activeRunCount(ctx))
		response.Catalog = &catalog
		response.PiRuntime = &status
		response.Output = "已保存到标准 ~/.pi/agent 配置并完成验证。"
		return response, nil

	default:
		return response, &api.Error{Code: api.InvalidCommand, Message: "未知 Pi 运行时操作"}
	}
}
