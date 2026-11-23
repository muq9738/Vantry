package desktop

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
)

// panelNode is one block of a plugin's structured "vantryPanel" answer:
// text, markdown, badge, kv, list, code, or a card that groups children.
// Unknown types degrade to their text so a newer plugin stays readable.
type panelNode struct {
	Type     string
	Title    string
	Text     string
	Tone     string
	Language string
	Pairs    [][2]string
	Items    []string
	Children []panelNode
}

type vantryPanel struct {
	Title, Subtitle string
	Nodes           []panelNode
}

// decodeVantryPanel accepts {"vantryPanel": {...}} or {"vantryPanel": [...]}
// in a plugin's output. Anything else is shown as plain output.
func decodeVantryPanel(output string) (*vantryPanel, bool) {
	trimmed := strings.TrimSpace(output)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(trimmed), &envelope) != nil {
		return nil, false
	}
	raw, ok := envelope["vantryPanel"]
	if !ok {
		return nil, false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	panel := &vantryPanel{}
	switch v := value.(type) {
	case []any:
		panel.Nodes = decodeNodes(v)
	case map[string]any:
		panel.Title, panel.Subtitle = stringField(v, "title"), stringField(v, "subtitle", "description")
		for _, key := range []string{"widgets", "nodes", "children", "blocks", "items"} {
			if list, ok := v[key].([]any); ok {
				panel.Nodes = decodeNodes(list)
				break
			}
		}
	default:
		return nil, false
	}
	return panel, true
}

func stringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := m[key].(type) {
		case string:
			if value != "" {
				return value
			}
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(value)
		}
	}
	return ""
}

func scalarText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case map[string]any:
		if text := stringField(v, "text", "value", "label", "title"); text != "" {
			return text
		}
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func decodeNodes(list []any) []panelNode {
	nodes := make([]panelNode, 0, len(list))
	for _, item := range list {
		switch v := item.(type) {
		case string:
			nodes = append(nodes, panelNode{Type: "text", Text: v})
		case map[string]any:
			nodes = append(nodes, decodeNode(v))
		}
	}
	return nodes
}

func decodeNode(m map[string]any) panelNode {
	node := panelNode{
		Type:     strings.ToLower(stringField(m, "type", "kind")),
		Title:    stringField(m, "title", "label"),
		Tone:     strings.ToLower(stringField(m, "tone", "variant", "status")),
		Language: stringField(m, "language", "lang"),
	}
	switch node.Type {
	case "markdown", "md":
		node.Type, node.Text = "markdown", stringField(m, "markdown", "text", "value", "content")
	case "code":
		node.Text = stringField(m, "code", "text", "value", "content")
	case "kv", "keyvalue", "table":
		node.Type = "kv"
		switch items := firstPresent(m, "entries", "items", "pairs", "rows", "value").(type) {
		case []any:
			for _, item := range items {
				if pair, ok := item.(map[string]any); ok {
					node.Pairs = append(node.Pairs, [2]string{stringField(pair, "key", "label", "name"), scalarText(firstPresent(pair, "value", "text"))})
				} else if tuple, ok := item.([]any); ok && len(tuple) >= 2 {
					node.Pairs = append(node.Pairs, [2]string{scalarText(tuple[0]), scalarText(tuple[1])})
				}
			}
		case map[string]any:
			keys := make([]string, 0, len(items))
			for key := range items {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				node.Pairs = append(node.Pairs, [2]string{key, scalarText(items[key])})
			}
		}
	case "list", "bullets":
		node.Type = "list"
		if items, ok := firstPresent(m, "items", "value").([]any); ok {
			for _, item := range items {
				node.Items = append(node.Items, scalarText(item))
			}
		}
		if children, ok := m["children"].([]any); ok {
			node.Type = "card"
			node.Children = decodeNodes(children)
		}
	case "card", "group", "section":
		node.Type = "card"
		if children, ok := firstPresent(m, "children", "nodes", "items").([]any); ok {
			node.Children = decodeNodes(children)
		}
		node.Text = stringField(m, "text", "description")
	case "badge", "tag":
		node.Type, node.Text = "badge", stringField(m, "text", "label", "value")
	default:
		if node.Type == "" {
			node.Type = "text"
		}
		node.Text = stringField(m, "text", "value", "content", "markdown")
	}
	return node
}

