package desktop

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	actionGitBranchUpdate = "git.branch.update"
	actionGitBranchRename = "git.branch.rename"
	actionGitBranchDelete = "git.branch.delete"
	actionGitRebase       = "git.rebase"
	actionGitCompare      = "git.compare"
)

type branchTreeNode struct {
	name     string // leaf segment or folder name
	fullName string // full branch name for leaf nodes
	isFolder bool
	children []branchTreeNode
}

// buildPopoverBranchTree groups slash-separated branch names (e.g. feature/a, feature/b)
// into collapsible folders matching JetBrains IDEA's branch widget.
func buildPopoverBranchTree(branches []string) []branchTreeNode {
	type dirBucket struct {
		leaves  []branchTreeNode
		folders map[string]*dirBucket
		order   []string
	}
	root := &dirBucket{folders: map[string]*dirBucket{}}
	for _, full := range branches {
		full = strings.TrimSpace(full)
		if full == "" {
			continue
		}
		parts := strings.Split(full, "/")
		cur := root
		for i := 0; i < len(parts)-1; i++ {
			seg := parts[i]
			next, ok := cur.folders[seg]
			if !ok {
				next = &dirBucket{folders: map[string]*dirBucket{}}
				cur.folders[seg] = next
				cur.order = append(cur.order, seg)
			}
			cur = next
		}
		cur.leaves = append(cur.leaves, branchTreeNode{
			name:     parts[len(parts)-1],
			fullName: full,
		})
	}
	var flatten func(b *dirBucket) []branchTreeNode
	flatten = func(b *dirBucket) []branchTreeNode {
		sort.Strings(b.order)
		out := make([]branchTreeNode, 0, len(b.leaves)+len(b.order))
		out = append(out, b.leaves...)
		for _, folder := range b.order {
			out = append(out, branchTreeNode{
				name:     folder,
				isFolder: true,
				children: flatten(b.folders[folder]),
			})
		}
		return out
	}
	return flatten(root)
}

func (w *workbench) recordRecentBranch(target, branch string) {
	branch = strings.TrimSpace(branch)
	if target == "" || branch == "" {
		return
	}
	if w.gitView.recentBranches == nil {
		w.gitView.recentBranches = map[string][]string{}
	}
	prev := w.gitView.recentBranches[target]
	next := []string{branch}
	for _, b := range prev {
		if b != branch && len(next) < 5 {
			next = append(next, b)
		}
	}
	w.gitView.recentBranches[target] = next
}

func (w *workbench) recentBranchesFor(target string, state *api.GitState) []string {
	if state == nil {
		return nil
	}
	available := map[string]bool{}
	for _, b := range state.Branches {
		available[b] = true
	}
	var out []string
	add := func(b string) {
		if b == "" || slices.Contains(out, b) {
			return
		}
		if b == state.Branch || available[b] {
			out = append(out, b)
		}
	}
	add(state.Branch)
	for _, b := range w.gitView.recentBranches[target] {
		add(b)
	}
	// Seed with common trunk branches when no switching history yet so Recent feels helpful.
	for _, trunk := range []string{"main", "master", "develop", "release-main"} {
		if len(out) >= 4 {
			break
		}
		add(trunk)
	}
	return out
}

func (w *workbench) gitUpdateBranch(branch string) bool {
	w.gitView.branchOpen = false
	return w.gitAction(actionGitBranchUpdate, w.tr("更新分支 ", "Update ")+branch, func(r *api.WorkbenchRequest) {
		r.Branch = branch
		r.Name = branch
	}, nil)
}

func (w *workbench) gitMergeNamedBranch(branch string) {
	if w.features.gitBranches == nil {
		w.features.gitBranches = map[string]string{}
	}
	w.features.gitBranches[w.selected] = branch
	w.gitView.branchOpen = false
	w.mergeBranch()
}

func (w *workbench) gitRebaseBranch(targetBranch string) bool {
	w.gitView.branchOpen = false
	return w.gitAction(actionGitRebase, w.tr("变基到 ", "Rebase onto ")+targetBranch, func(r *api.WorkbenchRequest) {
		r.Name = targetBranch
		r.Branch = targetBranch
	}, nil)
}

func (w *workbench) gitRenameBranch(oldName, newName string) bool {
	newName = strings.TrimSpace(newName)
	if newName == "" || strings.ContainsAny(newName, " \t\r\n~^:?*[\\") || strings.HasPrefix(newName, "-") {
		w.setToolFeedback("git:"+w.selected, notice{Text: w.tr("请输入有效的新分支名。", "Enter a valid branch name."), Level: noticeWarn})
		return false
	}
	return w.gitAction(actionGitBranchRename, w.tr("重命名分支 ", "Rename ")+oldName, func(r *api.WorkbenchRequest) {
		r.Branch = oldName
		r.Name = newName
	}, func() {
		w.gitView.branchRenaming = ""
		w.gitView.branchRenameDraft = ""
		w.recordRecentBranch(w.selected, newName)
	})
}

