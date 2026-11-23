package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
)

// A chart reply is a fenced ```chart block of JSON that the desktop draws
// natively with ui/chart, following the window's theme. It is the native
// counterpart of an HTML visual reply: the model sends data, the client owns
// the look, colors, hover and accessibility.
type chartSpec struct {
	Type        string         `json:"type"`
	Title       string         `json:"title"`
	Subtitle    string         `json:"subtitle"`
	Hero        string         `json:"hero"`
	HeroLabel   string         `json:"heroLabel"`
	Badge       string         `json:"badge"`
	BadgeTone   string         `json:"badgeTone"` // success | warning | danger | accent | neutral
	Delta       string         `json:"delta"`
	Style       string         `json:"style"` // dots (default heatmap) | tiles
	Labels      []string       `json:"labels"`
	X           []string       `json:"x"`
	Series      []chartSeries  `json:"series"`
	Values      chartValues    `json:"values"`
	Unit        string         `json:"unit"`
	UnitRight   string         `json:"unitRight"`
	XType       string         `json:"xType"`
	Stack       string         `json:"stack"`
	CenterLabel string         `json:"centerLabel"`
	RefLines    []chartRefLine `json:"refLines"`
	Height      float32        `json:"height"`
	Items       []chartStat    `json:"items"`
	Highlights  []bool         `json:"-"`
}

type chartSeries struct {
	Name        string       `json:"name"`
	AccessLabel string       `json:"-"`
	Subtitle    string       `json:"subtitle"`
	Badge       string       `json:"badge"`
	Data        chartValues  `json:"data"`
	Sparkline   chartValues  `json:"sparkline"`
	Points      []chartPoint `json:"points"`
	Kind        string       `json:"kind"` // bar | line | area (combo)
	Axis        string       `json:"axis"` // left | right (combo)
}

type chartPoint struct {
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	R     float64 `json:"r"`
}

type chartRefLine struct {
	Value float64 `json:"value"`
	Label string  `json:"label"`
	Tone  string  `json:"tone"` // warning | danger | success
}

// chartValues reads null as a missing value (NaN), not as zero.
type chartValues []float64

func (v *chartValues) UnmarshalJSON(data []byte) error {
	var raw []*float64
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*v = make(chartValues, len(raw))
	for i, x := range raw {
		(*v)[i] = math.NaN()
		if x != nil {
			(*v)[i] = *x
		}
	}
	return nil
}

type chartStat struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Delta string `json:"delta"`
	Note  string `json:"note"`
}

var chartTypes = map[string]bool{
	"bar":        true,
	"stacked":    true,
	"stacked100": true,
	"hbar":       true,
	"line":       true,
	"area":       true,
	"combo":      true,
	"scatter":    true,
	"bubble":     true,
	"heatmap":    true,
	"breakdown":  true,
	"pie":        true,
	"donut":      true,
	"stats":      true,
}

func parseChartSpec(code string) (*chartSpec, error) {
	return parseChartSpecWithInfo("", code)
}

func parseChartSpecWithInfo(info, code string) (*chartSpec, error) {
	if len(code) > 128<<10 {
		return nil, errors.New("图表数据超过 128 KiB，请拆分展示")
	}
	trimmed := strings.TrimSpace(code)
	if trimmed != "" && trimmed[0] != '{' {
		if spec, err := parsePipeChartSpec(info, trimmed); err == nil {
			return spec, nil
		}
	}
	var spec chartSpec
	decoder := json.NewDecoder(strings.NewReader(code))
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("图表数据不是有效的 JSON 或表格：%v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("图表需要单个 JSON 对象")
	}
	return validateChartSpec(&spec)
}

// parsePipeChartSpec parses answer-me-with-html's compact pipe-table chart DSL:
//
//	```chart bar [horizontal] [stacked] [unit=ms] [title="延迟对比"]
//	           | p50 | p95
//	* Redis    | 12  | 40
//	Memcached  | 9   | 31
//	```
func parsePipeChartSpec(info, body string) (*chartSpec, error) {
	attrs, words := parseFenceInfoAttrs(info)
	kind := "bar"
	horizontal, stacked := false, false
	for _, w := range words {
		switch strings.ToLower(w) {
		case "chart", "vantry-chart":
		case "bar", "bars":
			kind = "bar"
		case "hbar", "horizontal":
			horizontal = true
		case "stacked", "stack":
			stacked = true
		case "stacked100":
			kind = "stacked100"
		case "line", "lines":
			kind = "line"
		case "area":
			kind = "area"
		case "heatmap":
			kind = "heatmap"
		case "combo":
			kind = "combo"
		case "scatter":
			kind = "scatter"
		case "bubble":
			kind = "bubble"
		case "pie":
			kind = "pie"
		case "donut":
			kind = "donut"
		}
	}
	if t := strings.ToLower(attrs["type"]); chartTypes[t] {
		kind = t
	}
	var lines []string
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		lines = append(lines, raw)
	}
	if len(lines) == 0 {
		return nil, errors.New("图表内容为空")
	}
	spec := &chartSpec{
		Type:        kind,
		Title:       attrs["title"],
		Subtitle:    attrs["subtitle"],
		Unit:        attrs["unit"],
		UnitRight:   attrs["unitright"],
		XType:       attrs["xtype"],
		Stack:       attrs["stack"],
		CenterLabel: attrs["centerlabel"],
	}
	if h, err := strconv.ParseFloat(attrs["height"], 32); err == nil && h > 0 {
		spec.Height = float32(h)
	}
	if strings.HasPrefix(strings.TrimSpace(lines[0]), "#") && !strings.Contains(lines[0], "|") {
		spec.Title = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[0]), "#"))
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return nil, errors.New("图表缺少数据行")
	}
	var seriesNames []string
	firstParts := splitPipeFields(lines[0])
	if len(firstParts) >= 2 && strings.TrimSpace(firstParts[0]) == "" {
		for _, name := range firstParts[1:] {
			seriesNames = append(seriesNames, strings.TrimSpace(name))
		}
		lines = lines[1:]
	}
	var rows [][]float64
	maxCols := len(seriesNames)
	for _, line := range lines {
		parts := splitPipeFields(line)
		if len(parts) < 2 {
			return nil, fmt.Errorf("图表行缺少 '|' 分隔：%q", strings.TrimSpace(line))
		}
		label := strings.TrimSpace(parts[0])
		hi := false
		if strings.HasPrefix(label, "*") {
			hi = true
			label = strings.TrimSpace(strings.TrimPrefix(label, "*"))
		}
		if label == "" || strings.Trim(label, "-:") == "" {
			continue
		}
		vals := make([]float64, len(parts)-1)
		for i, rawVal := range parts[1:] {
			cell := strings.TrimSpace(rawVal)
			if spec.Unit != "" {
				cell = strings.TrimSpace(strings.TrimSuffix(cell, spec.Unit))
			}
			cell = strings.ReplaceAll(cell, ",", "")
			if cell == "" || cell == "-" || cell == "—" || strings.EqualFold(cell, "null") || strings.EqualFold(cell, "na") {
				vals[i] = math.NaN()
				continue
			}
			num, err := strconv.ParseFloat(strings.TrimSuffix(cell, "%"), 64)
			if err != nil {
				if len(spec.Labels) == 0 && len(seriesNames) == 0 {
					// Treat a titled header row like "数据库 | p50 | p95" as series names.
					for _, name := range parts[1:] {
						seriesNames = append(seriesNames, strings.TrimSpace(name))
					}
					maxCols = max(maxCols, len(seriesNames))
					vals = nil
					break
				}
				return nil, fmt.Errorf("无法解析图表数值 %q", rawVal)
			}
			vals[i] = num
		}
		if vals == nil {
			continue
		}
		spec.Labels = append(spec.Labels, label)
		spec.Highlights = append(spec.Highlights, hi)
		rows = append(rows, vals)
		if len(vals) > maxCols {
			maxCols = len(vals)
		}
	}
	if maxCols == 0 || len(rows) == 0 {
		return nil, errors.New("图表缺少有效数值行")
	}
	spec.Series = make([]chartSeries, maxCols)
	for c := 0; c < maxCols; c++ {
		name := ""
		if c < len(seriesNames) {
			name = seriesNames[c]
		} else if maxCols == 1 {
			name = spec.Title
		} else {
			name = fmt.Sprintf("系列 %d", c+1)
		}
		data := make(chartValues, len(rows))
		for r := range rows {
			if c < len(rows[r]) {
				data[r] = rows[r][c]
			} else {
				data[r] = math.NaN()
			}
		}
		spec.Series[c] = chartSeries{Name: name, Data: data}
	}
	if stacked && spec.Type == "bar" {
		if strings.EqualFold(spec.Stack, "percent") {
			spec.Type = "stacked100"
		} else {
			spec.Type = "stacked"
		}
	} else if horizontal && spec.Type == "bar" {
		spec.Type = "hbar"
	}
	return validateChartSpec(spec)
}

