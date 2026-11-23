package desktop

import (
	"encoding/json"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func measureFixed(text string, width float32) (float32, float32) {
	lines := strings.Split(text, "\n")
	w := float32(0)
	for _, line := range lines {
		w = max(w, float32(len([]rune(line)))*7)
	}
	return min(w, width), float32(len(lines)) * 15
}

func TestChartSpecReadsNullAsMissingAndRejectsBadData(t *testing.T) {
	spec, err := parseChartSpec(`{"type":"line","labels":["a","b","c"],"series":[{"name":"x","data":[1,null,3]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if data := spec.Series[0].Data; len(data) != 3 || !math.IsNaN(data[1]) || data[2] != 3 {
		t.Fatalf("series = %v", data)
	}
	short, _ := parseChartSpec(`{"labels":["a","b"],"values":[5,null]}`)
	if short.Type != "bar" || len(short.Series[0].Data) != 2 || !math.IsNaN(short.Series[0].Data[1]) {
		t.Fatalf("defaults and missing values: %+v", short)
	}
	for _, bad := range []string{`{`, `{"type":"radar","labels":["a"],"values":[1]}`, `{"type":"bar"}`, `{"type":"stats"}`, `{"labels":["a"],"values":[1,2]}`, `{"labels":["a"],"values":[null]}`, `{"labels":["a"],"values":[1]} {}`, `{"type":"pie","labels":["a"],"values":[-1]}`} {
		if _, err := parseChartSpec(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if got := formatChartValue(123456, "元"); got != "12.35万元" {
		t.Errorf("format = %q", got)
	}
}

func TestFlowchartParsesLinksShapesAndLaysOutWithoutOverlap(t *testing.T) {
	code := "flowchart LR\n  A[开始] --> B{检查?}\n  B -->|是| C([完成])\n  B -- 否 --> D[(数据库)]\n  D -.-> A\n  C & D ==> E((结束))\n  classDef x fill:#f00\n"
	flow, err := parseFlowchart(code)
	if err != nil {
		t.Fatal(err)
	}
	if flow.dir != "LR" || len(flow.nodes) != 5 || len(flow.edges) != 6 {
		t.Fatalf("dir=%s nodes=%d edges=%d", flow.dir, len(flow.nodes), len(flow.edges))
	}
	b := flow.nodes[flow.index["B"]]
	if b.label != "检查?" || b.shape != "diamond" {
		t.Fatalf("node B = %+v", b)
	}
	labels := map[string]bool{}
	styles := map[byte]int{}
	for _, e := range flow.edges {
		labels[e.label] = true
		styles[e.style]++
	}
	if !labels["是"] || !labels["否"] || styles['.'] != 1 || styles['='] != 2 {
		t.Fatalf("edges %+v", flow.edges)
	}
	flow.layout(measureFixed)
	backs := 0
	for _, e := range flow.edges {
		if e.back {
			backs++
		}
	}
	if backs != 1 {
		t.Fatalf("cycle D→A should be one back edge, got %d", backs)
	}
	for i, a := range flow.nodes {
		if a.x-a.w/2 < 0 || a.y-a.h/2 < 0 || a.x+a.w/2 > flow.width || a.y+a.h/2 > flow.height {
			t.Errorf("node %s outside the diagram", a.id)
		}
		for _, b := range flow.nodes[i+1:] {
			if math.Abs(float64(a.x-b.x)) < float64(a.w+b.w)/2 && math.Abs(float64(a.y-b.y)) < float64(a.h+b.h)/2 {
				t.Errorf("nodes %s and %s overlap", a.id, b.id)
			}
		}
	}
	if !strings.Contains(flow.describe(), "开始 → 检查?") {
		t.Errorf("description = %s", flow.describe())
	}
}

func TestSequenceAndPieDiagrams(t *testing.T) {
	seq, err := parseSequence("sequenceDiagram\n  autonumber\n  participant U as 用户\n  U->>Core: message.send\n  loop 重试\n    Core-->>U: receipt\n  end\n  Note over U,Core: 幂等\n")
	if err != nil {
		t.Fatal(err)
	}
	seq.layout(measureFixed)
	if len(seq.participants) != 2 || seq.participants[0].label != "用户" || seq.width <= 0 || seq.height <= 0 {
		t.Fatalf("sequence %+v", seq.participants)
	}
	frames := 0
	for _, step := range seq.steps {
		if step.kind == "frame" && step.h > 0 {
			frames++
		}
	}
	if frames != 1 || !strings.Contains(seq.describe(), "用户 → Core：message.send") {
		t.Fatalf("frames=%d describe=%s", frames, seq.describe())
	}
	pie, err := parseMermaidPie("pie title 分布\n  \"Go\" : 70\n  \"TS\" : 30\n")
	if err != nil || pie.Title != "分布" || len(pie.Labels) != 2 {
		t.Fatalf("pie %+v %v", pie, err)
	}
	if d := parseMermaid("gantt\n  title x", measureFixed); d.err == nil {
		t.Fatal("unsupported diagram should fall back to source")
	}
}

func TestSplitTurnInterleavesTextAndWorkByOffset(t *testing.T) {
	output := "先看代码。然后修改。完成。"
	first := len("先看代码。")
	tools := []api.ToolCall{
		{ID: "late", OutputOffset: -1},
		{ID: "read", OutputOffset: first},
		{ID: "grep", OutputOffset: first},
		{ID: "edit", OutputOffset: first + 4}, // inside a rune: snaps back
	}
	parts := splitTurn(output, tools)
	var shape []string
	for _, part := range parts {
		if part.tools != nil {
			ids := []string{}
			for _, tool := range part.tools {
				ids = append(ids, tool.ID)
			}
			shape = append(shape, "["+strings.Join(ids, ",")+"]")
		} else {
			shape = append(shape, part.text)
		}
	}
	want := "先看代码。|[read,grep]|然|[edit]|后修改。完成。|[late]"
	if got := strings.Join(shape, "|"); got != want {
		t.Fatalf("parts = %s, want %s", got, want)
	}
}

func TestStreamingBufferAppendsAndRestartsAfterReplacement(t *testing.T) {
	w := newWorkbench(&fakeService{})
	out := ""
	for _, delta := range []string{"一", "二", "三"} {
		out = w.appendOutput("r", out, delta)
	}
	if out != "一二三" {
		t.Fatalf("out = %q", out)
	}
	replaced := w.appendOutput("r", "全新", "!")
	if replaced != "全新!" || out != "一二三" {
		t.Fatalf("replacement = %q, earlier = %q", replaced, out)
	}
	w.releaseOutput("r")
	if w.outputs["r"] != nil {
		t.Fatal("finished reply kept its builder")
	}
}

func TestVisualReplyRendersChartsDiagramsAndWorkNatively(t *testing.T) {
	reply := "结果如下：\n\n```chart\n{\"type\":\"bar\",\"title\":\"耗时\",\"unit\":\"ms\",\"labels\":[\"冷启动\",\"热启动\"],\"series\":[{\"name\":\"前\",\"data\":[120,40]},{\"name\":\"后\",\"data\":[80,20]}]}\n```\n\n```mermaid\nflowchart TD\n  A[请求] --> B[Core]\n```\n"
	r := run("r1", "t1", api.RunCompleted, "画个图", reply, 0)
	r.Reasoning = "先比较两组数据\n再画图"
	w, _ := newViewWorkbench(r)
	w.tools = []api.ToolCall{
		{ID: "a", ThreadID: "t1", RunID: "r1", Name: "read", Arguments: json.RawMessage(`{"path":"/w/a.go"}`), Status: "completed", OutputOffset: len("结果如下：\n\n")},
		{ID: "b", ThreadID: "t1", RunID: "r1", Name: "edit", Arguments: json.RawMessage(`{"path":"/w/a.go","oldText":"x := 1","newText":"x := 2\ny := 3"}`), Status: "completed", OutputOffset: len("结果如下：\n\n")},
	}
	tt := ui.NewTester(w.view, 1280, 1400)
	tt.Advance(time.Second)
	for _, name := range []string{"耗时", "冷启动 · 前：120ms", "放大"} {
		if _, ok := tt.Access(name); !ok {
			t.Errorf("chart part %q missing: %q", name, tt.Texts())
		}
	}
	if !tt.HasText("流程图") || !tt.HasText("思考过程") || !tt.HasText("读取 1 个文件，修改 1 个文件") || !tt.HasText("已修改 1 个文件") {
		t.Fatalf("timeline parts missing: %q", tt.Texts())
	}
	if !tt.HasText("读取 1 个文件，修改 1 个文件") {
		t.Errorf("work summary missing: %q", tt.Texts())
	}
	mustClick(t, tt, "放大查看")
	if !w.visuals.expanded || w.visuals.expandedLang != "mermaid" {
		t.Fatal("diagram did not expand")
	}
}

func TestSidebarListRanksAttentionThenActivity(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunRunning, "工作中", "", 0))
	idle := thread("t2", "空闲会话", time.Minute)
	w.mergeThread(idle)
	w.mergeRun(run("r2", "t2", api.RunCompleted, "空闲会话", "完成", time.Minute))
	asking := thread("thread-c", "等待审批", time.Hour)
	w.mergeThread(asking)
	w.mergeRun(run("r3", "thread-c", api.RunRunning, "等待审批", "", time.Hour))
	w.requests = []api.ProviderRequest{{ID: "q", ThreadID: "thread-c", RunID: "r3", Status: "pending", Title: "允许？"}}
	settled := thread("t4", "旧会话", 0)
	settled.Archived = true
	w.mergeThread(settled)
	w.mergeRun(run("r4", "t4", api.RunCompleted, "旧会话", "", 0))
	sections := map[string][]string{}
	order := []string{}
	for _, g := range w.navigationGroups() {
		sections[g.key] = g.ids
		order = append(order, g.key)
	}
	// One flat list: a running thread keeps its place by activity instead of
	// jumping into a section of its own, and only settled threads leave it.
	if len(order) != 2 || order[0] != sectionActive || order[1] != sectionSettled {
		t.Fatalf("section order %v", order)
	}
	if strings.Join(sections[sectionSettled], ",") != "t4" {
		t.Fatalf("sections %v", sections)
	}
	if active := sections[sectionActive]; len(active) != 3 || active[0] != "thread-c" {
		t.Fatalf("the thread that needs you must lead the list: %v", active)
	}
	tt := ui.NewTester(w.view, 1080, 700)
	if shelf, ok := tt.Access("已归档"); !ok || shelf.Expanded || !tt.HasText("1") || tt.HasText("旧会话") {
		t.Fatalf("settled shelf should start folded: %q", tt.Texts())
	}
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "等待审批"); !ok {
		t.Fatal("attention row missing")
	}
}

func TestReasoningEventsFoldBesideTheReply(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunRunning, "问题", "回答", 0))
	ev := deltaEvent(4, "t1", "r1", "想一想")
	ev.Type = eventRunReasoning
	ev.Data = json.RawMessage(`{"reasoningDelta":"想一想"}`)
	if !w.applyEvent(ev) {
		t.Fatal("reasoning event not folded")
	}
	if w.runs[0].Output != "回答" || w.runs[0].Reasoning != "想一想" {
		t.Fatalf("output=%q reasoning=%q", w.runs[0].Output, w.runs[0].Reasoning)
	}
	stale := run("r1", "t1", api.RunRunning, "问题", "回答", 0)
	w.applyEvent(statusEvent(5, stale))
	if w.runs[0].Reasoning != "想一想" {
		t.Fatal("an older run event erased folded reasoning")
	}
}

