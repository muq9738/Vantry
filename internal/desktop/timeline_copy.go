package desktop

import (
	"strings"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
)

// copyConversationSelection is what a drag across the timeline copies:
// the glyphs the reader selected, without object-replacement characters,
// and with a blank line between paragraphs.
func copyConversationSelection(parts []ui.SelectedText) transfer.Data {
	return transfer.TextData(copiedParagraphs(parts))
}

func copiedParagraphs(parts []ui.SelectedText) string {
	var b strings.Builder
	for i, part := range parts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(stripCopyObjects(runeSlice(part.Text, part.Start, part.End)))
	}
	return b.String()
}

func stripCopyObjects(s string) string {
	return strings.NewReplacer("\uFFFC", "", "\u2060", "").Replace(s)
}
