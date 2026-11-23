package desktop

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/pkg/api"
)

type providerSettings struct {
	busy               bool
	checking           bool
	upgrading          bool
	saving             bool
	loaded             bool
	health             *piHealth
	status             *api.PiRuntimeStatus
	err                string
	output             string
	showCustomPath     bool
	customPiPath       string
	showAdvancedMirror bool
	customRegistry     string
}

func providerMark(c *ui.Context, p palette, mark string) {
	ui.Row(c).Size(32, 32).Radius(8).Justify(ui.Center).AlignItems(ui.Center).Background(p.soft).Border(1, p.lineSoft).Shrink(0).Children(func() {
		ui.Text(c, mark).FontSize(13).FontWeight(650).TextColor(p.ink)
	})
}

func (w *workbench) settingsProviders(c *ui.Context, p palette) {
	if !w.providers.loaded && !w.providers.busy && !w.defaults.RemotePeer {
		w.loadPiStatus()
	}
	ui.Column(c).Gap(12).Children(func() {
		settingsHeading(
			c, p,
			w.tr3("Pi 执行器", "Pi runtime", "Pi ランタイム"),
			w.tr3(
				"Vantry 轻量化调用本机外置 pi --mode rpc，不捆绑 Pi 主程序以保持应用小巧；模型与接口统一读写 ~/.pi/agent，升级前先在隔离沙箱完成零影响兼容校验。",
				"Vantry invokes your external pi --mode rpc without bundling a heavy binary, sharing ~/.pi/agent directly and sandbox-verifying upgrades for zero impact before offering them.",
				"Vantry は外部 pi --mode rpc を軽量呼び出しし、~/.pi/agent を直接共有します。更新前には互換性検証を実施します。",
			),
			nil,
		)
		w.piCard(c, p)
		w.piRuntimeCard(c, p)
		w.piCodemodeCard(c, p)
	})
}

func (w *workbench) piSourceLabel(source string) string {
	switch source {
	case piprovider.RuntimeSourceSystem:
		return w.tr3("本机 Pi", "System Pi", "システム Pi")
	case piprovider.RuntimeSourceCustom:
		return w.tr3("自定义 Pi", "Custom Pi", "カスタム Pi")
	case piprovider.RuntimeSourceMissing:
		return w.tr3("未安装", "Missing", "未インストール")
	default:
		return w.tr3("外置 Pi", "External Pi", "外部 Pi")
	}
}