func (w *workbench) gitDeleteBranch(branch string, force bool) bool {
	return w.gitAction(actionGitBranchDelete, w.tr("删除分支 ", "Delete ")+branch, func(r *api.WorkbenchRequest) {
		r.Branch = branch
		r.Name = branch
		r.Amend = force
	}, func() {
		w.gitView.branchDeleteConfirm = ""
		if w.gitView.branchExpanded == branch {
			w.gitView.branchExpanded = ""
		}
	})
}

func (w *workbench) toggleBranchPopover(anchor string) {
	if w.gitView.branchOpen && w.gitView.branchAnchor == anchor {
		w.gitView.branchOpen = false
		return
	}
	w.gitView.branchAnchor = anchor
	w.gitView.branchOpen = true
	w.gitView.branchExpanded = ""
	w.gitView.branchRenaming = ""
	w.gitView.branchDeleteConfirm = ""
	if w.features.git[w.selected] == nil && !w.features.busy["git:"+w.selected] {
		w.refreshGit(w.selected, false)
	}
}

// headerShowsBranchPill reports whether the top-left header currently renders
// the IDEA-style branch selector beside the project name.
func (w *workbench) headerShowsBranchPill(narrow bool) bool {
	if narrow || w.selectedWorkspace() == nil {
		return false
	}
	if ok, _ := w.toolAvailable("git"); !ok {
		return false
	}
	return w.currentBranchName() != ""
}

func (w *workbench) currentBranchName() string {
	if state := w.features.git[w.selected]; state != nil && state.Branch != "" {
		return state.Branch
	}
	if branch := w.selectedBranch(); branch != "" {
		return branch
	}
	if spec := w.selectedWorkspace(); spec != nil && spec.Branch != "" {
		return spec.Branch
	}
	return ""
}

// headerBranchPill sits immediately beside the project name in Vantry's top-left
// header, mirroring IntelliJ IDEA's Project + Branch widget while keeping the
// rest of the conversation canvas clean.
func (w *workbench) headerBranchPill(c *ui.Context, p palette, narrow bool) {
	if !w.headerShowsBranchPill(narrow) {
		return
	}
	branch := w.currentBranchName()
	state := w.features.git[w.selected]
	_, active := w.activeRun(w.selected)
	busy := w.features.busy["git:"+w.selected]
	w.ideaBranchTrigger(c, p, "header", branch, state, active || busy, false, false, ui.AnchorBottomStart, ui.AnchorTopStart)
}

// footerBranchPill renders the branch trigger in the composer's bottom context
// strip and opens the same IDEA branch popover upward.
func (w *workbench) footerBranchPill(c *ui.Context, p palette, branch string, width float32) {
	state := w.features.git[w.selected]
	_, active := w.activeRun(w.selected)
	busy := w.features.busy["git:"+w.selected]
	disabled := false
	if ok, _ := w.toolAvailable("git"); !ok {
		disabled = true
	}
	openHere := w.gitView.branchOpen && w.gitView.branchAnchor == "footer"
	pill := quietButton(c, p, "context-git", "").Label(w.tr("分支 ", "Branch ")+branch).Height(22).Padding(0, 6).Radius(6).MinWidth(0).Shrink(1).Expanded(openHere)
	pill.Tooltip(w.tr("切换、更新、合并或比较分支 (⌃⇧B)", "Switch, update, merge, or compare branches (⌃⇧B)"))
	if disabled {
		pill.Disabled(true)
	}
	pill.Children(func() {
		ui.Icon(c, branchIcon).FontSize(11).TextColor(p.faint)
		ui.Text(c, branch).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…").MaxWidth(min(float32(180), width*0.2))
		if state != nil && state.Ahead > 0 {
			ui.Text(c, fmt.Sprintf("↑%d", state.Ahead)).Font("Menlo").FontSize(10).TextColor(p.success).NoWrap()
		}
		if state != nil && state.Behind > 0 {
			ui.Text(c, fmt.Sprintf("↓%d", state.Behind)).Font("Menlo").FontSize(10).TextColor(p.warning).NoWrap()
		}
	})
	if pill.Clicked() {
		w.toggleBranchPopover("footer")
	}
	if w.gitView.branchAnchor == "footer" {
		w.mountIdeaBranchPopover(c, p, pill, state, active || busy, ui.AnchorTopEnd, ui.AnchorBottomEnd)
	}
}

