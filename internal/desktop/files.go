package desktop

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/muq9738/Vantry/pkg/api"
)

// Workbench actions served by Core's file reader. Responses arrive in the
// existing WorkbenchResponse envelope (Output, else Data) as JSON or as plain
// lines, so this view works with any Core that answers in either form.
const (
	actionFilesList   = "files.list"
	actionFilesRead   = "files.read"
	actionFilesSearch = "files.search"
	fileReadLimit     = 512 << 10
)

type fileEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

type fileBrowser struct {
	dir       map[string]string
	listings  map[string][]fileEntry
	listErr   map[string]string
	open      map[string]string
	views     map[string]*editor.State
	readErr   map[string]string
	showTree  map[string]bool
	source    map[string]bool
	markdowns map[string]*markdown.Options
}

type filePicker struct {
	index     int
	dismissed string
	results   map[string][]string
	errs      map[string]string
}

func (p *filePicker) reset() { p.index, p.dismissed = 0, "" }

// fileScope names the workspace a file request reads: a committed thread, or
// the project a local draft will start in.
func (w *workbench) fileScope(target string) (string, api.WorkbenchRequest, bool) {
	if w.hasThread(target) && !isLocalSession(target) {
		return "thread:" + target, api.WorkbenchRequest{ThreadID: target}, true
	}
	if spec := w.defaults.Workspace; spec != nil && spec.Path != "" {
		return "dir:" + spec.Path, api.WorkbenchRequest{Workspace: spec.Path}, true
	}
	return "", api.WorkbenchRequest{}, false
}

// ------------------------------------------------------------ decoding

func decodeFileEntries(response api.WorkbenchResponse) ([]fileEntry, bool) {
	for _, raw := range [][]byte{[]byte(response.Output), response.Data} {
		text := strings.TrimSpace(string(raw))
		if text == "" {
			continue
		}
		if entries, ok := decodeFileJSON([]byte(text)); ok {
			return entries, true
		}
		var entries []fileEntry
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			entries = append(entries, entryFromPath(line))
		}
		return entries, true
	}
	return nil, false
}

func decodeFileJSON(data []byte) ([]fileEntry, bool) {
	var wrapped struct {
		Entries json.RawMessage `json:"entries"`
		Files   json.RawMessage `json:"files"`
		Results json.RawMessage `json:"results"`
		Items   json.RawMessage `json:"items"`
	}
	if json.Unmarshal(data, &wrapped) == nil {
		for _, list := range []json.RawMessage{wrapped.Entries, wrapped.Files, wrapped.Results, wrapped.Items} {
			if len(list) > 0 {
				return decodeFileList(list)
			}
		}
	}
	return decodeFileList(data)
}

func decodeFileList(data json.RawMessage) ([]fileEntry, bool) {
	var names []string
	if json.Unmarshal(data, &names) == nil {
		entries := make([]fileEntry, 0, len(names))
		for _, name := range names {
			if strings.TrimSpace(name) != "" {
				entries = append(entries, entryFromPath(name))
			}
		}
		return entries, true
	}
	var objects []map[string]any
	if json.Unmarshal(data, &objects) != nil {
		return nil, false
	}
	entries := make([]fileEntry, 0, len(objects))
	for _, object := range objects {
		text := func(keys ...string) string {
			for _, key := range keys {
				if value, ok := object[key].(string); ok && value != "" {
					return value
				}
			}
			return ""
		}
		entry := fileEntry{Name: text("name"), Path: text("path", "relativePath", "file")}
		if entry.Path == "" {
			entry.Path = entry.Name
		}
		if entry.Path == "" {
			continue
		}
		if entry.Name == "" {
			entry.Name = path.Base(strings.TrimSuffix(entry.Path, "/"))
		}
		for _, key := range []string{"dir", "isDir", "directory"} {
			if value, ok := object[key].(bool); ok && value {
				entry.Dir = true
			}
		}
		if kind := text("type", "kind"); kind == "dir" || kind == "directory" || kind == "folder" {
			entry.Dir = true
		}
		if strings.HasSuffix(entry.Path, "/") {
			entry.Dir = true
		}
		entry.Path = strings.TrimSuffix(entry.Path, "/")
		if size, ok := object["size"].(float64); ok {
			entry.Size = int64(size)
		}
		entries = append(entries, entry)
	}
	return entries, true
}