func firstPresent(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			return value
		}
	}
	return nil
}

func toneColor(p palette, tone string) ui.Color {
	switch tone {
	case "success", "ok", "green", "passed":
		return p.success
	case "warning", "warn", "amber", "pending":
		return p.warning
	case "danger", "error", "red", "failed":
		return p.danger
	case "info", "running", "blue", "accent":
		return p.running
	}
	return p.muted
}

// panelText flattens a structured panel to plain text for the draft.
func panelText(panel *vantryPanel) string {
	var b strings.Builder
	if panel.Title != "" {
		b.WriteString(panel.Title + "\n")
	}
	var walk func([]panelNode, string)
	walk = func(nodes []panelNode, indent string) {
		for _, n := range nodes {
			if n.Title != "" && n.Type != "badge" {
				b.WriteString(indent + n.Title + "\n")
			}
			switch n.Type {
			case "kv":
				for _, pair := range n.Pairs {
					b.WriteString(indent + pair[0] + "：" + pair[1] + "\n")
				}
			case "list":
				for _, item := range n.Items {
					b.WriteString(indent + "- " + item + "\n")
				}
			case "card":
				if n.Text != "" {
					b.WriteString(indent + n.Text + "\n")
				}
				walk(n.Children, indent+"  ")
			default:
				if n.Text != "" {
					b.WriteString(indent + n.Text + "\n")
				}
			}
		}
	}
	walk(panel.Nodes, "")
	return strings.TrimSpace(b.String())
}

func (w *workbench) vantryPanelView(c *ui.Context, p palette, panel *vantryPanel) {
	ui.Column(c.Key("vantry-panel")).FillWidth().Gap(8).Padding(10, 12).Radius(10).Background(p.raised).Border(1, p.lineSoft).Children(func() {
		if panel.Title != "" || panel.Subtitle != "" {
			ui.Column(c).Gap(3).Children(func() {
				if panel.Title != "" {
					ui.Text(c, panel.Title).FontSize(13).Bold().TextColor(p.ink).Wrap()
				}
				if panel.Subtitle != "" {
					ui.Text(c, panel.Subtitle).FontSize(11).TextColor(p.muted).Wrap()
				}
			})
		}
		w.panelNodes(c, p, panel.Nodes, 0)
	})
}

func (w *workbench) panelNodes(c *ui.Context, p palette, nodes []panelNode, depth int) {
	// Consecutive badges share one wrapping row, as a tag strip.
	for i := 0; i < len(nodes); i++ {
		if nodes[i].Type == "badge" {
			j := i
			for j < len(nodes) && nodes[j].Type == "badge" {
				j++
			}
			badges := nodes[i:j]
			ui.Row(c.Key(fmt.Sprintf("badges:%d:%d", depth, i))).Gap(6).Wrap().Children(func() {
				for _, b := range badges {
					text := b.Text
					if b.Title != "" && b.Text != "" {
						text = b.Title + " · " + b.Text
					} else if text == "" {
						text = b.Title
					}
					pill(c, p, text, toneColor(p, b.Tone))
				}
			})
			i = j - 1
			continue
		}
		w.panelNode(c.Key(fmt.Sprintf("node:%d:%d", depth, i)), p, nodes[i], depth)
	}
}

