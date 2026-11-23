package desktop

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"github.com/muq9738/Vantry/pkg/api"
)

// Git actions of the IDEA-style tool window. The checkout, branch and stash
// names are kept together so Core's wiring can follow them in one place.
const (
	actionGitDiscard        = "git.discard"
	actionGitLog            = "git.log"
	actionGitCommitDetail   = "git.commit_detail"
	actionGitCheckout       = "git.checkout"
	actionGitBranchCreate   = "git.branch.create"
	actionGitStashPop       = "git.stash.pop"
	actionGitWorktreeStatus = "git.worktree.status"
	actionGitWorktreeMerge  = "git.worktree.merge_cleanup"
	actionGitWorktreeClean  = "git.worktree.cleanup"
)

// gitCommitDetail is one commit of the log with its changed files and the
// parsed per-file diffs, parsed once when Core answers.
type gitCommitDetail struct {
	commit    api.GitCommit
	files     []api.GitFile
	patches   []patchFile
	raw       string
	truncated bool
	err       string
	// IDEA's details: the full message, the author's address and the
	// branches that contain the commit, when Core reports them.
	body, email string
	branches    []string
	isHead      bool
}

// ------------------------------------------------------------ decoding

func decodeGitLog(response api.WorkbenchResponse) ([]api.GitCommit, bool) {
	if response.Git != nil && response.Git.Commits != nil {
		return response.Git.Commits, true
	}
	for _, raw := range [][]byte{[]byte(strings.TrimSpace(response.Output)), response.Data} {
		if len(raw) == 0 {
			continue
		}
		var list []api.GitCommit
		if json.Unmarshal(raw, &list) == nil {
			return list, true
		}
		var wrapped struct {
			Commits []api.GitCommit `json:"commits"`
		}
		if json.Unmarshal(raw, &wrapped) == nil && wrapped.Commits != nil {
			return wrapped.Commits, true
		}
	}
	return nil, false
}

func decodeCommitDetail(response api.WorkbenchResponse, id string) *gitCommitDetail {
	detail := &gitCommitDetail{commit: api.GitCommit{ID: id, ShortID: shortCommitID(id)}}
	switch {
	case response.Git != nil:
		detail.files, detail.raw, detail.truncated = response.Git.Files, response.Git.Diff, response.Git.Truncated
		if len(response.Git.Commits) > 0 {
			detail.commit = response.Git.Commits[0]
		}
	default:
		text := strings.TrimSpace(response.Output)
		if text == "" {
			text = strings.TrimSpace(string(response.Data))
		}
		var payload struct {
			Commit      api.GitCommit `json:"commit"`
			Files       []api.GitFile `json:"files"`
			Diff        string        `json:"diff"`
			Truncated   bool          `json:"truncated"`
			Body        string        `json:"body"`
			AuthorEmail string        `json:"authorEmail"`
			Branches    []string      `json:"branches"`
			IsHead      bool          `json:"isHead"`
		}
		if strings.HasPrefix(text, "{") && json.Unmarshal([]byte(text), &payload) == nil {
			if payload.Commit.ID != "" {
				detail.commit = payload.Commit
			}
			detail.files, detail.raw, detail.truncated = payload.Files, payload.Diff, payload.Truncated
			detail.body, detail.email, detail.branches, detail.isHead = payload.Body, payload.AuthorEmail, payload.Branches, payload.IsHead
		} else {
			detail.raw = text
		}
	}
	detail.patches = parsePatchFiles(detail.raw)
	if len(detail.files) == 0 {
		for _, f := range detail.patches {
			detail.files = append(detail.files, api.GitFile{Path: f.name, Index: "M", Worktree: " "})
		}
	}
	return detail
}

// parsePatchFiles is patchFiles without the shared one-text cache, for
// diffs kept by their owner (a commit's detail).
func parsePatchFiles(text string) []patchFile {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	parsed, err := editor.ParsePatch(text)
	if err != nil {
		return nil
	}
	var files []patchFile
	for _, fp := range parsed {
		f := patchFile{name: nonEmptyString(fp.NewName, fp.OldName), fp: fp}
		for _, h := range fp.Hunks {
			for _, line := range h.Lines {
				switch {
				case strings.HasPrefix(line, "+"):
					f.added++
				case strings.HasPrefix(line, "-"):
					f.removed++
				}
			}
		}
		if !fp.Binary && len(fp.Hunks) > 0 {
			state := &editor.DiffState{Layout: editor.DiffUnified, FitContent: true, Wrap: true}
			state.SetPatch(fp)
			if lang := editor.LanguageFor(f.name); lang != nil {
				state.Old.SetLanguage(lang)
				state.New.SetLanguage(lang)
			}
			f.state = state
		}
		files = append(files, f)
	}
	return files
}

