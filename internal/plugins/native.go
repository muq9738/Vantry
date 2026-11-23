package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

func desktopManifest() rawManifest {
	return rawManifest{ID: "vantry-remote-desktop", Name: "远程桌面", Version: "0.3.1", Runtime: runtimeNative, Builtin: false,
		Repo:        "muq9738/vantry-plugin-remote-desktop",
		Description: "通过独立桌面共享码连接另一台 Vantry；画面与键鼠共享由本机单独授权。",
		Navigation:  []api.PluginNavItem{{Title: "远程桌面", Page: "desktop", Icon: "monitor", NativePanel: "remote-desktop"}},
		Settings: []api.PluginSettingSpec{
			{Key: "enabled", Label: "启用远程桌面插件", Type: "boolean", Default: "true"},
			{Key: "relayKey", Label: "中继桌面注册令牌", Type: "password", Help: "跨网中继地址沿用 设置 › 远程环境；这里只填该中继桌面模块的注册令牌，仅共享端需要。"},
			{Key: "quality", Label: "默认画质", Type: "select", Default: "balanced", Options: []api.PluginSettingOption{{Value: "low", Label: "流畅"}, {Value: "balanced", Label: "均衡"}, {Value: "ultra", Label: "高清"}}},
		},
	}
}
func applyNativeDesktop(d *definition) {
	manifest := desktopManifest()
	d.Runtime = runtimeNative
	d.Entry = ""
	d.Main = ""
	d.Command = nil
	d.Builtin = false
	if d.Repo == "" {
		d.Repo = manifest.Repo
	}
	d.Name = manifest.Name
	d.Description = manifest.Description
	if d.Version == "" {
		d.Version = manifest.Version
	}
	d.Navigation = manifest.Navigation
	d.perms = pluginPermissions{}
	settings := manifest.Settings
	for _, old := range d.Settings {
		for i := range settings {
			if old.Key == "defaultQuality" && settings[i].Key == "quality" && old.Default != "" {
				settings[i].Default = old.Default
			}
		}
	}
	d.Settings = settings
}
func validateNative(d definition) error {
	if d.effectiveRuntime() != runtimeNative {
		return errors.New("脚本插件已停用，原文件和设置已保留")
	}
	for name, panel := range d.Pages {
		if strings.TrimSpace(name) == "" || len(panel) > maxJSONBytes || !json.Valid(panel) {
			return errors.New("原生页面 JSON 无效")
		}
		var value any
		if json.Unmarshal(panel, &value) != nil {
			return errors.New("原生页面 JSON 无效")
		}
		if _, err := ValidatePanelOutput(string(panel)); err != nil {
			return err
		}
	}
	for _, nav := range d.Navigation {
		if nav.NativePanel == "" {
			if _, ok := d.Pages[nav.Page]; !ok {
				return fmt.Errorf("页面 %q 缺少声明式 JSON", nav.Page)
			}
		}
	}
	return nil
}
func (h Host) invokeNative(ctx context.Context, id, page, action string, input json.RawMessage) (string, error) {
	if ctx == nil {
		return "", errors.New("plugins: nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	d, ok := h.lookupDefinition(ctx, id)
	if !ok {
		return "", fmt.Errorf("插件 %q 不存在", id)
	}
	if d.err != nil {
		return "", d.err
	}
	if effectiveSettingValue(d, h.readSettingsMap(id), "enabled") == "false" {
		return "", errors.New("插件已停用，请在插件设置中启用")
	}
	input, err := normalizePluginInput(input)
	if err != nil {
		return "", err
	}
	if d.effectiveRuntime() == runtimeProcess {
		field, value := "page", page
		if action != "" {
			field, value = "action", action
		}
		input, err = mergeJSONFieldIfObject(input, field, value)
		if err != nil {
			return "", err
		}
		command, err := resolveProcessCommand(d.pluginDir, d)
		if err != nil {
			return "", err
		}
		life, cancel := context.WithTimeout(ctx, d.effectiveTimeout())
		defer cancel()
		return runProcessPlugin(life, command, d.ID, h.WorkspaceDir, input)
	}
	if err := validateNative(d); err != nil {
		return "", err
	}
	if action != "" && action != "run" && action != "page" {
		return "", errors.New("原生页面操作由对应 Go 模块提供，未声明此操作")
	}
	if page == "" && len(d.Navigation) > 0 {
		page = d.Navigation[0].Page
	}
	if panel, ok := d.Pages[page]; ok {
		return string(panel), nil
	}
	for _, nav := range d.Navigation {
		if nav.Page == page && nav.NativePanel != "" {
			data, _ := json.Marshal(map[string]any{"nativePanel": nav.NativePanel, "title": nav.Title})
			return string(data), nil
		}
	}
	return "", errors.New("插件未声明该页面")
}
func (h Host) Run(ctx context.Context, id string, input json.RawMessage) (string, error) {
	return h.invokeNative(ctx, id, "", "run", input)
}
func (h Host) CallPage(ctx context.Context, id, page string, input json.RawMessage) (string, error) {
	return h.invokeNative(ctx, id, page, "", input)
}
func (h Host) RunAction(ctx context.Context, id, method string, input json.RawMessage) (string, error) {
	return h.invokeNative(ctx, id, "", method, input)
}

// Execute preserves the former API with an explicit migration error. No JS engine
// or external executable is loaded, including for previously installed scripts.
func Execute(ctx context.Context, name, source string, input json.RawMessage) (string, error) {
	if ctx != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "", errors.New("JavaScript 插件运行时已移除；请使用声明式 JSON 或 process 插件")
}
func normalizePluginInput(input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		return json.RawMessage("{}"), nil
	}
	if len(input) > maxJSONBytes || !json.Valid(input) {
		return nil, errors.New("插件输入必须是 1 MiB 以内的 JSON")
	}
	return input, nil
}
func mergeJSONFieldIfObject(input json.RawMessage, field, value string) (json.RawMessage, error) {
	norm, err := normalizePluginInput(input)
	if err != nil {
		return nil, err
	}
	if value == "" || !bytes.HasPrefix(bytes.TrimSpace(norm), []byte("{")) {
		return norm, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(norm, &object) != nil {
		return norm, nil
	}
	if _, ok := object[field]; !ok {
		object[field], _ = json.Marshal(value)
	}
	return json.Marshal(object)
}