func validateChartSpec(spec *chartSpec) (*chartSpec, error) {
	spec.Type = strings.ToLower(strings.TrimSpace(spec.Type))
	if spec.Type == "" {
		spec.Type = "bar"
	}
	if spec.Type == "stacked" && strings.EqualFold(spec.Stack, "percent") {
		spec.Type = "stacked100"
	}
	if !chartTypes[spec.Type] {
		return nil, fmt.Errorf("不支持的图表类型 %q", spec.Type)
	}
	if len(spec.Labels) == 0 {
		spec.Labels = spec.X
	}
	if len(spec.Series) == 0 && len(spec.Values) > 0 {
		spec.Series = []chartSeries{{Name: spec.Title, Data: spec.Values}}
	}
	switch spec.Type {
	case "stats":
		if len(spec.Items) == 0 || len(spec.Items) > 24 {
			return nil, errors.New("指标卡需要 1–24 个 items")
		}
		return spec, nil
	case "scatter", "bubble":
		if len(spec.Series) == 0 {
			return nil, errors.New("散点图/气泡图需要 series.points 数据")
		}
		if len(spec.Series) > 12 {
			return nil, errors.New("单图最多显示 12 个系列")
		}
		totalPoints := 0
		for sIdx := range spec.Series {
			s := &spec.Series[sIdx]
			if len(s.Points) == 0 && len(s.Data) > 0 && len(spec.Labels) == len(s.Data) {
				for i, raw := range spec.Labels {
					xv, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
					if err != nil || math.IsNaN(s.Data[i]) {
						continue
					}
					s.Points = append(s.Points, chartPoint{Label: raw, X: xv, Y: s.Data[i], R: 1})
				}
			}
			for pIdx := range s.Points {
				pt := &s.Points[pIdx]
				if math.IsNaN(pt.X) || math.IsInf(pt.X, 0) || math.IsNaN(pt.Y) || math.IsInf(pt.Y, 0) {
					return nil, errors.New("散点坐标包含无效数字")
				}
				if spec.Type == "bubble" {
					if math.IsNaN(pt.R) || math.IsInf(pt.R, 0) || pt.R < 0 {
						return nil, errors.New("气泡半径 r 不能为负数或无效值")
					}
					if pt.R == 0 {
						pt.R = 1
					}
				}
				totalPoints++
			}
		}
		if totalPoints == 0 {
			return nil, errors.New("散点图缺少有效坐标点")
		}
		if totalPoints > 2000 {
			return nil, errors.New("散点数量超过 2000 个")
		}
		if spec.Height <= 0 {
			spec.Height = 240
		}
		spec.Height = min(max(spec.Height, 140), 480)
		return spec, nil
	case "breakdown":
		if len(spec.Series) == 0 {
			return nil, errors.New("分布排行图需要 series 数据")
		}
		if len(spec.Series) > 12 {
			return nil, errors.New("单图最多显示 12 个系列")
		}
		hasPositive := false
		for idx := range spec.Series {
			s := &spec.Series[idx]
			if len(s.Data) == 0 && len(s.Sparkline) > 0 {
				sum := 0.0
				for _, v := range s.Sparkline {
					if !math.IsNaN(v) {
						sum += v
					}
				}
				s.Data = chartValues{sum}
			}
			if len(s.Data) == 0 {
				return nil, errors.New("分布排行图的每个系列都需要 data 数值")
			}
			for _, v := range s.Data {
				if !math.IsNaN(v) && v >= 0 {
					hasPositive = true
				}
			}
		}
		if !hasPositive {
			return nil, errors.New("分布排行图缺少有效正数值")
		}
		if len(spec.Labels) == 0 {
			for i := range spec.Series {
				spec.Labels = append(spec.Labels, seriesName(spec, i))
			}
		}
		if spec.Height <= 0 {
			spec.Height = 220
		}
		return spec, nil
	}
	if len(spec.Labels) == 0 || len(spec.Series) == 0 {
		return nil, errors.New("图表需要 labels 和 series")
	}
	if len(spec.Labels) > 2000 {
		return nil, errors.New("图表的类别超过 2000 个")
	}
	if len(spec.Series) > 12 {
		return nil, errors.New("单图最多显示 12 个系列")
	}
	if (spec.Type == "pie" || spec.Type == "donut") && len(spec.Series) != 1 {
		return nil, errors.New("饼图与环图需要单个系列，请拆分展示")
	}
	anyValue := false
	for _, series := range spec.Series {
		if len(series.Data) != len(spec.Labels) {
			return nil, errors.New("每个系列的数据数量必须与 labels 一致，缺失值请写 null")
		}
		for _, value := range series.Data {
			if math.IsNaN(value) {
				continue
			}
			if (spec.Type == "pie" || spec.Type == "donut") && value < 0 {
				return nil, errors.New("饼图与环图不能表示负数")
			}
			anyValue = true
		}
	}
	if !anyValue {
		return nil, errors.New("图表没有数值数据")
	}
	if spec.Height <= 0 {
		spec.Height = 220
	}
	spec.Height = min(max(spec.Height, 140), 480)
	return spec, nil
}

// visualCache keeps parsed specs and laid-out diagrams by their source, so a
// frame never parses again; it is bounded as the timeline is.
type visualCache[T any] struct {
	items map[string]T
	order []string
	limit int
}

func (v *visualCache[T]) get(key string, make func() T) T {
	if item, ok := v.items[key]; ok {
		return item
	}
	if v.items == nil {
		v.items = map[string]T{}
	}
	if v.limit == 0 {
		v.limit = 64
	}
	if len(v.order) >= v.limit {
		delete(v.items, v.order[0])
		v.order = v.order[1:]
	}
	item := make()
	v.items[key] = item
	v.order = append(v.order, key)
	return item
}

type parsedChart struct {
	spec *chartSpec
	err  error
}

// chartColors picks crisp studio hues tuned for both light and dark surfaces.
func chartColors(p palette) []ui.Color {
	if p.dark {
		return []ui.Color{ui.Hex("#F26D52"), ui.Hex("#34D399"), ui.Hex("#60A5FA"), ui.Hex("#FBBF24"), ui.Hex("#A78BFA"), ui.Hex("#22D3EE")}
	}
	return []ui.Color{ui.Hex("#E5482E"), ui.Hex("#16A34A"), ui.Hex("#3B6CE6"), ui.Hex("#D97706"), ui.Hex("#7C5CFA"), ui.Hex("#0891B2")}
}

func formatChartValue(v float64, unit string) string {
	if math.IsNaN(v) {
		return "—"
	}
	abs := math.Abs(v)
	var text string
	switch {
	case abs >= 1e8:
		text = strconv.FormatFloat(v/1e8, 'f', -1, 64)
		text = trimDecimals(text) + "亿"
	case abs >= 1e4:
		text = trimDecimals(strconv.FormatFloat(v/1e4, 'f', 2, 64)) + "万"
	case abs == math.Trunc(abs):
		text = strconv.FormatFloat(v, 'f', 0, 64)
	default:
		text = trimDecimals(strconv.FormatFloat(v, 'f', 2, 64))
	}
	return text + unit
}

func trimDecimals(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		end := min(len(s), i+3)
		s = strings.TrimRight(strings.TrimRight(s[:end], "0"), ".")
	}
	return s
}

type chartState struct {
	selected     int
	locked       int
	showTable    bool
	hiddenMask   uint32
	overrideType string
	exportOpen   bool
	hoverSlice   int
	lockedSlice  int
	hoverRow     int
	hoverCol     int
	lockedRow    int
	lockedCol    int
}

func hiddenMap(mask uint32) map[int]bool {
	if mask == 0 {
		return nil
	}
	m := make(map[int]bool, 4)
	for i := 0; i < 32; i++ {
		if mask&(1<<i) != 0 {
			m[i] = true
		}
	}
	return m
}

type chartDownsample struct {
	x, y []float32
}

// chartLayer retains per-chart downsampling buffers across hover frames; path
// masks themselves are cached by geometry hash in MyGo's shared MaskAtlas.
type chartLayer struct {
	ds *chartDownsample
}

func (l *chartLayer) paint(painter *ui.Painter, r ui.Rect, _ *chartSpec, _ bool, _ uint64, draw func(cp *ui.Painter, r ui.Rect)) {
	if r.W <= 0 || r.H <= 0 {
		return
	}
	draw(painter, r)
}

func visibleSeriesMask(spec *chartSpec, hidden map[int]bool, effType string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(spec.Series) && i < 32; i++ {
		if !hidden[i] {
			h |= 1 << i
		}
	}
	for i := 0; i < len(effType); i++ {
		h ^= uint64(effType[i])
		h *= 1099511628211
	}
	return h
}

func activeSeriesIndices(spec *chartSpec, hidden map[int]bool) []int {
	out := make([]int, 0, len(spec.Series))
	for i := range spec.Series {
		if !hidden[i] {
			out = append(out, i)
		}
	}
	if len(out) == 0 && len(spec.Series) > 0 {
		out = append(out, 0)
	}
	return out
}

func effectiveSpecForExport(spec *chartSpec, effType string, hidden map[int]bool) *chartSpec {
	cp := *spec
	cp.Type = effType
	if len(hidden) == 0 {
		return &cp
	}
	indices := activeSeriesIndices(spec, hidden)
	filtered := make([]chartSeries, 0, len(indices))
	for _, idx := range indices {
		filtered = append(filtered, spec.Series[idx])
	}
	cp.Series = filtered
	return &cp
}

func switchableChartTypes(spec *chartSpec) []string {
	switch spec.Type {
	case "bar", "stacked", "stacked100", "hbar", "line", "area":
		if len(spec.Series) <= 1 {
			return []string{"bar", "hbar", "line", "area"}
		}
		return []string{"bar", "stacked", "stacked100", "hbar", "line", "area"}
	default:
		return nil
	}
}

func nextSwitchableType(spec *chartSpec, cur string) string {
	choices := switchableChartTypes(spec)
	if len(choices) < 2 {
		return ""
	}
	for i, ch := range choices {
		if ch == cur {
			return choices[(i+1)%len(choices)]
		}
	}
	return choices[0]
}

func (w *workbench) chartViewWithInfo(c *ui.Context, p palette, info, code string) {
	cacheKey := info + "\x00" + code
	parsed := w.visuals.charts.get(cacheKey, func() parsedChart {
		spec, err := parseChartSpecWithInfo(info, code)
		return parsedChart{spec, err}
	})
	if parsed.err != nil {
		visualError(c, p, parsed.err.Error(), code)
		return
	}
	renderChart(c, p, parsed.spec, nil)
}

func (w *workbench) chartView(c *ui.Context, p palette, code string) {
	w.chartViewWithInfo(c, p, "", code)
}

// renderChart draws a chart card; actions, when given, sit in its header.
func renderChart(c *ui.Context, p palette, spec *chartSpec, actions func()) {
	renderChartWithOptions(c, p, spec, actions, nil)
}

func studioBadgeColors(p palette, tone string) (bg, fg ui.Color) {
	switch strings.ToLower(strings.TrimSpace(tone)) {
	case "success":
		return p.success.Alpha(0.13), p.success
	case "warning":
		return p.warning.Alpha(0.15), p.warning
	case "danger":
		return p.danger.Alpha(0.13), p.danger
	case "accent":
		return p.accent.Alpha(0.13), p.accentInk
	default:
		return p.soft, p.muted
	}
}

