package desktop

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
)

// A ```dispatch (or ```orchestrate) block renders an interactive task-dispatch
// confirmation card before multi-agent orchestration starts. Users can toggle
// subtasks, adjust parallel worker counts, switch shared/worktree isolation,
// pick any locally available model, and confirm to run (or fall back to
// single-thread execution).

type dispatchCardKey struct{}

type dispatchSpec struct {
	Title string
	Items []dispatchItem
}

type dispatchItem struct {
	Title     string `json:"title"`
	Role      string `json:"role,omitempty"`
	Model     string `json:"model,omitempty"`
	Isolation string `json:"isolation,omitempty"`
	Count     int    `json:"count,omitempty"`
	Note      string `json:"note,omitempty"`
	Selected  bool   `json:"selected"`
}

type dispatchUI struct {
	initialized bool
	selected    map[int]bool
	models      map[int]string
	counts      map[int]int
	isolations  map[int]string
	pickingRow  int
	modelQuery  string
}

func parseDispatchSpec(code string) dispatchSpec {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return dispatchSpec{}
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if spec, ok := parseDispatchJSON(trimmed); ok {
			return spec
		}
	}
	var spec dispatchSpec
	for _, raw := range strings.Split(trimmed, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		checked := false
		isItem := false
		switch {
		case strings.HasPrefix(line, "* "):
			checked, isItem, line = true, true, strings.TrimSpace(line[2:])
		case strings.HasPrefix(line, "- [x] "), strings.HasPrefix(line, "- [X] "):
			checked, isItem, line = true, true, strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "- [ ] "):
			checked, isItem, line = false, true, strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "- "):
			checked, isItem, line = false, true, strings.TrimSpace(line[2:])
		case strings.Contains(line, "|"):
			checked, isItem = true, true
		}
		if !isItem {
			if spec.Title == "" && len(spec.Items) == 0 {
				spec.Title = line
			}
			continue
		}
		item := parseDispatchLine(line, checked)
		if item.Title != "" {
			spec.Items = append(spec.Items, item)
		}
	}
	return spec
}

func parseDispatchJSON(code string) (dispatchSpec, bool) {
	var raw struct {
		Title string         `json:"title"`
		Tasks []dispatchItem `json:"tasks"`
		Items []dispatchItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(code), &raw); err == nil {
		items := raw.Tasks
		if len(items) == 0 {
			items = raw.Items
		}
		if len(items) > 0 {
			for i := range items {
				normalizeDispatchItem(&items[i])
			}
			return dispatchSpec{Title: strings.TrimSpace(raw.Title), Items: items}, true
		}
	}
	var list []dispatchItem
	if err := json.Unmarshal([]byte(code), &list); err == nil && len(list) > 0 {
		for i := range list {
			normalizeDispatchItem(&list[i])
		}
		return dispatchSpec{Items: list}, true
	}
	return dispatchSpec{}, false
}

func parseDispatchLine(line string, checked bool) dispatchItem {
	parts := strings.Split(line, "|")
	item := dispatchItem{
		Title:     strings.TrimSpace(parts[0]),
		Isolation: "shared",
		Count:     1,
		Selected:  checked,
	}
	var notes []string
	for _, part := range parts[1:] {
		seg := strings.TrimSpace(part)
		if seg == "" {
			continue
		}
		if key, val, ok := strings.Cut(seg, "="); ok {
			k := strings.ToLower(strings.TrimSpace(key))
			v := strings.TrimSpace(val)
			switch k {
			case "role", "type", "kind", "类型", "角色":
				item.Role = normalizeDispatchRole(v)
			case "model", "m", "模型":
				item.Model = v
			case "isolation", "iso", "隔离":
				item.Isolation = normalizeDispatchIsolation(v)
			case "count", "n", "parallel", "workers", "路数", "并行":
				item.Count = clampDispatchCount(parseCountValue(v))
			case "note", "desc", "说明":
				if v != "" {
					notes = append(notes, v)
				}
			default:
				notes = append(notes, seg)
			}
			continue
		}
		lower := strings.ToLower(seg)
		switch {
		case lower == "worktree" || lower == "shared":
			item.Isolation = lower
		case parseCountShorthand(lower) > 0:
			item.Count = clampDispatchCount(parseCountShorthand(lower))
		case isKnownDispatchRole(lower) && item.Role == "":
			item.Role = normalizeDispatchRole(lower)
		default:
			notes = append(notes, seg)
		}
	}
	item.Note = strings.Join(notes, " · ")
	normalizeDispatchItem(&item)
	return item
}

