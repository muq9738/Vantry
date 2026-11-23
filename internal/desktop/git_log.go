package desktop

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"github.com/muq9738/Vantry/pkg/api"
)

// The Log page follows IntelliJ IDEA's Git log: a branch tree (HEAD, local
// branches grouped into folders by prefix, remotes by remote and prefix), a
// filtered commit table with the graph, and the selected commit's changed
// files as a directory tree over its message, refs and containing branches.
// Every commit and branch carries IDEA's context actions.

const actionGitCommitOp = "git.commit_op"

// gitLogFilter is the log toolbar: text or hash, branch, author, path, period.
type gitLogFilter struct {
	branch, author, path, since string
}

func (w *workbench) logSignature() string {
	f := w.gitView.filter
	return strings.Join([]string{strings.TrimSpace(w.gitView.logQuery), f.branch, f.author, f.path, f.since}, "\x00")
}

// gitPrompt is the small dialog a commit or branch action opens for a name,
// a message or a confirmation.
type gitPrompt struct {
	open                bool
	kind                string // branch, tag, reword, reset-hard, delete-branch, new-branch
	commit, branch      string
	title, detail, hint string
	value               string
	danger              bool
}

// ------------------------------------------------------------ loading

func (w *workbench) loadGitLog(query string) {
	target := w.selected
	request, ok := w.gitRequest(target, actionGitLog)
	if !ok {
		return
	}
	f := w.gitView.filter
	request.Message = strings.TrimSpace(query)
	request.Branch = f.branch
	if f.author != "" || f.path != "" || f.since != "" {
		request.Input, _ = json.Marshal(map[string]string{"author": f.author, "path": f.path, "since": f.since})
	}
	if w.gitView.logLoaded == nil {
		w.gitView.logLoaded, w.gitView.logs = map[string]string{}, map[string][]api.GitCommit{}
	}
	signature := w.logSignature()
	w.gitView.logLoaded[target] = signature
	w.workbenchCall("git-log:"+target, request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.setToolFeedback("git:"+target, notice{Text: describe(w.tr("读取日志失败", "Reading the log failed"), err), Level: noticeError})
			return
		}
		if commits, ok := decodeGitLog(response); ok {
			w.gitView.logs[target] = commits
		}
	})
}

// reloadLog re-reads the log after a filter changed.
func (w *workbench) reloadLog() {
	w.loadGitLog(w.gitView.logQuery)
}

func (w *workbench) logCommits(state *api.GitState) []api.GitCommit {
	target := w.selected
	commits := state.Commits
	if loaded, ok := w.gitView.logs[target]; ok {
		if w.gitView.logLoaded[target] == w.logSignature() {
			return loaded
		}
		commits = loaded
	}
	query := strings.ToLower(strings.TrimSpace(w.gitView.logQuery))
	if query == "" {
		return commits
	}
	var out []api.GitCommit
	for _, commit := range commits {
		if strings.Contains(strings.ToLower(commit.Subject), query) || strings.Contains(strings.ToLower(commit.Author), query) || strings.HasPrefix(commit.ID, query) {
			out = append(out, commit)
		}
	}
	return out
}

// graphFor computes the graph once per list of commits.
func (w *workbench) graphFor(commits []api.GitCommit) ([]graphRow, int) {
	key := fmt.Sprint(len(commits))
	if len(commits) > 0 {
		key += commits[0].ID + commits[len(commits)-1].ID
	}
	if w.gitView.graphKey != key || len(w.gitView.graphRows) != len(commits) {
		w.gitView.graphKey = key
		w.gitView.graphRows = commitGraph(commits)
		w.gitView.graphLanes = 1
		for _, row := range w.gitView.graphRows {
			w.gitView.graphLanes = max(w.gitView.graphLanes, row.lanes)
		}
	}
	return w.gitView.graphRows, w.gitView.graphLanes
}

// ------------------------------------------------------------ actions

func (w *workbench) gitCommitOp(op, id, label string, input map[string]string) bool {
	return w.gitAction(actionGitCommitOp, label, func(r *api.WorkbenchRequest) {
		r.Name, r.Message = id, op
		if len(input) > 0 {
			r.Input, _ = json.Marshal(input)
		}
	}, nil)
}

// copyPatch reads the commit as a mail patch and copies it on the next frame.
func (w *workbench) copyPatch(id string) {
	target := w.selected
	request, ok := w.gitRequest(target, actionGitCommitOp)
	if !ok {
		return
	}
	request.Name, request.Message = id, "patch"
	w.workbenchCall("git-patch:"+target, request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.setToolFeedback("git:"+target, notice{Text: describe(w.tr("创建补丁失败", "Creating the patch failed"), err), Level: noticeError})
			return
		}
		w.gitView.clipboard = response.Output
		w.setToolFeedback("git:"+target, notice{Text: w.tr("补丁已复制到剪贴板。", "Patch copied to the clipboard."), Level: noticeInfo})
	})
}

func (w *workbench) openGitPrompt(prompt gitPrompt) {
	prompt.open = true
	w.gitView.prompt = prompt
}

func (w *workbench) setLogBranch(branch string) {
	if w.gitView.filter.branch == branch {
		return
	}
	w.gitView.filter.branch = branch
	w.reloadLog()
}

func (w *workbench) deleteBranch(branch string, force bool) bool {
	return w.gitAction("git.branch.delete", w.tr("删除分支 ", "Delete branch ")+branch, func(r *api.WorkbenchRequest) { r.Name, r.Branch, r.Amend = branch, branch, force }, nil)
}

// ------------------------------------------------------------ page