func renderChartWithOptions(c *ui.Context, p palette, spec *chartSpec, actions func(), onOpenCanvas func(*chartSpec)) {
	colors := chartColors(p)
	shadowCol := ui.RGBA(0, 0, 0, 0.045)
	if p.dark {
		shadowCol = ui.RGBA(0, 0, 0, 0.24)
	}
	box := ui.Column(c).FillWidth().Gap(12).Padding(18, 20).Radius(16).Background(p.raised).Border(1, p.lineSoft).Shadow(0, 2, 10, 0, shadowCol)
	box.Role(ui.RoleGroup).Label(nonEmptyString(spec.Title, "图表"))
	cardState := ui.Local(box, chartState{}, func() *chartState {
		return &chartState{selected: -1, locked: -1, hoverSlice: -1, lockedSlice: -1, hoverRow: -1, hoverCol: -1, lockedRow: -1, lockedCol: -1}
	})
	effType := spec.Type
	if (*cardState).overrideType != "" && chartTypes[(*cardState).overrideType] {
		effType = (*cardState).overrideType
	}
	hidden := hiddenMap((*cardState).hiddenMask)
	effSpec := effectiveSpecForExport(spec, effType, hidden)
	switchChoices := switchableChartTypes(spec)

	box.Children(func() {
		canTable := effType != "stats" && effType != "scatter" && effType != "bubble" && effType != "breakdown" && len(spec.Labels) >= 2
		canExport := len(spec.Series) > 0 || len(spec.Items) > 0
		hasHeader := spec.Title != "" || spec.Hero != "" || spec.Subtitle != "" || actions != nil || canTable || len(switchChoices) > 1 || canExport
		if hasHeader {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
					if spec.HeroLabel != "" || spec.Title != "" {
						eyebrow := nonEmptyString(spec.HeroLabel, spec.Title)
						titleSize := float32(13.5)
						if spec.Hero != "" {
							titleSize = 12
						}
						ui.Text(c, eyebrow).FontSize(titleSize).FontWeight(600).TextColor(p.ink)
					} else if effType != "stats" {
						ui.Text(c, chartKindBadge(effType, spec.Unit)).FontSize(11.5).FontWeight(500).TextColor(p.muted)
					}
					if spec.Hero == "" && spec.Subtitle != "" {
						ui.Text(c, spec.Subtitle).FontSize(11.5).TextColor(p.muted).Wrap()
					}
				})

				if len(switchChoices) > 1 {
					ui.Row(c.Key("type-pills")).Padding(2).Gap(2).Radius(9).Background(p.soft).AlignItems(ui.Center).Shrink(0).Children(func() {
						pillChoices := switchChoices
						if len(pillChoices) > 4 {
							pillChoices = []string{"bar", "stacked", "line", "area"}
						}
						for _, ch := range pillChoices {
							ch := ch
							active := ch == effType
							pb := ui.Button(c.Key("type-pill:"+ch), chartKindShort(ch)).Label("切换为"+chartKindBadge(ch, "")).
								Height(22).Padding(0, 8).Radius(7).Border(0, p.line).FontSize(11)
							if active {
								pb.Background(p.raised).TextColor(p.ink).FontWeight(600)
							} else {
								pb.Background(ui.Transparent).TextColor(p.muted)
							}
							if pb.Clicked() {
								(*cardState).overrideType = ch
								(*cardState).selected = -1
								(*cardState).locked = -1
							}
						}
					})
				}

				actionsRow := ui.Row(c).Gap(2).AlignItems(ui.Center).Shrink(0)
				shown := float32(0)
				if box.Hovered() || (*cardState).exportOpen || (*cardState).showTable {
					shown = 1
				}
				actionsRow.Opacity(actionsRow.AnimateTo("tools", shown, controlEase()))
				actionsRow.Children(func() {
					if canTable {
						tblLabel := "数据"
						if (*cardState).showTable {
							tblLabel = "收起数据"
						}
						if quietButton(c, p, "toggle-chart-table", tblLabel).Height(24).Padding(0, 7).FontSize(11).Clicked() {
							(*cardState).showTable = !(*cardState).showTable
						}
					}
					if canExport {
						expBtn := quietButton(c, p, "export-chart", "导出").Label("导出图表").Height(24).Padding(0, 7).FontSize(11).Expanded((*cardState).exportOpen)
						if expBtn.Clicked() {
							(*cardState).exportOpen = !(*cardState).exportOpen
						}
						ui.PopoverBase(c, expBtn, &(*cardState).exportOpen, func(panel ui.Element) {
							panel.Width(172).Padding(6).Radius(10).Background(p.raised).Border(1, p.line)
							ui.Column(c).Gap(2).FillWidth().Children(func() {
								if quietButton(c, p, "copy-csv", "复制 CSV").Height(26).FillWidth().Justify(ui.Start).Padding(0, 8).FontSize(11.5).Clicked() {
									c.WriteClipboard(chartSpecToCSV(effSpec))
									(*cardState).exportOpen = false
								}
								if quietButton(c, p, "save-csv", "保存 CSV…").Height(26).FillWidth().Justify(ui.Start).Padding(0, 8).FontSize(11.5).Clicked() {
									(*cardState).exportOpen = false
									saveTextWithDialog("保存图表 CSV", safeChartFilename(spec.Title, "chart")+".csv", chartSpecToCSV(effSpec))
								}
								if quietButton(c, p, "save-svg", "保存 SVG…").Height(26).FillWidth().Justify(ui.Start).Padding(0, 8).FontSize(11.5).Clicked() {
									(*cardState).exportOpen = false
									saveTextWithDialog("保存图表 SVG", safeChartFilename(spec.Title, "chart")+".svg", chartSpecToSVG(effSpec, p))
								}
							})
						})
					}
					if actions != nil {
						actions()
					}
				})
			})
		}

		if spec.Hero != "" {
			ui.Column(c.Key("chart-hero")).Gap(6).FillWidth().Children(func() {
				ui.Text(c, spec.Hero).FontSize(34).Bold().LetterSpacing(-0.7).FontFeatures("tnum").TextColor(p.ink)
				if spec.Badge != "" || spec.Delta != "" || spec.Subtitle != "" {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
						if spec.Badge != "" {
							tone := spec.BadgeTone
							if tone == "" {
								tone = "success"
							}
							bBg, bFg := studioBadgeColors(p, tone)
							ui.Text(c, spec.Badge).FontSize(11).FontWeight(600).TextColor(bFg).Padding(3, 8).Radius(7).Background(bBg).NoWrap()
						}
						if spec.Delta != "" {
							ui.Text(c, spec.Delta).FontSize(11.5).TextColor(p.muted).Wrap()
						}
						if spec.Subtitle != "" {
							ui.Text(c, spec.Subtitle).FontSize(11.5).TextColor(p.muted).Wrap()
						}
					})
				}
			})
		}

		if text, col, isLocked, showBar := chartReadoutText(p, spec, effType, colors, hidden, *cardState); showBar {
			ui.Row(c.Key("chart-readout")).FillWidth().Padding(5, 10).Gap(8).Radius(8).Background(p.soft).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(7, 7).Radius(3.5).Background(col).Shrink(0)
				ui.Text(c, text).FontSize(11).FontFeatures("tnum").TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…")
				if isLocked {
					if quietButton(c, p, "unlock-chart", "清除锁定 ×").Height(20).Padding(0, 6).FontSize(10.5).Clicked() {
						(*cardState).locked = -1
						(*cardState).lockedSlice = -1
						(*cardState).lockedRow = -1
						(*cardState).lockedCol = -1
					}
				}
			})
		}

		switch effType {
		case "stats":
			statCards(c, p, spec)
		case "pie", "donut":
			pieChart(c, p, effSpec, colors, &(*cardState).hoverSlice, &(*cardState).lockedSlice)
		case "line", "area":
			layer := ui.Local(box, chartLayer{}, func() *chartLayer { return &chartLayer{ds: &chartDownsample{}} })
			lineChart(c, p, spec, effType, colors, &(*cardState).selected, &(*cardState).locked, hidden, *layer)
		case "combo":
			layer := ui.Local(box, chartLayer{}, func() *chartLayer { return &chartLayer{ds: &chartDownsample{}} })
			comboChart(c, p, spec, colors, &(*cardState).selected, hidden, *layer)
		case "scatter", "bubble":
			scatterChart(c, p, spec, effType, colors, &(*cardState).selected, hidden)
		case "heatmap":
			heatmapChart(c, p, spec, p, &(*cardState).hoverRow, &(*cardState).hoverCol, &(*cardState).lockedRow, &(*cardState).lockedCol)
		case "breakdown":
			breakdownChart(c, p, spec, colors, &(*cardState).selected, &(*cardState).locked, hidden)
		case "hbar":
			hbarChart(c, p, spec, colors, hidden)
		default:
			barChart(c, p, spec, effType, colors, &(*cardState).selected, &(*cardState).locked, hidden)
		}
		if len(spec.Series) > 1 && effType != "pie" && effType != "donut" && effType != "heatmap" && effType != "stats" && effType != "breakdown" {
			chartLegend(c, p, spec, colors, hidden, func(s int) {
				if s < 0 || s >= 32 {
					return
				}
				bit := uint32(1) << s
				isHidden := (*cardState).hiddenMask&bit != 0
				visCount := 0
				for i := range spec.Series {
					if i < 32 && (*cardState).hiddenMask&(uint32(1)<<i) == 0 {
						visCount++
					}
				}
				if !isHidden && visCount <= 1 {
					return
				}
				(*cardState).hiddenMask ^= bit
			})
		}
		if canTable && (*cardState).showTable {
			chartDataTable(c, p, effSpec)
		}
	})
}

func chartReadoutText(p palette, spec *chartSpec, effType string, colors []ui.Color, hidden map[int]bool, st *chartState) (text string, dot ui.Color, isLocked, show bool) {
	switch effType {
	case "bar", "stacked", "stacked100", "line", "area", "combo":
		if len(spec.Labels) == 0 {
			return "", p.faint, false, false
		}
		idx := st.locked
		if idx >= 0 && idx < len(spec.Labels) {
			isLocked = true
		} else if st.selected >= 0 && st.selected < len(spec.Labels) {
			idx = st.selected
		} else {
			return "悬停图表查看明细 · 点击可锁定对比", p.faint, false, true
		}
		indices := activeSeriesIndices(spec, hidden)
		parts := make([]string, 0, len(indices))
		for _, s := range indices {
			if idx < len(spec.Series[s].Data) {
				parts = append(parts, seriesName(spec, s)+" "+formatChartValue(spec.Series[s].Data[idx], spec.Unit))
			}
		}
		prefix := spec.Labels[idx]
		if isLocked {
			prefix = "已锁定 " + prefix
		}
		return prefix + " · " + strings.Join(parts, " · "), colors[0], isLocked, true
	case "heatmap":
		r, c := st.lockedRow, st.lockedCol
		if r >= 0 && c >= 0 && r < len(spec.Series) && c < len(spec.Labels) {
			isLocked = true
		} else if st.hoverRow >= 0 && st.hoverCol >= 0 && st.hoverRow < len(spec.Series) && st.hoverCol < len(spec.Labels) {
			r, c = st.hoverRow, st.hoverCol
		} else {
			return "悬停圆点查看单格数值 · 点击可锁定", p.faint, false, true
		}
		v := math.NaN()
		if c < len(spec.Series[r].Data) {
			v = spec.Series[r].Data[c]
		}
		prefix := fmt.Sprintf("%s %s", seriesName(spec, r), spec.Labels[c])
		if isLocked {
			prefix = "已锁定 " + prefix
		}
		return prefix + " · " + formatChartValue(v, spec.Unit), colors[0], isLocked, true
	default:
		return "", p.faint, false, false
	}
}

func safeChartFilename(title, fallback string) string {
	s := strings.TrimSpace(title)
	if s == "" {
		return fallback
	}
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			sb.WriteByte('_')
		default:
			sb.WriteRune(r)
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return fallback
	}
	return out
}

func saveTextWithDialog(title, defaultName, content string) {
	go func() {
		path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{
			Title:       title,
			DefaultPath: defaultName,
		})
		if err != nil || strings.TrimSpace(path) == "" {
			return
		}
		_ = os.WriteFile(path, []byte(content), 0o644)
	}()
}

func chartKindShort(kind string) string {
	switch kind {
	case "bar":
		return "柱图"
	case "stacked":
		return "堆叠"
	case "stacked100":
		return "百分比"
	case "hbar":
		return "横条"
	case "line":
		return "折线"
	case "area":
		return "面积"
	case "combo":
		return "组合"
	case "scatter":
		return "散点"
	case "bubble":
		return "气泡"
	case "heatmap":
		return "热力"
	case "breakdown":
		return "分布"
	case "pie":
		return "饼图"
	case "donut":
		return "环图"
	}
	return kind
}

func chartKindBadge(kind, unit string) string {
	name := "图表"
	switch kind {
	case "bar":
		name = "柱状对比"
	case "stacked":
		name = "堆叠对比"
	case "stacked100":
		name = "百分比堆叠"
	case "hbar":
		name = "横向对比"
	case "line":
		name = "趋势折线"
	case "area":
		name = "面积趋势"
	case "combo":
		name = "组合双轴"
	case "scatter":
		name = "散点分布"
	case "bubble":
		name = "气泡分布"
	case "heatmap":
		name = "活跃热度分布"
	case "breakdown":
		name = "结构占比明细"
	case "pie", "donut":
		name = "占比分布"
	}
	if unit != "" {
		return name + " (" + unit + ")"
	}
	return name
}