func normalizeDispatchItem(item *dispatchItem) {
	item.Title = strings.TrimSpace(item.Title)
	item.Model = strings.TrimSpace(item.Model)
	item.Note = strings.TrimSpace(item.Note)
	item.Role = normalizeDispatchRole(item.Role)
	item.Isolation = normalizeDispatchIsolation(item.Isolation)
	item.Count = clampDispatchCount(item.Count)
}

func normalizeDispatchRole(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "research", "调研", "分析", "研究":
		return "research"
	case "design", "ui", "前端", "交互", "设计":
		return "design"
	case "implementation", "impl", "coding", "code", "script", "脚本", "实现", "开发":
		return "implementation"
	case "test", "qa", "测试":
		return "test"
	case "review", "审查", "评审":
		return "review"
	case "general", "grunt", "batch", "批处理", "苦力", "通用":
		return "general"
	default:
		return strings.TrimSpace(raw)
	}
}

func isKnownDispatchRole(lower string) bool {
	switch lower {
	case "research", "design", "ui", "implementation", "impl", "coding", "script", "test", "review", "general",
		"调研", "研究", "设计", "前端", "脚本", "实现", "开发", "测试", "审查", "通用":
		return true
	default:
		return false
	}
}

func normalizeDispatchIsolation(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "worktree") {
		return "worktree"
	}
	return "shared"
}

func parseCountValue(raw string) int {
	clean := strings.TrimSpace(strings.ToLower(raw))
	clean = strings.TrimPrefix(clean, "x")
	clean = strings.TrimPrefix(clean, "×")
	clean = strings.TrimSuffix(clean, "路")
	n, err := strconv.Atoi(strings.TrimSpace(clean))
	if err != nil {
		return 1
	}
	return n
}

func parseCountShorthand(lower string) int {
	if strings.HasPrefix(lower, "x") || strings.HasPrefix(lower, "×") || strings.HasSuffix(lower, "路") {
		n := parseCountValue(lower)
		if n > 0 {
			return n
		}
	}
	return 0
}

func clampDispatchCount(n int) int {
	if n < 1 {
		return 1
	}
	if n > 20 {
		return 20
	}
	return n
}

func (w *workbench) resolveDispatchModel(hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" || strings.EqualFold(hint, "default") || strings.EqualFold(hint, "inherit") || hint == "默认" {
		return ""
	}
	for _, m := range w.models {
		if strings.EqualFold(m.Key(), hint) || strings.EqualFold(m.ID, hint) {
			return m.Key()
		}
	}
	lower := strings.ToLower(hint)
	for _, m := range w.models {
		haystack := strings.ToLower(m.Key() + " " + m.Name)
		if strings.Contains(haystack, lower) {
			return m.Key()
		}
	}
	return hint
}

func dispatchModelShortLabel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "跟随主会话"
	}
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx+1 < len(model) {
		return model[idx+1:]
	}
	return model
}

// dispatchBlock draws the card as one quiet list: a single hairline
// container, one line per task (checkbox, title, model), and two actions.
// Worker count and isolation are low-frequency, so they only surface on
// hover or when they differ from the default.
func (w *workbench) dispatchBlock(c *ui.Context, b *markdown.CodeBlockView) {
	p := w.widgets.markdownPalette
	if b.Open {
		visualPending(c, p, w.tr3("正在整理编排方案…", "Preparing dispatch plan…", "編成プランを作成中…"))
		return
	}
	code := b.Code()
	spec := parseDispatchSpec(code)
	if len(spec.Items) == 0 {
		markdown.DefaultCodeBlock(c, b)
		return
	}
	key := askAnswerKey(w.selected, w.visuals.renderingMessage, "dispatch\x1f"+code)
	if rec, ok := w.askRecord(key); ok {
		w.dispatchDecided(c, p, rec)
		return
	}

	card := ui.Column(c.Key("dispatch-card:"+key)).FillWidth().Radius(12).Background(p.raised).Border(1, p.line)
	state := *ui.Local(card, dispatchCardKey{}, func() *dispatchUI {
		return &dispatchUI{
			selected:   map[int]bool{},
			models:     map[int]string{},
			counts:     map[int]int{},
			isolations: map[int]string{},
			pickingRow: -1,
		}
	})
	if !state.initialized {
		state.initialized = true
		for i, item := range spec.Items {
			state.selected[i] = item.Selected
			state.models[i] = w.resolveDispatchModel(item.Model)
			state.counts[i] = clampDispatchCount(item.Count)
			state.isolations[i] = normalizeDispatchIsolation(item.Isolation)
		}
	}

	selectedCount, totalWorkers := 0, 0
	for i := range spec.Items {
		if state.selected[i] {
			selectedCount++
			totalWorkers += clampDispatchCount(state.counts[i])
		}
	}

	card.Children(func() {
		ui.Row(c).FillWidth().Padding(11, 14).Gap(10).AlignItems(ui.Center).Children(func() {
			title := spec.Title
			if title == "" {
				title = w.tr3("编排计划", "Dispatch plan", "編成プラン")
			}
			ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
			summary := fmt.Sprintf(w.tr3("%d 项 · %d 路", "%d tasks · %d workers", "%d 件 · %d 並列"), selectedCount, totalWorkers)
			ui.Text(c, summary).FontSize(11).TextColor(p.faint).NoWrap().Shrink(0)
		})
		for i, item := range spec.Items {
			w.dispatchItemRow(c.Key(fmt.Sprintf("drow:%d", i)), p, state, item, i)
		}
		w.dispatchFooter(c, p, state, spec, key, selectedCount, totalWorkers)
	})
}