func (w *workbench) ideaBranchTrigger(c *ui.Context, p palette, anchor, branch string, state *api.GitState, locked, compact, narrow bool, at, self ui.Anchor) {
	maxBranchW := float32(168)
	if narrow {
		maxBranchW = 96
	} else if compact {
		maxBranchW = 128
	}
	openHere := w.gitView.branchOpen && w.gitView.branchAnchor == anchor
	pill := ui.Button(c.Key("git-branch-pill:"+anchor), "").Label(w.tr("分支：", "Branch: ")+branch).Height(26).Padding(0, 8).Radius(7).
		Background(p.soft).Border(1, p.lineSoft).TextColor(p.ink).Shrink(1).MinWidth(0).Expanded(openHere)
	if openHere {
		pill.Background(p.selected).BorderColor(p.line)
	} else if pill.Hovered() {
		pill.Background(p.rowHover).BorderColor(p.line)
	}
	tip := branch
	if state != nil && state.RemoteBranch != "" {
		tip = branch + " → " + state.RemoteBranch
	}
	pill.Tooltip(tip + " · " + w.tr("分支与 Git 操作 (⌃⇧B)", "Branches & Git actions (⌃⇧B)"))
	pill.Children(func() {
		ui.Row(c).Gap(5).AlignItems(ui.Center).MinWidth(0).Children(func() {
			ui.Icon(c, branchIcon).FontSize(12).TextColor(p.accentInk).Shrink(0)
			ui.Text(c, nonEmptyString(branch, "HEAD")).Font("Menlo").FontSize(11.5).FontWeight(600).MaxWidth(maxBranchW).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			if state != nil && state.Ahead > 0 {
				ui.Text(c, fmt.Sprintf("↑%d", state.Ahead)).Font("Menlo").FontSize(10).TextColor(p.success).NoWrap().Shrink(0)
			}
			if state != nil && state.Behind > 0 {
				ui.Text(c, fmt.Sprintf("↓%d", state.Behind)).Font("Menlo").FontSize(10).TextColor(p.warning).NoWrap().Shrink(0)
			}
			ui.Icon(c, chevronIcon).FontSize(10).TextColor(p.faint).Shrink(0)
		})
	})
	if pill.Clicked() {
		w.toggleBranchPopover(anchor)
	}
	if w.gitView.branchAnchor == anchor {
		w.mountIdeaBranchPopover(c, p, pill, state, locked, at, self)
	}
}

func (w *workbench) mountIdeaBranchPopover(c *ui.Context, p palette, trigger ui.Element, state *api.GitState, locked bool, at, self ui.Anchor) {
	winW, _ := c.Size()
	ui.PopoverBase(c, trigger, &w.gitView.branchOpen, func(panel ui.Element) {
		panel.AttachTo(trigger, at, self).Margin(4, 0).Width(min(float32(356), max(float32(280), winW-24))).Padding(8).Radius(12).Background(p.raised).Border(1, p.line)
		if p.dark {
			panel.Shadow(0, 14, 36, 0, ui.Hex("#00000080"))
		} else {
			panel.Shadow(0, 10, 28, 0, ui.Hex("#0F172A24"))
		}
		w.ideaBranchPopoverContent(c, p, state, locked)
	})
}