func chartDataTable(c *ui.Context, p palette, spec *chartSpec) {
	ui.Column(c.Key("chart-data-table")).FillWidth().Radius(8).Border(1, p.lineSoft).Background(p.soft).Children(func() {
		ui.Row(c).Padding(6, 10).Gap(8).Background(p.canvas.Alpha(0.35)).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
			ui.Text(c, "类别").FontSize(11).FontWeight(600).TextColor(p.muted).Grow(1).Basis(0)
			for i := range spec.Series {
				ui.Text(c, seriesName(spec, i)).FontSize(11).FontWeight(600).TextColor(p.muted).Grow(1).Basis(0).TextAlign(ui.End)
			}
		})
		limit := min(len(spec.Labels), 24)
		for r := 0; r < limit; r++ {
			hi := r < len(spec.Highlights) && spec.Highlights[r]
			row := ui.Row(c.Key("r:"+strconv.Itoa(r))).Padding(5, 10).Gap(8)
			if r > 0 {
				row.BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft)
			}
			if hi {
				row.Background(p.accent.Alpha(0.08))
			}
			row.Children(func() {
				tone := p.ink
				weight := 400
				if hi {
					tone = p.accentInk
					weight = 600
				}
				ui.Text(c, spec.Labels[r]).FontSize(11.5).FontWeight(weight).TextColor(tone).Grow(1).Basis(0).SingleLine().Ellipsis("…")
				for s := range spec.Series {
					ui.Text(c, formatChartValue(spec.Series[s].Data[r], spec.Unit)).Font("Menlo").FontSize(11).TextColor(p.muted).Grow(1).Basis(0).TextAlign(ui.End)
				}
			})
		}
	})
}

func nonEmptyString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func visualError(c *ui.Context, p palette, message, code string) {
	ui.Column(c).FillWidth().Gap(6).Padding(16, 16).Radius(12).Background(p.raised).Border(1, p.line).Children(func() {
		ui.Text(c, "没有可绘制的数据").FontSize(13).FontWeight(600).TextColor(p.ink)
		ui.Text(c, message).FontSize(12).TextColor(p.muted).Wrap()
		if strings.TrimSpace(code) != "" {
			ui.Text(c, strings.TrimSpace(code)).Font("Menlo").FontSize(11).TextColor(p.faint).Wrap().Selectable()
		}
	})
}

func axisLabel(c *ui.Context, p palette) func(chart.Side, chart.Tick) {
	return func(_ chart.Side, tick chart.Tick) {
		ui.Text(c, tick.Label).NoWrap().FontSize(10).TextColor(p.muted)
	}
}

func valueTicks(y chart.Linear, unit string) []chart.Tick {
	ticks := y.Ticks(4)
	for i := range ticks {
		ticks[i].Label = formatChartValue(ticks[i].Value, unit)
	}
	return ticks
}

func seriesExtent(spec *chartSpec, stacked bool) (lo, hi float64) {
	return seriesExtentVisible(spec, nil, stacked)
}

func seriesExtentVisible(spec *chartSpec, hidden map[int]bool, stacked bool) (lo, hi float64) {
	lo, hi = 0, 0
	indices := activeSeriesIndices(spec, hidden)
	if stacked {
		vals := make([][]float64, len(indices))
		for k, idx := range indices {
			vals[k] = make([]float64, len(spec.Series[idx].Data))
			for j, v := range spec.Series[idx].Data {
				if math.IsNaN(v) {
					v = 0
				}
				vals[k][j] = v
			}
		}
		stacks := chart.Stack(vals, chart.StackStandard)
		for _, series := range stacks {
			for _, seg := range series {
				lo, hi = math.Min(lo, seg.Lo), math.Max(hi, seg.Hi)
			}
		}
		if hi == lo {
			hi = lo + 1
		}
		return lo, hi
	}
	for _, idx := range indices {
		for _, v := range spec.Series[idx].Data {
			if !math.IsNaN(v) {
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
		}
	}
	if hi == lo {
		hi = lo + 1
	}
	return lo, hi
}

func seriesValues(spec *chartSpec) [][]float64 {
	values := make([][]float64, len(spec.Series))
	for i, s := range spec.Series {
		values[i] = make([]float64, len(s.Data))
		for j, v := range s.Data {
			if math.IsNaN(v) {
				v = 0
			}
			values[i][j] = v
		}
	}
	return values
}

func seriesValuesSubset(spec *chartSpec, indices []int) [][]float64 {
	values := make([][]float64, len(indices))
	for k, idx := range indices {
		s := spec.Series[idx]
		values[k] = make([]float64, len(s.Data))
		for j, v := range s.Data {
			if math.IsNaN(v) {
				v = 0
			}
			values[k][j] = v
		}
	}
	return values
}

func scatterExtents(spec *chartSpec, hidden map[int]bool) (xLo, xHi, yLo, yHi, rLo, rHi float64) {
	xLo, yLo, rLo = math.Inf(1), math.Inf(1), math.Inf(1)
	xHi, yHi, rHi = math.Inf(-1), math.Inf(-1), math.Inf(-1)
	found := false
	for _, sIdx := range activeSeriesIndices(spec, hidden) {
		for _, pt := range spec.Series[sIdx].Points {
			found = true
			xLo, xHi = math.Min(xLo, pt.X), math.Max(xHi, pt.X)
			yLo, yHi = math.Min(yLo, pt.Y), math.Max(yHi, pt.Y)
			if pt.R > 0 {
				rLo, rHi = math.Min(rLo, pt.R), math.Max(rHi, pt.R)
			}
		}
	}
	if !found {
		return 0, 1, 0, 1, 0, 1
	}
	if xHi <= xLo {
		xLo, xHi = xLo-1, xHi+1
	} else {
		pad := (xHi - xLo) * 0.08
		xLo, xHi = xLo-pad, xHi+pad
	}
	if yHi <= yLo {
		yLo, yHi = yLo-1, yHi+1
	} else {
		pad := (yHi - yLo) * 0.08
		yLo, yHi = yLo-pad, yHi+pad
	}
	if math.IsInf(rLo, 1) || rHi <= 0 {
		rLo, rHi = 1, 1
	}
	return xLo, xHi, yLo, yHi, rLo, rHi
}

var chartTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02",
	"2006-01",
}

func parseTimeLabels(labels []string, xType string) ([]time.Time, bool) {
	if len(labels) < 2 || strings.EqualFold(strings.TrimSpace(xType), "category") {
		return nil, false
	}
	layouts := chartTimeLayouts
	if strings.EqualFold(strings.TrimSpace(xType), "time") {
		layouts = append(append([]string(nil), chartTimeLayouts...), "15:04:05", "15:04", "01-02")
	}
	times := make([]time.Time, len(labels))
	for i, raw := range labels {
		s := strings.TrimSpace(raw)
		var parsed time.Time
		var ok bool
		for _, layout := range layouts {
			if t, err := time.Parse(layout, s); err == nil {
				parsed = t
				ok = true
				break
			}
		}
		if !ok {
			return nil, false
		}
		if i > 0 && parsed.Before(times[i-1]) {
			return nil, false
		}
		times[i] = parsed
	}
	if !times[len(times)-1].After(times[0]) {
		return nil, false
	}
	return times, true
}

func drawRefLines(c *ui.Context, p palette, y chart.Linear, refs []chartRefLine) {
	for _, ref := range refs {
		fy := y.At(ref.Value)
		if fy < -0.02 || fy > 1.02 {
			continue
		}
		col := p.warning
		switch strings.ToLower(strings.TrimSpace(ref.Tone)) {
		case "danger":
			col = p.danger
		case "success":
			col = p.success
		}
		chart.RuleY(c, fy, 1.5).Background(col.Alpha(0.7))
		if ref.Label != "" {
			chart.Anchor(c, 0.99, fy, ui.End, ui.End, func() {
				ui.Text(c, ref.Label).NoWrap().FontSize(9.5).FontWeight(600).TextColor(col).Padding(1, 5).Radius(4).Background(p.raised.Alpha(0.9)).Margin(0, 0, 3, 0)
			})
		}
	}
}

func gridLines(area ui.Element, ticks []chart.Tick, color ui.Color) {
	area.Draw(func(painter *ui.Painter, r ui.Rect) {
		for _, tick := range ticks {
			y := chart.Y(r, tick.At)
			painter.Line(r.X, y, r.X+r.W, y, 1, color)
		}
	})
}

func seriesName(spec *chartSpec, i int) string {
	if i >= 0 && i < len(spec.Series) {
		if name := spec.Series[i].Name; name != "" {
			return name
		}
	}
	return fmt.Sprintf("系列 %d", i+1)
}

