package desktop

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

type toolKind uint8

const (
	toolOther toolKind = iota
	toolCommand
	toolRead
	toolEdit
	toolWrite
	toolSearch
	toolList
)

type toolEditText struct{ oldText, newText string }

// toolView is how a timeline shows one tool call: a verb and its target, as
// the workbench's work rows read ("运行 go test ./..."), and what its details hold.
type toolView struct {
	kind         toolKind
	verb, target string
	command      string
	path         string
	content      string
	edits        []toolEditText
	added        int
	removed      int
	args         map[string]any
}

type toolViewEntry struct {
	updated   time.Time
	arguments string
	view      toolView
}

// toolView parses a call's arguments once per revision of the call.
func (w *workbench) toolView(tool api.ToolCall) toolView {
	if w.toolViews == nil {
		w.toolViews = map[string]toolViewEntry{}
	}
	if entry, ok := w.toolViews[tool.ID]; ok && entry.updated.Equal(tool.UpdatedAt) && entry.arguments == string(tool.Arguments) {
		return entry.view
	}
	view := classifyTool(tool)
	w.toolViews[tool.ID] = toolViewEntry{updated: tool.UpdatedAt, arguments: string(tool.Arguments), view: view}
	return view
}

func stringArg(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func classifyTool(tool api.ToolCall) toolView {
	view := toolView{}
	if len(tool.Arguments) > 0 {
		_ = json.Unmarshal(tool.Arguments, &view.args)
	}
	name := strings.ToLower(tool.Name)
	args := view.args
	view.path = stringArg(args, "path", "file_path", "filePath", "file")
	switch {
	case name == "bash" || name == "shell" || name == "exec" || strings.HasSuffix(name, "command"):
		view.kind, view.verb = toolCommand, "运行"
		view.command = stringArg(args, "command", "cmd")
		view.target = firstLine(view.command)
	case name == "read" || name == "read_file" || name == "view":
		view.kind, view.verb, view.target = toolRead, "读取", view.path
	case name == "edit" || name == "multiedit" || name == "str_replace" || name == "apply_patch":
		view.kind, view.verb, view.target = toolEdit, "编辑", view.path
		if edits, ok := args["edits"].([]any); ok {
			for _, raw := range edits {
				if edit, ok := raw.(map[string]any); ok {
					view.edits = append(view.edits, toolEditText{stringArg(edit, "oldText", "old_string", "old"), stringArg(edit, "newText", "new_string", "new")})
				}
			}
		}
		if old, nw := stringArg(args, "oldText", "old_string"), stringArg(args, "newText", "new_string"); old != "" || nw != "" {
			view.edits = append(view.edits, toolEditText{old, nw})
		}
		for _, edit := range view.edits {
			added, removed := lineDelta(edit.oldText, edit.newText)
			view.added += added
			view.removed += removed
		}
	case name == "write" || name == "write_file" || name == "create":
		view.kind, view.verb, view.target = toolWrite, "写入", view.path
		view.content = stringArg(args, "content", "text")
		view.added = lineCount(view.content)
	case name == "grep" || name == "search" || name == "rg" || name == "ffgrep" || strings.HasSuffix(name, "_ffgrep"):
		view.kind, view.verb = toolSearch, "搜索"
		view.target = stringArg(args, "pattern", "query", "regex")
		if where := stringArg(args, "path"); where != "" && view.target != "" {
			view.target += " · " + where
		}
	case name == "glob" || name == "find" || name == "fffind" || strings.HasSuffix(name, "_fffind"):
		view.kind, view.verb = toolList, "查找文件"
		view.target = nonEmptyString(stringArg(args, "pattern", "glob", "path"), ".")
		if where := stringArg(args, "path"); where != "" && where != view.target {
			view.target += " · " + where
		}
	case name == "ls" || name == "list" || name == "list_dir":
		view.kind, view.verb, view.target = toolList, "列出", nonEmptyString(view.path, ".")
	case name == "codemode" || strings.HasSuffix(name, "_codemode"):
		view.kind, view.verb = toolCommand, "批处理"
		view.command = stringArg(args, "code")
		view.target = nonEmptyString(stringArg(args, "toolSummary", "summary"), firstLine(view.command))
	case name == "tool_search" || strings.HasSuffix(name, "_tool_search"):
		view.kind, view.verb = toolSearch, "发现工具"
		view.target = stringArg(args, "query", "toolSummary")
	case strings.Contains(name, "delegate_task"):
		view.kind, view.verb = toolOther, "委派子代理"
		view.target = nonEmptyString(stringArg(args, "title", "toolSummary"), firstLine(stringArg(args, "task")))
	case strings.Contains(name, "task_status"):
		view.kind, view.verb = toolOther, "读取子代理状态"
		view.target = stringArg(args, "taskId", "toolSummary")
	case strings.Contains(name, "task_cancel"):
		view.kind, view.verb = toolOther, "停止子代理"
		view.target = stringArg(args, "taskId", "reason")
	case strings.Contains(name, "orchestrator_capabilities") || strings.Contains(name, "environment_list"):
		view.kind, view.verb = toolOther, "查询环境能力"
		view.target = stringArg(args, "search", "environmentId", "toolSummary")
	default:
		cleanName := strings.TrimPrefix(strings.TrimPrefix(tool.Name, "mcp_ide-tools_"), "vantry_")
		view.kind, view.verb = toolOther, "调用 "+cleanName
		view.target = stringArg(args, "toolSummary", "path", "command", "query", "url", "title", "message")
	}
	view.target = firstLine(view.target)
	return view
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return strings.TrimSpace(text[:i]) + " …"
	}
	return text
}

