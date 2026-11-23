package desktop

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

type forkDialogState struct {
	open        bool
	threadID    string
	runID       string
	model       string
	branch      string
	title       string
	useWorktree bool
}

func (w *workbench) tryBuiltinSlashCommand(threadID string, comp *composer, text string) bool {
	trimmed := strings.TrimSpace(text)
	switch {
	case trimmed == "/subagent" || strings.HasPrefix(trimmed, "/subagent ") || trimmed == "/delegate" || strings.HasPrefix(trimmed, "/delegate "):
		arg := trimmed
		if strings.HasPrefix(arg, "/subagent") {
			arg = strings.TrimSpace(strings.TrimPrefix(arg, "/subagent"))
		} else {
			arg = strings.TrimSpace(strings.TrimPrefix(arg, "/delegate"))
		}
		comp.draft = ""
		comp.mentions = nil
		comp.feedback = notice{}
		w.saveState()
		rootID := w.subagentPanelKey(threadID)
		if arg != "" {
			if !w.delegateMessage(rootID, arg, false) {
				comp.draft = text
				comp.feedback = notice{
					Text:  w.tr("请先发送一条消息建立会话后再派出子代理。", "Send a first message to create the thread before delegating a subagent."),
					Level: noticeWarn,
				}
			}
			return true
		}
		if items := w.subagentNavItems(rootID); len(items) > 0 {
			pick := items[len(items)-1].threadID
			for _, item := range items {
				if item.status == api.RunRunning || item.status == api.RunQueued {
					pick = item.threadID
					break
				}
			}
			w.openSubagentPanel(pick)
		} else {
			w.openSubagentCreator(rootID)
		}
		return true
	case trimmed == "/fork" || strings.HasPrefix(trimmed, "/fork "):
		arg := strings.TrimSpace(strings.TrimPrefix(trimmed, "/fork"))
		comp.draft = ""
		comp.mentions = nil
		comp.feedback = notice{}
		w.saveState()
		w.openForkDialog(threadID, "")
		if w.widgets.fork.open && arg != "" {
			w.widgets.fork.model = arg
		}
		return true
	case trimmed == "/rewind":
		comp.draft = ""
		comp.mentions = nil
		comp.feedback = notice{}
		w.saveState()
		w.openRewindDialog("")
		return true
	default:
		return false
	}
}

func (w *workbench) openForkDialog(threadID, runID string) {
	if threadID == "" || isLocalSession(threadID) || !w.hasThread(threadID) {
		w.setAction(w.tr("请先发送消息创建会话后再使用 /fork 分叉。", "Send a first message to create the thread before using /fork."))
		return
	}
	th, _ := w.threadByID(threadID)
	w.widgets.fork = forkDialogState{
		open:        true,
		threadID:    threadID,
		runID:       runID,
		model:       strings.TrimSpace(th.Model),
		useWorktree: th.Workspace != nil && th.Workspace.Kind == "worktree",
	}
	if !w.modelsLoaded && w.defaults.ProviderID != "controlled" {
		w.ensureModels(false)
	}
}

