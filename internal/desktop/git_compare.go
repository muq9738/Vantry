package desktop

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"github.com/muq9738/Vantry/pkg/api"
)

type gitBranchCompare struct {
	Base          string          `json:"base"`
	Target        string          `json:"target"`
	AheadCommits  []api.GitCommit `json:"aheadCommits"`
	BehindCommits []api.GitCommit `json:"behindCommits"`
	Files         []api.GitFile   `json:"files"`
	Diff          string          `json:"diff"`
	Truncated     bool            `json:"truncated,omitempty"`
	patches       []patchFile
}

func decodeBranchCompare(response api.WorkbenchResponse, base, target string) (*gitBranchCompare, error) {
	raw := strings.TrimSpace(response.Output)
	if raw == "" {
		raw = strings.TrimSpace(string(response.Data))
	}
	if raw == "" {
		return &gitBranchCompare{Base: base, Target: target}, nil
	}
	var out gitBranchCompare
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if out.Base == "" {
		out.Base = base
	}
	if out.Target == "" {
		out.Target = target
	}
	out.patches = parsePatchFiles(out.Diff)
	return &out, nil
}

func (w *workbench) compareKey(threadID, base, target string) string {
	return threadID + "\x00" + base + "\x00" + target
}

func (w *workbench) openBranchCompare(base, targetBranch string) {
	threadID := w.selected
	if state := w.features.git[threadID]; state != nil && base == "" {
		base = state.Branch
	}
	targetBranch = strings.TrimSpace(targetBranch)
	if targetBranch == "" {
		return
	}
	w.gitView.branchOpen = false
	w.gitView.compareBase = base
	w.gitView.compareTarget = targetBranch
	w.gitView.compareFile = ""
	w.gitView.tab = 2
	w.openDock("git")
	w.loadBranchCompare(base, targetBranch, true)
}

func (w *workbench) loadBranchCompare(base, targetBranch string, force bool) {
	threadID := w.selected
	if targetBranch == "" {
		return
	}
	key := w.compareKey(threadID, base, targetBranch)
	if w.gitView.compareResults == nil {
		w.gitView.compareResults = map[string]*gitBranchCompare{}
		w.gitView.compareErrs = map[string]string{}
	}
	if !force && w.gitView.compareResults[key] != nil {
		return
	}
	req, ok := w.gitRequest(threadID, actionGitCompare)
	if !ok {
		return
	}
	req.Branch = base
	req.Name = targetBranch
	delete(w.gitView.compareErrs, key)
	w.workbenchCall("git-compare:"+key, req, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.gitView.compareErrs[key] = describe(w.tr("比较分支失败", "Branch comparison failed"), err)
			return
		}
		parsed, parseErr := decodeBranchCompare(response, base, targetBranch)
		if parseErr != nil {
			w.gitView.compareErrs[key] = describe(w.tr("解析分支比较结果失败", "Failed to decode comparison"), parseErr)
			return
		}
		w.gitView.compareResults[key] = parsed
		delete(w.gitView.compareErrs, key)
	})
}