func barChart(c *ui.Context, p palette, spec *chartSpec, effType string, colors []ui.Color, selected, lockedPtr *int, hidden map[int]bool) {
	isStacked := effType == "stacked" || effType == "stacked100"
	isPercent := effType == "stacked100"
	indices := activeSeriesIndices(spec, hidden)
	x := chart.NewBand(spec.Labels...).Padding(0.28, 0.08)
	var lo, hi float64
	if isPercent {
		lo, hi = 0, 1
	} else {
		lo, hi = seriesExtentVisible(spec, hidden, isStacked)
	}
	y := chart.NewLinear(lo, hi).Nice(4)
	var ticks []chart.Tick
	if isPercent {
		ticks = y.Ticks(4)
		for i := range ticks {
			ticks[i].Label = fmt.Sprintf("%.0f%%", ticks[i].Value*100)
		}
	} else {
		ticks = valueTicks(y, spec.Unit)
	}
	bottom := x.Ticks()
	if len(bottom) > 12 { // keep category labels readable on long axes
		step := (len(bottom) + 11) / 12
		thinned := bottom[:0:0]
		for i := 0; i < len(bottom); i += step {
			thinned = append(thinned, bottom[i])
		}
		bottom = thinned
	}
	xs := make([]float32, len(spec.Labels))
	for i := range xs {
		xs[i] = x.At(i)
	}
	describe := func(i int) string {
		parts := []string{spec.Labels[i]}
		for _, s := range indices {
			parts = append(parts, seriesName(spec, s)+" "+formatChartValue(spec.Series[s].Data[i], spec.Unit))
		}
		return strings.Join(parts, " · ")
	}
	chart.Plot(c, chart.Axes{Left: ticks, Bottom: bottom, Label: axisLabel(c, p)}, func(area ui.Element) {
		gridLines(area, ticks, p.lineSoft.Alpha(0.65))
		chart.Select(c, area, selected, xs, describe)
		hover := *selected
		activeIdx := hover
		if activeIdx < 0 {
			activeIdx = *lockedPtr
		}
		if activeIdx >= 0 && activeIdx < len(xs) {
			band := x.Band(activeIdx)
			pad := (band.Hi - band.Lo) * 0.28
			chart.Rect(c, chart.Span{Lo: band.Lo - pad, Hi: band.Hi + pad}, chart.Span{Lo: 0, Hi: 1}).Radius(6).Background(p.muted.Alpha(0.08))
		}
		var stacks [][]chart.Segment
		if isStacked {
			how := chart.StackStandard
			if isPercent {
				how = chart.StackNormalized
			}
			stacks = chart.Stack(seriesValuesSubset(spec, indices), how)
		}
		n := len(indices)
		for i, label := range spec.Labels {
			i := i
			band := x.Band(i)
			width := (band.Hi - band.Lo) / float32(max(1, n))
			dim := activeIdx >= 0 && activeIdx != i
			for k, s := range indices {
				v := spec.Series[s].Data[i]
				if math.IsNaN(v) {
					continue
				}
				var xSpan, ySpan chart.Span
				if isStacked {
					xSpan, ySpan = band, y.Span(stacks[k][i].Lo, stacks[k][i].Hi)
				} else {
					gap := width * 0.08
					xSpan = chart.Span{Lo: band.Lo + width*float32(k) + gap, Hi: band.Lo + width*float32(k+1) - gap}
					ySpan = y.Span(0, v)
				}
				tip := label + " · " + seriesName(spec, s) + "：" + formatChartValue(v, spec.Unit)
				if n == 1 {
					tip = label + "：" + formatChartValue(v, spec.Unit)
				} else if isPercent {
					pct := (stacks[k][i].Hi - stacks[k][i].Lo) * 100
					tip = fmt.Sprintf("%s · %s：%s（%.1f%%）", label, seriesName(spec, s), formatChartValue(v, spec.Unit), pct)
				}
				barCol := colors[s%len(colors)]
				if dim {
					barCol = barCol.Alpha(0.32)
				}
				var barEl ui.Element
				if isStacked {
					barEl = chart.Rect(c, xSpan, ySpan).Radius(3)
				} else {
					barEl = chart.Rect(c, xSpan, ySpan).Radius(6, 6, 2, 2)
				}
				barEl.Background(barCol).Tooltip(tip).Label(tip)
				if barEl.Clicked() {
					if *lockedPtr == i {
						*lockedPtr = -1
					} else {
						*lockedPtr = i
					}
				}
				if !isStacked && n == 1 && len(spec.Labels) <= 12 && v != 0 {
					lblCol := p.muted
					if activeIdx == i {
						lblCol = p.ink
					}
					chart.Anchor(c, (xSpan.Lo+xSpan.Hi)/2, y.At(v), ui.Center, ui.End, func() {
						ui.Text(c, formatChartValue(v, spec.Unit)).FontSize(10).FontFeatures("tnum").TextColor(lblCol).NoWrap().Margin(0, 0, 2, 0)
					})
				}
			}
		}
		if len(spec.RefLines) > 0 {
			refs := spec.RefLines
			if isPercent {
				norm := make([]chartRefLine, len(refs))
				for idx, r := range refs {
					if r.Value > 1 {
						r.Value = r.Value / 100
					}
					norm[idx] = r
				}
				refs = norm
			}
			drawRefLines(c, p, y, refs)
		}
		if activeIdx >= 0 && activeIdx < len(xs) {
			chart.RuleX(c, xs[activeIdx], 1).Background(p.muted.Alpha(0.35))
			align := ui.Center
			if xs[activeIdx] < 0.15 {
				align = ui.Start
			} else if xs[activeIdx] > 0.85 {
				align = ui.End
			}
			chart.Anchor(c, xs[activeIdx], 1, align, ui.Start, func() {
				ui.Column(c).Gap(3).Padding(6, 10).Radius(8).Background(p.surface).Border(1, p.line).Shadow(0, 4, 14, 0, ui.Hex("#00000028")).Children(func() {
					ui.Text(c, spec.Labels[activeIdx]).FontSize(11).Bold().TextColor(p.ink).NoWrap()
					for _, s := range indices {
						ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
							ui.Box(c).Size(7, 7).Radius(2).Background(colors[s%len(colors)])
							ui.Text(c, seriesName(spec, s)).FontSize(10).TextColor(p.muted).NoWrap()
							ui.Text(c, formatChartValue(spec.Series[s].Data[activeIdx], spec.Unit)).Font("Menlo").FontSize(10).FontFeatures("tnum").TextColor(p.ink).NoWrap()
						})
					}
				})
			})
		}
	}).Label(nonEmptyString(spec.Title, "柱状图")).Height(spec.Height).Gap(6).FillWidth()
}

func hbarChart(c *ui.Context, p palette, spec *chartSpec, colors []ui.Color, hidden map[int]bool) {
	indices := activeSeriesIndices(spec, hidden)
	labels := make([]string, len(spec.Labels))
	for i := range spec.Labels { // the first label reads at the top
		labels[i] = spec.Labels[len(spec.Labels)-1-i]
	}
	band := chart.NewBand(labels...).Padding(0.28, 0.08)
	lo, hi := seriesExtentVisible(spec, hidden, false)
	x := chart.NewLinear(lo, hi).Nice(4)
	ticks := valueTicks(x, spec.Unit)
	rowUnit := float32(26)
	if len(indices) > 1 {
		rowUnit = float32(len(indices))*15 + 12
	}
	height := max(spec.Height, float32(len(labels))*rowUnit+30)
	chart.Plot(c, chart.Axes{Left: band.Ticks(), Bottom: ticks, Label: axisLabel(c, p)}, func(area ui.Element) {
		area.Draw(func(painter *ui.Painter, r ui.Rect) {
			for _, tick := range ticks {
				px := chart.X(r, tick.At)
				painter.Line(px, r.Y, px, r.Y+r.H, 1, p.lineSoft)
			}
		})
		n := len(indices)
		for i := range labels {
			rowIdx := len(labels) - 1 - i
			rowSpan := band.Band(i)
			barH := (rowSpan.Hi - rowSpan.Lo) / float32(max(1, n))
			for k, s := range indices {
				value := spec.Series[s].Data[rowIdx]
				if math.IsNaN(value) {
					continue
				}
				tip := labels[i] + "：" + formatChartValue(value, spec.Unit)
				if n > 1 {
					tip = labels[i] + " · " + seriesName(spec, s) + "：" + formatChartValue(value, spec.Unit)
				}
				var ySpan chart.Span
				if n == 1 {
					ySpan = rowSpan
				} else {
					gap := barH * 0.08
					ySpan = chart.Span{
						Lo: rowSpan.Hi - barH*float32(k+1) + gap,
						Hi: rowSpan.Hi - barH*float32(k) - gap,
					}
				}
				chart.Rect(c, x.Span(0, value), ySpan).Radius(5).Background(colors[s%len(colors)]).Tooltip(tip).Label(tip)
				if n == 1 || len(labels) <= 8 {
					chart.Anchor(c, x.At(value), (ySpan.Lo+ySpan.Hi)/2, ui.Start, ui.Center, func() {
						ui.Text(c, formatChartValue(value, spec.Unit)).NoWrap().FontSize(10).TextColor(p.muted).Margin(0, 0, 0, 4)
					})
				}
			}
		}
	}).Label(nonEmptyString(spec.Title, "横条图")).Height(height).Gap(6).FillWidth()
}

func lineChart(c *ui.Context, p palette, spec *chartSpec, effType string, colors []ui.Color, selected, lockedPtr *int, hidden map[int]bool, layer *chartLayer) {
	indices := activeSeriesIndices(spec, hidden)
	times, isTime := parseTimeLabels(spec.Labels, spec.XType)
	var bottom []chart.Tick
	xs := make([]float32, len(spec.Labels))
	if isTime {
		tScale := chart.NewTime(times[0], times[len(times)-1])
		for i, tm := range times {
			xs[i] = tScale.At(tm)
		}
		bottom = tScale.Ticks(6)
	} else {
		x := chart.NewBand(spec.Labels...).Padding(0, 0.02)
		for i := range xs {
			xs[i] = x.At(i)
		}
		bottom = x.Ticks()
		if len(bottom) > 10 {
			step := (len(bottom) + 9) / 10
			thinned := bottom[:0:0]
			for i := 0; i < len(bottom); i += step {
				thinned = append(thinned, bottom[i])
			}
			bottom = thinned
		}
	}
	lo, hi := seriesExtentVisible(spec, hidden, false)
	if effType == "line" { // a line need not start at zero
		lo, hi = math.Inf(1), math.Inf(-1)
		for _, s := range indices {
			for _, v := range spec.Series[s].Data {
				if !math.IsNaN(v) {
					lo, hi = math.Min(lo, v), math.Max(hi, v)
				}
			}
		}
		if hi <= lo {
			hi = lo + 1
		}
		pad := (hi - lo) * 0.08
		lo, hi = lo-pad, hi+pad
		if lo > 0 && lo < (hi-lo)*0.6 {
			lo = 0
		}
	}
	y := chart.NewLinear(lo, hi).Nice(4)
	ticks := valueTicks(y, spec.Unit)
	ys := make(map[int][]float32, len(indices))
	for _, s := range indices {
		series := spec.Series[s]
		arr := make([]float32, len(series.Data))
		for i, v := range series.Data {
			if math.IsNaN(v) {
				arr[i] = float32(math.NaN())
			} else {
				arr[i] = y.At(v)
			}
		}
		ys[s] = arr
	}
	describe := func(i int) string {
		parts := []string{spec.Labels[i]}
		for _, s := range indices {
			parts = append(parts, seriesName(spec, s)+" "+formatChartValue(spec.Series[s].Data[i], spec.Unit))
		}
		return strings.Join(parts, " · ")
	}
	stateKey := visibleSeriesMask(spec, hidden, effType)
	chart.Plot(c, chart.Axes{Left: ticks, Bottom: bottom, Label: axisLabel(c, p)}, func(area ui.Element) {
		area.Draw(func(painter *ui.Painter, r ui.Rect) {
			layer.paint(painter, r, spec, p.dark, stateKey, func(cp *ui.Painter, r ui.Rect) {
				for _, tick := range ticks {
					py := chart.Y(r, tick.At)
					cp.Line(r.X, py, r.X+r.W, py, 1, p.lineSoft.Alpha(0.65))
				}
				pixelW := max(1, int(math.Ceil(float64(r.W*cp.Scale()))))
				for _, s := range indices {
					color := colors[s%len(colors)]
					drawX, drawY := xs, ys[s]
					if len(drawX) > 300 && len(drawX) > pixelW*2 {
						var dsX, dsY []float32
						if layer.ds != nil {
							dsX, dsY = layer.ds.x, layer.ds.y
						}
						drawX, drawY = chart.MinMax(dsX, dsY, xs, ys[s], pixelW)
						if layer.ds != nil {
							layer.ds.x, layer.ds.y = drawX, drawY
						}
					}
					if effType == "area" {
						cp.FillPathGradient(chart.Area(r, drawX, nil, drawY, chart.CurveMonotone), ui.LinearGradient{
							From:  color.Alpha(0.24),
							To:    color.Alpha(0.0),
							Angle: 180,
						})
					}
					cp.StrokePath(chart.Line(r, drawX, drawY, chart.CurveMonotone), 2.5, color)
				}
			})
		})
		drawRefLines(c, p, y, spec.RefLines)
		chart.Select(c, area, selected, xs, describe)
		if area.Clicked() && *selected >= 0 && *selected < len(xs) {
			if *lockedPtr == *selected {
				*lockedPtr = -1
			} else {
				*lockedPtr = *selected
			}
		}
		activeIdx := *selected
		if activeIdx < 0 {
			activeIdx = *lockedPtr
		}
		if i := activeIdx; i >= 0 && i < len(xs) {
			chart.RuleX(c, xs[i], 1).Background(p.muted.Alpha(0.45))
			for _, s := range indices {
				if !math.IsNaN(float64(ys[s][i])) {
					chart.Point(c, xs[i], ys[s][i], 10, 10).Radius(5).Background(p.raised).Border(2.5, colors[s%len(colors)])
				}
			}
			align := ui.Center
			if xs[i] < 0.15 {
				align = ui.Start
			} else if xs[i] > 0.85 {
				align = ui.End
			}
			chart.Anchor(c, xs[i], 1, align, ui.Start, func() {
				if len(indices) <= 3 {
					ui.Text(c, describe(i)).NoWrap().FontSize(11).FontFeatures("tnum").Padding(4, 9).Radius(7).Background(p.surface).Border(1, p.line).TextColor(p.ink)
					return
				}
				ui.Column(c).Gap(3).Padding(6, 10).Radius(8).Background(p.surface).Border(1, p.line).Shadow(0, 4, 14, 0, ui.Hex("#00000028")).Children(func() {
					ui.Text(c, spec.Labels[i]).FontSize(11).Bold().TextColor(p.ink).NoWrap()
					for _, s := range indices {
						ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
							ui.Box(c).Size(7, 7).Radius(2).Background(colors[s%len(colors)])
							ui.Text(c, seriesName(spec, s)).FontSize(10).TextColor(p.muted).NoWrap()
							ui.Text(c, formatChartValue(spec.Series[s].Data[i], spec.Unit)).Font("Menlo").FontSize(10).FontFeatures("tnum").TextColor(p.ink).NoWrap()
						})
					}
				})
			})
		} else if len(xs) <= 24 {
			for _, s := range indices {
				for i := range xs {
					if !math.IsNaN(float64(ys[s][i])) {
						chart.Point(c, xs[i], ys[s][i], 6, 6).Radius(3).Background(p.raised).Border(1.8, colors[s%len(colors)])
						if len(indices) == 1 && len(xs) <= 8 {
							chart.Anchor(c, xs[i], ys[s][i], ui.Center, ui.End, func() {
								ui.Text(c, formatChartValue(spec.Series[s].Data[i], spec.Unit)).FontSize(10).FontFeatures("tnum").TextColor(p.muted).NoWrap().Margin(0, 0, 4, 0)
							})
						}
					}
				}
			}
		}
	}).Label(nonEmptyString(spec.Title, "折线图")).Height(spec.Height).Gap(6).FillWidth()
}

