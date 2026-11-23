package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// PanelSchemaVersion is the supported declarative native panel schema version.
	PanelSchemaVersion = 1
	// MaxPanelDepth bounds widget tree nesting (root widgets are depth 1).
	MaxPanelDepth = 6
	// MaxPanelNodes bounds total widgets and list/kv entries across one panel.
	MaxPanelNodes = 128

	maxPanelTitleBytes = 256
	maxPanelBadgeBytes = 256
	maxPanelKeyBytes   = 128
	maxPanelLangBytes  = 64
	maxPanelTextBytes  = 256 << 10
)

// PanelWidgetKind enumerates the declarative widgets supported by native UI panels.
type PanelWidgetKind string

const (
	WidgetText     PanelWidgetKind = "text"
	WidgetMarkdown PanelWidgetKind = "markdown"
	WidgetBadge    PanelWidgetKind = "badge"
	WidgetKV       PanelWidgetKind = "kv"
	WidgetList     PanelWidgetKind = "list"
	WidgetCode     PanelWidgetKind = "code"
)

// PanelKV is one key/value row in a "kv" panel widget.
type PanelKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// PanelWidget is one validated node in a declarative native panel tree.
type PanelWidget struct {
	Kind     PanelWidgetKind `json:"kind"`
	Key      string          `json:"key,omitempty"`
	Title    string          `json:"title,omitempty"`
	Text     string          `json:"text,omitempty"`
	Tone     string          `json:"tone,omitempty"`
	Language string          `json:"language,omitempty"`
	Entries  []PanelKV       `json:"entries,omitempty"`
	Items    []string        `json:"items,omitempty"`
	Children []PanelWidget   `json:"children,omitempty"`
}

// PanelView is the validated root of a plugin's declarative native panel output.
type PanelView struct {
	Version int           `json:"version"`
	Title   string        `json:"title,omitempty"`
	Widgets []PanelWidget `json:"widgets"`
}

type rawPanelView struct {
	Version *int              `json:"version"`
	Title   *string           `json:"title"`
	Widgets []json.RawMessage `json:"widgets"`
}

type panelParseState struct {
	nodes int
	keys  map[string]struct{}
}

var allowedPanelTones = map[string]struct{}{
	"":        {},
	"default": {},
	"neutral": {},
	"muted":   {},
	"info":    {},
	"accent":  {},
	"success": {},
	"warning": {},
	"danger":  {},
}

// ParsePanelOutput checks whether output is a valid { "vantryPanel": { "version": 1, "widgets": [...] } }
// payload. When valid, it returns the sanitized PanelView tree and true; otherwise nil and false
// so callers can safely fall back to plain-text rendering.
func ParsePanelOutput(output string) (*PanelView, bool) {
	view, err := ValidatePanelOutput(output)
	if err != nil {
		return nil, false
	}
	return view, true
}

// ValidatePanelOutput parses and validates a declarative panel JSON payload,
// returning a descriptive error when the payload is not a valid panel view.
func ValidatePanelOutput(output string) (*PanelView, error) {
	trimmed := strings.TrimSpace(output)
	if len(trimmed) == 0 || len(trimmed) > maxJSONBytes || trimmed[0] != '{' || !utf8.ValidString(trimmed) {
		return nil, errors.New("输出不是有效的面板 JSON 对象")
	}

	var topFields map[string]json.RawMessage
	if err := decodeStrictObject([]byte(trimmed), &topFields); err != nil {
		return nil, fmt.Errorf("解析面板顶层对象：%w", err)
	}
	rawPanel, ok := topFields["vantryPanel"]
	if !ok || len(topFields) != 1 {
		return nil, errors.New("面板顶层必须且只能包含 vantryPanel 字段")
	}

	dec := json.NewDecoder(bytes.NewReader(rawPanel))
	dec.DisallowUnknownFields()
	var raw rawPanelView
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析 vantryPanel：%w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, fmt.Errorf("解析 vantryPanel 尾部数据：%w", err)
	}
	if raw.Version == nil || *raw.Version != PanelSchemaVersion {
		return nil, fmt.Errorf("不支持的面板协议版本（仅支持 version=%d）", PanelSchemaVersion)
	}
	var title string
	if raw.Title != nil {
		if len(*raw.Title) > maxPanelTitleBytes || !utf8.ValidString(*raw.Title) {
			return nil, errors.New("面板标题超过长度上限或包含非法 UTF-8")
		}
		title = *raw.Title
	}
	if len(raw.Widgets) == 0 {
		return nil, errors.New("vantryPanel.widgets 不能为空")
	}
	if len(raw.Widgets) > MaxPanelNodes {
		return nil, fmt.Errorf("面板节点数超过上限 %d", MaxPanelNodes)
	}

	state := &panelParseState{
		keys: make(map[string]struct{}, len(raw.Widgets)),
	}
	widgets := make([]PanelWidget, 0, len(raw.Widgets))
	for i, item := range raw.Widgets {
		fallbackKey := fmt.Sprintf("w-%d", i)
		w, err := parsePanelWidget(item, 1, fallbackKey, state)
		if err != nil {
			return nil, fmt.Errorf("widgets[%d]：%w", i, err)
		}
		widgets = append(widgets, w)
	}

	return &PanelView{
		Version: PanelSchemaVersion,
		Title:   title,
		Widgets: widgets,
	}, nil
}