func (w *workbench) gitComparePage(c *ui.Context, p palette, state *api.GitState, width float32, locked bool) {
	threadID := w.selected
	base := nonEmptyString(w.gitView.compareBase, state.Branch)
	targetBranch := w.gitView.compareTarget

	// Pick a sensible default target branch if user navigated to Compare without picking one.
	if targetBranch == "" {
		for _, candidate := range append(append([]string{}, state.Branches...), state.RemoteBranches...) {
			if candidate != "" && candidate != base {
				targetBranch = candidate
				w.gitView.compareBase = base
				w.gitView.compareTarget = targetBranch
				w.loadBranchCompare(base, targetBranch, false)
				break
			}
		}
	}

	key := w.compareKey(threadID, base, targetBranch)
	busy := w.features.busy["git-compare:"+key]
	cmp := w.gitView.compareResults[key]
	errMsg := w.gitView.compareErrs[key]

	// 1. Compare header bar: [base] ⇄ [target picker] + Merge / Refresh.
	ui.Column(c.Key("git-compare-page")).Grow(1).Basis(0).MinHeight(0).Gap(8).Children(func() {
		ui.Row(c.Key("compare-bar")).FillWidth().Padding(6, 8).Gap(6).Radius(9).AlignItems(ui.Center).
			Background(p.soft).Border(1, p.lineSoft).Shrink(0).Wrap().Children(func() {
			ui.Row(c).Gap(4).AlignItems(ui.Center).Shrink(1).MinWidth(0).Children(func() {
				ui.Icon(c, branchIcon).FontSize(11.5).TextColor(p.accentInk).Shrink(0)
				ui.Text(c, nonEmptyString(base, "HEAD")).Font("Menlo").FontSize(11.5).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…").MaxWidth(120)
			})
			swap := iconButton(c, p, "compare-swap", w.tr("交换比较方向", "Swap comparison direction"), lucide("arrow-right-left")).Size(24, 24).Padding(5)
			if targetBranch == "" {
				swap.Disabled(true)
			}
			if swap.Clicked() && targetBranch != "" {
				w.gitView.compareBase, w.gitView.compareTarget = targetBranch, base
				w.gitView.compareFile = ""
				w.loadBranchCompare(w.gitView.compareBase, w.gitView.compareTarget, true)
			}
			targetBtn := quietButton(c, p, "compare-target-pick", "").Label(w.tr("选择对比分支", "Choose target branch")).Height(24).Padding(0, 8).Radius(6).Shrink(1).MinWidth(0)
			targetBtn.Children(func() {
				ui.Row(c).Gap(4).AlignItems(ui.Center).MinWidth(0).Children(func() {
					ui.Text(c, nonEmptyString(targetBranch, w.tr("选择分支…", "Pick branch…"))).Font("Menlo").FontSize(11.5).TextColor(p.ink).SingleLine().Ellipsis("…").MaxWidth(140)
					ui.Icon(c, chevronIcon).FontSize(10).TextColor(p.faint).Shrink(0)
				})
			})
			targetBtn.PopoverMenuButton(func(m *ui.Menu) {
				for _, b := range state.Branches {
					if b == base {
						continue
					}
					b := b
					if m.Item(b).Radio(b == targetBranch).Chosen() {
						w.gitView.compareTarget = b
						w.gitView.compareFile = ""
						w.loadBranchCompare(base, b, true)
					}
				}
				if len(state.RemoteBranches) > 0 {
					m.Separator()
					for _, rb := range state.RemoteBranches {
						if rb == base {
							continue
						}
						rb := rb
						if m.Item(rb).Radio(rb == targetBranch).Chosen() {
							w.gitView.compareTarget = rb
							w.gitView.compareFile = ""
							w.loadBranchCompare(base, rb, true)
						}
					}
				}
			}, ui.PopoverMenuButtonOptions{At: ui.AnchorBottomStart, Self: ui.AnchorTopStart})

			ui.Spacer(c)
			isLocalTarget := false
			for _, b := range state.Branches {
				if b == targetBranch {
					isLocalTarget = true
					break
				}
			}
			if base == state.Branch && targetBranch != "" && isLocalTarget {
				mergeBtn := kitButton(c, p, "compare-merge", w.tr("合并到当前", "Merge"), controlOutline, controlSM)
				mergeBtn.Disabled(locked)
				if mergeBtn.Clicked() {
					w.gitMergeNamedBranch(targetBranch)
				}
			}
			refresh := iconButton(c, p, "compare-refresh", w.tr("重新比较", "Refresh comparison"), refreshIcon).Size(24, 24).Padding(4)
			refresh.Disabled(busy || targetBranch == "")
			if refresh.Clicked() {
				w.loadBranchCompare(base, targetBranch, true)
			}
		})

		if targetBranch == "" {
			ui.Column(c).Grow(1).Justify(ui.Center).AlignItems(ui.Center).Gap(6).Padding(20).Children(func() {
				ui.Icon(c, lucide("arrow-right-left")).FontSize(22).TextColor(p.faint)
				ui.Text(c, w.tr("当前仓库没有其他可比较的分支。", "No other branches available to compare.")).FontSize(12).TextColor(p.muted)
			})
			return
		}
		if busy && cmp == nil {
			ui.Column(c).Grow(1).Justify(ui.Center).AlignItems(ui.Center).Gap(8).Children(func() {
				activityIndicator(c, p.accent).Size(16, 16)
				ui.Text(c, w.tr("正在比较分支差异…", "Comparing branches…")).FontSize(12).TextColor(p.muted)
			})
			return
		}
		if errMsg != "" {
			ui.Text(c, errMsg).FontSize(12).TextColor(p.danger).Wrap().Padding(8)
			return
		}
		if cmp == nil {
			return
		}

		// 2. Sub-mode switch: Commits vs Files.
		commitCount := len(cmp.AheadCommits) + len(cmp.BehindCommits)
		fileCount := len(cmp.Files)
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Shrink(0).Children(func() {
			ui.Segmented(
				c.Key("compare-subtab"), &w.gitView.compareTab,
				fmt.Sprintf(w.tr("提交差异 (%d)", "Commits (%d)"), commitCount),
				fmt.Sprintf(w.tr("文件差异 (%d)", "Files (%d)"), fileCount),
			).Label(w.tr("比较视图", "Compare view"))
			ui.Spacer(c)
			if cmp.Truncated {
				ui.Text(c, w.tr("部分截断", "Truncated")).FontSize(10.5).TextColor(p.warning)
			}
		})

		if w.gitView.compareTab == 0 {
			w.gitCompareCommitsView(c, p, cmp)
		} else {
			w.gitCompareFilesView(c, p, cmp, width)
		}
	})
}