func (w *workbench) ideaBranchPopoverContent(c *ui.Context, p palette, state *api.GitState, locked bool) {
	target := w.selected
	busy := w.features.busy["git:"+target]
	_, active := w.activeRun(target)
	query := strings.ToLower(strings.TrimSpace(w.gitView.branchQuery))
	match := func(name string) bool { return query == "" || strings.Contains(strings.ToLower(name), query) }

	ui.Column(c.Key("idea-branch-popover")).Gap(6).FillWidth().Children(func() {
		// 1. IDEA-style top search bar + Fetch & Open Git icons.
		ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Row(c).Grow(1).MinWidth(0).Height(30).Padding(0, 8).Gap(6).Radius(8).AlignItems(ui.Center).
				Background(p.soft).Border(1, p.lineSoft).Children(func() {
				ui.Icon(c, searchIcon).FontSize(12.5).TextColor(p.faint).Shrink(0)
				ui.TextInputBase(c.Key("branch-query"), &w.gitView.branchQuery).
					Label(w.tr("搜索分支和操作", "Search branches and actions")).
					Placeholder(w.tr("搜索分支和操作…", "Search branches & actions…")).
					FontSize(12).Grow(1).MinWidth(0)
				if w.gitView.branchQuery != "" {
					if iconButton(c, p, "branch-query-clear", w.tr("清空搜索", "Clear search"), closeIcon).Size(18, 18).Padding(3).Clicked() {
						w.gitView.branchQuery = ""
					}
				}
			})
			fetchBtn := iconButton(c, p, "branch-pop-fetch", w.tr("Fetch 远端分支", "Fetch remote branches"), refreshIcon).Size(28, 28).Padding(6)
			if busy {
				fetchBtn.Disabled(true)
			}
			if fetchBtn.Clicked() {
				w.gitRemote(actionGitFetch)
			}
			openGit := iconButton(c, p, "branch-pop-open-git", w.tr("打开 Git 面板", "Open Git panel"), lucide("panel-right")).Size(28, 28).Padding(6)
			if openGit.Clicked() {
				w.gitView.branchOpen = false
				w.openDock("git")
			}
		})

		// Optional inline rename bar or delete confirmation bar.
		if w.gitView.branchRenaming != "" {
			oldName := w.gitView.branchRenaming
			ui.Column(c.Key("branch-rename-bar")).FillWidth().Padding(8).Gap(6).Radius(8).Background(p.soft).Border(1, p.accent.Alpha(0.4)).Children(func() {
				ui.Text(c, w.tr("重命名分支：", "Rename branch: ")+oldName).FontSize(11).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…")
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.TextInput(c.Key("branch-rename-input"), &w.gitView.branchRenameDraft).Label(w.tr("重命名后的分支名", "Renamed branch name")).Grow(1).MinWidth(0)
					if quietButton(c, p, "branch-rename-cancel", w.tr("取消", "Cancel")).Height(26).Clicked() {
						w.gitView.branchRenaming = ""
					}
					okBtn := primaryAction(c, p, "branch-rename-ok", w.tr("确认", "Rename"))
					okBtn.Disabled(locked || strings.TrimSpace(w.gitView.branchRenameDraft) == "" || strings.TrimSpace(w.gitView.branchRenameDraft) == oldName)
					if okBtn.Clicked() {
						w.gitRenameBranch(oldName, w.gitView.branchRenameDraft)
					}
				})
			})
		}
		if w.gitView.branchDeleteConfirm != "" {
			delName := w.gitView.branchDeleteConfirm
			ui.Row(c.Key("branch-delete-bar")).FillWidth().Padding(6, 8).Gap(6).Radius(8).AlignItems(ui.Center).
				Background(p.danger.Alpha(0.10)).Border(1, p.danger.Alpha(0.35)).Children(func() {
				ui.Icon(c, lucide("trash-2")).FontSize(12).TextColor(p.danger).Shrink(0)
				ui.Text(c, w.tr("删除分支 ", "Delete ")+delName+"？").FontSize(11).TextColor(p.ink).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
				if quietButton(c, p, "branch-del-cancel", w.tr("取消", "Cancel")).Height(22).Clicked() {
					w.gitView.branchDeleteConfirm = ""
				}
				delBtn := ui.Button(c.Key("branch-del-ok"), w.tr("删除", "Delete")).Height(22).Padding(0, 8).TextColor(p.danger)
				delBtn.Disabled(locked)
				if delBtn.Clicked() {
					w.gitDeleteBranch(delName, false)
				}
			})
		}

		if state == nil {
			ui.Text(c, w.tr("正在加载仓库分支…", "Loading repository branches…")).FontSize(12).TextColor(p.muted).Padding(12, 8)
			return
		}

		// 2. IDEA quick project actions: Update Project (Pull), Commit, Push, Stash.
		showActions := query == "" || match("更新") || match("pull") || match("提交") || match("commit") || match("推送") || match("push") || match("储藏") || match("stash")
		if showActions {
			ui.Column(c).FillWidth().Gap(1).Children(func() {
				if query == "" || match("更新") || match("pull") {
					behindBadge := ""
					if state.Behind > 0 {
						behindBadge = fmt.Sprintf("↓%d", state.Behind)
					}
					if w.ideaPopoverActionRow(c, p, "act-update", pullIcon, w.tr("更新项目 (Pull)…", "Update Project (Pull)…"), behindBadge, "⌘T", p.warning, locked) {
						w.gitView.branchOpen = false
						w.gitRemote(actionGitPull)
					}
				}
				if query == "" || match("提交") || match("commit") {
					changeBadge := ""
					if len(state.Files) > 0 {
						changeBadge = fmt.Sprint(len(state.Files))
					}
					if w.ideaPopoverActionRow(c, p, "act-commit", commitIcon, w.tr("提交变更…", "Commit…"), changeBadge, "⌘K", p.accentInk, false) {
						w.gitView.branchOpen = false
						w.gitView.tab = 0
						w.openDock("git")
						w.gitView.messageFocus.Focus()
					}
				}
				if query == "" || match("推送") || match("push") {
					aheadBadge := ""
					if state.Ahead > 0 {
						aheadBadge = fmt.Sprintf("↑%d", state.Ahead)
					}
					if w.ideaPopoverActionRow(c, p, "act-push", pushIcon, w.tr("推送…", "Push…"), aheadBadge, "⇧⌘K", p.success, locked) {
						w.gitView.branchOpen = false
						w.gitView.pushConfirm = target
						w.openDock("git")
					}
				}
				if (query == "" || match("储藏") || match("stash")) && (len(state.Files) > 0 || len(state.Stashes) > 0) {
					if len(state.Files) > 0 {
						if w.ideaPopoverActionRow(c, p, "act-stash", stashIcon, w.tr("储藏本地更改…", "Stash changes…"), "", "", p.muted, locked) {
							w.gitView.branchOpen = false
							w.gitRemote(actionGitStash)
						}
					}
					if len(state.Stashes) > 0 {
						if w.ideaPopoverActionRow(c, p, "act-stash-pop", pullIcon, fmt.Sprintf(w.tr("恢复最新储藏 (%d)", "Pop latest stash (%d)"), len(state.Stashes)), "", "", p.muted, locked) {
							w.gitView.branchOpen = false
							w.gitStashPop(state.Stashes[0].Ref)
						}
					}
				}
			})
			ui.Box(c).FillWidth().Height(1).Background(p.lineSoft)
		}

		// 3. Inline "+ 新建分支…" row (fast, single-click, zero modal overhead).
		ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, plusIcon).FontSize(12.5).TextColor(p.muted).Shrink(0)
			ui.TextInput(c.Key("new-branch"), &w.gitView.newBranch).
				Label(w.tr("新分支名", "New branch name")).
				Placeholder(w.tr("新建分支…", "New branch…")).
				Grow(1).MinWidth(0)
			create := kitButton(c, p, "create-branch", w.tr("新建", "Create"), controlOutline, controlSM).Label(w.tr("新建分支", "New branch"))
			if locked || strings.TrimSpace(w.gitView.newBranch) == "" {
				create.Disabled(true)
			}
			if create.Clicked() {
				w.gitCreateBranch(w.gitView.newBranch)
			}
		})

		ui.Box(c).FillWidth().Height(1).Background(p.lineSoft)

		// 4. Grouped IDEA Branch Tree: 最近 / 本地 (folder tree) / 远程 (folder tree).
		if w.gitView.branchFolders == nil {
			w.gitView.branchFolders = map[string]bool{}
		}
		ui.Scroll(c.Key("branch-list")).MaxHeight(340).FillWidth().Padding(0, 8, 0, 0).Gap(2).Children(func() {
			// Section A: Recent branches (only when not filtering tightly).
			recent := w.recentBranchesFor(target, state)
			var filteredRecent []string
			for _, b := range recent {
				if match(b) {
					filteredRecent = append(filteredRecent, b)
				}
			}
			if len(filteredRecent) > 0 {
				recentOpen := !w.gitView.branchFolders["sec:recent"]
				if w.ideaSectionHeader(c, p, "sec:recent", w.tr("最近", "Recent"), len(filteredRecent), recentOpen) {
					w.gitView.branchFolders["sec:recent"] = recentOpen
				}
				if recentOpen {
					for _, b := range filteredRecent {
						tracking := ""
						if b == state.Branch && state.RemoteBranch != "" {
							tracking = state.RemoteBranch
						}
						w.ideaBranchNodeRow(c, p, "recent", b, b, tracking, b == state.Branch, false, locked, active, 0)
					}
				}
			}

			// Section B: Local branches (grouped into folders by "/").
			var localBranches []string
			for _, b := range state.Branches {
				if match(b) {
					localBranches = append(localBranches, b)
				}
			}
			if state.Branch != "" && !slices.Contains(localBranches, state.Branch) && match(state.Branch) {
				localBranches = append([]string{state.Branch}, localBranches...)
			}
			localOpen := !w.gitView.branchFolders["sec:local"] || query != ""
			if w.ideaSectionHeader(c, p, "sec:local", w.tr("本地分支", "Local"), len(localBranches), localOpen) {
				w.gitView.branchFolders["sec:local"] = localOpen
			}
			if localOpen {
				tree := buildPopoverBranchTree(localBranches)
				limit := 60
				shown := 0
				w.renderBranchTreeNodes(c, p, "local", state, tree, false, locked, active, 0, &shown, limit, query != "")
				if len(localBranches) > limit {
					ui.Text(c, fmt.Sprintf(w.tr("还有 %d 个本地分支，输入名称筛选", "%d more local branches; type to filter"), len(localBranches)-limit)).FontSize(11).TextColor(p.faint).Padding(4, 10)
				}
			}

			// Section C: Remote branches (grouped into folders by "/").
			if len(state.RemoteBranches) > 0 {
				var remoteBranches []string
				for _, b := range state.RemoteBranches {
					if match(b) {
						remoteBranches = append(remoteBranches, b)
					}
				}
				remoteCollapsed, hasState := w.gitView.branchFolders["sec:remote"]
				remoteOpen := (!remoteCollapsed && hasState) || (!hasState && len(remoteBranches) <= 12) || query != ""
				if w.ideaSectionHeader(c, p, "sec:remote", w.tr("远程分支", "Remote"), len(remoteBranches), remoteOpen) {
					w.gitView.branchFolders["sec:remote"] = remoteOpen
				}
				if remoteOpen {
					tree := buildPopoverBranchTree(remoteBranches)
					limit := 40
					shown := 0
					w.renderBranchTreeNodes(c, p, "remote", state, tree, true, locked, active, 0, &shown, limit, query != "")
					if len(remoteBranches) > limit {
						ui.Text(c, fmt.Sprintf(w.tr("还有 %d 个远程分支，输入名称筛选", "%d more remote branches; type to filter"), len(remoteBranches)-limit)).FontSize(11).TextColor(p.faint).Padding(4, 10)
					}
				}
			}
		})
	})
}