func shortCommitID(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

// ------------------------------------------------------------ changelist

type gitGroups struct{ staged, changes, untracked []api.GitFile }

func groupGitFiles(files []api.GitFile) gitGroups {
	var g gitGroups
	for _, f := range files {
		switch {
		case f.Index == "?":
			g.untracked = append(g.untracked, f)
			continue
		case f.Index != " " && f.Index != "":
			g.staged = append(g.staged, f)
		}
		if f.Worktree != " " && f.Worktree != "" {
			g.changes = append(g.changes, f)
		}
	}
	return g
}

func paths(files []api.GitFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func commitPaths(files []api.GitFile) []string {
	out := make([]string, 0, len(files)*2)
	for _, f := range files {
		out = append(out, f.Path)
		if f.OriginalPath != "" && strings.Contains(f.Index+f.Worktree, "R") {
			out = append(out, f.OriginalPath)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// gitStatusTone colors a porcelain letter as IDEA does: modified blue, added
// green, deleted red, renamed violet, untracked muted, conflicts amber.
func gitStatusTone(p palette, letter string) ui.Color {
	switch letter {
	case "M", "T":
		return p.running
	case "A":
		return p.success
	case "D":
		return p.danger
	case "R", "C":
		if p.dark {
			return ui.Hex("#A78BFA")
		}
		return ui.Hex("#7C3AED")
	case "U":
		return p.warning
	}
	return p.muted
}

func (w *workbench) checkedPaths(target string) []string {
	var out []string
	for path, on := range w.gitView.checked[target] {
		if on {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func (w *workbench) setChecked(target, path string, on bool) {
	if w.gitView.checked == nil {
		w.gitView.checked = map[string]map[string]bool{}
	}
	if w.gitView.checked[target] == nil {
		w.gitView.checked[target] = map[string]bool{}
	}
	if on {
		w.gitView.checked[target][path] = true
	} else {
		delete(w.gitView.checked[target], path)
	}
}

// checkBox is a compact list checkbox, IDEA's changelist inclusion mark.
func checkBox(c *ui.Context, p palette, key string, on *bool, label string, disabled bool) ui.Element {
	box := ui.CheckboxBase(c.Key(key), on).Label(label).Size(15, 15).Radius(4).Shrink(0).Justify(ui.Center).AlignItems(ui.Center)
	if disabled {
		box.Disabled(true)
	}
	switch {
	case *on:
		box.Background(p.accent).Border(1, p.accent)
	case box.Hovered():
		box.Background(ui.Transparent).Border(1.2, p.muted)
	default:
		box.Background(ui.Transparent).Border(1.2, p.faint)
	}
	checked := *on
	box.Children(func() {
		if checked {
			ui.Icon(c, checkIcon).FontSize(11).TextColor(ui.Hex("#FFFFFF"))
		}
	})
	return box
}

// pruneGitSelection drops checks and selections of files no longer changed.
func (w *workbench) pruneGitSelection(target string, state *api.GitState) {
	if state == nil || state.Files == nil {
		return
	}
	present := map[string]bool{}
	for _, f := range state.Files {
		present[f.Path] = true
	}
	for path := range w.gitView.checked[target] {
		if !present[path] {
			delete(w.gitView.checked[target], path)
		}
	}
	if key := w.gitView.selected[target]; key != "" {
		if _, path, _ := strings.Cut(key, ":"); !present[path] {
			delete(w.gitView.selected, target)
		}
	}
}

// ------------------------------------------------------------ actions

func (w *workbench) gitAction(action, label string, configure func(*api.WorkbenchRequest), after func()) bool {
	target := w.selected
	request, ok := w.gitRequest(target, action)
	if !ok {
		return false
	}
	if _, active := w.activeRun(target); active {
		w.setToolFeedback("git:"+target, notice{Text: "请等待任务结束后再执行 " + label + "。", Level: noticeWarn})
		return false
	}
	if state := w.features.git[target]; state != nil {
		request.Branch = state.Branch
	}
	if configure != nil {
		configure(&request)
	}
	w.workbenchCall("git:"+target, request, func(response api.WorkbenchResponse, err error) {
		if w.features.gitOutputs == nil {
			w.features.gitOutputs = map[string]string{}
		}
		w.features.gitOutputs[target] = response.Output
		if err != nil {
			w.setToolFeedback("git:"+target, notice{Text: describe(label+" 未完成", err), Level: noticeError})
		} else {
			w.setToolFeedback("git:"+target, notice{Text: label + " 已完成；已刷新实际状态。", Level: noticeInfo})
			if after != nil {
				after()
			}
		}
		delete(w.gitView.logLoaded, target)
		if !w.adoptGit(target, response) {
			w.refreshGit(target, true)
		}
	})
	return true
}

func (w *workbench) gitCheckout(branch string) bool {
	w.gitView.branchOpen = false
	return w.gitAction(actionGitCheckout, w.tr("签出 ", "Checkout ")+branch, func(r *api.WorkbenchRequest) { r.Branch, r.Name = branch, branch }, func() {
		w.recordRecentBranch(w.selected, branch)
	})
}

func (w *workbench) gitCreateBranch(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t\r\n~^:?*[\\") || strings.HasPrefix(name, "-") {
		w.setToolFeedback("git:"+w.selected, notice{Text: w.tr("请输入有效的分支名。", "Enter a valid branch name."), Level: noticeWarn})
		return false
	}
	return w.gitAction(actionGitBranchCreate, w.tr("新建分支 ", "New branch ")+name, func(r *api.WorkbenchRequest) { r.Name = name; r.Message = "checkout" }, func() {
		w.recordRecentBranch(w.selected, name)
		w.gitView.newBranch = ""
		w.gitView.branchOpen = false
	})
}

func (w *workbench) gitStashPop(ref string) bool {
	return w.gitAction(actionGitStashPop, "Stash Pop", func(r *api.WorkbenchRequest) { r.Name = ref }, nil)
}

func (w *workbench) gitDiscard(path string) {
	w.changeGitWith(actionGitDiscard, []string{path}, commitIntent{})
}

func (w *workbench) commitFromBox(push bool) {
	target := w.selected
	state := w.features.git[target]
	if state == nil {
		return
	}
	groups := groupGitFiles(state.Files)
	checked := w.checkedPaths(target)
	var selection []string
	switch {
	case len(checked) > 0:
		// Commit exactly what is staged plus the files checked in the list.
		var checkedFiles []api.GitFile
		checkedSet := map[string]bool{}
		for _, p := range checked {
			checkedSet[p] = true
		}
		for _, f := range state.Files {
			if checkedSet[f.Path] {
				checkedFiles = append(checkedFiles, f)
			}
		}
		selection = append(commitPaths(groups.staged), commitPaths(checkedFiles)...)
		sort.Strings(selection)
		selection = slices.Compact(selection)
	case len(groups.staged) == 0 && len(state.Files) > 0:
		// One-click commit when nothing was manually staged: stage & commit all changed files.
		selection = commitPaths(state.Files)
	}
	message := strings.TrimSpace(w.gitView.messages[target])
	if message == "" {
		message = w.autoCommitSummaryForSelection(state, selection, w.widgets.locale)
	}
	if message == "" {
		return
	}
	w.changeGitWith("git.commit", selection, commitIntent{message: message, amend: w.gitView.amend, push: push})
}

func (w *workbench) selectCommit(id string) {
	target := w.selected
	if w.gitView.commit == nil {
		w.gitView.commit, w.gitView.details, w.gitView.detailFile = map[string]string{}, map[string]*gitCommitDetail{}, map[string]string{}
	}
	w.gitView.commit[target] = id
	key := target + ":" + id
	if w.gitView.details[key] != nil {
		return
	}
	request, ok := w.gitRequest(target, actionGitCommitDetail)
	if !ok {
		return
	}
	request.Name = id
	w.workbenchCall("git-detail:"+key, request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.gitView.details[key] = &gitCommitDetail{commit: api.GitCommit{ID: id, ShortID: shortCommitID(id)}, err: describe(w.tr("读取提交详情失败", "Reading the commit failed"), err)}
			return
		}
		w.gitView.details[key] = decodeCommitDetail(response, id)
	})
}

// ------------------------------------------------------------ view

func (w *workbench) dockContentWidth(c *ui.Context, mode dockMode) float32 {
	window, _ := c.Size()
	main := w.mainStageWidth(c, window < narrowWidth)
	raw := main
	if mode == dockSide {
		raw = w.dockWidth(main)
	}
	return max(float32(260), raw-28)
}

// ideaGitPanel is the Git tool window: a single consolidated header bar (page
// tabs + icon sync actions, plus branch pill when not already shown in the
// top-left header), then Commit (with clean empty-state), Log, or Compare.
func (w *workbench) ideaGitPanel(c *ui.Context, p palette, mode dockMode) {
	target := w.selected
	state := w.features.git[target]
	_, active := w.activeRun(target)
	busy := w.features.busy["git:"+target]
	winW, _ := c.Size()
	narrowWin := winW < narrowWidth
	width := w.dockContentWidth(c, mode)
	if w.gitView.expanded {
		width = max(float32(260), winW-32)
	}
	showBranchHere := state != nil && (!w.headerShowsBranchPill(narrowWin) || w.gitView.expanded)
	compact := width < 680
	iconOnlySync := width < 640

	if state == nil {
		ui.Row(c.Key("git-toolbar")).Gap(6).Padding(0, 0, 2).AlignItems(ui.Center).Shrink(0).Role(ui.RoleToolbar).Label(w.tr("Git 工具栏", "Git toolbar")).Children(func() {
			ui.Icon(c, branchIcon).FontSize(13).TextColor(p.muted)
			text := w.tr("尚未读取 Git 状态", "Git state not read yet")
			if busy {
				text = w.tr("正在读取 Git…", "Reading Git…")
			}
			ui.Text(c, text).FontSize(11.5).TextColor(p.muted).NoWrap()
			ui.Spacer(c)
			w.gitRefreshButton(c, p, busy)
		})
		if output := w.features.gitOutputs[target]; output != "" {
			ui.Scroll(c.Key("git-output:" + target)).Grow(1).MinHeight(0).Children(func() {
				ui.Text(c, w.tr("合并结果", "Merge result")).FontSize(11).Bold().TextColor(p.muted)
				ui.Text(c, output).FontSize(11).TextColor(p.ink).Selectable().Wrap()
			})
		}
		return
	}
	if state.Files == nil {
		ui.Row(c.Key("git-toolbar")).Gap(6).Padding(0, 0, 2).Wrap().AlignItems(ui.Center).Shrink(0).Role(ui.RoleToolbar).Label(w.tr("Git 工具栏", "Git toolbar")).Children(func() {
			if showBranchHere {
				w.gitBranchPill(c, p, state, active || busy, compact, iconOnlySync)
			}
			ui.Spacer(c)
			w.gitSyncBar(c, p, state, compact, iconOnlySync)
			w.gitRefreshButton(c, p, busy)
		})
		w.gitPushConfirm(c, p, state)
		w.legacyGitView(c, p, state)
		return
	}

	groups := groupGitFiles(state.Files)
	// Single consolidated top bar: Page tabs (+ dock branch pill when needed) on left, compact sync icons + refresh on right.
	ui.Row(c.Key("git-toolbar")).Gap(6).Padding(0, 0, 2).Wrap().AlignItems(ui.Center).Shrink(0).Role(ui.RoleToolbar).Label(w.tr("Git 工具栏", "Git toolbar")).Children(func() {
		w.gitPageTabStrip(c, p, len(state.Files))
		if showBranchHere {
			w.gitBranchPill(c, p, state, active || busy, compact, iconOnlySync)
		}
		ui.Spacer(c)
		w.prBadge(c, p, target)
		w.gitSyncBar(c, p, state, compact, iconOnlySync)
		if w.gitView.tab != 1 {
			w.gitRefreshButton(c, p, busy)
		}
	})
	w.gitPushConfirm(c, p, state)
	w.worktreeBanner(c, p, target, active || busy)
	if output := w.features.gitOutputs[target]; output != "" {
		ui.Text(c, firstLine(output)).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…").Tooltip(output).Shrink(0)
	}
	switch w.gitView.tab {
	case 1:
		w.gitLogPage(c, p, state, width)
	case 2:
		w.gitComparePage(c, p, state, width, active || busy)
	default:
		w.gitCommitPage(c, p, state, groups, width, active || busy)
	}
}

func (w *workbench) gitRefreshButton(c *ui.Context, p palette, busy bool) {
	refresh := iconButton(c, p, "git-refresh", w.tr("刷新差异", "Refresh diff"), refreshIcon).Size(26, 26).Padding(5)
	if busy {
		refresh.Disabled(true)
	}
	if refresh.Clicked() {
		w.loadGit()
	}
}

func (w *workbench) gitPageTabStrip(c *ui.Context, p palette, changes int) {
	pages := []struct {
		id                       int
		zh, en, labelZh, labelEn string
		icon                     *ui.SVG
	}{
		{0, "提交", "Commit", "Git 提交页", "Git commit page", commitIcon},
		{1, "日志", "Log", "Git 日志页", "Git log page", logIcon},
	}
	if w.gitView.compareTarget != "" || w.gitView.tab == 2 {
		pages = append(pages, struct {
			id                       int
			zh, en, labelZh, labelEn string
			icon                     *ui.SVG
		}{2, "比较", "Compare", "Git 比较页", "Git compare page", lucide("arrow-right-left")})
	}
	ui.Row(c.Key("git-pages-tabs")).Height(28).Padding(2).Gap(2).Radius(8).Background(p.soft).AlignItems(ui.Center).Shrink(0).
		Role(ui.RoleTabList).Label(w.tr("Git 页面", "Git pages")).Children(func() {
		for _, page := range pages {
			page := page
			selected := w.gitView.tab == page.id
			tab := ui.Button(c.Key(fmt.Sprintf("git-page:%d", page.id)), "").Label(w.tr(page.labelZh, page.labelEn)).Height(24).Padding(0, 9).
				Border(0, p.line).TextColor(p.muted).Role(ui.RoleTab).Selected(selected).Radius(6).Background(ui.Transparent).Shrink(0)
			if selected {
				tab.Background(p.raised).TextColor(p.ink)
				if !p.dark {
					tab.Border(1, p.lineSoft)
				}
			} else if tab.Hovered() {
				tab.Background(p.lineSoft).TextColor(p.ink)
			}
			tab.Children(func() {
				ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
					iconTone := p.muted
					if selected {
						iconTone = p.accentInk
					}
					ui.Icon(c, page.icon).FontSize(12).TextColor(iconTone).Shrink(0)
					ui.Text(c, w.tr(page.zh, page.en)).FontSize(11.5).FontWeight(500).NoWrap()
					if page.id == 0 && changes > 0 {
						badgeBg := p.lineSoft
						if selected {
							badgeBg = p.soft
						}
						ui.Text(c, fmt.Sprint(changes)).FontSize(10).TextColor(p.muted).Padding(1, 5).Radius(7).Background(badgeBg).NoWrap()
					}
				})
			})
			if tab.Clicked() {
				w.gitView.tab = page.id
				if page.id == 1 && !w.features.busy["git-log:"+w.selected] {
					target := w.selected
					if _, loaded := w.gitView.logLoaded[target]; !loaded {
						w.loadGitLog(w.gitView.logQuery)
					}
				}
			}
		}
	})
}

// gitBranchPill delegates to the shared IDEA branch popover anchored on the dock.
func (w *workbench) gitBranchPill(c *ui.Context, p palette, state *api.GitState, locked, compact, narrow bool) {
	w.ideaBranchTrigger(c, p, "dock", state.Branch, state, locked, compact, narrow, ui.AnchorBottomStart, ui.AnchorTopStart)
}

// stashPopButton lists the stashes to pop once there are any.
func (w *workbench) stashPopButton(c *ui.Context, p palette, state *api.GitState, locked bool) {
	if state == nil || len(state.Stashes) == 0 {
		return
	}
	pop := quietButton(c, p, "stash-pop", "").Label(fmt.Sprintf(w.tr("恢复储藏（%d）", "Pop stash (%d)"), len(state.Stashes))).Height(24).Padding(0, 7)
	pop.Children(func() {
		ui.Icon(c, pullIcon).FontSize(11.5).Shrink(0)
		ui.Text(c, fmt.Sprintf("Pop %d", len(state.Stashes))).FontSize(11).NoWrap()
	})
	if locked {
		pop.Disabled(true)
	}
	pop.PopoverMenuButton(func(m *ui.Menu) {
		for _, stash := range state.Stashes {
			label := stash.Ref + " · " + nonEmptyString(stash.Message, stash.Branch)
			if m.Item(label).Chosen() {
				w.gitStashPop(stash.Ref)
			}
		}
	})
}

// legacyGitView is the plain status and diff of a Core that reports no
// structured file list.
func (w *workbench) legacyGitView(c *ui.Context, p palette, state *api.GitState) {
	ui.Scroll(c.Key("git-diff:" + w.selected)).Grow(1).MinHeight(0).Gap(6).Children(func() {
		if output := w.features.gitOutputs[w.selected]; output != "" {
			ui.Text(c, w.tr("合并结果", "Merge result")).FontSize(11).Bold().TextColor(p.muted)
			ui.Text(c, output).FontSize(11).TextColor(p.ink).Selectable().Wrap()
		}
		status := state.Status
		if status == "" {
			status = w.tr("工作区干净", "Working tree clean")
		}
		ui.Text(c, status).FontSize(11).TextColor(p.muted).Selectable().Wrap()
		for _, line := range strings.Split(state.Diff, "\n") {
			if line == "" {
				continue
			}
			color := p.ink
			switch {
			case strings.HasPrefix(line, "+"):
				color = p.success
			case strings.HasPrefix(line, "-"):
				color = p.danger
			case strings.HasPrefix(line, "@@"):
				color = p.accentInk
			}
			ui.Text(c, line).Font("Menlo").FontSize(11).TextColor(color).Selectable().Wrap()
		}
	})
}

// ------------------------------------------------------------ commit page

func (w *workbench) gitCleanEmptyState(c *ui.Context, p palette, state *api.GitState, locked bool) {
	syncLine := w.tr("工作区无未提交更改", "No uncommitted local changes")
	if state.RemoteBranch != "" {
		switch {
		case state.Ahead > 0 && state.Behind > 0:
			syncLine = fmt.Sprintf(w.tr("与 %s 存在分叉（↑%d / ↓%d）", "Diverged from %s (↑%d / ↓%d)"), state.RemoteBranch, state.Ahead, state.Behind)
		case state.Ahead > 0:
			syncLine = fmt.Sprintf(w.tr("领先 %s %d 个提交，可以推送", "%d commits ahead of %s"), state.RemoteBranch, state.Ahead)
		case state.Behind > 0:
			syncLine = fmt.Sprintf(w.tr("落后 %s %d 个提交，可以拉取更新", "%d commits behind %s"), state.RemoteBranch, state.Behind)
		default:
			syncLine = fmt.Sprintf(w.tr("与 %s 保持同步", "Up to date with %s"), state.RemoteBranch)
		}
	}

	ui.Column(c.Key("git-clean-empty")).Grow(1).MinHeight(0).Justify(ui.Center).AlignItems(ui.Center).Padding(24, 16).Gap(14).Children(func() {
		ui.Row(c).Size(44, 44).Radius(22).Justify(ui.Center).AlignItems(ui.Center).Background(p.success.Alpha(0.12)).Border(1, p.success.Alpha(0.28)).Children(func() {
			ui.Icon(c, checkIcon).FontSize(20).TextColor(p.success)
		})
		ui.Column(c).Gap(4).AlignItems(ui.Center).Children(func() {
			ui.Text(c, w.tr("工作区干净", "Working tree clean")).FontSize(14).FontWeight(600).TextColor(p.ink)
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				if state.Branch != "" {
					ui.Text(c, state.Branch).Font("Menlo").FontSize(11.5).TextColor(p.accentInk)
					ui.Text(c, "·").FontSize(11).TextColor(p.faint)
				}
				ui.Text(c, syncLine).FontSize(11.5).TextColor(p.muted)
			})
		})
		ui.Row(c).Gap(8).Wrap().Justify(ui.Center).AlignItems(ui.Center).Children(func() {
			if state.Behind > 0 {
				pullBtn := primaryAction(c, p, "clean-pull", fmt.Sprintf(w.tr("拉取更新 (↓%d)", "Pull (↓%d)"), state.Behind))
				pullBtn.Disabled(locked)
				if pullBtn.Clicked() {
					w.gitRemote(actionGitPull)
				}
			}
			if state.Ahead > 0 {
				var pushBtn ui.Element
				if state.Behind == 0 {
					pushBtn = primaryAction(c, p, "clean-push", fmt.Sprintf(w.tr("推送提交 (↑%d)", "Push (↑%d)"), state.Ahead))
				} else {
					pushBtn = outlineButton(c, p, "clean-push", fmt.Sprintf(w.tr("推送提交 (↑%d)", "Push (↑%d)"), state.Ahead))
				}
				pushBtn.Disabled(locked)
				if pushBtn.Clicked() {
					w.gitView.pushConfirm = w.selected
				}
			}
			if outlineButton(c, p, "clean-view-log", w.tr("查看提交日志", "View commit log")).Clicked() {
				w.gitView.tab = 1
				if _, loaded := w.gitView.logLoaded[w.selected]; !loaded {
					w.loadGitLog(w.gitView.logQuery)
				}
			}
			var compareCandidate string
			for _, b := range state.Branches {
				if b != "" && b != state.Branch {
					compareCandidate = b
					break
				}
			}
			if compareCandidate != "" {
				if outlineButton(c, p, "clean-compare", w.tr("与其他分支比较", "Compare branches")).Clicked() {
					w.openBranchCompare(state.Branch, compareCandidate)
				}
			}
			if len(state.Stashes) > 0 {
				popBtn := outlineButton(c, p, "clean-stash-pop", fmt.Sprintf(w.tr("恢复储藏 (%d)", "Pop stash (%d)"), len(state.Stashes)))
				popBtn.Disabled(locked)
				if popBtn.Clicked() {
					w.gitStashPop(state.Stashes[0].Ref)
				}
			}
		})
		if quietButton(c, p, "clean-amend", w.tr("修正上次提交 (Amend)", "Amend last commit")).Height(24).FontSize(11).TextColor(p.faint).Clicked() {
			w.gitView.amend = true
			if w.gitView.messages == nil {
				w.gitView.messages = map[string]string{}
			}
			if strings.TrimSpace(w.gitView.messages[w.selected]) == "" && len(state.Commits) > 0 {
				w.gitView.messages[w.selected] = state.Commits[0].Subject
			}
			w.gitView.messageFocus.Focus()
		}
	})
}

func (w *workbench) gitCommitPage(c *ui.Context, p palette, state *api.GitState, groups gitGroups, width float32, locked bool) {
	if len(state.Files) == 0 && !w.gitView.amend {
		w.gitCleanEmptyState(c, p, state, locked)
		return
	}
	target := w.selected
	if w.gitView.selected == nil {
		w.gitView.selected = map[string]string{}
	}
	if w.gitView.messages == nil {
		w.gitView.messages = map[string]string{}
	}
	unstaged := append(paths(groups.changes), paths(groups.untracked)...)
	ui.Row(c.Key("changelist-toolbar")).Gap(6).Padding(2, 0).AlignItems(ui.Center).Shrink(0).Wrap().Children(func() {
		ui.Text(c, fmt.Sprintf(w.tr("变更文件 · %d", "Changes · %d"), len(state.Files))).FontSize(11).FontWeight(600).TextColor(p.muted).NoWrap()
		ui.Spacer(c)
		stageAll := quietButton(c, p, "stage-all", w.tr("全选暂存", "Stage all")).Label(w.tr("暂存全部", "Stage all")).Height(24).Padding(0, 7).FontSize(11).NoWrap()
		if locked || len(unstaged) == 0 {
			stageAll.Disabled(true)
		}
		if stageAll.Clicked() {
			w.changeGit("git.stage", unstaged, "")
		}
		unstageAll := quietButton(c, p, "unstage-all", w.tr("取消全暂存", "Unstage all")).Label(w.tr("取消全部暂存", "Unstage all")).Height(24).Padding(0, 7).FontSize(11).NoWrap()
		if locked || len(groups.staged) == 0 {
			unstageAll.Disabled(true)
		}
		if unstageAll.Clicked() {
			w.changeGit("git.unstage", paths(groups.staged), "")
		}
		review := quietButton(c, p, "open-commit", w.tr("提交已暂存", "Commit staged")).Height(24).Padding(0, 7).FontSize(11).NoWrap()
		if locked || len(groups.staged) == 0 || state.Conflicts {
			review.Disabled(true)
		}
		if review.Clicked() {
			w.gitView.messageFocus.Focus()
		}
	})
	_, height := c.Size()
	wide := width >= 720
	ui.Column(c.Key("commit-page")).Grow(1).Basis(0).MinHeight(0).Gap(8).Children(func() {
		if wide {
			ui.Row(c).Grow(1).Basis(0).MinHeight(0).Gap(10).AlignItems(ui.Stretch).Children(func() {
				ui.Scroll(c.Key("git-files:" + target)).Width(max(260, min(420, width*0.36))).Shrink(0).Children(func() {
					w.changelist(c, p, state, groups, locked)
				})
				ui.Column(c).Grow(1).MinWidth(0).Children(func() { w.gitDiffPane(c, p, state, groups, width >= 960, locked) })
			})
		} else {
			rows := len(groups.staged) + len(groups.changes) + len(groups.untracked) + 3
			listHeight := min(float32(rows)*27+8, max(float32(96), height*0.26))
			if height < 680 {
				listHeight = min(float32(rows)*26+6, max(float32(110), height*0.24))
			}
			// Short stages give way list first, then the diff; the commit box
			// below always keeps its room.
			ui.Scroll(c.Key("git-files:" + target)).Height(listHeight).Shrink(1).MinHeight(0).Children(func() {
				w.changelist(c, p, state, groups, locked)
			})
			ui.Column(c).Grow(1).Basis(0).MinHeight(0).Clip().Children(func() { w.gitDiffPane(c, p, state, groups, false, locked) })
		}
		w.commitBox(c, p, state, groups, locked)
	})
	if state.Truncated {
		ui.Text(c, w.tr("仅显示部分变更；可在终端检查完整结果。", "Showing part of the changes; check the terminal for the rest.")).FontSize(10).TextColor(p.warning).Shrink(0)
	}
}

func (w *workbench) changelist(c *ui.Context, p palette, state *api.GitState, groups gitGroups, locked bool) {
	if len(state.Files) == 0 {
		ui.Column(c).Gap(4).Padding(16, 8).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, checkIcon).FontSize(18).TextColor(p.success)
			ui.Text(c, w.tr("工作区干净", "Working tree clean")).FontSize(12).TextColor(p.muted)
		})
		return
	}
	for _, group := range []struct {
		key, zh, en string
		files       []api.GitFile
		staged      bool
	}{
		{"staged", "已暂存", "Staged", groups.staged, true},
		{"changes", "更改", "Changes", groups.changes, false},
		{"untracked", "未版本控制", "Untracked", groups.untracked, false},
	} {
		if len(group.files) == 0 {
			continue
		}
		if w.gitView.closed == nil {
			w.gitView.closed = map[string]bool{}
		}
		closed := w.gitView.closed[group.key]
		head := ui.Button(c.Key("group:"+group.key), "").Label(fmt.Sprintf("%s (%d)", w.tr(group.zh, group.en), len(group.files))).Expanded(!closed).
			FillWidth().Height(26).Padding(0, 4).Radius(6).Border(0, p.line).Background(ui.Transparent).TextColor(p.muted)
		head.Children(func() {
			ui.Row(c).Gap(5).AlignItems(ui.Center).FillWidth().Children(func() {
				icon := chevronIcon
				if closed {
					icon = rightIcon
				}
				ui.Icon(c, icon).FontSize(10).TextColor(p.faint)
				ui.Text(c, w.tr(group.zh, group.en)).FontSize(11).Bold().TextColor(p.ink)
				ui.Text(c, fmt.Sprint(len(group.files))).FontSize(10).TextColor(p.faint)
			})
		})
		if head.Clicked() {
			w.gitView.closed[group.key] = !closed
		}
		if closed {
			continue
		}
		for _, file := range group.files {
			w.changelistRow(c, p, file, group.key, group.staged, locked)
		}
	}
}

