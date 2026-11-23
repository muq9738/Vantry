package desktop

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// legacyInlineChip matches historical composer chips such as "[🖼 name · 48.2 KB]".
var legacyInlineChip = regexp.MustCompile(`\[(?:🖼|📄|📎) ([^\[\]\n]+)\]`)

type userPiece struct {
	text  string
	class attachmentClass
	chip  bool
}

func cleanChipInner(inner string) string {
	inner = strings.TrimSpace(inner)
	if i := strings.Index(inner, " · "); i >= 0 {
		inner = strings.TrimSpace(inner[:i])
	}
	if i := strings.LastIndex(inner, " #"); i > 0 {
		suffix := inner[i+2:]
		if suffix != "" && strings.Trim(suffix, "0123456789") == "" {
			inner = strings.TrimSpace(inner[:i])
		}
	}
	return inner
}

func chipBoundary(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("，。！？、；：,;:!?)]）", r)
}

func userMessagePieces(input string, items []api.Attachment) []userPiece {
	ordered := append([]api.Attachment(nil), items...)
	for i := 1; i < len(ordered); i++ {
		if len(ordered[i].Name) > len(ordered[i-1].Name) {
			ordered[i], ordered[i-1] = ordered[i-1], ordered[i]
			i = 0
		}
	}
	var out []userPiece
	rest := input
	for rest != "" {
		at := len(rest)
		take := ""
		class := attachFile
		chip := false
		if loc := legacyInlineChip.FindStringIndex(rest); loc != nil && loc[0] <= at {
			at = loc[0]
			take = rest[loc[0]:loc[1]]
			switch {
			case strings.Contains(take, "🖼"):
				class = attachImage
			case strings.Contains(take, "📄"):
				class = attachPDF
			}
			chip = true
		}
		for _, item := range ordered {
			if item.Name == "" {
				continue
			}
			token := "@" + item.Name
			i := strings.Index(rest, token)
			if i < 0 || i > at {
				continue
			}
			if i > 0 {
				r, _ := utf8.DecodeLastRuneInString(rest[:i])
				if !chipBoundary(r) {
					continue
				}
			}
			after := i + len(token)
			if after < len(rest) {
				r, _ := utf8.DecodeRuneInString(rest[after:])
				if !chipBoundary(r) {
					continue
				}
			}
			at = i
			take = token
			class = classifyAttachment(item)
			chip = true
		}
		if !chip {
			out = append(out, userPiece{text: rest})
			break
		}
		if at > 0 {
			out = append(out, userPiece{text: rest[:at]})
		}
		label := take
		if strings.HasPrefix(take, "[") {
			inner := take[len("[🖼 "):]
			if i := strings.LastIndex(take, "]"); i >= 0 {
				inner = take[strings.Index(take, " ")+1 : i]
			}
			label = "@" + cleanChipInner(inner)
		}
		out = append(out, userPiece{text: label, class: class, chip: true})
		rest = rest[at+len(take):]
	}
	return out
}

func visibleUserMessage(input string, items []api.Attachment) string {
	var b strings.Builder
	for _, piece := range userMessagePieces(input, items) {
		b.WriteString(piece.text)
	}
	return b.String()
}

func piecesHaveChip(pieces []userPiece) bool {
	for _, piece := range pieces {
		if piece.chip {
			return true
		}
	}
	return false
}

func chipColorFor(p palette, class attachmentClass) ui.Color {
	image, file, pdf, _, _ := chipColors(p)
	switch class {
	case attachImage:
		return image
	case attachPDF:
		return pdf
	default:
		return file
	}
}

func (w *workbench) userMessageBody(c *ui.Context, p palette, input string, items []api.Attachment) {
	pieces := userMessagePieces(input, items)
	switch {
	case strings.TrimSpace(input) == "":
	case !piecesHaveChip(pieces):
		ui.Text(c, input).FontSize(14.5).LineHeight(1.58).TextColor(p.ink).Wrap()
	default:
		spans := make([]ui.Span, 0, len(pieces))
		for _, piece := range pieces {
			if !piece.chip {
				spans = append(spans, ui.Span{Text: piece.text})
				continue
			}
			color := chipColorFor(p, piece.class)
			spans = append(spans, ui.Span{
				Text: piece.text, Color: color, Weight: 600,
				Background: color.Alpha(0.16), Radius: 5, PaddingX: 5, PaddingY: 1,
			})
		}
		ui.RichText(c, spans...).FontSize(14.5).LineHeight(1.58).TextColor(p.ink).Wrap()
	}
	w.sentAttachmentStrip(c, p, items)
}

func (w *workbench) sentAttachmentStrip(c *ui.Context, p palette, items []api.Attachment) {
	var images, others []api.Attachment
	for _, item := range items {
		if classifyAttachment(item) == attachImage {
			images = append(images, item)
		} else if item.Name != "" {
			others = append(others, item)
		}
	}
	if len(images) > 0 {
		ui.Row(c).Gap(6).Wrap().Children(func() {
			for idx, item := range images {
				idx, item := idx, item
				frame := ui.Button(c.Key("sent-image:"+item.ID), "").Label(w.tr3("查看图片 ", "View image ", "画像を表示 ")+item.Name).
					Size(72, 72).Padding(0).Radius(12).Clip().Border(1, p.line).Background(p.soft).Shrink(0)
				frame.Description(item.Name)
				if frame.Hovered() {
					frame.Border(1, p.accent.Alpha(0.6))
				}
				if frame.Clicked() {
					w.visuals.lightbox, w.visuals.lightboxIndex = images, idx
				}
				frame.Children(func() {
					if bitmap := attachmentPreview(item); bitmap != nil {
						ui.Image(c, bitmap).Size(72, 72).Fit(ui.Cover)
					} else {
						w.loadAttachmentPreview(item)
						ui.Icon(c, imageAttachmentIcon).FontSize(16).TextColor(p.faint)
					}
				})
			}
		})
	}
	for _, item := range others {
		icon := fileIcon
		if classifyAttachment(item) == attachPDF {
			icon = pdfAttachmentIcon
		}
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icon).FontSize(12).TextColor(p.muted).Shrink(0)
			ui.Text(c, item.Name).FontSize(12).TextColor(p.muted).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			if item.Size > 0 {
				ui.Text(c, "· "+formatByteSize(item.Size)).FontSize(11).TextColor(p.faint).NoWrap().Shrink(0)
			}
		})
	}
}