func (w *workbench) gitLogPage(c *ui.Context, p palette, state *api.GitState, width float32) {
	target := w.selected
	if w.gitView.clipboard != "" {
		c.WriteClipboard(w.gitView.clipboard)
		w.gitView.clipboard = ""
	}
	if _, loaded := w.gitView.logLoaded[target]; !loaded && !w.features.busy["git-log:"+target] {
		// An action invalidated the log (checkout, reset, cherry-pick…).
		w.reloadLog()
	}
	commits := w.logCommits(state)
	selected := ""
	if w.gitView.commit != nil {
		selected = w.gitView.commit[target]
	}
	w.logToolbar(c, p, state, commits, width)
	ui.Column(c.Key("git-log-card")).Grow(1).MinHeight(0).Radius(10).Border(1, p.lineSoft).Clip().Children(func() {
		switch {
		case width >= 1000:
			treeW := float32(0)
			if !w.gitView.treeHidden {
				treeW = min(260, width*0.2)
			}
			detailW := min(420, width*0.32)
			tableW := max(320, width-treeW-detailW)
			ui.Row(c.Key("git-log-panes")).Grow(1).MinHeight(0).Gap(0).AlignItems(ui.Stretch).Children(func() {
				if !w.gitView.treeHidden {
					ui.Column(c.Key("branch-tree-pane")).Width(treeW).Shrink(0).MinHeight(0).BorderWidth(0, 1, 0, 0).BorderColor(p.lineSoft).Children(func() {
						w.branchTree(c, p, state)
					})
				}
				ui.Column(c.Key("log-table-pane")).Grow(1).MinWidth(0).MinHeight(0).Children(func() { w.logTable(c, p, state, commits, selected, true, tableW) })
				ui.Column(c.Key("commit-detail-pane")).Width(detailW).Shrink(0).MinHeight(0).BorderWidth(0, 0, 0, 1).BorderColor(p.lineSoft).Children(func() {
					w.commitDetailView(c, p, selected)
				})
			})
		case width >= 680:
			detailW := width * 0.42
			tableW := max(280, width-detailW)
			ui.Row(c.Key("git-log-panes")).Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
				ui.Column(c.Key("log-table-pane")).Grow(1).MinWidth(0).MinHeight(0).Children(func() { w.logTable(c, p, state, commits, selected, false, tableW) })
				ui.Column(c.Key("commit-detail-pane")).Width(detailW).Shrink(0).MinHeight(0).BorderWidth(0, 0, 0, 1).BorderColor(p.lineSoft).Children(func() {
					w.commitDetailView(c, p, selected)
				})
			})
		default:
			ui.Column(c.Key("log-table-pane")).Grow(1).MinHeight(0).Children(func() { w.logTable(c, p, state, commits, selected, false, width) })
			if selected != "" {
				_, height := c.Size()
				ui.Column(c.Key("commit-detail-pane")).Height(max(float32(200), height*0.4)).Shrink(0).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
					w.commitDetailView(c, p, selected)
				})
			}
		}
	})
	w.gitPromptDialog(c, p)
}

// filterChip is one of the log toolbar's dropdowns: its name, or its value
// in the accent once set.
func filterChip(c *ui.Context, p palette, key, name, value string, maxTextW float32) ui.Element {
	chip := ui.Button(c.Key(key), "").Label(name).Height(26).Padding(0, 7).Gap(4).Radius(6).Border(1, ui.Transparent).
		Background(ui.Transparent).TextColor(p.muted).Shrink(0)
	if value != "" {
		chip.Background(p.accent.Alpha(0.12)).TextColor(p.accentInk).BorderColor(p.accent.Alpha(0.3))
		chip.Tooltip(name + ": " + value)
	} else if chip.Hovered() {
		chip.Background(p.soft).TextColor(p.ink)
	}
	chip.Children(func() {
		text := name
		if value != "" {
			text = name + ": " + value
		}
		ui.Text(c, text).FontSize(11.5).SingleLine().Ellipsis("…").MaxWidth(maxTextW)
		ui.Icon(c, chevronIcon).FontSize(10).TextColor(p.faint).Shrink(0)
	})
	return chip
}