func (w *workbench) ideaPopoverActionRow(c *ui.Context, p palette, key string, icon *ui.SVG, label, badge, shortcut string, badgeTone ui.Color, disabled bool) bool {
	btn := ui.Button(c.Key(key), "").Label(label).FillWidth().Height(28).Padding(0, 8).Radius(6).
		Border(0, ui.Transparent).Background(ui.Transparent).TextColor(p.ink)
	if disabled {
		btn.Disabled(true).TextColor(p.faint)
	} else if btn.Hovered() {
		btn.Background(p.accent.Alpha(0.16)).TextColor(p.ink)
	}
	btn.Children(func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icon).FontSize(12.5).TextColor(p.muted).Shrink(0)
			ui.Text(c, label).FontSize(12).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
			if badge != "" {
				ui.Text(c, badge).Font("Menlo").FontSize(10.5).FontWeight(500).TextColor(badgeTone).Padding(1, 5).Radius(6).Background(badgeTone.Alpha(0.14)).NoWrap().Shrink(0)
			}
			if shortcut != "" {
				ui.Text(c, shortcut).Font("Menlo").FontSize(10.5).TextColor(p.faint).NoWrap().Shrink(0)
			}
		})
	})
	return btn.Clicked()
}

func (w *workbench) ideaSectionHeader(c *ui.Context, p palette, key, title string, count int, open bool) bool {
	btn := ui.Button(c.Key(key), "").Label(title).FillWidth().Height(24).Padding(0, 4).Radius(5).
		Border(0, ui.Transparent).Background(ui.Transparent).TextColor(p.faint)
	if btn.Hovered() {
		btn.Background(p.rowHover).TextColor(p.muted)
	}
	btn.Children(func() {
		ui.Row(c).FillWidth().Gap(5).AlignItems(ui.Center).Children(func() {
			ic := rightIcon
			if open {
				ic = chevronIcon
			}
			ui.Icon(c, ic).FontSize(10).TextColor(p.faint).Shrink(0)
			ui.Text(c, title).FontSize(11).FontWeight(600).TextColor(p.muted).NoWrap()
			ui.Text(c, fmt.Sprint(count)).FontSize(10).TextColor(p.faint).NoWrap()
			ui.Spacer(c)
		})
	})
	return btn.Clicked()
}