func TestFlowchartSubgraphsLinkStylesAndShapes(t *testing.T) {
	code := `flowchart LR
  direction LR
  subgraph Edge [边缘节点]
    direction TB
    subgraph Inner [内部]
      A[(数据库)] --> B{{网关}}
    end
    B -.-> C[缓存]
  end
  subgraph Core [核心服务]
    D((调度器)) == 强依赖 ==> C
    C <--> D
  end
  D -.- A
`
	flow, err := parseFlowchart(code)
	if err != nil {
		t.Fatal(err)
	}
	if len(flow.groups) != 3 || len(flow.nodes) != 4 || len(flow.edges) != 5 {
		t.Fatalf("groups=%d nodes=%d edges=%d", len(flow.groups), len(flow.nodes), len(flow.edges))
	}
	if flow.nodes[flow.index["A"]].shape != "cylinder" || flow.nodes[flow.index["B"]].shape != "hexagon" {
		t.Fatalf("shapes: A=%s B=%s", flow.nodes[flow.index["A"]].shape, flow.nodes[flow.index["B"]].shape)
	}
	if flow.groups[1].title != "内部" || flow.groups[1].parent != 0 || flow.groups[0].depth != 0 {
		t.Fatalf("nesting: %+v %+v", flow.groups[0], flow.groups[1])
	}
	if flow.groups[0].dir != "TD" || flow.groups[1].dir != "" {
		t.Fatalf("only the group that declares a direction may set it: %q %q", flow.groups[0].dir, flow.groups[1].dir)
	}
	if flow.dir != "LR" {
		t.Fatalf("a local direction changed the whole flow: %q", flow.dir)
	}
	if len(flow.groups[0].children) != 1 || len(flow.groups[0].nodes) != 1 {
		t.Fatalf("membership: children=%v nodes=%v", flow.groups[0].children, flow.groups[0].nodes)
	}
	styles := map[byte]int{}
	tails, heads := 0, 0
	for _, e := range flow.edges {
		styles[e.style]++
		if e.tail != 0 {
			tails++
		}
		if e.head != 0 {
			heads++
		}
	}
	if styles['.'] != 2 || styles['='] != 1 || styles['-'] != 2 {
		t.Fatalf("styles = %v", styles)
	}
	if tails != 1 || heads < 4 {
		t.Fatalf("two-headed link lost an end: tails=%d heads=%d", tails, heads)
	}
	flow.layout(measureFixed)
	for _, grp := range flow.groups {
		if !grp.drawn || grp.x1 <= grp.x0 || grp.y1 <= grp.y0 {
			t.Fatalf("group box not laid out: %+v", grp)
		}
		if grp.x0 < 0 || grp.y0 < 0 || grp.x1 > flow.width || grp.y1 > flow.height {
			t.Errorf("group %s outside the diagram: %+v in %vx%v", grp.title, grp, flow.width, flow.height)
		}
	}
	for _, n := range flow.nodes {
		if n.x-n.w/2 < -0.01 || n.y-n.h/2 < -0.01 || n.x+n.w/2 > flow.width+0.01 || n.y+n.h/2 > flow.height+0.01 {
			t.Errorf("node %s outside the diagram", n.id)
		}
		grp := flow.groups[n.group]
		if n.group < 0 || grp == nil {
			t.Fatalf("node %s has no group", n.id)
		}
		if n.x-n.w/2 < grp.x0-0.01 || n.y-n.h/2 < grp.y0-0.01 || n.x+n.w/2 > grp.x1+0.01 || n.y+n.h/2 > grp.y1+0.01 {
			t.Errorf("node %s is not inside its group box", n.id)
		}
	}
	for _, child := range flow.groups[0].children {
		box := flow.groups[child]
		if box.x0 < flow.groups[0].x0 || box.y0 < flow.groups[0].y0 || box.x1 > flow.groups[0].x1 || box.y1 > flow.groups[0].y1 {
			t.Errorf("nested group box escapes its parent: %+v", box)
		}
	}
	description := flow.describe()
	if !strings.Contains(description, "分组：边缘节点、内部、核心服务") || !strings.Contains(description, "数据库") {
		t.Fatalf("description = %s", description)
	}
}