func (w *workbench) piCard(c *ui.Context, p palette) {
	st := w.providers.status
	status, tone := w.tr3("外置", "External", "外部"), p.success
	tagline := w.tr3("调用本机外置 pi --mode rpc（支持 npm 安全升级）", "Uses your external pi --mode rpc (with verified npm upgrades)", "外部 pi --mode rpc を使用")
	badgeText := w.tr3("外置执行器", "External runtime", "外部ランタイム")

	if st != nil {
		badgeText = w.piSourceLabel(st.ActiveSource)
		if st.ActiveVersion != "" {
			badgeText += " v" + strings.TrimPrefix(st.ActiveVersion, "v")
		}
	}

	switch {
	case w.providers.upgrading:
		status, tone = w.tr3("升级并验证中…", "Updating & verifying…", "更新と検証中…"), p.running
	case w.providers.checking:
		status, tone = w.tr3("沙箱兼容检查中…", "Checking & verifying…", "互換性確認中…"), p.muted
	case w.providers.busy:
		status, tone = w.tr3("检测中…", "Testing…", "テスト中…"), p.muted
	case w.providers.health != nil:
		if w.providers.health.OK {
			status, tone = w.tr3("已就绪", "Ready", "準備完了"), p.success
		} else if st != nil && st.ActiveSource != piprovider.RuntimeSourceMissing {
			status, tone = w.tr3("待配置模型", "Needs model setup", "モデル未設定"), p.warning
		} else {
			status, tone = w.tr3("未检测到", "Missing", "未検出"), p.danger
		}
		if w.providers.health.Detail != "" {
			tagline = w.providers.health.Detail
		}
	}

	settingsGroup(c, p, "provider-card:pi", func() {
		ui.Column(c).FillWidth().Children(func() {
			ui.Row(c).FillWidth().MinHeight(64).Padding(12, 12, 12, 14).Gap(12).AlignItems(ui.Center).Children(func() {
				providerMark(c, p, "π")
				ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
						ui.Text(c, "Pi").FontSize(13).FontWeight(600).TextColor(p.ink).NoWrap()
						kitBadge(c, badgeText, p.muted)
						if st != nil && st.NodeVersion != "" {
							kitBadge(c, "Node v"+strings.TrimPrefix(st.NodeVersion, "v"), p.faint)
						}
						settingsStatus(c, nil, status, tone)
					})
					ui.Text(c, tagline).FontSize(12).TextColor(p.muted).SingleLine().Ellipsis("…")
					if st != nil && st.ActivePath != "" {
						ui.Text(c, st.ActivePath).Font("Menlo").FontSize(10.5).TextColor(p.faint).SingleLine().Ellipsis("…").Tooltip(st.ActivePath)
					}
				})
				ui.Row(c).Gap(6).AlignItems(ui.Center).Shrink(0).Children(func() {
					if !w.defaults.RemotePeer {
						setupBtn := outlineButton(c, p, "settings-pi-setup", w.tr3("配置模型", "Setup model", "モデル設定"))
						if setupBtn.Clicked() {
							w.openPiSetup()
						}
						checkBtn := outlineButton(c, p, "settings-pi-check-update", w.tr3("检查更新", "Check update", "更新確認"))
						checkBtn.Disabled(w.providers.checking || w.providers.upgrading)
						if checkBtn.Clicked() {
							w.checkPiUpdate()
						}
						bannerHasUpgrade := st != nil && st.UpdateAvailable && st.CanUpgrade && st.CompatVerified
						canDirectUpgrade := st != nil && !st.CompatBlocked && !bannerHasUpgrade &&
							st.ActiveSource != piprovider.RuntimeSourceMissing &&
							st.NPMAvailable && st.SystemIsNPM &&
							(st.ActiveSource != piprovider.RuntimeSourceCustom || st.ActivePath == st.SystemPath)
						if canDirectUpgrade {
							upBtn := primaryAction(c, p, "settings-pi-upgrade-direct", w.tr3("升级 Pi", "Upgrade Pi", "Pi を更新"))
							upBtn.Disabled(w.providers.upgrading || w.providers.checking)
							if upBtn.Clicked() {
								w.upgradePi(false)
							}
						} else if st != nil && !st.CompatBlocked && st.ActiveSource == piprovider.RuntimeSourceMissing && st.NPMAvailable && !(st.CanInstall && st.CompatVerified) {
							installBtn := primaryAction(c, p, "settings-pi-install-direct", w.tr3("安装 Pi", "Install Pi", "Pi を導入"))
							installBtn.Disabled(w.providers.upgrading || w.providers.checking)
							if installBtn.Clicked() {
								w.upgradePi(true)
							}
						}
					}
					btn := outlineButton(c, p, "settings-pi-test", w.tr3("测试连接", "Test connection", "接続テスト"))
					btn.Disabled(w.providers.busy || w.providers.upgrading)
					if btn.Clicked() {
						w.probePi()
					}
				})
			})

			w.piUpdateBanner(c, p, st)

			if w.providers.output != "" {
				ui.Row(c).FillWidth().Padding(8, 14).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
					ui.Text(c, w.providers.output).FontSize(11.5).TextColor(p.success).Wrap()
				})
			}
			if w.providers.err != "" {
				ui.Row(c).FillWidth().Padding(8, 14).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
					ui.Text(c, w.providers.err).FontSize(11.5).TextColor(p.danger).Wrap()
				})
			}
		})
	})
}

