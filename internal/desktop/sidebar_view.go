package desktop

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// sidebarView follows the workbench's Sidebar + SidebarChrome, top to bottom: the brand
// header with ← / → history (drag region), the search and action bar, the
// thread tree (drafts → pinned → projects → working), the folded Settled
// shelf, and the icon-only utility footer. Hovering a row on a wide window
// shows the workbench's ThreadHoverCard beside the sidebar.
func (w *workbench) sidebarView(c *ui.Context, p palette, width float32) {
	column := ui.Column(c).Background(p.sidebar).BorderWidth(0, 1, 0, 0).BorderColor(p.lineSoft).Cache()
	if width > 0 {
		column.Width(width).Shrink(0)
	} else {
		column.Grow(1)
	}
	column.Children(func() {
		w.sidebarChrome(c, p)
		w.sidebarSearchBar(c, p)
		w.environmentSidebar(c, p)
		w.widgets.navActivate = false
		wasDragging := w.widgets.draggingThread
		w.widgets.draggingThread = false
		hoveredBefore := w.widgets.hoverThread
		w.widgets.hoverThread, w.widgets.hoverRect = "", ui.Rect{}
		w.widgets.sidebarMultiMachine = w.aggregatedMachines()
		groups := w.sidebarGroups()
		selected := w.sidebarSelectedScopedID(groups)
		before := selected
		var settled []sidebarGroup
		active := groups[:0]
		for _, group := range groups {
			if group.section == sectionSettled {
				settled = append(settled, group)
				continue
			}
			active = append(active, group)
		}
		matches := 0
		sidebarTheme := *c.Theme()
		sidebarTheme.Background = p.sidebar
		sidebarTheme.Surface = p.sidebar
		sidebarTheme.Accent = p.selected
		sidebarTheme.AccentText = p.ink
		sidebarTheme.InactiveAccent = p.selected
		sidebarTheme.SurfaceHover = p.rowHover
		sidebarTheme.SurfacePressed = p.selected
		ui.Themed(c, &sidebarTheme).Grow(1).MinHeight(0).Children(func() {
			sidebar := ui.Sidebar(c, &selected, func() {
				matches = w.threadList(c, p, active)
			}).Grow(1).MinHeight(0).Shrink(1).Padding(0, 6).Background(p.sidebar).Bind(&w.widgets.sidebarFocus)
			sidebar.Description(w.tr("按最近活动选择会话；草稿与阅读位置按会话保留", "Threads by recent activity; drafts and scroll positions are kept per thread"))
		})
		for _, group := range settled {
			matches += len(group.entries)
		}
		if matches == 0 {
			text := w.tr("还没有对话", "No threads yet")
			if strings.TrimSpace(w.sidebarSearch()) != "" || w.sidebarProjectFilter() != "" {
				text = w.tr("没有匹配的会话", "No matching threads")
			}
			ui.Text(c, text).FontSize(11).TextColor(p.muted).Padding(8, 18)
		}
		ui.Themed(c, &sidebarTheme).Shrink(0).Children(func() {
			w.settledShelf(c, p, settled, &selected)
		})
		// Only a choice the sidebar itself made applies. A context-menu action
		// (fork, new session in project) may already have moved the selection
		// during this frame; the stale binding must not undo it. The composite
		// ID carries its machine, so choosing a remote row opens that view.
		if selected != "" && selected != before && selected != w.currentScopedID() {
			if scope, id, ok := parseScopedSessionID(selected); ok {
				owner := w
				if h := w.environments; h != nil {
					if rec := h.recordByScope(scope); rec != nil && rec.view != nil {
						owner = rec.view
					}
				}
				if owner.isUnread(id) {
					w.widgets.attentionHold = selected
				} else {
					w.widgets.attentionHold = ""
				}
			}
			w.openScopedSession(selected)
		}
		if w.widgets.navActivate && w.widgets.mobileNav {
			w.widgets.mobileNav = false
			if current := w.environments; current != nil {
				current.current.widgets.composerFocus.Focus()
			} else {
				w.widgets.composerFocus.Focus()
			}
		}
		w.sidebarFooter(c, p, width)
		if w.widgets.hoverThread != hoveredBefore {
			w.widgets.hoverSince = c.Now()
		}
		if w.widgets.draggingThread {
			w.widgets.hoverSince = c.Now()
		}
		w.sidebarDropShelf(c, p, column, wasDragging)
		renaming := false
		if h := w.environments; h != nil {
			renaming = h.anyRenaming()
		} else {
			renaming = w.sessions.renamingID != ""
		}
		if width > 0 && !w.widgets.sidebarClosed && !renaming {
			w.threadHoverCard(c, p, width)
		}
	})
}

