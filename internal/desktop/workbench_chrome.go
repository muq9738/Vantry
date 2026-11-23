package desktop

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// Glyphs of the workbench's chrome that the earlier icon set lacked.
var (
	// piLogoIcon is a blocky pixel π, the mark beside Pi model names.
	piLogoIcon          = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor"><path d="M3 4h18v4H3z"/><path d="M6 8h4v12H6z"/><path d="M14 8h4v8h-4z"/><path d="M17 16h4v4h-4z"/></svg>`))
	filterIcon          = lucide("list-filter")
	folderPlusIcon      = lucide("folder-plus")
	clockIcon           = lucide("clock")
	backIcon            = lucide("arrow-left")
	forwardIcon         = lucide("arrow-right")
	serverIcon          = lucide("server")
	vscodeIcon          = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><path fill="currentColor" fill-rule="evenodd" d="M70.912 99.317a6.223 6.223 0 0 0 4.96-.19l20.589-9.907A6.25 6.25 0 0 0 100 83.587V16.413a6.25 6.25 0 0 0-3.54-5.632L75.874.874a6.226 6.226 0 0 0-7.104 1.21L29.355 38.04 12.187 25.01a4.162 4.162 0 0 0-5.318.236l-5.506 5.009a4.168 4.168 0 0 0-.004 6.162L16.247 50 1.36 63.583a4.168 4.168 0 0 0 .004 6.162l5.506 5.01a4.162 4.162 0 0 0 5.318.236l17.168-13.032L68.77 97.917a6.217 6.217 0 0 0 2.143 1.4ZM75.015 27.3 45.11 50l29.906 22.701V27.3Z"/></svg>`)) // Editor brand silhouette; source attribution is retained in notices.
	cloudUpIcon         = lucide("cloud-upload")
	changesIcon         = lucide("file-diff")
	unsettleIcon        = lucide("undo-2")
	swapIcon            = lucide("arrow-right-left")
	shieldOffIcon       = lucide("shield-off")
	codeEditorIcon      = strokeIcon(`<path d="m18 16 4-4-4-4"/><path d="m6 8-4 4 4 4"/><path d="m14.5 4-5 16"/>`)
	imageAttachmentIcon = strokeIcon(`<rect width="18" height="18" x="3" y="3" rx="2" ry="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21"/>`)
	pdfAttachmentIcon   = strokeIcon(`<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="M10 9H8"/><path d="M16 13H8"/><path d="M16 17H8"/>`)
)

// badgeColors are the ten Stardust project hues - amber, orange, red, rose,
// violet, indigo, blue, cyan, teal and green - each readable as text on its
// own 20% wash in both palettes.
var badgeColors = []ui.Color{
	ui.Hex("#D99A06"), ui.Hex("#F26B1D"), ui.Hex("#E5484D"), ui.Hex("#E54D8A"), ui.Hex("#8E5CF0"),
	ui.Hex("#5B63E8"), ui.Hex("#2F7FF0"), ui.Hex("#0EA5C6"), ui.Hex("#12A594"), ui.Hex("#30A46C"),
}

// badgeColor picks a stable hue for a project name: an FNV-1a hash, so the
// same project keeps its colour in every row, header and window.
func badgeColor(name string) ui.Color {
	h := uint32(2166136261)
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= 16777619
	}
	return badgeColors[h%uint32(len(badgeColors))]
}

// projectInitials is the workbench's two-letter project mark: the first letters of the
// first two words of a hyphenated, spaced or underscored name; otherwise the
// first and last letter of a single word (Vantry → VY, FrameTrail → FL).
func projectInitials(name string) string {
	name = strings.TrimSpace(name)
	words := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.' || r == '/'
	})
	letters := func(word string) []rune {
		var out []rune
		for _, r := range word {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				out = append(out, r)
			}
		}
		return out
	}
	var picked []rune
	if len(words) >= 2 {
		for _, word := range words {
			if l := letters(word); len(l) > 0 {
				picked = append(picked, l[0])
			}
			if len(picked) == 2 {
				break
			}
		}
	}
	if len(picked) < 2 && len(words) > 0 {
		l := letters(strings.Join(words, ""))
		switch {
		case len(l) >= 2:
			picked = []rune{l[0], l[len(l)-1]}
		case len(l) == 1:
			picked = []rune{l[0]}
		}
	}
	if len(picked) == 0 {
		return "?"
	}
	return strings.ToUpper(string(picked))
}

// projectBadge draws the colored rounded-square initials of a project.
func projectBadge(c *ui.Context, name string, size float32) ui.Element {
	tone := badgeColor(name)
	box := ui.Row(c).Size(size, size).Radius(4).Background(tone.Alpha(0.2)).Justify(ui.Center).AlignItems(ui.Center).Shrink(0).AccessHidden(true)
	box.Children(func() {
		ui.Text(c, projectInitials(name)).FontSize(max(size*0.46, 6.5)).Bold().TextColor(tone).NoWrap()
	})
	return box
}

// isScratchWorkspacePath identifies Core/t3compat ephemeral scratch workspaces
// (<DataDir>/workspaces/<threadID>/chat) so UI labels them as "No project"
// rather than showing fake projects named "chat".
func isScratchWorkspacePath(path string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if cleaned == "" || cleaned == "." || !strings.HasSuffix(cleaned, "/chat") {
		return false
	}
	return strings.Contains(cleaned, "/workspaces/")
}

// workspaceName is a workspace's display name: the folder or repository base.
func workspaceName(spec *api.WorkspaceSpec) string {
	if spec == nil {
		return ""
	}
	path := nonEmptyString(spec.Repository, spec.Path)
	if path == "" || isScratchWorkspacePath(path) {
		return ""
	}
	return filepath.Base(path)
}

// workspaceKey identifies a project for grouping and filtering.
func workspaceKey(spec *api.WorkspaceSpec) string {
	path := api.WorkspaceProjectPath(spec)
	if isScratchWorkspacePath(path) {
		return ""
	}
	return path
}

var hostnameOnce struct {
	sync.Once
	name string
}

// machineName is the short local host name, read once per process.
func machineName() string {
	hostnameOnce.Do(func() {
		name, err := os.Hostname()
		if err != nil || strings.TrimSpace(name) == "" {
			name = "localhost"
		}
		name = strings.TrimSuffix(name, ".local")
		if i := strings.IndexByte(name, '.'); i > 0 {
			name = name[:i]
		}
		hostnameOnce.name = name
	})
	return hostnameOnce.name
}

// hostLabel is where the thread runs: the remote host when one is set.
func (w *workbench) hostLabel() string {
	if w.defaults.RemoteName != "" {
		name := w.defaults.RemoteName
		if w.defaults.ConnectionMode != nil {
			switch w.defaults.ConnectionMode() {
			case "direct":
				name += w.tr(" · 直连", " · Direct")
			case "relay":
				name += w.tr(" · 中继", " · Relay")
			}
		}
		return name
	}
	if host := strings.TrimSpace(w.defaults.RemoteHost); host != "" {
		return host
	}
	return machineName()
}

// piMark is the small Pi logo glyph shown before Pi model names.
func piMark(c *ui.Context, color ui.Color, size float32) {
	ui.Icon(c, piLogoIcon).FontSize(size).TextColor(color).Shrink(0)
}

// editorOption represents an installed code editor or OS file manager fallback.
type editorOption struct {
	ID            string
	Name          string
	ZHName        string
	JAName        string
	Icon          *ui.SVG
	IsFileManager bool
	Open          func(path string) error
}

var systemEditorsOnce struct {
	sync.Once
	editors []editorOption
}

func macOSAppPath(names ...string) string {
	home, _ := os.UserHomeDir()
	roots := []string{"/Applications", "/System/Applications/Utilities"}
	if home != "" {
		roots = append(roots,
			filepath.Join(home, "Applications"),
			filepath.Join(home, "Applications", "JetBrains Toolbox"),
		)
	}
	for _, name := range names {
		for _, root := range roots {
			candidate := filepath.Join(root, name)
			if st, err := os.Stat(candidate); err == nil && st.IsDir() {
				if info, plistErr := os.Stat(filepath.Join(candidate, "Contents", "Info.plist")); plistErr == nil && info.Mode().IsRegular() {
					return candidate
				}
			}
		}
	}
	return ""
}

func detectSystemEditors() []editorOption {
	systemEditorsOnce.Do(func() {
		type candidateSpec struct {
			id, name, zh, ja string
			clis             []string
			macApps          []string
			icon             *ui.SVG
		}
		codeGlyph := codeEditorIcon
		specs := []candidateSpec{
			{"cursor", "Cursor", "Cursor", "Cursor", []string{"cursor"}, []string{"Cursor.app"}, codeGlyph},
			{"windsurf", "Windsurf", "Windsurf", "Windsurf", []string{"windsurf"}, []string{"Windsurf.app"}, codeGlyph},
			{"zed", "Zed", "Zed", "Zed", []string{"zed"}, []string{"Zed.app"}, codeGlyph},
			{"antigravity", "Antigravity", "Antigravity", "Antigravity", []string{"antigravity", "agy"}, []string{"Antigravity.app"}, codeGlyph},
			{"vscode", "VS Code", "VS Code", "VS Code", []string{"code"}, []string{"Visual Studio Code.app", "VSCodium.app"}, vscodeIcon},
			{"trae", "Trae", "Trae", "Trae", []string{"trae"}, []string{"Trae.app"}, codeGlyph},
			{"goland", "GoLand", "GoLand", "GoLand", []string{"goland"}, []string{"GoLand.app"}, codeGlyph},
			{"idea", "IntelliJ IDEA", "IntelliJ IDEA", "IntelliJ IDEA", []string{"idea"}, []string{"IntelliJ IDEA.app", "IntelliJ IDEA CE.app", "IntelliJ IDEA Ultimate.app"}, codeGlyph},
			{"pycharm", "PyCharm", "PyCharm", "PyCharm", []string{"pycharm"}, []string{"PyCharm.app", "PyCharm CE.app"}, codeGlyph},
			{"webstorm", "WebStorm", "WebStorm", "WebStorm", []string{"webstorm"}, []string{"WebStorm.app"}, codeGlyph},
			{"sublime", "Sublime Text", "Sublime Text", "Sublime Text", []string{"subl"}, []string{"Sublime Text.app"}, codeGlyph},
			{"xcode", "Xcode", "Xcode", "Xcode", nil, []string{"Xcode.app"}, codeGlyph},
		}

		var out []editorOption
		for _, s := range specs {
			var cliBin string
			for _, cli := range s.clis {
				if p, err := exec.LookPath(cli); err == nil && p != "" {
					cliBin = p
					break
				}
			}
			var appBundle string
			if runtime.GOOS == "darwin" {
				appBundle = macOSAppPath(s.macApps...)
			}
			if cliBin == "" && appBundle == "" {
				continue
			}
			cliCopy, appCopy := cliBin, appBundle
			out = append(out, editorOption{
				ID:     s.id,
				Name:   s.name,
				ZHName: s.zh,
				JAName: s.ja,
				Icon:   s.icon,
				Open: func(targetPath string) error {
					if runtime.GOOS == "darwin" && appCopy != "" {
						if err := exec.Command("open", "-a", appCopy, targetPath).Start(); err == nil {
							return nil
						}
					}
					if cliCopy != "" {
						if err := exec.Command(cliCopy, targetPath).Start(); err == nil {
							return nil
						}
					}
					return systemFileManagerOption().Open(targetPath)
				},
			})
		}
		out = append(out, systemFileManagerOption())
		systemEditorsOnce.editors = out
	})
	return systemEditorsOnce.editors
}

func systemFileManagerOption() editorOption {
	switch runtime.GOOS {
	case "darwin":
		return editorOption{
			ID:            "finder",
			Name:          "Finder",
			ZHName:        "访达",
			JAName:        "Finder",
			Icon:          folderIcon,
			IsFileManager: true,
			Open:          func(path string) error { return exec.Command("open", path).Start() },
		}
	case "windows":
		return editorOption{
			ID:            "explorer",
			Name:          "Explorer",
			ZHName:        "资源管理器",
			JAName:        "エクスプローラー",
			Icon:          folderIcon,
			IsFileManager: true,
			Open:          func(path string) error { return exec.Command("explorer", path).Start() },
		}
	default:
		return editorOption{
			ID:            "files",
			Name:          "Files",
			ZHName:        "文件管理器",
			JAName:        "ファイル",
			Icon:          folderIcon,
			IsFileManager: true,
			Open:          func(path string) error { return exec.Command("xdg-open", path).Start() },
		}
	}
}

func (w *workbench) availableEditors() []editorOption {
	if len(w.detectedEditors) > 0 {
		return w.detectedEditors
	}
	return detectSystemEditors()
}

// activeEditor returns the user's preferred editor if installed, otherwise the
// first detected code editor, otherwise Finder / OS file manager.
func (w *workbench) activeEditor() editorOption {
	list := w.availableEditors()
	if len(list) == 0 {
		return systemFileManagerOption()
	}
	if pref := strings.TrimSpace(w.widgets.preferredEditor); pref != "" {
		for _, ed := range list {
			if ed.ID == pref {
				return ed
			}
		}
	}
	for _, ed := range list {
		if !ed.IsFileManager {
			return ed
		}
	}
	return list[0]
}

func (w *workbench) openInEditorLabel(ed editorOption) string {
	switch normalizeLocale(w.widgets.locale) {
	case localeEN:
		return "Open in " + ed.Name
	case localeJA:
		return ed.JAName + " で開く"
	default:
		if ed.ID == "finder" {
			return "在访达中打开"
		}
		return "在 " + ed.ZHName + " 中打开"
	}
}

func (w *workbench) openInSpecificEditor(ed editorOption, path string) {
	w.openInSpecificEditorForTarget(ed, w.selected, path)
}

func (w *workbench) openInSpecificEditorForTarget(ed editorOption, target, path string) {
	if w.defaults.RemotePeer {
		w.setAction(w.tr3("项目位于远程主机，可在文件面板浏览和编辑任务中使用。", "Workspace is on a remote host; use the Files panel to browse.", "リモートホスト上のプロジェクトはファイルパネルから参照できます。"))
		return
	}
	path = strings.TrimSpace(path)
	openFn := ed.Open
	if openFn == nil {
		openFn = systemFileManagerOption().Open
	}
	if path != "" {
		w.start(func() {
			_ = openFn(path)
		})
		return
	}
	if target == "" || isLocalSession(target) || !w.hasThread(target) {
		return
	}
	service, ok := w.service.(api.WorkbenchQueries)
	if !ok {
		return
	}
	ctx := w.ctx
	w.start(func() {
		changes, err := service.InspectWorkspace(ctx, target)
		if err != nil || strings.TrimSpace(changes.Path) == "" {
			w.post(func() {
				errMsg := w.tr3("无法解析会话工作树路径", "Failed to resolve workspace path", "ワークスペースのパスを取得できませんでした")
				if err != nil {
					errMsg = describe(errMsg, err)
				}
				w.workspaceNotice = notice{Text: errMsg, Level: noticeError}
			})
			return
		}
		w.post(func() {
			if w.selected == target {
				w.workspaceThread = target
				w.workspaceChanges = &changes
			}
		})
		_ = openFn(changes.Path)
	})
}

func (w *workbench) openInEditor(path string) {
	w.openInSpecificEditor(w.activeEditor(), path)
}

// selectedWorkspacePath is the folder of the selected thread or draft.
// For isolated worktrees without an explicit resolved Path yet, it never falls
// back to spec.Repository (the source checkout).
func (w *workbench) selectedWorkspacePath() string {
	spec := w.selectedWorkspace()
	if spec == nil {
		return ""
	}
	if w.workspaceChanges != nil && w.workspaceThread == w.selected && w.workspaceChanges.Path != "" {
		return w.workspaceChanges.Path
	}
	if spec.Kind == "worktree" {
		return spec.Path
	}
	return nonEmptyString(spec.Path, spec.Repository)
}
