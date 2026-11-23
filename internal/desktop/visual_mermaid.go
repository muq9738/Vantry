package desktop

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
)

// Mermaid diagrams draw natively: flowcharts and state machines by a layered
// layout, sequence diagrams by columns and rows, and pies as charts. Others
// show their source, as an open fence does while a reply still writes it.

const (
	mmFont      = float32(12)
	mmLabelWrap = float32(180)
	mmMargin    = float32(8)
)

type mmNode struct {
	id, label, shape string
	tw, th           float32 // the label's size
	w, h, x, y       float32 // x, y: the center
	rank, order      int
	group            int // the subgraph that owns it, -1 for none
}

// mmGroup is one `subgraph … end` box. Members are the nodes declared inside
// it; a nested subgraph's box is unioned into its parent's. dir keeps a
// `direction` written inside the group: Mermaid ignores a local direction once
// the group links to the rest of the graph, and this layout uses one axis for
// the whole diagram, so the group follows the diagram instead of the whole
// diagram falling back to source.
type mmGroup struct {
	id, title, dir string
	nodes          []int
	children       []int
	parent, depth  int
	x0, y0, x1, y1 float32
	drawn          bool
}

type mmEdge struct {
	from, to   int
	label      string
	style      byte // '-' solid, '.' dotted, '=' thick
	head, tail byte // '>' arrow, 'o' circle, 'x' cross, 0 none
	lw, lh     float32
	back       bool
}

type mmFlow struct {
	dir           string
	kind          string // flowchart | state
	nodes         []*mmNode
	index         map[string]int
	edges         []mmEdge
	groups        []*mmGroup
	group         int // the subgraph being parsed, -1 for none
	width, height float32
}

type mmDiagram struct {
	kind  string // flowchart | state | sequence | pie
	flow  *mmFlow
	seq   *mmSequence
	pie   *chartSpec
	title string
	err   error
}

func mermaidKind(code string) string {
	for _, line := range strings.Split(code, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "%%") || strings.HasPrefix(line, "---") {
			continue
		}
		head := strings.Fields(line)[0]
		switch strings.ToLower(head) {
		case "graph", "flowchart":
			return "flowchart"
		case "sequencediagram":
			return "sequence"
		case "pie":
			return "pie"
		case "statediagram", "statediagram-v2", "state":
			return "state"
		}
		return head
	}
	return ""
}

// mermaidStatements yields the statements after the header, without
// comments, front matter and init directives.
func mermaidStatements(code string) (header string, statements []string) {
	lines := strings.Split(strings.ReplaceAll(code, "\r\n", "\n"), "\n")
	inFront := false
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if i == 0 && line == "---" {
			inFront = true
			continue
		}
		if inFront {
			if line == "---" {
				inFront = false
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "%%") {
			continue
		}
		if header == "" {
			header = line
			continue
		}
		statements = append(statements, line)
	}
	return header, statements
}

func parseMermaid(code string, measure func(string, float32) (float32, float32)) *mmDiagram {
	if len(code) > 64<<10 {
		return &mmDiagram{err: errors.New("图超过 64 KiB，显示源码")}
	}
	d := &mmDiagram{kind: mermaidKind(code)}
	switch d.kind {
	case "flowchart":
		d.flow, d.err = parseFlowchart(code)
		if d.err == nil {
			d.flow.layout(measure)
		}
	case "state":
		d.flow, d.err = parseStateDiagram(code)
		if d.err == nil {
			d.flow.layout(measure)
		}
	case "sequence":
		d.seq, d.err = parseSequence(code)
		if d.err == nil {
			d.seq.layout(measure)
		}
	case "pie":
		d.pie, d.err = parseMermaidPie(code)
	default:
		d.err = fmt.Errorf("暂不支持 %s 图，显示源码", nonEmptyString(d.kind, "该"))
	}
	return d
}

// ---- flowchart parsing ----

// parseFlowchart reads a graph/flowchart, including nested `subgraph … end`
// boxes, dotted (`.->`), thick (`==>`) and two-headed (`<-->`) links.
func parseFlowchart(code string) (*mmFlow, error) {
	header, statements := mermaidStatements(code)
	g := &mmFlow{dir: "TD", kind: "flowchart", index: map[string]int{}, group: -1}
	fields := strings.Fields(header)
	if len(fields) > 1 {
		if dir, ok := mermaidDirection(fields[1]); ok {
			g.dir = dir
		}
		if rest := strings.TrimSpace(strings.Join(fields[2:], " ")); rest != "" {
			statements = append([]string{rest}, statements...)
		}
	}
	var stack []int
	for _, line := range statements {
		text := strings.TrimSpace(line)
		lower := strings.ToLower(text)
		if rest, ok := splitKeyword(text, "direction"); ok {
			dir, valid := mermaidDirection(rest)
			if !valid {
				return nil, fmt.Errorf("无法识别流程图方向：%s", text)
			}
			if g.group >= 0 {
				g.groups[g.group].dir = dir
			} else {
				g.dir = dir
			}
			continue
		}
		subgraph, isSubgraph := splitKeyword(text, "subgraph")
		switch {
		case lower == "end":
			if len(stack) == 0 {
				return nil, errors.New("流程图出现没有对应 subgraph 的 end，显示源码")
			}
			stack = stack[:len(stack)-1]
			g.group = -1
			if len(stack) > 0 {
				g.group = stack[len(stack)-1]
			}
			continue
		case isSubgraph:
			if len(stack) >= 8 {
				return nil, errors.New("子图嵌套超过 8 层，显示源码")
			}
			if len(g.groups) >= 40 {
				return nil, errors.New("流程图超过 40 个分组，显示源码")
			}
			index := g.openGroup(subgraph)
			if len(stack) > 0 {
				parent := g.groups[stack[len(stack)-1]]
				g.groups[index].parent, g.groups[index].depth = stack[len(stack)-1], parent.depth+1
				parent.children = append(parent.children, index)
			}
			stack = append(stack, index)
			g.group = index
			continue
		}
		for _, statement := range splitStatements(text) {
			if err := g.statement(statement); err != nil {
				return nil, err
			}
			if len(g.nodes) > 150 || len(g.edges) > 400 {
				return nil, errors.New("流程图过大，显示源码")
			}
		}
	}
	if len(stack) != 0 {
		return nil, errors.New("流程图的 subgraph 没有用 end 闭合，显示源码")
	}
	if len(g.nodes) == 0 {
		return nil, errors.New("流程图没有节点")
	}
	if len(g.nodes) > 150 {
		return nil, errors.New("流程图超过 150 个节点，显示源码")
	}
	return g, nil
}

// mermaidDirection accepts the graph directions Mermaid writes.
func mermaidDirection(dir string) (string, bool) {
	switch strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(dir), ";")) {
	case "TB", "TD":
		return "TD", true
	case "BT":
		return "BT", true
	case "LR":
		return "LR", true
	case "RL":
		return "RL", true
	}
	return "", false
}

// splitKeyword separates a statement's leading keyword, as `subgraph` or
// `direction`, from the rest. A keyword must stand alone, so an identifier that
// merely starts with the same letters stays a node name.
func splitKeyword(text, keyword string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < len(keyword) || !strings.EqualFold(trimmed[:len(keyword)], keyword) {
		return "", false
	}
	rest := trimmed[len(keyword):]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// openGroup reads the header of a subgraph: `ID [title]`, `"title"` or a bare
// title. A header without a name gets a generated one and no box label.
func (g *mmFlow) openGroup(rest string) int {
	rest = strings.TrimSpace(rest)
	id, title := "", ""
	if at := strings.IndexByte(rest, '['); at >= 0 {
		id = strings.TrimSpace(rest[:at])
		body := rest[at+1:]
		if close := strings.LastIndexByte(body, ']'); close >= 0 {
			body = body[:close]
		}
		title = cleanMermaidLabel(body)
	} else {
		title = cleanMermaidLabel(rest)
	}
	if id == "" {
		id = title
	}
	return g.addGroup(id, title)
}

func (g *mmFlow) addGroup(id, title string) int {
	if id == "" {
		id = "group-" + strconv.Itoa(len(g.groups)+1)
	}
	g.groups = append(g.groups, &mmGroup{id: id, title: title, parent: -1})
	return len(g.groups) - 1
}

// splitStatements splits on semicolons outside labels.
func splitStatements(line string) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
		case quoted:
		case strings.ContainsRune("[({", r):
			depth++
		case strings.ContainsRune("])}", r):
			depth = max(0, depth-1)
		case r == ';' && depth == 0:
			out = append(out, line[start:i])
			start = i + 1
		}
	}
	return append(out, line[start:])
}