func comboChart(c *ui.Context, p palette, spec *chartSpec, colors []ui.Color, selected *int, hidden map[int]bool, layer *chartLayer) {
	indices := activeSeriesIndices(spec, hidden)
	var leftIdx, rightIdx []int
	hasRight := spec.UnitRight != ""
	for _, s := range indices {
		if strings.EqualFold(spec.Series[s].Axis, "right") {
			rightIdx = append(rightIdx, s)
			hasRight = true
		} else {
			leftIdx = append(leftIdx, s)
		}
	}
	if len(leftIdx) == 0 && len(rightIdx) > 0 {
		leftIdx = rightIdx
		rightIdx = nil
		hasRight = false
	}
	extentFor := func(list []int) (lo, hi float64) {
		lo, hi = 0, 0
		for _, s := range list {
			for _, v := range spec.Series[s].Data {
				if !math.IsNaN(v) {
					lo, hi = math.Min(lo, v), math.Max(hi, v)
				}
			}
		}
		if hi == lo {
			hi = lo + 1
		}
		return lo, hi
	}
	lLo, lHi := extentFor(leftIdx)
	yLeft := chart.NewLinear(lLo, lHi).Nice(4)
	leftTicks := valueTicks(yLeft, spec.Unit)
	var yRight chart.Linear
	var rightTicks []chart.Tick
	if len(rightIdx) > 0 {
		rLo, rHi := extentFor(rightIdx)
		yRight = chart.NewLinear(rLo, rHi).Nice(4)
		unitR := nonEmptyString(spec.UnitRight, spec.Unit)
		rightTicks = valueTicks(yRight, unitR)
	}

	x := chart.NewBand(spec.Labels...).Padding(0.26, 0.08)
	bottom := x.Ticks()
	if len(bottom) > 12 {
		step := (len(bottom) + 11) / 12
		thinned := bottom[:0:0]
		for i := 0; i < len(bottom); i += step {
			thinned = append(thinned, bottom[i])
		}
		bottom = thinned
	}
	xs := make([]float32, len(spec.Labels))
	for i := range xs {
		xs[i] = x.At(i)
	}
	axisFor := func(s int) (chart.Linear, string) {
		if len(rightIdx) > 0 && strings.EqualFold(spec.Series[s].Axis, "right") {
			return yRight, nonEmptyString(spec.UnitRight, spec.Unit)
		}
		return yLeft, spec.Unit
	}
	describe := func(i int) string {
		parts := []string{spec.Labels[i]}
		for _, s := range indices {
			_, unit := axisFor(s)
			parts = append(parts, seriesName(spec, s)+" "+formatChartValue(spec.Series[s].Data[i], unit))
		}
		return strings.Join(parts, " · ")
	}
	axes := chart.Axes{Left: leftTicks, Bottom: bottom, Label: axisLabel(c, p)}
	if hasRight && len(rightTicks) > 0 {
		axes.Right = rightTicks
	}
	var barIndices, lineIndices []int
	for _, s := range indices {
		kind := strings.ToLower(strings.TrimSpace(spec.Series[s].Kind))
		if kind == "" {
			if strings.EqualFold(spec.Series[s].Axis, "right") {
				kind = "line"
			} else {
				kind = "bar"
			}
		}
		if kind == "line" || kind == "area" {
			lineIndices = append(lineIndices, s)
		} else {
			barIndices = append(barIndices, s)
		}
	}
	lineYs := make(map[int][]float32, len(lineIndices))
	for _, s := range lineIndices {
		sc, _ := axisFor(s)
		arr := make([]float32, len(spec.Series[s].Data))
		for i, v := range spec.Series[s].Data {
			if math.IsNaN(v) {
				arr[i] = float32(math.NaN())
			} else {
				arr[i] = sc.At(v)
			}
		}
		lineYs[s] = arr
	}
	stateKey := visibleSeriesMask(spec, hidden, "combo")
	chart.Plot(c, axes, func(area ui.Element) {
		gridLines(area, leftTicks, p.lineSoft)
		nBars := max(1, len(barIndices))
		for i, label := range spec.Labels {
			band := x.Band(i)
			bw := (band.Hi - band.Lo) / float32(nBars)
			for k, s := range barIndices {
				v := spec.Series[s].Data[i]
				if math.IsNaN(v) {
					continue
				}
				sc, unit := axisFor(s)
				gap := bw * 0.08
				xSpan := chart.Span{Lo: band.Lo + bw*float32(k) + gap, Hi: band.Lo + bw*float32(k+1) - gap}
				tip := label + " · " + seriesName(spec, s) + "：" + formatChartValue(v, unit)
				chart.Rect(c, xSpan, sc.Span(0, v)).Radius(5).Background(colors[s%len(colors)]).Tooltip(tip).Label(tip)
			}
		}
		area.Draw(func(painter *ui.Painter, r ui.Rect) {
			layer.paint(painter, r, spec, p.dark, stateKey, func(cp *ui.Painter, r ui.Rect) {
				for _, s := range lineIndices {
					color := colors[s%len(colors)]
					if strings.EqualFold(spec.Series[s].Kind, "area") {
						cp.FillPath(chart.Area(r, xs, nil, lineYs[s], chart.CurveMonotone), color.Alpha(0.14))
					}
					cp.StrokePath(chart.Line(r, xs, lineYs[s], chart.CurveMonotone), 2.4, color)
				}
			})
		})
		drawRefLines(c, p, yLeft, spec.RefLines)
		chart.Select(c, area, selected, xs, describe)
		if i := *selected; i >= 0 && i < len(xs) {
			chart.RuleX(c, xs[i], 1).Background(p.muted.Alpha(0.45))
			for _, s := range lineIndices {
				if !math.IsNaN(float64(lineYs[s][i])) {
					chart.Point(c, xs[i], lineYs[s][i], 8, 8).Radius(4).Background(colors[s%len(colors)]).Border(2, p.soft)
				}
			}
			align := ui.Center
			if xs[i] < 0.15 {
				align = ui.Start
			} else if xs[i] > 0.85 {
				align = ui.End
			}
			chart.Anchor(c, xs[i], 1, align, ui.Start, func() {
				ui.Column(c).Gap(3).Padding(5, 9).Radius(7).Background(p.surface).Border(1, p.line).Shadow(0, 4, 14, 0, ui.Hex("#0000002E")).Children(func() {
					ui.Text(c, spec.Labels[i]).FontSize(11).Bold().TextColor(p.ink).NoWrap()
					for _, s := range indices {
						_, unit := axisFor(s)
						ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
							ui.Box(c).Size(7, 7).Radius(2).Background(colors[s%len(colors)])
							ui.Text(c, seriesName(spec, s)).FontSize(10).TextColor(p.muted).NoWrap()
							ui.Text(c, formatChartValue(spec.Series[s].Data[i], unit)).Font("Menlo").FontSize(10).TextColor(p.ink).NoWrap()
						})
					}
				})
			})
		}
	}).Label(nonEmptyString(spec.Title, "组合图")).Height(spec.Height).Gap(6).FillWidth()
}