// sidebarChrome is the brand row: the sidebar toggle, the name, and the workbench's
// back / forward buttons through visited threads, after the traffic lights.
func (w *workbench) sidebarChrome(c *ui.Context, p palette) {
	bar := c.TitleBar()
	ui.Row(c).Height(max(float32(46), bar.Height)).Padding(0, 6, 0, max(float32(10), bar.Left+2)).Gap(1).AlignItems(ui.Center).DragWindow().Children(func() {
		if iconButton(c, p, "sidebar-close", w.tr("收起会话导航", "Collapse sidebar"), panelIcon).Size(24, 24).Padding(4).Clicked() {
			w.widgets.sidebarClosed = true
			w.widgets.mobileNav = false
		}
		ui.Text(c, "Vantry").FontSize(13.5).Bold().TextColor(p.ink).SingleLine().Padding(0, 3, 0, 2).Shrink(0).Role(ui.RoleHeading).Level(1)
		back := iconButton(c, p, "history-back", w.tr("后退", "Back"), backIcon).Size(22, 22).Padding(4)
		back.Description(w.tr("回到上一个查看的会话", "Go to the previously viewed thread"))
		if !w.canGoBack() {
			back.Disabled(true).TextColor(p.faint.Alpha(0.6))
		}
		if back.Clicked() {
			w.goHistory(-1)
		}
		forward := iconButton(c, p, "history-forward", w.tr("前进", "Forward"), forwardIcon).Size(22, 22).Padding(4)
		forward.Description(w.tr("前往下一个查看过的会话", "Go to the next viewed thread"))
		if !w.canGoForward() {
			forward.Disabled(true).TextColor(p.faint.Alpha(0.6))
		}
		if forward.Clicked() {
			w.goHistory(1)
		}
		ui.Spacer(c)
		w.inboxChromeBadge(c, p)
	})
}