var flowSkipped = []string{"classdef ", "class ", "style ", "linkstyle ", "click ", "accTitle", "accDescr"}

func (g *mmFlow) statement(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	lower := strings.ToLower(s)
	if lower == "end" {
		return errors.New("流程图的 end 必须自成一行，显示源码")
	}
	if _, ok := splitKeyword(s, "subgraph"); ok {
		return errors.New("流程图的 subgraph 块必须自成一行，显示源码")
	}
	for _, prefix := range flowSkipped {
		if lower == strings.TrimSpace(prefix) || strings.HasPrefix(lower, prefix) {
			return nil
		}
	}
	pos := 0
	from, pos, err := g.nodeGroup(s, pos)
	if err != nil {
		return err
	}
	for {
		pos = skipSpace(s, pos)
		if pos >= len(s) {
			return nil
		}
		edge, next, ok := parseFlowEdge(s, pos)
		if !ok {
			return fmt.Errorf("无法解析流程图语句：%s", s)
		}
		to, after, err := g.nodeGroup(s, next)
		if err != nil {
			return err
		}
		for _, a := range from {
			for _, b := range to {
				e := edge
				e.from, e.to = a, b
				g.edges = append(g.edges, e)
			}
		}
		from, pos = to, after
	}
}

func skipSpace(s string, pos int) int {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t') {
		pos++
	}
	return pos
}

func (g *mmFlow) nodeGroup(s string, pos int) ([]int, int, error) {
	var ids []int
	for {
		pos = skipSpace(s, pos)
		id, next, err := g.node(s, pos)
		if err != nil {
			return nil, pos, err
		}
		ids = append(ids, id)
		if len(ids) > 20 {
			return nil, pos, errors.New("单组节点过多，显示源码")
		}
		pos = skipSpace(s, next)
		if pos < len(s) && s[pos] == '&' {
			pos++
			continue
		}
		return ids, pos, nil
	}
}

var flowShapes = []struct{ open, close, shape string }{
	{"(((", ")))", "circle"}, {"((", "))", "circle"}, {"([", "])", "stadium"}, {"[[", "]]", "subroutine"},
	{"[(", ")]", "cylinder"}, {"{{", "}}", "hexagon"}, {"[/", "/]", "parallelogram"}, {"[\\", "\\]", "parallelogram"},
	{"[/", "\\]", "trapezoid"}, {"[", "]", "rect"}, {"(", ")", "round"}, {"{", "}", "diamond"}, {">", "]", "flag"},
}

func isIDRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || r >= 0x80 && !unicode.IsSpace(r) && !unicode.IsPunct(r)
}

func (g *mmFlow) node(s string, pos int) (int, int, error) {
	start := pos
	for pos < len(s) {
		r, size := utf8.DecodeRuneInString(s[pos:])
		if !isIDRune(r) {
			break
		}
		pos += size
	}
	if pos == start {
		return 0, pos, fmt.Errorf("流程图缺少节点名：%s", s)
	}
	id := s[start:pos]
	label, shape := "", ""
	for _, candidate := range flowShapes {
		if !strings.HasPrefix(s[pos:], candidate.open) {
			continue
		}
		body := pos + len(candidate.open)
		end := closingIndex(s, body, candidate.close)
		if end < 0 {
			continue // as [/a\] after [/ found no /]
		}
		label, shape = cleanMermaidLabel(s[body:end]), candidate.shape
		pos = end + len(candidate.close)
		break
	}
	if strings.HasPrefix(s[pos:], ":::") { // a class reference
		pos += 3
		for pos < len(s) && (isIDRune(rune(s[pos])) || s[pos] == '-') {
			pos++
		}
	}
	index, ok := g.index[id]
	if !ok {
		index = len(g.nodes)
		g.index[id] = index
		g.nodes = append(g.nodes, &mmNode{id: id, label: id, shape: "rect", group: -1})
	}
	if shape != "" {
		g.nodes[index].label, g.nodes[index].shape = label, shape
	}
	// A node belongs to the subgraph it first appears in, so a group drawn
	// later still collects the nodes its links introduce.
	if g.group >= 0 && g.nodes[index].group < 0 {
		g.nodes[index].group = g.group
		g.groups[g.group].nodes = append(g.groups[g.group].nodes, index)
	}
	return index, pos, nil
}