func (w *workbench) piUpdateBanner(c *ui.Context, p palette, st *api.PiRuntimeStatus) {
	if st == nil || w.defaults.RemotePeer {
		return
	}

	// Only surface "升级 Pi" when all 4 safety gates passed and CompatVerified is true.
	if st.UpdateAvailable && st.CanUpgrade && st.CompatVerified {
		ui.Row(c).FillWidth().Padding(10, 14).Gap(10).AlignItems(ui.Center).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Background(p.soft).Children(func() {
			ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
					ui.Text(c, fmt.Sprintf(w.tr("发现兼容的 Pi 新版本 v%s", "Compatible Pi v%s available"), st.LatestVersion)).FontSize(12).FontWeight(600).TextColor(p.ink)
					kitBadge(c, w.tr("已通过 Vantry 兼容验证", "Verified zero-impact"), p.success)
				})
				if st.ResolvedRoute != "" {
					ui.Text(c, w.tr("更新通道：", "Registry route: ")+st.ResolvedRoute).FontSize(11).TextColor(p.muted).Wrap()
				}
			})
			upBtn := primaryAction(c, p, "settings-pi-upgrade", w.tr3("升级 Pi", "Upgrade Pi", "Pi を更新"))
			upBtn.Disabled(w.providers.upgrading || w.providers.checking)
			if upBtn.Clicked() {
				w.upgradePi(false)
			}
			if w.providers.upgrading {
				activityIndicator(c, p.accent)
			}
		})
		return
	}

	// Missing external Pi: allow verified one-click npm install, or copy install command.
	if st.ActiveSource == piprovider.RuntimeSourceMissing {
		if st.CanInstall && st.CompatVerified {
			ui.Row(c).FillWidth().Padding(10, 14).Gap(10).AlignItems(ui.Center).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Background(p.soft).Children(func() {
				ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
						ui.Text(c, fmt.Sprintf(w.tr("通过 npm 安装外置 Pi v%s", "Install external Pi v%s via npm"), st.LatestVersion)).FontSize(12).FontWeight(600).TextColor(p.ink)
						kitBadge(c, w.tr("已通过 Vantry 兼容验证", "Verified zero-impact"), p.success)
					})
					if st.ResolvedRoute != "" {
						ui.Text(c, w.tr("安装通道：", "Registry route: ")+st.ResolvedRoute).FontSize(11).TextColor(p.muted).Wrap()
					}
				})
				installBtn := primaryAction(c, p, "settings-pi-install", w.tr3("安装外置 Pi", "Install Pi", "Pi を導入"))
				installBtn.Disabled(w.providers.upgrading || w.providers.checking)
				if installBtn.Clicked() {
					w.upgradePi(true)
				}
			})
			return
		}
		if !st.CompatBlocked {
			ui.Row(c).FillWidth().Padding(10, 14).Gap(10).AlignItems(ui.Center).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Background(p.soft).Children(func() {
				ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
					if st.NPMAvailable {
						ui.Text(c, w.tr("检测到本机 npm，点击上方「检查更新」完成兼容校验后即可一键安装外置 Pi。", "Local npm detected; click Check Update above to verify and install external Pi.")).FontSize(11.5).TextColor(p.muted).Wrap()
					} else {
						ui.Text(c, w.tr("未检测到外置 Pi 或 npm。请先安装 Node.js (>=22)，再执行：", "External Pi and npm not found. Install Node.js (>=22), then run:")).FontSize(11.5).TextColor(p.muted).Wrap()
						ui.Text(c, "npm install -g "+piprovider.PiNPMPackage).Font("Menlo").FontSize(11).TextColor(p.ink).Selectable().Wrap()
					}
				})
				if !st.NPMAvailable {
					if outlineButton(c, p, "settings-pi-copy-cmd", w.tr("复制安装命令", "Copy install cmd")).Clicked() {
						c.WriteClipboard("npm install -g " + piprovider.PiNPMPackage)
						w.providers.output = w.tr("已复制外置 Pi 安装命令。", "Copied Pi install command.")
					}
				}
			})
			return
		}
	}

	// Compatibility gate blocked the candidate update or install: explain why and do NOT show upgrade button.
	if st.CompatBlocked && st.CompatReason != "" {
		ui.Row(c).FillWidth().Padding(10, 14).Gap(10).AlignItems(ui.Center).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Background(p.soft).Children(func() {
			ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
				headline := w.tr("暂不开放外置 Pi 升级（零影响保护）", "External Pi upgrade withheld (safety gate)")
				if st.LatestVersion != "" {
					headline = fmt.Sprintf(w.tr("暂不开放 Pi v%s 升级（零影响保护）", "Pi v%s upgrade withheld (safety gate)"), st.LatestVersion)
				}
				ui.Text(c, headline).FontSize(12).FontWeight(600).TextColor(p.warning).Wrap()
				ui.Text(c, st.CompatReason).FontSize(11.5).TextColor(p.muted).Wrap()
			})
		})
	}
}