func lineCount(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
}

// lineDelta counts the lines an edit adds and removes, past the lines its
// old and new texts share at their starts and ends.
func lineDelta(oldText, newText string) (added, removed int) {
	a := strings.Split(oldText, "\n")
	b := strings.Split(newText, "\n")
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	endA, endB := len(a), len(b)
	for endA > start && endB > start && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	if oldText == "" {
		endA = start
	}
	return endB - start, endA - start
}

func (w *workbench) relativePath(threadID, path string) string {
	if path == "" {
		return path
	}
	root := ""
	if thread, ok := w.threadByID(threadID); ok && thread.Workspace != nil {
		root = nonEmptyString(thread.Workspace.Path, thread.Workspace.Repository)
	}
	if root != "" && filepath.IsAbs(path) {
		if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return path
}

func toolKindIcon(kind toolKind) *ui.SVG {
	switch kind {
	case toolCommand:
		return terminalIcon
	case toolRead:
		return fileIcon
	case toolEdit, toolWrite:
		return editIcon
	case toolSearch:
		return searchIcon
	case toolList:
		return listIcon
	}
	return toolIcon
}

// workSummary says what a group of calls did, as "读取 3 个文件 · 运行 2 条命令".
func (w *workbench) workSummary(tools []api.ToolCall) string {
	// T3 Code's WorkLog sentence: group the calls by action, name the two
	// most telling groups (commands and file changes first, then reads and
	// searches), and fold everything else into "N other actions".
	type group struct {
		action   string
		priority int
		count    int
		paths    map[string]bool
	}
	var groups []*group
	byAction := map[string]*group{}
	for _, tool := range tools {
		view := w.toolView(tool)
		action, priority := "other", 2
		switch view.kind {
		case toolCommand:
			action, priority = "command", 0
		case toolEdit, toolWrite:
			action, priority = "edit", 0
		case toolRead:
			action, priority = "read", 1
		case toolSearch:
			action, priority = "search", 1
		case toolList:
			action, priority = "list", 1
		}
		g := byAction[action]
		if g == nil {
			g = &group{action: action, priority: priority, paths: map[string]bool{}}
			byAction[action] = g
			groups = append(groups, g)
		}
		g.count++
		if action == "edit" && view.path != "" {
			g.paths[view.path] = true
		}
	}
	chosen := append([]*group(nil), groups...)
	sort.SliceStable(chosen, func(i, j int) bool { return chosen[i].priority < chosen[j].priority })
	if len(chosen) > 2 {
		chosen = chosen[:2]
	}
	named := map[*group]bool{}
	for _, g := range chosen {
		named[g] = true
	}
	var parts []string
	rest := 0
	for _, g := range groups {
		if !named[g] {
			rest += g.count
			continue
		}
		n := g.count
		if g.action == "edit" && len(g.paths) > 0 {
			n = len(g.paths)
		}
		switch g.action {
		case "command":
			parts = append(parts, fmt.Sprintf(w.tr("运行 %d 条命令", plural(n, "Ran %d command", "Ran %d commands")), n))
		case "edit":
			parts = append(parts, fmt.Sprintf(w.tr("修改 %d 个文件", plural(n, "changed %d file", "changed %d files")), n))
		case "read":
			parts = append(parts, fmt.Sprintf(w.tr("读取 %d 个文件", plural(n, "read %d file", "read %d files")), n))
		case "search":
			parts = append(parts, fmt.Sprintf(w.tr("搜索代码 %d 次", plural(n, "searched code %d time", "searched code %d times")), n))
		case "list":
			parts = append(parts, fmt.Sprintf(w.tr("查看目录 %d 次", plural(n, "listed files %d time", "listed files %d times")), n))
		default:
			parts = append(parts, fmt.Sprintf(w.tr("调用 %d 个工具", plural(n, "used %d tool", "used %d tools")), n))
		}
	}
	if rest > 0 {
		parts = append(parts, fmt.Sprintf(w.tr("另有 %d 项操作", plural(rest, "performed %d other action", "performed %d other actions")), rest))
	}
	if len(parts) == 0 {
		return ""
	}
	if w.english() {
		parts[0] = strings.ToUpper(parts[0][:1]) + parts[0][1:]
		switch len(parts) {
		case 1:
			return parts[0]
		case 2:
			return parts[0] + " and " + parts[1]
		default:
			return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
		}
	}
	return strings.Join(parts, "，")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

type changedFile struct {
	path           string
	added, removed int
	tools          []api.ToolCall
}

// changedFiles lists the files a turn's edit and write calls changed, in the
// order first changed, as the workbench's changed-files summary under a reply.
func (w *workbench) changedFiles(tools []api.ToolCall) []changedFile {
	var files []changedFile
	index := map[string]int{}
	for _, tool := range tools {
		if tool.Status == "failed" {
			continue
		}
		view := w.toolView(tool)
		if view.kind != toolEdit && view.kind != toolWrite || view.path == "" {
			continue
		}
		i, ok := index[view.path]
		if !ok {
			i = len(files)
			index[view.path] = i
			files = append(files, changedFile{path: view.path})
		}
		files[i].added += view.added
		files[i].removed += view.removed
		files[i].tools = append(files[i].tools, tool)
	}
	return files
}

var toolVerbsEN = map[string]string{
	"运行": "Ran", "读取": "Read", "编辑": "Edited", "写入": "Wrote", "搜索": "Searched",
	"查找文件": "Found files", "列出": "Listed", "批处理": "Batched", "发现工具": "Found tools",
	"委派子代理": "Delegated", "读取子代理状态": "Checked task", "停止子代理": "Stopped task",
	"查询环境能力": "Checked capabilities",
}

// toolVerb is the step's verb in the interface language.
func (w *workbench) toolVerb(view toolView) string {
	if !w.english() {
		return view.verb
	}
	if en, ok := toolVerbsEN[view.verb]; ok {
		return en
	}
	if rest, ok := strings.CutPrefix(view.verb, "调用 "); ok {
		return "Called " + rest
	}
	return view.verb
}