// sidebarSearchBar is the workbench's SidebarThreadHeader: the rounded search
// field with ⌘K hint, then the project filter, add project and compose buttons.
func (w *workbench) sidebarSearchBar(c *ui.Context, p palette) {
	ui.Row(c.Key("sidebar-search-bar")).Padding(2, 8, 8).Gap(2).AlignItems(ui.Center).Children(func() {
		query := w.sidebarSearch()
		searchBg := p.card
		if p.dark {
			searchBg = p.raised
		}
		searchBox := ui.Row(c.Key("thread-search-shell")).Grow(1).MinWidth(0).Height(30).Padding(0, 7, 0, 2).Gap(3).
			Radius(9).Background(searchBg).Border(1, p.lineSoft).AlignItems(ui.Center)
		if w.widgets.searchFocus.Focused(c) {
			searchBox.BorderColor(p.accent.Alpha(0.55))
		} else if searchBox.Hovered() {
			searchBox.BorderColor(p.line)
		}
		searchBox.Children(func() {
			ui.SearchField(c.Key("thread-search"), w.sidebarSearchState()).Label(w.tr("搜索会话", "Search threads")).
				Placeholder(w.tr3("搜索项目、会话", "Search threads", "スレッドを検索")).
				Grow(1).MinWidth(0).Height(26).Padding(0, 4).Radius(7).
				Border(0, ui.Transparent).Background(ui.Transparent).Bind(&w.widgets.searchFocus)
			if strings.TrimSpace(query) == "" && !w.widgets.searchFocus.Focused(c) {
				ui.Text(c, "⌘K").Font("Menlo").FontSize(10).TextColor(p.faint).Shrink(0).NoWrap().AccessHidden(true)
			}
		})
		filter := iconButton(c, p, "project-filter", w.tr("按项目筛选", "Filter by project"), filterIcon).Size(24, 24).Padding(4)
		if w.sidebarProjectFilter() != "" {
			filter.TextColor(p.accentInk).Background(p.accent.Alpha(0.12))
			filter.Description(w.tr("正在筛选：", "Filtering: ") + filepath.Base(w.sidebarProjectFilter()))
		}
		filter.PopoverMenuButton(func(m *ui.Menu) {
			if m.Item(w.tr("全部项目", "All projects")).Radio(w.sidebarProjectFilter() == "").Chosen() {
				w.setProjectFilter("")
			}
			projects := w.projectKeys()
			if len(projects) > 0 {
				m.Separator()
			}
			for _, key := range projects {
				if m.Item(filepath.Base(key)).Radio(w.sidebarProjectFilter() == key).Chosen() {
					w.setProjectFilter(key)
				}
			}
		}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomEnd, Self: ui.AnchorTopEnd})
		target := w.sidebarTargetView()
		add := iconButton(c, p, "add-project", w.tr("添加项目", "Add project"), folderPlusIcon).Size(24, 24).Padding(4).Disabled(target == nil)
		if add.Clicked() && target != nil {
			w.showThread(target, "")
			if target.pickDirectory != nil {
				target.pickDirectory()
			} else {
				target.beginProjectChoice()
				target.widgets.projectPickerOpen = true
				target.widgets.modelPickerOpen = false
			}
		}
		newThread := iconButton(c, p, "new-session", w.tr("新会话", "New thread"), composeIcon).Size(24, 24).Padding(4).Disabled(target == nil)
		newThread.Tooltip(w.tr("新会话 ⌘ N", "New thread ⌘ N")).Description("⌘ N")
		if target == nil {
			add.Tooltip(w.tr("请先连接所选机器", "Connect the selected machine first"))
			newThread.Tooltip(w.tr("请先连接所选机器", "Connect the selected machine first"))
		}
		if newThread.Clicked() {
			w.newSidebarSession()
		}
	})
}

// projectKeys lists the distinct projects of the committed threads of every
// listed machine, so the filter reaches remote projects too.
func (w *workbench) projectKeys() []string {
	seen := map[string]bool{}
	var keys []string
	views := []*workbench{w}
	if h := w.environments; h != nil {
		views = views[:0]
		for _, record := range h.records {
			if record.view != nil {
				views = append(views, record.view)
			}
		}
	}
	for _, view := range views {
		for _, thread := range view.threads {
			if key := workspaceKey(thread.Workspace); key != "" && !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.ToLower(filepath.Base(keys[i])) < strings.ToLower(filepath.Base(keys[j]))
	})
	return keys
}

func (w *workbench) setProjectFilter(key string) {
	if w.sidebarProjectFilter() == key {
		return
	}
	*w.sidebarProjectFilterState() = key
	w.navigation.valid = false
}

const projectPreviewLimit = 5

type sidebarProjectBucket struct {
	key       string
	name      string
	workspace *api.WorkspaceSpec
	owner     *workbench
	entries   []sidebarEntry
	activity  time.Time
}