func (w *workbench) changelistRow(c *ui.Context, p palette, file api.GitFile, group string, staged, locked bool) {
	target := w.selected
	key := fmt.Sprintf("%t:%s", staged, file.Path)
	selected := w.gitView.selected[target] == key
	letter := file.Worktree
	if staged {
		letter = file.Index
	}
	if group == "untracked" {
		letter = "?"
	}
	row := ui.Row(c.Key(group+":"+file.Path)).Height(27).Padding(0, 2, 0, 4).Gap(4).Radius(6).AlignItems(ui.Center)
	hovered := row.Hovered()
	if selected {
		row.Background(p.selected)
	} else if hovered {
		row.Background(p.rowHover)
	}
	row.ContextMenu(func(m *ui.Menu) {
		if !staged && m.Item(w.tr("暂存", "Stage")).Disabled(locked).Chosen() {
			w.changeGit("git.stage", []string{file.Path}, "")
		}
		if staged && m.Item(w.tr("取消暂存", "Unstage")).Disabled(locked).Chosen() {
			w.changeGit("git.unstage", []string{file.Path}, "")
		}
		if m.Item(w.tr("回滚…", "Rollback…")).Disabled(locked).Chosen() {
			w.gitView.selected[target] = key
			w.gitView.pushConfirm = ""
			w.setDiscardConfirm(target, file.Path)
		}
		m.Separator()
		if m.Item(w.tr("复制路径", "Copy path")).Chosen() {
			c.WriteClipboard(file.Path)
		}
	})
	row.Children(func() {
		if staged {
			on := true
			checkBox(c, p, "include", &on, w.tr("包含于提交 ", "Included ")+file.Path, locked)
			if !on {
				w.changeGit("git.unstage", []string{file.Path}, "")
			}
		} else {
			on := w.gitView.checked[target][file.Path]
			checkBox(c, p, "include", &on, w.tr("选择提交 ", "Include ")+file.Path, locked)
			if on != w.gitView.checked[target][file.Path] {
				w.setChecked(target, file.Path, on)
			}
		}
		pick := ui.Button(c.Key("select"), "").Label(file.Path).Grow(1).MinWidth(0).Height(25).Padding(0, 4).Radius(5).
			Border(0, p.line).Background(ui.Transparent).TextColor(p.ink)
		pick.Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).FillWidth().Children(func() {
				tone := gitStatusTone(p, letter)
				ui.Text(c, letter).Font("Menlo").FontSize(10).Bold().TextColor(tone).Width(14).TextAlign(ui.Center).Shrink(0)
				dir, name := path.Split(file.Path)
				nameText := ui.Text(c, name).FontSize(12).Bold().TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
				if letter == "D" {
					nameText.Strikethrough().TextColor(p.muted)
				}
				if dir = strings.TrimSuffix(dir, "/"); dir != "" {
					ui.Text(c, dir).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…").Shrink(4).MinWidth(0)
				}
				if file.OriginalPath != "" {
					ui.Text(c, "← "+path.Base(file.OriginalPath)).FontSize(10).TextColor(p.faint).SingleLine().Shrink(3).MinWidth(0)
				}
			})
		})
		if pick.Clicked() {
			w.gitView.selected[target] = key
		}
		if (hovered || selected) && !staged {
			rollback := iconButton(c, p, "rollback", w.tr("回滚 ", "Rollback ")+file.Path, rollbackIcon).Size(22, 22).Padding(4)
			if locked {
				rollback.Disabled(true)
			}
			if rollback.Clicked() {
				w.setDiscardConfirm(target, file.Path)
			}
		}
		verb, action, symbol := w.tr("暂存 ", "Stage "), "git.stage", "+"
		if staged {
			verb, action, symbol = w.tr("取消暂存 ", "Unstage "), "git.unstage", "−"
		}
		toggle := quietButton(c, p, "stage", symbol).Label(verb+file.Path).Size(22, 22).Padding(0)
		if locked {
			toggle.Disabled(true)
		}
		if toggle.Clicked() {
			w.changeGit(action, []string{file.Path}, "")
		}
	})
	if w.discardPending(target) == file.Path && !staged {
		ui.Row(c.Key("discard:"+file.Path)).Gap(6).Padding(4, 8).Radius(7).AlignItems(ui.Center).Background(p.danger.Alpha(0.08)).Border(1, p.danger.Alpha(0.3)).Children(func() {
			ui.Icon(c, rollbackIcon).FontSize(12).TextColor(p.danger)
			text := w.tr("回滚此文件的本地修改？", "Discard local changes to this file?")
			if group == "untracked" {
				text = w.tr("删除这个未版本控制的文件？", "Delete this untracked file?")
			}
			ui.Text(c, text).FontSize(11).TextColor(p.ink).Grow(1).MinWidth(0).Wrap()
			if quietButton(c, p, "cancel-discard", w.tr("取消", "Cancel")).Height(22).Clicked() {
				w.setDiscardConfirm(target, "")
			}
			confirm := ui.Button(c.Key("confirm-discard"), w.tr("回滚", "Rollback")).Label(w.tr("确认回滚 ", "Confirm rollback ")+file.Path).Height(22).Padding(0, 9).TextColor(p.danger)
			if locked {
				confirm.Disabled(true)
			}
			if confirm.Clicked() {
				w.setDiscardConfirm(target, "")
				w.gitDiscard(file.Path)
			}
		})
	}
}