// closingIndex finds close after from, skipping quoted text.
func closingIndex(s string, from int, close string) int {
	quoted := false
	for i := from; i < len(s); i++ {
		if s[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted && strings.HasPrefix(s[i:], close) {
			return i
		}
	}
	return -1
}

var htmlBreak = regexp.MustCompile(`(?i)<br\s*/?>`)
var htmlTag = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

func cleanMermaidLabel(label string) string {
	label = strings.TrimSpace(label)
	if len(label) >= 2 && label[0] == '"' && label[len(label)-1] == '"' {
		label = label[1 : len(label)-1]
	}
	label = strings.Trim(label, "`")
	label = htmlBreak.ReplaceAllString(label, "\n")
	label = htmlTag.ReplaceAllString(label, "")
	label = strings.NewReplacer("#quot;", "\"", "&quot;", "\"", "&amp;", "&", "&lt;", "<", "&gt;", ">", "#35;", "#").Replace(label)
	return strings.TrimSpace(label)
}

// parseFlowEdge reads a link, as -->, ---, -.->, ==>, <-->, --o, --x, with a
// label written inside it (-- text -->) or after it (-->|text|).
func parseFlowEdge(s string, pos int) (mmEdge, int, bool) {
	e := mmEdge{style: '-'}
	if pos < len(s) && (s[pos] == '<' || (s[pos] == 'o' || s[pos] == 'x') && pos+1 < len(s) && (s[pos+1] == '-' || s[pos+1] == '=')) {
		e.tail = '>'
		if s[pos] != '<' {
			e.tail = s[pos]
		}
		pos++
	}
	run := func(at int) int {
		end := at
		for end < len(s) && (s[end] == '-' || s[end] == '=' || s[end] == '.') {
			end++
		}
		return end
	}
	end := run(pos)
	if end-pos < 2 {
		return e, pos, false
	}
	body := s[pos:end]
	head := func(at int) (byte, int) {
		if at < len(s) {
			switch s[at] {
			case '>':
				return '>', at + 1
			case 'o', 'x':
				if at+1 >= len(s) || s[at+1] == ' ' || s[at+1] == '|' {
					return s[at], at + 1
				}
			}
		}
		return 0, at
	}
	e.head, end = head(end)
	if e.head == 0 && end < len(s) && s[end] == ' ' && (body == "--" || body == "==" || body == "-.") {
		// A label inside the link: find where the link continues.
		for i := end + 1; i < len(s); i++ {
			if s[i] != '-' && s[i] != '=' && s[i] != '.' {
				continue
			}
			closeEnd := run(i)
			closing := s[i:closeEnd]
			h, after := head(closeEnd)
			if len(closing) >= 2 || closing == "." && h != 0 || h != 0 {
				e.label = cleanMermaidLabel(s[end:i])
				body += closing
				e.head, end = h, after
				break
			}
		}
	}
	switch {
	case strings.Contains(body, "."):
		e.style = '.'
	case strings.Contains(body, "="):
		e.style = '='
	}
	pos = skipSpace(s, end)
	if pos < len(s) && s[pos] == '|' {
		close := strings.IndexByte(s[pos+1:], '|')
		if close < 0 {
			return e, pos, false
		}
		e.label = cleanMermaidLabel(s[pos+1 : pos+1+close])
		pos += close + 2
	}
	return e, pos, true
}

// ---- flowchart layout ----

func (g *mmFlow) horizontal() bool { return g.dir == "LR" || g.dir == "RL" }

func (g *mmFlow) layout(measure func(string, float32) (float32, float32)) {
	for _, n := range g.nodes {
		n.tw, n.th = measure(n.label, mmLabelWrap)
		n.w, n.h = max(n.tw+24, 56), max(n.th+16, 34)
		switch n.shape {
		case "diamond":
			n.w, n.h = n.w*1.3+8, n.h*1.4
		case "circle":
			d := max(n.w, n.h) + 4
			n.w, n.h = d, d
		case "hexagon", "parallelogram", "trapezoid", "flag":
			n.w += 20
		case "cylinder":
			n.h += 10
		case "start":
			n.w, n.h = 18, 18
		case "end":
			n.w, n.h = 24, 24
		}
	}
	for i := range g.edges {
		if g.edges[i].label != "" {
			g.edges[i].lw, g.edges[i].lh = measure(g.edges[i].label, 140)
		}
	}
	g.rank()
	layers := g.order()
	g.place(layers)
}

// rank breaks cycles by DFS order and gives every node its longest path
// from a source.
func (g *mmFlow) rank() {
	n := len(g.nodes)
	out := make([][]int, n)
	for i, e := range g.edges {
		out[e.from] = append(out[e.from], i)
	}
	state := make([]uint8, n) // 0 new, 1 on stack, 2 done
	var visit func(int)
	visit = func(v int) {
		state[v] = 1
		for _, ei := range out[v] {
			e := &g.edges[ei]
			switch state[e.to] {
			case 0:
				visit(e.to)
			case 1:
				e.back = true
			}
		}
		state[v] = 2
	}
	for v := range g.nodes {
		if state[v] == 0 {
			visit(v)
		}
	}
	indegree := make([]int, n)
	for _, e := range g.edges {
		if !e.back && e.from != e.to {
			indegree[e.to]++
		}
	}
	queue := []int{}
	for v := range g.nodes {
		if indegree[v] == 0 {
			queue = append(queue, v)
		}
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, ei := range out[v] {
			e := g.edges[ei]
			if e.back || e.from == e.to {
				continue
			}
			if r := g.nodes[v].rank + 1; r > g.nodes[e.to].rank {
				g.nodes[e.to].rank = r
			}
			indegree[e.to]--
			if indegree[e.to] == 0 {
				queue = append(queue, e.to)
			}
		}
	}
}

// order orders each layer by the barycenters of its neighbors, sweeping down
// and up, as Sugiyama's method does, to uncross edges.
func (g *mmFlow) order() [][]int {
	ranks := 0
	for _, n := range g.nodes {
		ranks = max(ranks, n.rank+1)
	}
	layers := make([][]int, ranks)
	for v, n := range g.nodes {
		n.order = len(layers[n.rank])
		layers[n.rank] = append(layers[n.rank], v)
	}
	preds, succs := make([][]int, len(g.nodes)), make([][]int, len(g.nodes))
	for _, e := range g.edges {
		a, b := e.from, e.to
		if g.nodes[a].rank > g.nodes[b].rank {
			a, b = b, a
		}
		if g.nodes[a].rank == g.nodes[b].rank {
			continue
		}
		preds[b] = append(preds[b], a)
		succs[a] = append(succs[a], b)
	}
	sweep := func(layer []int, neighbors [][]int) {
		center := make(map[int]float64, len(layer))
		for _, v := range layer {
			if len(neighbors[v]) == 0 {
				center[v] = float64(g.nodes[v].order)
				continue
			}
			sum := 0.0
			for _, u := range neighbors[v] {
				sum += float64(g.nodes[u].order)
			}
			center[v] = sum / float64(len(neighbors[v]))
		}
		sort.SliceStable(layer, func(i, j int) bool { return center[layer[i]] < center[layer[j]] })
		for i, v := range layer {
			g.nodes[v].order = i
		}
	}
	for pass := 0; pass < 4; pass++ {
		for r := 1; r < ranks; r++ {
			sweep(layers[r], preds)
		}
		for r := ranks - 2; r >= 0; r-- {
			sweep(layers[r], succs)
		}
	}
	return layers
}

// place gives each node its center: layers along the main axis, nodes of a
// layer along the cross axis, each pulled toward its neighbors.
func (g *mmFlow) place(layers [][]int) {
	horizontal := g.horizontal()
	main := func(n *mmNode) float32 {
		if horizontal {
			return n.w
		}
		return n.h
	}
	cross := func(n *mmNode) float32 {
		if horizontal {
			return n.h
		}
		return n.w
	}
	labelRoom := float32(0)
	for _, e := range g.edges {
		if e.label != "" {
			labelRoom = max(labelRoom, map[bool]float32{true: e.lw, false: e.lh}[horizontal])
		}
	}
	gap := float32(44) + labelRoom
	nodeGap := float32(26)
	pos := float32(0)
	mainPos := make([]float32, len(layers))
	for r, layer := range layers {
		size := float32(0)
		for _, v := range layer {
			size = max(size, main(g.nodes[v]))
		}
		mainPos[r] = pos + size/2
		pos += size + gap
	}
	crossPos := make([]float32, len(g.nodes))
	for _, layer := range layers {
		at := float32(0)
		for _, v := range layer {
			crossPos[v] = at + cross(g.nodes[v])/2
			at += cross(g.nodes[v]) + nodeGap
		}
	}
	neighbors := make([][]int, len(g.nodes))
	for _, e := range g.edges {
		if e.from != e.to {
			neighbors[e.from] = append(neighbors[e.from], e.to)
			neighbors[e.to] = append(neighbors[e.to], e.from)
		}
	}
	for pass := 0; pass < 6; pass++ {
		for _, layer := range layers {
			want := make([]float32, len(layer))
			for i, v := range layer {
				want[i] = crossPos[v]
				if len(neighbors[v]) > 0 {
					sum := float32(0)
					for _, u := range neighbors[v] {
						sum += crossPos[u]
					}
					want[i] = (crossPos[v] + 2*sum/float32(len(neighbors[v]))) / 3
				}
			}
			// Keep the order and spacing: push right past overlaps, then
			// left, averaging so neither end drifts.
			right := make([]float32, len(layer))
			for i, v := range layer {
				right[i] = want[i]
				if i > 0 {
					right[i] = max(right[i], right[i-1]+(cross(g.nodes[layer[i-1]])+cross(g.nodes[v]))/2+nodeGap)
				}
			}
			left := make([]float32, len(layer))
			for i := len(layer) - 1; i >= 0; i-- {
				left[i] = want[i]
				if i < len(layer)-1 {
					left[i] = min(left[i], left[i+1]-(cross(g.nodes[layer[i+1]])+cross(g.nodes[layer[i]]))/2-nodeGap)
				}
			}
			for i, v := range layer {
				crossPos[v] = (left[i] + right[i]) / 2
			}
			for i := 1; i < len(layer); i++ { // averaging may close gaps again
				minimum := crossPos[layer[i-1]] + (cross(g.nodes[layer[i-1]])+cross(g.nodes[layer[i]]))/2 + nodeGap
				crossPos[layer[i]] = max(crossPos[layer[i]], minimum)
			}
		}
	}
	lo := float32(math.Inf(1))
	for v, n := range g.nodes {
		lo = min(lo, crossPos[v]-cross(n)/2)
	}
	extentMain := pos - gap
	for v, n := range g.nodes {
		c := crossPos[v] - lo
		m := mainPos[n.rank]
		if g.dir == "BT" || g.dir == "RL" {
			m = extentMain - m
		}
		if horizontal {
			n.x, n.y = m, c
		} else {
			n.x, n.y = c, m
		}
	}
	layoutGroups := g.placeGroups()
	// Group padding can reach past the nodes; shift everything so the
	// diagram still starts at the margin.
	loX, loY := float32(math.Inf(1)), float32(math.Inf(1))
	hiX, hiY := float32(math.Inf(-1)), float32(math.Inf(-1))
	for _, n := range g.nodes {
		loX, hiX = min(loX, n.x-n.w/2), max(hiX, n.x+n.w/2)
		loY, hiY = min(loY, n.y-n.h/2), max(hiY, n.y+n.h/2)
	}
	if layoutGroups {
		for _, box := range g.groups {
			if !box.drawn {
				continue
			}
			loX, hiX = min(loX, box.x0), max(hiX, box.x1)
			loY, hiY = min(loY, box.y0), max(hiY, box.y1)
		}
	}
	dx, dy := mmMargin-loX, mmMargin-loY
	for _, n := range g.nodes {
		n.x, n.y = n.x+dx, n.y+dy
	}
	for _, box := range g.groups {
		box.x0, box.x1 = box.x0+dx, box.x1+dx
		box.y0, box.y1 = box.y0+dy, box.y1+dy
	}
	g.width, g.height = hiX-loX+2*mmMargin, hiY-loY+2*mmMargin
	// Back edges and self loops bend outside the nodes.
	if horizontal {
		g.height += 46
	} else {
		g.width += 30
	}
}

// placeGroups boxes every subgraph after layout: a parent encloses its own
// nodes and its children's boxes, with room for the title above them.
func (g *mmFlow) placeGroups() bool {
	any := false
	// Children have larger indexes than their parents, so going backwards
	// boxes them first.
	for i := len(g.groups) - 1; i >= 0; i-- {
		grp := g.groups[i]
		empty := true
		loX, loY := float32(math.Inf(1)), float32(math.Inf(1))
		hiX, hiY := float32(math.Inf(-1)), float32(math.Inf(-1))
		grow := func(x0, y0, x1, y1 float32) {
			loX, loY, hiX, hiY = min(loX, x0), min(loY, y0), max(hiX, x1), max(hiY, y1)
			empty = false
		}
		for _, v := range grp.nodes {
			n := g.nodes[v]
			grow(n.x-n.w/2, n.y-n.h/2, n.x+n.w/2, n.y+n.h/2)
		}
		for _, child := range grp.children {
			box := g.groups[child]
			if box.drawn {
				grow(box.x0, box.y0, box.x1, box.y1)
			}
		}
		if empty {
			continue
		}
		pad := float32(12)
		title := float32(0)
		if grp.title != "" {
			title = 20
		}
		grp.x0, grp.x1 = loX-pad, hiX+pad
		grp.y0, grp.y1 = loY-pad-title, hiY+pad
		grp.drawn = true
		any = true
	}
	return any
}

// ---- flowchart painting ----

type mmColors struct {
	fill, stroke, text, edge, labelBG ui.Color
	groupFill, groupStroke            ui.Color
}

func mermaidColors(p palette) mmColors {
	edge := p.accent.Mix(p.muted, 0.62)
	if !p.dark {
		edge = p.accent.Mix(p.muted, 0.45)
	}
	return mmColors{
		fill: p.raised, stroke: p.line, text: p.ink, edge: edge, labelBG: p.raised,
		groupFill: p.soft.Mix(p.accent, 0.06), groupStroke: p.line,
	}
}

func (g *mmFlow) paint(painter *ui.Painter, r ui.Rect, colors mmColors) {
	ox, oy := r.X, r.Y
	for _, grp := range g.groups { // parents first: a child's box is drawn on top
		g.paintGroup(painter, ox, oy, grp, colors)
	}
	for _, e := range g.edges {
		g.paintEdge(painter, ox, oy, e, colors)
	}
	for _, n := range g.nodes {
		paintNode(painter, ox+n.x, oy+n.y, n, colors)
	}
	for _, e := range g.edges {
		if e.label == "" {
			continue
		}
		x, y := g.edgeMid(e)
		box := ui.Rect{X: ox + x - e.lw/2 - 5, Y: oy + y - e.lh/2 - 2, W: e.lw + 10, H: e.lh + 4}
		painter.Fill(box, colors.labelBG, 4)
		painter.RichText(box.X+5, box.Y+2, 140, ui.Span{Text: e.label, Size: mmFont - 1, Color: colors.text})
	}
}

func paintNode(painter *ui.Painter, cx, cy float32, n *mmNode, colors mmColors) {
	rect := ui.Rect{X: cx - n.w/2, Y: cy - n.h/2, W: n.w, H: n.h}
	switch n.shape {
	case "diamond":
		path := (&ui.Path{}).MoveTo(cx, rect.Y).LineTo(rect.X+rect.W, cy).LineTo(cx, rect.Y+rect.H).LineTo(rect.X, cy).Close()
		painter.FillPath(path, colors.fill)
		painter.StrokePath(path, 1.2, colors.stroke)
	case "hexagon":
		inset := float32(12)
		path := (&ui.Path{}).MoveTo(rect.X+inset, rect.Y).LineTo(rect.X+rect.W-inset, rect.Y).LineTo(rect.X+rect.W, cy).
			LineTo(rect.X+rect.W-inset, rect.Y+rect.H).LineTo(rect.X+inset, rect.Y+rect.H).LineTo(rect.X, cy).Close()
		painter.FillPath(path, colors.fill)
		painter.StrokePath(path, 1.2, colors.stroke)
	case "parallelogram", "trapezoid", "flag":
		inset := float32(10)
		path := (&ui.Path{}).MoveTo(rect.X+inset, rect.Y).LineTo(rect.X+rect.W, rect.Y).LineTo(rect.X+rect.W-inset, rect.Y+rect.H).LineTo(rect.X, rect.Y+rect.H).Close()
		if n.shape == "trapezoid" {
			path = (&ui.Path{}).MoveTo(rect.X+inset, rect.Y).LineTo(rect.X+rect.W-inset, rect.Y).LineTo(rect.X+rect.W, rect.Y+rect.H).LineTo(rect.X, rect.Y+rect.H).Close()
		}
		if n.shape == "flag" {
			path = (&ui.Path{}).MoveTo(rect.X, rect.Y).LineTo(rect.X+rect.W, rect.Y).LineTo(rect.X+rect.W, rect.Y+rect.H).LineTo(rect.X, rect.Y+rect.H).LineTo(rect.X+inset, cy).Close()
		}
		painter.FillPath(path, colors.fill)
		painter.StrokePath(path, 1.2, colors.stroke)
	case "circle":
		painter.Fill(rect, colors.fill, n.w/2)
		painter.Stroke(rect, colors.stroke, n.w/2, 1.2)
	case "stadium":
		painter.Fill(rect, colors.fill, n.h/2)
		painter.Stroke(rect, colors.stroke, n.h/2, 1.2)
	case "round":
		painter.Fill(rect, colors.fill, 14)
		painter.Stroke(rect, colors.stroke, 14, 1.2)
	case "start": // a state machine's initial pseudostate
		painter.FillPath((&ui.Path{}).Circle(cx, cy, n.w/2), colors.edge)
	case "end": // its final pseudostate: a ring inside a ring
		r := min(n.w, n.h) / 2
		painter.FillPath((&ui.Path{}).Circle(cx, cy, r), colors.labelBG)
		painter.StrokePath((&ui.Path{}).Circle(cx, cy, r), 1.4, colors.edge)
		painter.FillPath((&ui.Path{}).Circle(cx, cy, r-4.5), colors.edge)
	case "cylinder": // a database, drawn as a rimmed barrel
		painter.Fill(rect, colors.fill, 12)
		ry := max(6, min(10, n.h*0.16))
		back := (&ui.Path{}).MoveTo(rect.X, rect.Y+ry).CubeTo(rect.X, rect.Y+ry-1.33*ry, rect.X+rect.W, rect.Y+ry-1.33*ry, rect.X+rect.W, rect.Y+ry)
		painter.StrokePath(back, 1.2, colors.stroke)
		front := (&ui.Path{}).MoveTo(rect.X, rect.Y+ry).CubeTo(rect.X, rect.Y+ry+1.33*ry, rect.X+rect.W, rect.Y+ry+1.33*ry, rect.X+rect.W, rect.Y+ry)
		painter.StrokePath(front, 1.2, colors.stroke)
		painter.Stroke(rect, colors.stroke, 12, 1.2)
	default:
		painter.Fill(rect, colors.fill, 10)
		painter.Stroke(rect, colors.stroke, 10, 1.2)
		if n.shape == "subroutine" {
			painter.Line(rect.X+7, rect.Y, rect.X+7, rect.Y+rect.H, 1.2, colors.stroke)
			painter.Line(rect.X+rect.W-7, rect.Y, rect.X+rect.W-7, rect.Y+rect.H, 1.2, colors.stroke)
		}
	}
	if n.label != "" {
		dy := float32(0)
		if n.shape == "cylinder" {
			dy = 4
		}
		painter.RichText(cx-n.tw/2, cy-n.th/2+dy, mmLabelWrap, ui.Span{Text: n.label, Size: mmFont, Color: colors.text})
	}
}

// paintGroup draws a subgraph box: a translucent rounded panel, its border and
// the group title across the top.
func (g *mmFlow) paintGroup(painter *ui.Painter, ox, oy float32, grp *mmGroup, colors mmColors) {
	if !grp.drawn {
		return
	}
	box := ui.Rect{X: ox + grp.x0, Y: oy + grp.y0, W: grp.x1 - grp.x0, H: grp.y1 - grp.y0}
	painter.Fill(box, colors.groupFill, 10)
	painter.Stroke(box, colors.groupStroke, 10, 1)
	if grp.title == "" {
		return
	}
	tw, th := painter.MeasureText(0, ui.Span{Text: grp.title, Size: mmFont - 1, Weight: 700})
	chip := ui.Rect{X: box.X + 10, Y: box.Y + 4, W: tw + 16, H: th + 6}
	painter.Fill(chip, colors.labelBG, 6)
	painter.Stroke(chip, colors.groupStroke, 6, 1)
	painter.RichText(chip.X+8, chip.Y+3, 0, ui.Span{Text: grp.title, Size: mmFont - 1, Weight: 700, Color: colors.edge})
}

// edgeGeometry returns the cubic of an edge in diagram coordinates.
func (g *mmFlow) edgeGeometry(e mmEdge) (x0, y0, c1x, c1y, c2x, c2y, x1, y1 float32) {
	a, b := g.nodes[e.from], g.nodes[e.to]
	if e.from == e.to { // a self loop beside the node
		if g.horizontal() {
			x0, y0, x1, y1 = a.x-a.w/4, a.y+a.h/2, a.x+a.w/4, a.y+a.h/2
			return x0, y0, x0 - 10, y0 + 34, x1 + 10, y1 + 34, x1, y1
		}
		x0, y0, x1, y1 = a.x+a.w/2, a.y-a.h/4, a.x+a.w/2, a.y+a.h/4
		return x0, y0, x0 + 34, y0 - 10, x1 + 34, y1 + 10, x1, y1
	}
	if g.horizontal() {
		dir := float32(1)
		if b.x < a.x {
			dir = -1
		}
		if e.back { // swing below the row so a loop back stays readable
			drop := max(a.h, b.h)/2 + 34
			x0, y0, x1, y1 = a.x, a.y+a.h/2, b.x, b.y+b.h/2
			return x0, y0, x0, a.y + drop, x1, b.y + drop, x1, y1
		}
		x0, y0 = a.x+dir*a.w/2, a.y
		x1, y1 = b.x-dir*b.w/2, b.y
		if a.rank == b.rank {
			x0, x1 = a.x, b.x
			y0, y1 = a.y+a.h/2, b.y+b.h/2
			return x0, y0, x0, y0 + 30, x1, y1 + 30, x1, y1
		}
		bend := (x1 - x0) / 2
		return x0, y0, x0 + bend, y0, x1 - bend, y1, x1, y1
	}
	dir := float32(1)
	if b.y < a.y {
		dir = -1
	}
	x0, y0 = a.x, a.y+dir*a.h/2
	x1, y1 = b.x, b.y-dir*b.h/2
	if a.rank == b.rank {
		x0, x1 = a.x+a.w/2, b.x-b.w/2
		if b.x < a.x {
			x0, x1 = a.x-a.w/2, b.x+b.w/2
		}
		y0, y1 = a.y, b.y
		return x0, y0, (x0 + x1) / 2, y0, (x0 + x1) / 2, y1, x1, y1
	}
	bend := (y1 - y0) / 2
	if e.back { // swing out to the side so a loop back stays readable
		side := max(a.w, b.w)/2 + 26
		return a.x + a.w/2, a.y, a.x + side + 30, a.y, b.x + side + 30, b.y, b.x + b.w/2, b.y
	}
	return x0, y0, x0, y0 + bend, x1, y1 - bend, x1, y1
}

func (g *mmFlow) edgeMid(e mmEdge) (float32, float32) {
	x0, y0, c1x, c1y, c2x, c2y, x1, y1 := g.edgeGeometry(e)
	return 0.125*x0 + 0.375*c1x + 0.375*c2x + 0.125*x1, 0.125*y0 + 0.375*c1y + 0.375*c2y + 0.125*y1
}

func (g *mmFlow) paintEdge(painter *ui.Painter, ox, oy float32, e mmEdge, colors mmColors) {
	x0, y0, c1x, c1y, c2x, c2y, x1, y1 := g.edgeGeometry(e)
	width := float32(1.3)
	if e.style == '=' {
		width = 2.6
	}
	path := (&ui.Path{}).MoveTo(ox+x0, oy+y0).CubeTo(ox+c1x, oy+c1y, ox+c2x, oy+c2y, ox+x1, oy+y1)
	if e.style == '.' {
		path.Dash(0, 4, 3)
	}
	painter.StrokePath(path, width, colors.edge)
	paintEdgeEnd(painter, ox+x1, oy+y1, ox+c2x, oy+c2y, e.head, width, colors)
	paintEdgeEnd(painter, ox+x0, oy+y0, ox+c1x, oy+c1y, e.tail, width, colors)
}

func paintEdgeEnd(painter *ui.Painter, x, y, fromX, fromY float32, kind byte, width float32, colors mmColors) {
	if kind == 0 {
		return
	}
	dx, dy := x-fromX, y-fromY
	length := float32(math.Hypot(float64(dx), float64(dy)))
	if length < 0.01 {
		dx, dy, length = 0, 1, 1
	}
	dx, dy = dx/length, dy/length
	switch kind {
	case 'o':
		painter.FillPath((&ui.Path{}).Circle(x-dx*4, y-dy*4, 4), colors.labelBG)
		painter.StrokePath((&ui.Path{}).Circle(x-dx*4, y-dy*4, 4), width, colors.edge)
	case 'x':
		cx, cy := x-dx*5, y-dy*5
		painter.Line(cx-4, cy-4, cx+4, cy+4, width+0.4, colors.edge)
		painter.Line(cx-4, cy+4, cx+4, cy-4, width+0.4, colors.edge)
	default:
		size := float32(9) + width
		bx, by := x-dx*size, y-dy*size
		px, py := -dy*size*0.45, dx*size*0.45
		painter.FillPath((&ui.Path{}).MoveTo(x, y).LineTo(bx+px, by+py).LineTo(bx-px, by-py).Close(), colors.edge)
	}
}

func (g *mmFlow) describe() string {
	name := func(n *mmNode) string {
		switch {
		case n.shape == "start":
			return "起始"
		case n.shape == "end":
			return "结束"
		case n.label != "":
			return n.label
		}
		return n.id
	}
	var parts []string
	for _, e := range g.edges {
		text := name(g.nodes[e.from]) + " → " + name(g.nodes[e.to])
		if e.label != "" {
			text += "（" + e.label + "）"
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		for _, n := range g.nodes {
			if n.label != "" {
				parts = append(parts, n.label)
			}
		}
	}
	if titles := g.groupTitles(); len(titles) > 0 {
		parts = append(parts, "分组："+strings.Join(titles, "、"))
	}
	if g.kind == "state" {
		return "状态图：" + strings.Join(parts, "；")
	}
	return "流程图：" + strings.Join(parts, "；")
}

func (g *mmFlow) groupTitles() []string {
	var titles []string
	seen := map[string]bool{}
	for _, grp := range g.groups {
		if grp.title == "" || seen[grp.title] {
			continue
		}
		seen[grp.title] = true
		titles = append(titles, grp.title)
	}
	return titles
}

// ---- state diagrams ----

// A state machine is a directed graph with two pseudostates, so it reuses the
// layered flowchart layout and only adds its own statement syntax.
const (
	stateStart = "[*] start"
	stateEnd   = "[*] end"
)

var stateTransition = regexp.MustCompile(`^(.+?)\s*-->\s*(.+)$`)
var stateDecl = regexp.MustCompile(`^state\s+(?:"([^"]*)"\s+as\s+([^\s{]+)|([^\s{]+))\s*(<<\w+>>)?\s*(\{)?$`)

func parseStateDiagram(code string) (*mmFlow, error) {
	header, statements := mermaidStatements(code)
	g := &mmFlow{dir: "TD", kind: "state", index: map[string]int{}, group: -1}
	if fields := strings.Fields(header); len(fields) > 1 {
		if rest := strings.TrimSpace(strings.Join(fields[1:], " ")); rest != "" {
			statements = append([]string{rest}, statements...)
		}
	}
	var stack []int
	for _, line := range statements {
		text := strings.TrimSpace(line)
		lower := strings.ToLower(text)
		if text == "" {
			continue
		}
		if rest, ok := splitKeyword(text, "direction"); ok {
			dir, valid := mermaidDirection(rest)
			if !valid {
				return nil, fmt.Errorf("无法识别状态图方向：%s", text)
			}
			g.dir = dir
			continue
		}
		_, isNote := splitKeyword(text, "note")
		switch {
		case text == "}":
			if len(stack) == 0 {
				return nil, errors.New("状态图出现多余的 }，显示源码")
			}
			stack = stack[:len(stack)-1]
			g.group = -1
			if len(stack) > 0 {
				g.group = stack[len(stack)-1]
			}
			continue
		case isNote || lower == "--":
			return nil, errors.New("状态图的 note 与并发区暂以源码展示")
		case strings.HasPrefix(lower, "state "):
			// `state "描述" as S1 {`, `state S1 <<choice>>`, `state S1`
			m := stateDecl.FindStringSubmatch(text)
			if m == nil {
				return nil, fmt.Errorf("无法解析状态声明：%s", text)
			}
			id, label := m[3], ""
			if m[2] != "" {
				id, label = m[2], cleanMermaidLabel(m[1])
			}
			shape := "round"
			switch strings.Trim(m[4], "<>") {
			case "choice":
				shape = "diamond"
			case "fork", "join":
				shape = "rect"
			}
			node := g.stateNode(id, "decl", shape)
			if label != "" {
				g.nodes[node].label = label
			}
			if m[5] == "{" { // a composite state: draw its region as a group
				if len(stack) >= 8 {
					return nil, errors.New("状态嵌套超过 8 层，显示源码")
				}
				if len(g.groups) >= 40 {
					return nil, errors.New("状态图超过 40 个复合状态，显示源码")
				}
				index := g.addGroup(id, g.nodes[node].label)
				if len(stack) > 0 {
					parent := g.groups[stack[len(stack)-1]]
					g.groups[index].parent, g.groups[index].depth = stack[len(stack)-1], parent.depth+1
					parent.children = append(parent.children, index)
				}
				stack = append(stack, index)
				g.group = index
			}
			continue
		case lower == "end", lower == "endif", lower == "endregion":
			return nil, errors.New("状态图不支持该块语句，显示源码")
		}
		for _, prefix := range stateSkipped {
			if lower == strings.TrimSpace(prefix) || strings.HasPrefix(lower, prefix) {
				text = ""
				break
			}
		}
		if text == "" {
			continue
		}
		if m := stateTransition.FindStringSubmatch(text); m != nil {
			event := ""
			right := strings.TrimSpace(m[2])
			if at := strings.Index(right, ":"); at >= 0 {
				right, event = strings.TrimSpace(right[:at]), cleanMermaidLabel(right[at+1:])
			}
			from, to := g.stateNode(m[1], "from", "round"), g.stateNode(right, "to", "round")
			g.edges = append(g.edges, mmEdge{from: from, to: to, label: event, style: '-'})
			if len(g.nodes) > 150 || len(g.edges) > 400 {
				return nil, errors.New("状态图过大，显示源码")
			}
			continue
		}
		if at := strings.Index(text, ":"); at >= 0 { // S1 : 描述
			node := g.stateNode(text[:at], "decl", "round")
			desc := cleanMermaidLabel(text[at+1:])
			if desc != "" {
				if n := g.nodes[node]; n.label == n.id {
					n.label = n.id + "\n" + desc
				} else if !strings.Contains(n.label, desc) {
					n.label += "\n" + desc
				}
			}
			continue
		}
		return nil, fmt.Errorf("无法解析状态图语句：%s", text)
	}
	if len(stack) != 0 {
		return nil, errors.New("状态图的复合状态没有用 } 闭合，显示源码")
	}
	if len(g.nodes) == 0 {
		return nil, errors.New("状态图没有状态")
	}
	if len(g.nodes) > 150 || len(g.edges) > 400 {
		return nil, errors.New("状态图过大，显示源码")
	}
	return g, nil
}

var stateSkipped = []string{"classdef ", "class ", "style ", "linkstyle ", "click ", "direction ", "accTitle", "accDescr", "hide "}

// stateNode resolves a state's name, honouring a quoted label and a stereotype.
// A `[*]` endpoint becomes the shared initial dot or final ring.
func (g *mmFlow) stateNode(raw, role, shape string) int {
	name := strings.TrimSpace(raw)
	if name == "[*]" {
		if role == "from" {
			return g.addState(stateStart, "", "start")
		}
		return g.addState(stateEnd, "", "end")
	}
	return g.addState(name, name, shape)
}

func (g *mmFlow) addState(id, label, shape string) int {
	if index, ok := g.index[id]; ok {
		if shape == "diamond" || shape == "rect" {
			g.nodes[index].shape = shape
		}
		return index
	}
	node := &mmNode{id: id, label: label, shape: shape, group: -1}
	index := len(g.nodes)
	g.index[id] = index
	g.nodes = append(g.nodes, node)
	// A state written inside a composite region belongs to that region.
	if g.group >= 0 {
		node.group = g.group
		g.groups[g.group].nodes = append(g.groups[g.group].nodes, index)
	}
	return index
}

// ---- sequence diagrams ----

type mmParticipant struct {
	id, label string
	actor     bool
	w, h, x   float32
}

type mmStep struct {
	kind          string // message | note | open | divider | close | frame | activate | deactivate
	from, to      int
	text          string
	dashed        bool
	head          byte // '>' filled, ')' open, 'x' cross, '-' line
	sign          byte // '+' activates the receiver, '-' deactivates it
	placement     string
	y, tw, th, h  float32
	block         string
	number        int
	frameTop      float32
	frameDividers []float32
	color         ui.Color // a rect block's background
	hasColor      bool
}

// mmActivation is one `activate` bar on a lifeline. level nests from 0.
type mmActivation struct {
	participant, level int
	top, bottom        float32
}

type mmSequence struct {
	participants  []*mmParticipant
	index         map[string]int
	steps         []*mmStep
	activations   []*mmActivation
	width, height float32
	numbered      bool
}

var seqMessage = regexp.MustCompile(`^(.+?)\s*(-{1,2})(>>|>|x|\))\s*([+-]?)\s*([^:]+?)\s*(?::\s*(.*))?$`)
var seqNote = regexp.MustCompile(`(?i)^note\s+(left of|right of|over)\s+([^:]+?)\s*:\s*(.*)$`)
var seqRectColor = regexp.MustCompile(`(?i)^rgba?\s*\(([^)]*)\)$`)

// parseRectColor reads the `rgb(…)`, `rgba(…)` or `#rrggbb` color a `rect`
// block paints behind its messages. Only colors are read here; nothing that
// could name a file or a command reaches the painter.
func parseRectColor(spec string) (ui.Color, bool) {
	spec = strings.TrimSpace(spec)
	if m := seqRectColor.FindStringSubmatch(spec); m != nil {
		parts := strings.Split(m[1], ",")
		if len(parts) < 3 || len(parts) > 4 {
			return ui.Color{}, false
		}
		var v [4]float64
		v[3] = 1
		for i, part := range parts {
			f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
				return ui.Color{}, false
			}
			if i == 3 {
				if f > 1 {
					return ui.Color{}, false
				}
				v[3] = f
				continue
			}
			if f > 255 {
				return ui.Color{}, false
			}
			v[i] = f
		}
		return ui.RGBA(uint8(v[0]), uint8(v[1]), uint8(v[2]), float32(v[3])), true
	}
	if strings.HasPrefix(spec, "#") {
		hex := spec[1:]
		if len(hex) != 3 && len(hex) != 6 {
			return ui.Color{}, false
		}
		for _, r := range hex {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return ui.Color{}, false
			}
		}
		return ui.Hex(spec), true
	}
	return ui.Color{}, false
}