// threadList renders the Stardust sectioned sidebar:
//  1. Unsent drafts (top, no header)
//  2. 运行中 · N (actively running threads without a blocking prompt)
//  3. 待我处理 · N (orange count badge: approvals, unconfirmed, unread results, failed)
//  4. 已置顶 (pinned threads not in Running / Needs-you)
//  5. 分类 (collapsible project groups with colored badges, counts, and "更多 N")
//
// Each entry appears at most once so native SidebarItem keys, keyboard nav,
// typeahead and drag-and-drop remain collision-free.
func (w *workbench) threadList(c *ui.Context, p palette, groups []sidebarGroup) int {
	matches := 0
	now := c.Now()
	var drafts, running, attention, pinned, rest []sidebarEntry
	liveSubtasks := 0
	curScoped := w.currentScopedID()
	if w.widgets.attentionHold != "" && w.widgets.attentionHold != curScoped {
		w.widgets.attentionHold = ""
	}
	for _, group := range groups {
		matches += len(group.entries)
		for _, entry := range group.entries {
			owner := entry.owner
			if owner == nil {
				owner = w
			}
			if group.section == sectionDraft {
				drafts = append(drafts, entry)
				continue
			}
			composite := scopedSessionID(entry.scope, entry.id)
			liveAgents, _ := owner.threadSubagentCounts(entry.id)
			needsYou := owner.familyNeedsYou(entry.id)
			working := owner.threadWorking(entry.id)
			unread := owner.isUnread(entry.id) && !(w.environments != nil && w.environments.current == owner && owner.selected == entry.id)
			failed := false
			if latest, ok := owner.latestRun(entry.id); ok && (latest.Status == api.RunFailed || latest.Status == api.RunInterrupted) {
				failed = true
			}
			switch {
			case needsYou:
				attention = append(attention, entry)
			case working || liveAgents > 0:
				running = append(running, entry)
				liveSubtasks += liveAgents
			case unread || failed || composite == w.widgets.attentionHold:
				attention = append(attention, entry)
			case group.section == sectionPinned:
				pinned = append(pinned, entry)
			default:
				rest = append(rest, entry)
			}
		}
	}

	// 1. Drafts
	for _, entry := range drafts {
		w.sidebarRow(c, p, entry, now, true)
	}

	// 2. 运行中 · N
	if len(running) > 0 {
		sub := ""
		if liveSubtasks > 0 {
			sub = fmt.Sprintf(w.tr3("含 %d 个子任务", "%d subtasks", "サブタスク %d 件"), liveSubtasks)
		}
		w.sidebarSectionHeader(c, p, "sec-running", w.tr3("运行中", "Running", "実行中"), sub, len(running), false)
		for _, entry := range running {
			w.sidebarRow(c, p, entry, now, true)
		}
	}

	// 3. 待我处理 · N (orange count badge)
	if len(attention) > 0 {
		w.sidebarSectionHeader(c, p, "sec-attention", w.tr3("待我处理", "Needs you", "要対応"), "", len(attention), true)
		for _, entry := range attention {
			w.sidebarRow(c, p, entry, now, true)
		}
	}

	// 4. 已置顶
	if len(pinned) > 0 {
		w.sidebarSectionHeader(c, p, "sec-pinned", w.tr3("已置顶", "Pinned", "ピン留め"), "", len(pinned), false)
		for _, entry := range pinned {
			w.sidebarRow(c, p, entry, now, true)
		}
	}

	// 5. 分类 — project groups
	if len(rest) > 0 {
		if len(running) > 0 || len(attention) > 0 || len(pinned) > 0 {
			ui.Row(c.Key("sec-categories")).Height(24).Padding(6, 8, 2).AlignItems(ui.Center).Children(func() {
				ui.Text(c, w.tr3("分类", "Projects", "プロジェクト")).FontSize(11).FontWeight(600).TextColor(p.faint).SingleLine()
			})
		}
		buckets := w.groupSidebarByProject(rest)
		searching := strings.TrimSpace(w.sidebarSearch()) != ""
		for _, bucket := range buckets {
			w.sidebarProjectGroup(c, p, bucket, now, searching)
		}
	}

	c.After(time.Minute) // ages in the rows
	return matches
}

func (w *workbench) sidebarSectionHeader(c *ui.Context, p palette, key, title, subtitle string, count int, accentBadge bool) {
	ui.Row(c.Key(key)).Height(26).Padding(6, 8, 2).Gap(5).AlignItems(ui.Center).Children(func() {
		ui.Text(c, title).FontSize(11.5).FontWeight(600).TextColor(p.muted).SingleLine().Shrink(0)
		if subtitle != "" {
			ui.Text(c, "· "+subtitle).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
		}
		ui.Spacer(c)
		if accentBadge {
			ui.Row(c).Height(18).MinWidth(18).Padding(0, 5.5).Radius(9).Justify(ui.Center).AlignItems(ui.Center).
				Background(p.accent).Shrink(0).Children(func() {
				ui.Text(c, fmt.Sprint(count)).FontSize(10.5).FontWeight(700).FontFeatures("tnum").TextColor(ui.Hex("#FFFFFF")).NoWrap()
			})
		} else {
			ui.Text(c, fmt.Sprint(count)).FontSize(11).FontWeight(500).FontFeatures("tnum").TextColor(p.faint).Shrink(0).NoWrap()
		}
	})
}

