package desktop

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// Composer mentions follow T3 Code's composer chips:
// - a pasted thread link reads as its title in teal,
// - a picked skill reads as its name in violet,
// - a pasted image, PDF or file reads as "@name" (no emoji), colored by kind.
// Deleting that text removes only the reference. The attachment itself stays
// until its thumbnail is dismissed or the paste is undone.

type mentionKind uint8

const (
	mentionThread mentionKind = iota + 1
	mentionSkill
	mentionAttachment
)

type composerMention struct {
	kind         mentionKind
	display      string // what the draft shows, e.g. "@Fix login", "/brave", "@img.png"
	expand       string // what send delivers
	attachmentID string // non-empty for inline pasted attachments
}

// threadLinkPattern matches api.FormatThreadLink and its environment form.
var threadLinkPattern = regexp.MustCompile(`\[([^\[\]\n]{1,300})\]\((vantry-thread://[^\s()]+)\)`)

// skillTokenPattern finds an explicit /skill:name typed or pasted by hand.
var skillTokenPattern = regexp.MustCompile(`(^|\s)(/skill:[A-Za-z0-9._-]+)`)

func formatByteSize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", max(int64(0), size))
	}
}

type attachmentClass uint8

const (
	attachFile attachmentClass = iota
	attachImage
	attachPDF
)

func classifyAttachment(item api.Attachment) attachmentClass {
	mime := strings.ToLower(strings.TrimSpace(item.MIME))
	ext := strings.ToLower(filepath.Ext(item.Name))
	switch {
	case strings.HasPrefix(mime, "image/") || ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".gif" || ext == ".webp":
		return attachImage
	case strings.HasPrefix(mime, "application/pdf") || ext == ".pdf":
		return attachPDF
	default:
		return attachFile
	}
}

// middleTruncateAttachmentName shortens a file name from the middle, matching
// T3 Code's middleTruncateAttachmentName so the extension stays visible.
func middleTruncateAttachmentName(name string, maxCharacters int) string {
	characters := []rune(name)
	if len(characters) <= maxCharacters {
		return name
	}
	if maxCharacters <= 0 {
		return ""
	}
	if maxCharacters == 1 {
		return "…"
	}
	available := maxCharacters - 1
	suffixLength := (available + 1) / 2
	if available >= 18 {
		suffixLength = 14
	}
	if suffixLength > available-1 {
		suffixLength = available - 1
	}
	prefixLength := available - suffixLength
	suffix := ""
	if suffixLength > 0 {
		suffix = string(characters[len(characters)-suffixLength:])
	}
	return string(characters[:prefixLength]) + "…" + suffix
}

func (c *composer) buildInlineAttachmentChip(item api.Attachment) string {
	base := "@" + middleTruncateAttachmentName(item.Name, 36)
	candidate := base
	seq := 2
	for {
		conflict := false
		for _, m := range c.mentions {
			if m.display == candidate && m.attachmentID != item.ID {
				conflict = true
				break
			}
		}
		if !conflict {
			return candidate
		}
		candidate = fmt.Sprintf("%s #%d", base, seq)
		seq++
	}
}

func (c *composer) attachmentExpand(item api.Attachment) string {
	return "@" + item.Name
}

func mentionTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if utf8.RuneCountInString(title) > 48 {
		title = string([]rune(title)[:47]) + "…"
	}
	return title
}

// addMention records m, replacing an older mention with the same display.
func (c *composer) addMention(m composerMention) {
	for i := range c.mentions {
		if c.mentions[i].display == m.display {
			c.mentions[i] = m
			return
		}
	}
	c.mentions = append(c.mentions, m)
}