func parseSequence(code string) (*mmSequence, error) {
	_, statements := mermaidStatements(code)
	s := &mmSequence{index: map[string]int{}}
	participant := func(id string) int {
		id = strings.TrimSpace(id)
		if i, ok := s.index[id]; ok {
			return i
		}
		s.index[id] = len(s.participants)
		s.participants = append(s.participants, &mmParticipant{id: id, label: id})
		return len(s.participants) - 1
	}
	number, depth := 0, 0
	for _, line := range statements {
		if len(s.participants) > 20 || len(s.steps) > 300 {
			return nil, errors.New("时序图过大，显示源码")
		}
		lower := strings.ToLower(line)
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch {
		case lower == "autonumber" || strings.HasPrefix(lower, "autonumber "):
			s.numbered = true
		case strings.HasPrefix(lower, "actor ") || strings.HasPrefix(lower, "participant "):
			// `actor Alice as 用户` draws the stick figure, `participant` a box.
			actor := strings.HasPrefix(lower, "actor ")
			rest := strings.TrimSpace(line[len(fields[0]):])
			id, label := rest, rest
			if at := strings.Index(rest, " as "); at >= 0 {
				id, label = strings.TrimSpace(rest[:at]), strings.TrimSpace(rest[at+4:])
			}
			i := participant(cleanMermaidLabel(id))
			s.participants[i].label = cleanMermaidLabel(label)
			s.participants[i].actor = actor
		case strings.HasPrefix(lower, "activate ") || strings.HasPrefix(lower, "deactivate "):
			kind := "activate"
			if strings.HasPrefix(lower, "deactivate ") {
				kind = "deactivate"
			}
			step := &mmStep{kind: kind, from: participant(strings.TrimSpace(line[len(fields[0]):]))}
			step.to = step.from
			s.steps = append(s.steps, step)
		case strings.HasPrefix(lower, "create ") || strings.HasPrefix(lower, "destroy ") ||
			strings.HasPrefix(lower, "box") || strings.HasPrefix(lower, "title"):
			return nil, errors.New("时序图的生命周期、box 与 title 语法暂以源码展示")
		case len(fields) > 0 && (strings.EqualFold(fields[0], "loop") || strings.EqualFold(fields[0], "alt") || strings.EqualFold(fields[0], "opt") ||
			strings.EqualFold(fields[0], "par") || strings.EqualFold(fields[0], "critical") || strings.EqualFold(fields[0], "break")):
			if depth >= 8 {
				return nil, errors.New("时序图块嵌套超过 8 层，显示源码")
			}
			depth++
			s.steps = append(s.steps, &mmStep{kind: "open", block: strings.ToLower(fields[0]), text: cleanMermaidLabel(strings.TrimSpace(line[len(fields[0]):]))})
		case strings.HasPrefix(lower, "rect"):
			// A tinted background region: `rect rgb(200, 220, 255)` … `end`
			if depth >= 8 {
				return nil, errors.New("时序图块嵌套超过 8 层，显示源码")
			}
			depth++
			color, ok := parseRectColor(strings.TrimSpace(line[len(fields[0]):]))
			if !ok {
				return nil, fmt.Errorf("无法解析时序图 rect 背景色：%s", line)
			}
			s.steps = append(s.steps, &mmStep{kind: "open", block: "rect", color: color, hasColor: true})
		case strings.EqualFold(fields[0], "else") || strings.EqualFold(fields[0], "and") || strings.EqualFold(fields[0], "option"):
			if depth == 0 {
				return nil, errors.New("时序图分支缺少起始块")
			}
			s.steps = append(s.steps, &mmStep{kind: "divider", block: strings.ToLower(fields[0]), text: cleanMermaidLabel(strings.TrimSpace(line[len(fields[0]):]))})
		case lower == "end":
			if depth == 0 {
				return nil, errors.New("时序图 end 没有对应起始块")
			}
			depth--
			s.steps = append(s.steps, &mmStep{kind: "close"})
		default:
			if m := seqNote.FindStringSubmatch(line); m != nil {
				ids := strings.Split(m[2], ",")
				step := &mmStep{kind: "note", placement: strings.ToLower(m[1]), from: participant(ids[0]), text: cleanMermaidLabel(m[3])}
				step.to = step.from
				if len(ids) > 1 {
					step.to = participant(ids[1])
				}
				s.steps = append(s.steps, step)
				continue
			}
			m := seqMessage.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("无法解析时序图语句：%s", line)
			}
			number++
			step := &mmStep{kind: "message", from: participant(m[1]), to: participant(m[5]), dashed: m[2] == "--", text: cleanMermaidLabel(m[6]), number: number}
			if m[4] != "" {
				step.sign = m[4][0] // `->>+` activates, `-->>-` deactivates the receiver
			}
			switch m[3] {
			case ">>":
				step.head = '>'
			case ">":
				step.head = '-'
			default:
				step.head = m[3][0]
			}
			s.steps = append(s.steps, step)
		}
	}
	if depth != 0 {
		return nil, errors.New("时序图块未闭合，显示源码")
	}
	if len(s.participants) == 0 {
		return nil, errors.New("时序图没有参与者")
	}
	if len(s.participants) > 20 || len(s.steps) > 300 {
		return nil, errors.New("时序图过大，显示源码")
	}
	return s, nil
}