func (w *workbench) panelNode(c *ui.Context, p palette, node panelNode, depth int) {
	ui.Column(c).FillWidth().Gap(5).Children(func() {
		if node.Title != "" && node.Type != "card" {
			ui.Text(c, node.Title).FontSize(11).Bold().TextColor(p.muted)
		}
		switch node.Type {
		case "markdown":
			markdown.View(c, node.Text, w.replyMarkdown(w.selected))
		case "kv":
			ui.Column(c).FillWidth().Radius(8).Border(1, p.lineSoft).Children(func() {
				for i, pair := range node.Pairs {
					row := ui.Row(c.Key("kv:"+strconv.Itoa(i))).Gap(10).Padding(5, 9).AlignItems(ui.Start)
					if i > 0 {
						row.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
					}
					row.Children(func() {
						ui.Text(c, pair[0]).FontSize(11).TextColor(p.muted).Width(120).Shrink(0).Wrap()
						ui.Text(c, pair[1]).FontSize(11).TextColor(p.ink).Grow(1).MinWidth(0).Wrap().Selectable()
					})
				}
			})
		case "list":
			ui.Column(c).Gap(3).Children(func() {
				for _, item := range node.Items {
					ui.Row(c).Gap(7).AlignItems(ui.Start).Children(func() {
						ui.Box(c).Size(4, 4).Radius(2).Background(p.faint).Margin(7, 0, 0, 2).Shrink(0)
						ui.Text(c, item).FontSize(12).TextColor(p.ink).Wrap().Grow(1).MinWidth(0).Selectable()
					})
				}
			})
		case "code":
			ui.Column(c).FillWidth().Radius(8).Background(p.code).Border(1, p.lineSoft).Children(func() {
				ui.Row(c).Height(24).Padding(0, 4, 0, 9).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
					ui.Text(c, nonEmptyString(node.Language, "code")).Font("Menlo").FontSize(10).TextColor(p.faint).Grow(1)
					if iconButton(c, p, "copy-code", "复制代码", copyIcon).Size(20, 20).Padding(3).Clicked() {
						c.WriteClipboard(node.Text)
					}
				})
				lines := strings.Count(node.Text, "\n") + 1
				ui.Scroll(c).FillWidth().Height(min(float32(lines)*17+16, 220)).Children(func() {
					ui.Text(c, node.Text).Font("Menlo").FontSize(11).LineHeight(1.5).TextColor(p.ink).Selectable().Padding(7, 10)
				})
			})
		case "card":
			ui.Column(c).FillWidth().Gap(6).Padding(8, 10).Radius(8).Background(p.soft).Border(1, p.lineSoft).Children(func() {
				if node.Title != "" {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						if node.Tone != "" {
							ui.Box(c).Size(6, 6).Radius(3).Background(toneColor(p, node.Tone)).Shrink(0)
						}
						ui.Text(c, node.Title).FontSize(12).Bold().TextColor(p.ink).Wrap()
					})
				}
				if node.Text != "" {
					ui.Text(c, node.Text).FontSize(11).TextColor(p.muted).Wrap()
				}
				if depth < 4 {
					w.panelNodes(c, p, node.Children, depth+1)
				}
			})
		default:
			color := p.ink
			if node.Tone != "" {
				color = toneColor(p, node.Tone)
			}
			ui.Text(c, node.Text).FontSize(12).TextColor(color).Wrap().Selectable()
		}
	})
}

// pluginOutputView renders a plugin's answer: a structured panel when the
// plugin sends one, else its text in a quiet code well.
func (w *workbench) pluginOutputView(c *ui.Context, p palette, output string) {
	panel, structured := decodeVantryPanel(output)
	draftText := output
	if structured {
		draftText = panelText(panel)
	}
	// Actions lead the result so they stay reachable above a long answer.
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "运行结果").FontSize(11).Bold().TextColor(p.muted).Grow(1)
		if quietButton(c, p, "plugin-to-draft", "将结果加入草稿").Height(24).Clicked() {
			comp := w.composerFor(w.selected)
			w.setDraft(w.selected, strings.TrimSpace(comp.draft+"\n"+draftText))
		}
		if quietButton(c, p, "plugin-copy", "复制结果").Height(24).Clicked() {
			c.WriteClipboard(draftText)
		}
	})
	if structured {
		w.vantryPanelView(c, p, panel)
		return
	}
	ui.Column(c).FillWidth().Radius(8).Background(p.code).Border(1, p.lineSoft).Padding(8, 10).Children(func() {
		ui.Text(c, output).FontSize(12).TextColor(p.ink).Selectable().Wrap()
	})
}