func (w *workbench) logToolbar(c *ui.Context, p palette, state *api.GitState, commits []api.GitCommit, width float32) {
	target := w.selected
	f := &w.gitView.filter
	compact := width < 720
	chipMaxW := float32(180)
	if width < 400 {
		chipMaxW = 92
	} else if compact {
		chipMaxW = 124
	}

	renderSearch := func(grow bool) {
		box := ui.Row(c.Key("log-search-box")).Height(28).Padding(0, 8).Gap(6).Radius(7).Border(1, p.line).Background(p.canvas.Alpha(0.5)).AlignItems(ui.Center)
		if grow {
			box.Grow(1).MinWidth(120)
		} else {
			box.Width(min(240, width*0.28)).Shrink(0)
		}
		box.Children(func() {
			ui.Icon(c, searchIcon).FontSize(12).TextColor(p.faint).Shrink(0)
			search := ui.TextInputBase(c.Key("log-query"), &w.gitView.logQuery).Label(w.tr("筛选提交", "Filter commits")).
				Placeholder(w.tr("文本或哈希", "Text or hash")).Grow(1).MinWidth(0).FontSize(12)
			if search.Submitted() {
				w.loadGitLog(w.gitView.logQuery)
			}
		})
	}

	renderCountAndRefresh := func() {
		if w.features.busy["git-log:"+target] {
			activityIndicator(c, p.accent).Size(11, 11).Shrink(0)
		}
		ui.Text(c, fmt.Sprintf(w.tr("%d 个提交", "%d commits"), len(commits))).FontSize(11).TextColor(p.faint).NoWrap().Shrink(0)
		refresh := iconButton(c, p, "log-refresh", w.tr("刷新日志", "Refresh log"), refreshIcon).Size(26, 26).Padding(5)
		if refresh.Clicked() {
			delete(w.gitView.logLoaded, target)
			w.reloadLog()
			w.refreshGit(target, true)
		}
	}

	renderFilters := func() {
		branch := filterChip(c, p, "log-branch-filter", w.tr("分支", "Branch"), f.branch, chipMaxW)
		branch.PopoverMenuButton(func(m *ui.Menu) {
			if m.Item(w.tr("全部（当前与远程）", "All (HEAD and remotes)")).Radio(f.branch == "").Chosen() {
				w.setLogBranch("")
			}
			if m.Item(w.tr("所有引用（含标签与储藏）", "All refs")).Radio(f.branch == "*").Chosen() {
				w.setLogBranch("*")
			}
			if m.Item("HEAD").Radio(f.branch == "HEAD").Chosen() {
				w.setLogBranch("HEAD")
			}
			m.Separator()
			for i, name := range state.Branches {
				if i >= 40 {
					m.Item(fmt.Sprintf(w.tr("…其余 %d 个请在分支树中选择", "…%d more in the branch tree"), len(state.Branches)-40)).Disabled(true)
					break
				}
				if m.Item(name).Radio(f.branch == name).Chosen() {
					w.setLogBranch(name)
				}
			}
		}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomStart, Self: ui.AnchorTopStart})
		user := filterChip(c, p, "log-author-filter", w.tr("用户", "User"), f.author, chipMaxW)
		user.PopoverMenuButton(func(m *ui.Menu) {
			if m.Item(w.tr("全部用户", "All users")).Radio(f.author == "").Chosen() && f.author != "" {
				f.author = ""
				w.reloadLog()
			}
			seen := map[string]bool{}
			var authors []string
			for _, commit := range append(append([]api.GitCommit(nil), commits...), state.Commits...) {
				if commit.Author != "" && !seen[commit.Author] {
					seen[commit.Author] = true
					authors = append(authors, commit.Author)
				}
			}
			sort.Strings(authors)
			for i, author := range authors {
				if i >= 30 {
					break
				}
				if m.Item(author).Radio(f.author == author).Chosen() && f.author != author {
					f.author = author
					w.reloadLog()
				}
			}
		}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomStart, Self: ui.AnchorTopStart})
		periods := []struct{ value, zh, en string }{{"", "全部时间", "Any time"}, {"1d", "最近一天", "Last day"}, {"7d", "最近一周", "Last week"}, {"30d", "最近一个月", "Last month"}, {"365d", "最近一年", "Last year"}}
		periodLabel := ""
		for _, period := range periods {
			if period.value != "" && period.value == f.since {
				periodLabel = w.tr(period.zh, period.en)
			}
		}
		date := filterChip(c, p, "log-date-filter", w.tr("日期", "Date"), periodLabel, chipMaxW)
		date.PopoverMenuButton(func(m *ui.Menu) {
			for _, period := range periods {
				if m.Item(w.tr(period.zh, period.en)).Radio(f.since == period.value).Chosen() && f.since != period.value {
					f.since = period.value
					w.reloadLog()
				}
			}
		}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomStart, Self: ui.AnchorTopStart})
		pathChip := filterChip(c, p, "log-path-filter", w.tr("路径", "Path"), f.path, chipMaxW)
		if pathChip.Clicked() {
			w.gitView.pathOpen = !w.gitView.pathOpen
			w.gitView.pathDraft = f.path
		}
		ui.PopoverBase(c, pathChip, &w.gitView.pathOpen, func(panel ui.Element) {
			panel.Width(320).Padding(10).Radius(10).Background(p.raised).Border(1, p.line)
			ui.Column(c).Gap(8).FillWidth().Children(func() {
				ui.Text(c, w.tr("只显示改动了此路径的提交", "Only commits that touch this path")).FontSize(11.5).TextColor(p.muted)
				input := ui.TextInput(c.Key("log-path-input"), &w.gitView.pathDraft).Label(w.tr("日志路径", "Log path")).
					Placeholder("src/ 或 README.md").FillWidth().Height(28).FontSize(12).AutoFocus()
				ui.Row(c).Gap(6).Children(func() {
					if quietButton(c, p, "log-path-clear", w.tr("清除", "Clear")).Height(26).NoWrap().Clicked() {
						w.gitView.pathOpen = false
						if f.path != "" {
							f.path = ""
							w.reloadLog()
						}
					}
					ui.Spacer(c)
					if primaryAction(c, p, "log-path-apply", w.tr("应用", "Apply")).Height(26).NoWrap().Clicked() || input.Submitted() {
						w.gitView.pathOpen = false
						f.path = strings.TrimSpace(w.gitView.pathDraft)
						w.reloadLog()
					}
				})
			})
		})
		if f.branch != "" || f.author != "" || f.path != "" || f.since != "" {
			if quietButton(c, p, "log-filter-reset", w.tr("重置筛选", "Reset")).Height(26).Padding(0, 7).NoWrap().Clicked() {
				*f = gitLogFilter{}
				w.reloadLog()
			}
		}
	}

	if compact {
		ui.Column(c.Key("log-filter")).Gap(5).Padding(0, 0, 2).Shrink(0).Children(func() {
			ui.Row(c.Key("log-filter-top")).Height(28).Gap(8).AlignItems(ui.Center).Children(func() {
				renderSearch(true)
				renderCountAndRefresh()
			})
			ui.Row(c.Key("log-filter-chips")).MinHeight(26).Gap(4).AlignItems(ui.Center).Wrap().Children(func() {
				renderFilters()
			})
		})
		return
	}

	ui.Row(c.Key("log-filter")).MinHeight(32).Padding(0, 0, 2).Gap(6).AlignItems(ui.Center).Shrink(0).Wrap().Children(func() {
		if width >= 1000 {
			label := w.tr("隐藏分支树", "Hide branches")
			if w.gitView.treeHidden {
				label = w.tr("显示分支树", "Show branches")
			}
			tree := iconButton(c, p, "toggle-branch-tree", label, lucide("panel-left")).Size(26, 26).Padding(5)
			if !w.gitView.treeHidden {
				tree.TextColor(p.accentInk)
			}
			if tree.Clicked() {
				w.gitView.treeHidden = !w.gitView.treeHidden
			}
		}
		renderSearch(false)
		renderFilters()
		ui.Spacer(c)
		renderCountAndRefresh()
	})
}

// ------------------------------------------------------------ branch tree

type branchNode struct {
	label, full string
	children    []*branchNode
	leaf        bool
	count       int
}

// buildBranchTree groups names into folders by their slash prefixes, as
// IDEA's branches pane: "feat/a" and "feat/b" under a "feat" folder.
func buildBranchTree(names []string, strip string) []*branchNode {
	root := &branchNode{}
	for _, name := range names {
		rest := strings.TrimPrefix(name, strip)
		parts := strings.Split(rest, "/")
		node := root
		for i, part := range parts {
			if i == len(parts)-1 {
				node.children = append(node.children, &branchNode{label: part, full: name, leaf: true})
				break
			}
			var folder *branchNode
			for _, child := range node.children {
				if !child.leaf && child.label == part {
					folder = child
				}
			}
			if folder == nil {
				folder = &branchNode{label: part, full: node.full + part + "/"}
				node.children = append(node.children, folder)
			}
			node = folder
		}
	}
	var finish func(n *branchNode) int
	finish = func(n *branchNode) int {
		sort.SliceStable(n.children, func(i, j int) bool {
			a, b := n.children[i], n.children[j]
			if a.leaf != b.leaf {
				return !a.leaf
			}
			return strings.ToLower(a.label) < strings.ToLower(b.label)
		})
		if n.leaf {
			return 1
		}
		n.count = 0
		for _, child := range n.children {
			n.count += finish(child)
		}
		return n.count
	}
	finish(root)
	return root.children
}

