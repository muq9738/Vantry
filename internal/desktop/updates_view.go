package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	actionUpdateCheck   = "update.check"
	actionUpdateInstall = "update.install"
	actionUpdateApply   = "update.apply"
)

// updateState is the GitHub incremental self-update shown in Settings.
type updateState struct {
	info   *api.AppUpdateInfo
	err    string
	output string
	// autoCheckedAt is when the quiet background check last ran; it only
	// feeds the sidebar's small update badge and never surfaces errors.
	autoCheckedAt time.Time
}

// updateBadgeIcon marks the sidebar's "new version" button.
var updateBadgeIcon = lucide("download")

// updateAutoCheckInterval spaces the quiet background checks.
const updateAutoCheckInterval = 6 * time.Hour

// maybeAutoCheckUpdates runs a silent update check at startup and then every
// few hours, so the sidebar can show an update badge only when one exists.
func (w *workbench) maybeAutoCheckUpdates(now time.Time) {
	if w.defaults.RemotePeer || strings.TrimSpace(w.defaults.Version) == "" || w.features.busy["update-check"] {
		return
	}
	if _, ok := w.service.(api.WorkbenchService); !ok {
		return
	}
	if !w.updates.autoCheckedAt.IsZero() && now.Sub(w.updates.autoCheckedAt) < updateAutoCheckInterval {
		return
	}
	w.updates.autoCheckedAt = now
	w.workbenchCall("update-check", api.WorkbenchRequest{Action: actionUpdateCheck}, func(response api.WorkbenchResponse, err error) {
		if err != nil || response.Update == nil {
			return // quiet: the explicit check in Settings reports failures
		}
		w.updates.info = response.Update
	})
}

// updateBadgeVisible is true only when a newer version is ready to install.
func (w *workbench) updateBadgeVisible() bool {
	info := w.updates.info
	return info != nil && info.Available && strings.TrimSpace(info.LatestVersion) != ""
}

func (w *workbench) checkForUpdates() {
	w.workbenchCall("update-check", api.WorkbenchRequest{Action: actionUpdateCheck}, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.updates.err = describeUpdateError(w.tr("检查更新失败", "Checking for updates failed"), err)
			return
		}
		w.updates.err = ""
		if response.Update != nil {
			w.updates.info = response.Update
			if !response.Update.Staged {
				w.updates.output = ""
			}
		} else {
			w.updates.info = &api.AppUpdateInfo{CurrentVersion: w.defaults.Version}
			w.updates.output = ""
		}
	})
}

func (w *workbench) installUpdate() {
	info := w.updates.info
	if info == nil || !info.Available {
		return
	}
	w.workbenchCall("update-install", api.WorkbenchRequest{Action: actionUpdateInstall, Name: info.LatestVersion}, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.updates.err = describeUpdateError(w.tr("更新未完成", "The update did not finish"), err)
			return
		}
		w.updates.err = ""
		if response.Update != nil {
			w.updates.info = response.Update
		}
		if response.Update != nil && response.Update.Staged {
			w.updates.output = ""
		} else {
			w.updates.output = nonEmptyString(strings.TrimSpace(response.Output), w.tr("更新已下载，请退出 Vantry 后安装。", "Update downloaded; quit Vantry before installing."))
		}
	})
}

func (w *workbench) applyUpdate() {
	info := w.updates.info
	if info == nil || !info.Staged || !info.CanAutoRestart || w.defaults.RemotePeer {
		return
	}
	payload, _ := json.Marshal(struct {
		DesktopPID int `json:"desktopPid"`
	}{DesktopPID: os.Getpid()})
	w.workbenchCall("update-apply", api.WorkbenchRequest{
		Action: actionUpdateApply,
		Name:   info.LatestVersion,
		Input:  payload,
	}, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.updates.err = describeUpdateError(w.tr("无法重启更新", "Restart to update failed"), err)
			return
		}
		w.updates.err = ""
		if response.Update != nil {
			w.updates.info = response.Update
		}
		w.updates.output = nonEmptyString(strings.TrimSpace(response.Output), w.tr("正在退出并安装更新…", "Restarting to install update…"))
		if w.quit != nil {
			w.quit()
		}
	})
}