// dispatchCheck is the small square checkbox used by each task line.
func dispatchCheck(c *ui.Context, p palette, checked bool) {
	box := ui.Row(c).Size(16, 16).Radius(4).Justify(ui.Center).AlignItems(ui.Center).Shrink(0)
	if checked {
		box.Background(p.accent)
	} else {
		box.Border(1.5, p.faint.Alpha(0.7))
	}
	box.Children(func() {
		if checked {
			ui.Icon(c, checkIcon).FontSize(11).TextColor(ui.Hex("#FFFFFF"))
		}
	})
}

// dispatchGhost is a borderless inline control that only tints on hover.
func dispatchGhost(c *ui.Context, p palette, key, label string, active bool) ui.Element {
	btn := ui.Button(c.Key(key), "").Label(label).Height(24).Padding(0, 6).Radius(6).Border(0, p.line).Background(ui.Transparent)
	if active {
		btn.Background(p.soft)
	} else if btn.Hovered() {
		btn.Background(p.rowHover)
	}
	return btn
}

func (w *workbench) dispatchItemRow(c *ui.Context, p palette, state *dispatchUI, item dispatchItem, idx int) {
	checked := state.selected[idx]
	count := clampDispatchCount(state.counts[idx])
	iso := normalizeDispatchIsolation(state.isolations[idx])
	model := state.models[idx]
	picking := state.pickingRow == idx

	row := ui.Column(c).FillWidth().BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
	hovered := row.Hovered() || picking
	if hovered {
		row.Background(p.rowHover.Alpha(0.5))
	}
	row.Children(func() {
		ui.Row(c).FillWidth().Padding(7, 10, 7, 8).Gap(6).AlignItems(ui.Center).Children(func() {
			toggle := ui.Button(c.Key("toggle"), "").Label(item.Title).Padding(3, 6).Radius(6).
				Border(0, p.line).Background(ui.Transparent).Justify(ui.Start).Grow(1).MinWidth(0)
			toggle.Children(func() {
				ui.Row(c).Gap(10).AlignItems(ui.Center).Grow(1).MinWidth(0).Children(func() {
					dispatchCheck(c, p, checked)
					ui.Column(c).Gap(1).Grow(1).MinWidth(0).Children(func() {
						ink := p.ink
						if !checked {
							ink = p.muted
						}
						ui.Text(c, item.Title).FontSize(13).FontWeight(500).TextColor(ink).SingleLine().Ellipsis("…")
						if item.Note != "" {
							ui.Text(c, item.Note).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…")
						}
					})
				})
			})
			if toggle.Clicked() {
				state.selected[idx] = !checked
			}

			ui.Row(c).Gap(2).AlignItems(ui.Center).Shrink(0).Children(func() {
				// Worker count: hidden at ×1, a faint ×N otherwise; −/+ only on hover.
				if hovered && checked {
					dec := dispatchGhost(c, p, "dec", w.tr3("减少并行路数", "Fewer workers", "並列数を減らす"), false).Size(22, 24).Padding(0).Justify(ui.Center)
					dec.Children(func() { ui.Text(c, "−").FontSize(12).TextColor(p.muted) })
					dec.Disabled(count <= 1)
					if dec.Clicked() && count > 1 {
						state.counts[idx] = count - 1
					}
				}
				if count > 1 || (hovered && checked) {
					tone := p.muted
					if count > 1 {
						tone = p.ink
					}
					ui.Text(c, fmt.Sprintf("×%d", count)).FontSize(11).FontWeight(600).TextColor(tone).Padding(0, 2).NoWrap()
				}
				if hovered && checked {
					inc := dispatchGhost(c, p, "inc", w.tr3("增加并行路数", "More workers", "並列数を増やす"), false).Size(22, 24).Padding(0).Justify(ui.Center)
					inc.Children(func() { ui.Text(c, "+").FontSize(12).TextColor(p.muted) })
					inc.Disabled(count >= 20)
					if inc.Clicked() && count < 20 {
						state.counts[idx] = count + 1
					}
				}

				// Isolation: a single fork glyph, lit for worktree, faint on hover for shared.
				if iso == "worktree" || hovered {
					tip := w.tr3("共享当前目录 · 点击改为独立 worktree", "Shared checkout · click for a separate worktree", "共有チェックアウト · クリックで worktree")
					tone := p.faint
					if iso == "worktree" {
						tip = w.tr3("独立 worktree · 点击改为共享目录", "Separate worktree · click to share the checkout", "worktree · クリックで共有")
						tone = p.accentInk
					}
					isoBtn := dispatchGhost(c, p, "iso", tip, false).Size(24, 24).Padding(0).Justify(ui.Center).Tooltip(tip)
					isoBtn.Children(func() { ui.Icon(c, forkIcon).FontSize(12).TextColor(tone) })
					if isoBtn.Clicked() {
						if iso == "worktree" {
							state.isolations[idx] = "shared"
						} else {
							state.isolations[idx] = "worktree"
						}
					}
				}

				modelLabel := dispatchModelShortLabel(model)
				modelBtn := dispatchGhost(c, p, "model", "选择模型: "+modelLabel, picking).Padding(0, 6, 0, 8)
				if model != "" {
					modelBtn.Tooltip(model)
				}
				modelBtn.Children(func() {
					ui.Row(c).Gap(3).AlignItems(ui.Center).Children(func() {
						ink := p.ink
						if model == "" {
							ink = p.muted
						}
						ui.Text(c, modelLabel).FontSize(12).TextColor(ink).SingleLine().Ellipsis("…").MaxWidth(200)
						ui.Icon(c, chevronIcon).FontSize(11).TextColor(p.faint)
					})
				})
				if modelBtn.Clicked() {
					if picking {
						state.pickingRow = -1
					} else {
						state.pickingRow = idx
					}
					state.modelQuery = ""
				}
			})
		})
		if picking {
			w.dispatchModelDrawer(c.Key("picker"), p, state, idx)
		}
	})
}