func (w *workbench) discardPending(target string) string { return w.gitView.discard[target] }

func (w *workbench) setDiscardConfirm(target, file string) {
	if w.gitView.discard == nil {
		w.gitView.discard = map[string]string{}
	}
	if file == "" {
		delete(w.gitView.discard, target)
		return
	}
	w.gitView.discard[target] = file
}

// gitDiffLayout is the diff layout: the user's choice, else side by side
// once the pane is wide enough.
func (w *workbench) gitDiffLayout(c *ui.Context, p palette, wide bool) editor.DiffLayout {
	current := 0
	if w.gitView.layoutSet {
		current = w.gitView.layout
	} else if wide {
		current = 1
	}
	before := current
	ui.Segmented(c.Key("git-diff-layout"), &current, w.tr("统一", "Unified"), w.tr("并排", "Split")).Label(w.tr("Git 差异布局", "Git diff layout"))
	if current != before {
		w.gitView.layout, w.gitView.layoutSet = current, true
	}
	if current == 1 {
		return editor.DiffSplit
	}
	return editor.DiffUnified
}

func (w *workbench) worktreeBanner(c *ui.Context, p palette, target string, locked bool) {
	wt := w.gitView.worktree[target]
	if wt == nil || !wt.IsWorktree || !wt.Exists {
		return
	}
	ui.Column(c.Key("git-worktree-banner")).FillWidth().Padding(8, 10).Gap(6).Radius(9).
		Background(p.soft).Border(1, p.accent.Alpha(0.35)).Shrink(0).Children(func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
			ui.Icon(c, forkIcon).FontSize(12.5).TextColor(p.accentInk)
			route := wt.Branch
			if wt.BaseBranch != "" {
				route += " → " + wt.BaseBranch
			}
			ui.Text(c, w.tr("独立 Worktree：", "Isolated worktree: ")+route).FontSize(11.5).FontWeight(600).TextColor(p.ink).Grow(1).MinWidth(0)
			if wt.UnmergedCommits > 0 {
				ui.Text(c, fmt.Sprintf(w.tr("%d 个未合并提交", "%d unmerged"), wt.UnmergedCommits)).
					FontSize(10.5).TextColor(p.accentInk).Padding(1, 6).Radius(6).Background(p.accent.Alpha(0.12)).NoWrap()
			}
		})
		if wt.Note != "" {
			tone := p.muted
			if wt.Dirty {
				tone = p.warning
			}
			ui.Text(c, wt.Note).FontSize(11).TextColor(tone).Wrap()
		}
		confirm := ""
		if w.gitView.worktreeConfirm != nil {
			confirm = w.gitView.worktreeConfirm[target]
		}
		ui.Row(c).Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
			if confirm == "merge" {
				ui.Text(c, w.tr("合并到主分支并移除当前 Worktree？", "Merge into base branch and clean up worktree?")).FontSize(11).TextColor(p.ink).Grow(1)
				if quietButton(c, p, "git-wt-cancel", w.tr("取消", "Cancel")).Height(24).Padding(0, 8).Clicked() {
					delete(w.gitView.worktreeConfirm, target)
				}
				doMerge := ui.PrimaryButton(c.Key("git-wt-merge-confirm"), w.tr("确认合并并清理", "Confirm merge & cleanup")).Height(24).Padding(0, 9).Disabled(locked || !wt.CanMerge)
				if doMerge.Clicked() {
					w.mergeAndCleanupWorktree(target)
				}
				return
			}
			if confirm == "cleanup" {
				msg := w.tr("确认删除该隔离 Worktree？", "Remove this isolated worktree?")
				if wt.Dirty {
					msg = w.tr("工作区存在未提交改动，将强制清理该 Worktree？", "Force-remove dirty worktree?")
				}
				ui.Text(c, msg).FontSize(11).TextColor(p.danger).Grow(1)
				if quietButton(c, p, "git-wt-cancel", w.tr("取消", "Cancel")).Height(24).Padding(0, 8).Clicked() {
					delete(w.gitView.worktreeConfirm, target)
				}
				doClean := ui.Button(c.Key("git-wt-cleanup-confirm"), w.tr("确认清理", "Confirm cleanup")).Height(24).Padding(0, 9).TextColor(p.danger).Disabled(locked || !wt.CanCleanup)
				if doClean.Clicked() {
					w.cleanupWorktreeOnly(target, wt.Dirty)
				}
				return
			}
			mergeBtn := ui.PrimaryButton(c.Key("git-wt-merge"), w.tr("合并到主分支并清理", "Merge & cleanup worktree")).Height(24).Padding(0, 9).FontSize(11).Disabled(locked || !wt.CanMerge)
			if mergeBtn.Clicked() {
				if w.gitView.worktreeConfirm == nil {
					w.gitView.worktreeConfirm = map[string]string{}
				}
				w.gitView.worktreeConfirm[target] = "merge"
			}
			cleanBtn := quietButton(c, p, "git-wt-cleanup", w.tr("仅清理 Worktree", "Cleanup only")).Height(24).Padding(0, 8).FontSize(11).Disabled(locked || !wt.CanCleanup)
			if cleanBtn.Clicked() {
				if w.gitView.worktreeConfirm == nil {
					w.gitView.worktreeConfirm = map[string]string{}
				}
				w.gitView.worktreeConfirm[target] = "cleanup"
			}
		})
	})
}