func (w *workbench) piRuntimeCard(c *ui.Context, p palette) {
	st := w.providers.status
	currentMirror := piprovider.MirrorModeAuto
	routeText := w.tr("自动探测中…", "Detecting automatically…")
	agentDir := "~/.pi/agent"
	defaultModel := w.configuredDefaultModel()
	pathDesc := w.tr("自动探测本机 PATH / Homebrew / nvm 中的外置 Pi", "Auto-detecting external Pi in PATH / Homebrew / nvm")
	if st != nil {
		if st.MirrorMode != "" {
			currentMirror = st.MirrorMode
		}
		if st.ResolvedRoute != "" {
			routeText = st.ResolvedRoute
		}
		if st.AgentDir != "" {
			agentDir = st.AgentDir
		}
		if st.DefaultModel != "" {
			defaultModel = st.DefaultModel
		}
		if st.CustomPath != "" {
			pathDesc = w.tr("当前已固定为自定义路径：", "Pinned custom path: ") + st.CustomPath
		} else if st.SystemPath != "" {
			pathDesc = w.tr("当前已发现本机外置 Pi：", "Detected external Pi: ") + st.SystemPath
		}
	}

	settingsGroup(c, p, "provider-card:pi-runtime", func() {
		ui.Column(c).FillWidth().Children(func() {
			customBtnLabel := w.tr("自定义路径", "Custom path")
			if w.providers.showCustomPath {
				customBtnLabel = w.tr("收起", "Hide")
			}
			settingsRow(
				c, p, "settings-pi-path", true,
				w.tr3("外置 Pi 执行路径", "External Pi path", "外部 Pi パス"),
				pathDesc,
				func() {
					if !w.defaults.RemotePeer {
						if outlineButton(c, p, "settings-pi-path-toggle", customBtnLabel).Clicked() {
							w.providers.showCustomPath = !w.providers.showCustomPath
						}
					}
				},
			)

			if w.providers.showCustomPath && !w.defaults.RemotePeer {
				ui.Column(c).FillWidth().Padding(10, 16, 12).Gap(8).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Background(p.soft).Children(func() {
					ui.Field(c, w.tr("指定外置 pi 可执行文件（留空则自动探测本机）", "Custom pi executable path (leave empty for auto-detection)"), func() {
						ui.TextInput(c.Key("pi-custom-path-input"), &w.providers.customPiPath).Placeholder("/opt/homebrew/bin/pi").FillWidth()
					})
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						saveBtn := primaryAction(c, p, "pi-custom-path-save", w.tr("保存路径", "Save path"))
						saveBtn.Disabled(w.providers.saving)
						if saveBtn.Clicked() {
							w.savePiRuntimeSettings(api.PiRuntimeSettingsInput{
								CustomPath: strings.TrimSpace(w.providers.customPiPath),
							})
						}
						clearBtn := outlineButton(c, p, "pi-custom-path-clear", w.tr("恢复自动探测", "Use auto-detect"))
						clearBtn.Disabled(w.providers.saving)
						if clearBtn.Clicked() {
							w.providers.customPiPath = ""
							w.savePiRuntimeSettings(api.PiRuntimeSettingsInput{CustomPath: ""})
						}
					})
				})
			}

			settingsRow(
				c, p, "settings-pi-mirror", false,
				w.tr3("Pi npm 更新镜像", "Pi npm mirror", "Pi npm ミラー"),
				w.tr3("当前路线：", "Active route: ", "現在の経路：")+routeText,
				func() {
					mirrorIdx := 0
					switch currentMirror {
					case piprovider.MirrorModeCN:
						mirrorIdx = 1
					case piprovider.MirrorModeDirect:
						mirrorIdx = 2
					}
					seg := ui.Segmented(
						c.Key("settings-pi-mirror-seg"), &mirrorIdx,
						w.tr("自动识别", "Auto"),
						w.tr("国内镜像", "CN mirror"),
						w.tr("官方直连", "Direct"),
					).Label(w.tr("Pi npm 更新镜像", "Pi npm mirror"))
					if w.providers.saving || w.providers.upgrading || w.defaults.RemotePeer {
						seg.Disabled(true)
					}
					nextMirror := []string{piprovider.MirrorModeAuto, piprovider.MirrorModeCN, piprovider.MirrorModeDirect}[mirrorIdx]
					if nextMirror != currentMirror && !w.providers.saving && !w.defaults.RemotePeer {
						w.savePiRuntimeSettings(api.PiRuntimeSettingsInput{MirrorMode: nextMirror})
					}
					advLabel := w.tr("自定义", "Custom")
					if w.providers.showAdvancedMirror {
						advLabel = w.tr("收起", "Hide")
					}
					if quietButton(c, p, "settings-pi-mirror-adv", advLabel).Height(26).FontSize(11).Clicked() {
						w.providers.showAdvancedMirror = !w.providers.showAdvancedMirror
					}
				},
			)

			if w.providers.showAdvancedMirror && !w.defaults.RemotePeer {
				ui.Column(c).FillWidth().Padding(10, 16, 12).Gap(8).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Background(p.soft).Children(func() {
					ui.Field(c, w.tr("自定义 npm 镜像源（仅用于 Pi 查询、沙箱验证与升级）", "Custom npm registry (for Pi checks, preflight, and upgrades)"), func() {
						ui.TextInput(c.Key("pi-custom-registry"), &w.providers.customRegistry).Placeholder("https://registry.npmmirror.com").FillWidth()
					})
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						saveBtn := primaryAction(c, p, "pi-mirror-save", w.tr("保存镜像设置", "Save mirror settings"))
						saveBtn.Disabled(w.providers.saving)
						if saveBtn.Clicked() {
							w.savePiRuntimeSettings(api.PiRuntimeSettingsInput{
								NPMRegistry: strings.TrimSpace(w.providers.customRegistry),
							})
						}
						clearBtn := outlineButton(c, p, "pi-mirror-clear", w.tr("恢复默认", "Reset defaults"))
						clearBtn.Disabled(w.providers.saving)
						if clearBtn.Clicked() {
							w.providers.customRegistry = ""
							w.savePiRuntimeSettings(api.PiRuntimeSettingsInput{
								MirrorMode:  piprovider.MirrorModeAuto,
								NPMRegistry: "",
							})
						}
					})
				})
			}

			desc := agentDir
			if defaultModel != "" {
				desc += " · " + w.tr("默认模型：", "Default: ") + defaultModel
			} else {
				desc += " · " + w.tr("尚未设置默认模型", "No default model set")
			}
			settingsRow(
				c, p, "settings-pi-config-dir", false,
				w.tr3("共享配置目录", "Shared config directory", "共有設定ディレクトリ"),
				desc,
				func() {
					if !w.defaults.RemotePeer {
						if outlineButton(c, p, "settings-pi-open-setup", w.tr3("模型配置向导", "Model setup wizard", "設定ウィザード")).Clicked() {
							w.openPiSetup()
						}
					}
				},
			)
		})
	})
}

