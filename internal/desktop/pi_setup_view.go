package desktop

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/pkg/api"
)

type piSetupState struct {
	open       bool
	preset     string
	providerID string
	apiKey     string
	showKey    bool
	baseURL    string
	apiType    string
	modelsText string
	setDefault bool
	saving     bool
	err        string
	output     string
	dismissed  bool
}

func (w *workbench) openPiSetup() {
	s := &w.widgets.piSetup
	if s.preset == "" {
		w.applyPiPreset("deepseek")
		s.setDefault = true
	}
	s.err = ""
	s.output = ""
	s.open = true
	w.widgets.modelPickerOpen = false
	if !w.providers.loaded && !w.providers.busy && !w.defaults.RemotePeer {
		w.loadPiStatus()
	}
}

func (w *workbench) applyPiPreset(id string) {
	s := &w.widgets.piSetup
	for _, preset := range piprovider.SetupPresets() {
		if preset.ID == id {
			s.preset = preset.ID
			s.providerID = preset.ProviderID
			s.baseURL = preset.DefaultBaseURL
			s.apiType = preset.API
			s.modelsText = preset.DefaultModel
			s.err = ""
			return
		}
	}
}

func (w *workbench) homeOnboardingCard(c *ui.Context, p palette) {
	if w.defaults.RemotePeer || w.defaults.ProviderID != "pi" || !w.modelsLoaded || w.modelsLoading || w.modelsErr != "" || len(w.models) > 0 || w.widgets.piSetup.dismissed {
		return
	}
	ui.Column(c.Key("home-pi-onboarding")).FillWidth().Padding(14, 16).Gap(10).Radius(14).Background(p.raised).Border(1, p.line).Children(func() {
		ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
			providerMark(c, p, "π")
			ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
				ui.Text(c, w.tr3("尚未配置可用模型", "No models configured yet", "利用可能なモデルが未設定です")).FontSize(13.5).FontWeight(600).TextColor(p.ink)
				ui.Text(c, w.tr3(
					"外置 Pi 已就绪，填写 API Key 或兼容接口即可开始对话；配置保存在标准 ~/.pi/agent 目录，与本机 Pi CLI 互通（不建软链）。",
					"External Pi is ready. Add an API key or compatible endpoint to start chatting; saved directly in ~/.pi/agent for full Pi CLI compatibility.",
					"外部 Pi の準備ができています。API キーまたは互換エンドポイントを設定してください（~/.pi/agent に直接保存）。",
				)).FontSize(12).TextColor(p.muted).Wrap()
			})
		})
		ui.Row(c).Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
			cfgBtn := primaryAction(c, p, "onboarding-setup-models", w.tr3("配置模型", "Configure models", "モデルを設定"))
			if cfgBtn.Clicked() {
				w.openPiSetup()
			}
			rtBtn := outlineButton(c, p, "onboarding-open-runtime", w.tr3("Pi 运行时与升级", "Pi runtime & updates", "Pi ランタイムと更新"))
			if rtBtn.Clicked() {
				w.widgets.settingsPage = "providers"
				w.widgets.settingsOpen = true
			}
			laterBtn := quietButton(c, p, "onboarding-dismiss", w.tr3("稍后", "Later", "後で")).Height(28).FontSize(12)
			if laterBtn.Clicked() {
				w.widgets.piSetup.dismissed = true
			}
		})
	})
}