func (w *workbench) gitDiffPane(c *ui.Context, p palette, state *api.GitState, groups gitGroups, wide, locked bool) {
	target := w.selected
	selected := w.gitView.selected[target]
	if selected == "" {
		// Show the first change until the user picks one, as IDEA's preview.
		switch {
		case len(groups.staged) > 0:
			selected = "true:" + groups.staged[0].Path
		case len(groups.changes) > 0:
			selected = "false:" + groups.changes[0].Path
		case len(groups.untracked) > 0:
			selected = "false:" + groups.untracked[0].Path
		}
	}
	stagedKey, file, _ := strings.Cut(selected, ":")
	staged := stagedKey == "true"
	text := state.Diff
	heading := "[未暂存 / unstaged]"
	if staged {
		heading = "[已暂存 / staged]"
	}
	if _, rest, ok := strings.Cut(text, heading); ok {
		if cut := strings.Index(rest, "\n[已暂存 / staged]"); cut >= 0 {
			rest = rest[:cut]
		}
		if cut := strings.Index(rest, "\n[未暂存 / unstaged]"); cut >= 0 {
			rest = rest[:cut]
		}
		text = rest
	}
	var shown *patchFile
	for _, f := range w.patchFiles(text) {
		if f.name == file {
			f := f
			shown = &f
			break
		}
	}
	hunkCount := 0
	if shown != nil {
		hunkCount = len(shown.fp.Hunks)
	}
	var layout editor.DiffLayout
	ui.Row(c.Key("diff-header")).Height(30).Gap(6).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Icon(c, fileIcon).FontSize(12).TextColor(p.muted)
		ui.Text(c, nonEmptyString(file, w.tr("差异", "Diff"))).Font("Menlo").FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
		if shown != nil {
			ui.Text(c, fmt.Sprintf("+%d", shown.added)).Font("Menlo").FontSize(10).TextColor(p.success).NoWrap()
			ui.Text(c, fmt.Sprintf("-%d", shown.removed)).Font("Menlo").FontSize(10).TextColor(p.danger).NoWrap()
			if shown.state != nil && hunkCount > 0 {
				ui.Text(c, fmt.Sprintf("%d 块", hunkCount)).FontSize(10).TextColor(p.faint).NoWrap()
				curHunk, _ := shown.state.CurrentHunk()
				if quietButton(c, p, "prev-hunk", "↑ 块").Label(w.tr("上一差异块", "Previous hunk")).Height(22).Padding(0, 6).FontSize(10.5).Clicked() {
					prev := max(0, curHunk-1)
					shown.state.GoToHunk(prev)
				}
				if quietButton(c, p, "next-hunk", "↓ 块").Label(w.tr("下一差异块", "Next hunk")).Height(22).Padding(0, 6).FontSize(10.5).Clicked() {
					next := min(hunkCount-1, curHunk+1)
					shown.state.GoToHunk(next)
				}
			}
		}
		layout = w.gitDiffLayout(c, p, wide)
	})
	ui.Scroll(c.Key("git-patches:"+target)).Grow(1).Basis(0).MinHeight(0).Radius(8).Background(p.code).Border(1, p.lineSoft).Children(func() {
		switch {
		case file == "":
			ui.Text(c, w.tr("选择文件查看差异", "Select a file to see its diff")).FontSize(11).TextColor(p.muted).Padding(12)
		case shown == nil:
			ui.Text(c, w.tr("该文件没有可显示的文本差异；未跟踪文件暂存后可查看。", "No text diff to show; stage an untracked file to see it.")).FontSize(11).TextColor(p.muted).Wrap().Padding(12)
		case shown.state == nil:
			ui.Text(c, w.tr("二进制文件", "Binary file")).FontSize(11).TextColor(p.muted).Padding(12)
		default:
			shown.state.Layout = layout
			shown.state.HunkActions = true
			shown.state.RenderHunkActions = func(c *ui.Context, bar ui.Element, a *editor.DiffHunkActions) {
				bar.Children(func() {
					ui.Row(c).Gap(4).Padding(2, 6).Radius(7).Background(p.surface).Border(1, p.line).AlignItems(ui.Center).Children(func() {
						if staged {
							btn := quietButton(c, p, fmt.Sprintf("unstage-hunk:%d", a.Index), w.tr("取消暂存此块", "Unstage hunk")).Height(22).Padding(0, 8).FontSize(11).Disabled(locked)
							if btn.Clicked() {
								w.changeGitHunk(file, true, a.Index, "unstage")
							}
						} else {
							stageBtn := quietButton(c, p, fmt.Sprintf("stage-hunk:%d", a.Index), w.tr("暂存此块", "Stage hunk")).Height(22).Padding(0, 8).FontSize(11).TextColor(p.accentInk).Disabled(locked)
							if stageBtn.Clicked() {
								w.changeGitHunk(file, false, a.Index, "stage")
							}
							revertBtn := quietButton(c, p, fmt.Sprintf("revert-hunk:%d", a.Index), w.tr("撤回此块", "Revert hunk")).Height(22).Padding(0, 8).FontSize(11).TextColor(p.danger).Disabled(locked)
							if revertBtn.Clicked() {
								w.changeGitHunk(file, false, a.Index, "revert")
							}
						}
					})
				})
			}
			editor.DiffView(c, shown.state).FillWidth()
		}
	})
}