func (w *workbench) branchTree(c *ui.Context, p palette, state *api.GitState) {
	if w.gitView.treeOpen == nil {
		// Local is open by default; remotes and folders start closed.
		w.gitView.treeOpen = map[string]bool{"local": true}
	}
	_, active := w.activeRun(w.selected)
	locked := active || w.features.busy["git:"+w.selected]
	ui.Row(c).Height(38).Padding(0, 8).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Row(c).Height(26).Grow(1).Padding(0, 8).Gap(6).Radius(6).Border(1, p.line).Background(p.canvas.Alpha(0.5)).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, searchIcon).FontSize(12).TextColor(p.faint)
			ui.TextInputBase(c.Key("branch-tree-query"), &w.gitView.treeQuery).Label(w.tr("搜索分支", "Search branches")).
				Placeholder(w.tr("分支", "Branches")).Grow(1).MinWidth(0).FontSize(12)
		})
	})
	query := strings.ToLower(strings.TrimSpace(w.gitView.treeQuery))
	ui.Scroll(c.Key("branch-tree")).Grow(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Padding(0, 4, 8).Gap(0).Role(ui.RoleTree).Label(w.tr("分支", "Branches")).Children(func() {
			head := w.branchTreeRow(c, p, "tree:head", 0, lucide("crosshair"), w.tr("HEAD（当前分支）", "HEAD (current branch)"), "", w.gitView.filter.branch == "HEAD", false)
			if head.Clicked() {
				w.setLogBranch("HEAD")
			}
			if query != "" {
				shown := 0
				for gi, group := range [][]string{state.Branches, state.RemoteBranches} {
					remote := gi == 1
					for _, name := range group {
						if !strings.Contains(strings.ToLower(name), query) {
							continue
						}
						if shown++; shown > 200 {
							return
						}
						w.branchLeaf(c, p, &branchNode{label: name, full: name, leaf: true}, 1, state, remote, locked)
					}
				}
				return
			}
			w.branchSection(c, p, "local", w.tr("本地", "Local"), buildBranchTree(state.Branches, ""), state, false, locked)
			remotes := map[string][]string{}
			var order []string
			for _, name := range state.RemoteBranches {
				remote, _, _ := strings.Cut(name, "/")
				if _, ok := remotes[remote]; !ok {
					order = append(order, remote)
				}
				remotes[remote] = append(remotes[remote], name)
			}
			if len(order) > 0 {
				open := w.gitView.treeOpen["remote"]
				section := w.branchTreeRow(c, p, "tree:remote", 0, nil, w.tr("远程", "Remote"), "", false, true)
				if section.Clicked() {
					w.gitView.treeOpen["remote"] = !open
				}
				if open {
					for _, remote := range order {
						w.branchSection(c, p, "remote:"+remote, remote, buildBranchTree(remotes[remote], remote+"/"), state, true, locked)
					}
				}
			}
		})
	})
}

func (w *workbench) branchSection(c *ui.Context, p palette, key, title string, nodes []*branchNode, state *api.GitState, remote, locked bool) {
	depth := 0
	if remote {
		depth = 1
	}
	open := w.gitView.treeOpen[key]
	icon := folderIcon
	if remote {
		icon = lucide("cloud")
	}
	header := w.branchTreeRow(c, p, "tree:"+key, depth, icon, title, "", false, true)
	header.Expanded(open)
	if header.Clicked() {
		w.gitView.treeOpen[key] = !open
	}
	if !open {
		return
	}
	var walk func(nodes []*branchNode, depth int)
	walk = func(nodes []*branchNode, depth int) {
		for _, node := range nodes {
			if node.leaf {
				w.branchLeaf(c, p, node, depth, state, remote, locked)
				continue
			}
			folderKey := key + ":" + node.full
			folderOpen := w.gitView.treeOpen[folderKey]
			row := w.branchTreeRow(c, p, "tree:"+folderKey, depth, folderIcon, node.label, fmt.Sprint(node.count), false, true)
			row.Expanded(folderOpen)
			if row.Clicked() {
				w.gitView.treeOpen[folderKey] = !folderOpen
			}
			if folderOpen {
				walk(node.children, depth+1)
			}
		}
	}
	walk(nodes, depth+1)
}

// branchTreeRow draws one line of the tree; folders get a disclosure.
func (w *workbench) branchTreeRow(c *ui.Context, p palette, key string, depth int, icon *ui.SVG, label, trailing string, selected, folder bool) ui.Element {
	row := ui.Button(c.Key(key), "").Label(label).Role(ui.RoleTreeItem).FillWidth().Height(24).Padding(0, 6, 0, 4+float32(depth)*14).Gap(5).Radius(5).
		Border(0, p.line).Background(ui.Transparent).TextColor(p.ink).Justify(ui.Start).Selected(selected)
	if selected {
		row.Background(p.accent.Alpha(0.16))
	} else if row.Hovered() {
		row.Background(p.soft)
	}
	open := folder && w.gitView.treeOpen[strings.TrimPrefix(key, "tree:")]
	row.Children(func() {
		if folder {
			chevron := rightIcon
			if open {
				chevron = chevronIcon
			}
			ui.Icon(c, chevron).FontSize(10).TextColor(p.faint).Shrink(0)
		}
		if icon != nil {
			ui.Icon(c, icon).FontSize(12).TextColor(p.muted).Shrink(0)
		}
		weight := 400
		if folder && depth == 0 {
			weight = 600
		}
		ui.Text(c, label).FontSize(12).FontWeight(weight).SingleLine().Ellipsis("…").MinWidth(0).Shrink(1)
		if trailing != "" {
			ui.Spacer(c)
			ui.Text(c, trailing).FontSize(10.5).TextColor(p.faint).NoWrap()
		}
	})
	return row
}

func (w *workbench) branchLeaf(c *ui.Context, p palette, node *branchNode, depth int, state *api.GitState, remote, locked bool) {
	name := node.full
	current := !remote && name == state.Branch
	icon, tone := branchIcon, p.muted
	switch {
	case current:
		icon, tone = lucide("tag"), p.warning
	case !remote && (name == "main" || name == "master"):
		icon, tone = lucide("star"), p.warning
	}
	row := ui.Button(c.Key("tree-leaf:"+name), "").Label(w.tr("分支 ", "Branch ")+name).Role(ui.RoleTreeItem).FillWidth().Height(24).
		Padding(0, 6, 0, 18+float32(depth)*14).Gap(6).Radius(5).Border(0, p.line).Background(ui.Transparent).TextColor(p.ink).Justify(ui.Start).
		Selected(w.gitView.filter.branch == name)
	if w.gitView.filter.branch == name {
		row.Background(p.accent.Alpha(0.16))
	} else if row.Hovered() {
		row.Background(p.soft)
	}
	row.Children(func() {
		ui.Icon(c, icon).FontSize(12).TextColor(tone).Shrink(0)
		text := ui.Text(c, node.label).FontSize(12).SingleLine().Ellipsis("…").MinWidth(0).Shrink(1)
		if current {
			text.FontWeight(600)
		}
		if current && (state.Ahead > 0 || state.Behind > 0) {
			ui.Spacer(c)
			if state.Ahead > 0 {
				ui.Text(c, fmt.Sprintf("↑%d", state.Ahead)).FontSize(10.5).TextColor(p.success).NoWrap()
			}
			if state.Behind > 0 {
				ui.Text(c, fmt.Sprintf("↓%d", state.Behind)).FontSize(10.5).TextColor(p.accentInk).NoWrap()
			}
		}
	})
	if row.Clicked() {
		w.setLogBranch(name)
	}
	if row.DoubleClicked() && !current && !locked {
		w.gitCheckout(name)
	}
	row.ContextMenu(func(m *ui.Menu) {
		if m.Item(w.tr("签出", "Checkout")).Disabled(current || locked).Chosen() {
			w.gitCheckout(name)
		}
		if m.Item(fmt.Sprintf(w.tr("从 '%s' 新建分支…", "New branch from '%s'…"), name)).Disabled(locked).Chosen() {
			w.openGitPrompt(gitPrompt{kind: "new-branch", branch: name, title: w.tr("新建分支", "New branch"), detail: w.tr("从 ", "From ") + name, hint: w.tr("新分支名", "New branch name")})
		}
		if m.Item(fmt.Sprintf(w.tr("将 '%s' 合并到 '%s'", "Merge '%s' into '%s'"), name, state.Branch)).Disabled(current || locked).Chosen() {
			if w.features.gitBranches == nil {
				w.features.gitBranches = map[string]string{}
			}
			w.features.gitBranches[w.selected] = name
			w.mergeBranch()
		}
		m.Separator()
		if m.Item(w.tr("在日志中仅显示此分支", "Show only this branch in the log")).Chosen() {
			w.setLogBranch(name)
		}
		if m.Item(w.tr("复制分支名", "Copy branch name")).Chosen() {
			c.WriteClipboard(name)
		}
		if !remote {
			m.Separator()
			if m.Item(w.tr("删除…", "Delete…")).Danger().Disabled(current || locked).Chosen() {
				w.openGitPrompt(gitPrompt{kind: "delete-branch", branch: name, title: w.tr("删除分支", "Delete branch"), detail: fmt.Sprintf(w.tr("删除本地分支 %s？未合并的提交会被拒绝删除。", "Delete local branch %s? Unmerged commits are refused."), name), danger: true})
			}
		}
	})
}

