package desktop

import (
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// Aggregated sidebar routing. The sidebar lists the sessions of every saved
// machine, not only the machine whose conversation fills the stage. A row's
// identity is therefore (machine scope, thread ID): two Cores may carry the
// same thread ID, and every action - select, rename, menu, drag - must reach
// the workbench that owns the row. scope is the record's stable identity; the
// local record's is "local".

// scopeSep cannot appear in a thread ID or an environment key.
const scopeSep = "\x1f"

// identity is the environment record's stable key for filters and row scopes:
// the saved connection for paired machines, its key otherwise ("local").
func (r environmentRecord) identity() string {
	if r.connectionID != "" {
		return r.connectionID
	}
	return r.key
}

func scopedSessionID(scope, id string) string { return scope + scopeSep + id }

func parseScopedSessionID(composite string) (scope, id string, ok bool) {
	at := strings.Index(composite, scopeSep)
	if at <= 0 || at == len(composite)-1 {
		return "", "", false
	}
	return composite[:at], composite[at+len(scopeSep):], true
}

// Shared sidebar state lives on the hub's root view: the aggregated tree is
// one logical sidebar, so the search query and project filter survive the
// stage moving to another machine's view.
func (w *workbench) sidebarSearchState() *string {
	if h := w.environments; h != nil {
		return &h.root.widgets.search
	}
	return &w.widgets.search
}

func (w *workbench) sidebarSearch() string { return *w.sidebarSearchState() }

func (w *workbench) sidebarProjectFilterState() *string {
	if h := w.environments; h != nil {
		return &h.root.widgets.projectFilter
	}
	return &w.widgets.projectFilter
}

func (w *workbench) sidebarProjectFilter() string { return *w.sidebarProjectFilterState() }

// invalidateSidebars drops the navigation caches of every listed view, so a
// filter change on one view refreshes the groups of them all.
func (w *workbench) invalidateSidebars() {
	if h := w.environments; h != nil {
		for _, record := range h.records {
			if record.view != nil {
				record.view.navigation.valid = false
			}
		}
		return
	}
	w.navigation.valid = false
}

// sidebarEntry is one aggregated row: which record it belongs to, the owning
// workbench and the thread ID inside that workbench.
type sidebarEntry struct {
	scope string
	id    string
	owner *workbench
}

// sidebarGroup is one rendered section: the machine-qualified rows it lists,
// already ordered for drawing. The flat list merges the same section of every
// machine into one group, so a row never sits under a machine header.
type sidebarGroup struct {
	section string // sectionDraft, sectionPinned, sectionActive or sectionSettled
	entries []sidebarEntry
}

// recordFor finds the record that owns view; nil when view is the root or
// was built outside this hub.
func (h *environmentHub) recordFor(view *workbench) *environmentRecord {
	for i := range h.records {
		if h.records[i].view == view {
			return &h.records[i]
		}
	}
	return nil
}

// recordByScope resolves a row scope back to its record.
func (h *environmentHub) recordByScope(scope string) *environmentRecord {
	for i := range h.records {
		if h.records[i].identity() == scope {
			return &h.records[i]
		}
	}
	return nil
}

// scopeFor names view in composite row IDs.
func (h *environmentHub) scopeFor(view *workbench) string {
	if record := h.recordFor(view); record != nil {
		return record.identity()
	}
	if view == h.root {
		return "local"
	}
	return view.environmentKey
}

func (h *environmentHub) currentScope() string { return h.scopeFor(h.current) }

func (h *environmentHub) anyConnecting() bool {
	for i := range h.records {
		if h.records[i].connecting {
			return true
		}
	}
	return false
}

// anyRenaming reports an in-place rename open on any listed machine, so the
// hover card stays hidden whichever view renders the sidebar.
func (h *environmentHub) anyRenaming() bool {
	for i := range h.records {
		if view := h.records[i].view; view != nil && view.sessions.renamingID != "" {
			return true
		}
	}
	return false
}

// setFilter narrows the sidebar to one machine; empty restores the default
// aggregation. Filtering only changes what is listed - no thread, draft or
// selection moves between machines.
func (h *environmentHub) setFilter(scope string) {
	if h.filter == scope {
		return
	}
	h.filter = scope
	h.root.invalidateSidebars()
}

// filteredRecords are the machines whose groups the sidebar builds.
func (h *environmentHub) filteredRecords() []*environmentRecord {
	out := make([]*environmentRecord, 0, len(h.records))
	for i := range h.records {
		record := &h.records[i]
		if h.filter == "" || record.identity() == h.filter {
			out = append(out, record)
		}
	}
	return out
}

// sidebarTargetView is where the sidebar's own + actions start work. An
// unavailable filtered machine never falls back to the current machine.
func (w *workbench) sidebarTargetView() *workbench {
	h := w.environments
	if h == nil || h.filter == "" {
		return w
	}
	if record := h.recordByScope(h.filter); record != nil && record.view != nil {
		return record.view
	}
	return nil
}

// newSidebarSession is shared by the button and Cmd+N so both obey the same
// machine filter without turning an unavailable remote into local work.
func (w *workbench) newSidebarSession() {
	target := w.sidebarTargetView()
	if target == nil {
		w.setAction(w.tr("请先连接所选机器，再新建会话。", "Connect the selected machine before starting a thread."))
		return
	}
	w.showThread(target, "")
	target.newSession()
	w.widgets.mobileNav = false
}

// showThread makes owner's stage show the thread: the machine holding the
// thread becomes current, then its own selection moves. Thread ownership
// never changes here.
func (w *workbench) showThread(owner *workbench, id string) {
	if h := w.environments; h != nil && owner != nil {
		if h.current != nil && h.current != owner {
			h.current.cancelRemoteHostingIntent()
			h.current.releaseRemoteKeys()
		}
		h.current = owner
	}
	if owner != nil {
		owner.selectThread(id)
	}
}

// openScopedSession applies a sidebar choice, whether by pointer or the
// tree's arrow keys, to the machine that owns the row.
func (w *workbench) openScopedSession(composite string) bool {
	h := w.environments
	scope, id, ok := parseScopedSessionID(composite)
	if !ok {
		return false
	}
	if h == nil {
		if scope != "local" || !w.hasSession(id) {
			return false
		}
		w.selectThread(id)
		w.widgets.sidebarFocus.Focus()
		return true
	}
	record := h.recordByScope(scope)
	if record == nil || record.view == nil || !record.view.hasSession(id) {
		return false
	}
	w.showThread(record.view, id)
	record.view.widgets.sidebarFocus.Focus()
	return true
}

// currentScopedID is the composite ID the aggregated sidebar shows selected.
func (w *workbench) currentScopedID() string {
	h := w.environments
	if h == nil {
		if w.selected == "" {
			return ""
		}
		return scopedSessionID("local", w.selected)
	}
	if h.current == nil || h.current.selected == "" {
		return ""
	}
	return scopedSessionID(h.currentScope(), h.current.selected)
}

// sidebarGroups assembles the sections of every listed machine. Each row keeps
// the scope and owner that made it, so equal thread IDs on two machines stay
// two distinct sessions, and the sections themselves are merged across
// machines: drafts, pinned, one activity-ordered list, then settled.
func (w *workbench) sidebarGroups() []sidebarGroup {
	search, project := w.sidebarSearch(), w.sidebarProjectFilter()
	type machine struct {
		owner *workbench
		scope string
	}
	var machines []machine
	if h := w.environments; h == nil {
		machines = append(machines, machine{owner: w, scope: "local"})
	} else {
		for _, record := range h.filteredRecords() {
			if record.view != nil {
				machines = append(machines, machine{owner: record.view, scope: record.identity()})
			}
		}
	}
	merged := map[string]*sidebarGroup{}
	for _, m := range machines {
		var groups []navigationGroup
		if m.owner.frame.valid && strings.TrimSpace(search) == "" && project == "" {
			groups = m.owner.cachedNavigationGroups()
		} else {
			groups = m.owner.navigationGroupsFor(search, project)
		}
		for _, g := range groups {
			group := merged[g.key]
			if group == nil {
				group = &sidebarGroup{section: g.key}
				merged[g.key] = group
			}
			for _, id := range g.ids {
				group.entries = append(group.entries, sidebarEntry{scope: m.scope, id: id, owner: m.owner})
			}
		}
	}
	return w.orderSidebarGroups(merged)
}

// orderSidebarGroups draws drafts, pinned, the flat list, then settled. Drafts
// and the flat list are ordered by attention and activity across every
// machine, so the thread that needs you, then the most recent one, leads
// regardless of which machine runs it.
func (w *workbench) orderSidebarGroups(merged map[string]*sidebarGroup) []sidebarGroup {
	order := []string{sectionDraft, sectionPinned, sectionActive, sectionSettled}
	out := make([]sidebarGroup, 0, len(order))
	for _, key := range order {
		group := merged[key]
		if group == nil {
			continue
		}
		if key == sectionActive || key == sectionDraft {
			group.entries = sortedSidebarEntries(group.entries)
		}
		out = append(out, *group)
	}
	return out
}

// sidebarRank is a row's place in its section, read from the row's whole
// family: a family sorts as one block, and a waiting parent lifts its
// subtasks with it.
type sidebarRank struct {
	attention bool
	activity  time.Time
}

func sortedSidebarEntries(entries []sidebarEntry) []sidebarEntry {
	ranks := make([]sidebarRank, len(entries))
	order := make([]int, len(entries))
	for i, entry := range entries {
		ranks[i] = sidebarSortKey(entry.owner, entry.id)
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := ranks[order[i]], ranks[order[j]]
		if a.attention != b.attention {
			return a.attention
		}
		return a.activity.After(b.activity)
	})
	ordered := make([]sidebarEntry, len(entries))
	for i, at := range order {
		ordered[i] = entries[at]
	}
	return ordered
}

