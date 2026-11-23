package desktop

import (
	"fmt"
	"reflect"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

type projectChoice struct {
	mode               int
	path, branch, base string
}

func (w *workbench) beginProjectChoice() {
	spec := w.defaults.Workspace
	if thread, ok := w.selectedThread(); ok {
		spec = thread.Workspace
	}
	w.projectChoice = projectChoice{}
	if spec != nil {
		if spec.Kind == "worktree" || spec.Repository != "" {
			w.projectChoice = projectChoice{mode: 1, path: spec.Repository, base: spec.BaseRef}
		} else {
			w.projectChoice.path = spec.Path
		}
	}
}

// currentWorkspace is the selected thread's workspace, or the draft default.
func (w *workbench) currentWorkspace() *api.WorkspaceSpec {
	if thread, ok := w.selectedThread(); ok {
		return thread.Workspace
	}
	return w.defaults.Workspace
}

// chatColumnWidth returns the actual horizontal room available to the chat
// column after accounting for the sidebar and any open side dock or inline workspace.
func (w *workbench) chatColumnWidth(c *ui.Context) float32 {
	winW, _ := c.Size()
	narrow := winW < narrowWidth
	main := w.mainStageWidth(c, narrow)
	switch {
	case w.dockMode(c, narrow) == dockSide:
		return max(float32(chatMinWidth), main-w.dockWidth(main)-5)
	case w.widgets.workspaceOpen && w.canShowInlineWorkspace(c, narrow):
		return max(float32(chatMinWidth), main-300)
	default:
		return main
	}
}

// contextStrip is the workbench's BranchToolbar under the composer card: the machine
// and the project with its mode (current checkout or isolated worktree) on
// the left, opening the project picker; a PR mark, the branch (opening Git)
// and the tool permission chip on the right.
func (w *workbench) contextStrip(c *ui.Context, p palette) {
	spec := w.defaults.Workspace
	if thread, ok := w.selectedThread(); ok {
		spec = thread.Workspace
	}
	width := w.chatColumnWidth(c)
	chatWide := width >= 700
	proxy := w.proxyEnvironmentID() != ""
	ui.Row(c.Key("context-strip")).Height(26).Margin(6, 0, 0).Padding(0, 8).Gap(2).AlignItems(ui.Center).Children(func() {
		if proxy {
			ui.Row(c.Key("proxy-environment")).Label(w.tr("远端执行环境 ", "Remote execution environment ")+w.proxyEnvironmentLabel()).Height(22).Padding(0, 6).Gap(5).AlignItems(ui.Center).Shrink(0).Children(func() {
				ui.Icon(c, serverIcon).FontSize(12).TextColor(p.accentInk)
				ui.Text(c, w.proxyEnvironmentLabel()).FontSize(11).TextColor(p.accentInk).SingleLine().Ellipsis("…").MaxWidth(min(float32(160), width*0.22))
			})
			ui.Box(c).Size(1, 12).Background(p.lineSoft).Margin(0, 4).Shrink(0)
		} else if chatWide && w.environments != nil {
			w.environmentMenu(c, p, true)
			ui.Box(c).Size(1, 12).Background(p.lineSoft).Margin(0, 4).Shrink(0)
		} else if chatWide {
			ui.Row(c.Key("context-host")).Height(22).Padding(0, 6).Gap(5).AlignItems(ui.Center).MinWidth(0).Shrink(1).Label(w.tr("运行机器 ", "Machine ") + w.hostLabel()).Children(func() {
				ui.Icon(c, serverIcon).FontSize(12).TextColor(p.faint)
				ui.Text(c, w.hostLabel()).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…").MaxWidth(min(float32(180), width*0.18))
			})
			ui.Box(c).Size(1, 12).Background(p.lineSoft).Margin(0, 4).Shrink(0)
		}
		worktree := spec != nil && (spec.Kind == "worktree" || spec.Repository != "")
		icon, mode := folderIcon, w.tr("本地目录", "Local")
		if worktree {
			icon, mode = worktreeIcon, w.tr("隔离分支", "Worktree")
		}
		if proxy {
			mode = w.tr("远端工作区", "Remote workspace")
		}
		name := w.workspaceLabel()
		project := quietButton(c, p, "project-picker", "").Label(w.tr("项目与工作区", "Project & workspace")).Height(22).Padding(0, 6).Radius(6).
			MinWidth(0).Shrink(1).Expanded(w.widgets.projectPickerOpen)
		if spec != nil {
			project.Tooltip(nonEmptyString(spec.Path, spec.Repository))
			project.Description(name + " · " + mode)
		} else {
			project.Description(w.tr("不选项目也可直接聊天；操作项目文件时请先选择项目", "Chat works without a project; choose one to work with files"))
		}
		if proxy {
			project.Disabled(true).Description(w.tr("远端任务代理；请切到目标机器操作其项目文件。", "Remote task proxy; open the target environment to work with its files."))
		}
		project.Children(func() {
			ui.Icon(c, icon).FontSize(12).TextColor(p.faint)
			ui.Text(c, name).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…").MaxWidth(min(float32(200), width*0.32))
			if spec != nil && chatWide {
				ui.Text(c, "·").FontSize(11).TextColor(p.faint)
				ui.Text(c, mode).FontSize(11).TextColor(p.faint).NoWrap()
			}
		})
		if project.Clicked() {
			w.widgets.projectPickerOpen = !w.widgets.projectPickerOpen
			if w.widgets.projectPickerOpen {
				w.beginProjectChoice()
				w.widgets.modelPickerOpen = false
			}
		}
		ui.Spacer(c)
		if pr := w.gitView.pr[w.selected]; pr != nil && pr.Number > 0 {
			_, tone := w.prChecksTone(p, pr.Checks)
			ui.Row(c).Height(18).Padding(0, 6).Gap(3).Radius(9).AlignItems(ui.Center).Background(tone.Alpha(0.12)).Shrink(0).Children(func() {
				ui.Icon(c, prIcon).FontSize(11).TextColor(tone)
				ui.Text(c, fmt.Sprintf("PR #%d", pr.Number)).FontSize(11).TextColor(tone).NoWrap()
			})
		}
		branch := w.selectedBranch()
		if branch == "" && spec != nil {
			branch = spec.Branch
		}
		if branch != "" {
			w.footerBranchPill(c, p, branch, width)
		}
	})
}

func (w *workbench) configureWorkspaceSpec(spec *api.WorkspaceSpec) {
	current := w.defaults.Workspace
	if thread, ok := w.selectedThread(); ok {
		current = thread.Workspace
	}
	if reflect.DeepEqual(current, spec) {
		return
	}
	if !isLocalSession(w.selected) {
		comp := w.composerFor(w.selected)
		draft := comp.draft
		attachments := append([]api.Attachment(nil), comp.attachments...)
		mentions := append([]composerMention(nil), comp.mentions...)
		pasteSeq := comp.pasteSeq
		w.newSession()
		nextComp := w.composerFor(w.selected)
		nextComp.attachments = attachments
		nextComp.mentions = mentions
		nextComp.pasteSeq = pasteSeq
		w.setDraft(w.selected, draft)
	}
	w.defaults.Workspace = cloneWorkspace(spec)
	w.invalidateModels()
	w.saveState()
}