func (w *workbench) renderBranchTreeNodes(c *ui.Context, p palette, section string, state *api.GitState, nodes []branchTreeNode, remote, locked, active bool, depth int, shown *int, limit int, forceExpand bool) {
	for _, node := range nodes {
		if *shown >= limit {
			return
		}
		if node.isFolder {
			folderKey := section + ":dir:" + fmt.Sprintf("%d:%s", depth, node.name)
			collapsed := w.gitView.branchFolders[folderKey]
			open := !collapsed || forceExpand
			indent := float32(10 + depth*14)
			row := ui.Button(c.Key(folderKey), "").Label(node.name).FillWidth().Height(26).Padding(0, 6, 0, indent).Radius(6).
				Border(0, ui.Transparent).Background(ui.Transparent).TextColor(p.muted)
			if row.Hovered() {
				row.Background(p.rowHover).TextColor(p.ink)
			}
			row.Children(func() {
				ui.Row(c).FillWidth().Gap(5).AlignItems(ui.Center).Children(func() {
					ic := rightIcon
					if open {
						ic = chevronIcon
					}
					ui.Icon(c, ic).FontSize(9.5).TextColor(p.faint).Shrink(0)
					ui.Icon(c, folderIcon).FontSize(11.5).TextColor(p.faint).Shrink(0)
					ui.Text(c, node.name).Font("Menlo").FontSize(11.5).TextColor(p.ink).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
					ui.Text(c, fmt.Sprint(len(node.children))).FontSize(10).TextColor(p.faint).Shrink(0)
				})
			})
			if row.Clicked() {
				w.gitView.branchFolders[folderKey] = open
			}
			if open {
				w.renderBranchTreeNodes(c, p, section, state, node.children, remote, locked, active, depth+1, shown, limit, forceExpand)
			}
			continue
		}
		*shown++
		tracking := ""
		if !remote && node.fullName == state.Branch && state.RemoteBranch != "" {
			tracking = state.RemoteBranch
		}
		w.ideaBranchNodeRow(c, p, section, node.name, node.fullName, tracking, !remote && node.fullName == state.Branch, remote, locked, active, depth)
	}
}