func parsePanelWidget(raw json.RawMessage, depth int, fallbackKey string, state *panelParseState) (PanelWidget, error) {
	if depth > MaxPanelDepth {
		return PanelWidget{}, fmt.Errorf("面板嵌套深度超过上限 %d", MaxPanelDepth)
	}
	state.nodes++
	if state.nodes > MaxPanelNodes {
		return PanelWidget{}, fmt.Errorf("面板总节点数超过上限 %d", MaxPanelNodes)
	}

	var fields map[string]json.RawMessage
	if err := decodeStrictObject(raw, &fields); err != nil {
		return PanelWidget{}, fmt.Errorf("组件必须是 JSON 对象：%w", err)
	}

	kindStr, ok, err := readOptionalStringField(fields, "kind", 32)
	if err != nil || !ok || kindStr == "" {
		return PanelWidget{}, errors.New("组件必须包含有效的 kind 字符串")
	}
	kind := PanelWidgetKind(kindStr)

	explicitKey, hasKey, err := readOptionalStringField(fields, "key", maxPanelKeyBytes)
	if err != nil {
		return PanelWidget{}, fmt.Errorf("key 字段非法：%w", err)
	}
	var key string
	if hasKey {
		if strings.TrimSpace(explicitKey) == "" {
			return PanelWidget{}, errors.New("组件 key 不能为空白字符串")
		}
		key = explicitKey
	} else {
		key = fallbackKey
	}
	if _, exists := state.keys[key]; exists {
		return PanelWidget{}, fmt.Errorf("组件 key %q 重复", key)
	}
	state.keys[key] = struct{}{}

	widget := PanelWidget{
		Kind: kind,
		Key:  key,
	}

	switch kind {
	case WidgetText:
		if err := ensureAllowedFields(fields, "kind", "key", "title", "text", "tone"); err != nil {
			return PanelWidget{}, err
		}
		title, _, err := readOptionalStringField(fields, "title", maxPanelTitleBytes)
		if err != nil {
			return PanelWidget{}, err
		}
		text, hasText, err := readOptionalStringField(fields, "text", maxPanelTextBytes)
		if err != nil || !hasText || len(text) == 0 {
			return PanelWidget{}, errors.New("text 组件必须包含非空的 text 字符串")
		}
		tone, err := readToneField(fields)
		if err != nil {
			return PanelWidget{}, err
		}
		widget.Title = title
		widget.Text = text
		widget.Tone = tone

	case WidgetMarkdown:
		if err := ensureAllowedFields(fields, "kind", "key", "title", "markdown", "text"); err != nil {
			return PanelWidget{}, err
		}
		title, _, err := readOptionalStringField(fields, "title", maxPanelTitleBytes)
		if err != nil {
			return PanelWidget{}, err
		}
		content, err := readAliasedRequiredString(fields, "markdown", "text", maxPanelTextBytes)
		if err != nil {
			return PanelWidget{}, fmt.Errorf("markdown 组件：%w", err)
		}
		widget.Title = title
		widget.Text = content

	case WidgetBadge:
		if err := ensureAllowedFields(fields, "kind", "key", "label", "text", "tone"); err != nil {
			return PanelWidget{}, err
		}
		label, err := readAliasedRequiredString(fields, "label", "text", maxPanelBadgeBytes)
		if err != nil || strings.TrimSpace(label) == "" {
			return PanelWidget{}, errors.New("badge 组件必须包含非空白的 label 或 text 字符串")
		}
		tone, err := readToneField(fields)
		if err != nil {
			return PanelWidget{}, err
		}
		widget.Text = label
		widget.Tone = tone

	case WidgetKV:
		if err := ensureAllowedFields(fields, "kind", "key", "title", "entries", "items"); err != nil {
			return PanelWidget{}, err
		}
		title, _, err := readOptionalStringField(fields, "title", maxPanelTitleBytes)
		if err != nil {
			return PanelWidget{}, err
		}
		rawEntries, hasEntries := fields["entries"]
		rawItems, hasItems := fields["items"]
		if hasEntries == hasItems {
			return PanelWidget{}, errors.New("kv 组件必须提供 entries（或 items）数组且不能同时提供")
		}
		if !hasEntries {
			rawEntries = rawItems
		}
		entries, err := parseKVEntries(rawEntries, state)
		if err != nil {
			return PanelWidget{}, err
		}
		widget.Title = title
		widget.Entries = entries

	case WidgetList:
		if err := ensureAllowedFields(fields, "kind", "key", "title", "items", "children"); err != nil {
			return PanelWidget{}, err
		}
		title, _, err := readOptionalStringField(fields, "title", maxPanelTitleBytes)
		if err != nil {
			return PanelWidget{}, err
		}
		rawItems, hasItems := fields["items"]
		rawChildren, hasChildren := fields["children"]
		if hasItems == hasChildren {
			return PanelWidget{}, errors.New("list 组件必须提供 items 或 children 数组之一")
		}
		widget.Title = title
		if hasChildren {
			children, err := parseChildWidgets(rawChildren, depth+1, key, state)
			if err != nil {
				return PanelWidget{}, err
			}
			widget.Children = children
		} else {
			items, children, err := parseListItems(rawItems, depth+1, key, state)
			if err != nil {
				return PanelWidget{}, err
			}
			widget.Items = items
			widget.Children = children
		}

	case WidgetCode:
		if err := ensureAllowedFields(fields, "kind", "key", "title", "code", "text", "language"); err != nil {
			return PanelWidget{}, err
		}
		title, _, err := readOptionalStringField(fields, "title", maxPanelTitleBytes)
		if err != nil {
			return PanelWidget{}, err
		}
		codeText, err := readAliasedRequiredString(fields, "code", "text", maxPanelTextBytes)
		if err != nil {
			return PanelWidget{}, fmt.Errorf("code 组件：%w", err)
		}
		lang, hasLang, err := readOptionalStringField(fields, "language", maxPanelLangBytes)
		if err != nil {
			return PanelWidget{}, err
		}
		if hasLang && !isValidLanguageTag(lang) {
			return PanelWidget{}, fmt.Errorf("code 组件 language %q 非法", lang)
		}
		widget.Title = title
		widget.Text = codeText
		widget.Language = lang

	default:
		return PanelWidget{}, fmt.Errorf("未知面板组件类型 %q", kind)
	}

	return widget, nil
}