func (w *workbench) piCodemodeCard(c *ui.Context, p palette) {
	settingsGroup(c, p, "provider-card:pi-codemode", func() {
		ui.Column(c).FillWidth().Children(func() {
			ui.Row(c).FillWidth().MinHeight(52).Padding(12, 14).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
					ui.Text(c, w.tr3("运行形态：pi --mode rpc", "Integration: pi --mode rpc", "接続形態：pi --mode rpc")).FontSize(12.5).FontWeight(600).TextColor(p.ink)
					ui.Text(c, w.tr3("由分离的 Vantry worker 长期持有 JSONL 会话流，支持断点重连、事件审计、工作区隔离与实时 steer。", "Detached Vantry worker owns the JSONL stream with replay, workspace leases, and live steering.", "独立した Vantry worker が JSONL ストリームを管理し、再接続と live steer に対応します。")).FontSize(11.5).TextColor(p.muted).Wrap()
				})
			})
			ui.Row(c).FillWidth().MinHeight(52).Padding(12, 14).Gap(12).AlignItems(ui.Center).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
				ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
					ui.Text(c, w.tr3("MCP 模式：codemode（默认）", "MCP mode: codemode (default)", "MCP モード：codemode（既定）")).FontSize(12.5).FontWeight(600).TextColor(p.ink)
					ui.Text(c, w.tr3("启用 builtin:mcp + builtin:codemode + builtin:tool-search，避免全量工具 schema 膨胀上下文。", "Uses builtin:mcp + builtin:codemode + builtin:tool-search to avoid flooding context with schemas.", "builtin:mcp + codemode + tool_search を有効化し、コンテキスト肥大を防ぎます。")).FontSize(11.5).TextColor(p.muted).Wrap()
				})
				if outlineButton(c, p, "settings-open-mcp", w.tr3("管理 MCP", "Manage MCP", "MCP を管理")).Clicked() {
					w.widgets.settingsOpen = false
					w.openDock("mcp")
				}
			})
		})
	})
}