func scatterChart(c *ui.Context, p palette, spec *chartSpec, effType string, colors []ui.Color, selected *int, hidden map[int]bool) {
	indices := activeSeriesIndices(spec, hidden)
	xLo, xHi, yLo, yHi, _, rHi := scatterExtents(spec, hidden)
	xsScale := chart.NewLinear(xLo, xHi).Nice(5)
	ysScale := chart.NewLinear(yLo, yHi).Nice(4)
	xTicks := valueTicks(xsScale, "")
	yTicks := valueTicks(ysScale, spec.Unit)

	type flatPt struct {
		series int
		pt     chartPoint
		nx, ny float32
		size   float32
	}
	var flat []flatPt
	for _, s := range indices {
		for _, pt := range spec.Series[s].Points {
			sz := float32(9)
			if effType == "bubble" && rHi > 0 {
				sz = float32(8 + 20*math.Sqrt(max(0.0, pt.R)/rHi))
			}
			flat = append(flat, flatPt{
				series: s,
				pt:     pt,
				nx:     xsScale.At(pt.X),
				ny:     ysScale.At(pt.Y),
				size:   sz,
			})
		}
	}
	xsFlat := make([]float32, len(flat))
	for i, fp := range flat {
		xsFlat[i] = fp.nx
	}
	describe := func(i int) string {
		if i < 0 || i >= len(flat) {
			return ""
		}
		fp := flat[i]
		name := nonEmptyString(fp.pt.Label, seriesName(spec, fp.series))
		return fmt.Sprintf("%s (%s, %s)", name, formatChartValue(fp.pt.X, ""), formatChartValue(fp.pt.Y, spec.Unit))
	}
	chart.Plot(c, chart.Axes{Left: yTicks, Bottom: xTicks, Label: axisLabel(c, p)}, func(area ui.Element) {
		gridLines(area, yTicks, p.lineSoft)
		for idx, fp := range flat {
			col := colors[fp.series%len(colors)]
			tip := describe(idx)
			if effType == "bubble" && fp.pt.R > 0 {
				tip += " · r=" + formatChartValue(fp.pt.R, "")
			}
			bg := col.Alpha(0.75)
			if *selected == idx {
				bg = col
			}
			chart.Point(c, fp.nx, fp.ny, fp.size, fp.size).Radius(fp.size/2).Background(bg).Border(1.5, col).Tooltip(tip).Label(tip)
		}
		drawRefLines(c, p, ysScale, spec.RefLines)
		if len(xsFlat) > 0 {
			chart.Select(c, area, selected, xsFlat, describe)
		}
		if idx := *selected; idx >= 0 && idx < len(flat) {
			fp := flat[idx]
			align := ui.Center
			if fp.nx < 0.2 {
				align = ui.Start
			} else if fp.nx > 0.8 {
				align = ui.End
			}
			chart.Anchor(c, fp.nx, min(float32(0.96), fp.ny+0.04), align, ui.End, func() {
				ui.Text(c, describe(idx)).NoWrap().FontSize(10.5).Padding(3, 8).Radius(6).Background(p.surface).Border(1, p.line).TextColor(p.ink)
			})
		}
	}).Label(nonEmptyString(spec.Title, "散点图")).Height(spec.Height).Gap(6).FillWidth()
}

func heatmapChart(c *ui.Context, p palette, spec *chartSpec, pal palette, hoverRowPtr, hoverColPtr, lockedRowPtr, lockedColPtr *int) {
	rows := len(spec.Series)
	rowLabels := make([]string, rows)
	for r := 0; r < rows; r++ {
		rowLabels[r] = seriesName(spec, rows-1-r)
	}
	x := chart.NewBand(spec.Labels...).Padding(0.06, 0.04)
	y := chart.NewBand(rowLabels...).Padding(0.06, 0.04)
	lo, hi := seriesExtent(spec, false)
	colors := chartColors(pal)
	dotAccent := colors[0]
	dotMode := !strings.EqualFold(strings.TrimSpace(spec.Style), "tiles")

	var peakRow, peakCol string
	peakVal := math.Inf(-1)
	for r, s := range spec.Series {
		for colIdx, colLabel := range spec.Labels {
			if colIdx < len(s.Data) && !math.IsNaN(s.Data[colIdx]) && s.Data[colIdx] > peakVal {
				peakVal = s.Data[colIdx]
				peakRow = seriesName(spec, r)
				peakCol = colLabel
			}
		}
	}

	rowH := float32(28)
	if !dotMode {
		rowH = 34
	}
	height := max(spec.Height, float32(rows)*rowH+42)
	bottom := x.Ticks()
	if len(bottom) > 12 {
		step := (len(bottom) + 11) / 12
		thinned := bottom[:0:0]
		for i := 0; i < len(bottom); i += step {
			thinned = append(thinned, bottom[i])
		}
		bottom = thinned
	}

	nextHoverR, nextHoverC := -1, -1
	activeR, activeC := *hoverRowPtr, *hoverColPtr
	if *lockedRowPtr >= 0 && *lockedColPtr >= 0 {
		activeR, activeC = *lockedRowPtr, *lockedColPtr
	}

	chart.Plot(c, chart.Axes{Left: y.Ticks(), Bottom: bottom, Label: axisLabel(c, p)}, func(area ui.Element) {
		dotSz := float32(16)
		if len(spec.Labels) >= 18 {
			dotSz = 12
		} else if len(spec.Labels) >= 12 {
			dotSz = 14
		} else if len(spec.Labels) <= 6 && rows <= 6 {
			dotSz = 20
		}

		for r := 0; r < rows; r++ {
			sIdx := rows - 1 - r
			ySpan := y.Band(r)
			cy := (ySpan.Lo + ySpan.Hi) / 2
			s := spec.Series[sIdx]
			rowTitle := seriesName(spec, sIdx)
			for colIdx, colLabel := range spec.Labels {
				v := math.NaN()
				if colIdx < len(s.Data) {
					v = s.Data[colIdx]
				}
				xSpan := x.Band(colIdx)
				cx := (xSpan.Lo + xSpan.Hi) / 2

				if !dotMode {
					if math.IsNaN(v) {
						chart.Rect(c, xSpan, ySpan).Radius(4).Background(pal.soft)
						continue
					}
					t := 0.5
					if hi > lo {
						t = (v - lo) / (hi - lo)
					}
					fill := pal.soft.Mix(dotAccent, float32(max(0.14, min(1.0, t))))
					tip := fmt.Sprintf("%s · %s：%s", rowTitle, colLabel, formatChartValue(v, spec.Unit))
					cell := chart.Rect(c, xSpan, ySpan).Radius(4).Background(fill).Tooltip(tip).Label(tip)
					if cell.Hovered() {
						nextHoverR, nextHoverC = sIdx, colIdx
					}
					if len(spec.Labels) <= 10 && rows <= 8 {
						txtColor := pal.ink
						if t > 0.65 {
							txtColor = ui.Hex("#FFFFFF")
						}
						chart.Anchor(c, cx, cy, ui.Center, ui.Center, func() {
							ui.Text(c, formatChartValue(v, spec.Unit)).NoWrap().Font("Menlo").FontSize(10).TextColor(txtColor)
						})
					}
					continue
				}

				if math.IsNaN(v) {
					tip := fmt.Sprintf("%s · %s：暂无数据", rowTitle, colLabel)
					pt := chart.Point(c, cx, cy, dotSz, dotSz).Tooltip(tip).Label(tip)
					pt.Draw(func(painter *ui.Painter, dr ui.Rect) {
						painter.StrokeDashed(dr, pal.faint.Alpha(0.65), dotSz/2, 1.1)
					})
					if pt.Hovered() {
						nextHoverR, nextHoverC = sIdx, colIdx
					}
					continue
				}

				var fill ui.Color
				if v == 0 && lo == 0 {
					fill = pal.lineSoft
				} else {
					t := 0.5
					if hi > lo {
						t = (v - lo) / (hi - lo)
					}
					var strength float32
					switch {
					case t >= 0.75:
						strength = 1.0
					case t >= 0.50:
						strength = 0.74
					case t >= 0.25:
						strength = 0.48
					default:
						strength = 0.26
					}
					fill = pal.soft.Mix(dotAccent, strength)
				}
				isActive := activeR == sIdx && activeC == colIdx
				if activeR >= 0 && !isActive {
					fill = fill.Alpha(0.38)
				}
				tip := fmt.Sprintf("%s · %s：%s", rowTitle, colLabel, formatChartValue(v, spec.Unit))
				pt := chart.Point(c, cx, cy, dotSz, dotSz).Radius(dotSz / 2).Background(fill).Tooltip(tip).Label(tip)
				if isActive {
					pt.Border(2, pal.ink)
				}
				if pt.Hovered() {
					nextHoverR, nextHoverC = sIdx, colIdx
				}
				if pt.Clicked() {
					if *lockedRowPtr == sIdx && *lockedColPtr == colIdx {
						*lockedRowPtr, *lockedColPtr = -1, -1
					} else {
						*lockedRowPtr, *lockedColPtr = sIdx, colIdx
					}
				}
			}
		}
	}).Label(nonEmptyString(spec.Title, "活跃热度图")).Height(height).Gap(6).FillWidth()
	*hoverRowPtr, *hoverColPtr = nextHoverR, nextHoverC

	if dotMode {
		ui.Row(c.Key("heatmap-footer")).FillWidth().Gap(8).AlignItems(ui.Center).Wrap().Children(func() {
			ui.Text(c, "虚线 缺数据 · 浅灰 零用量 · 4 档热度").FontSize(10.5).TextColor(p.muted).Grow(1)
			if !math.IsInf(peakVal, -1) {
				ui.Text(c, fmt.Sprintf("高峰：%s %s · %s", peakRow, peakCol, formatChartValue(peakVal, spec.Unit))).
					FontSize(10.5).FontWeight(600).FontFeatures("tnum").TextColor(p.ink).NoWrap()
			}
		})
	}
}

func breakdownSeriesTotal(s chartSeries) float64 {
	if len(s.Data) == 1 {
		if math.IsNaN(s.Data[0]) {
			return 0
		}
		return max(0.0, s.Data[0])
	}
	sum := 0.0
	for _, v := range s.Data {
		if !math.IsNaN(v) && v > 0 {
			sum += v
		}
	}
	return sum
}

func breakdownSparkline(s chartSeries) []float64 {
	if len(s.Sparkline) >= 2 {
		return s.Sparkline
	}
	if len(s.Data) >= 2 {
		return s.Data
	}
	return nil
}

