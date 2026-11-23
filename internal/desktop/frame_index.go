package desktop

import (
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// frameIndex is built once at the start of each frame from the projections,
// in one pass over each, so rows and the sidebar look things up instead of
// scanning every run, tool and message per thread. Maps keep their buckets
// and slices their arrays between frames: a steady frame allocates nothing.
type frameIndex struct {
	conversationThread string
	conversation       []conversationEntry
	conversationBuilt  bool
	valid              bool
	tools              map[string][]api.ToolCall // run → reusable projection slices
	activity           map[string]time.Time
	active             map[string]bool // threads with a queued, running or waiting run
	waiting            map[string]bool // threads whose run waits on the user
	latest             map[string]int  // thread → index of its latest run
	threads            map[string]int  // thread → index into w.threads
}

func (w *workbench) indexFrame() {
	f := &w.frame
	f.conversationBuilt = false
	if f.tools == nil {
		f.tools, f.activity = map[string][]api.ToolCall{}, map[string]time.Time{}
		f.active, f.waiting, f.latest, f.threads = map[string]bool{}, map[string]bool{}, map[string]int{}, map[string]int{}
	}
	for k, v := range f.tools {
		f.tools[k] = v[:0]
	}
	clear(f.activity)
	clear(f.active)
	clear(f.waiting)
	clear(f.latest)
	clear(f.threads)
	note := func(id string, at time.Time) {
		if at.After(f.activity[id]) {
			f.activity[id] = at
		}
	}
	for i := range w.threads {
		f.threads[w.threads[i].ID] = i
		note(w.threads[i].ID, w.threads[i].CreatedAt)
	}
	for i := range w.runs {
		r := &w.runs[i]
		note(r.ThreadID, r.CreatedAt)
		note(r.ThreadID, r.UpdatedAt)
		if !r.Status.Terminal() {
			f.active[r.ThreadID] = true
		}
		if j, ok := f.latest[r.ThreadID]; !ok || !w.runs[j].CreatedAt.After(r.CreatedAt) {
			f.latest[r.ThreadID] = i
		}
	}
	for i := range w.messages {
		note(w.messages[i].ThreadID, w.messages[i].CreatedAt)
	}
	for i := range w.steering {
		note(w.steering[i].ThreadID, w.steering[i].CreatedAt)
		note(w.steering[i].ThreadID, w.steering[i].UpdatedAt)
	}
	for id, comp := range w.composers {
		if comp == nil {
			continue
		}
		if comp.pending != nil {
			note(id, comp.pending.startedAt)
		}
		if comp.steering != nil {
			note(id, comp.steering.startedAt)
		}
	}
	for i := range w.tools {
		f.tools[w.tools[i].RunID] = append(f.tools[w.tools[i].RunID], w.tools[i])
	}
	for i := range w.requests {
		if s := w.requests[i].Status; s == "pending" || s == "responding" {
			f.waiting[w.requests[i].ThreadID] = true
		}
	}
	f.valid = true
}

// runTools returns a run's tool calls; it allocates only outside a frame.
func (w *workbench) runTools(runID string) []api.ToolCall {
	var out []api.ToolCall
	if w.frame.valid {
		return w.frame.tools[runID]
	}
	for _, tool := range w.tools {
		if tool.RunID == runID {
			out = append(out, tool)
		}
	}
	return out
}