func parseKVEntries(raw json.RawMessage, state *panelParseState) ([]PanelKV, error) {
	elems, err := decodeStrictArray(raw)
	if err != nil {
		return nil, fmt.Errorf("kv entries 必须是数组：%w", err)
	}
	if len(elems) == 0 {
		return nil, errors.New("kv entries 不能为空")
	}
	out := make([]PanelKV, 0, len(elems))
	for i, elem := range elems {
		state.nodes++
		if state.nodes > MaxPanelNodes {
			return nil, fmt.Errorf("面板总节点数超过上限 %d", MaxPanelNodes)
		}
		var itemFields map[string]json.RawMessage
		if err := decodeStrictObject(elem, &itemFields); err != nil {
			return nil, fmt.Errorf("kv[%d] 必须是对象：%w", i, err)
		}
		if err := ensureAllowedFields(itemFields, "key", "value"); err != nil {
			return nil, fmt.Errorf("kv[%d]：%w", i, err)
		}
		k, hasK, err := readOptionalStringField(itemFields, "key", maxPanelTitleBytes)
		if err != nil || !hasK || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("kv[%d].key 必须是非空白字符串", i)
		}
		v, hasV, err := readOptionalStringField(itemFields, "value", maxPanelTextBytes)
		if err != nil || !hasV {
			return nil, fmt.Errorf("kv[%d].value 必须是字符串", i)
		}
		out = append(out, PanelKV{Key: k, Value: v})
	}
	return out, nil
}

func parseChildWidgets(raw json.RawMessage, depth int, parentKey string, state *panelParseState) ([]PanelWidget, error) {
	elems, err := decodeStrictArray(raw)
	if err != nil {
		return nil, fmt.Errorf("children 必须是数组：%w", err)
	}
	children := make([]PanelWidget, 0, len(elems))
	for i, elem := range elems {
		childKey := fmt.Sprintf("%s.%d", parentKey, i)
		child, err := parsePanelWidget(elem, depth, childKey, state)
		if err != nil {
			return nil, fmt.Errorf("children[%d]：%w", i, err)
		}
		children = append(children, child)
	}
	return children, nil
}

