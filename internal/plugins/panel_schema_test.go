package plugins

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParsePanelOutputValidAllWidgetKindsAndTree(t *testing.T) {
	payload := `{
		"vantryPanel": {
			"version": 1,
			"title": "构建检查摘要",
			"widgets": [
				{
					"kind": "badge",
					"key": "status-badge",
					"label": "PASSED",
					"tone": "success"
				},
				{
					"kind": "text",
					"title": "概览",
					"text": "全部 12 项检查通过",
					"tone": "info"
				},
				{
					"kind": "markdown",
					"key": "summary-md",
					"markdown": "## 详情\n- 无警告"
				},
				{
					"kind": "kv",
					"title": "环境信息",
					"entries": [
						{"key": "分支", "value": "main"},
						{"key": "耗时", "value": "1.2s"}
					]
				},
				{
					"kind": "list",
					"title": "检查项",
					"items": ["lint", "unit-test"]
				},
				{
					"kind": "list",
					"key": "nested-group",
					"title": "产物详情",
					"children": [
						{
							"kind": "code",
							"title": "命令",
							"language": "sh",
							"code": "go test -race ./..."
						},
						{
							"kind": "list",
							"items": [
								{"kind": "text", "text": "子级说明"}
							]
						}
					]
				}
			]
		}
	}`

	view, ok := ParsePanelOutput(payload)
	if !ok || view == nil {
		err := func() error { _, e := ValidatePanelOutput(payload); return e }()
		t.Fatalf("expected valid panel, got ok=%v err=%v", ok, err)
	}
	if view.Version != 1 || view.Title != "构建检查摘要" || len(view.Widgets) != 6 {
		t.Fatalf("unexpected root view: %+v", view)
	}
	if view.Widgets[0].Kind != WidgetBadge || view.Widgets[0].Key != "status-badge" || view.Widgets[0].Text != "PASSED" || view.Widgets[0].Tone != "success" {
		t.Fatalf("unexpected badge widget: %+v", view.Widgets[0])
	}
	// Auto-assigned stable fallback key when omitted.
	if view.Widgets[1].Key != "w-1" || view.Widgets[1].Kind != WidgetText {
		t.Fatalf("unexpected text widget: %+v", view.Widgets[1])
	}
	if view.Widgets[2].Kind != WidgetMarkdown || !strings.Contains(view.Widgets[2].Text, "## 详情") {
		t.Fatalf("unexpected markdown widget: %+v", view.Widgets[2])
	}
	if view.Widgets[3].Kind != WidgetKV || len(view.Widgets[3].Entries) != 2 || view.Widgets[3].Entries[0].Key != "分支" {
		t.Fatalf("unexpected kv widget: %+v", view.Widgets[3])
	}
	if view.Widgets[4].Kind != WidgetList || len(view.Widgets[4].Items) != 2 || view.Widgets[4].Items[1] != "unit-test" {
		t.Fatalf("unexpected string list widget: %+v", view.Widgets[4])
	}
	nested := view.Widgets[5]
	if nested.Kind != WidgetList || len(nested.Children) != 2 {
		t.Fatalf("unexpected nested list widget: %+v", nested)
	}
	if nested.Children[0].Kind != WidgetCode || nested.Children[0].Language != "sh" || nested.Children[0].Text != "go test -race ./..." {
		t.Fatalf("unexpected code child: %+v", nested.Children[0])
	}
	if len(nested.Children[1].Children) != 1 || nested.Children[1].Children[0].Text != "子级说明" {
		t.Fatalf("unexpected grandchildren: %+v", nested.Children[1])
	}
}