func (s *mmSequence) layout(measure func(string, float32) (float32, float32)) {
	for _, p := range s.participants {
		tw, th := measure(p.label, 160)
		p.w, p.h = max(tw+24, 72), th+16
		if p.actor {
			p.w += 14 // room for the stick figure beside the name
		}
	}
	gaps := make([]float32, len(s.participants))
	for i := 1; i < len(s.participants); i++ {
		gaps[i] = (s.participants[i-1].w+s.participants[i].w)/2 + 36
	}
	for _, step := range s.steps {
		text := step.text
		if s.numbered && step.kind == "message" {
			text = strconv.Itoa(step.number) + ". " + text
		}
		step.tw, step.th = measure(text, 260)
		if step.kind != "message" {
			continue
		}
		a, b := min(step.from, step.to), max(step.from, step.to)
		if a == b {
			if b+1 < len(gaps) {
				gaps[b+1] = max(gaps[b+1], step.tw+50)
			}
			continue
		}
		need := step.tw + 30
		have := float32(0)
		for i := a + 1; i <= b; i++ {
			have += gaps[i]
		}
		if have < need {
			gaps[b] += need - have
		}
	}
	x := mmMargin + s.participants[0].w/2 + 10
	for i, p := range s.participants {
		x += gaps[i]
		p.x = x
	}
	headH := float32(0)
	for _, p := range s.participants {
		headH = max(headH, p.h)
	}
	y := mmMargin + headH + 18
	var open []*mmStep
	// Activation bars open on `activate` (or `->>+`) and close on
	// `deactivate` (or `-->>-`); a bar left open runs to the bottom.
	var bars []*mmActivation
	active := make([][]*mmActivation, len(s.participants))
	activate := func(p int, top float32) {
		if p < 0 || p >= len(active) {
			return
		}
		bar := &mmActivation{participant: p, level: len(active[p]), top: top}
		active[p] = append(active[p], bar)
		bars = append(bars, bar)
	}
	deactivate := func(p int, bottom float32) {
		if p < 0 || p >= len(active) || len(active[p]) == 0 {
			return
		}
		stack := active[p]
		stack[len(stack)-1].bottom = max(stack[len(stack)-1].top+4, bottom)
		active[p] = stack[:len(stack)-1]
	}
	// `deactivate` may name a participant that was never activated; that is a
	// source error, not a reason to stop drawing.
	for _, step := range s.steps {
		switch step.kind {
		case "message":
			step.y = y + step.th + 4
			step.h = step.th + 26
			if step.from == step.to {
				step.h += 24
			}
			if step.sign == '+' {
				activate(step.to, step.y)
			} else if step.sign == '-' {
				deactivate(step.to, step.y)
			}
		case "note":
			step.y = y + 4
			step.h = step.th + 20
		case "open":
			step.y = y
			step.frameTop = y
			step.h = step.th + 16
			open = append(open, step)
		case "divider":
			step.y = y
			step.h = step.th + 14
			if len(open) > 0 {
				top := open[len(open)-1]
				top.frameDividers = append(top.frameDividers, y)
			}
		case "activate":
			step.y = y
			step.h = 0
			activate(step.from, y)
		case "deactivate":
			step.y = y
			step.h = 0
			deactivate(step.from, y)
		case "close":
			step.y = y
			step.h = 10
			if len(open) > 0 {
				top := open[len(open)-1]
				top.h = y + 4 - top.frameTop // the frame's whole height
				top.kind = "frame"
				open = open[:len(open)-1]
			}
		}
		y += step.h
	}
	for p := range active {
		for _, bar := range active[p] {
			bar.bottom = max(bar.top+4, y)
		}
	}
	s.activations = bars
	last := s.participants[len(s.participants)-1]
	s.width = last.x + last.w/2 + mmMargin + 60
	s.height = y + mmMargin + 6
}