func entryFromPath(p string) fileEntry {
	dir := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	return fileEntry{Name: path.Base(p), Path: p, Dir: dir}
}

func decodeFileContent(response api.WorkbenchResponse) string {
	text := response.Output
	if text == "" && len(response.Data) > 0 {
		text = string(response.Data)
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var payload struct {
			Content *string `json:"content"`
			Text    *string `json:"text"`
		}
		if json.Unmarshal([]byte(trimmed), &payload) == nil {
			if payload.Content != nil {
				return *payload.Content
			}
			if payload.Text != nil {
				return *payload.Text
			}
		}
	}
	if !utf8.ValidString(text) {
		return ""
	}
	return text
}

func sortEntries(entries []fileEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
}

// ------------------------------------------------------------ browser

func (w *workbench) fileDir(target string) string { return w.files.dir[target] }

func listingKey(scope, dir string) string { return scope + "\x00" + dir }

func (w *workbench) ensureFileListing() {
	scope, _, ok := w.fileScope(w.selected)
	if !ok {
		return
	}
	if _, loaded := w.files.listings[listingKey(scope, w.fileDir(w.selected))]; !loaded {
		w.loadFileListing(w.selected, w.fileDir(w.selected))
	}
}

func (w *workbench) loadFileListing(target, dir string) {
	scope, request, ok := w.fileScope(target)
	if !ok {
		return
	}
	request.Action = actionFilesList
	if dir != "" {
		request.Paths = []string{dir}
	}
	key := listingKey(scope, dir)
	w.workbenchCall("files.list:"+key, request, func(response api.WorkbenchResponse, err error) {
		if w.files.listings == nil {
			w.files.listings, w.files.listErr = map[string][]fileEntry{}, map[string]string{}
		}
		if err != nil {
			w.files.listings[key] = nil
			w.files.listErr[key] = describe("读取目录失败", err)
			return
		}
		entries, _ := decodeFileEntries(response)
		sortEntries(entries)
		for i := range entries {
			if !strings.Contains(entries[i].Path, "/") && dir != "" {
				entries[i].Path = path.Join(dir, entries[i].Path)
			}
		}
		w.files.listings[key] = entries
		delete(w.files.listErr, key)
	})
}

func (w *workbench) enterDir(target, dir string) {
	if w.files.dir == nil {
		w.files.dir = map[string]string{}
	}
	w.files.dir[target] = strings.Trim(dir, "/")
	w.ensureFileListing()
}

func (w *workbench) openFile(target, file string) {
	w.openFileAt(target, file, 0)
}

func (w *workbench) openFileAt(target, file string, line int) {
	scope, request, ok := w.fileScope(target)
	if !ok {
		return
	}
	if w.files.open == nil {
		w.files.open = map[string]string{}
	}
	w.files.open[target] = file
	if line > 0 {
		if w.files.source == nil {
			w.files.source = map[string]bool{}
		}
		w.files.source[listingKey(scope, file)] = true
	}
	key := listingKey(scope, file)
	if state := w.files.views[key]; state != nil {
		if line > 0 {
			state.GoTo(editor.Pos{Line: line - 1})
		}
		return
	}
	request.Action = actionFilesRead
	request.Paths = []string{file}
	w.workbenchCall("files.read:"+key, request, func(response api.WorkbenchResponse, err error) {
		if w.files.views == nil {
			w.files.views, w.files.readErr = map[string]*editor.State{}, map[string]string{}
		}
		if err != nil {
			w.files.readErr[key] = describe("读取文件失败", err)
			return
		}
		content := decodeFileContent(response)
		if content == "" && (len(response.Output) > 0 || len(response.Data) > 0) {
			w.files.readErr[key] = "该文件不是文本文件，暂不预览。"
			return
		}
		if len(content) > fileReadLimit {
			content = content[:fileReadLimit]
			for len(content) > 0 && !utf8.ValidString(content) {
				content = content[:len(content)-1]
			}
			content += "\n…（仅显示前 512 KiB）"
		}
		buffer := editor.NewBuffer(content)
		if lang := editor.LanguageFor(file); lang != nil {
			buffer.SetLanguage(lang)
		}
		state := &editor.State{Buffer: buffer, ReadOnly: true, NoFolding: true, NoActiveLine: true, NoScrollPastEnd: true, SoftWrap: true, LineHeight: 1.55}
		if line > 0 {
			state.GoTo(editor.Pos{Line: line - 1})
		}
		w.files.views[key] = state
		delete(w.files.readErr, key)
	})
}