// dispatchModelDrawer is a search box over a compact single-column list,
// matching the composer's model picker rather than a wall of pills.
func (w *workbench) dispatchModelDrawer(c *ui.Context, p palette, state *dispatchUI, idx int) {
	ui.Column(c).FillWidth().Padding(0, 12, 10, 40).Gap(4).Children(func() {
		kitField(ui.TextInput(c.Key("model-filter"), &state.modelQuery).
			Label(w.tr3("筛选模型", "Filter models", "モデルを絞り込む")).
			Placeholder(w.tr3("搜索模型…", "Search models…", "モデルを検索…")).
			FillWidth().Height(28), p, false)

		options := w.modelOptions(state.modelQuery)
		const maxShown = 6
		shown := options
		if len(shown) > maxShown {
			shown = shown[:maxShown]
		}
		current := state.models[idx]
		ui.Column(c).FillWidth().Gap(1).Children(func() {
			for i, opt := range shown {
				label := dispatchModelShortLabel(opt)
				active := opt == current
				btn := ui.Button(c.Key(fmt.Sprintf("mopt:%d", i)), "").Label(label).FillWidth().Height(26).Padding(0, 8).Radius(6).
					Border(0, p.line).Background(ui.Transparent).Justify(ui.Start)
				if btn.Hovered() {
					btn.Background(p.rowHover)
				}
				if opt != "" {
					btn.Tooltip(opt)
				}
				btn.Children(func() {
					ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, label).FontSize(12).TextColor(p.ink).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
						if opt != "" && opt != label {
							ui.Text(c, strings.TrimSuffix(opt, "/"+label)).FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…").MaxWidth(180)
						}
						if active {
							ui.Icon(c, checkIcon).FontSize(12).TextColor(p.accentInk).Shrink(0)
						}
					})
				})
				if btn.Clicked() {
					state.models[idx] = opt
					state.pickingRow = -1
					state.modelQuery = ""
				}
			}
			if len(options) > maxShown {
				ui.Text(c, fmt.Sprintf(w.tr3("另有 %d 个，输入关键字筛选", "%d more · type to filter", "他 %d 件 · 入力で絞り込み"), len(options)-maxShown)).
					FontSize(11).TextColor(p.faint).Padding(4, 8, 0)
			}
		})
	})
}