func TestFlowchartSubgraphErrorsFallBackCleanly(t *testing.T) {
	cases := map[string]string{
		"flowchart TD\n  A --> B\n  end":              "end",
		"flowchart TD\n  subgraph X [组]\n    A --> B": "end",
		"flowchart TD\n  subgraph\n end":              "",
	}
	for source, want := range cases {
		diagram := parseMermaid(source, measureFixed)
		if want == "" {
			continue // an unnamed group is drawn, not rejected
		}
		if diagram.err == nil {
			t.Errorf("accepted %q", source)
			continue
		}
		if !strings.Contains(diagram.err.Error(), want) {
			t.Errorf("error %q does not explain %q", diagram.err, want)
		}
	}
	deep := "flowchart TD\n"
	for i := 0; i < 12; i++ {
		deep += strings.Repeat("  ", i) + "subgraph G" + strings.Repeat("0", i+1) + "\n"
	}
	if diagram := parseMermaid(deep, measureFixed); diagram.err == nil || !strings.Contains(diagram.err.Error(), "嵌套") {
		t.Fatalf("deep nesting should fall back with a reason, got %v", diagram.err)
	}
}

func TestStateDiagramParsesStatesPseudostatesAndEvents(t *testing.T) {
	code := `stateDiagram-v2
  [*] --> Idle
  state "空闲等待" as Idle
  Idle : 没有任务在运行
  Idle --> Running : 收到任务
  state Working {
    Running --> Paused : 暂停
  }
  Running --> [*] : 完成
  state Check <<choice>>
  Running --> Check
`
	diagram := parseMermaid(code, measureFixed)
	if diagram.err != nil {
		t.Fatal(diagram.err)
	}
	if diagram.kind != "state" || diagram.flow == nil {
		t.Fatalf("diagram = %+v", diagram)
	}
	flow := diagram.flow
	start := flow.nodes[flow.index[stateStart]]
	end := flow.nodes[flow.index[stateEnd]]
	if start.shape != "start" || end.shape != "end" || start.label != "" {
		t.Fatalf("pseudostates: start=%+v end=%+v", start, end)
	}
	idle := flow.nodes[flow.index["Idle"]]
	if idle.label != "空闲等待\n没有任务在运行" || idle.shape != "round" {
		t.Fatalf("state label = %q (%s)", idle.label, idle.shape)
	}
	if flow.nodes[flow.index["Check"]].shape != "diamond" {
		t.Fatalf("choice state = %s", flow.nodes[flow.index["Check"]].shape)
	}
	if len(flow.groups) != 1 || flow.groups[0].title != "Working" || !flow.groups[0].drawn {
		t.Fatalf("composite state group = %+v", flow.groups)
	}
	if flow.nodes[flow.index["Paused"]].group != 0 {
		t.Fatal("a state inside a composite region belongs to it")
	}
	var events []string
	for _, e := range flow.edges {
		events = append(events, e.label)
	}
	if strings.Join(events, ",") != ",收到任务,暂停,完成," {
		t.Fatalf("events = %v", events)
	}
	if start.w > 30 || end.w > 30 {
		t.Fatalf("pseudostates should stay small: %v %v", start.w, end.w)
	}
	for _, n := range flow.nodes {
		if n.x-n.w/2 < -0.01 || n.y-n.h/2 < -0.01 || n.x+n.w/2 > flow.width+0.01 || n.y+n.h/2 > flow.height+0.01 {
			t.Errorf("state %s outside the diagram", n.id)
		}
	}
	description := flow.describe()
	if !strings.HasPrefix(description, "状态图：") || !strings.Contains(description, "起始 → 空闲等待") || !strings.Contains(description, "结束") {
		t.Fatalf("description = %s", description)
	}
	for _, source := range []string{"stateDiagram\nstate A\nnote right of A : 说明", "stateDiagram-v2\n} "} {
		if d := parseMermaid(source, measureFixed); d.err == nil {
			t.Errorf("unsupported state syntax should fall back: %q", source)
		}
	}
}