// ------------------------------------------------------------ table

func (w *workbench) logTable(c *ui.Context, p palette, state *api.GitState, commits []api.GitCommit, selected string, wide bool, tableWidth float32) {
	target := w.selected
	rows, lanes := w.graphFor(commits)
	graphWidth := float32(min(lanes, 8))*12 + 10
	now := time.Now()
	ui.Row(c.Key("log-head")).Height(28).Padding(0, 10, 0, 8+graphWidth).Gap(10).AlignItems(ui.Center).Shrink(0).
		Background(p.soft.Alpha(0.55)).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
		ui.Text(c, w.tr("提交", "Commit")).FontSize(11).FontWeight(500).TextColor(p.faint).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		if wide {
			ui.Text(c, w.tr("作者", "Author")).FontSize(11).FontWeight(500).TextColor(p.faint).Width(110).NoWrap().Shrink(0)
			ui.Text(c, w.tr("日期", "Date")).FontSize(11).FontWeight(500).TextColor(p.faint).Width(116).NoWrap().Shrink(0)
		} else {
			ui.Text(c, w.tr3("时间", "Age", "時間")).FontSize(11).FontWeight(500).TextColor(p.faint).Width(34).NoWrap().Shrink(0).TextAlign(ui.End)
		}
		ui.Text(c, w.tr("哈希", "Hash")).FontSize(11).FontWeight(500).TextColor(p.faint).Width(58).NoWrap().Shrink(0).TextAlign(ui.End)
	})
	ui.Scroll(c.Key("git-log:" + target)).Grow(1).MinHeight(0).Children(func() {
		if len(commits) == 0 {
			text := w.tr("没有符合条件的提交", "No commits match")
			if w.features.busy["git-log:"+target] {
				text = w.tr("正在读取日志…", "Reading the log…")
			}
			ui.Text(c, text).FontSize(12).TextColor(p.muted).Padding(14)
		}
		for i, commit := range commits {
			w.logRow(c, p, state, commit, rows[i], graphWidth, commit.ID == selected, now, wide, tableWidth, commits)
		}
	})
}

// logDate is IDEA's absolute log date, "今天 14:05" for today.
func (w *workbench) logDate(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.Local()
	y, m, d := now.Date()
	ty, tm, td := t.Date()
	switch {
	case y == ty && m == tm && d == td:
		return w.tr("今天 ", "Today ") + t.Format("15:04")
	case now.Sub(t) < 48*time.Hour && now.AddDate(0, 0, -1).Day() == td:
		return w.tr("昨天 ", "Yesterday ") + t.Format("15:04")
	}
	return t.Format("2006/01/02 15:04")
}

func (w *workbench) logRow(c *ui.Context, p palette, state *api.GitState, commit api.GitCommit, row graphRow, graphWidth float32, selected bool, now time.Time, wide bool, tableWidth float32, commits []api.GitCommit) {
	const rowHeight = 28
	short := nonEmptyString(commit.ShortID, shortCommitID(commit.ID))
	button := ui.Button(c.Key("commit:"+commit.ID), "").Label(commit.Subject+" · "+short).FillWidth().Height(rowHeight).
		Padding(0, 10, 0, 0).Radius(0).Border(0, p.line).Background(ui.Transparent).TextColor(p.ink).Justify(ui.Start)
	if selected {
		button.Background(p.accent.Alpha(0.2))
	} else if button.Hovered() {
		button.Background(p.rowHover)
	}
	mine := len(commit.Parents) > 1
	maxPills := 3
	maxRefW := float32(168)
	minSubjectW := float32(140)
	if !wide {
		availMid := tableWidth - graphWidth - 34 - 58 - 40
		switch {
		case availMid < 195:
			maxPills = 1
			maxRefW = 72
			minSubjectW = 54
		case availMid < 265:
			maxPills = 2
			if len(commit.Refs) > 1 && utf8.RuneCountInString(commit.Refs[0]) > 14 {
				maxPills = 1
			}
			maxRefW = 88
			minSubjectW = 76
		default:
			maxRefW = 104
			minSubjectW = 92
			if len(commit.Refs) > 2 {
				totalRunes := 0
				for i := 0; i < min(3, len(commit.Refs)); i++ {
					totalRunes += utf8.RuneCountInString(commit.Refs[i])
				}
				if totalRunes > 30 {
					maxPills = 2
				}
			}
		}
	}
	button.Children(func() {
		ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Clip().Children(func() {
			ui.Box(c).Size(graphWidth, rowHeight).Shrink(0).Role(ui.RoleImage).Label(w.tr("提交图", "Commit graph")).Draw(func(painter *ui.Painter, r ui.Rect) {
				x := func(lane int) float32 { return r.X + 8 + float32(min(lane, 7))*12 }
				top, mid, bottom := r.Y, r.Y+r.H/2, r.Y+r.H
				for _, lane := range row.through {
					painter.Line(x(lane), top, x(lane), bottom, 1.5, graphColor(p, lane))
				}
				for _, lane := range row.in {
					path := (&ui.Path{}).MoveTo(x(lane), top).CubeTo(x(lane), mid-4, x(row.col), mid-6, x(row.col), mid)
					painter.StrokePath(path, 1.5, graphColor(p, lane))
				}
				for _, lane := range row.out {
					path := (&ui.Path{}).MoveTo(x(row.col), mid).CubeTo(x(row.col), mid+6, x(lane), mid+4, x(lane), bottom)
					painter.StrokePath(path, 1.5, graphColor(p, lane))
				}
				color := graphColor(p, row.col)
				painter.Fill(ui.Rect{X: x(row.col) - 4.5, Y: mid - 4.5, W: 9, H: 9}, p.surface, 4.5)
				painter.Fill(ui.Rect{X: x(row.col) - 3.5, Y: mid - 3.5, W: 7, H: 7}, color, 3.5)
			})
			ui.Row(c).Grow(1).MinWidth(0).Gap(5).AlignItems(ui.Center).Clip().Children(func() {
				subject := ui.Text(c, commit.Subject).FontSize(12.5).SingleLine().Ellipsis("…").Shrink(1).MinWidth(minSubjectW)
				if mine {
					subject.TextColor(p.muted)
				} else {
					subject.TextColor(p.ink)
				}
				for i, ref := range commit.Refs {
					if i < maxPills {
						w.refPill(c, p, ref, maxRefW, true)
					} else {
						ui.Text(c, fmt.Sprintf("+%d", len(commit.Refs)-maxPills)).FontSize(10).TextColor(p.faint).NoWrap().Shrink(0)
						break
					}
				}
			})
			if wide {
				ui.Text(c, commit.Author).FontSize(11.5).TextColor(p.muted).SingleLine().Ellipsis("…").Width(110).Shrink(0)
				ui.Text(c, w.logDate(commit.AuthorTime, now)).FontSize(11.5).TextColor(p.muted).NoWrap().Width(116).Shrink(0)
			} else {
				ui.Text(c, relativeAge(commit.AuthorTime, now)).FontSize(11).TextColor(p.faint).NoWrap().Width(34).Shrink(0).TextAlign(ui.End)
			}
			ui.Text(c, short).Font("Menlo").FontSize(10.5).TextColor(p.faint).NoWrap().Width(58).Shrink(0).TextAlign(ui.End)
		})
	})
	if button.Clicked() {
		w.selectCommit(commit.ID)
	}
	button.ContextMenu(func(m *ui.Menu) { w.commitMenu(c, m, state, commit, commits) })
}