func (w *workbench) piSetupDialog(c *ui.Context, p palette) {
	if !w.widgets.piSetup.open {
		return
	}
	s := &w.widgets.piSetup
	w.dialogFrame(
		c, p, &s.open,
		"pi-setup",
		w.tr3("配置 Pi 模型与接口", "Configure Pi models & provider", "Pi モデルとプロバイダー設定"),
		w.tr("关闭模型配置", "Close model setup"),
		agentsIcon,
		560,
		func(_ float32) {
			ui.Column(c).FillWidth().Padding(10, 12).Gap(4).Radius(10).Background(p.soft).Border(1, p.lineSoft).Children(func() {
				ui.Text(c, w.tr3(
					"直接写入标准 ~/.pi/agent（auth.json / models.json / settings.json），保留已有配置且不建软链。",
					"Writes directly to standard ~/.pi/agent (auth.json / models.json / settings.json) without overwriting existing entries or creating symlinks.",
					"標準の ~/.pi/agent に直接書き込み、既存設定を保持します。",
				)).FontSize(11.5).TextColor(p.muted).Wrap()
			})

			ui.Column(c).FillWidth().Gap(6).Children(func() {
				ui.Text(c, w.tr("选择服务商预设", "Choose provider preset")).FontSize(12).FontWeight(600).TextColor(p.ink)
				ui.Row(c).FillWidth().Gap(6).Wrap().Children(func() {
					for _, preset := range piprovider.SetupPresets() {
						preset := preset
						chosen := s.preset == preset.ID
						btn := ui.Button(c.Key("pi-preset:"+preset.ID), preset.Name).Label(preset.Name).
							Height(28).Padding(0, 10).Radius(8).Border(1, p.line)
						styleSegment(btn, p, chosen)
						btn.Disabled(s.saving)
						if btn.Clicked() && !chosen {
							w.applyPiPreset(preset.ID)
						}
					}
				})
			})

			isCustom := s.preset == "custom"
			showEndpointFields := isCustom || strings.TrimSpace(s.baseURL) != ""

			ui.Field(c, "API Key", func() {
				ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
					keyInput := ui.TextInput(c.Key("pi-setup-key"), &s.apiKey).Placeholder("sk-...").Grow(1).MinWidth(0)
					if !s.showKey {
						keyInput.Password()
					}
					if s.saving {
						keyInput.Disabled(true)
					}
					toggleLabel := w.tr("显示", "Show")
					if s.showKey {
						toggleLabel = w.tr("隐藏", "Hide")
					}
					if outlineButton(c, p, "pi-setup-toggle-key", toggleLabel).Clicked() {
						s.showKey = !s.showKey
					}
				})
			}).Description(w.tr("将保存到 ~/.pi/agent/auth.json（0600 私有权限）", "Saved privately in ~/.pi/agent/auth.json (0600)"))

			baseLabel := w.tr("中转 / 自定义 Base URL（可选）", "Custom Base URL (optional)")
			basePlaceholder := "https://api.example.com/v1"
			if isCustom {
				baseLabel = w.tr("接口 Base URL（必填）", "Endpoint Base URL (required)")
			}
			ui.Field(c, baseLabel, func() {
				inp := ui.TextInput(c.Key("pi-setup-base-url"), &s.baseURL).Placeholder(basePlaceholder).FillWidth()
				if s.saving {
					inp.Disabled(true)
				}
			})

			if showEndpointFields {
				ui.Field(c, w.tr("API 协议类型", "API protocol"), func() {
					apis := []string{"openai-completions", "openai-responses", "anthropic-messages", "google-generative-ai"}
					if s.apiType == "" {
						s.apiType = apis[0]
					}
					ui.Select(c.Key("pi-setup-api-type"), &s.apiType, apis).Label(w.tr("API 协议类型", "API protocol")).FillWidth()
				})
				if isCustom {
					ui.Field(c, "Provider ID", func() {
						inp := ui.TextInput(c.Key("pi-setup-provider-id"), &s.providerID).Placeholder("custom").FillWidth()
						if s.saving {
							inp.Disabled(true)
						}
					}).Description(w.tr("仅允许小写字母、数字、.、_、-", "Lowercase letters, digits, '.', '_', '-' only"))
				}
			}

			modelLabel := w.tr("模型 ID（可选，逗号分隔）", "Model IDs (optional, comma-separated)")
			modelPlaceholder := "deepseek-flash"
			if isCustom {
				modelLabel = w.tr("模型 ID（必填，逗号分隔）", "Model IDs (required, comma-separated)")
				modelPlaceholder = "gpt-4o, claude-sonnet-4-6"
			} else if s.modelsText != "" {
				modelPlaceholder = s.modelsText
			}
			ui.Field(c, modelLabel, func() {
				inp := ui.TextInput(c.Key("pi-setup-models"), &s.modelsText).Placeholder(modelPlaceholder).FillWidth()
				if s.saving {
					inp.Disabled(true)
				}
			})

			ui.Checkbox(c.Key("pi-setup-default"), &s.setDefault, w.tr("设为 Pi 默认模型", "Set as default Pi model"))

			if s.err != "" {
				ui.Text(c, s.err).FontSize(12).TextColor(p.danger).Wrap()
			}
			if s.output != "" {
				ui.Text(c, s.output).FontSize(12).TextColor(p.success).Wrap()
			}

			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
				saveLabel := w.tr3("保存并验证", "Save & Verify", "保存して検証")
				if s.saving {
					saveLabel = w.tr("正在验证…", "Verifying…")
				}
				saveBtn := primaryAction(c, p, "pi-setup-save", saveLabel)
				saveBtn.Disabled(s.saving)
				if saveBtn.Clicked() {
					w.submitPiSetup()
				}
				if s.saving {
					activityIndicator(c, p.accent)
				}
				oauthBtn := outlineButton(c, p, "pi-setup-oauth", w.tr("终端 OAuth 登录", "OAuth in Terminal"))
				oauthBtn.Disabled(s.saving)
				if oauthBtn.Clicked() {
					w.launchPiOAuthTerminal()
				}
				ui.Spacer(c)
				if outlineButton(c, p, "pi-setup-cancel", w.tr("取消", "Cancel")).Clicked() {
					s.open = false
				}
			})
		},
	)
}