func (w *workbench) groupSidebarByProject(entries []sidebarEntry) []*sidebarProjectBucket {
	byKey := map[string]*sidebarProjectBucket{}
	var buckets []*sidebarProjectBucket
	for _, entry := range entries {
		owner := entry.owner
		if owner == nil {
			owner = w
		}
		thread, _ := owner.threadByID(entry.id)
		key := workspaceKey(thread.Workspace)
		name := workspaceName(thread.Workspace)
		if key == "" {
			key = "__unassigned__"
			name = w.tr3("无项目", "No project", "プロジェクトなし")
		}
		b := byKey[key]
		if b == nil {
			b = &sidebarProjectBucket{
				key:       key,
				name:      name,
				workspace: thread.Workspace,
				owner:     owner,
			}
			byKey[key] = b
			buckets = append(buckets, b)
		}
		b.entries = append(b.entries, entry)
		if at := owner.familyActivity(entry.id); at.After(b.activity) {
			b.activity = at
		}
	}
	sort.SliceStable(buckets, func(i, j int) bool {
		if buckets[i].key == "__unassigned__" && buckets[j].key != "__unassigned__" {
			return false
		}
		if buckets[j].key == "__unassigned__" && buckets[i].key != "__unassigned__" {
			return true
		}
		if !buckets[i].activity.Equal(buckets[j].activity) {
			return buckets[i].activity.After(buckets[j].activity)
		}
		return strings.ToLower(buckets[i].name) < strings.ToLower(buckets[j].name)
	})
	return buckets
}

func (w *workbench) sidebarProjectGroup(c *ui.Context, p palette, bucket *sidebarProjectBucket, now time.Time, searching bool) {
	openPtr := w.projectExpanded("cat:" + bucket.key)
	open := *openPtr || searching
	headerRow := ui.Row(c.Key("proj-hdr:"+bucket.key)).FillWidth().Height(28).Padding(0, 6).Gap(4).Radius(7).AlignItems(ui.Center)
	hovered := headerRow.Hovered()
	if hovered {
		headerRow.Background(p.rowHover)
	}
	headerRow.Children(func() {
		toggle := ui.Button(c.Key("proj-toggle"), "").Label(bucket.name).Expanded(open).Grow(1).MinWidth(0).Height(26).Padding(0, 2).
			Border(0, p.line).Background(ui.Transparent).Justify(ui.Start)
		toggle.Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Grow(1).MinWidth(0).Children(func() {
				chev := rightIcon
				if open {
					chev = chevronIcon
				}
				ui.Icon(c, chev).FontSize(11).TextColor(p.faint).Shrink(0)
				if bucket.key == "__unassigned__" {
					ui.Icon(c, folderIcon).FontSize(13).TextColor(p.faint).Shrink(0)
				} else {
					projectBadge(c, bucket.name, 16)
				}
				ui.Text(c, bucket.name).FontSize(12).FontWeight(600).TextColor(p.ink.Mix(p.sidebar, 0.14)).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
			})
		})
		if toggle.Clicked() {
			*openPtr = !*openPtr
			w.saveState()
		}
		if hovered && bucket.workspace != nil && bucket.owner != nil {
			addBtn := rowIconButton(c, p, "proj-new:"+bucket.key, w.tr3("在此项目新建会话", "New thread in project", "このプロジェクトで新規スレッド"), lucide("plus"))
			if addBtn.Clicked() {
				w.showThread(bucket.owner, "")
				bucket.owner.newSessionIn(bucket.workspace)
			}
		} else {
			ui.Text(c, fmt.Sprint(len(bucket.entries))).FontSize(11).FontFeatures("tnum").TextColor(p.faint).Padding(0, 4).Shrink(0).NoWrap()
		}
	})
	if !open {
		return
	}
	showAll := searching || (w.widgets.projectMoreOpen != nil && w.widgets.projectMoreOpen[bucket.key])
	visible := bucket.entries
	hidden := 0
	if !showAll && len(visible) > projectPreviewLimit {
		hidden = len(visible) - projectPreviewLimit
		visible = visible[:projectPreviewLimit]
	}
	for _, entry := range visible {
		w.sidebarRow(c, p, entry, now, false)
	}
	if hidden > 0 {
		moreLabel := fmt.Sprintf(w.tr3("更多 %d", "More %d", "他 %d 件"), hidden)
		more := ui.Button(c.Key("proj-more:"+bucket.key), "").Label(moreLabel).Height(24).Padding(0, 10, 0, 28).Radius(6).
			Border(0, p.line).Background(ui.Transparent).TextColor(p.faint).Justify(ui.Start).FillWidth()
		if more.Hovered() {
			more.Background(p.rowHover).TextColor(p.muted)
		}
		more.Children(func() {
			ui.Text(c, moreLabel).FontSize(11).NoWrap()
		})
		if more.Clicked() {
			if w.widgets.projectMoreOpen == nil {
				w.widgets.projectMoreOpen = map[string]bool{}
			}
			w.widgets.projectMoreOpen[bucket.key] = true
		}
	}
}