// commitMenu is IDEA's log context menu for one commit.
func (w *workbench) commitMenu(c *ui.Context, m *ui.Menu, state *api.GitState, commit api.GitCommit, commits []api.GitCommit) {
	_, active := w.activeRun(w.selected)
	locked := active || w.features.busy["git:"+w.selected]
	short := nonEmptyString(commit.ShortID, shortCommitID(commit.ID))
	head := len(state.Commits) > 0 && state.Commits[0].ID == commit.ID
	for _, ref := range commit.Refs {
		if strings.HasPrefix(ref, "HEAD") {
			head = true
		}
	}
	if m.Item(w.tr("复制修订号", "Copy revision number")).Chosen() {
		c.WriteClipboard(commit.ID)
	}
	if m.Item(w.tr("创建补丁（复制到剪贴板）", "Create patch (to clipboard)")).Chosen() {
		w.copyPatch(commit.ID)
	}
	if m.Item(w.tr("优选（Cherry-pick）", "Cherry-pick")).Disabled(locked || head).Chosen() {
		w.gitCommitOp("cherry-pick", commit.ID, w.tr("优选 ", "Cherry-pick ")+short, nil)
	}
	m.Separator()
	if m.Item(w.tr("签出修订", "Checkout revision")).Disabled(locked).Chosen() {
		w.gitCommitOp("checkout", commit.ID, w.tr("签出修订 ", "Checkout ")+short, nil)
	}
	m.Submenu(w.tr("将当前分支重置到此处", "Reset current branch to here"), func(sub *ui.Menu) {
		if sub.Item(w.tr("Soft：保留改动并暂存", "Soft: keep changes staged")).Disabled(locked).Chosen() {
			w.gitCommitOp("reset-soft", commit.ID, w.tr("软重置到 ", "Soft reset to ")+short, nil)
		}
		if sub.Item(w.tr("Mixed：保留改动不暂存", "Mixed: keep changes unstaged")).Disabled(locked).Chosen() {
			w.gitCommitOp("reset-mixed", commit.ID, w.tr("混合重置到 ", "Mixed reset to ")+short, nil)
		}
		if sub.Item(w.tr("Hard：丢弃所有改动…", "Hard: discard all changes…")).Danger().Disabled(locked).Chosen() {
			w.openGitPrompt(gitPrompt{kind: "reset-hard", commit: commit.ID, title: w.tr("硬重置当前分支", "Hard reset"), danger: true,
				detail: fmt.Sprintf(w.tr("将 %s 重置到 %s，并丢弃工作区与暂存区的全部改动。此操作无法撤销。", "Reset %s to %s and discard every uncommitted change. This cannot be undone."), state.Branch, short)})
		}
	})
	if m.Item(w.tr("还原提交（Revert）", "Revert commit")).Disabled(locked).Chosen() {
		w.gitCommitOp("revert", commit.ID, w.tr("还原 ", "Revert ")+short, nil)
	}
	if m.Item(w.tr("撤销提交", "Undo commit")).Disabled(locked || !head).Chosen() {
		w.gitCommitOp("undo", commit.ID, w.tr("撤销提交 ", "Undo commit ")+short, nil)
	}
	if m.Item(w.tr("编辑提交消息…", "Edit commit message…")).Disabled(locked || !head).Chosen() {
		w.openGitPrompt(gitPrompt{kind: "reword", commit: commit.ID, title: w.tr("编辑提交消息", "Edit commit message"), value: commit.Subject, hint: w.tr("提交说明", "Commit message")})
	}
	m.Separator()
	if m.Item(w.tr("新建分支…", "New branch…")).Disabled(locked).Chosen() {
		w.openGitPrompt(gitPrompt{kind: "branch", commit: commit.ID, title: w.tr("在此提交新建分支", "New branch at this commit"), detail: short + " · " + commit.Subject, hint: w.tr("新分支名", "New branch name")})
	}
	if m.Item(w.tr("新建标记…", "New tag…")).Disabled(locked).Chosen() {
		w.openGitPrompt(gitPrompt{kind: "tag", commit: commit.ID, title: w.tr("新建标记", "New tag"), detail: short + " · " + commit.Subject, hint: w.tr("标记名", "Tag name")})
	}
	m.Separator()
	index := -1
	for i := range commits {
		if commits[i].ID == commit.ID {
			index = i
		}
	}
	child := ""
	for i := index - 1; i >= 0 && index > 0; i-- {
		for _, parent := range commits[i].Parents {
			if parent == commit.ID {
				child = commits[i].ID
			}
		}
		if child != "" {
			break
		}
	}
	if m.Item(w.tr("转到子提交", "Go to child commit")).Disabled(child == "").Chosen() {
		w.selectCommit(child)
	}
	if m.Item(w.tr("转到父提交", "Go to parent commit")).Disabled(len(commit.Parents) == 0).Chosen() {
		w.selectCommit(commit.Parents[0])
	}
}

