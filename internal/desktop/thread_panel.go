package desktop

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
)

// threadPanelRows renders the T3 Code-aligned top-right workspace & Git quick
// actions shared by both the floating home projectCard and the conversation
// workspaceView/popover:
//  1. Host & project directory
//  2. Open in <Detected Editor / Finder> + ▾ editor switcher
//  3. Open in Terminal
//  4. Branch + ↑ahead / ↓behind
//  5. One-click Commit & push (auto <=20-char localized summary) + ▾ Git actions
//  6. Changes (+added / -removed)
func (w *workbench) threadPanelRows(c *ui.Context, p palette, showHeader bool) {
	target := w.selected
	path := w.selectedWorkspacePath()
	committed := w.hasThread(target) && !isLocalSession(target)
	gitState := w.features.git[target]
	wsChanges := w.workspaceChanges
	if wsChanges != nil && w.workspaceThread != "" && w.workspaceThread != target {
		wsChanges = nil
	}
	if path == "" && wsChanges != nil {
		path = wsChanges.Path
	}
	gitBusy := w.features.busy["git:"+target]
	_, runActive := w.activeRun(target)

	rowBtn := func(key, label string, icon *ui.SVG, trailing func()) ui.Element {
		button := ui.Button(c.Key(key), "").Label(label).FillWidth().Height(32).Padding(0, 9).Radius(8).
			Border(0, p.line).Background(ui.Transparent).TextColor(p.ink).Justify(ui.Start)
		if button.Hovered() {
			button.Background(p.soft)
		}
		button.Children(func() {
			ui.Row(c).FillWidth().Gap(9).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, icon).FontSize(14).TextColor(p.muted).Shrink(0)
				ui.Text(c, label).FontSize(12.5).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
				ui.Spacer(c)
				if trailing != nil {
					trailing()
				}
			})
		})
		return button
	}

	if showHeader {
		ui.Row(c.Key("ws-header")).Height(30).Padding(0, 4, 0, 6).Gap(8).AlignItems(ui.Center).Children(func() {
			name := w.workspaceLabel()
			if name != "" && path != "" {
				projectBadge(c, name, 18)
			} else {
				ui.Icon(c, folderIcon).FontSize(15).TextColor(p.muted).Shrink(0)
			}
			ui.Text(c, name).FontSize(13).FontWeight(600).TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…")
			if committed {
				refresh := iconButton(c, p, "refresh-workspace", w.tr3("刷新", "Refresh", "更新"), refreshIcon).Size(24, 24).Padding(4)
				refresh.Disabled(w.workspaceLoading || gitBusy)
				if refresh.Clicked() {
					w.refreshWorkspace()
					w.refreshGit(target, false)
				}
			}
		})
		if path != "" {
			ui.Text(c, path).Font("Menlo").FontSize(10.5).TextColor(p.faint).Padding(0, 8, 2).SingleLine().Ellipsis("…").Selectable()
		}
	}

	// 1. Host & project folder
	hostTitle := w.hostLabel()
	if folder := w.workspaceLabel(); folder != "" && !showHeader {
		hostTitle = hostTitle + " · " + folder
	}
	host := rowBtn("card-host", hostTitle, serverIcon, func() {
		ui.Icon(c, folderIcon).FontSize(13).TextColor(p.faint).Shrink(0)
	})
	if !committed {
		host.Description(w.tr3("选择项目文件夹", "Choose the project folder", "プロジェクトフォルダを選択"))
		if host.Clicked() {
			w.beginProjectChoice()
			w.widgets.projectPickerOpen = true
		}
	} else if path != "" {
		host.Tooltip(path).Description(path)
		if host.Clicked() {
			c.WriteClipboard(path)
			w.workspaceNotice = notice{Text: w.tr3("已复制工作区路径。", "Workspace path copied.", "パスをコピーしました。"), Level: noticeInfo}
		}
	}

	// 2. Smart Open in <Editor / Finder> + ▾ editor picker
	activeEd := w.activeEditor()
	openLabel := w.openInEditorLabel(activeEd)
	editors := w.availableEditors()
	canOpenTarget := path != "" || (committed && w.selectedWorkspace() != nil)
	ui.Row(c.Key("card-editor-split")).FillWidth().Gap(2).AlignItems(ui.Center).Children(func() {
		openBtn := ui.Button(c.Key("card-vscode"), "").Label(openLabel).Grow(1).MinWidth(0).Height(32).Padding(0, 9).Radius(8).
			Border(0, p.line).Background(ui.Transparent).TextColor(p.ink).Justify(ui.Start)
		if openBtn.Hovered() {
			openBtn.Background(p.soft)
		}
		if !canOpenTarget {
			openBtn.Disabled(true)
		} else if path != "" {
			openBtn.Tooltip(path)
		}
		openBtn.Children(func() {
			ui.Row(c).FillWidth().Gap(9).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, activeEd.Icon).FontSize(14).TextColor(p.muted).Shrink(0)
				ui.Text(c, openLabel).FontSize(12.5).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			})
		})
		if openBtn.Clicked() {
			w.openInSpecificEditorForTarget(activeEd, target, path)
		}
		if len(editors) > 1 {
			pickEd := iconButton(c, p, "card-editor-menu", w.tr3("选择打开方式", "Choose editor", "開くアプリを選択"), chevronIcon).
				Size(26, 32).Padding(0, 6).Radius(7)
			if !canOpenTarget {
				pickEd.Disabled(true)
			}
			pickEd.PopoverMenuButton(func(m *ui.Menu) {
				for _, ed := range editors {
					ed := ed
					lbl := w.openInEditorLabel(ed)
					if m.Item(lbl).Radio(ed.ID == activeEd.ID).Chosen() {
						w.widgets.preferredEditor = ed.ID
						w.saveState()
						if canOpenTarget {
							w.openInSpecificEditorForTarget(ed, target, path)
						}
					}
				}
			}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomEnd, Self: ui.AnchorTopEnd})
		}
	})

	// 3. Open in Terminal
	termText := w.tr3("在终端中打开", "Open in Terminal", "ターミナルで開く")
	term := rowBtn("card-terminal", termText, terminalIcon, nil)
	if showHeader {
		term.Label(w.tr3("打开终端", "Open terminal", "ターミナルで開く"))
	}
	if (!committed && path == "") || w.terminalCalls[target] {
		term.Disabled(true)
	}
	if term.Clicked() {
		w.openSystemTerminal()
	}

	ui.Box(c).FillWidth().Height(1).Background(p.lineSoft).Margin(4, 2)

	// 4. Git Branch
	branch := w.selectedBranch()
	if gitState != nil && gitState.Branch != "" {
		branch = gitState.Branch
	} else if wsChanges != nil && wsChanges.Branch != "" {
		branch = wsChanges.Branch
	}
	if branch == "" {
		branch = w.tr3("未关联分支", "No branch", "ブランチなし")
	}
	gitRow := rowBtn("card-branch", branch, branchIcon, func() {
		if gitState == nil {
			return
		}
		if gitState.Ahead > 0 {
			ui.Text(c, fmt.Sprintf("↑%d", gitState.Ahead)).Font("Menlo").FontSize(10.5).TextColor(p.success).NoWrap()
		}
		if gitState.Behind > 0 {
			ui.Text(c, fmt.Sprintf("↓%d", gitState.Behind)).Font("Menlo").FontSize(10.5).TextColor(p.warning).NoWrap()
		}
	})
	gitRow.Description(w.tr3("打开 Git 面板", "Open Git", "Git パネルを開く"))
	if !committed && path == "" {
		gitRow.Disabled(true)
	}
	if gitRow.Clicked() {
		w.widgets.workspaceOpen = false
		w.features.panel = ""
		w.openDock("git")
	}

	// 5. One-click Commit & push (T3 Code aligned) + ▾ Git menu
	cpLabel := w.tr3("提交并推送", "Commit & push", "コミット＆プッシュ")
	autoPreview := w.autoCommitSummary(gitState, wsChanges, w.widgets.locale)
	ui.Row(c.Key("card-commit-push-split")).FillWidth().Gap(2).AlignItems(ui.Center).Children(func() {
		cpBtn := ui.Button(c.Key("card-push"), "").Label(cpLabel).Grow(1).MinWidth(0).Height(32).Padding(0, 9).Radius(8).
			Border(0, p.line).Background(ui.Transparent).TextColor(p.ink).Justify(ui.Start)
		if cpBtn.Hovered() {
			cpBtn.Background(p.soft)
		}
		cpBtn.Description(w.tr3("一键暂存、按当前语言生成 20 字内提交信息并推送", "Stage all, generate a concise localized commit summary, and push", "変更をステージし、20文字以内の要約でコミット＆プッシュ"))
		if autoPreview != "" {
			cpBtn.Tooltip(cpLabel + " · " + autoPreview)
		}
		if (!committed && path == "") || gitBusy || runActive {
			cpBtn.Disabled(true)
		}
		cpBtn.Children(func() {
			ui.Row(c).FillWidth().Gap(9).AlignItems(ui.Center).Children(func() {
				if gitBusy {
					activityIndicator(c, p.accent).Size(13, 13).Shrink(0)
				} else {
					ui.Icon(c, cloudUpIcon).FontSize(14).TextColor(p.accentInk).Shrink(0)
				}
				ui.Text(c, cpLabel).FontSize(12.5).FontWeight(500).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			})
		})
		if cpBtn.Clicked() {
			w.oneClickCommitAndPush(target)
		}
		moreGit := iconButton(c, p, "card-git-more", w.tr3("更多 Git 操作", "More Git actions", "その他の Git 操作"), chevronIcon).
			Size(26, 32).Padding(0, 6).Radius(7)
		if (!committed && path == "") || gitBusy {
			moreGit.Disabled(true)
		}
		moreGit.PopoverMenuButton(func(m *ui.Menu) {
			if m.Item(cpLabel + "（" + autoPreview + "）").Disabled(runActive).Chosen() {
				w.oneClickCommitAndPush(target)
			}
			if m.Item(w.tr3("拉取（快进 Pull）", "Pull (fast-forward)", "プル (Fast-forward)")).Disabled(runActive).Chosen() {
				w.gitRemoteFor(target, actionGitPull)
			}
			if m.Item(w.tr3("仅推送（Push）", "Push only", "プッシュのみ")).Disabled(runActive).Chosen() {
				w.gitRemoteFor(target, actionGitPush)
			}
			if m.Item(w.tr3("获取远程（Fetch）", "Fetch", "フェッチ")).Chosen() {
				w.gitRemoteFor(target, actionGitFetch)
			}
			m.Separator()
			if m.Item(w.tr3("打开 Git 面板…", "Open Git panel…", "Git パネルを開く…")).Chosen() {
				w.widgets.workspaceOpen = false
				w.features.panel = ""
				w.openDock("git")
			}
		}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomEnd, Self: ui.AnchorTopEnd})
	})

	// 6. Changes row (+added / -removed)
	changesTitle := w.tr3("变更", "Changes", "変更")
	if showHeader {
		changesTitle = w.tr3("文件变更", "Changes", "変更")
	}
	changes := rowBtn("card-changes", changesTitle, changesIcon, func() {
		var diffText string
		var fileCount int
		switch {
		case wsChanges != nil:
			diffText = wsChanges.Diff
			fileCount = len(wsChanges.Files)
		case gitState != nil:
			diffText = gitState.Diff
			fileCount = len(gitState.Files)
		default:
			return
		}
		added, removed := diffLineCounts(diffText)
		if added+removed == 0 {
			if fileCount > 0 {
				ui.Text(c, fmt.Sprintf(w.tr3("%d 个文件", "%d files", "%d 件"), fileCount)).FontSize(11).TextColor(p.muted).NoWrap()
			}
			return
		}
		ui.Text(c, fmt.Sprintf("+%d", added)).Font("Menlo").FontSize(11).TextColor(p.success).NoWrap()
		ui.Text(c, fmt.Sprintf("-%d", removed)).Font("Menlo").FontSize(11).TextColor(p.danger).NoWrap()
	})
	if !committed && path == "" {
		changes.Disabled(true)
	}
	if changes.Clicked() {
		if committed {
			w.widgets.diffOpen = true
			if wsChanges == nil {
				w.refreshWorkspace()
			}
		} else {
			w.widgets.workspaceOpen = false
			w.features.panel = ""
			w.openDock("git")
		}
	}

	if w.workspaceLoading {
		ui.Text(c, w.tr3("正在检查工作区…", "Inspecting workspace…", "ワークスペースを確認中…")).FontSize(11).TextColor(p.muted).Padding(2, 8)
	}
	if w.workspaceErr != "" {
		ui.Text(c, w.workspaceErr).FontSize(11).TextColor(p.warning).Wrap().Padding(2, 8)
	}
	if w.workspaceNotice.valid() {
		ui.Box(c).Padding(2, 6).Children(func() { w.noticeLine(c, p, w.workspaceNotice) })
	}

	// When opened as the conversation workspace drawer/popover, also show the compact changed-file list underneath.
	if showHeader && wsChanges != nil {
		if len(wsChanges.Files) > 0 {
			ui.Box(c).FillWidth().Height(1).Background(p.lineSoft).Margin(4, 2)
			ui.Scroll(c.Key("changed-files")).MaxHeight(156).Children(func() {
				ui.Column(c).Gap(2).Padding(2, 6).Children(func() {
					for _, file := range wsChanges.Files {
						ui.Row(c).Height(24).Gap(8).AlignItems(ui.Center).Children(func() {
							ui.Text(c, file.Path).FontSize(11).TextColor(p.muted).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
							ui.Text(c, strings.TrimSpace(file.Status)).Font("Menlo").FontSize(10.5).TextColor(p.success).NoWrap().Shrink(0)
						})
					}
				})
			})
			reviewAll := quietButton(c, p, "review-all-changes", w.tr3("查看全部变更", "Review all changes", "すべての変更を確認")).FillWidth().Height(28)
			if reviewAll.Clicked() {
				w.widgets.diffOpen = true
			}
		} else {
			ui.Text(c, w.tr3("工作区没有未提交的修改", "No uncommitted changes", "未コミットの変更はありません")).FontSize(11).TextColor(p.muted).Padding(2, 8)
		}
		if wsChanges.Truncated {
			ui.Text(c, w.tr3("内容过长，仅载入部分变更。", "Too long; only part of the changes is loaded.", "長すぎるため一部の変更のみ読み込みました。")).FontSize(11).TextColor(p.warning).Wrap().Padding(2, 8)
		}
	}
}
