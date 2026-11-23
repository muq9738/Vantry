package desktop

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// The composer asks the target Core for its real skills; the UI never scans
// skill directories of its own. Core answers action=skills.list through the
// existing Workbench envelope with Output as JSON:
// {"skills":[{"name":"docx","description":"…","filePath":"/target/…/SKILL.md",
// "source":"…","command":"skill:docx"}]}
const actionSkillsList = "skills.list"

// skillEntry is the skill metadata Core reports, kept display-only.
type skillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	FilePath    string `json:"filePath,omitempty"`
	Source      string `json:"source,omitempty"`
	Command     string `json:"command,omitempty"`
}

// token is what the draft needs after the "/": Core names the actual
// invocation; skill:<name> stays the default contract. Built-in local slash
// commands keep their bare command name.
func (s skillEntry) token() string {
	if token := strings.TrimSpace(s.Command); token != "" {
		return token
	}
	return "skill:" + s.Name
}

func builtinSlashCommands(w *workbench) []skillEntry {
	return []skillEntry{
		{Name: "fork", Description: w.tr("分叉当前会话（支持切换模型或独立 Worktree）", "Fork current thread with optional model or worktree"), Source: "builtin", Command: "fork"},
		{Name: "rewind", Description: w.tr("从历史轮次或文件检查点回退", "Rewind conversation turns or workspace checkpoints"), Source: "builtin", Command: "rewind"},
	}
}