// Update operations return actionable configuration and download errors from
// Core. Keep those messages visible instead of replacing them with the generic
// invalid-command explanation used by ordinary workbench actions.
func describeUpdateError(action string, err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Code == api.InvalidCommand && strings.TrimSpace(apiErr.Message) != "" {
		return action + "：" + apiErr.Message
	}
	return describe(action, err)
}

func versionLabel(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "—"
	}
	return "v" + strings.TrimPrefix(version, "v")
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (w *workbench) updateSection(c *ui.Context, p palette) {
	info := w.updates.info
	current := w.defaults.Version
	if info != nil && info.CurrentVersion != "" {
		current = info.CurrentVersion
	}
	settingsGroup(c, p, "updates", func() {
		settingsRow(c, p, "update-version", true, "Vantry "+versionLabel(current), w.tr("优先增量下载，点击重启即可安全更新", "Delta download first; restart to apply safely"), func() {
			check := outlineButton(c, p, "update-check", w.tr("检查更新", "Check for Updates"))
			if w.features.busy["update-check"] {
				check.Disabled(true)
			}
			if check.Clicked() {
				w.checkForUpdates()
			}
		})
		ui.Column(c).FillWidth().Padding(0, 16, 14).Gap(10).Children(func() { w.updateDetails(c, p, info) })
	})
}