func (w *workbench) ideaBranchNodeRow(c *ui.Context, p palette, section, displayName, fullName, tracking string, current, remote, locked, active bool, depth int) {
	indent := float32(12 + depth*14)
	rowKey := section + ":" + fullName
	expanded := w.gitView.branchExpanded == rowKey
	currentBranch := ""
	if st := w.features.git[w.selected]; st != nil {
		currentBranch = st.Branch
	}

	ui.Column(c.Key("bnode:"+rowKey)).FillWidth().Gap(2).Children(func() {
		row := ui.Row(c).FillWidth().Height(27).Padding(0, 4, 0, indent).Gap(5).Radius(6).AlignItems(ui.Center)
		if expanded {
			row.Background(p.selected)
		} else if row.Hovered() {
			row.Background(p.rowHover)
		}
		row.Children(func() {
			mainBtn := ui.Button(c.Key("bpick:"+rowKey), "").Label(fullName).Grow(1).MinWidth(0).Height(25).Padding(0, 2).
				Border(0, ui.Transparent).Background(ui.Transparent).Justify(ui.Start)
			mainBtn.Children(func() {
				ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).MinWidth(0).Children(func() {
					icon, tone := branchIcon, p.muted
					if current {
						icon, tone = lucide("tag"), p.warning
					} else if remote {
						icon = lucide("cloud")
					}
					ui.Icon(c, icon).FontSize(11.5).TextColor(tone).Shrink(0)
					nameEl := ui.Text(c, displayName).Font("Menlo").FontSize(11.5).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
					if current {
						nameEl.FontWeight(600).TextColor(p.ink)
					} else {
						nameEl.TextColor(p.ink)
					}
					ui.Spacer(c)
					if tracking != "" {
						ui.Text(c, tracking).Font("Menlo").FontSize(10).TextColor(p.faint).SingleLine().Ellipsis("…").MaxWidth(118).Shrink(2).MinWidth(0)
					}
				})
			})
			if mainBtn.Clicked() {
				if expanded {
					w.gitView.branchExpanded = ""
				} else {
					w.gitView.branchExpanded = rowKey
				}
			}

			// Right ">" submenu button — mirrors IDEA's branch flyout actions.
			more := iconButton(c, p, "bmenu:"+rowKey, w.tr("分支操作 ", "Branch actions ")+fullName, rightIcon).Size(22, 22).Padding(4)
			more.PopoverMenuButton(func(m *ui.Menu) {
				if !current {
					if m.Item(w.tr("签出 (Checkout)", "Checkout")).Disabled(locked).Chosen() {
						w.recordRecentBranch(w.selected, fullName)
						w.gitCheckout(fullName)
					}
				}
				if !remote {
					if m.Item(w.tr("更新分支 (Update)", "Update")).Disabled(locked).Chosen() {
						w.gitUpdateBranch(fullName)
					}
				}
				if !current {
					if !remote {
						mergeLabel := w.tr("合并到当前分支", "Merge into Current")
						if currentBranch != "" {
							mergeLabel = fmt.Sprintf(w.tr("合并到 %s", "Merge into %s"), currentBranch)
						}
						if m.Item(mergeLabel).Disabled(locked).Chosen() {
							w.gitMergeNamedBranch(fullName)
						}
					}
					rebaseLabel := w.tr("变基当前分支到此处", "Rebase Current onto")
					if currentBranch != "" {
						rebaseLabel = fmt.Sprintf(w.tr("将 %s 变基到此处", "Rebase %s onto this"), currentBranch)
					}
					if m.Item(rebaseLabel).Disabled(locked).Chosen() {
						w.gitRebaseBranch(fullName)
					}
					cmpLabel := w.tr("与当前分支比较", "Compare with Current")
					if currentBranch != "" {
						cmpLabel = fmt.Sprintf(w.tr("与 %s 比较", "Compare with %s"), currentBranch)
					}
					if m.Item(cmpLabel).Chosen() {
						w.openBranchCompare(currentBranch, fullName)
					}
				}
				if !remote {
					m.Separator()
					if m.Item(w.tr("重命名…", "Rename…")).Disabled(locked).Chosen() {
						w.gitView.branchRenaming = fullName
						w.gitView.branchRenameDraft = fullName
						w.gitView.branchDeleteConfirm = ""
					}
					if !current {
						if m.Item(w.tr("删除分支…", "Delete…")).Disabled(locked).Chosen() {
							w.gitView.branchDeleteConfirm = fullName
							w.gitView.branchRenaming = ""
						}
					}
				}
			}, ui.PopoverMenuButtonOptions{At: ui.AnchorTopEnd, Self: ui.AnchorTopStart})
		})

		// Inline recessed IDEA action tray on click: 1-click Checkout / Update / Merge / Rebase / Compare.
		if expanded {
			ui.Row(c.Key("bactions:"+rowKey)).FillWidth().Padding(4, 6, 4, indent+12).Gap(4).Wrap().AlignItems(ui.Center).
				Radius(7).Background(p.soft).Border(1, p.lineSoft).Children(func() {
				if !current {
					co := quietButton(c, p, "co:"+rowKey, w.tr("签出", "Checkout")).Label(w.tr("签出 ", "Checkout ")+fullName).Height(22).Padding(0, 7).FontSize(11)
					co.Disabled(locked)
					if co.Clicked() {
						w.recordRecentBranch(w.selected, fullName)
						w.gitCheckout(fullName)
					}
				}
				if !remote {
					up := quietButton(c, p, "up:"+rowKey, w.tr("更新", "Update")).Label(w.tr("更新 ", "Update ")+fullName).Height(22).Padding(0, 7).FontSize(11)
					up.Disabled(locked)
					if up.Clicked() {
						w.gitUpdateBranch(fullName)
					}
				}
				if !current && !remote {
					mg := quietButton(c, p, "mg:"+rowKey, w.tr("合并", "Merge")).Label(w.tr("自动合并到当前分支", "Merge into the current branch")).Height(22).Padding(0, 7).FontSize(11)
					mg.Disabled(locked)
					if mg.Clicked() {
						w.gitMergeNamedBranch(fullName)
					}
				}
				if !current {
					rb := quietButton(c, p, "rb:"+rowKey, w.tr("变基", "Rebase")).Label(w.tr("变基到 ", "Rebase onto ")+fullName).Height(22).Padding(0, 7).FontSize(11)
					rb.Disabled(locked)
					if rb.Clicked() {
						w.gitRebaseBranch(fullName)
					}
					cmp := quietButton(c, p, "cmp:"+rowKey, w.tr("比较", "Compare")).Label(w.tr("比较 ", "Compare ")+fullName).Height(22).Padding(0, 7).FontSize(11).TextColor(p.accentInk)
					if cmp.Clicked() {
						w.openBranchCompare(currentBranch, fullName)
					}
				}
				if !remote {
					rn := quietButton(c, p, "rn:"+rowKey, w.tr("重命名", "Rename")).Label(w.tr("重命名 ", "Rename ")+fullName).Height(22).Padding(0, 7).FontSize(11)
					rn.Disabled(locked)
					if rn.Clicked() {
						w.gitView.branchRenaming = fullName
						w.gitView.branchRenameDraft = fullName
						w.gitView.branchDeleteConfirm = ""
					}
				}
				if !current && !remote {
					del := quietButton(c, p, "del:"+rowKey, w.tr("删除", "Delete")).Label(w.tr("删除 ", "Delete ")+fullName).Height(22).Padding(0, 7).FontSize(11).TextColor(p.danger)
					del.Disabled(locked)
					if del.Clicked() {
						w.gitView.branchDeleteConfirm = fullName
						w.gitView.branchRenaming = ""
					}
				}
				_ = active
			})
		}
	})
}