func decodeSkillEntries(response api.WorkbenchResponse) []skillEntry {
	var payload struct {
		Skills []skillEntry `json:"skills"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(response.Output)), &payload); err != nil {
		return nil
	}
	entries := payload.Skills[:0]
	for _, entry := range payload.Skills {
		if strings.TrimSpace(entry.Name) != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// ensureSkills asks Core for the catalog of the target's scope once per
// scope; a failed catalog stays so an error row can offer retry.
func (w *workbench) ensureSkills(scope string, request api.WorkbenchRequest) {
	if w.skillCatalogs != nil {
		if _, loaded := w.skillCatalogs[scope]; loaded {
			return
		}
	}
	if w.features.busy["skills.list:"+scope] {
		return
	}
	request.Action = actionSkillsList
	w.workbenchCall("skills.list:"+scope, request, func(response api.WorkbenchResponse, err error) {
		if w.skillCatalogs == nil {
			w.skillCatalogs, w.skillErrs = map[string][]skillEntry{}, map[string]string{}
		}
		if err != nil {
			w.skillCatalogs[scope] = nil
			w.skillErrs[scope] = describe(w.tr("读取技能列表失败", "Could not load the skill list"), err)
			return
		}
		w.skillCatalogs[scope] = decodeSkillEntries(response)
		delete(w.skillErrs, scope)
	})
}

// skillQuery reports the /token being typed at the end of the draft. A /
// inside a word (a file path or URL like /usr/bin or x/y) is not a command,
// the same boundary rule @ mentions use.
func skillQuery(draft string) (query string, start int, ok bool) {
	slash := strings.LastIndexByte(draft, '/')
	if slash < 0 {
		return "", 0, false
	}
	if slash > 0 {
		r, _ := utf8.DecodeLastRuneInString(draft[:slash])
		if !unicode.IsSpace(r) {
			return "", 0, false
		}
	}
	query = draft[slash+1:]
	if strings.IndexFunc(query, unicode.IsSpace) >= 0 || utf8.RuneCountInString(query) > 80 {
		return "", 0, false
	}
	return query, slash, true
}

func matchSkill(entry skillEntry, query string) int {
	if query == "" {
		return 2
	}
	lower := strings.ToLower(query)
	name := strings.ToLower(entry.Name)
	if strings.HasPrefix(name, lower) {
		return 0
	}
	if strings.Contains(name, lower) {
		return 1
	}
	if strings.Contains(strings.ToLower(entry.Description), lower) {
		return 2
	}
	return -1
}

func filterSkills(entries []skillEntry, query string) []skillEntry {
	type rank struct {
		entry skillEntry
		score int
	}
	ranked := make([]rank, 0, len(entries))
	for _, entry := range entries {
		if score := matchSkill(entry, query); score >= 0 {
			ranked = append(ranked, rank{entry, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score < ranked[j].score })
	out := make([]skillEntry, 0, min(len(ranked), 8))
	for i, r := range ranked {
		if i >= 8 {
			break
		}
		out = append(out, r.entry)
	}
	return out
}

type skillState struct {
	open       bool
	query      string
	start      int
	candidates []skillEntry
	scope      string
	err        string
	loading    bool
	none       bool // the target answered with an empty catalog
}

// skillScope also supports a projectless draft: the target Core can discover
// its global skills without preparing a conversation or reading another host.
func (w *workbench) skillScope(target string) (string, api.WorkbenchRequest, bool) {
	if scope, request, ok := w.fileScope(target); ok {
		return scope, request, true
	}
	if isLocalSession(target) {
		return "global", api.WorkbenchRequest{}, true
	}
	return "", api.WorkbenchRequest{}, false
}

// skillStateFor reports the picker as the draft ends: closed when the token
// is gone or was dismissed, loading/empty/error rows otherwise.
func (w *workbench) skillStateFor(target, draft string) skillState {
	query, start, ok := skillQuery(draft)
	if !ok || target == "" {
		return skillState{}
	}
	comp := w.composerFor(target)
	if comp.skillDismissed == draft[start:] {
		return skillState{}
	}
	scope, request, ok := w.skillScope(target)
	if !ok {
		return skillState{open: true, query: query, start: start}
	}
	if _, loaded := w.skillCatalogs[scope]; !loaded {
		w.ensureSkills(scope, request)
	}
	catalog, loaded := w.skillCatalogs[scope]
	combined := append([]skillEntry(nil), catalog...)
	if query != "" {
		combined = append(combined, builtinSlashCommands(w)...)
	}
	candidates := filterSkills(combined, query)
	if query == "" && len(catalog) > 0 && len(candidates) < 8 {
		for _, bi := range builtinSlashCommands(w) {
			if len(candidates) >= 8 {
				break
			}
			candidates = append(candidates, bi)
		}
	}
	state := skillState{
		open:       true,
		query:      query,
		start:      start,
		scope:      scope,
		err:        w.skillErrs[scope],
		loading:    w.features.busy["skills.list:"+scope] || !loaded,
		none:       loaded && len(catalog) == 0 && len(candidates) == 0,
		candidates: candidates,
	}
	if comp.skillIndex >= len(state.candidates) {
		comp.skillIndex = max(0, len(state.candidates)-1)
	}
	return state
}

func insertSkill(draft string, start int, entry skillEntry) string {
	return draft[:start] + skillMention(entry).display + " "
}

// skillMention shows a picked skill by its name, as T3 Code's skill chip;
// send restores the /token Core expects.
func skillMention(entry skillEntry) composerMention {
	return composerMention{kind: mentionSkill, display: "/" + entry.Name, expand: "/" + entry.token()}
}

// skillTextKey handles the picker's keys while it is open, between frames,
// against the text as typed — as pickerTextKey does for @ files.
func (w *workbench) skillTextKey(target string, k ui.TextKey) bool {
	switch k.Key {
	case ui.KeyUp, ui.KeyDown, ui.KeyEnter, ui.KeyTab, ui.KeyEscape:
	default:
		return false
	}
	if k.Mods != 0 {
		return false
	}
	text := k.Text()
	state := w.skillStateFor(target, text)
	if !state.open {
		return false
	}
	comp := w.composerFor(target)
	switch k.Key {
	case ui.KeyEscape:
		comp.skillDismissed = text[state.start:]
		return true
	case ui.KeyUp:
		if len(state.candidates) > 0 {
			comp.skillIndex = (comp.skillIndex - 1 + len(state.candidates)) % len(state.candidates)
		}
		return true
	case ui.KeyDown:
		if len(state.candidates) > 0 {
			comp.skillIndex = (comp.skillIndex + 1) % len(state.candidates)
		}
		return true
	}
	if len(state.candidates) == 0 {
		return true // A loading, empty or failed menu must not submit its token.
	}
	chosen := state.candidates[min(comp.skillIndex, len(state.candidates)-1)]
	next := insertSkill(text, state.start, chosen)
	comp.addMention(skillMention(chosen))
	k.SetText(next)
	w.setDraft(target, next)
	comp.skillIndex, comp.skillDismissed = 0, ""
	// SetText clamps the caret to the old end: park it after the token.
	comp.caretAfter = utf8.RuneCountInString(next)
	comp.caretRestore = true
	return true
}

// skillPickerView floats the skill candidates just above the composer, as
// the @ file picker does. It never takes focus: typing continues in the
// input while the catalog arrives.
func (w *workbench) skillPickerView(c *ui.Context, p palette, target, draft string) {
	state := w.skillStateFor(target, draft)
	if !state.open {
		return
	}
	comp := w.composerFor(target)
	ui.Column(c.Key("skill-picker")).FillWidth().Padding(5).Gap(1).Margin(0, 0, 6).Radius(12).Background(p.raised).Border(1, p.line).
		Role(ui.RoleListBox).Label(w.tr("技能候选", "Skill candidates")).Children(func() {
		ui.Row(c).Height(22).Padding(0, 7).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "/").Font("Menlo").FontSize(11).Bold().TextColor(p.accentInk)
			label := w.tr("调用目标环境技能", "Run a skill of the target")
			if state.query != "" {
				label = w.tr("匹配 “", "Matching “") + state.query + "”"
			}
			ui.Text(c, label).FontSize(10).TextColor(p.muted).Grow(1).SingleLine().Ellipsis("…")
			ui.Text(c, w.tr("↑↓ 选择 · Enter 插入 · Esc 关闭", "↑↓ pick · Enter insert · Esc close")).FontSize(10).TextColor(p.faint).NoWrap()
		})
		switch {
		case state.scope == "":
			ui.Text(c, w.tr("目标会话暂不可用，请重新选择会话。", "The target thread is unavailable; select it again.")).FontSize(11).TextColor(p.muted).Padding(4, 8)
			return
		case state.err != "":
			ui.Row(c).Gap(8).Padding(4, 8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, state.err).FontSize(11).TextColor(p.warning).Wrap().Grow(1).MinWidth(0)
				if quietButton(c, p, "retry-skills", w.tr("重试", "Retry")).Clicked() {
					delete(w.skillErrs, state.scope)
					delete(w.skillCatalogs, state.scope)
					if _, request, ok := w.skillScope(target); ok {
						w.ensureSkills(state.scope, request)
					}
				}
			})
			return
		case state.loading && len(state.candidates) == 0:
			ui.Row(c).Gap(6).Padding(4, 8).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c).Size(11, 11).Label(w.tr("正在读取技能", "Loading skills"))
				ui.Text(c, w.tr("正在读取技能…可继续输入", "Loading skills… you can keep typing")).FontSize(11).TextColor(p.muted)
			})
			return
		case state.none:
			ui.Text(c, w.tr("目标环境没有可用技能", "No skills on the target")).FontSize(11).TextColor(p.muted).Padding(4, 8)
			return
		case len(state.candidates) == 0:
			ui.Text(c, w.tr("没有匹配的技能", "No matching skills")).FontSize(11).TextColor(p.muted).Padding(4, 8)
			return
		}
		for i, candidate := range state.candidates {
			candidate := candidate
			option := ui.Button(c.Key("skill:"+candidate.Name), "").Label("/"+candidate.token()).Role(ui.RoleOption).AccessSelected(i == comp.skillIndex).
				FillWidth().Height(26).Padding(2, 8).Border(0, p.line).Background(ui.Transparent).Radius(7).Justify(ui.Start)
			if i == comp.skillIndex || option.Hovered() {
				option.Background(p.selected)
			}
			option.Children(func() {
				ui.Icon(c, toolIcon).FontSize(12).TextColor(p.muted)
				ui.Text(c, "/"+candidate.Name).Font("Menlo").FontSize(12).TextColor(p.ink).Shrink(0).MaxWidth(200).SingleLine().Ellipsis("…")
				if candidate.Description != "" {
					ui.Text(c, candidate.Description).FontSize(11).TextColor(p.faint).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
				}
			})
			if option.Clicked() {
				next := insertSkill(draft, state.start, candidate)
				comp.addMention(skillMention(candidate))
				w.setDraft(target, next)
				comp.skillIndex, comp.skillDismissed = 0, ""
				comp.caretAfter = utf8.RuneCountInString(next)
				comp.caretRestore = true
				w.widgets.composerFocus.Focus()
			}
		}
		if state.loading {
			ui.Text(c, fmt.Sprintf(w.tr("已显示 %d 个匹配，正在读取技能…", "Showing %d matches, loading skills…"), len(state.candidates))).FontSize(10).TextColor(p.faint).Padding(2, 8)
		}
	})
}