func (w *workbench) refPill(c *ui.Context, p palette, ref string, maxWidth float32, shrinkable bool) {
	text, tone, icon := ref, p.running, lucide("tag")
	switch {
	case strings.HasPrefix(ref, "HEAD -> "):
		text, tone = "HEAD → "+strings.TrimPrefix(ref, "HEAD -> "), p.accentInk
	case ref == "HEAD":
		tone = p.accentInk
	case strings.HasPrefix(ref, "tag: "):
		text, tone = strings.TrimPrefix(ref, "tag: "), p.warning
	case strings.Contains(ref, "/"):
		tone = p.success
	}
	pill := ui.Row(c).Height(18).Padding(0, 5).Gap(3).Radius(4).AlignItems(ui.Center).Background(tone.Alpha(0.1)).MaxWidth(maxWidth).Clip().Tooltip(text)
	if shrinkable {
		pill.Shrink(1).MinWidth(28)
	} else {
		pill.Shrink(0)
	}
	pill.Children(func() {
		ui.Icon(c, icon).FontSize(9).TextColor(tone).Shrink(0)
		ui.Text(c, text).FontSize(10.5).TextColor(tone).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
	})
}

// ------------------------------------------------------------ details

type fileGroup struct {
	dir   string
	files []api.GitFile
}

func groupByDirectory(files []api.GitFile) []fileGroup {
	var groups []fileGroup
	index := map[string]int{}
	for _, f := range files {
		dir := path.Dir(f.Path)
		if dir == "." {
			dir = ""
		}
		i, ok := index[dir]
		if !ok {
			i = len(groups)
			index[dir] = i
			groups = append(groups, fileGroup{dir: dir})
		}
		groups[i].files = append(groups[i].files, f)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].dir < groups[j].dir })
	return groups
}

func (w *workbench) commitDetailView(c *ui.Context, p palette, id string) {
	target := w.selected
	ui.Column(c.Key("commit-detail")).Grow(1).MinHeight(0).Children(func() {
		if id == "" {
			ui.Column(c).Grow(1).Justify(ui.Center).AlignItems(ui.Center).Gap(6).Children(func() {
				ui.Icon(c, logIcon).FontSize(22).TextColor(p.faint)
				ui.Text(c, w.tr("选择提交查看更改", "Select a commit to see its changes")).FontSize(12).TextColor(p.muted)
			})
			return
		}
		key := target + ":" + id
		detail := w.gitView.details[key]
		if detail == nil {
			ui.Row(c).Padding(12).Gap(8).AlignItems(ui.Center).Children(func() {
				activityIndicator(c, p.accent)
				ui.Text(c, w.tr("正在读取提交…", "Reading the commit…")).FontSize(12).TextColor(p.muted)
			})
			return
		}
		if detail.err != "" {
			ui.Text(c, detail.err).FontSize(12).TextColor(p.danger).Wrap().Padding(12)
			return
		}
		chosen := w.gitView.detailFile[key]
		if chosen == "" && len(detail.files) > 0 {
			chosen = detail.files[0].Path
		}
		// Changed files as IDEA's tree: a folder per directory with its count.
		ui.Scroll(c.Key("commit-files")).MaxHeight(220).Shrink(0).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
			ui.Column(c).FillWidth().Padding(6, 6).Children(func() {
				for _, group := range groupByDirectory(detail.files) {
					group := group
					folderKey := key + ":" + group.dir
					closed := w.gitView.dirClosed[folderKey]
					if group.dir != "" {
						folder := ui.Button(c.Key("detail-dir:"+group.dir), "").Label(group.dir).FillWidth().Height(22).Padding(0, 6).Gap(5).Radius(5).
							Border(0, p.line).Background(ui.Transparent).TextColor(p.ink).Justify(ui.Start).Expanded(!closed)
						if folder.Hovered() {
							folder.Background(p.soft)
						}
						folder.Children(func() {
							chevron := chevronIcon
							if closed {
								chevron = rightIcon
							}
							ui.Icon(c, chevron).FontSize(10).TextColor(p.faint)
							ui.Icon(c, folderIcon).FontSize(12).TextColor(p.muted)
							ui.Text(c, group.dir).FontSize(12).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
							ui.Text(c, fmt.Sprintf(w.tr("%d 个文件", plural(len(group.files), "%d file", "%d files")), len(group.files))).FontSize(11).TextColor(p.faint).NoWrap()
						})
						if folder.Clicked() {
							if w.gitView.dirClosed == nil {
								w.gitView.dirClosed = map[string]bool{}
							}
							w.gitView.dirClosed[folderKey] = !closed
						}
						if closed {
							continue
						}
					}
					for _, f := range group.files {
						f := f
						letter := nonEmptyString(strings.TrimSpace(f.Index), "M")
						indent := float32(24)
						if group.dir == "" {
							indent = 8
						}
						row := ui.Button(c.Key("detail-file:"+f.Path), "").Label(w.tr("查看提交文件 ", "Show commit file ")+f.Path).FillWidth().Height(22).
							Padding(0, 6, 0, indent).Gap(6).Radius(5).Border(0, p.line).Background(ui.Transparent).Justify(ui.Start)
						if f.Path == chosen {
							row.Background(p.accent.Alpha(0.16))
						} else if row.Hovered() {
							row.Background(p.soft)
						}
						row.Children(func() {
							ui.Icon(c, fileIcon).FontSize(11).TextColor(p.faint).Shrink(0)
							name := ui.Text(c, path.Base(f.Path)).FontSize(12).TextColor(gitStatusTone(p, letter)).SingleLine().Ellipsis("…").MinWidth(0)
							if letter == "D" {
								name.Strikethrough()
							}
						})
						if row.Clicked() {
							w.gitView.detailFile[key] = f.Path
						}
					}
				}
			})
		})
		ui.Scroll(c.Key("commit-diff")).Grow(1).MinHeight(60).Background(p.code).Children(func() {
			for _, patch := range detail.patches {
				if patch.name != chosen || patch.state == nil {
					continue
				}
				patch.state.Layout = editor.DiffUnified
				editor.DiffView(c, patch.state).FillWidth()
				return
			}
			if detail.raw != "" && len(detail.patches) == 0 {
				ui.Text(c, detail.raw).Font("Menlo").FontSize(11).TextColor(p.muted).Wrap().Selectable().Padding(8)
				return
			}
			ui.Text(c, w.tr("该文件没有可显示的文本差异", "No text diff for this file")).FontSize(11.5).TextColor(p.muted).Padding(10)
		})
		w.commitMessageBlock(c, p, detail)
	})
}