// sidebarSelectedScopedID resolves the sidebar row that represents the open
// session: when a subagent is open, its nearest listed ancestor highlights in
// the sidebar, matching T3 Code's resolveListedAncestorThreadRef.
func (w *workbench) sidebarSelectedScopedID(groups []sidebarGroup) string {
	raw := w.currentScopedID()
	scope, id, ok := parseScopedSessionID(raw)
	if !ok || id == "" {
		return raw
	}
	listed := map[string]bool{}
	var owner *workbench
	for _, g := range groups {
		for _, entry := range g.entries {
			if entry.scope == scope {
				listed[entry.id] = true
				if owner == nil {
					owner = entry.owner
				}
			}
		}
	}
	if listed[id] || owner == nil {
		return raw
	}
	return scopedSessionID(scope, owner.resolveListedThreadID(id, listed))
}

// sidebarSortKey is the attention/activity of the row's family (the thread and
// all of its descendant subagents), so live subagent progress lifts the parent.
func sidebarSortKey(owner *workbench, id string) sidebarRank {
	key := sidebarRank{}
	if owner == nil {
		return key
	}
	key.attention = owner.familyNeedsYou(id)
	key.activity = owner.familyActivity(id)
	return key
}

// sidebarMachineLabel names the machine a row runs on, without the connection
// suffix that belongs to the machine menu.
func (w *workbench) sidebarMachineLabel(owner *workbench) string {
	if owner == nil {
		return ""
	}
	if h := w.environments; h != nil {
		if record := h.recordFor(owner); record != nil {
			return record.label
		}
	}
	if w.environments == nil || owner == w.environments.root {
		return machineName()
	}
	return trimConnectionSuffix(owner.hostLabel())
}