func (s *mmSequence) paint(painter *ui.Painter, r ui.Rect, colors mmColors) {
	ox, oy := r.X, r.Y
	left := ox + mmMargin
	right := ox + s.width - mmMargin
	// Tinted `rect` regions sit behind everything else.
	for _, step := range s.steps {
		if step.kind != "frame" || !step.hasColor {
			continue
		}
		region := ui.Rect{X: left, Y: oy + step.frameTop, W: right - left, H: step.h}
		painter.Fill(region, step.color.Alpha(0.45), 3)
		painter.Stroke(region, step.color.Alpha(0.85), 3, 1)
	}
	for _, p := range s.participants {
		x := ox + p.x
		path := (&ui.Path{}).MoveTo(x, oy+mmMargin+p.h).LineTo(x, oy+s.height-mmMargin)
		path.Dash(0, 4, 4)
		painter.StrokePath(path, 1, colors.stroke)
	}
	for _, bar := range s.activations {
		p := s.participants[bar.participant]
		x := ox + p.x - 5 + float32(bar.level)*8
		box := ui.Rect{X: x, Y: oy + bar.top, W: 10, H: max(bar.bottom-bar.top, 4)}
		painter.Fill(box, colors.labelBG, 2)
		painter.Stroke(box, colors.stroke, 2, 1.2)
	}
	for _, step := range s.steps {
		switch step.kind {
		case "frame":
			top := oy + step.frameTop
			frame := ui.Rect{X: left, Y: top, W: right - left, H: step.h}
			if step.hasColor { // a rect region only separates, it needs no tag
				for _, y := range step.frameDividers {
					painter.Line(left, oy+y, right, oy+y, 1, colors.stroke)
				}
				continue
			}
			painter.Stroke(frame, colors.stroke, 4, 1)
			tag := strings.ToUpper(step.block)
			tagW, _ := painter.MeasureText(0, ui.Span{Text: tag, Size: mmFont - 2, Weight: 700})
			painter.Fill(ui.Rect{X: left, Y: top, W: tagW + 14, H: 18}, colors.stroke.Alpha(0.35), 4)
			painter.RichText(left+7, top+2, 0, ui.Span{Text: tag, Size: mmFont - 2, Weight: 700, Color: colors.text})
			if step.text != "" {
				painter.RichText(left+tagW+22, top+2, 260, ui.Span{Text: "[" + step.text + "]", Size: mmFont - 1, Color: colors.edge})
			}
			for _, y := range step.frameDividers {
				path := (&ui.Path{}).MoveTo(left, oy+y).LineTo(right, oy+y)
				path.Dash(0, 4, 3)
				painter.StrokePath(path, 1, colors.stroke)
			}
		case "divider":
			label := step.text
			if label == "" {
				label = strings.ToLower(step.block) // `and` alone still marks the branch
			}
			if label != "" {
				painter.RichText(left+8, oy+step.y+2, 260, ui.Span{Text: "[" + label + "]", Size: mmFont - 1, Color: colors.edge})
			}
		case "note":
			a, b := s.participants[step.from], s.participants[step.to]
			w := step.tw + 16
			var x float32
			switch step.placement {
			case "left of":
				x = a.x - w - 10
			case "right of":
				x = a.x + 10
			default:
				x = (a.x+b.x)/2 - w/2
				if a != b {
					w = max(w, float32(math.Abs(float64(b.x-a.x)))+40)
					x = min(a.x, b.x) - 20
				}
			}
			box := ui.Rect{X: ox + x, Y: oy + step.y, W: w, H: step.th + 12}
			painter.Fill(box, colors.fill, 4)
			painter.Stroke(box, colors.stroke, 4, 1)
			painter.RichText(box.X+8, box.Y+6, 260, ui.Span{Text: step.text, Size: mmFont - 1, Color: colors.text})
		case "message":
			s.paintMessage(painter, ox, oy, step, colors)
		}
	}
	for _, p := range s.participants {
		box := ui.Rect{X: ox + p.x - p.w/2, Y: oy + mmMargin, W: p.w, H: p.h}
		painter.Fill(box, colors.fill, 6)
		width := float32(1.2)
		if p.actor {
			width = 1.8
		}
		painter.Stroke(box, colors.stroke, 6, width)
		labelBox := box
		if p.actor {
			drawActorIcon(painter, box.X+13, box.Y+box.H/2, colors.edge)
			labelBox = ui.Rect{X: box.X + 24, Y: box.Y, W: box.W - 24, H: box.H}
		}
		tw, th := painter.MeasureText(160, ui.Span{Text: p.label, Size: mmFont})
		painter.RichText(labelBox.X+(labelBox.W-tw)/2, box.Y+(p.h-th)/2, 160, ui.Span{Text: p.label, Size: mmFont, Color: colors.text, Weight: 600})
	}
}