// quotePath adds an @path reference to the end of the current draft.
func (w *workbench) quotePath(target, file string) {
	comp := w.composerFor(target)
	draft := comp.draft
	if draft != "" && !strings.HasSuffix(draft, " ") && !strings.HasSuffix(draft, "\n") {
		draft += " "
	}
	w.setDraft(target, draft+"@"+file+" ")
	w.widgets.composerFocus.Focus()
}

func (w *workbench) filesPanel(c *ui.Context, p palette) {
	target := w.selected
	scope, _, ok := w.fileScope(target)
	if !ok {
		ui.Column(c).Grow(1).Justify(ui.Center).AlignItems(ui.Center).Gap(6).Children(func() {
			ui.Icon(c, folderIcon).FontSize(20).TextColor(p.faint)
			ui.Text(c, "先选择项目目录，即可浏览并引用文件。").FontSize(12).TextColor(p.muted)
		})
		return
	}
	dir := w.fileDir(target)
	key := listingKey(scope, dir)
	entries, loaded := w.files.listings[key]
	if !loaded && !w.features.busy["files.list:"+key] {
		w.loadFileListing(target, dir)
		entries, loaded = w.files.listings[key]
	}
	showTree := w.files.open[target] == "" || w.files.showTree[target]
	ui.Row(c).Grow(1).MinHeight(0).MinWidth(0).Gap(8).AlignItems(ui.Stretch).Children(func() {
		if showTree {
			ui.Column(c.Key("file-tree")).Width(170).Shrink(0).Gap(2).Radius(8).Background(p.code).Border(1, p.lineSoft).Padding(4).Children(func() {
				ui.Row(c).Height(26).Gap(4).Padding(0, 4).AlignItems(ui.Center).Children(func() {
					label := "/" + dir
					ui.Text(c, label).Font("Menlo").FontSize(10).TextColor(p.muted).Grow(1).MinWidth(0).SingleLine().Ellipsis("…").Tooltip(label)
					if iconButton(c, p, "refresh-files", "刷新目录", refreshIcon).Size(22, 22).Padding(4).Disabled(w.features.busy["files.list:"+key]).Clicked() {
						delete(w.files.listings, key)
						w.loadFileListing(target, dir)
					}
				})
				ui.Scroll(c.Key("file-entries:" + key)).Grow(1).MinHeight(0).Children(func() {
					ui.Column(c).Gap(1).Role(ui.RoleList).Label("目录列表").Children(func() {
						if dir != "" {
							up := quietButton(c, p, "dir-up", "").Label("返回上级目录 ..").FillWidth().Height(24).Padding(2, 6).Justify(ui.Start)
							up.Children(func() {
								ui.Icon(c, folderUpIcon).FontSize(12)
								ui.Text(c, "..").Font("Menlo").FontSize(11)
							})
							if up.Clicked() {
								parent := path.Dir(dir)
								if parent == "." {
									parent = ""
								}
								w.enterDir(target, parent)
							}
						}
						switch {
						case !loaded || w.features.busy["files.list:"+key] && len(entries) == 0:
							ui.Row(c).Gap(6).Padding(6).AlignItems(ui.Center).Children(func() {
								ui.Spinner(c).Size(12, 12).Label("正在读取目录")
								ui.Text(c, "正在读取目录…").FontSize(11).TextColor(p.muted)
							})
						case w.files.listErr[key] != "":
							ui.Text(c, w.files.listErr[key]).FontSize(11).TextColor(p.warning).Wrap().Padding(6)
						case len(entries) == 0:
							ui.Text(c, "目录为空").FontSize(11).TextColor(p.muted).Padding(6)
						}
						for _, entry := range entries {
							entry := entry
							selected := !entry.Dir && w.files.open[target] == entry.Path
							row := quietButton(c, p, "entry:"+entry.Path, "").Label(entry.Path).FillWidth().Height(24).Padding(2, 6).Justify(ui.Start)
							if selected {
								row.Background(p.selected).TextColor(p.accentInk)
							}
							row.Children(func() {
								icon, color := fileIcon, p.muted
								if entry.Dir {
									icon, color = folderIcon, p.accentInk
								}
								ui.Icon(c, icon).FontSize(12).TextColor(color)
								ui.Text(c, entry.Name).FontSize(11).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
							})
							if row.Clicked() {
								if entry.Dir {
									w.enterDir(target, entry.Path)
								} else {
									w.openFile(target, entry.Path)
								}
							}
						}
					})
				})
			})
		}
		ui.Column(c.Key("file-reader")).Grow(1).MinWidth(0).MinHeight(0).Gap(6).Children(func() {
			file := w.files.open[target]
			if file == "" {
				ui.Column(c).Grow(1).Radius(8).Border(1, p.lineSoft).Justify(ui.Center).AlignItems(ui.Center).Gap(6).Children(func() {
					ui.Icon(c, fileIcon).FontSize(18).TextColor(p.faint)
					ui.Text(c, "选择左侧文件以阅读内容").FontSize(12).TextColor(p.muted)
				})
				return
			}
			readKey := listingKey(scope, file)
			ui.Row(c).Height(28).Gap(4).AlignItems(ui.Center).Children(func() {
				treeLabel := w.tr("显示目录", "Show folders")
				if showTree {
					treeLabel = w.tr("隐藏目录", "Hide folders")
				}
				if iconButton(c, p, "toggle-file-tree", treeLabel, folderIcon).Size(24, 24).Padding(5).Clicked() {
					if w.files.showTree == nil {
						w.files.showTree = map[string]bool{}
					}
					w.files.showTree[target] = !showTree
				}
				ui.Text(c, file).Font("Menlo").FontSize(11).TextColor(p.ink).Grow(1).MinWidth(0).SingleLine().Ellipsis("…").Tooltip(file).Selectable()
				if isMarkdownFile(file) {
					label := w.tr("查看源码", "View source")
					if w.files.source[readKey] {
						label = w.tr("阅读预览", "Read preview")
					}
					if quietButton(c, p, "toggle-file-source", label).FontSize(11).Height(24).Clicked() {
						if w.files.source == nil {
							w.files.source = map[string]bool{}
						}
						w.files.source[readKey] = !w.files.source[readKey]
					}
				}
				if quietButton(c, p, "copy-path", "复制路径").Height(24).Clicked() {
					c.WriteClipboard(file)
				}
				quote := quietButton(c, p, "quote-path", "").Label("引用路径到输入框").Height(24)
				quote.Children(func() {
					ui.Icon(c, quoteIcon).FontSize(12)
					ui.Text(c, "引用到输入框").FontSize(11)
				})
				if quote.Clicked() {
					w.quotePath(target, file)
				}
			})
			state := w.files.views[readKey]
			switch {
			case w.files.readErr[readKey] != "":
				ui.Text(c, w.files.readErr[readKey]).FontSize(11).TextColor(p.warning).Wrap()
			case state == nil:
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c).Size(12, 12).Label("正在读取文件")
					ui.Text(c, "正在读取文件…").FontSize(11).TextColor(p.muted)
				})
			default:
				if isMarkdownFile(file) && !w.files.source[readKey] {
					ui.Scroll(c.Key("file-markdown:"+readKey)).Grow(1).FillWidth().MinWidth(0).MinHeight(0).Padding(14, 16).Radius(8).Border(1, p.lineSoft).Background(p.canvas).Label("文件预览 " + file).Children(func() {
						markdown.View(c, state.Buffer.String(), w.fileMarkdownOptions(target, file, readKey))
					})
				} else {
					ui.Column(c).Grow(1).FillWidth().MinWidth(0).MinHeight(0).Radius(8).Border(1, p.lineSoft).Background(p.code).Children(func() {
						editor.View(c, state).Grow(1).FillWidth().MinWidth(0).Label("文件内容 " + file)
					})
				}
			}
		})
	})
}