func (w *workbench) gitCompareCommitsView(c *ui.Context, p palette, cmp *gitBranchCompare) {
	now := time.Now()
	if len(cmp.AheadCommits) == 0 && len(cmp.BehindCommits) == 0 {
		ui.Column(c).Grow(1).Justify(ui.Center).AlignItems(ui.Center).Gap(6).Padding(20).Children(func() {
			ui.Icon(c, checkIcon).FontSize(20).TextColor(p.success)
			ui.Text(c, w.tr("两个分支的提交完全一致", "Branches have identical commit history")).FontSize(12.5).TextColor(p.muted)
		})
		return
	}
	ui.Scroll(c.Key("compare-commits")).Grow(1).Basis(0).MinHeight(0).Gap(10).Children(func() {
		renderSection := func(key, heading string, tone ui.Color, commits []api.GitCommit) {
			settingsGroup(c, p, key, func() {
				ui.Column(c).FillWidth().Children(func() {
					ui.Row(c).FillWidth().Padding(8, 10).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Box(c).Size(7, 7).Radius(4).Background(tone).Shrink(0)
						ui.Text(c, heading).FontSize(11.5).FontWeight(600).TextColor(p.ink).Grow(1).SingleLine().Ellipsis("…")
						ui.Text(c, fmt.Sprint(len(commits))).Font("Menlo").FontSize(10.5).TextColor(p.faint)
					})
					if len(commits) == 0 {
						ui.Text(c, w.tr("无独有提交", "No unique commits")).FontSize(11).TextColor(p.faint).Padding(4, 10, 10)
						return
					}
					for _, cm := range commits {
						ui.Row(c.Key(key+":"+cm.ID)).FillWidth().Padding(6, 10).Gap(8).AlignItems(ui.Center).
							BorderWidth(1, 0, 0, 0).BorderColor(p.lineSoft).Children(func() {
							ui.Text(c, nonEmptyString(cm.ShortID, shortCommitID(cm.ID))).Font("Menlo").FontSize(10.5).TextColor(p.accentInk).NoWrap().Shrink(0)
							ui.Text(c, cm.Subject).FontSize(11.5).TextColor(p.ink).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
							if cm.Author != "" {
								ui.Text(c, cm.Author).FontSize(10.5).TextColor(p.faint).SingleLine().Ellipsis("…").MaxWidth(84).Shrink(1)
							}
							if age := relativeAge(cm.AuthorTime, now); age != "" {
								ui.Text(c, age).Font("Menlo").FontSize(10).TextColor(p.faint).NoWrap().Shrink(0)
							}
						})
					}
				})
			})
		}
		renderSection("cmp-ahead", fmt.Sprintf(w.tr("仅在 %s（待合入）", "Only in %s (incoming)"), cmp.Target), p.success, cmp.AheadCommits)
		renderSection("cmp-behind", fmt.Sprintf(w.tr("仅在 %s（当前独有）", "Only in %s (local)"), cmp.Base), p.accentInk, cmp.BehindCommits)
	})
}

