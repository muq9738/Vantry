package desktop

import (
	_ "embed"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo/ui"
)

//go:embed icons/avatar.png
var avatarPNG []byte

var avatarImage, _ = ui.DecodeBitmap(avatarPNG)

func strokeIcon(path string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + path + `</svg>`))
}

// lucide renders named Lucide geometry with a consistent stroke (lucide-react
// defaults: 24-unit box, 2-unit round stroke), drawn natively by MyGo. An
// unknown name panics at start-up, so a typo cannot ship.
func lucide(name string) *ui.SVG {
	markup, ok := lucideMarkup[name]
	if !ok {
		panic("desktop: unknown Lucide icon " + name)
	}
	return strokeIcon(markup)
}

var (
	searchIcon       = lucide("search")
	composeIcon      = lucide("square-pen")
	folderIcon       = lucide("folder")
	chevronIcon      = lucide("chevron-down")
	rightIcon        = lucide("chevron-right")
	closeIcon        = lucide("x")
	thinkingIcon     = lucide("brain")
	branchIcon       = lucide("git-branch")
	terminalIcon     = lucide("square-terminal")
	moreIcon         = lucide("ellipsis")
	checkIcon        = lucide("check")
	shieldIcon       = lucide("shield-check")
	agentsIcon       = lucide("bot")
	moonIcon         = lucide("moon")
	sunIcon          = lucide("sun")
	expandIcon       = lucide("maximize-2")
	copyIcon         = lucide("copy")
	brainIcon        = lucide("brain")
	fileIcon         = lucide("file")
	editIcon         = lucide("pen-line")
	listIcon         = lucide("list")
	toolIcon         = lucide("wrench")
	alertIcon        = lucide("circle-alert")
	pinIcon          = lucide("pin")
	archiveIcon      = lucide("archive")
	gearIcon         = lucide("settings")
	chartIcon        = lucide("chart-no-axes-column")
	remoteIcon       = lucide("monitor")
	gridIcon         = lucide("blocks")
	sidebarRightIcon = lucide("panel-right")
	sparkIcon        = lucide("sparkles")
	worktreeIcon     = lucide("folder-git-2")
	settleIcon       = lucide("inbox")

	attachmentIcon   = lucide("paperclip")
	queueIcon        = lucide("list-plus")
	steerIcon        = lucide("corner-down-right")
	composerSendIcon = lucide("arrow-up")
	composerStopIcon = lucide("square")
)

func iconButton(c *ui.Context, p palette, key, label string, icon *ui.SVG) ui.Element {
	return kitIconButton(c, p, key, label, icon, controlGhost)
}

func quietButton(c *ui.Context, p palette, key, label string) ui.Element {
	return kitButton(c, p, key, label, controlGhost, controlSM)
}

func shortModel(model string) string {
	if model == "" {
		return "默认模型"
	}
	parts := strings.Split(model, "/")
	return parts[len(parts)-1]
}

func (w *workbench) currentModel() string {
	if thread, ok := w.selectedThread(); ok {
		return thread.Model
	}
	return w.draftModel(w.selected)
}

func (w *workbench) modelLabel() string {
	if w.threadProvider(w.selected) == "controlled" {
		return w.tr("本地演示", "Offline demo")
	}
	model := w.currentModel()
	if model == "" {
		model = w.configuredDefaultModel()
	}
	for _, item := range w.models {
		if item.Key() == model && item.Name != "" {
			return item.Name
		}
	}
	for _, item := range w.models {
		if strings.HasPrefix(model, item.Key()+":") && item.Name != "" {
			return item.Name
		}
	}
	if model == "" {
		return w.tr("默认模型", "Default model")
	}
	return shortModel(model)
}

func (w *workbench) workspaceLabel() string {
	spec := w.defaults.Workspace
	if thread, ok := w.selectedThread(); ok {
		spec = thread.Workspace
	}
	if spec == nil || spec.Path == "" && spec.Repository == "" {
		return w.tr("选择项目", "Choose project")
	}
	path := spec.Path
	if path == "" {
		path = spec.Repository
	}
	if isScratchWorkspacePath(path) {
		return w.tr3("无项目", "No project", "プロジェクトなし")
	}
	return filepath.Base(path)
}
