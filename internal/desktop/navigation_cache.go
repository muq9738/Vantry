package desktop

import (
	"slices"
	"strings"
	"time"
)

// Cache only the fields that affect navigation. Text deltas and changing
// relative-time labels do not sort the whole sidebar again. Search is rebuilt
// because it also consumes message text. No file/version coupling is involved.
type navigationRowInput struct {
	id, title, parent, project                                  string
	pinned, archived, active, attention, draft, pending, hidden bool
	activity                                                    time.Time
}
type navigationCache struct {
	previous, scratch []navigationRowInput
	groups            []navigationGroup
	taskLinks         string
	valid             bool
}

func (w *workbench) cachedNavigationGroups() []navigationGroup {
	if !w.frame.valid || strings.TrimSpace(w.sidebarSearch()) != "" || w.sidebarProjectFilter() != "" {
		w.navigation.valid = false
		return w.navigationGroups()
	}
	cache := &w.navigation
	rows := cache.scratch[:0]
	add := func(id string) {
		th, committed := w.threadByID(id)
		comp := w.composers[id]
		row := navigationRowInput{id: id, title: th.Title, parent: th.ParentThreadID, pinned: th.Pinned, archived: th.Archived,
			activity: w.familyActivity(id), active: w.threadWorking(id), attention: w.familyNeedsYou(id)}
		if th.Workspace != nil {
			row.project = nonEmptyString(th.Workspace.Repository, th.Workspace.Path)
		}
		if comp != nil {
			row.draft = strings.TrimSpace(comp.draft) != "" || len(comp.attachments) > 0
			row.pending = comp.pending != nil
		}
		_, hasRun := w.frame.latest[id]
		row.hidden = committed && id != w.selected && th.Workspace == nil && !hasRun && !row.draft && !row.pending
		rows = append(rows, row)
	}
	for _, th := range w.threads {
		add(th.ID)
	}
	for _, id := range w.localOrder {
		add(id)
	}
	// Pending identities can precede the committed thread projection.
	for id, comp := range w.composers {
		if !isLocalSession(id) && comp.pending != nil && !w.hasThread(id) {
			add(id)
		}
	}
	if w.awaitingThread != "" {
		add(w.awaitingThread)
	}
	links := w.navigationTaskLinks()
	if !cache.valid || !slices.Equal(rows, cache.previous) || links != cache.taskLinks {
		cache.groups = w.navigationGroups()
		cache.scratch, cache.previous = cache.previous[:0], rows
		cache.taskLinks = links
		cache.valid = true
	} else {
		cache.scratch = rows
	}
	return cache.groups
}

// navigationTaskLinks fingerprints task parent/child links. Those links hide
// subagent rows even when the child thread has no ParentThreadID of its own,
// and they are not otherwise part of a navigation row.
func (w *workbench) navigationTaskLinks() string {
	if len(w.tasks) == 0 {
		return ""
	}
	var b strings.Builder
	for _, task := range w.tasks {
		if task.ChildThreadID == "" || task.ParentThreadID == "" {
			continue
		}
		b.WriteString(task.ChildThreadID)
		b.WriteByte('>')
		b.WriteString(task.ParentThreadID)
		b.WriteByte(';')
	}
	return b.String()
}