func parseListItems(raw json.RawMessage, childDepth int, parentKey string, state *panelParseState) ([]string, []PanelWidget, error) {
	elems, err := decodeStrictArray(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("list items 必须是数组：%w", err)
	}
	if len(elems) == 0 {
		return []string{}, nil, nil
	}
	first := bytes.TrimSpace(elems[0])
	if len(first) == 0 {
		return nil, nil, errors.New("list items 包含空元素")
	}
	switch first[0] {
	case '"':
		items := make([]string, 0, len(elems))
		for i, elem := range elems {
			state.nodes++
			if state.nodes > MaxPanelNodes {
				return nil, nil, fmt.Errorf("面板总节点数超过上限 %d", MaxPanelNodes)
			}
			s, err := decodeStrictString(elem, maxPanelTextBytes)
			if err != nil {
				return nil, nil, fmt.Errorf("items[%d] 必须是字符串：%w", i, err)
			}
			items = append(items, s)
		}
		return items, nil, nil
	case '{':
		children := make([]PanelWidget, 0, len(elems))
		for i, elem := range elems {
			childKey := fmt.Sprintf("%s.%d", parentKey, i)
			child, err := parsePanelWidget(elem, childDepth, childKey, state)
			if err != nil {
				return nil, nil, fmt.Errorf("items[%d]：%w", i, err)
			}
			children = append(children, child)
		}
		return nil, children, nil
	default:
		return nil, nil, errors.New("list items 元素必须是字符串或子组件对象")
	}
}

func ensureAllowedFields(fields map[string]json.RawMessage, allowed ...string) error {
	for name := range fields {
		found := false
		for _, okName := range allowed {
			if name == okName {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("包含未授权或未知属性 %q", name)
		}
	}
	return nil
}

func readOptionalStringField(fields map[string]json.RawMessage, name string, maxBytes int) (string, bool, error) {
	raw, ok := fields[name]
	if !ok {
		return "", false, nil
	}
	val, err := decodeStrictString(raw, maxBytes)
	if err != nil {
		return "", true, fmt.Errorf("字段 %q：%w", name, err)
	}
	return val, true, nil
}

func readAliasedRequiredString(fields map[string]json.RawMessage, primary, secondary string, maxBytes int) (string, error) {
	pVal, hasP, err := readOptionalStringField(fields, primary, maxBytes)
	if err != nil {
		return "", err
	}
	sVal, hasS, err := readOptionalStringField(fields, secondary, maxBytes)
	if err != nil {
		return "", err
	}
	if hasP && hasS {
		return "", fmt.Errorf("不能同时指定 %q 与 %q", primary, secondary)
	}
	if hasP {
		if len(pVal) == 0 {
			return "", fmt.Errorf("字段 %q 不能为空", primary)
		}
		return pVal, nil
	}
	if hasS {
		if len(sVal) == 0 {
			return "", fmt.Errorf("字段 %q 不能为空", secondary)
		}
		return sVal, nil
	}
	return "", fmt.Errorf("必须提供非空的 %q 或 %q 字符串", primary, secondary)
}

func readToneField(fields map[string]json.RawMessage) (string, error) {
	tone, hasTone, err := readOptionalStringField(fields, "tone", 32)
	if err != nil {
		return "", err
	}
	if !hasTone {
		return "", nil
	}
	if _, ok := allowedPanelTones[tone]; !ok {
		return "", fmt.Errorf("不支持的 tone 值 %q", tone)
	}
	return tone, nil
}

func isValidLanguageTag(lang string) bool {
	for _, r := range lang {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func decodeStrictString(raw json.RawMessage, maxBytes int) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '"' {
		return "", errors.New("必须是 JSON 字符串")
	}
	var s string
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&s); err != nil {
		return "", err
	}
	if err := ensureEOF(dec); err != nil {
		return "", err
	}
	if len(s) > maxBytes || !utf8.ValidString(s) {
		return "", fmt.Errorf("字符串超过 %d 字节上限或包含非法 UTF-8", maxBytes)
	}
	return s, nil
}

func decodeStrictObject(raw []byte, out *map[string]json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return errors.New("不是 JSON 对象")
	}
	// Reject duplicate keys in the same JSON object so ambiguous payloads cannot
	// slip past field validation.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return errors.New("不是 JSON 对象")
	}
	result := make(map[string]json.RawMessage)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok || !utf8.ValidString(key) {
			return errors.New("对象键名非法")
		}
		if _, dup := result[key]; dup {
			return fmt.Errorf("对象包含重复键 %q", key)
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return err
		}
		result[key] = val
	}
	endTok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := endTok.(json.Delim); !ok || delim != '}' {
		return errors.New("对象未正常闭合")
	}
	if err := ensureEOF(dec); err != nil {
		return err
	}
	*out = result
	return nil
}

func decodeStrictArray(raw json.RawMessage) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '[' {
		return nil, errors.New("不是 JSON 数组")
	}
	var arr []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&arr); err != nil {
		return nil, err
	}
	if err := ensureEOF(dec); err != nil {
		return nil, err
	}
	return arr, nil
}

func ensureEOF(dec *json.Decoder) error {
	if dec.More() {
		return errors.New("存在多余 JSON 数据")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("存在多余 JSON 尾部数据")
	}
	return nil
}