func (w *workbench) submitPiSetup() {
	s := &w.widgets.piSetup
	if s.saving {
		return
	}
	var models []string
	if s.preset == "custom" || strings.TrimSpace(s.baseURL) != "" {
		for _, part := range strings.Split(s.modelsText, ",") {
			if m := strings.TrimSpace(part); m != "" {
				models = append(models, m)
			}
		}
	} else if trimmed := strings.TrimSpace(s.modelsText); trimmed != "" {
		// For built-in presets without a custom baseUrl, treat a custom-typed
		// single model as a requested default rather than replacing Pi's built-in list.
		for _, preset := range piprovider.SetupPresets() {
			if preset.ID == s.preset && trimmed != preset.DefaultModel {
				models = []string{trimmed}
				break
			}
		}
	}
	req := api.PiSetupRequest{
		Preset:     s.preset,
		ProviderID: strings.TrimSpace(s.providerID),
		APIKey:     s.apiKey,
		BaseURL:    strings.TrimSpace(s.baseURL),
		API:        strings.TrimSpace(s.apiType),
		Models:     models,
		SetDefault: s.setDefault,
	}
	payload, _ := json.Marshal(req)
	s.saving = true
	s.err = ""
	s.output = ""
	w.workbenchCall("pi-setup-save", api.WorkbenchRequest{Action: "pi.setup.save", Input: payload}, func(response api.WorkbenchResponse, err error) {
		s.saving = false
		if err != nil {
			s.err = describeUpdateError(w.tr("保存 Pi 模型配置失败", "Saving Pi configuration failed"), err)
			return
		}
		if response.PiRuntime != nil {
			w.applyPiRuntimeStatus(response.PiRuntime)
		}
		if response.Catalog != nil {
			w.models = append([]api.ModelInfo(nil), response.Catalog.Models...)
			w.modelsLoaded = true
			w.modelsErr = ""
			w.features.catalog = response.Catalog
			if def := nonEmptyString(response.Catalog.ConfiguredDefault, response.Catalog.Default); def != "" {
				w.defaultModel = def
			}
		}
		s.apiKey = ""
		s.open = false
		s.dismissed = true
		w.actionNotice = notice{Text: w.tr("Pi 模型配置已保存并验证就绪。", "Pi model configuration saved and verified."), Level: noticeInfo}
		w.invalidateModels()
		w.ensureModels(true)
		if _, ok := w.service.(api.WorkbenchService); ok {
			w.loadModels()
		}
	})
}

func (w *workbench) launchPiOAuthTerminal() {
	s := &w.widgets.piSetup
	oauthProvider := "anthropic"
	switch s.preset {
	case "openai":
		oauthProvider = "openai-codex"
	case "google":
		oauthProvider = "google-gemini-cli"
	case "openrouter":
		oauthProvider = "openrouter"
	case "anthropic":
		oauthProvider = "anthropic"
	}
	activePi := ""
	if w.providers.status != nil && strings.TrimSpace(w.providers.status.ActivePath) != "" {
		activePi = w.providers.status.ActivePath
	} else {
		activePi = piprovider.ResolveExecutable("", "")
	}
	if activePi == "" {
		activePi = "pi"
	}
	dir := w.selectedWorkspacePath()
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	term, err := w.makeTerminal(terminal.Options{
		Dir:        dir,
		Scrollback: 4 << 20,
		Command:    []string{activePi, "auth", "login", oauthProvider},
	})
	if err != nil {
		s.err = describe(w.tr("打开登录终端失败", "Failed to open login terminal"), err)
		return
	}
	target := w.selected
	if target == "" {
		target = "pi-oauth"
	}
	if old := w.features.terminals[target]; old != nil {
		_ = old.Close()
	}
	if w.features.terminals == nil {
		w.features.terminals = map[string]*terminal.Terminal{}
	}
	w.features.terminals[target] = term
	s.open = false
	w.features.panel = "terminal"
}