func (w *workbench) forkDialog(c *ui.Context, p palette) {
	if !w.widgets.fork.open {
		return
	}
	st := &w.widgets.fork
	th, ok := w.threadByID(st.threadID)
	if !ok {
		st.open = false
		return
	}
	canWorktree := th.Workspace != nil && (strings.TrimSpace(th.Workspace.Repository) != "" || strings.TrimSpace(th.Workspace.Path) != "")
	if !canWorktree {
		st.useWorktree = false
	}

	width, height := c.Size()
	ui.DialogBase(c, &st.open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000066"))
		panel.Width(min(540, width-32)).MaxHeight(min(620, height-40)).Padding(18, 20).Radius(14).Background(p.surface).Border(1, p.line)
		ui.Column(c).FillWidth().Gap(12).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, forkIcon).FontSize(15).TextColor(p.accentInk)
				ui.Text(c, w.tr("分叉会话（模型交接 / Worktree 隔离）", "Fork thread (model handoff / worktree)")).FontSize(14).Bold().TextColor(p.ink).Grow(1)
				if iconButton(c, p, "close-fork-dialog", w.tr("关闭", "Close"), closeIcon).Size(26, 26).Padding(5).Clicked() {
					st.open = false
				}
			})

			pointDesc := fmt.Sprintf(w.tr("来源会话：%s（当前完整上下文）", "Source: %s (latest turn)"), w.displayTitleFor(st.threadID))
			if st.runID != "" {
				pointDesc = fmt.Sprintf(w.tr("来源会话：%s（截至本轮）", "Source: %s (up to selected turn)"), w.displayTitleFor(st.threadID))
			}
			ui.Text(c, pointDesc).FontSize(12).TextColor(p.muted).Wrap()

			ui.Column(c).Gap(6).FillWidth().Children(func() {
				ui.Text(c, w.tr("模型交接", "Target model")).FontSize(12).FontWeight(600).TextColor(p.ink)
				ui.Row(c).Gap(6).Wrap().FillWidth().Children(func() {
					keepBtn := quietButton(c, p, "fork-model-keep", w.tr("保持原模型", "Keep current")).Height(26).Padding(0, 10).FontSize(11.5).Radius(7)
					if st.model == "" || st.model == th.Model {
						keepBtn.Background(p.selected).TextColor(p.ink).Border(1, p.line)
					}
					if keepBtn.Clicked() {
						st.model = th.Model
					}
					limit := min(len(w.models), 12)
					for i := 0; i < limit; i++ {
						m := w.models[i]
						key := m.Key()
						label := nonEmptyString(m.Name, key)
						btn := quietButton(c, p, "fork-model:"+key, label).Height(26).Padding(0, 10).FontSize(11.5).Radius(7)
						if st.model == key {
							btn.Background(p.selected).TextColor(p.accentInk).Border(1, p.accent.Alpha(0.5))
						}
						if btn.Clicked() {
							st.model = key
						}
					}
				})
				ui.TextInput(c.Key("fork-custom-model"), &st.model).
					Label(w.tr("模型标识", "Model ID")).
					Placeholder(w.tr("可选：直接输入 provider/model 覆盖默认模型…", "Optional: enter provider/model ID…")).
					FillWidth()
			})

			ui.Column(c).Gap(6).FillWidth().Children(func() {
				wtRow := ui.Button(c.Key("fork-worktree-toggle"), "").Label(w.tr("在独立 Git Worktree 中分叉", "Fork in isolated Git worktree")).
					Role(ui.RoleSwitch).Checked(st.useWorktree).Disabled(!canWorktree).
					FillWidth().Padding(8, 10).Radius(9).Border(1, p.lineSoft).Background(p.soft).Justify(ui.Start)
				if st.useWorktree {
					wtRow.BorderColor(p.accent.Alpha(0.5)).Background(p.accent.Alpha(0.08))
				}
				wtRow.Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).FillWidth().Children(func() {
						ui.Icon(c, branchIcon).FontSize(13).TextColor(p.muted)
						ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
							ui.Text(c, w.tr("在独立 Git Worktree 中隔离改动", "Isolate edits in a new Git worktree")).FontSize(12).FontWeight(600).TextColor(p.ink)
							sub := w.tr("为分叉会话创建独立工作树与分支，不干扰当前工作目录", "Creates a clean branch & checkout so experiments stay isolated")
							if !canWorktree {
								sub = w.tr("当前会话未绑定 Git 项目目录，无法创建 Worktree", "Requires a project workspace to create a worktree")
							}
							ui.Text(c, sub).FontSize(11).TextColor(p.muted).Wrap()
						})
						stateTxt := w.tr("关", "Off")
						if st.useWorktree {
							stateTxt = w.tr("开", "On")
						}
						ui.Text(c, stateTxt).FontSize(11).FontWeight(600).TextColor(p.accentInk)
					})
				})
				if wtRow.Clicked() && canWorktree {
					st.useWorktree = !st.useWorktree
				}
				if st.useWorktree {
					ui.TextInput(c.Key("fork-branch"), &st.branch).
						Label(w.tr("分叉分支名（可选）", "Worktree branch (optional)")).
						Placeholder(w.tr("留空则自动生成 vantry/… 分支名", "Leave blank to auto-generate a branch name")).
						FillWidth()
				}
			})

			ui.TextInput(c.Key("fork-title"), &st.title).
				Label(w.tr("新会话标题（可选）", "New thread title (optional)")).
				Placeholder(w.tr("留空则继承原会话标题", "Leave blank to inherit parent title")).
				FillWidth()

			ui.Row(c).Gap(8).Justify(ui.End).FillWidth().Children(func() {
				if quietButton(c, p, "cancel-fork", w.tr("取消", "Cancel")).Height(30).Padding(0, 12).Clicked() {
					st.open = false
				}
				confirm := ui.PrimaryButton(c.Key("confirm-fork"), w.tr("创建分叉会话", "Fork thread")).Height(30).Padding(0, 14)
				if confirm.Clicked() {
					opts := forkOptions{
						ThreadID:    st.threadID,
						RunID:       st.runID,
						Model:       st.model,
						Title:       st.title,
						Branch:      st.branch,
						UseWorktree: st.useWorktree,
					}
					st.open = false
					w.forkThreadWithOptions(opts)
				}
			})
		})
	})
}