func (w *workbench) dispatchFooter(c *ui.Context, p palette, state *dispatchUI, spec dispatchSpec, key string, selectedCount, totalWorkers int) {
	ui.Row(c).FillWidth().Gap(4).AlignItems(ui.Center).Padding(10, 14).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
		confirmLabel := fmt.Sprintf(w.tr3("开始编排 · %d 路", "Dispatch · %d", "開始 · %d 並列"), totalWorkers)
		confirm := kitButton(c, p, "dispatch-confirm", confirmLabel, controlPrimary, controlSM)
		if selectedCount == 0 {
			confirm.Disabled(true)
		}
		if confirm.Clicked() && selectedCount > 0 {
			reply := w.formatDispatchConfirm(spec, state)
			if w.deliverAsk(w.selected, reply) {
				w.rememberAsk(key, askAnswerRecord{Answer: w.formatDispatchSummary(spec, state)})
			}
		}

		direct := kitButton(c, p, "dispatch-direct", w.tr3("直接做", "Do it here", "直接実行"), controlGhost, controlSM)
		direct.Tooltip(w.tr3("不拆分子代理，主会话直接完成", "Skip subagents and finish in this thread", "分割せずこのスレッドで実行"))
		if direct.Clicked() {
			reply := w.tr3("不用拆分派发子代理，主会话直接完成即可。", "Do not delegate to subagents; handle this directly in the main thread.", "サブエージェントに分割せず、メインスレッドで直接完了してください。")
			if w.deliverAsk(w.selected, reply) {
				w.rememberAsk(key, askAnswerRecord{Answer: w.tr3("不拆分，主会话直接执行", "Direct execution", "分割せず直接実行")})
			}
		}
	})
}

func (w *workbench) formatDispatchConfirm(spec dispatchSpec, state *dispatchUI) string {
	var lines []string
	header := "确认编排计划，请用单次 codemode 并发派发以下子任务（Promise.allSettled）："
	if spec.Title != "" {
		header = fmt.Sprintf("确认编排计划「%s」，请用单次 codemode 并发派发以下子任务（Promise.allSettled）：", spec.Title)
	}
	lines = append(lines, header)
	idx := 0
	for i, item := range spec.Items {
		if !state.selected[i] {
			continue
		}
		idx++
		count := clampDispatchCount(state.counts[i])
		iso := normalizeDispatchIsolation(state.isolations[i])
		model := strings.TrimSpace(state.models[i])
		if model == "" {
			model = "继承主会话模型"
		}
		rolePart := ""
		if item.Role != "" {
			rolePart = " | role=" + item.Role
		}
		countPart := ""
		if count > 1 {
			countPart = fmt.Sprintf(" | 并行 %d 路", count)
		}
		notePart := ""
		if item.Note != "" {
			notePart = " — " + item.Note
		}
		lines = append(lines, fmt.Sprintf("%d. %s%s | model=%s | isolation=%s%s%s", idx, item.Title, rolePart, model, iso, countPart, notePart))
	}
	return strings.Join(lines, "\n")
}

func (w *workbench) formatDispatchSummary(spec dispatchSpec, state *dispatchUI) string {
	var parts []string
	total := 0
	for i, item := range spec.Items {
		if !state.selected[i] {
			continue
		}
		count := clampDispatchCount(state.counts[i])
		total += count
		label := item.Title + "(" + dispatchModelShortLabel(state.models[i])
		if count > 1 {
			label += fmt.Sprintf(" ×%d", count)
		}
		label += ")"
		parts = append(parts, label)
	}
	return fmt.Sprintf("已派发 %d 路：%s", total, strings.Join(parts, "、"))
}

func (w *workbench) dispatchDecided(c *ui.Context, p palette, rec askAnswerRecord) {
	text := w.tr3("已跳过编排", "Dispatch skipped", "編成をスキップ済み")
	tone := p.muted
	if !rec.Skipped && rec.Answer != "" {
		text = rec.Answer
		tone = p.accentInk
	}
	ui.Row(c).FillWidth().Padding(10, 12).Gap(8).Radius(12).AlignItems(ui.Center).Background(p.raised).Border(1, p.line).Children(func() {
		ui.Icon(c, checkIcon).FontSize(14).TextColor(tone).Shrink(0)
		ui.Text(c, text).FontSize(13).TextColor(p.ink).Wrap().Grow(1)
	})
}
