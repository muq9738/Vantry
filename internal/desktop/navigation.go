package desktop

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type navigationGroup struct {
	key      string
	ids      []string
	activity time.Time
}

func (w *workbench) sessionActivity(id string) time.Time {
	if w.frame.valid {
		return w.frame.activity[id]
	}
	thread, _ := w.threadByID(id)
	latest := thread.CreatedAt
	for _, run := range w.runs {
		if run.ThreadID != id {
			continue
		}
		if run.CreatedAt.After(latest) {
			latest = run.CreatedAt
		}
		if run.UpdatedAt.After(latest) {
			latest = run.UpdatedAt
		}
	}
	for _, message := range w.messages {
		if message.ThreadID == id && message.CreatedAt.After(latest) {
			latest = message.CreatedAt
		}
	}
	for _, message := range w.steering {
		if message.ThreadID != id {
			continue
		}
		if message.CreatedAt.After(latest) {
			latest = message.CreatedAt
		}
		if message.UpdatedAt.After(latest) {
			latest = message.UpdatedAt
		}
	}
	if comp := w.composers[id]; comp != nil {
		if comp.pending != nil && comp.pending.startedAt.After(latest) {
			latest = comp.pending.startedAt
		}
		if comp.steering != nil && comp.steering.startedAt.After(latest) {
			latest = comp.steering.startedAt
		}
	}
	return latest
}

// Sidebar sections, in the order the list draws them: unsent drafts, pinned
// threads, then one flat list of everything else ordered by activity - the
// thread that needs you first, then most recent - and the settled (archived)
// shelf. Each row names its own project, so the list needs no project groups
// and rows never jump between sections while a run is live.
const (
	sectionDraft   = "!draft"
	sectionPinned  = "!pinned"
	sectionActive  = "!active"
	sectionSettled = "!settled"
)

// needsYou reports a thread waiting on the user: an approval or an answer, or
// a send whose result is still unconfirmed.
func (w *workbench) needsYou(id string) bool {
	if comp := w.composers[id]; comp != nil && comp.pending != nil && !comp.pending.accepted && !comp.pending.inFlight {
		return true
	}
	if w.frame.valid {
		return w.frame.waiting[id]
	}
	for _, request := range w.requests {
		if request.ThreadID == id && (request.Status == "pending" || request.Status == "responding") {
			return true
		}
	}
	return false
}

func (w *workbench) threadWorking(id string) bool {
	if w.frame.valid {
		return w.frame.active[id]
	}
	_, active := w.activeRun(id)
	return active
}

func (w *workbench) navigationGroups() []navigationGroup {
	return w.navigationGroupsFor(w.sidebarSearch(), w.sidebarProjectFilter())
}

// navigationGroupsFor groups this workbench's sessions under an explicit
// search and project filter, so another view can aggregate it without the
// remote workbench ever seeing the viewer's filter state.
//
// A subagent whose parent thread still exists on this machine, or whose task
// still points at such a parent, never gets its own sidebar row — even when
// that parent is archived, pinned, project-filtered or missed by search.
// A search hit on the subagent promotes the root parent row instead; the row
// annotates the match. Only a true orphan (no local parent thread and no
// task linking one) is listed on its own.
func (w *workbench) navigationGroupsFor(search, projectFilter string) []navigationGroup {
	search = strings.TrimSpace(search)
	promoted := map[string]bool{}
	if search != "" {
		for _, id := range w.navigationSessions() {
			if !w.hiddenSubagentRow(id) || !w.sessionMatches(id, search) {
				continue
			}
			root := w.sidebarAncestor(id)
			if root == "" || root == id {
				continue
			}
			if projectFilter != "" {
				parent, ok := w.threadByID(root)
				if !ok || workspaceKey(parent.Workspace) != projectFilter {
					continue
				}
			}
			promoted[root] = true
		}
	}
	type candidate struct {
		id      string
		section string
	}
	var candidates []candidate
	for _, id := range w.navigationSessions() {
		thread, committed := w.threadByID(id)
		comp := w.composers[id]
		forced := promoted[id]
		if !forced && committed && id != w.selected && thread.Workspace == nil && len(w.runsFor(id)) == 0 && (comp == nil || comp.pending == nil && strings.TrimSpace(comp.draft) == "" && len(comp.attachments) == 0) {
			continue
		}
		if !forced && !w.sessionMatches(id, search) {
			continue
		}
		if filter := projectFilter; filter != "" && committed && workspaceKey(thread.Workspace) != filter {
			continue
		}
		if w.hiddenSubagentRow(id) {
			continue
		}
		key := sectionActive
		switch {
		case !committed:
			key = sectionDraft
		case thread.Archived:
			key = sectionSettled
		case thread.Pinned:
			key = sectionPinned
		}
		candidates = append(candidates, candidate{id: id, section: key})
	}

	groups := map[string]*navigationGroup{}
	for _, cand := range candidates {
		if groups[cand.section] == nil {
			groups[cand.section] = &navigationGroup{key: cand.section}
		}
		group := groups[cand.section]
		group.ids = append(group.ids, cand.id)
		if activity := w.familyActivity(cand.id); activity.After(group.activity) {
			group.activity = activity
		}
	}
	result := make([]navigationGroup, 0, len(groups))
	for _, group := range groups {
		group.ids = w.familyOrder(group.ids)
		result = append(result, *group)
	}
	rank := func(key string) int {
		switch key {
		case sectionDraft:
			return 0
		case sectionPinned:
			return 1
		case sectionSettled:
			return 3
		default:
			return 2
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if rank(a.key) != rank(b.key) {
			return rank(a.key) < rank(b.key)
		}
		if !a.activity.Equal(b.activity) {
			return a.activity.After(b.activity)
		}
		return a.key < b.key
	})
	return result
}