// commitBox is IDEA's non-modal commit panel, always under the changes.
func (w *workbench) commitBox(c *ui.Context, p palette, state *api.GitState, groups gitGroups, locked bool) {
	target := w.selected
	checked := w.checkedPaths(target)
	count := len(groups.staged) + len(checked)
	effectiveCount := count
	if effectiveCount == 0 && len(state.Files) > 0 {
		effectiveCount = len(state.Files)
	}
	_, height := c.Size()
	lines := 2
	if height < 720 {
		lines = 1
	}
	autoMsg := w.autoCommitSummaryForSelection(state, checked, w.widgets.locale)
	placeholder := fmt.Sprintf(w.tr3("提交说明（留空自动生成：%s）", "Commit message (blank auto-fills: %s)", "コミットメッセージ（空欄で自動生成：%s）"), autoMsg)
	boxPad, boxGap := float32(10), float32(8)
	if height < 680 {
		boxPad, boxGap = 6, 5
	}
	ui.Column(c.Key("commit-box")).Gap(boxGap).Padding(boxPad).Radius(12).Background(p.soft).Border(1, p.lineSoft).Shrink(0).Children(func() {
		message := w.gitView.messages[target]
		area := ui.TextArea(c.Key("commit-message:"+target), &message).Label(w.tr("提交说明", "Commit message")).
			Placeholder(placeholder).Lines(lines, 5).FillWidth().Bind(&w.gitView.messageFocus)
		area.OnTextKey(func(k ui.TextKey) bool {
			if k.Key == ui.KeyEnter && k.Mods == ui.Cmd {
				w.gitView.messages[target] = k.Text()
				w.commitFromBox(false)
				return true
			}
			return false
		})
		w.gitView.messages[target] = message
		ready := !locked && !state.Conflicts && (effectiveCount > 0 || w.gitView.amend)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
			amend := w.gitView.amend
			ui.Checkbox(c.Key("amend"), &w.gitView.amend, w.tr("修正上次提交", "Amend")).Description(w.tr("把更改并入上一次提交", "Fold the changes into the previous commit"))
			if w.gitView.amend && !amend && strings.TrimSpace(w.gitView.messages[target]) == "" && len(state.Commits) > 0 {
				w.gitView.messages[target] = state.Commits[0].Subject
			}
			summary := fmt.Sprintf(w.tr("%d 个文件", "%d files"), effectiveCount)
			if state.Conflicts {
				summary = w.tr("请先解决冲突", "Resolve conflicts first")
			}
			ui.Text(c, summary).FontSize(10.5).TextColor(p.faint).NoWrap()
			ui.Spacer(c)
			andPush := ui.Button(c.Key("commit-push"), w.tr("提交并推送", "Commit & push")).Label(w.tr("提交并推送", "Commit and push")).Height(28).Padding(0, 11).NoWrap()
			if !ready {
				andPush.Disabled(true)
			}
			if andPush.Clicked() {
				w.commitFromBox(true)
			}
			commit := ui.PrimaryButton(c.Key("commit"), w.tr("提交", "Commit")).Label(w.tr("创建本地提交", "Commit")).Height(28).Padding(0, 14).NoWrap()
			if !ready {
				commit.Disabled(true)
			}
			if commit.Clicked() {
				w.commitFromBox(false)
			}
		})
	})
}