func TestSequenceActorsActivationAndBlocks(t *testing.T) {
	code := `sequenceDiagram
  autonumber
  actor U as 用户
  participant Core
  U->>+Core: 提交任务
  activate Core
  Core-->>-Core: 回执
  deactivate Core
  par 并行分支
    Core->>Core: 写入
  and 另一分支
    Core->>U: 通知
  end
  rect rgb(200, 220, 255)
    U->>Core: 查询
    Core-->>-U: 完成
  end
`
	seq, err := parseSequence(code)
	if err != nil {
		t.Fatal(err)
	}
	if !seq.participants[0].actor || seq.participants[0].label != "用户" {
		t.Fatalf("actor = %+v", seq.participants[0])
	}
	if seq.participants[1].actor || seq.participants[1].label != "Core" {
		t.Fatalf("participant = %+v", seq.participants[1])
	}
	seq.layout(measureFixed)
	if len(seq.activations) != 2 {
		t.Fatalf("activations = %+v", seq.activations)
	}
	for _, bar := range seq.activations {
		if bar.bottom < bar.top || bar.participant != 1 {
			t.Errorf("bad activation bar: %+v", bar)
		}
	}
	if seq.activations[0].level != 0 || seq.activations[1].level != 1 {
		t.Fatalf("nested activations need distinct levels: %+v", seq.activations)
	}
	if seq.activations[0].bottom <= seq.activations[1].bottom {
		t.Fatalf("the outer activation must outlast the inner one: %+v", seq.activations)
	}
	frames, dividers, tinted := 0, 0, 0
	for _, step := range seq.steps {
		switch {
		case step.kind == "frame" && step.block == "par":
			frames++
			if len(step.frameDividers) != 1 {
				t.Errorf("par frame dividers = %v", step.frameDividers)
			}
		case step.kind == "frame" && step.hasColor:
			tinted++
			if step.color.R != 200 || step.color.G != 220 || step.color.B != 255 {
				t.Errorf("rect color = %+v", step.color)
			}
		case step.kind == "divider":
			dividers++
		}
	}
	if frames != 1 || dividers != 1 || tinted != 1 {
		t.Fatalf("frames=%d dividers=%d tinted=%d", frames, dividers, tinted)
	}
	if seq.width <= 0 || seq.height <= 0 {
		t.Fatalf("size = %vx%v", seq.width, seq.height)
	}
	for _, bad := range []string{
		"sequenceDiagram\n  participant A\n  rect notacolor\n    A->>A: x\n  end",
		"sequenceDiagram\n  A->>+B: x\n  activate",
	} {
		if _, err := parseSequence(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if color, ok := parseRectColor("rgba(10, 20, 30, 0.5)"); !ok || color.R != 10 || color.A == 255 {
		t.Fatalf("rgba = %+v %v", color, ok)
	}
	if color, ok := parseRectColor("#0f0"); !ok || color.G != 255 {
		t.Fatalf("hex = %+v %v", color, ok)
	}
	if _, ok := parseRectColor("rgb(1,2)"); ok {
		t.Fatal("a short rgb() should be rejected")
	}
	if _, ok := parseRectColor("#12345"); ok {
		t.Fatal("a five digit hex color should be rejected")
	}
}

func TestNewMermaidKindsRenderNatively(t *testing.T) {
	reply := "图如下：\n\n```mermaid\nflowchart LR\n  subgraph Edge [边缘]\n    A[(数据库)] -.-> B{{网关}}\n  end\n  B ==> C([完成])\n```\n\n```mermaid\nstateDiagram-v2\n  [*] --> Idle : 启动\n  Idle --> Busy : 收到任务\n  Busy --> [*]\n```\n\n```mermaid\nsequenceDiagram\n  actor U as 用户\n  U->>+Core: 提交\n  Core-->>-U: 回执\n```\n"
	r := run("r1", "t1", api.RunCompleted, "画图", reply, 0)
	w, _ := newViewWorkbench(r)
	tt := ui.NewTester(w.view, 1280, 1600)
	tt.Advance(time.Second)
	for _, source := range []string{
		"flowchart LR\n  subgraph Edge [边缘]\n    A[(数据库)] -.-> B{{网关}}\n  end\n  B ==> C([完成])\n",
		"stateDiagram-v2\n  [*] --> Idle : 启动\n  Idle --> Busy : 收到任务\n  Busy --> [*]\n",
		"sequenceDiagram\n  actor U as 用户\n  U->>+Core: 提交\n  Core-->>-U: 回执\n",
	} {
		if d := parseMermaid(source, measureFixed); d.err != nil {
			t.Fatalf("diagram failed to parse: %v", d.err)
		}
	}
	if !tt.HasText("流程图") || !tt.HasText("时序图") {
		t.Fatalf("titles missing: %q", tt.Texts())
	}
	if _, ok := tt.AccessByRole(ui.RoleImage, "状态图：起始 → Idle（启动）；Idle → Busy（收到任务）；Busy → 结束"); !ok {
		t.Fatalf("state diagram is not exposed as an image: %q", tt.Texts())
	}
	if _, ok := tt.AccessByRole(ui.RoleImage, "流程图：数据库 → 网关；网关 → 完成；分组：边缘"); !ok {
		t.Fatalf("flowchart label missing: %q", tt.Texts())
	}
}

func TestPipeTableChartAndStructuredVisualBlocksRenderNatively(t *testing.T) {
	spec, err := parseChartSpecWithInfo("chart bar unit=ms title=\"延迟对比\"", " | p50 | p95\n* Redis | 12 | 40\nMemcached | 9 | 31\n")
	if err != nil {
		t.Fatalf("pipe chart parse failed: %v", err)
	}
	if spec.Type != "bar" || spec.Unit != "ms" || spec.Title != "延迟对比" || len(spec.Series) != 2 || len(spec.Labels) != 2 || !spec.Highlights[0] {
		t.Fatalf("unexpected pipe chart spec: %+v", spec)
	}
	reply := "核心结论如下：\n\n" +
		"```callout ok 延迟优化通过\n整体 P95 下降 38%。\n```\n\n" +
		"```chart bar unit=ms title=\"缓存延迟对比\"\n | p50 | p95\n* Redis | 12 | 40\nMemcached | 9 | 31\n```\n\n" +
		"```kv cols=2\n* 方案: 原生 MyGo GPU 渲染\n依赖: 无 HTML / 无浏览器\n主题: 自动跟随亮暗色\n```\n\n" +
		"```timeline\n阶段 1 | 解析数据 | Markdown 围栏块\n*阶段 2 | GPU 绘制 | 原生交互组件\n```\n\n" +
		"```tree\nsrc/\n  + visual_blocks.go | 原生结构化视觉块\n  ~ visuals.go | 接入渲染管线\n```\n\n" +
		"```limits\nP95 延迟 | 40 / 100 | ms\n```\n\n" +
		"```ask\n下一步采取哪种部署策略？\n* 灰度发布 | 先覆盖 10% 节点\n- 全量发布 | 一次性切换\n```\n"
	r := run("r1", "t1", api.RunCompleted, "展示数据", reply, 0)
	w, f := newViewWorkbench(r)
	tt := ui.NewTester(w.view, 1280, 1800)
	tt.Advance(time.Second)
	for _, expected := range []string{"延迟优化通过", "缓存延迟对比", "原生 MyGo GPU 渲染", "阶段 2", "visual_blocks.go", "P95 延迟", "灰度发布"} {
		if !tt.HasText(expected) {
			t.Fatalf("missing rendered visual block text %q in %v", expected, tt.Texts())
		}
	}
	w.setDraft("t1", "保留的草稿")
	mustClick(t, tt, "灰度发布")
	tt.Frame()
	if commands := f.dispatched(); len(commands) != 1 || !strings.Contains(commands[0].Message, "灰度发布") || !strings.Contains(commands[0].Message, "下一步采取哪种部署策略") {
		t.Fatalf("ask choice was not sent: %+v", commands)
	}
	if w.composerFor("t1").draft != "保留的草稿" {
		t.Fatalf("ask send replaced the unsent draft: %q", w.composerFor("t1").draft)
	}
	if !tt.HasText("已回答：") {
		t.Fatalf("ask card did not collapse: %q", tt.Texts())
	}
}

func TestInlineRenameInputDoesNotClipOrSinkAndSubagentHighlightsParent(t *testing.T) {
	w := designReviewWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	// Subagent "构建与类型检查" must not appear as a separate sidebar tree item.
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "构建与类型检查"); ok {
		t.Fatal("subagent rendered as a standalone sidebar row")
	}
	// Selecting the subagent highlights its parent in the sidebar and shows the Subagent bar.
	w.selectThread("child")
	tt.Frame()
	parentItem, ok := tt.AccessByRole(ui.RoleTreeItem, "梳理下完成度")
	if !ok || !parentItem.Selected {
		t.Fatalf("parent sidebar row should stay selected when viewing subagent: %+v %v", parentItem, ok)
	}
	if !tt.HasText("返回父会话") {
		t.Fatal("missing subagent bar above composer")
	}
	mustClick(t, tt, "返回父会话")
	if w.selected != "t1" {
		t.Fatalf("expected return to parent t1, got %q", w.selected)
	}
	// Double-click / inline rename field must have enough height (>= 24px) so 13px text never sinks.
	w.beginRename("t1")
	tt.Frame()
	field, ok := tt.Access("重命名会话")
	if !ok || field.Bounds.H < 24 {
		t.Fatalf("rename input too short (causes text sinking): %+v %v", field, ok)
	}
}

func TestAskCardMultiSkipPersistsAndHotkey(t *testing.T) {
	reply := "```ask multi\n要保留哪些检查？\n* 类型检查 | 推荐先跑\n- 竞态检测\n```"
	w, f := newViewWorkbench(run("r1", "t1", api.RunCompleted, "检查", reply, 0))
	path := filepath.Join(t.TempDir(), "desktop-state.json")
	w.defaults.StatePath = path
	tt := ui.NewTester(w.view, 1100, 800)
	tt.Advance(time.Second)
	if !tt.HasText("可多选") || !tt.HasText("推荐") {
		t.Fatalf("ask chrome missing: %q", tt.Texts())
	}
	mustClick(t, tt, "类型检查")
	mustClick(t, tt, "竞态检测")
	mustClick(t, tt, "提交")
	tt.Frame()
	commands := f.dispatched()
	if len(commands) != 1 || !strings.Contains(commands[0].Message, "类型检查") || !strings.Contains(commands[0].Message, "竞态检测") {
		t.Fatalf("multi ask = %+v", commands)
	}
	if !tt.HasText("已回答：") {
		t.Fatalf("collapsed answer missing: %q", tt.Texts())
	}
	restored := newWorkbench(f)
	restored.defaults.StatePath = path
	if err := restored.restoreState(); err != nil {
		t.Fatal(err)
	}
	restored.applySnapshot(api.Snapshot{Sequence: 3, Threads: []api.Thread{thread("t1", "第一条线程", 0)}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "检查", reply, 0)}})
	restored.selected = "t1"
	again := ui.NewTester(restored.view, 1100, 800)
	again.Advance(time.Second)
	if !again.HasText("已回答：") || again.HasText("提交") {
		t.Fatalf("answer did not survive restart: %q", again.Texts())
	}

	ask := "```ask\n要现在发布吗？\n- 立即发布\n- 先不发布\n```\n"
	w2, f2 := newViewWorkbench(run("r2", "t1", api.RunRunning, "发布", ask+"\n仍在执行。\n", 0))
	w2.tools = []api.ToolCall{{ID: "tool-1", ThreadID: "t1", RunID: "r2", Name: "read", Status: "running", OutputOffset: len(ask)}}
	view := ui.NewTester(w2.view, 1100, 1600)
	view.Advance(time.Second)
	if !view.HasText("要现在发布吗？") || !view.HasText("跳过") {
		t.Fatalf("running ask missing: %q", view.Texts())
	}
	mustClick(t, view, "立即发布")
	view.Frame()
	commands = f2.dispatched()
	if len(commands) != 1 || (commands[0].Kind != api.SteerRun && commands[0].Kind != api.QueueMessage) || !strings.Contains(commands[0].Message, "立即发布") {
		t.Fatalf("running ask did not follow the composer path: %+v", commands)
	}
	if !view.HasText("已回答：") {
		t.Fatalf("running answer did not collapse: %q", view.Texts())
	}

	w3, f3 := newViewWorkbench(run("r3", "t1", api.RunCompleted, "询问", ask, 0))
	skipView := ui.NewTester(w3.view, 1100, 900)
	skipView.Advance(time.Second)
	mustClick(t, skipView, "跳过")
	skipView.Frame()
	if len(f3.dispatched()) != 0 || !skipView.HasText("已跳过") {
		t.Fatalf("skip = dispatched %+v texts %q", f3.dispatched(), skipView.Texts())
	}

	// Number keys answer a single-choice card without touching the composer
	// draft, and stay inert while the composer holds the keyboard focus.
	digits := "```ask\n选哪个主题？\n- 深色\n- 浅色\n```\n"
	w4, f4 := newViewWorkbench(run("r4", "t1", api.RunCompleted, "主题", digits, 0))
	tt4 := ui.NewTester(w4.view, 1100, 900)
	tt4.Advance(time.Second)
	mustClick(t, tt4, "消息输入框")
	tt4.Key(0, ui.Key2)
	if len(f4.dispatched()) != 0 {
		t.Fatal("a digit typed in the composer must not answer the card")
	}
	w4.widgets.sidebarFocus.Focus() // leave the composer so the card's keys arm
	tt4.Frame()
	tt4.Key(0, ui.Key2)
	tt4.Frame()
	commands = f4.dispatched()
	if len(commands) != 1 || !strings.Contains(commands[0].Message, "浅色") {
		t.Fatalf("digit key did not answer the card: %+v", commands)
	}
	if !tt4.HasText("已回答：") {
		t.Fatalf("hotkey answer did not collapse: %q", tt4.Texts())
	}
}