// ------------------------------------------------------------ @ picker

// mentionQuery reports the @token being typed at the end of the draft. An @
// inside a word (an e-mail address) is not a mention.
func mentionQuery(draft string) (query string, start int, ok bool) {
	at := strings.LastIndexByte(draft, '@')
	if at < 0 {
		return "", 0, false
	}
	if at > 0 {
		r, _ := utf8.DecodeLastRuneInString(draft[:at])
		if !unicode.IsSpace(r) {
			return "", 0, false
		}
	}
	query = draft[at+1:]
	if strings.IndexFunc(query, unicode.IsSpace) >= 0 || utf8.RuneCountInString(query) > 80 {
		return "", 0, false
	}
	return query, at, true
}

func pickerKey(scope, query string) string { return scope + "\x00" + strings.ToLower(query) }

// pickerCandidates are the known paths for query: an exact search answer,
// else the nearest cached prefix answer and loaded listings, filtered.
func (w *workbench) pickerCandidates(scope, query string) ([]string, bool) {
	if results, ok := w.picker.results[pickerKey(scope, query)]; ok {
		return limitStrings(results, 8), true
	}
	lower := strings.ToLower(query)
	seen := map[string]bool{}
	var out []string
	add := func(candidate string) {
		if candidate != "" && !seen[candidate] && strings.Contains(strings.ToLower(candidate), lower) {
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	for q := query; q != ""; q = q[:len(q)-1] {
		if results, ok := w.picker.results[pickerKey(scope, q)]; ok {
			for _, r := range results {
				add(r)
			}
			break
		}
	}
	for key, entries := range w.files.listings {
		if !strings.HasPrefix(key, scope+"\x00") {
			continue
		}
		for _, entry := range entries {
			if !entry.Dir {
				add(entry.Path)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) < len(out[j]) })
	return limitStrings(out, 8), false
}

func limitStrings(values []string, n int) []string {
	if len(values) > n {
		return values[:n]
	}
	return values
}

func (w *workbench) searchFiles(scope string, request api.WorkbenchRequest, query string) {
	key := pickerKey(scope, query)
	if _, ok := w.picker.results[key]; ok {
		return
	}
	request.Action = actionFilesSearch
	request.Message = query
	w.workbenchCall("files.search:"+scope, request, func(response api.WorkbenchResponse, err error) {
		if w.picker.results == nil {
			w.picker.results, w.picker.errs = map[string][]string{}, map[string]string{}
		}
		if err != nil {
			w.picker.results[key] = nil
			w.picker.errs[scope] = "文件搜索暂不可用；可在“文件”面板中浏览后引用。"
			return
		}
		entries, _ := decodeFileEntries(response)
		paths := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.Dir {
				paths = append(paths, entry.Path)
			}
		}
		w.picker.results[key] = paths
		delete(w.picker.errs, scope)
	})
}

type pickerState struct {
	open       bool
	query      string
	start      int
	candidates []string
	exact      bool
	scope      string
}

func (w *workbench) pickerFor(target, draft string) pickerState {
	query, start, ok := mentionQuery(draft)
	if !ok || target == "" || w.picker.dismissed == draft[start:] {
		return pickerState{}
	}
	// A pasted attachment also reads "@name": the token at the caret is its
	// chip, not a file search. Suppress the picker while it ends the draft.
	for _, m := range w.composerFor(target).mentions {
		if m.display != "" && m.display == draft[start:] {
			return pickerState{}
		}
	}
	scope, request, ok := w.fileScope(target)
	if !ok {
		return pickerState{open: true, query: query, start: start}
	}
	candidates, exact := w.pickerCandidates(scope, query)
	if !exact && query != "" {
		w.searchFiles(scope, request, query)
		candidates, exact = w.pickerCandidates(scope, query)
	}
	if query == "" && len(candidates) == 0 {
		w.ensureFileListing()
	}
	if w.picker.index >= len(candidates) {
		w.picker.index = max(0, len(candidates)-1)
	}
	return pickerState{open: true, query: query, start: start, candidates: candidates, exact: exact, scope: scope}
}

func insertMention(draft string, start int, file string) string {
	return draft[:start] + "@" + file + " "
}

// pickerKey handles the picker's keys while it is open. It runs between
// frames from the composer's OnTextKey, against the text as typed.
func (w *workbench) pickerTextKey(target string, k ui.TextKey) bool {
	switch k.Key {
	case ui.KeyUp, ui.KeyDown, ui.KeyEnter, ui.KeyTab, ui.KeyEscape:
	default:
		return false
	}
	if k.Mods != 0 {
		return false
	}
	text := k.Text()
	state := w.pickerFor(target, text)
	if !state.open {
		return false
	}
	switch k.Key {
	case ui.KeyEscape:
		w.picker.dismissed = text[state.start:]
		return true
	case ui.KeyUp:
		if len(state.candidates) > 0 {
			w.picker.index = (w.picker.index - 1 + len(state.candidates)) % len(state.candidates)
		}
		return true
	case ui.KeyDown:
		if len(state.candidates) > 0 {
			w.picker.index = (w.picker.index + 1) % len(state.candidates)
		}
		return true
	}
	if len(state.candidates) == 0 {
		return k.Key == ui.KeyTab // Enter keeps its usual meaning without a choice
	}
	next := insertMention(text, state.start, state.candidates[min(w.picker.index, len(state.candidates)-1)])
	k.SetText(next)
	w.setDraft(target, next)
	w.picker.reset()
	// SetText clamps the caret to the old end: park it after the mention.
	comp := w.composerFor(target)
	comp.caretAfter = utf8.RuneCountInString(next)
	comp.caretRestore = true
	return true
}

// filePickerView floats the candidates just above the composer. It never
// takes focus: typing continues in the input while results arrive.
func (w *workbench) filePickerView(c *ui.Context, p palette, target, draft string) {
	state := w.pickerFor(target, draft)
	if !state.open {
		return
	}
	ui.Column(c.Key("file-picker")).FillWidth().Padding(5).Gap(1).Margin(0, 0, 6).Radius(12).Background(p.raised).Border(1, p.line).
		Role(ui.RoleListBox).Label("文件候选").Children(func() {
		ui.Row(c).Height(22).Padding(0, 7).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "@").Font("Menlo").FontSize(11).Bold().TextColor(p.accentInk)
			label := "引用工作区文件"
			if state.query != "" {
				label = "匹配 “" + state.query + "”"
			}
			ui.Text(c, label).FontSize(10).TextColor(p.muted).Grow(1).SingleLine().Ellipsis("…")
			ui.Text(c, "↑↓ 选择 · Enter 插入 · Esc 关闭").FontSize(10).TextColor(p.faint).NoWrap()
		})
		switch {
		case state.scope == "":
			ui.Text(c, "先选择项目目录，才能引用文件。").FontSize(11).TextColor(p.muted).Padding(4, 8)
			return
		case len(state.candidates) == 0 && w.features.busy["files.search:"+state.scope]:
			ui.Row(c).Gap(6).Padding(4, 8).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c).Size(11, 11).Label("正在搜索文件")
				ui.Text(c, "正在搜索文件…可继续输入").FontSize(11).TextColor(p.muted)
			})
			return
		case len(state.candidates) == 0:
			text := "没有匹配的文件"
			if errText := w.picker.errs[state.scope]; errText != "" {
				text = errText
			}
			ui.Text(c, text).FontSize(11).TextColor(p.muted).Padding(4, 8).Wrap()
			return
		}
		for i, candidate := range state.candidates {
			candidate := candidate
			option := ui.Button(c.Key("pick:"+candidate), "").Label("插入 @"+candidate).Role(ui.RoleOption).AccessSelected(i == w.picker.index).
				FillWidth().Height(26).Padding(2, 8).Border(0, p.line).Background(ui.Transparent).Radius(7).Justify(ui.Start)
			if i == w.picker.index || option.Hovered() {
				option.Background(p.selected)
			}
			option.Children(func() {
				ui.Icon(c, fileIcon).FontSize(12).TextColor(p.muted)
				ui.Text(c, path.Base(candidate)).FontSize(12).TextColor(p.ink).Shrink(0).MaxWidth(200).SingleLine().Ellipsis("…")
				if dir := path.Dir(candidate); dir != "." {
					ui.Text(c, dir).Font("Menlo").FontSize(10).TextColor(p.faint).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
				}
			})
			if option.Clicked() {
				next := insertMention(draft, state.start, candidate)
				w.setDraft(target, next)
				w.picker.reset()
				comp := w.composerFor(target)
				comp.caretAfter = utf8.RuneCountInString(next)
				comp.caretRestore = true
				w.widgets.composerFocus.Focus()
			}
		}
		if !state.exact && w.features.busy["files.search:"+state.scope] {
			ui.Text(c, fmt.Sprintf("已显示 %d 个本地匹配，正在搜索更多…", len(state.candidates))).FontSize(10).TextColor(p.faint).Padding(2, 8)
		}
	})
}