func breakdownChart(c *ui.Context, p palette, spec *chartSpec, colors []ui.Color, selected, lockedPtr *int, hidden map[int]bool) {
	indices := activeSeriesIndices(spec, hidden)
	totals := make([]float64, len(indices))
	grand := 0.0
	for k, s := range indices {
		v := breakdownSeriesTotal(spec.Series[s])
		totals[k] = v
		grand += v
	}
	if grand <= 0 {
		visualError(c, p, "分布图总和必须大于 0", "")
		return
	}

	activeIdx := *lockedPtr
	hoverIdx := *selected
	if activeIdx < 0 {
		activeIdx = hoverIdx
	}

	// Top proportional segmented bar
	ui.Row(c.Key("breakdown-bar")).FillWidth().Height(10).Gap(2).Children(func() {
		for k, s := range indices {
			share := float32(totals[k] / grand)
			if share <= 0 {
				continue
			}
			col := colors[s%len(colors)]
			if activeIdx >= 0 && activeIdx != s {
				col = col.Alpha(0.32)
			}
			seg := ui.Box(c.Key("seg:" + strconv.Itoa(s))).Grow(max(0.02, share)).Height(10).Radius(5).Background(col).
				Tooltip(fmt.Sprintf("%s · %s (%.1f%%)", seriesName(spec, s), formatChartValue(totals[k], spec.Unit), share*100))
			if seg.Hovered() {
				hoverIdx = s
			}
			if seg.Clicked() {
				if *lockedPtr == s {
					*lockedPtr = -1
				} else {
					*lockedPtr = s
				}
			}
		}
	})

	nextHover := -1
	ui.Column(c.Key("breakdown-rows")).FillWidth().Gap(4).Children(func() {
		for k, s := range indices {
			s := s
			series := spec.Series[s]
			val := totals[k]
			share := val / grand
			col := colors[s%len(colors)]
			isActive := activeIdx == s
			dim := activeIdx >= 0 && !isActive

			rowCol := col
			if dim {
				rowCol = col.Alpha(0.36)
			}

			row := ui.Row(c.Key("bd-row:"+strconv.Itoa(s))).FillWidth().Padding(8, 10).Gap(10).Radius(10).AlignItems(ui.Center)
			if isActive {
				row.Background(p.soft)
			} else if row.Hovered() {
				row.Background(p.soft.Alpha(0.7))
			}
			if row.Hovered() {
				nextHover = s
			}
			if row.Clicked() {
				if *lockedPtr == s {
					*lockedPtr = -1
				} else {
					*lockedPtr = s
				}
			}
			if series.AccessLabel != "" {
				row.Label(series.AccessLabel)
			} else {
				row.Label(fmt.Sprintf("%s %s", seriesName(spec, s), formatChartValue(val, spec.Unit)))
			}
			row.Children(func() {
				ui.Box(c).Size(9, 9).Radius(4.5).Background(rowCol).Shrink(0)
				ui.Column(c).Gap(4).Grow(1).MinWidth(0).Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Text(c, seriesName(spec, s)).FontSize(12.5).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…")
						sub := series.Subtitle
						if sub == "" {
							sub = fmt.Sprintf("占 %.1f%%", share*100)
						}
						ui.Text(c, sub).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…")
					})
					ui.Box(c).FillWidth().Height(4).Radius(2).Background(p.lineSoft.Alpha(0.7)).Draw(func(painter *ui.Painter, r ui.Rect) {
						fw := max(float32(4), r.W*float32(max(0, min(1, share))))
						painter.Fill(ui.Rect{X: r.X, Y: r.Y, W: fw, H: r.H}, rowCol, 2)
					})
				})

				if spark := breakdownSparkline(series); len(spark) >= 2 {
					ui.Box(c).Size(76, 24).Shrink(0).Draw(func(painter *ui.Painter, r ui.Rect) {
						lo, hi := math.Inf(1), math.Inf(-1)
						for _, v := range spark {
							if !math.IsNaN(v) {
								lo, hi = math.Min(lo, v), math.Max(hi, v)
							}
						}
						if math.IsInf(lo, 1) {
							return
						}
						if hi <= lo {
							hi = lo + 1
						}
						xs := make([]float32, len(spark))
						ys := make([]float32, len(spark))
						for i, v := range spark {
							xs[i] = float32(i) / float32(max(1, len(spark)-1))
							if math.IsNaN(v) {
								ys[i] = float32(math.NaN())
							} else {
								ys[i] = float32((v - lo) / (hi - lo))
							}
						}
						padR := ui.Rect{X: r.X + 2, Y: r.Y + 3, W: r.W - 4, H: r.H - 6}
						painter.FillPathGradient(chart.Area(padR, xs, nil, ys, chart.CurveMonotone), ui.LinearGradient{
							From:  rowCol.Alpha(0.22),
							To:    rowCol.Alpha(0.0),
							Angle: 180,
						})
						painter.StrokePath(chart.Line(padR, xs, ys, chart.CurveMonotone), 1.6, rowCol)
					})
				}

				ui.Column(c).Gap(1).AlignItems(ui.End).Shrink(0).Children(func() {
					ui.Text(c, formatChartValue(val, spec.Unit)).FontSize(13).Bold().FontFeatures("tnum").TextColor(p.ink).NoWrap()
					note := series.Badge
					if note == "" {
						note = fmt.Sprintf("%.1f%%", share*100)
					}
					ui.Text(c, note).FontSize(10.5).FontFeatures("tnum").TextColor(p.muted).NoWrap()
				})
			})
		}
	})
	*selected = nextHover
}

func pieChart(c *ui.Context, p palette, spec *chartSpec, colors []ui.Color, hoverPtr, lockedPtr *int) {
	values := make([]float64, len(spec.Labels))
	total := 0.0
	for i, v := range spec.Series[0].Data {
		if !math.IsNaN(v) && v > 0 {
			values[i] = v
			total += v
		}
	}
	if total == 0 {
		visualError(c, p, "饼图没有正数数据", "")
		return
	}
	slices := chart.Pie(values, 0.025)
	trackSlice := chart.Pie([]float64{1}, 0)[0]
	inner := float32(0)
	if spec.Type == "donut" {
		inner = 56
	}
	const size, outer = float32(184), float32(84)
	ui.Row(c).Gap(24).AlignItems(ui.Center).Children(func() {
		ring := ui.Box(c).Size(size, size).Shrink(0).Label(nonEmptyString(spec.Title, "饼图"))
		ringHover := -1
		if px, py, over := ring.PointerPosition(); over {
			if i, ok := chart.SliceAt(slices, px-size/2, py-size/2, inner, outer+4); ok {
				ringHover = i
			}
		}
		if ringHover >= 0 && ring.Clicked() {
			if *lockedPtr == ringHover {
				*lockedPtr = -1
			} else {
				*lockedPtr = ringHover
			}
		}
		active := *lockedPtr
		if ringHover >= 0 {
			active = ringHover
		} else if *hoverPtr >= 0 && *lockedPtr < 0 {
			active = *hoverPtr
		}
		if active >= 0 && active < len(spec.Labels) {
			ring.Tooltip(fmt.Sprintf("%s：%s（%.1f%%）", spec.Labels[active], formatChartValue(values[active], spec.Unit), values[active]/total*100))
		}
		ring.Draw(func(painter *ui.Painter, r ui.Rect) {
			if inner > 0 {
				painter.FillPath(chart.Sector(r.X+size/2, r.Y+size/2, inner, outer-2, trackSlice), p.soft)
			}
			for i, s := range slices {
				radius := outer - 2
				if i == active {
					radius = outer + 3
				}
				col := colors[i%len(colors)]
				if active >= 0 && i != active {
					col = col.Alpha(0.35)
				}
				painter.FillPath(chart.Sector(r.X+size/2, r.Y+size/2, inner, radius, s), col)
			}
		})
		if spec.Type == "donut" {
			ring.Children(func() {
				centerTop := nonEmptyString(spec.CenterLabel, formatChartValue(total, spec.Unit))
				centerSub := "总计"
				if spec.CenterLabel != "" {
					centerSub = ""
				}
				if active >= 0 && active < len(spec.Labels) {
					centerTop = fmt.Sprintf("%.1f%%", values[active]/total*100)
					centerSub = spec.Labels[active]
				}
				ui.Column(c).Fill().Justify(ui.Center).AlignItems(ui.Center).Padding(0, 14).Children(func() {
					ui.Text(c, centerTop).FontSize(17).Bold().FontFeatures("tnum").TextColor(p.ink).SingleLine().Ellipsis("…")
					if centerSub != "" {
						ui.Text(c, centerSub).FontSize(10).TextColor(p.muted).SingleLine().Ellipsis("…")
					}
				})
			})
		}
		nextLegHover := -1
		ui.Column(c).Gap(6).Grow(1).MinWidth(0).Children(func() {
			for i, label := range spec.Labels {
				i := i
				share := values[i] / total
				col := colors[i%len(colors)]
				if active >= 0 && i != active {
					col = col.Alpha(0.38)
				}
				row := ui.Row(c.Key("pie-leg:"+strconv.Itoa(i))).Gap(8).Padding(5, 8).Radius(8).AlignItems(ui.Center)
				if row.Hovered() {
					nextLegHover = i
					row.Background(p.soft)
				} else if active == i {
					row.Background(p.soft)
				}
				if row.Clicked() {
					if *lockedPtr == i {
						*lockedPtr = -1
					} else {
						*lockedPtr = i
					}
				}
				row.Children(func() {
					ui.Box(c).Size(8, 8).Radius(4).Background(col).Shrink(0)
					ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
						ui.Text(c, label).FontSize(12).FontWeight(500).TextColor(p.ink).SingleLine().Ellipsis("…")
						ui.Box(c).FillWidth().Height(3).Radius(1.5).Background(p.lineSoft.Alpha(0.65)).Draw(func(painter *ui.Painter, r ui.Rect) {
							painter.Fill(ui.Rect{X: r.X, Y: r.Y, W: max(float32(3), r.W*float32(share)), H: r.H}, col, 1.5)
						})
					})
					ui.Text(c, formatChartValue(values[i], spec.Unit)).FontSize(12).FontWeight(600).FontFeatures("tnum").TextColor(p.ink).NoWrap()
					ui.Text(c, fmt.Sprintf("%.1f%%", share*100)).FontSize(11).FontFeatures("tnum").TextColor(p.muted).Width(46).TextAlign(ui.End)
				})
			}
		})
		if ringHover >= 0 {
			*hoverPtr = ringHover
		} else {
			*hoverPtr = nextLegHover
		}
	})
}

func statCards(c *ui.Context, p palette, spec *chartSpec) {
	width, _ := c.Size()
	columns := 4
	if width < 860 {
		columns = 2
	}
	if width < 420 {
		columns = 1
	}
	for start := 0; start < len(spec.Items); start += columns {
		end := min(start+columns, len(spec.Items))
		ui.Row(c).Gap(10).FillWidth().AlignItems(ui.Stretch).Children(func() {
			for _, item := range spec.Items[start:end] {
				ui.Column(c).Grow(1).Basis(0).MinWidth(0).Gap(5).Padding(14, 16).Radius(14).Background(p.soft).Border(1, p.lineSoft).Children(func() {
					ui.Text(c, item.Label).FontSize(11).FontWeight(500).TextColor(p.muted).SingleLine().Ellipsis("…")
					ui.Text(c, item.Value).FontSize(24).Bold().FontFeatures("tnum").LetterSpacing(-0.4).TextColor(p.ink).SingleLine().Ellipsis("…")
					if item.Delta != "" {
						fg := p.muted
						bg := p.raised
						switch {
						case strings.HasPrefix(item.Delta, "+") || strings.HasPrefix(item.Delta, "↑"):
							fg = p.success
							bg = p.success.Alpha(0.13)
						case strings.HasPrefix(item.Delta, "-") || strings.HasPrefix(item.Delta, "−") || strings.HasPrefix(item.Delta, "↓"):
							fg = p.danger
							bg = p.danger.Alpha(0.13)
						}
						ui.Text(c, item.Delta).FontSize(10.5).FontWeight(600).FontFeatures("tnum").TextColor(fg).Padding(2, 7).Radius(6).Background(bg).NoWrap()
					}
					if item.Note != "" {
						ui.Text(c, item.Note).FontSize(10.5).TextColor(p.muted).Wrap()
					}
				})
			}
		})
	}
}

func chartLegend(c *ui.Context, p palette, spec *chartSpec, colors []ui.Color, hidden map[int]bool, onToggle func(int)) {
	ui.Row(c).Gap(6).Wrap().Children(func() {
		for s := range spec.Series {
			s := s
			off := hidden[s]
			swatch := colors[s%len(colors)]
			textColor := p.muted
			if off {
				swatch = swatch.Alpha(0.28)
				textColor = p.faint
			}
			item := ui.Button(c.Key("legend:"+strconv.Itoa(s)), "").Label("切换系列 "+seriesName(spec, s)).
				Height(22).Padding(0, 7).Radius(6).Border(0, p.line).Background(ui.Transparent)
			if item.Hovered() {
				item.Background(p.soft)
			}
			item.Children(func() {
				ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
					ui.Box(c).Size(8, 8).Radius(4).Background(swatch)
					lbl := ui.Text(c, seriesName(spec, s)).FontSize(11).TextColor(textColor)
					if off {
						lbl.Strikethrough()
					}
				})
			})
			if item.Clicked() && onToggle != nil {
				onToggle(s)
			}
		}
	})
}