func (w *workbench) newSessionIn(workspace *api.WorkspaceSpec) {
	spec := cloneWorkspace(workspace)
	if spec != nil && (spec.Kind == "worktree" || spec.Repository != "") {
		spec.Path, spec.Branch = "", ""
	}
	w.defaults.Workspace = spec
	w.newSession()
	w.widgets.mobileNav = false
	w.widgets.composerFocus.Focus()
}

// settledShelf is the workbench's folded drawer of settled threads, anchored above the
// footer. Settling reuses the thread's Archived flag, so Core is unchanged.
func (w *workbench) settledShelf(c *ui.Context, p palette, groups []sidebarGroup, selected *string) {
	count := 0
	for _, group := range groups {
		count += len(group.entries)
	}
	open := w.projectExpanded(sectionSettled)
	shown := *open || strings.TrimSpace(w.sidebarSearch()) != "" && count > 0
	_, height := c.Size()
	ui.Column(c.Key("settled-shelf")).Padding(4, 6).Gap(2).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
		title := w.tr("已归档", "Settled")
		head := ui.Button(c.Key("settled-toggle"), "").Label(title).Expanded(shown).FillWidth().Height(30).Padding(0, 8).
			Radius(6).Border(0, p.line).Background(ui.Transparent).TextColor(p.muted)
		head.Description(w.tr("已归档的会话；拖动会话到这里即可归档", "Settled threads; drop a thread here to settle it"))
		if head.Hovered() {
			head.Background(p.lineSoft).TextColor(p.ink)
		}
		head.Children(func() {
			ui.Icon(c, settleIcon).FontSize(13).TextColor(p.faint)
			ui.Text(c, title).FontSize(12).Bold().SingleLine()
			ui.Spacer(c)
			ui.Row(c).Height(17).Padding(0, 6).Radius(9).AlignItems(ui.Center).Background(p.lineSoft).Shrink(0).Children(func() {
				ui.Text(c, fmt.Sprint(count)).FontSize(11).TextColor(p.muted).NoWrap()
			})
			icon := rightIcon
			if shown {
				icon = chevronIcon
			}
			ui.Icon(c, icon).FontSize(12).TextColor(p.faint)
		})
		w.threadDropTarget(c, p, head, sectionSettled)
		if head.Clicked() {
			*open = !*open
			shown = *open
			w.saveState()
		}
		if !shown {
			return
		}
		if count == 0 {
			ui.Text(c, w.tr("还没有已归档的会话", "Nothing settled yet")).FontSize(11).TextColor(p.faint).Padding(4, 10)
			return
		}
		now := c.Now()
		ui.Sidebar(c.Key("settled-tree"), selected, func() {
			for _, group := range groups {
				for _, entry := range group.entries {
					w.sidebarRow(c, p, entry, now, true)
				}
			}
		}).Height(min(float32(count)*(rowHeight+2)+8, max(float32(90), height*0.4))).Background(p.sidebar).Label(title)
	})
}