// drawActorIcon marks an `actor` participant with a stick figure, so it is not
// mistaken for a plain participant box.
func drawActorIcon(painter *ui.Painter, cx, cy float32, color ui.Color) {
	painter.FillPath((&ui.Path{}).Circle(cx, cy-5, 3), color)
	painter.Line(cx, cy-2, cx, cy+2, 1.4, color)
	painter.Line(cx-4, cy, cx+4, cy, 1.4, color)
	painter.Line(cx, cy+2, cx-3.5, cy+6, 1.4, color)
	painter.Line(cx, cy+2, cx+3.5, cy+6, 1.4, color)
}

func (s *mmSequence) paintMessage(painter *ui.Painter, ox, oy float32, step *mmStep, colors mmColors) {
	a, b := s.participants[step.from], s.participants[step.to]
	y := oy + step.y
	text := step.text
	if s.numbered {
		text = strconv.Itoa(step.number) + ". " + text
	}
	var path *ui.Path
	endX, endY, fromX, fromY := ox+b.x, y, ox+a.x, y
	if a == b {
		x := ox + a.x
		path = (&ui.Path{}).MoveTo(x, y).LineTo(x+36, y).LineTo(x+36, y+22).LineTo(x+4, y+22)
		endX, endY, fromX, fromY = x+2, y+22, x+36, y+22
		painter.RichText(x+44, y-step.th+2, 260, ui.Span{Text: text, Size: mmFont - 1, Color: colors.text})
	} else {
		dir := float32(1)
		if b.x < a.x {
			dir = -1
		}
		endX = ox + b.x - dir*2
		path = (&ui.Path{}).MoveTo(ox+a.x, y).LineTo(endX, y)
		mid := ox + (a.x+b.x)/2
		painter.RichText(mid-step.tw/2, y-step.th-3, 260, ui.Span{Text: text, Size: mmFont - 1, Color: colors.text})
	}
	if step.dashed {
		path.Dash(0, 5, 3)
	}
	painter.StrokePath(path, 1.3, colors.edge)
	switch step.head {
	case '>':
		paintEdgeEnd(painter, endX, endY, fromX, fromY, '>', 1.3, colors)
	case ')', '-':
		dx := float32(1)
		if endX < fromX {
			dx = -1
		}
		painter.Line(endX, endY, endX-dx*8, endY-5, 1.3, colors.edge)
		painter.Line(endX, endY, endX-dx*8, endY+5, 1.3, colors.edge)
	case 'x':
		paintEdgeEnd(painter, endX, endY, fromX, fromY, 'x', 1.3, colors)
	}
}