// ------------------------------------------------------------ log page

// graphRow is one row of the commit graph: the node's lane, the lanes that
// pass through, the lanes that end at the node and those it starts.
type graphRow struct {
	col     int
	lanes   int
	through []int
	in      []int
	out     []int
}

func commitGraph(commits []api.GitCommit) []graphRow {
	rows := make([]graphRow, len(commits))
	var lanes []string
	free := func() int {
		for i, id := range lanes {
			if id == "" {
				return i
			}
		}
		lanes = append(lanes, "")
		return len(lanes) - 1
	}
	for i, commit := range commits {
		row := graphRow{col: -1}
		for j, id := range lanes {
			if id == commit.ID {
				row.in = append(row.in, j)
				if row.col < 0 {
					row.col = j
				}
			} else if id != "" {
				row.through = append(row.through, j)
			}
		}
		if row.col < 0 {
			row.col = free()
		}
		for _, j := range row.in {
			lanes[j] = ""
		}
		for k, parent := range commit.Parents {
			at := slices.Index(lanes, parent)
			if at < 0 {
				if k == 0 {
					at = row.col
				} else {
					at = free()
				}
				lanes[at] = parent
			}
			row.out = append(row.out, at)
		}
		for len(lanes) > 0 && lanes[len(lanes)-1] == "" {
			lanes = lanes[:len(lanes)-1]
		}
		row.lanes = max(len(lanes), row.col+1)
		for _, j := range append(append([]int{}, row.through...), row.in...) {
			row.lanes = max(row.lanes, j+1)
		}
		rows[i] = row
	}
	return rows
}

func graphColor(p palette, lane int) ui.Color {
	colors := chartColors(p)
	return colors[lane%len(colors)]
}

func relativeAge(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy", int(d.Hours()/24/365))
}
