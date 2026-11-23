package desktop

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/muq9738/Vantry/pkg/api"
)

// An ask card is one ```ask fence. The answer is kept on the workbench,
// keyed by thread, message and block text, so reopening the window does
// not ask again. A click sends that answer through the composer path.

type askCardKey struct{}

type askUI struct {
	picks  map[int]bool
	other  bool
	custom string
}

type askAnswerRecord struct {
	Answer  string `json:"answer,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
}

func cloneAskAnswers(in map[string]askAnswerRecord) map[string]askAnswerRecord {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]askAnswerRecord, len(in))
	for key, rec := range in {
		out[key] = rec
	}
	return out
}

type askHotkey struct {
	ready bool
	count int
	pick  func(int)
}

type askOption struct {
	label, note string
	recommended bool
}

func askAnswerKey(threadID, messageID, content string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(content)))
	return threadID + "\x1f" + messageID + "\x1f" + hex.EncodeToString(sum[:8])
}

func (w *workbench) askRecord(key string) (askAnswerRecord, bool) {
	if w.visuals.askAnswers == nil {
		return askAnswerRecord{}, false
	}
	rec, ok := w.visuals.askAnswers[key]
	return rec, ok
}

func (w *workbench) rememberAsk(key string, rec askAnswerRecord) {
	if w.visuals.askAnswers == nil {
		w.visuals.askAnswers = map[string]askAnswerRecord{}
	}
	w.visuals.askAnswers[key] = rec
	w.saveState()
}

func (w *workbench) askBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr3("正在准备决策选项…", "Preparing options…", "選択肢を準備しています…"))
		return
	}
	_, words := parseFenceInfoAttrs(b.Info)
	multi := false
	for _, word := range words {
		if strings.EqualFold(word, "multi") {
			multi = true
		}
	}
	question, options := parseAskOptions(b.Code())
	if len(options) == 0 {
		markdown.DefaultCodeBlock(c, b)
		return
	}
	threadID := w.selected
	key := askAnswerKey(threadID, w.visuals.renderingMessage, b.Info+"\n"+b.Code())
	if rec, ok := w.askRecord(key); ok {
		w.askAnswered(c, p, rec)
		return
	}
	card := ui.Column(c).FillWidth().Radius(12).Background(p.raised).Border(1, p.line).Padding(12, 14).Gap(8)
	state := ui.Local(card, askCardKey{}, func() *askUI { return &askUI{picks: map[int]bool{}} })
	card.Children(func() {
		w.askHeader(c, p, question, multi)
		for idx, opt := range options {
			w.askOptionRow(c, p, *state, opt, idx, multi, key, question)
		}
		w.askOtherRow(c, p, *state, key, question)
		w.askFooter(c, p, *state, options, multi, key, question)
	})
	uiState := *state
	w.visuals.askHotkey = askHotkey{
		ready: true,
		count: len(options),
		pick: func(i int) {
			if i < 0 || i >= len(options) {
				return
			}
			if multi {
				uiState.picks[i] = !uiState.picks[i]
				return
			}
			w.commitAsk(key, w.formatAskReply(question, []string{options[i].label}))
		},
	}
}

func parseAskOptions(code string) (string, []askOption) {
	var question string
	var options []askOption
	inPreview := false
	for _, raw := range nonBlankLines(code) {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, ":::") {
			inPreview = !inPreview
			continue
		}
		if inPreview {
			continue
		}
		if strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "- ") {
			rec := trimmed[0] == '*'
			parts := splitPipeFields(strings.TrimSpace(trimmed[2:]))
			note := ""
			if len(parts) > 1 {
				note = strings.Join(parts[1:], " · ")
			}
			options = append(options, askOption{label: parts[0], note: note, recommended: rec})
			continue
		}
		if question == "" {
			question = trimmed
		}
	}
	return question, options
}

func (w *workbench) askHeader(c *ui.Context, p palette, question string, multi bool) {
	hint := w.tr3("点选即发送", "Choose one to send", "選ぶと送信")
	tone := p.muted
	if multi {
		hint = w.tr3("可多选", "Multi-select", "複数選択")
		tone = p.accentInk
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, nonEmptyString(question, w.tr3("请选择方案", "Choose an option", "選択肢を選んでください"))).FontSize(14).FontWeight(620).TextColor(p.ink).Wrap()
		})
		kitBadge(c, hint, tone)
	})
}

func (w *workbench) askOptionRow(c *ui.Context, p palette, state *askUI, opt askOption, idx int, multi bool, key, question string) {
	selected := state.picks[idx]
	row := ui.Button(c.Key("opt:"+strconv.Itoa(idx)), "").Label(opt.label).FillWidth().Padding(7, 8).Radius(8).
		Justify(ui.Start).Border(0, p.line).Background(ui.Transparent)
	switch {
	case selected:
		row.Background(p.accent.Alpha(0.1))
	case row.Hovered():
		row.Background(p.rowHover)
	}
	if row.FocusVisible() {
		row.Shadow(0, 0, 0, 2, p.accent.Alpha(0.35))
	}
	row.Children(func() {
		ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
			kitIndex(c, p, strconv.Itoa(idx+1), selected)
			ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
					ui.Text(c, opt.label).FontSize(13).FontWeight(550).TextColor(p.ink).Wrap()
					if opt.recommended {
						kitBadge(c, w.tr3("推荐", "Recommended", "おすすめ"), p.accentInk)
					}
				})
				if opt.note != "" {
					ui.Text(c, opt.note).FontSize(12).TextColor(p.muted).Wrap()
				}
			})
		})
	})
	if row.Clicked() {
		if multi {
			state.picks[idx] = !selected
			return
		}
		w.commitAsk(key, w.formatAskReply(question, []string{opt.label}))
	}
}

func (w *workbench) askOtherRow(c *ui.Context, p palette, state *askUI, key, question string) {
	label := w.tr3("其他…", "Other…", "その他…")
	row := ui.Button(c.Key("ask-other"), "").Label(label).FillWidth().Padding(7, 8).Radius(8).
		Justify(ui.Start).Border(0, p.line).Background(ui.Transparent)
	if state.other {
		row.Background(p.soft)
	} else if row.Hovered() {
		row.Background(p.rowHover)
	}
	row.Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			kitIndex(c, p, "…", state.other)
			ui.Text(c, label).FontSize(13).TextColor(p.ink)
		})
	})
	if row.Clicked() {
		state.other = !state.other
	}
	if !state.other {
		return
	}
	field := kitField(ui.TextInput(c.Key("ask-custom"), &state.custom).Label(w.tr3("自定义答案", "Your own answer", "自分の回答")).
		Placeholder(w.tr3("输入自定义答案", "Type your own answer", "自分の回答を入力")).FillWidth().Bind(&w.visuals.askCustomFocus), p, false)
	send := kitButton(c, p, "ask-custom-send", w.tr3("发送", "Send", "送信"), controlPrimary, controlSM)
	if strings.TrimSpace(state.custom) == "" {
		send.Disabled(true)
	}
	if send.Clicked() || field.Submitted() {
		if strings.TrimSpace(state.custom) != "" {
			w.commitAsk(key, w.formatAskReply(question, []string{strings.TrimSpace(state.custom)}))
		}
	}
}

func (w *workbench) askFooter(c *ui.Context, p palette, state *askUI, options []askOption, multi bool, key, question string) {
	var picked []string
	for idx, opt := range options {
		if state.picks[idx] {
			picked = append(picked, opt.label)
		}
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(2, 0, 0).Children(func() {
		if multi {
			submit := kitButton(c, p, "ask-submit", w.tr3("提交", "Submit", "送信"), controlPrimary, controlSM)
			if custom := strings.TrimSpace(state.custom); custom != "" {
				picked = append(picked, custom)
			}
			if len(picked) == 0 {
				submit.Disabled(true)
			}
			if submit.Clicked() && len(picked) > 0 {
				w.commitAsk(key, w.formatAskReply(question, picked))
			}
		}
		ui.Spacer(c)
		if kitButton(c, p, "ask-skip", w.tr3("跳过", "Skip", "スキップ"), controlOutline, controlSM).Clicked() {
			w.rememberAsk(key, askAnswerRecord{Skipped: true})
		}
	})
}

func (w *workbench) commitAsk(key, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if !w.deliverAsk(w.selected, text) {
		return
	}
	w.rememberAsk(key, askAnswerRecord{Answer: text})
}

func (w *workbench) askAnswered(c *ui.Context, p palette, rec askAnswerRecord) {
	text := w.tr3("已跳过", "Skipped", "スキップ済み")
	tone := p.muted
	if !rec.Skipped {
		text = w.tr3("已回答：", "Answered: ", "回答済み：") + rec.Answer
		tone = p.accentInk
	}
	ui.Row(c).FillWidth().Padding(10, 12).Gap(8).Radius(12).AlignItems(ui.Center).Background(p.raised).Border(1, p.line).Children(func() {
		ui.Icon(c, checkIcon).FontSize(14).TextColor(tone).Shrink(0)
		ui.Text(c, text).FontSize(13).TextColor(p.ink).Wrap().Grow(1)
	})
}

func (w *workbench) formatAskReply(question string, picked []string) string {
	ans := strings.Join(picked, "、")
	if strings.TrimSpace(question) == "" {
		return "选择方案：" + ans
	}
	return fmt.Sprintf("针对「%s」，我选择：%s", question, ans)
}

// deliverAsk sends text as a user message without discarding an unsent draft.
// A running turn uses the composer's queue or steer path.
func (w *workbench) deliverAsk(threadID, text string) bool {
	text = strings.TrimSpace(text)
	if threadID == "" || text == "" {
		return false
	}
	comp := w.composerFor(threadID)
	savedDraft := comp.draft
	savedAttachments := append([]api.Attachment(nil), comp.attachments...)
	savedMentions := append([]composerMention(nil), comp.mentions...)
	comp.draft = text
	comp.attachments = nil
	comp.mentions = nil
	run, running := w.activeRun(threadID)
	var ok bool
	if running {
		ok = w.queueDraft(threadID, w.widgets.steerMode && w.canSteer(run, true))
	} else {
		ok = w.send(threadID)
	}
	comp.draft = savedDraft
	comp.attachments = savedAttachments
	comp.mentions = savedMentions
	if ok {
		comp.feedback = notice{}
		w.saveState()
	}
	return ok
}

func (w *workbench) bindAskHotkeys(c *ui.Context) {
	hk := w.visuals.askHotkey
	if !hk.ready || hk.pick == nil {
		return
	}
	if w.widgets.composerFocus.Focused(c) || w.visuals.askCustomFocus.Focused(c) || w.widgets.searchFocus.Focused(c) {
		return
	}
	if w.stage != "" || w.widgets.settingsOpen || w.widgets.modelPickerOpen || w.widgets.projectPickerOpen || w.widgets.workspaceOpen || w.widgets.queueOpen || w.widgets.shortcutsOpen || w.widgets.thinkingOpen || w.widgets.diffOpen || w.visuals.expanded || len(w.visuals.lightbox) > 0 {
		return
	}
	keys := []ui.Key{ui.Key1, ui.Key2, ui.Key3, ui.Key4, ui.Key5, ui.Key6, ui.Key7, ui.Key8, ui.Key9}
	for i := 0; i < hk.count && i < len(keys); i++ {
		if c.Shortcut(0, keys[i]) {
			hk.pick(i)
		}
	}
}