func (w *workbench) updateDetails(c *ui.Context, p palette, info *api.AppUpdateInfo) {
	if w.features.busy["update-check"] {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			activityIndicator(c, p.accent)
			ui.Text(c, w.tr("正在检查 GitHub Releases…", "Checking GitHub Releases…")).FontSize(11).TextColor(p.muted)
		})
	}
	if info != nil && !info.Available && !w.features.busy["update-check"] {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, checkIcon).FontSize(12).TextColor(p.success)
			ui.Text(c, w.tr("已是最新版本", "You're up to date")).FontSize(11).TextColor(p.muted)
		})
	}
	if info != nil && info.Available {
		staged := info.Staged && (info.StagedVersion == "" || strings.TrimPrefix(info.StagedVersion, "v") == strings.TrimPrefix(info.LatestVersion, "v"))
		ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
			ui.Text(c, w.tr("新版本 ", "New version ")+versionLabel(info.LatestVersion)).FontSize(12).Bold().TextColor(p.ink)
			if info.DeltaAvailable && info.DeltaSize > 0 {
				ui.Row(c).Padding(2, 6).Radius(4).Background(p.accent.Alpha(0.14)).Children(func() {
					ui.Text(c, w.tr("增量更新", "Delta")).FontSize(10).FontWeight(500).TextColor(p.accent)
				})
				ui.Text(c, byteSize(info.DeltaSize)).Font("Menlo").FontSize(10).TextColor(p.ink)
				if info.AssetSize > 0 {
					ui.Text(c, w.tr("完整 ", "Full ")+byteSize(info.AssetSize)).Font("Menlo").FontSize(10).TextColor(p.faint)
				}
			} else if info.AssetSize > 0 {
				ui.Text(c, byteSize(info.AssetSize)).Font("Menlo").FontSize(10).TextColor(p.faint)
			}
			if !info.PublishedAt.IsZero() {
				ui.Text(c, info.PublishedAt.Local().Format("2006-01-02")).FontSize(10).TextColor(p.faint)
			}
		})
		if strings.TrimSpace(info.Notes) != "" {
			ui.Scroll(c.Key("update-notes")).FillWidth().MaxHeight(180).Radius(8).Background(p.surface).Border(1, p.lineSoft).Children(func() {
				ui.Column(c).FillWidth().Padding(8, 12).Children(func() {
					markdown.View(c, info.Notes, w.replyMarkdown(w.selected))
				})
			})
		}
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
			switch {
			case !staged:
				label := w.tr("下载更新", "Download Update")
				if info.DeltaAvailable && info.DeltaSize > 0 {
					label = w.tr("下载增量更新", "Download Delta Update")
				}
				install := ui.PrimaryButton(c.Key("update-install"), label).Label(label).Height(30).Padding(0, 14)
				if w.features.busy["update-install"] {
					install.Disabled(true)
				}
				if install.Clicked() {
					w.installUpdate()
				}
				if w.features.busy["update-install"] {
					activityIndicator(c, p.accent)
				}
			case info.CanAutoRestart && !w.defaults.RemotePeer:
				label := w.tr("重启并更新", "Restart & Update")
				apply := ui.PrimaryButton(c.Key("update-apply"), label).Label(label).Height(30).Padding(0, 14)
				if w.features.busy["update-apply"] {
					apply.Disabled(true)
				}
				if apply.Clicked() {
					w.applyUpdate()
				}
				if w.features.busy["update-apply"] {
					activityIndicator(c, p.accent)
				}
				if info.InstallerPath != "" {
					if quietButton(c, p, "update-copy-cmd", w.tr("复制安装命令", "Copy install command")).Height(26).FontSize(11).Clicked() {
						c.WriteClipboard(info.InstallerPath)
						w.updates.output = w.tr("已复制安装命令路径。", "Install command path copied.")
					}
				}
			default:
				copyTarget := nonEmptyString(info.InstallerPath, info.StagedPath)
				if copyTarget != "" {
					copyLabel := w.tr("复制路径", "Copy path")
					if info.InstallerPath != "" {
						copyLabel = w.tr("复制安装命令", "Copy install command")
					}
					if quietButton(c, p, "update-copy-cmd", copyLabel).Height(26).FontSize(11).Clicked() {
						c.WriteClipboard(copyTarget)
						w.updates.output = w.tr("已复制更新路径。", "Update path copied.")
					}
				}
			}
			if info.ReleaseURL != "" {
				if quietButton(c, p, "update-release", w.tr("复制发布页链接", "Copy release link")).Height(26).FontSize(11).Clicked() {
					c.WriteClipboard(info.ReleaseURL)
				}
			}
		})
		if staged {
			modeTag := w.tr("（完整包）", " (full package)")
			if info.UsedDelta {
				modeTag = w.tr("（增量更新）", " (delta)")
			}
			summary := versionLabel(info.LatestVersion) + w.tr(" 已下载并完成校验", " downloaded and verified") + modeTag
			hint := w.tr("当前应用尚未替换，请先退出 Vantry 后手动安装。", "The running app has not been replaced yet; quit Vantry before installing manually.")
			if info.CanAutoRestart && !w.defaults.RemotePeer {
				hint = w.tr("点击「重启并更新」将安全退出当前窗口与空闲 Core，替换应用并自动重启；若 Core 仍有活动任务会拒绝安装。", "Click \"Restart & Update\" to quit cleanly, drain idle Core, replace the app, and relaunch. Active Core tasks will block replacement.")
			}
			ui.Column(c).FillWidth().MinWidth(0).Padding(10, 12).Gap(4).Radius(8).Background(p.surface).Border(1, p.lineSoft).Children(func() {
				ui.Text(c, summary).FontSize(11).FontWeight(500).TextColor(p.success).FillWidth().Wrap()
				ui.Text(c, hint).FontSize(11).TextColor(p.muted).FillWidth().Wrap()
				if !info.CanAutoRestart || w.defaults.RemotePeer {
					if path := nonEmptyString(info.InstallerPath, info.StagedPath); path != "" {
						ui.Text(c, path).Font("Menlo").FontSize(10).TextColor(p.faint).FillWidth().MinWidth(0).SingleLine().Ellipsis("…").Tooltip(path)
					}
				}
			})
		}
	}
	if w.updates.output != "" {
		ui.Column(c).FillWidth().MinWidth(0).Children(func() {
			ui.Text(c, w.updates.output).FontSize(11).TextColor(p.success).FillWidth().Wrap()
		})
	}
	if w.updates.err != "" {
		ui.Column(c).FillWidth().MinWidth(0).Children(func() {
			ui.Text(c, w.updates.err).FontSize(11).TextColor(p.danger).FillWidth().Wrap()
		})
	}
}