func (s *mmSequence) describe() string {
	var parts []string
	for _, step := range s.steps {
		if step.kind == "message" {
			parts = append(parts, s.participants[step.from].label+" → "+s.participants[step.to].label+"："+step.text)
		}
	}
	return "时序图：" + strings.Join(parts, "；")
}

// ---- pie ----

var pieSlice = regexp.MustCompile(`^"([^"]*)"\s*:\s*([-+0-9.eE]+)$`)

func parseMermaidPie(code string) (*chartSpec, error) {
	header, statements := mermaidStatements(code)
	spec := &chartSpec{Type: "pie"}
	if rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(header, "pie")), "showData")); strings.HasPrefix(strings.ToLower(rest), "title ") {
		spec.Title = strings.TrimSpace(rest[6:])
	}
	var values []float64
	for _, line := range statements {
		if strings.HasPrefix(strings.ToLower(line), "title ") {
			spec.Title = strings.TrimSpace(line[6:])
			continue
		}
		m := pieSlice.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("无法解析饼图语句：%s", line)
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, errors.New("饼图需要有限的非负数值")
		}
		spec.Labels = append(spec.Labels, m[1])
		values = append(values, v)
	}
	if len(values) == 0 {
		return nil, errors.New("饼图没有数据")
	}
	spec.Series = []chartSeries{{Name: spec.Title, Data: chartValues(values)}}
	return spec, nil
}