// commitMessageBlock is IDEA's commit details: message, hash, author with
// address and date, refs, and the branches that contain the commit.
func (w *workbench) commitMessageBlock(c *ui.Context, p palette, detail *gitCommitDetail) {
	commit := detail.commit
	ui.Scroll(c.Key("commit-message")).MaxHeight(200).Shrink(0).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
		ui.Column(c).FillWidth().Padding(10, 12).Gap(6).Children(func() {
			subject, body := commit.Subject, ""
			if detail.body != "" {
				subject, body, _ = strings.Cut(detail.body, "\n")
				body = strings.TrimSpace(body)
			}
			ui.Text(c, nonEmptyString(subject, shortCommitID(commit.ID))).FontSize(13).FontWeight(600).TextColor(p.ink).Wrap().Selectable()
			if body != "" {
				ui.Text(c, body).FontSize(12).LineHeight(1.5).TextColor(p.muted).Wrap().Selectable()
			}
			ui.Row(c).Gap(6).Wrap().AlignItems(ui.Center).Children(func() {
				hash := quietButton(c, p, "copy-commit-id", shortCommitID(commit.ID)).Label(w.tr("复制提交号", "Copy commit ID")).Height(20).Padding(0, 4).FontSize(11)
				hash.Font("Menlo")
				if hash.Clicked() {
					c.WriteClipboard(commit.ID)
				}
				who := commit.Author
				if detail.email != "" {
					who += " <" + detail.email + ">"
				}
				if who != "" {
					ui.Text(c, w.tr("作者 ", "by ")+who).FontSize(11.5).TextColor(p.muted).Selectable()
				}
				if !commit.AuthorTime.IsZero() {
					ui.Text(c, commit.AuthorTime.Local().Format("2006/01/02 15:04")).FontSize(11.5).TextColor(p.faint)
				}
			})
			if len(commit.Refs) > 0 {
				ui.Row(c).Gap(4).Wrap().Children(func() {
					for _, ref := range commit.Refs {
						w.refPill(c, p, ref, 260, false)
					}
				})
			}
			if n := len(detail.branches); n > 0 {
				shown := detail.branches
				if !w.gitView.allBranches && n > 3 {
					shown = shown[:3]
				}
				ui.Row(c).Gap(6).Wrap().AlignItems(ui.Center).Children(func() {
					ui.Text(c, fmt.Sprintf(w.tr("在 %d 个分支中：", "In %d branches: "), n)+strings.Join(shown, ", ")).FontSize(11.5).TextColor(p.muted).Wrap().Shrink(1).MinWidth(0)
					if n > 3 {
						label := w.tr("全部显示", "Show all")
						if w.gitView.allBranches {
							label = w.tr("收起", "Less")
						}
						if quietButton(c, p, "commit-all-branches", label).Height(20).Padding(0, 4).FontSize(11).TextColor(p.accentInk).Clicked() {
							w.gitView.allBranches = !w.gitView.allBranches
						}
					}
				})
			}
			if detail.truncated {
				ui.Text(c, w.tr("差异已截断", "Diff truncated")).FontSize(11).TextColor(p.warning)
			}
		})
	})
}

// ------------------------------------------------------------ prompt

func (w *workbench) gitPromptDialog(c *ui.Context, p palette) {
	prompt := &w.gitView.prompt
	if !prompt.open {
		return
	}
	width, _ := c.Size()
	ui.DialogBase(c, &prompt.open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000066"))
		panel.Width(min(420, width-32)).Padding(18).Radius(12).Background(p.raised).Border(1, p.line).Label(prompt.title)
		ui.Column(c).FillWidth().Gap(10).Children(func() {
			ui.Text(c, prompt.title).FontSize(14).FontWeight(600).TextColor(p.ink)
			if prompt.detail != "" {
				ui.Text(c, prompt.detail).FontSize(12).TextColor(p.muted).Wrap()
			}
			var input ui.Element
			if prompt.hint != "" {
				if prompt.kind == "reword" {
					input = ui.TextArea(c.Key("git-prompt-area"), &prompt.value).Label(prompt.hint).Lines(3, 8).FillWidth().FontSize(12.5).AutoFocus()
				} else {
					input = kitField(ui.TextInput(c.Key("git-prompt-input"), &prompt.value).Label(prompt.hint).Placeholder(prompt.hint).FillWidth().AutoFocus(), p, false)
				}
			}
			ui.Row(c).Gap(8).Children(func() {
				ui.Spacer(c)
				if quietButton(c, p, "git-prompt-cancel", w.tr("取消", "Cancel")).Height(28).Padding(0, 12).Clicked() {
					prompt.open = false
				}
				label := w.tr("确定", "OK")
				if prompt.danger {
					label = w.tr("确认执行", "Confirm")
				}
				ok := primaryAction(c, p, "git-prompt-ok", label).Height(28).Padding(0, 14)
				if prompt.danger {
					ok.Background(p.danger).TextColor(ui.Hex("#FFFFFF"))
				}
				value := strings.TrimSpace(prompt.value)
				if prompt.hint != "" && value == "" {
					ok.Disabled(true)
				}
				submitted := input.Submitted() && prompt.kind != "reword"
				if (ok.Clicked() || submitted) && (prompt.hint == "" || value != "") {
					w.runGitPrompt(*prompt, value)
					prompt.open = false
				}
			})
		})
	})
}

func (w *workbench) runGitPrompt(prompt gitPrompt, value string) {
	short := shortCommitID(prompt.commit)
	switch prompt.kind {
	case "branch":
		w.gitCommitOp("branch", prompt.commit, w.tr("新建分支 ", "New branch ")+value, map[string]string{"name": value})
	case "tag":
		w.gitCommitOp("tag", prompt.commit, w.tr("新建标记 ", "New tag ")+value, map[string]string{"name": value})
	case "reword":
		w.gitCommitOp("reword", prompt.commit, w.tr("编辑提交消息", "Edit commit message"), map[string]string{"message": value})
	case "reset-hard":
		w.gitCommitOp("reset-hard", prompt.commit, w.tr("硬重置到 ", "Hard reset to ")+short, nil)
	case "delete-branch":
		w.deleteBranch(prompt.branch, false)
	case "new-branch":
		w.gitAction(actionGitBranchCreate, w.tr("新建分支 ", "New branch ")+value, func(r *api.WorkbenchRequest) {
			r.Name, r.Branch, r.Message = value, prompt.branch, "checkout"
		}, nil)
	}
}
