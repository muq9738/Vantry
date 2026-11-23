package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// Catalog asks Pi's own model registry. It never parses credentials or invokes
// an LLM. Resource loading matches the durable worker's extension policy.
func Catalog(ctx context.Context, executable, directory, selected string) (api.ModelCatalog, error) {
	return queryCatalog(ctx, executable, directory, selected, nil)
}

// catalogModelKey drops a trailing thinking level ("provider/model:high");
// catalog keys never carry one, so callers may pass a thread's full model.
func catalogModelKey(model string) string {
	if index := strings.LastIndexByte(model, ':'); index >= 0 {
		switch model[index+1:] {
		case "off", "minimal", "low", "medium", "high", "xhigh", "max":
			return model[:index]
		}
	}
	return model
}

func queryCatalog(ctx context.Context, executable, directory, selected string, selection *MagpieSelection) (api.ModelCatalog, error) {
	selected = catalogModelKey(selected)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	executable = resolvePiExecutable(executable)
	cmd, err := newPiQueryCommand(ctx, executable, "--mode", "rpc", "--no-session", "--no-extensions", "--no-mcp", "--no-tools")
	if err != nil {
		return api.ModelCatalog{}, err
	}
	defer cmd.Close()
	cmd.Env = piEnvironment(executable)
	cmd.Dir = directory
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return api.ModelCatalog{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return api.ModelCatalog{}, err
	}
	// Pi may print provider setup diagnostics. Do not expose credentials or raw
	// model headers by returning an entire registry response to the desktop.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return api.ModelCatalog{}, fmt.Errorf("启动 Pi 模型查询：%w", err)
	}
	defer func() { _ = stdin.Close(); cancel(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), 16<<20)
	serial := 0
	call := func(kind string, fields map[string]any, target any) error {
		serial++
		id := fmt.Sprintf("catalog-%d", serial)
		if fields == nil {
			fields = map[string]any{}
		}
		fields["id"], fields["type"] = id, kind
		if err := json.NewEncoder(stdin).Encode(fields); err != nil {
			return err
		}
		for scan.Scan() {
			var record struct {
				ID      string          `json:"id"`
				Type    string          `json:"type"`
				Success bool            `json:"success"`
				Data    json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(scan.Bytes(), &record); err != nil {
				return fmt.Errorf("Pi RPC 响应无效：%w", err)
			}
			if record.ID != id || record.Type != "response" {
				continue
			}
			if !record.Success {
				return fmt.Errorf("Pi 拒绝 %s 查询，请检查模型配置", kind)
			}
			if target == nil {
				return nil
			}
			return json.Unmarshal(record.Data, target)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if scan.Err() != nil {
			return scan.Err()
		}
		return errors.New("Pi 模型查询提前退出")
	}
	// A normal query reports only real gaps. There is no standing hint about
	// which resources the catalog probes, so the model picker stays quiet
	// unless the user's configuration actually disagrees with what Pi loaded.
	catalog := api.ModelCatalog{ConfiguredDefault: ConfiguredModel()}
	if err := call("get_available_models", nil, &catalog); err != nil {
		return catalog, err
	}
	if selection != nil {
		catalog.Models = selection.filter(catalog.Models)
		catalog.Warnings = nil
		if len(catalog.Models) < len(selection.keys) {
			catalog.Warnings = append(catalog.Warnings, "部分 Magpie 已选模型尚未被 Pi 识别，请在 Magpie 应用 Pi 配置后重新载入。")
		}
	} else if configuredKeys, ok := ConfiguredModelKeys(); ok {
		byKey := make(map[string]api.ModelInfo, len(catalog.Models))
		for _, m := range catalog.Models {
			byKey[m.Key()] = m
		}
		filtered := make([]api.ModelInfo, 0, len(configuredKeys))
		for _, key := range configuredKeys {
			if m, exists := byKey[key]; exists {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) > 0 {
			catalog.Models = filtered
			catalog.Warnings = nil
		}
	}
	var state struct {
		Model api.ModelInfo `json:"model"`
	}
	if err := call("get_state", nil, &state); err != nil {
		return catalog, err
	}
	if state.Model.ID != "" {
		catalog.Default = state.Model.Key()
		if selection != nil {
			if _, ok := selection.Base(catalog.Default); !ok {
				catalog.Default = ""
			}
		}
	}
	if catalog.ConfiguredDefault != "" {
		found := false
		for _, model := range catalog.Models {
			found = found || model.Key() == catalog.ConfiguredDefault
		}
		if !found {
			catalog.Warnings = append(catalog.Warnings, "Pi 设置中的默认模型当前不可用，请先在 Pi 中检查配置，或选择目录中的可用模型。")
		}
	}
	if selected != "" && selected != catalog.Default {
		// Match the complete key; model IDs may themselves contain slashes.
		found := false
		for _, m := range catalog.Models {
			if m.Key() == selected {
				found = true
				if err := call("set_model", map[string]any{"provider": m.Provider, "modelId": m.ID}, nil); err != nil {
					return catalog, err
				}
				break
			}
		}
		if !found {
			return catalog, fmt.Errorf("模型不在 Pi 当前可用目录中：%s", selected)
		}
	}
	var thinking struct {
		Levels []string `json:"levels"`
	}
	if err := call("get_available_thinking_levels", nil, &thinking); err != nil {
		return catalog, err
	}
	catalog.ThinkingLevels = thinking.Levels
	if _, hasConfiguredOrder := ConfiguredModelKeys(); selection == nil && !hasConfiguredOrder {
		sort.Slice(catalog.Models, func(i, j int) bool {
			return strings.ToLower(catalog.Models[i].Key()) < strings.ToLower(catalog.Models[j].Key())
		})
	}
	return catalog, nil
}