// isLocalMachine reports whether a view runs against this window's own Core.
func (w *workbench) isLocalMachine() bool {
	if w.environments == nil {
		return true
	}
	return w == w.environments.root
}

// aggregatedMachines reports whether the list mixes more than one machine, so
// a row can name its machine only when the name disambiguates something.
func (w *workbench) aggregatedMachines() bool {
	h := w.environments
	if h == nil {
		return false
	}
	n := 0
	for _, record := range h.filteredRecords() {
		if record.view != nil {
			n++
		}
	}
	return n > 1
}

func trimConnectionSuffix(label string) string {
	for _, suffix := range []string{" · 直连", " · 中继", " · Direct", " · Relay"} {
		label = strings.TrimSuffix(label, suffix)
	}
	return label
}

// shortMachine keeps a machine's tag inside the room a row's last line has;
// the hover card and the row's description still carry the full name.
func shortMachine(label string) string {
	label = trimConnectionSuffix(strings.TrimSpace(label))
	runes := []rune(label)
	if len(runes) <= 13 {
		return label
	}
	return string(runes[:12]) + "…"
}

// machineFilterMenu is the sidebar's machine selector, as the workbench's machine list:
// the default lists every machine's threads and choosing a machine only
// narrows the list. Opening a row chooses its execution environment.
func (w *workbench) machineFilterMenu(c *ui.Context, p palette) {
	h := w.environments
	if h == nil {
		return
	}
	label := w.tr("全部机器", "All machines")
	if h.filter != "" {
		if record := h.recordByScope(h.filter); record != nil {
			label = record.label
		} else {
			label = h.filter
		}
	}
	button := quietButton(c, p, "machine-filter", "").Label(w.tr("机器筛选：", "Machine filter: ")+label).Height(26).Padding(0, 8).MinWidth(0)
	button.Description(w.tr("侧栏默认列出所有机器的会话；选择一台机器收窄", "The sidebar lists every machine's threads; pick one to narrow"))
	button.Children(func() {
		ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, serverIcon).FontSize(12).TextColor(p.muted)
			ui.Text(c, label).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…")
			ui.Icon(c, chevronIcon).FontSize(9).TextColor(p.faint)
		})
	})
	button.PopoverMenuButton(func(m *ui.Menu) {
		if m.Item(w.tr("全部机器", "All machines")).Radio(h.filter == "").Chosen() {
			h.setFilter("")
		}
		for _, record := range h.records {
			status := ""
			switch {
			case record.connecting:
				status = w.tr(" · 连接中", " · connecting")
			case record.view == nil:
				status = w.tr(" · 未连接", " · offline")
			case record.view.connNotice.valid() && record.view.connNotice.Level != noticeInfo:
				status = w.tr(" · 已离线", " · offline")
			}
			if m.Item(record.label + status).Radio(h.filter == record.identity() && h.filter != "").Chosen() {
				h.setFilter(record.identity())
				if target := h.recordByScope(record.identity()); target != nil && target.view == nil {
					h.connectRecord(target, nil, false)
				}
			}
		}
		m.Separator()
		if m.Item(w.tr("连接另一台机器…", "Connect another machine…")).Chosen() {
			h.manage = true
			h.load()
		}
		if m.Item(w.tr("环境设置…", "Environment settings…")).Chosen() {
			w.openEnvironmentSettings("")
		}
		if m.Item(w.tr("刷新机器连接", "Refresh machine connections")).Chosen() {
			h.load()
		}
	})
}