func TestVisualGalleryRenders(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR to export native review images")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	reply := strings.Join([]string{
		"```callout warn 发布前确认\n灰度范围只覆盖 10%。\n```",
		"```kv title=\"发布参数\"\n环境: 生产\n窗口: 30 分钟\n```",
		"```ask\n下一步采取哪种部署策略？\n* 灰度发布 | 先覆盖 10% 节点\n- 全量发布 | 一次性切换\n```",
		"```chart bar title=\"延迟对比\"\n | p50 | p95\nRedis | 12 | 40\nMemcached | 9 | 31\n```",
		"```chart line title=\"趋势\"\n | QPS\n周一 | 120\n周二 | 180\n周三 | 160\n```",
		"```chart pie title=\"占比\"\n | 份额\n缓存 | 62\n回源 | 38\n```",
		"```flow LR\n请求 -> 网关 -> 缓存\n网关 --> 回源\n```",
	}, "\n\n")
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "看图", reply, 0))
	for _, spec := range []struct {
		name string
		dark bool
	}{{"visuals-light", false}, {"visuals-dark", true}} {
		tt := ui.NewTesterWith(w.view, 1280, 2200, ui.TesterOptions{Dark: spec.dark})
		tt.Advance(time.Second)
		for _, text := range []string{"发布前确认", "灰度发布", "延迟对比", "趋势", "占比", "流程图"} {
			if !tt.HasText(text) {
				t.Fatalf("%s missing %q in %v", spec.name, text, tt.Texts())
			}
		}
		file, err := os.Create(filepath.Join(output, spec.name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(file, tt.Image()); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}

	studioReply := strings.Join([]string{
		"```chart\n{\"type\":\"bar\",\"title\":\"24 小时请求与费用\",\"heroLabel\":\"今日累计消耗\",\"hero\":\"$42.80\",\"badge\":\"缓存已省 $19.20\",\"badgeTone\":\"success\",\"delta\":\"较昨日 -14%\",\"unit\":\"k\",\"labels\":[\"00\",\"02\",\"04\",\"06\",\"08\",\"10\",\"12\",\"14\",\"16\",\"18\",\"20\",\"22\"],\"series\":[{\"name\":\"Tokens\",\"data\":[12,8,4,10,48,92,76,118,104,88,54,31]}]}\n```",
		"```chart\n{\"type\":\"breakdown\",\"title\":\"模型算力分布\",\"heroLabel\":\"主力模型占比\",\"hero\":\"68.4%\",\"badge\":\"Claude 4.5\",\"badgeTone\":\"accent\",\"unit\":\"k\",\"series\":[{\"name\":\"Claude Sonnet\",\"subtitle\":\"328 轮 · 主力编码\",\"badge\":\"$24.10\",\"data\":[420],\"sparkline\":[18,22,19,35,44,39,58,64]},{\"name\":\"DeepSeek V4\",\"subtitle\":\"214 轮 · 推理检索\",\"badge\":\"$11.30\",\"data\":[260],\"sparkline\":[9,14,18,16,28,31,27,36]},{\"name\":\"Gemini Flash\",\"subtitle\":\"96 轮 · 快速摘要\",\"badge\":\"$7.40\",\"data\":[110],\"sparkline\":[12,10,8,15,11,14,19,21]}]}\n```",
		"```chart\n{\"type\":\"heatmap\",\"style\":\"dots\",\"title\":\"7×12 活跃热点矩阵\",\"unit\":\"轮\",\"labels\":[\"00\",\"02\",\"04\",\"06\",\"08\",\"10\",\"12\",\"14\",\"16\",\"18\",\"20\",\"22\"],\"series\":[{\"name\":\"周一\",\"data\":[null,0,0,1,4,8,6,9,7,5,2,0]},{\"name\":\"周二\",\"data\":[0,0,null,2,6,11,9,12,8,6,3,1]},{\"name\":\"周三\",\"data\":[1,0,0,1,5,9,8,14,10,4,2,null]},{\"name\":\"周四\",\"data\":[0,null,0,0,3,7,10,11,9,7,4,1]},{\"name\":\"周五\",\"data\":[2,0,0,1,4,8,7,8,6,3,1,0]}]}\n```",
		"```chart\n{\"type\":\"donut\",\"title\":\"Token 缓存效率\",\"centerLabel\":\"78%\",\"unit\":\"k\",\"labels\":[\"缓存命中\",\"常规输入\",\"模型输出\"],\"series\":[{\"name\":\"Tokens\",\"data\":[580,165,142]}]}\n```",
		"```limits\nContext 窗口 | 42 / 200 | k tok\n并发配额 | 11 / 20 | slots\nTPM 上限 | 82 / 100 | %\n错误预算 | 105 / 100 | %\n```",
	}, "\n\n")
	studioW, _ := newViewWorkbench(run("r-studio", "t1", api.RunCompleted, "Studio Showcase", studioReply, 0))
	for _, spec := range []struct {
		name string
		dark bool
	}{{"studio-light", false}, {"studio-dark", true}} {
		tt := ui.NewTesterWith(studioW.view, 1280, 2600, ui.TesterOptions{Dark: spec.dark})
		tt.Advance(time.Second)
		for _, text := range []string{"$42.80", "模型算力分布", "Claude Sonnet", "7×12 活跃热点矩阵", "高峰：", "78%", "接近上限"} {
			if !tt.HasText(text) {
				t.Fatalf("%s missing %q in %v", spec.name, text, tt.Texts())
			}
		}
		file, err := os.Create(filepath.Join(output, spec.name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(file, tt.Image()); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpgradedNativeChartTypesCSVSVGAndLegend(t *testing.T) {
	_, p := workbenchTheme(false, true, false)
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "stacked100",
			raw:  "```chart\n{\"type\":\"stacked100\",\"title\":\"配额占比\",\"labels\":[\"Q1\",\"Q2\"],\"series\":[{\"name\":\"A\",\"data\":[30,60]},{\"name\":\"B\",\"data\":[70,40]}],\"refLines\":[{\"value\":0.5,\"label\":\"50%线\",\"tone\":\"warning\"}]}\n```",
			want: []string{"配额占比", "50%线", "A", "B"},
		},
		{
			name: "multi_hbar",
			raw:  "```chart hbar title=\"横向对比\"\n | 前 | 后\nAPI | 120 | 40\nDB | 80 | 30\n```",
			want: []string{"横向对比", "前", "后"},
		},
		{
			name: "combo_dual_axis",
			raw:  "```chart\n{\"type\":\"combo\",\"title\":\"请求与错误率\",\"unit\":\"次\",\"unitRight\":\"%\",\"labels\":[\"Mon\",\"Tue\",\"Wed\"],\"series\":[{\"name\":\"QPS\",\"kind\":\"bar\",\"data\":[100,200,150]},{\"name\":\"错误率\",\"kind\":\"line\",\"axis\":\"right\",\"data\":[1.2,2.4,0.8]}]}\n```",
			want: []string{"请求与错误率", "QPS", "错误率"},
		},
		{
			name: "scatter_bubble",
			raw:  "```chart\n{\"type\":\"bubble\",\"title\":\"延迟与流量\",\"unit\":\"ms\",\"series\":[{\"name\":\"核心服务\",\"points\":[{\"label\":\"Auth\",\"x\":10,\"y\":25,\"r\":8}]},{\"name\":\"边缘服务\",\"points\":[{\"label\":\"Pay\",\"x\":30,\"y\":65,\"r\":20}]}]}\n```",
			want: []string{"延迟与流量", "核心服务", "边缘服务"},
		},
		{
			name: "heatmap",
			raw:  "```chart\n{\"type\":\"heatmap\",\"title\":\"负载热力图\",\"labels\":[\"00:00\",\"06:00\",\"12:00\"],\"series\":[{\"name\":\"API\",\"data\":[12,45,88]},{\"name\":\"Worker\",\"data\":[8,30,62]}]}\n```",
			want: []string{"负载热力图", "00:00", "12:00"},
		},
		{
			name: "time_axis_line",
			raw:  "```chart\n{\"type\":\"line\",\"xType\":\"time\",\"title\":\"连续时间趋势\",\"labels\":[\"2026-01-01\",\"2026-01-05\",\"2026-01-12\"],\"series\":[{\"name\":\"Visits\",\"data\":[100,220,180]}]}\n```",
			want: []string{"连续时间趋势"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := newViewWorkbench(run("r-"+tc.name, "t1", api.RunCompleted, "chart", tc.raw, 0))
			tt := ui.NewTester(w.view, 1100, 900)
			tt.Advance(time.Second)
			for _, want := range tc.want {
				if !tt.HasText(want) {
					t.Fatalf("%s missing %q in %v", tc.name, want, tt.Texts())
				}
			}
		})
	}

	// Verify CSV & SVG exporters + scatterExtents + time parsing helpers directly.
	scatterSpec, err := parseChartSpec(`{"type":"bubble","title":"S&B","unit":"ms","series":[{"name":"S1","points":[{"label":"P1","x":1,"y":10,"r":4},{"label":"P2","x":5,"y":50,"r":16}]}]}`)
	if err != nil {
		t.Fatalf("parse bubble: %v", err)
	}
	csvOut := chartSpecToCSV(scatterSpec)
	if !strings.Contains(csvOut, "series,label,x,y,r") || !strings.Contains(csvOut, "S1,P1,1,10,4") {
		t.Fatalf("unexpected bubble CSV:\n%s", csvOut)
	}
	svgOut := chartSpecToSVG(scatterSpec, p)
	if !strings.HasPrefix(svgOut, "<svg ") || !strings.Contains(svgOut, "<circle ") || !strings.HasSuffix(svgOut, "</svg>") {
		t.Fatalf("unexpected SVG output:\n%s", svgOut)
	}
	if times, ok := parseTimeLabels([]string{"2026-01-01", "2026-01-03", "2026-01-07"}, ""); !ok || len(times) != 3 {
		t.Fatalf("parseTimeLabels failed: ok=%v len=%d", ok, len(times))
	}

	// Interactive legend toggle hides/restores series without crash, leaving at least 1 active series.
	raw := "```chart bar title=\"双系列交互\"\n | A | B\n一 | 10 | 20\n二 | 15 | 25\n```"
	w, _ := newViewWorkbench(run("r-toggle", "t1", api.RunCompleted, "chart", raw, 0))
	tt := ui.NewTester(w.view, 1100, 900)
	tt.Advance(time.Second)
	if err := tt.Click("切换系列 A"); err != nil {
		t.Fatalf("legend toggle button click failed: %v", err)
	}
	tt.Frame()
}

func TestStudioChartsBreakdownHeatmapHeroAndLimits(t *testing.T) {
	_, lightPal := workbenchTheme(false, true, false)

	// 1. Breakdown chart + Hero KPI + sparklines + CSV/SVG exporters.
	bdRaw := "{\"type\":\"breakdown\",\"title\":\"成本结构\",\"heroLabel\":\"本周总花费\",\"hero\":\"$128.40\",\"badge\":\"缓存已省 $64\",\"badgeTone\":\"success\",\"delta\":\"同比上周 -18%\",\"unit\":\" tok\",\"series\":[{\"name\":\"Claude Sonnet\",\"subtitle\":\"main / auth\",\"badge\":\"$82.10\",\"data\":[64000],\"sparkline\":[12,18,15,24,30]},{\"name\":\"DeepSeek V4\",\"subtitle\":\"legacy\",\"badge\":\"$46.30\",\"data\":[36000],\"sparkline\":[8,10,14,12,16]}]}"
	bdSpec, err := parseChartSpec(bdRaw)
	if err != nil {
		t.Fatalf("parse breakdown: %v", err)
	}
	csvOut := chartSpecToCSV(bdSpec)
	if !strings.Contains(csvOut, "series,subtitle,value,badge") || !strings.Contains(csvOut, "Claude Sonnet,main / auth,64000,$82.10") {
		t.Fatalf("unexpected breakdown CSV:\n%s", csvOut)
	}
	svgOut := chartSpecToSVG(bdSpec, lightPal)
	if !strings.HasPrefix(svgOut, "<svg ") || !strings.Contains(svgOut, "Claude Sonnet") || !strings.Contains(svgOut, "</svg>") {
		t.Fatalf("unexpected breakdown SVG:\n%s", svgOut)
	}

	// 2. Render Studio markdown cards (Hero bar + breakdown + dot-matrix heatmap + upgraded limits).
	md := strings.Join([]string{
		"```chart\n{\"type\":\"bar\",\"title\":\"小时流量\",\"heroLabel\":\"今日总请求\",\"hero\":\"9.6k\",\"badge\":\"健康\",\"badgeTone\":\"success\",\"delta\":\"+12%\",\"labels\":[\"08\",\"10\",\"12\"],\"series\":[{\"name\":\"Req\",\"data\":[120,240,180]}]}\n```",
		"```chart\n" + bdRaw + "\n```",
		"```chart\n{\"type\":\"heatmap\",\"style\":\"dots\",\"title\":\"时段热点\",\"unit\":\"轮\",\"labels\":[\"00\",\"06\",\"12\"],\"series\":[{\"name\":\"周一\",\"data\":[null,0,12]},{\"name\":\"周二\",\"data\":[3,8,20]}]}\n```",
		"```limits\n配额 A | 30 / 100 | %\n配额 B | 82 / 100 | %\n配额 C | 110 / 100 | %\n```",
	}, "\n\n")
	w, _ := newViewWorkbench(run("r-studio-unit", "t1", api.RunCompleted, "studio", md, 0))
	tt := ui.NewTester(w.view, 1280, 1800)
	tt.Advance(time.Second)

	for _, want := range []string{"9.6k", "$128.40", "缓存已省 $64", "同比上周 -18%", "Claude Sonnet", "main / auth", "高峰：周二 12 · 20轮", "接近上限", "超限"} {
		if !tt.HasText(want) {
			t.Fatalf("studio cards missing %q in %v", want, tt.Texts())
		}
	}

	// Inline chart type switch pill on Hero bar chart.
	if err := tt.Click("折线"); err != nil {
		t.Fatalf("click line pill failed: %v", err)
	}
	tt.Frame()
	if err := tt.Click("面积"); err != nil {
		t.Fatalf("click area pill failed: %v", err)
	}
	tt.Frame()

	// Dot-matrix heatmap SVG output emits circles.
	hmSpec, err := parseChartSpec(`{"type":"heatmap","style":"dots","title":"H","labels":["00","06"],"series":[{"name":"Mon","data":[null,5]}]}`)
	if err != nil {
		t.Fatalf("parse dot heatmap: %v", err)
	}
	hmSVG := chartSpecToSVG(hmSpec, lightPal)
	if !strings.Contains(hmSVG, "<circle ") {
		t.Fatalf("dot heatmap SVG missing <circle> elements:\n%s", hmSVG)
	}

	// 3. Upgraded usageDialog renders 24h chart, breakdown by model, 7x24 heatmap, and cache donut.
	baseTime := time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local)
	r1 := api.Run{
		ID: "ru1", ThreadID: "t1", Status: api.RunCompleted, CreatedAt: baseTime, Model: "anthropic/claude-sonnet-4-5",
		Telemetry: &api.RunTelemetry{InputTokens: 4000, OutputTokens: 1500, CacheReadTokens: 3000, TotalTokens: 8500, CostUSD: 0.025, TTFTMs: 500},
	}
	r2 := api.Run{
		ID: "ru2", ThreadID: "t1", Status: api.RunCompleted, CreatedAt: baseTime.Add(2 * time.Hour), Model: "anthropic/claude-sonnet-4-5",
		Telemetry: &api.RunTelemetry{InputTokens: 3000, OutputTokens: 1200, CacheReadTokens: 2000, TotalTokens: 6200, CostUSD: 0.018, TTFTMs: 400},
	}
	r3 := api.Run{
		ID: "ru3", ThreadID: "t1", Status: api.RunCompleted, CreatedAt: baseTime.Add(27 * time.Hour), Model: "deepseek/deepseek-v4",
		Telemetry: &api.RunTelemetry{InputTokens: 2000, OutputTokens: 900, CacheReadTokens: 1000, TotalTokens: 3900, CostUSD: 0.004, TTFTMs: 300},
	}
	uw, _ := newViewWorkbench(r1, r2, r3)
	utt := ui.NewTester(uw.view, 1280, 1400)
	uw.openUsage()
	utt.Advance(time.Second)

	for _, want := range []string{"24 小时活跃分布", "按 Pi 模型", "claude-sonnet-4-5", "deepseek-v4", "7×24 活跃节奏", "Token 结构与缓存效率"} {
		if !utt.HasText(want) {
			t.Fatalf("upgraded usageDialog missing %q in %v", want, utt.Texts())
		}
	}
}