// collapseThreadLinks turns every Markdown thread link in draft into its
// "@title" mention. caret is in runes; the returned caret keeps its place
// relative to the text around it.
func (c *composer) collapseThreadLinks(draft string, caret int) (string, int, bool) {
	matches := threadLinkPattern.FindAllStringSubmatchIndex(draft, -1)
	if len(matches) == 0 {
		return draft, caret, false
	}
	var out strings.Builder
	last := 0
	shift := 0
	for _, m := range matches {
		full := draft[m[0]:m[1]]
		display := "@" + mentionTitle(draft[m[2]:m[3]])
		c.addMention(composerMention{kind: mentionThread, display: display, expand: full})
		out.WriteString(draft[last:m[0]])
		out.WriteString(display)
		startRunes := utf8.RuneCountInString(draft[:m[0]])
		endRunes := utf8.RuneCountInString(draft[:m[1]])
		removed := (endRunes - startRunes) - utf8.RuneCountInString(display)
		switch {
		case caret >= endRunes:
			shift += removed
		case caret > startRunes:
			shift += caret - startRunes - utf8.RuneCountInString(display)
		}
		last = m[1]
	}
	out.WriteString(draft[last:])
	return out.String(), max(0, caret-shift), true
}

// pruneMentions forgets mentions whose display text left the draft.
// Attachment bytes stay: only the thumbnail's × or undoing the paste removes them.
func (c *composer) pruneMentions() {
	kept := c.mentions[:0]
	for _, m := range c.mentions {
		if strings.Contains(c.draft, m.display) {
			kept = append(kept, m)
		}
	}
	c.mentions = kept
}

// outgoing is the draft as it is sent: every mention expanded. Longer
// displays go first so "@ab" never rewrites part of "@abc".
func (c *composer) outgoing() string {
	text := c.draft
	if len(c.mentions) == 0 {
		return text
	}
	ordered := append([]composerMention(nil), c.mentions...)
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i].display) > len(ordered[j].display) })
	pairs := make([]string, 0, len(ordered)*2)
	for _, m := range ordered {
		pairs = append(pairs, m.display, m.expand)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// chipColors are T3 Code's context-chip hues: image sky, file indigo, PDF red,
// thread teal, skill violet. Each stays readable on the composer card.
func chipColors(p palette) (image, file, pdf, thread, skill ui.Color) {
	if p.dark {
		return ui.Hex("#7DD3FC"), ui.Hex("#A5B4FC"), ui.Hex("#FCA5A5"), ui.Hex("#5EEAD4"), ui.Hex("#D8B4FE")
	}
	return ui.Hex("#0369A1"), ui.Hex("#4338CA"), ui.Hex("#B91C1C"), ui.Hex("#0F766E"), ui.Hex("#7E22CE")
}

func (c *composer) mentionColor(m composerMention, p palette) (ui.Color, int) {
	image, file, pdf, thread, skill := chipColors(p)
	switch m.kind {
	case mentionSkill:
		return skill, 500
	case mentionAttachment:
		class := attachFile
		known := false
		for _, item := range c.attachments {
			if item.ID == m.attachmentID {
				class = classifyAttachment(item)
				known = true
				break
			}
		}
		if !known {
			switch {
			case strings.Contains(m.display, "🖼"):
				class = attachImage
			case strings.Contains(m.display, "📄"):
				class = attachPDF
			}
		}
		switch class {
		case attachImage:
			return image, 600
		case attachPDF:
			return pdf, 600
		default:
			return file, 600
		}
	default:
		return thread, 500
	}
}

// mentionRanges colors each mention and any hand-typed /skill:name in the draft.
func (c *composer) mentionRanges(p palette) []ui.TextRange {
	_, _, _, _, skillColor := chipColors(p)
	var ranges []ui.TextRange
	add := func(byteStart, byteEnd int, color ui.Color, weight int) {
		start := utf8.RuneCountInString(c.draft[:byteStart])
		ranges = append(ranges, ui.TextRange{Start: start, End: start + utf8.RuneCountInString(c.draft[byteStart:byteEnd]), Color: color, Weight: weight})
	}
	for _, m := range c.mentions {
		color, weight := c.mentionColor(m, p)
		for from := 0; from < len(c.draft); {
			i := strings.Index(c.draft[from:], m.display)
			if i < 0 {
				break
			}
			add(from+i, from+i+len(m.display), color, weight)
			from += i + len(m.display)
		}
	}
	for _, m := range skillTokenPattern.FindAllStringSubmatchIndex(c.draft, -1) {
		add(m[4], m[5], skillColor, 500)
	}
	return ranges
}