func (w *workbench) gitCompareFilesView(c *ui.Context, p palette, cmp *gitBranchCompare, width float32) {
	if len(cmp.Files) == 0 {
		ui.Column(c).Grow(1).Justify(ui.Center).AlignItems(ui.Center).Gap(6).Padding(20).Children(func() {
			ui.Icon(c, checkIcon).FontSize(20).TextColor(p.success)
			ui.Text(c, w.tr("两个分支没有文件内容差异", "No file differences between these branches")).FontSize(12.5).TextColor(p.muted)
		})
		return
	}
	selected := w.gitView.compareFile
	if selected == "" {
		selected = cmp.Files[0].Path
	}
	var shown *patchFile
	for i := range cmp.patches {
		if cmp.patches[i].name == selected {
			shown = &cmp.patches[i]
			break
		}
	}

	renderFileList := func() {
		for _, f := range cmp.Files {
			f := f
			isSel := f.Path == selected
			row := ui.Button(c.Key("cmp-file:"+f.Path), "").Label(f.Path).FillWidth().Height(26).Padding(0, 6).Radius(6).
				Border(0, ui.Transparent).Background(ui.Transparent).Justify(ui.Start)
			if isSel {
				row.Background(p.selected)
			} else if row.Hovered() {
				row.Background(p.rowHover)
			}
			row.Children(func() {
				ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
					letter := nonEmptyString(strings.TrimSpace(f.Index), "M")
					ui.Text(c, letter).Font("Menlo").FontSize(10).Bold().TextColor(gitStatusTone(p, letter)).Width(14).TextAlign(ui.Center).Shrink(0)
					dir, name := path.Split(f.Path)
					ui.Text(c, name).FontSize(11.5).FontWeight(500).TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
					if dir = strings.TrimSuffix(dir, "/"); dir != "" {
						ui.Text(c, dir).FontSize(10.5).TextColor(p.faint).SingleLine().Ellipsis("…").Shrink(3).MinWidth(0)
					}
				})
			})
			if row.Clicked() {
				w.gitView.compareFile = f.Path
			}
		}
	}

	renderDiff := func(wide bool) {
		var layout editor.DiffLayout
		ui.Row(c).Height(28).Gap(6).AlignItems(ui.Center).Shrink(0).Children(func() {
			ui.Icon(c, fileIcon).FontSize(12).TextColor(p.muted)
			ui.Text(c, selected).Font("Menlo").FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
			if shown != nil {
				ui.Text(c, fmt.Sprintf("+%d", shown.added)).Font("Menlo").FontSize(10).TextColor(p.success).NoWrap()
				ui.Text(c, fmt.Sprintf("-%d", shown.removed)).Font("Menlo").FontSize(10).TextColor(p.danger).NoWrap()
			}
			layout = w.gitDiffLayout(c, p, wide)
		})
		ui.Scroll(c.Key("cmp-diff:"+selected)).Grow(1).Basis(0).MinHeight(0).Radius(8).Background(p.code).Border(1, p.lineSoft).Children(func() {
			switch {
			case shown == nil || shown.state == nil:
				ui.Text(c, w.tr("该文件没有可显示的文本差异（可能为二进制或被截断）。", "No text diff available for this file.")).FontSize(11).TextColor(p.muted).Wrap().Padding(12)
			default:
				shown.state.Layout = layout
				editor.DiffView(c, shown.state).FillWidth()
			}
		})
	}

	if width >= 720 {
		ui.Row(c).Grow(1).Basis(0).MinHeight(0).Gap(10).AlignItems(ui.Stretch).Children(func() {
			ui.Scroll(c.Key("cmp-files")).Width(max(220, min(360, width*0.34))).Shrink(0).Children(renderFileList)
			ui.Column(c).Grow(1).MinWidth(0).Children(func() { renderDiff(width >= 960) })
		})
	} else {
		ui.Scroll(c.Key("cmp-files")).MaxHeight(150).Shrink(0).Children(renderFileList)
		ui.Column(c).Grow(1).Basis(0).MinHeight(0).Clip().Children(func() { renderDiff(false) })
	}
}