// familyActivity returns the latest activity of id and all of its descendant
// subagent threads, so a parent thread reflects live subagent progress.
func (w *workbench) familyActivity(id string) time.Time {
	latest := w.sessionActivity(id)
	for _, childID := range w.descendantThreadIDs(id) {
		if at := w.sessionActivity(childID); at.After(latest) {
			latest = at
		}
	}
	for _, task := range w.tasks {
		if task.ParentThreadID == id && task.UpdatedAt.After(latest) {
			latest = task.UpdatedAt
		}
	}
	return latest
}

// familyNeedsYou reports whether id or any of its descendant subagents is
// waiting on user approval or input.
func (w *workbench) familyNeedsYou(id string) bool {
	if w.needsYou(id) {
		return true
	}
	for _, childID := range w.descendantThreadIDs(id) {
		if w.needsYou(childID) {
			return true
		}
	}
	return false
}

// existingParentID is the nearest parent thread that still exists locally:
// the thread's own ParentThreadID, or a task that links this child to a parent.
func (w *workbench) existingParentID(id string) (string, bool) {
	if th, ok := w.threadByID(id); ok && th.ParentThreadID != "" && th.ParentThreadID != id {
		if _, ok := w.threadByID(th.ParentThreadID); ok {
			return th.ParentThreadID, true
		}
	}
	for i := range w.tasks {
		task := &w.tasks[i]
		if task.ChildThreadID != id || task.ParentThreadID == "" || task.ParentThreadID == id {
			continue
		}
		if _, ok := w.threadByID(task.ParentThreadID); ok {
			return task.ParentThreadID, true
		}
	}
	return "", false
}

// hiddenSubagentRow reports a subagent that must not occupy a sidebar row.
func (w *workbench) hiddenSubagentRow(id string) bool {
	_, ok := w.existingParentID(id)
	return ok
}

// sidebarAncestor walks to the furthest existing parent, or id when the
// thread is a true orphan.
func (w *workbench) sidebarAncestor(id string) string {
	seen := map[string]bool{}
	cur := id
	for cur != "" && !seen[cur] {
		seen[cur] = true
		parent, ok := w.existingParentID(cur)
		if !ok {
			return cur
		}
		cur = parent
	}
	if cur == "" {
		return id
	}
	return cur
}

// subagentSearchNote annotates a parent row when the sidebar search hit one
// of its subagents rather than (or as well as) the parent itself.
func (w *workbench) subagentSearchNote(id, query string) string {
	query = strings.TrimSpace(query)
	if id == "" || query == "" {
		return ""
	}
	var n int
	var one string
	for _, child := range w.descendantThreadIDs(id) {
		if child == id || !w.sessionMatches(child, query) {
			continue
		}
		n++
		if one == "" {
			one = w.displayTitleFor(child)
		}
	}
	switch n {
	case 0:
		return ""
	case 1:
		return w.tr("匹配子代理：", "Subagent match: ") + one
	default:
		return fmt.Sprintf(w.tr("匹配 %d 个子代理", "%d subagent matches"), n)
	}
}

// descendantThreadIDs lists every child/descendant subagent thread of rootID,
// including children that are linked only by a task.
func (w *workbench) descendantThreadIDs(rootID string) []string {
	if rootID == "" || (len(w.threads) == 0 && len(w.tasks) == 0) {
		return nil
	}
	children := map[string][]string{}
	link := func(parent, child string) {
		if parent == "" || child == "" || parent == child {
			return
		}
		for _, id := range children[parent] {
			if id == child {
				return
			}
		}
		children[parent] = append(children[parent], child)
	}
	for _, th := range w.threads {
		link(th.ParentThreadID, th.ID)
	}
	for _, task := range w.tasks {
		link(task.ParentThreadID, task.ChildThreadID)
	}
	if len(children[rootID]) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{rootID: true}
	queue := append([]string(nil), children[rootID]...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		out = append(out, cur)
		queue = append(queue, children[cur]...)
	}
	return out
}

// resolveListedThreadID walks ParentThreadID to the nearest ancestor present in
// listed, falling back to id itself when no ancestor is listed.
func (w *workbench) resolveListedThreadID(id string, listed map[string]bool) string {
	if id == "" || listed[id] {
		return id
	}
	seen := map[string]bool{id: true}
	cur := id
	for {
		th, ok := w.threadByID(cur)
		if !ok || th.ParentThreadID == "" || seen[th.ParentThreadID] {
			return id
		}
		parent := th.ParentThreadID
		seen[parent] = true
		if listed[parent] {
			return parent
		}
		cur = parent
	}
}

// Order listed threads by attention first, then by family activity (including
// descendant subagents).
func (w *workbench) familyOrder(ids []string) []string {
	ordered := append([]string(nil), ids...)
	attention := make(map[string]bool, len(ordered))
	activity := make(map[string]time.Time, len(ordered))
	for _, id := range ordered {
		attention[id] = w.familyNeedsYou(id)
		activity[id] = w.familyActivity(id)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if attention[ordered[i]] != attention[ordered[j]] {
			return attention[ordered[i]]
		}
		return activity[ordered[i]].After(activity[ordered[j]])
	})
	return ordered
}

func (w *workbench) projectExpanded(key string) *bool {
	// The aggregated sidebar is one tree: expansion lives on the hub's root
	// view so a folded project stays folded across machine switches.
	tools := &w.features
	if h := w.environments; h != nil {
		tools = &h.root.features
	}
	if tools.projectOpen == nil {
		tools.projectOpen = map[string]*bool{}
	}
	if tools.projectOpen[key] == nil {
		open := key != sectionSettled // the settled shelf starts folded, as the workbench's
		tools.projectOpen[key] = &open
	}
	return tools.projectOpen[key]
}