func TestParsePanelOutputPlainTextAndInvalidSchemasFallbackToFalse(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"empty", ""},
		{"plain text", "整理完成：共 3 个段落"},
		{"ordinary json object", `{"draft":{"title":"记录"},"stats":{"paragraphs":2}}`},
		{"json array", `[{"vantryPanel":{"version":1}}]`},
		{"null panel", `{"vantryPanel":null}`},
		{"extra root field", `{"vantryPanel":{"version":1,"widgets":[{"kind":"text","text":"ok"}]},"extra":1}`},
		{"trailing garbage", `{"vantryPanel":{"version":1,"widgets":[{"kind":"text","text":"ok"}]}} trailing`},
		{"duplicate json key", `{"vantryPanel":{"version":1,"version":2,"widgets":[{"kind":"text","text":"ok"}]}}`},
		{"missing version", `{"vantryPanel":{"widgets":[{"kind":"text","text":"ok"}]}}`},
		{"unsupported version", `{"vantryPanel":{"version":2,"widgets":[{"kind":"text","text":"ok"}]}}`},
		{"empty widgets", `{"vantryPanel":{"version":1,"widgets":[]}}`},
		{"unknown kind", `{"vantryPanel":{"version":1,"widgets":[{"kind":"webview","url":"https://evil.example"}]}}`},
		{"unknown widget prop", `{"vantryPanel":{"version":1,"widgets":[{"kind":"text","text":"hi","onClick":"run()"}]}}`},
		{"cross widget prop", `{"vantryPanel":{"version":1,"widgets":[{"kind":"text","text":"hi","language":"go"}]}}`},
		{"null text", `{"vantryPanel":{"version":1,"widgets":[{"kind":"text","text":null}]}}`},
		{"empty text", `{"vantryPanel":{"version":1,"widgets":[{"kind":"text","text":""}]}}`},
		{"numeric text", `{"vantryPanel":{"version":1,"widgets":[{"kind":"text","text":42}]}}`},
		{"blank badge", `{"vantryPanel":{"version":1,"widgets":[{"kind":"badge","label":"   "}]}}`},
		{"invalid tone", `{"vantryPanel":{"version":1,"widgets":[{"kind":"badge","label":"OK","tone":"neon"}]}}`},
		{"both markdown and text", `{"vantryPanel":{"version":1,"widgets":[{"kind":"markdown","markdown":"a","text":"b"}]}}`},
		{"empty kv", `{"vantryPanel":{"version":1,"widgets":[{"kind":"kv","entries":[]}]}}`},
		{"kv non-string value", `{"vantryPanel":{"version":1,"widgets":[{"kind":"kv","entries":[{"key":"k","value":123}]}]}}`},
		{"list mixed types", `{"vantryPanel":{"version":1,"widgets":[{"kind":"list","items":["str",{"kind":"text","text":"obj"}]}]}}`},
		{"list missing items", `{"vantryPanel":{"version":1,"widgets":[{"kind":"list","title":"only title"}]}}`},
		{"code bad language", `{"vantryPanel":{"version":1,"widgets":[{"kind":"code","language":"go\nrm","code":"x"}]}}`},
		{"blank explicit key", `{"vantryPanel":{"version":1,"widgets":[{"kind":"text","key":"  ","text":"hi"}]}}`},
		{"duplicate explicit key", `{"vantryPanel":{"version":1,"widgets":[{"kind":"text","key":"same","text":"1"},{"kind":"text","key":"same","text":"2"}]}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if view, ok := ParsePanelOutput(tc.payload); ok || view != nil {
				t.Fatalf("expected rejection for %s, got %+v", tc.name, view)
			}
		})
	}
}

func TestPanelDepthAndNodeCountBounds(t *testing.T) {
	buildNested := func(depth int) string {
		node := `{"kind":"text","text":"leaf"}`
		for d := depth - 1; d >= 1; d-- {
			node = fmt.Sprintf(`{"kind":"list","children":[%s]}`, node)
		}
		return fmt.Sprintf(`{"vantryPanel":{"version":1,"widgets":[%s]}}`, node)
	}

	if view, ok := ParsePanelOutput(buildNested(MaxPanelDepth)); !ok || view == nil {
		t.Fatalf("expected MaxPanelDepth (%d) to pass", MaxPanelDepth)
	}
	if view, ok := ParsePanelOutput(buildNested(MaxPanelDepth + 1)); ok || view != nil {
		t.Fatalf("expected depth %d to be rejected", MaxPanelDepth+1)
	}

	makeNodes := func(count int) string {
		widgets := make([]string, count)
		for i := 0; i < count; i++ {
			b, _ := json.Marshal(map[string]string{
				"kind": "text",
				"key":  fmt.Sprintf("k-%d", i),
				"text": "ok",
			})
			widgets[i] = string(b)
		}
		return fmt.Sprintf(`{"vantryPanel":{"version":1,"widgets":[%s]}}`, strings.Join(widgets, ","))
	}

	if view, ok := ParsePanelOutput(makeNodes(MaxPanelNodes)); !ok || len(view.Widgets) != MaxPanelNodes {
		t.Fatalf("expected MaxPanelNodes (%d) to pass", MaxPanelNodes)
	}
	if view, ok := ParsePanelOutput(makeNodes(MaxPanelNodes + 1)); ok || view != nil {
		t.Fatalf("expected %d nodes to be rejected", MaxPanelNodes+1)
	}
}
