package desktop

import (
	"strings"
)

// appendOutput grows a streaming reply without copying it for each delta.
// Normal appends compare the same string backing store, which Go handles
// without scanning. A replacement with different text starts a fresh builder.
func (w *workbench) appendOutput(runID, current, delta string) string {
	if w.outputs == nil {
		w.outputs = map[string]*strings.Builder{}
	}
	b := w.outputs[runID]
	if b == nil || b.String() != current {
		b = &strings.Builder{}
		b.Grow(max(2*len(current), 4096))
		b.WriteString(current)
		w.outputs[runID] = b
	}
	b.WriteString(delta)
	return b.String()
}

// releaseOutput drops a finished reply's builder; its string stays valid.
func (w *workbench) releaseOutput(runID string) {
	delete(w.outputs, runID)
	delete(w.outputs, "reasoning:"+runID)
}
