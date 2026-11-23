package desktop

import (
	"encoding/json"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// The folder palette is T3 Code's "Add project" command palette: a path
// field over the folders of the directory it names, filtered as you type,
// with ↑↓ to move, Enter to go into a folder, ⌘ Enter to use the folder the
// field names, Backspace to climb and Esc to close. It reads directories
// through the selected Core, so the same palette serves this computer and
// a remote machine; nothing is read from the viewer's own disk for a peer.

type remoteDirectoryState struct {
	open    bool
	path    string // the directory being listed
	input   string // what the field shows: "~/Develop/" or "~/Develop/Ch"
	index   int
	listing api.EnvironmentDirectories
	cache   map[string]api.EnvironmentDirectories
	failure string
	loading string // the directory requested and not answered yet
	select_ bool   // place the caret at the end of the field next frame
}

// home is the directory "~" stands for on the machine being browsed.
func (w *workbench) browseHome() string {
	if home := strings.TrimSpace(w.environmentInfo.Home); home != "" {
		return home
	}
	if !w.defaults.RemotePeer {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	return ""
}

func (w *workbench) tildePath(p string) string {
	home := w.browseHome()
	switch {
	case home == "":
		return p
	case p == home:
		return "~"
	case strings.HasPrefix(p, home+"/"):
		return "~" + p[len(home):]
	}
	return p
}

func (w *workbench) expandTilde(p string) string {
	home := w.browseHome()
	switch {
	case home == "":
		return p
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return home + p[1:]
	}
	return p
}

// splitPaletteInput separates the field into the directory to list and the
// filter typed after its last slash.
func (w *workbench) splitPaletteInput(input string) (dir, filter string) {
	full := w.expandTilde(strings.TrimSpace(input))
	if full == "" {
		return w.browseHome(), ""
	}
	if strings.HasSuffix(full, "/") {
		return path.Clean(full), ""
	}
	i := strings.LastIndexByte(full, '/')
	if i < 0 {
		return w.browseHome(), full
	}
	dir = full[:i]
	if dir == "" {
		dir = "/"
	}
	return dir, full[i+1:]
}

func dirInput(display string) string {
	if strings.HasSuffix(display, "/") {
		return display
	}
	return display + "/"
}

// openRemoteDirectory opens the palette at path (home when empty) and lists it.
func (w *workbench) openRemoteDirectory(dir string) {
	state := &w.remoteDirectories
	state.open = true
	if dir == "" {
		dir = w.browseHome()
	}
	if dir != "" {
		state.input = dirInput(w.tildePath(dir))
	}
	state.index, state.select_ = 0, true
	w.listPaletteDirectory(dir)
}

func (w *workbench) listPaletteDirectory(dir string) {
	state := &w.remoteDirectories
	if cached, ok := state.cache[dir]; ok && dir != "" {
		state.path, state.listing, state.failure = cached.Path, cached, ""
		return
	}
	if state.loading == dir && dir != "" {
		return
	}
	state.path, state.loading = dir, dir
	w.workbenchCall("environment-directories", api.WorkbenchRequest{Action: "environment.directories", Workspace: dir}, func(response api.WorkbenchResponse, err error) {
		if state.loading == dir {
			state.loading = ""
		}
		if err != nil {
			state.failure = describe(w.tr("读取目录失败", "Could not read the folder"), err)
			state.listing = api.EnvironmentDirectories{Path: dir}
			return
		}
		var listing api.EnvironmentDirectories
		if json.Unmarshal([]byte(response.Output), &listing) != nil {
			state.failure = w.tr("目标机器没有返回目录列表。", "The machine returned no folder list.")
			return
		}
		sort.SliceStable(listing.Entries, func(i, j int) bool {
			return strings.ToLower(listing.Entries[i].Name) < strings.ToLower(listing.Entries[j].Name)
		})
		if state.cache == nil {
			state.cache = map[string]api.EnvironmentDirectories{}
		}
		state.cache[listing.Path] = listing
		if dir != listing.Path {
			state.cache[dir] = listing
		}
		state.failure = ""
		if state.path == dir || state.path == "" {
			state.path, state.listing = listing.Path, listing
			if state.input == "" {
				state.input = dirInput(w.tildePath(listing.Path))
				state.select_ = true
			}
		}
	})
}

type paletteRow struct {
	name, path string
	parent     bool
}

// paletteRows are the parent link and the folders matching the filter:
// prefix matches first, then names containing it.
func (w *workbench) paletteRows(filter string) []paletteRow {
	state := &w.remoteDirectories
	var rows []paletteRow
	if filter == "" && state.listing.Parent != "" && state.listing.Parent != state.listing.Path {
		rows = append(rows, paletteRow{name: "..", path: state.listing.Parent, parent: true})
	}
	lower := strings.ToLower(filter)
	var contains []paletteRow
	for _, entry := range state.listing.Entries {
		name := strings.ToLower(entry.Name)
		switch {
		case lower == "" || strings.HasPrefix(name, lower):
			rows = append(rows, paletteRow{name: entry.Name, path: entry.Path})
		case strings.Contains(name, lower):
			contains = append(contains, paletteRow{name: entry.Name, path: entry.Path})
		}
	}
	return append(rows, contains...)
}

// usePaletteFolder adopts dir as the workspace and closes the palette.
func (w *workbench) usePaletteFolder(dir string) {
	dir = strings.TrimRight(w.expandTilde(strings.TrimSpace(dir)), "/")
	if dir == "" {
		return
	}
	spec := &api.WorkspaceSpec{Kind: "directory", Path: dir}
	if w.projectChoice.mode == 1 {
		spec = &api.WorkspaceSpec{Kind: "worktree", Repository: dir}
	}
	w.configureWorkspaceSpec(spec)
	w.remoteDirectories.open = false
	w.widgets.projectPickerOpen = false
	w.widgets.composerFocus.Focus()
}

func (w *workbench) paletteKey(k ui.TextKey) bool {
	state := &w.remoteDirectories
	text := k.Text()
	_, filter := w.splitPaletteInput(text)
	rows := w.paletteRows(filter)
	switch k.Key {
	case ui.KeyEscape:
		state.open = false
		w.widgets.projectPickerOpen = false
		return true
	case ui.KeyUp:
		if len(rows) > 0 {
			state.index = (state.index - 1 + len(rows)) % len(rows)
		}
		return true
	case ui.KeyDown:
		if len(rows) > 0 {
			state.index = (state.index + 1) % len(rows)
		}
		return true
	case ui.KeyEnter:
		if k.Mods&(ui.Super|ui.Ctrl) != 0 {
			dir, filter := w.splitPaletteInput(text)
			if filter != "" && len(rows) > 0 {
				dir = rows[min(state.index, len(rows)-1)].path
			}
			w.usePaletteFolder(dir)
			return true
		}
		if len(rows) == 0 {
			return true
		}
		row := rows[min(state.index, len(rows)-1)]
		next := dirInput(w.tildePath(row.path))
		k.SetText(next)
		state.input, state.index, state.select_ = next, 0, true
		w.listPaletteDirectory(row.path)
		return true
	case ui.KeyBackspace:
		// At a directory boundary Backspace climbs one level, as T3's does.
		if strings.HasSuffix(text, "/") && text != "/" && text != "~/" {
			dir, _ := w.splitPaletteInput(text)
			parent := path.Dir(dir)
			next := dirInput(w.tildePath(parent))
			k.SetText(next)
			state.input, state.index, state.select_ = next, 0, true
			w.listPaletteDirectory(parent)
			return true
		}
	}
	return false
}

func keyHint(c *ui.Context, p palette, keys []string, label string) {
	ui.Row(c).Gap(4).AlignItems(ui.Center).Shrink(0).Children(func() {
		for _, key := range keys {
			ui.Row(c).Height(18).MinWidth(18).Padding(0, 5).Radius(4).Border(1, p.line).Background(p.soft).Justify(ui.Center).AlignItems(ui.Center).Children(func() {
				ui.Text(c, key).FontSize(10.5).TextColor(p.muted).NoWrap()
			})
		}
		ui.Text(c, label).FontSize(11).TextColor(p.faint).NoWrap()
	})
}

// remoteDirectoryDialog draws the folder palette.
func (w *workbench) remoteDirectoryDialog(c *ui.Context, p palette) {
	state := &w.remoteDirectories
	if w.widgets.projectPickerOpen && !state.open {
		w.widgets.projectPickerOpen = false
		start := ""
		if spec := w.currentWorkspace(); spec != nil {
			start = nonEmptyString(spec.Repository, spec.Path)
			start = path.Dir(strings.TrimRight(start, "/"))
		}
		w.openRemoteDirectory(start)
	}
	if !state.open {
		return
	}
	width, height := c.Size()
	panelWidth := min(float32(576), width-32)
	ui.DialogBase(c, &state.open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000073")).Justify(ui.Start).Padding(max(48, height*0.12), 0, 0)
		panel.Width(panelWidth).Radius(14).Background(p.raised).Border(1, p.line).
			Label(w.tr("选择项目文件夹", "Choose a project folder")).Shadow(0, 24, 64, 0, ui.Hex("#00000066"))
		ui.Column(c).FillWidth().Children(func() {
			dir, filter := w.splitPaletteInput(state.input)
			if dir != state.path && dir != "" {
				w.listPaletteDirectory(dir)
			}
			rows := w.paletteRows(filter)
			if state.index >= len(rows) {
				state.index = max(0, len(rows)-1)
			}
			// Header: back, the path field, and Add ⌘ Enter.
			ui.Row(c).Height(48).Padding(0, 10, 0, 8).Gap(6).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).Children(func() {
				back := iconButton(c, p, "palette-up", w.tr("上一级目录", "Parent folder"), backIcon).Size(28, 28).Padding(7)
				if back.Disabled(state.listing.Parent == "" || state.listing.Parent == state.listing.Path).Clicked() {
					next := dirInput(w.tildePath(state.listing.Parent))
					state.input, state.index, state.select_ = next, 0, true
					w.listPaletteDirectory(state.listing.Parent)
				}
				field := ui.TextInputBase(c.Key("palette-path"), &state.input).Label(w.tr("文件夹路径", "Folder path")).
					Placeholder("~/").Grow(1).MinWidth(0).Height(30).FontSize(13.5).TextColor(p.ink).AutoFocus()
				if state.select_ {
					state.select_ = false
					n := len([]rune(state.input))
					field.SetTextSelection(n, n)
				}
				field.OnTextKey(w.paletteKey)
				if field.Changed() {
					state.index = 0
				}
				add := ui.Button(c.Key("palette-add"), "").Label(w.tr("添加此文件夹", "Add this folder")).Height(26).Padding(0, 4, 0, 8).Gap(6).Radius(7).
					Border(1, p.line).Background(p.soft).TextColor(p.ink).Shrink(0)
				if add.Hovered() {
					add.Background(p.selected)
				}
				add.Children(func() {
					ui.Text(c, w.tr("添加", "Add")).FontSize(12).FontWeight(500).NoWrap()
					ui.Row(c).Height(18).Padding(0, 5).Radius(4).Background(p.canvas.Alpha(0.6)).AlignItems(ui.Center).Children(func() {
						ui.Text(c, "⌘ Enter").FontSize(10.5).TextColor(p.muted).NoWrap()
					})
				})
				if add.Disabled(dir == "").Clicked() {
					w.usePaletteFolder(dir)
				}
			})
			ui.Scroll(c.Key("palette-list")).FillWidth().MaxHeight(min(360, height*0.5)).Children(func() {
				ui.Column(c).FillWidth().Padding(6).Gap(1).Role(ui.RoleListBox).Label(w.tr("文件夹", "Folders")).Children(func() {
					ui.Text(c, w.tr("文件夹", "Directories")).FontSize(11).FontWeight(500).TextColor(p.faint).Padding(6, 8, 4)
					switch {
					case state.failure != "":
						ui.Text(c, state.failure).FontSize(12).TextColor(p.warning).Wrap().Padding(6, 8)
					case state.loading != "" && len(rows) == 0:
						ui.Row(c).Gap(8).Padding(6, 8).AlignItems(ui.Center).Children(func() {
							activityIndicator(c, p.accent).Size(10, 10)
							ui.Text(c, w.tr("正在读取…", "Loading…")).FontSize(12).TextColor(p.muted)
						})
					case len(rows) == 0:
						ui.Text(c, w.tr("没有匹配的文件夹；⌘ Enter 使用当前路径", "No matching folders; ⌘ Enter uses this path")).FontSize(12).TextColor(p.faint).Padding(6, 8)
					}
					for i, row := range rows {
						row := row
						option := ui.Button(c.Key("palette-row:"+row.path), "").Label(row.name).Role(ui.RoleOption).AccessSelected(i == state.index).
							FillWidth().Height(30).Padding(0, 8).Gap(10).Radius(7).Border(0, p.line).Background(ui.Transparent).Justify(ui.Start)
						if i == state.index {
							option.Background(p.selected)
						} else if option.Hovered() {
							option.Background(p.soft)
						}
						option.Children(func() {
							icon := folderIcon
							if row.parent {
								icon = lucide("corner-left-up")
							}
							ui.Icon(c, icon).FontSize(14).TextColor(p.muted).Shrink(0)
							ui.Text(c, row.name).FontSize(13).TextColor(p.ink).SingleLine().Ellipsis("…").MinWidth(0)
						})
						if option.Clicked() {
							next := dirInput(w.tildePath(row.path))
							state.input, state.index, state.select_ = next, 0, true
							w.listPaletteDirectory(row.path)
						}
						if option.DoubleClicked() && !row.parent {
							w.usePaletteFolder(row.path)
						}
					}
				})
			})
			// Footer: key hints, workspace mode and the system picker.
			ui.Row(c).Height(40).Padding(0, 12).Gap(14).AlignItems(ui.Center).BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
				keyHint(c, p, []string{"↑", "↓"}, w.tr("选择", "Navigate"))
				keyHint(c, p, []string{"Enter"}, w.tr("进入", "Open"))
				if width > 620 {
					keyHint(c, p, []string{"⌫"}, w.tr("上一级", "Back"))
					keyHint(c, p, []string{"Esc"}, w.tr("关闭", "Close"))
				}
				ui.Spacer(c)
				mode := quietButton(c, p, "palette-mode", "").Label(w.tr("工作区模式", "Workspace mode")).Height(24).Padding(0, 7).Radius(6)
				mode.Children(func() {
					icon, label := folderIcon, w.tr("本地目录", "Local folder")
					if w.projectChoice.mode == 1 {
						icon, label = worktreeIcon, w.tr("新建 worktree", "New worktree")
					}
					ui.Icon(c, icon).FontSize(12)
					ui.Text(c, label).FontSize(11.5).NoWrap()
					ui.Icon(c, chevronIcon).FontSize(10).TextColor(p.faint)
				})
				mode.PopoverMenuButton(func(m *ui.Menu) {
					if m.Item(w.tr("本地目录：直接在该文件夹工作", "Local folder: work in place")).Radio(w.projectChoice.mode == 0).Chosen() {
						w.projectChoice.mode = 0
					}
					if m.Item(w.tr("新建 worktree：首次发送时创建独立分支", "New worktree: isolated branch on first send")).Radio(w.projectChoice.mode == 1).Chosen() {
						w.projectChoice.mode = 1
					}
					m.Separator()
					if m.Item(w.tr("仅聊天，不选项目", "Chat only, no project")).Chosen() {
						w.configureWorkspaceSpec(nil)
						state.open = false
					}
				}, ui.PopoverMenuButtonOptions{At: ui.AnchorTopEnd, Self: ui.AnchorBottomEnd})
				if !w.defaults.RemotePeer && w.nativeDirectoryPicker != nil {
					if quietButton(c, p, "palette-system", w.tr("访达…", "Finder…")).Label(w.tr("用系统选择器打开", "Use the system picker")).Height(24).Padding(0, 7).Clicked() {
						state.open = false
						w.nativeDirectoryPicker()
					}
				}
			})
		})
	})
	if !state.open {
		w.widgets.projectPickerOpen = false
	}
}