// machineStatusRows reports machines that cannot list threads: connecting,
// unreachable, or offline with their already loaded sessions still shown.
// Aligned with T3 Code's sidebar, unconnected saved machines stay quiet inside
// the machine selector menu when viewing "All machines" (h.filter == ""), and
// only surface an inline status/retry row when explicitly filtered to that
// machine (or when a previously connected machine drops mid-session).
func (w *workbench) machineStatusRows(c *ui.Context, p palette) {
	h := w.environments
	if h == nil {
		return
	}
	for _, record := range h.filteredRecords() {
		record := record
		if h.filter == "" && (record.view == nil || record.connecting) {
			continue
		}
		text, color, retry := "", p.faint, false
		switch {
		case record.connecting:
			text = record.label + w.tr(" · 正在连接…", " · connecting…")
		case record.view == nil:
			text = record.label + w.tr(" · 未连接", " · not connected")
			if record.failed != "" {
				text = record.label + " · " + record.failed
			}
			color, retry = p.warning, true
		case record.view.connNotice.valid() && record.view.connNotice.Level != noticeInfo:
			text = record.label + w.tr(" · 已离线，显示已加载会话", " · offline, showing loaded threads")
			color = p.warning
		case record.view.streamNotice.valid():
			text = record.label + w.tr(" · 正在重连…", " · reconnecting…")
		}
		if text == "" {
			continue
		}
		ui.Row(c.Key("machine-status:"+record.identity())).Padding(0, 12, 4).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, serverIcon).FontSize(10).TextColor(color).Shrink(0)
			ui.Text(c, text).FontSize(10).TextColor(color).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
			if retry {
				button := quietButton(c, p, "machine-retry:"+record.identity(), w.tr("连接", "Connect")).Height(20).Padding(0, 6)
				button.Label(w.tr("连接机器 ", "Connect machine ") + record.label)
				if button.Clicked() {
					h.connectRecord(record, nil, false)
				}
			}
			if record.connectionID != "" {
				if iconButton(c, p, "machine-settings:"+record.connectionID, w.tr("环境设置 ", "Environment settings ")+record.label, gearIcon).Size(20, 20).Padding(4).Clicked() {
					w.openEnvironmentSettings(record.connectionID)
				}
			}
		})
	}
}
