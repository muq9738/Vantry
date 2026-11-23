package desktop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestHomeOnboardingAppearsOnlyWhenPiModelsEmptyAndSavesSetup(t *testing.T) {
	models := []api.ModelInfo{}
	w, rec := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case "pi.status":
			return api.WorkbenchResponse{
				PiRuntime: &api.PiRuntimeStatus{
					ActiveSource:  "system",
					ActivePath:    "/opt/homebrew/bin/pi",
					ActiveVersion: "1.6.0",
					SystemIsNPM:   true,
					NPMAvailable:  true,
					NodeVersion:   "22.21.0",
					MirrorMode:    "auto",
					ResolvedRoute: "国内镜像（registry.npmmirror.com）",
					AgentDir:      "/Users/demo/.pi/agent",
					Configured:    len(models) > 0,
					ModelCount:    len(models),
				},
			}, nil
		case "pi.setup.save":
			models = []api.ModelInfo{{Provider: "deepseek", ID: "deepseek-flash", Name: "DeepSeek Flash"}}
			return api.WorkbenchResponse{
				Catalog: &api.ModelCatalog{
					Models:            models,
					Default:           "deepseek/deepseek-flash",
					ConfiguredDefault: "deepseek/deepseek-flash",
				},
				PiRuntime: &api.PiRuntimeStatus{
					ActiveSource:  "system",
					ActivePath:    "/opt/homebrew/bin/pi",
					ActiveVersion: "1.6.0",
					SystemIsNPM:   true,
					NPMAvailable:  true,
					Configured:    true,
					ModelCount:    1,
					DefaultModel:  "deepseek/deepseek-flash",
				},
			}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	w.threads = nil
	w.runs = nil
	w.defaults.Workspace = nil
	w.selected = ""
	w.newSession()
	w.models = nil
	w.modelsLoaded = true
	w.modelsLoading = false
	w.modelsErr = ""

	tt := ui.NewTester(w.view, 1180, 820)
	tt.Frame()
	if !tt.HasText("尚未配置可用模型") {
		t.Fatalf("expected onboarding card when no models configured: %v", tt.Texts())
	}

	mustClick(t, tt, "配置模型")
	tt.Frame()
	if !w.widgets.piSetup.open || !tt.HasText("配置 Pi 模型与接口") {
		t.Fatalf("setup dialog did not open: open=%v texts=%v", w.widgets.piSetup.open, tt.Texts())
	}

	mustClick(t, tt, "OpenAI")
	tt.Frame()
	if w.widgets.piSetup.providerID != "openai" {
		t.Fatalf("preset did not update providerID: %q", w.widgets.piSetup.providerID)
	}
	w.widgets.piSetup.apiKey = "sk-test-123456"
	mustClick(t, tt, "保存并验证")
	tt.Frame()

	req, ok := rec.find("pi.setup.save")
	if !ok {
		t.Fatal("pi.setup.save was not dispatched")
	}
	var sent api.PiSetupRequest
	if err := json.Unmarshal(req.Input, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ProviderID != "openai" || sent.APIKey != "sk-test-123456" || !sent.SetDefault {
		t.Fatalf("unexpected pi.setup.save payload: %+v", sent)
	}
	if w.widgets.piSetup.open || tt.HasText("尚未配置可用模型") {
		t.Fatalf("onboarding/dialog did not close after setup: open=%v", w.widgets.piSetup.open)
	}
}

func TestProviderSettingsShowsVerifiedExternalPiUpgradeAndMirrorRouting(t *testing.T) {
	w, rec := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case "pi.status":
			return api.WorkbenchResponse{
				PiRuntime: &api.PiRuntimeStatus{
					ActiveSource:    "system",
					ActivePath:      "/opt/homebrew/bin/pi",
					ActiveVersion:   "1.5.2",
					SystemPath:      "/opt/homebrew/bin/pi",
					SystemVersion:   "1.5.2",
					SystemIsNPM:     true,
					NPMAvailable:    true,
					NodeVersion:     "22.21.0",
					LatestVersion:   "1.6.0",
					UpdateAvailable: true,
					CompatVerified:  true,
					CanUpgrade:      true,
					MirrorMode:      "auto",
					ResolvedRoute:   "国内镜像（registry.npmmirror.com）",
					AgentDir:        "/Users/demo/.pi/agent",
					Configured:      true,
					ModelCount:      2,
					DefaultModel:    "deepseek/deepseek-flash",
				},
			}, nil
		case "pi.upgrade":
			return api.WorkbenchResponse{
				PiRuntime: &api.PiRuntimeStatus{
					ActiveSource:    "system",
					ActivePath:      "/opt/homebrew/bin/pi",
					ActiveVersion:   "1.6.0",
					SystemPath:      "/opt/homebrew/bin/pi",
					SystemVersion:   "1.6.0",
					SystemIsNPM:     true,
					NPMAvailable:    true,
					NodeVersion:     "22.21.0",
					LatestVersion:   "1.6.0",
					UpdateAvailable: false,
					CompatVerified:  true,
					CanUpgrade:      false,
					MirrorMode:      "auto",
					ResolvedRoute:   "国内镜像（registry.npmmirror.com）",
					AgentDir:        "/Users/demo/.pi/agent",
					Configured:      true,
					ModelCount:      2,
					DefaultModel:    "deepseek/deepseek-flash",
				},
				Output: "已通过 registry.npmmirror.com 将外置 Pi 升级到 v1.6.0，并完成 Vantry 兼容验证",
			}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	w.widgets.settingsOpen = true
	w.widgets.settingsPage = "providers"
	tt := ui.NewTester(w.view, 1100, 900)
	tt.Frame()

	for _, want := range []string{"Pi 执行器", "本机 Pi v1.5.2", "已通过 Vantry 兼容验证", "发现兼容的 Pi 新版本 v1.6.0", "升级 Pi", "外置 Pi 执行路径", "Pi npm 更新镜像", "registry.npmmirror.com"} {
		if !tt.HasText(want) {
			t.Errorf("providers view missing %q; texts=%v", want, tt.Texts())
		}
	}

	mustClick(t, tt, "升级 Pi")
	tt.Frame()
	if _, ok := rec.find("pi.upgrade"); !ok {
		t.Fatal("pi.upgrade action was not dispatched")
	}
	if !tt.HasText("本机 Pi v1.6.0") || !strings.Contains(strings.Join(tt.Texts(), "\n"), "将外置 Pi 升级到 v1.6.0") {
		t.Fatalf("upgraded version not reflected in UI: %v", tt.Texts())
	}
}

func TestProviderSettingsWithholdsUnverifiedPiUpgrade(t *testing.T) {
	w, _ := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if r.Action == "pi.status" {
			return api.WorkbenchResponse{
				PiRuntime: &api.PiRuntimeStatus{
					ActiveSource:    "system",
					ActivePath:      "/opt/homebrew/bin/pi",
					ActiveVersion:   "1.6.0",
					SystemPath:      "/opt/homebrew/bin/pi",
					SystemVersion:   "1.6.0",
					SystemIsNPM:     true,
					NPMAvailable:    true,
					NodeVersion:     "22.21.0",
					LatestVersion:   "2.0.0",
					UpdateAvailable: false,
					CompatVerified:  false,
					CompatBlocked:   true,
					CompatReason:    "Pi v2.0.0 为跨主版本更新（当前仅认证 1.x），需等待 Vantry 适配后再开放升级",
					CanUpgrade:      false,
					MirrorMode:      "auto",
					ResolvedRoute:   "国内镜像（registry.npmmirror.com）",
					AgentDir:        "/Users/demo/.pi/agent",
					Configured:      true,
					ModelCount:      2,
				},
			}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	w.widgets.settingsOpen = true
	w.widgets.settingsPage = "providers"
	tt := ui.NewTester(w.view, 1100, 900)
	tt.Frame()

	if tt.HasText("升级 Pi") {
		t.Fatalf("upgrade button must NOT be rendered when compat gate blocked update; texts=%v", tt.Texts())
	}
	if !tt.HasText("暂不开放 Pi v2.0.0 升级（零影响保护）") || !tt.HasText("跨主版本更新") {
		t.Fatalf("expected safety block notice in providers view; texts=%v", tt.Texts())
	}
}

func TestProviderSettingsShowsDirectUpgradeButtonForLocalNPMPi(t *testing.T) {
	w, rec := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case "pi.status":
			return api.WorkbenchResponse{
				PiRuntime: &api.PiRuntimeStatus{
					ActiveSource:  "system",
					ActivePath:    "/Users/demo/.nvm/versions/node/v26.12.1/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js",
					ActiveVersion: "1.5.2",
					SystemPath:    "/Users/demo/.nvm/versions/node/v26.12.1/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js",
					SystemVersion: "1.5.2",
					SystemIsNPM:   true,
					NPMAvailable:  true,
					NodeVersion:   "26.12.1",
					MirrorMode:    "auto",
					ResolvedRoute: "国内镜像（registry.npmmirror.com）",
					AgentDir:      "/Users/demo/.pi/agent",
					Configured:    true,
					ModelCount:    2,
				},
			}, nil
		case "pi.upgrade":
			return api.WorkbenchResponse{
				PiRuntime: &api.PiRuntimeStatus{
					ActiveSource:  "system",
					ActivePath:    "/Users/demo/.nvm/versions/node/v26.12.1/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js",
					ActiveVersion: "1.6.0",
					SystemPath:    "/Users/demo/.nvm/versions/node/v26.12.1/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js",
					SystemVersion: "1.6.0",
					SystemIsNPM:   true,
					NPMAvailable:  true,
					NodeVersion:   "26.12.1",
					LatestVersion: "1.6.0",
					MirrorMode:    "auto",
					ResolvedRoute: "国内镜像（registry.npmmirror.com）",
					AgentDir:      "/Users/demo/.pi/agent",
					Configured:    true,
					ModelCount:    2,
				},
				Output: "已通过 registry.npmmirror.com 将外置 Pi 升级到 v1.6.0，并完成 Vantry 兼容验证",
			}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	w.widgets.settingsOpen = true
	w.widgets.settingsPage = "providers"
	tt := ui.NewTester(w.view, 1100, 900)
	tt.Frame()

	if !tt.HasText("升级 Pi") {
		t.Fatalf("expected direct 升级 Pi button for local npm-managed Pi; texts=%v", tt.Texts())
	}
	mustClick(t, tt, "升级 Pi")
	tt.Frame()
	if _, ok := rec.find("pi.upgrade"); !ok {
		t.Fatal("pi.upgrade action was not dispatched from direct upgrade button")
	}
	if !tt.HasText("本机 Pi v1.6.0") {
		t.Fatalf("upgraded version not reflected in UI: %v", tt.Texts())
	}
}
