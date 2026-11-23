package desktop

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	gitwork "github.com/muq9738/Vantry/internal/adapters/git"
	"github.com/muq9738/Vantry/pkg/api"
)

type rewindCheckpoint struct {
	ID        string    `json:"id"`
	Ref       string    `json:"ref"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"createdAt"`
}

type rewindState struct {
	open           bool
	threadID       string
	selectedRunID  string
	checkpoints    []rewindCheckpoint
	checkpointsMap map[string]rewindCheckpoint
	loading        bool
	executing      bool
	err            string
	diffForID      string
	diffText       string
	diffLoading    bool
	diffErr        string
}

func (w *workbench) openRewindDialog(initialRunID string) {
	threadID := w.selected
	if threadID == "" || isLocalSession(threadID) || !w.hasThread(threadID) {
		return
	}
	runs := w.runsFor(threadID)
	var settled []api.Run
	for _, r := range runs {
		if r.Status.Terminal() {
			settled = append(settled, r)
		}
	}
	if len(settled) == 0 {
		w.setAction(w.tr("当前会话还没有已完成的轮次可回退。", "No settled turns to rewind yet."))
		return
	}
	chosen := strings.TrimSpace(initialRunID)
	found := false
	for _, r := range settled {
		if r.ID == chosen {
			found = true
			break
		}
	}
	if !found {
		chosen = settled[len(settled)-1].ID
	}
	w.widgets.rewind = rewindState{
		open:          true,
		threadID:      threadID,
		selectedRunID: chosen,
	}
	if _, ok := w.service.(api.WorkbenchService); ok {
		w.loadCheckpointsForRewind(threadID)
	}
}

func (w *workbench) loadCheckpointsForRewind(threadID string) {
	w.widgets.rewind.loading = true
	w.widgets.rewind.err = ""
	w.workbenchCall("checkpoints:"+threadID, api.WorkbenchRequest{
		Action:   "git.checkpoints.list",
		ThreadID: threadID,
	}, func(response api.WorkbenchResponse, err error) {
		if !w.widgets.rewind.open || w.widgets.rewind.threadID != threadID {
			return
		}
		w.widgets.rewind.loading = false
		if err != nil {
			w.widgets.rewind.err = describe(w.tr("读取工作区检查点失败", "Could not list workspace checkpoints"), err)
			return
		}
		var payload struct {
			Checkpoints []rewindCheckpoint `json:"checkpoints"`
		}
		if err := json.Unmarshal([]byte(response.Output), &payload); err != nil {
			w.widgets.rewind.err = w.tr("检查点列表格式无效", "Unrecognized checkpoint list")
			return
		}
		index := make(map[string]rewindCheckpoint, len(payload.Checkpoints))
		for _, cp := range payload.Checkpoints {
			if cp.ID != "" {
				index[cp.ID] = cp
			}
		}
		w.widgets.rewind.checkpoints = payload.Checkpoints
		w.widgets.rewind.checkpointsMap = index
	})
}

func (w *workbench) loadCheckpointDiff(threadID, cpID string) {
	if cpID == "" {
		return
	}
	w.widgets.rewind.diffForID = cpID
	w.widgets.rewind.diffText = ""
	w.widgets.rewind.diffErr = ""
	w.widgets.rewind.diffLoading = true
	w.workbenchCall("checkpoint-diff:"+threadID+":"+cpID, api.WorkbenchRequest{
		Action:   "git.checkpoints.diff",
		ThreadID: threadID,
		Name:     cpID,
	}, func(response api.WorkbenchResponse, err error) {
		if !w.widgets.rewind.open || w.widgets.rewind.threadID != threadID || w.widgets.rewind.diffForID != cpID {
			return
		}
		w.widgets.rewind.diffLoading = false
		if err != nil {
			w.widgets.rewind.diffErr = describe(w.tr("读取文件差异失败", "Could not diff checkpoint"), err)
			return
		}
		w.widgets.rewind.diffText = response.Output
	})
}

func (w *workbench) restoreBackupCheckpoint(threadID, backupID string) {
	if threadID == "" || backupID == "" || w.widgets.rewind.executing {
		return
	}
	w.widgets.rewind.executing = true
	w.widgets.rewind.err = ""
	w.workbenchCall("checkpoint-restore:"+threadID, api.WorkbenchRequest{
		Action:   "git.checkpoints.restore",
		ThreadID: threadID,
		Name:     backupID,
		Amend:    true,
	}, func(_ api.WorkbenchResponse, err error) {
		w.widgets.rewind.executing = false
		if err != nil {
			w.widgets.rewind.err = describe(w.tr("恢复备份失败", "Could not restore backup"), err)
			return
		}
		w.widgets.rewind.open = false
		if w.widgets.workspaceOpen || w.features.panel == "git" {
			w.refreshWorkspace()
		}
		w.setAction(w.tr("已恢复到备份工作区状态。", "Restored workspace to the selected backup."))
	})
}

func (w *workbench) executeRewind(threadID string, targetRun api.Run, restoreFiles, rewindConv bool) {
	if threadID == "" || targetRun.ID == "" || w.widgets.rewind.executing {
		return
	}
	w.widgets.rewind.executing = true
	w.widgets.rewind.err = ""

	dispatchConversationRewind := func(filesRestored bool) {
		cmd := api.Command{
			Version:   api.ProtocolVersion,
			CommandID: newCommandID("rewind"),
			Kind:      api.RewindThread,
			ThreadID:  threadID,
			RunID:     targetRun.ID,
		}
		w.dispatch(cmd, func(receipt api.Receipt, err error) {
			w.post(func() {
				w.widgets.rewind.executing = false
				if err != nil {
					w.widgets.rewind.err = describe(w.tr("回退对话失败", "Could not rewind conversation"), err)
					return
				}
				if !receipt.Accepted {
					w.widgets.rewind.err = w.tr("本地服务未接受回退请求，请重试。", "Local service did not accept rewind request.")
					return
				}
				w.widgets.rewind.open = false
				if targetRun.Input != "" {
					w.setDraft(threadID, strings.TrimPrefix(targetRun.Input, planModePrefix))
				}
				if filesRestored {
					if w.widgets.workspaceOpen || w.features.panel == "git" {
						w.refreshWorkspace()
					}
					w.setAction(w.tr("已回退对话与文件快照，并恢复输入到草稿。", "Rewound conversation and workspace files, and restored prompt to draft."))
				} else {
					w.setAction(w.tr("已回退对话并恢复输入到草稿。", "Rewound conversation and restored prompt to draft."))
				}
				w.widgets.composerFocus.Focus()
			})
		})
	}

	if !restoreFiles {
		if rewindConv {
			dispatchConversationRewind(false)
		} else {
			w.widgets.rewind.executing = false
		}
		return
	}

	cpID := gitwork.RunCheckpointID(targetRun.ID)
	w.workbenchCall("checkpoint-restore:"+threadID, api.WorkbenchRequest{
		Action:   "git.checkpoints.restore",
		ThreadID: threadID,
		Name:     cpID,
		Amend:    true,
	}, func(_ api.WorkbenchResponse, err error) {
		if err != nil {
			w.widgets.rewind.executing = false
			w.widgets.rewind.err = describe(w.tr("恢复文件快照失败，已保留当前对话与工作区", "Could not restore workspace files; conversation left unchanged"), err)
			return
		}
		if !rewindConv {
			w.widgets.rewind.executing = false
			w.widgets.rewind.open = false
			if w.widgets.workspaceOpen || w.features.panel == "git" {
				w.refreshWorkspace()
			}
			w.setAction(w.tr("已恢复该轮次前的文件快照（已自动保存恢复前备份）。", "Restored workspace files to before this turn (pre-restore backup saved)."))
			return
		}
		dispatchConversationRewind(true)
	})
}

func (w *workbench) rewindDialog(c *ui.Context, p palette) {
	if !w.widgets.rewind.open {
		return
	}
	threadID := w.widgets.rewind.threadID
	if threadID == "" || !w.hasThread(threadID) {
		w.widgets.rewind.open = false
		return
	}
	runs := w.runsFor(threadID)
	type turnEntry struct {
		index     int
		run       api.Run
		prevRunID string
	}
	var settled []turnEntry
	prevID := ""
	for i, r := range runs {
		if r.Status.Terminal() {
			settled = append(settled, turnEntry{index: i + 1, run: r, prevRunID: prevID})
			prevID = r.ID
		}
	}
	if len(settled) == 0 {
		w.widgets.rewind.open = false
		return
	}

	var selectedEntry *turnEntry
	for i := len(settled) - 1; i >= 0; i-- {
		if settled[i].run.ID == w.widgets.rewind.selectedRunID {
			e := settled[i]
			selectedEntry = &e
			break
		}
	}
	if selectedEntry == nil {
		e := settled[len(settled)-1]
		selectedEntry = &e
		w.widgets.rewind.selectedRunID = e.run.ID
	}

	_, hasActive := w.activeRun(threadID)
	now := c.Now()

	w.dialogFrame(c, p, &w.widgets.rewind.open, "rewind", w.tr("回退与检查点", "Rewind & checkpoints"), w.tr("关闭回退窗口", "Close rewind"), rollbackIcon, 640, func(inner float32) {
		ui.Text(c, w.tr("选择要回到的对话轮次；若该轮有 Git 文件快照，可同步恢复工作区或分叉新会话。", "Choose a turn to rewind to; restore workspace files when a Git snapshot exists, or fork from that point.")).
			FontSize(12).TextColor(p.muted).Wrap()

		if hasActive {
			ui.Text(c, w.tr("当前仍有任务运行中，请先停止后再执行回退。", "A turn is currently running; stop it before rewinding.")).
				FontSize(11.5).TextColor(p.warning).Wrap()
		}
		if w.widgets.rewind.err != "" {
			ui.Text(c, w.widgets.rewind.err).FontSize(11.5).TextColor(p.danger).Wrap()
		}

		ui.Scroll(c.Key("rewind-turns")).FillWidth().MaxHeight(240).Radius(10).Border(1, p.lineSoft).Background(p.surface).Children(func() {
			ui.Column(c).FillWidth().Padding(6).Gap(4).Children(func() {
				for i := len(settled) - 1; i >= 0; i-- {
					entry := settled[i]
					r := entry.run
					cpID := gitwork.RunCheckpointID(r.ID)
					_, hasCP := w.widgets.rewind.checkpointsMap[cpID]
					selected := r.ID == selectedEntry.run.ID
					prompt := singleLineSummaryText(strings.TrimPrefix(r.Input, planModePrefix), 72)
					if prompt == "" {
						prompt = w.tr("（空输入）", "(empty prompt)")
					}
					row := ui.Button(c.Key("rewind-run:"+r.ID), "").Label(fmt.Sprintf("#%d %s", entry.index, prompt)).
						FillWidth().Padding(8, 10).Radius(8).Border(1, ui.Transparent).Background(ui.Transparent).TextColor(p.ink).Justify(ui.Start)
					if selected {
						row.Background(p.selected).BorderColor(p.accent.Alpha(0.45))
					} else if row.Hovered() {
						row.Background(p.soft)
					}
					row.Children(func() {
						ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
							ui.Text(c, fmt.Sprintf("#%d", entry.index)).Font("Menlo").FontSize(11).TextColor(p.faint).NoWrap().Shrink(0)
							ui.Text(c, prompt).FontSize(12.5).TextColor(p.ink).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
							if hasCP {
								pill(c, p, w.tr("含文件快照", "Snapshot"), p.success)
							}
							if !r.CreatedAt.IsZero() {
								ui.Text(c, w.formatAge(now, r.CreatedAt)).FontSize(11).TextColor(p.faint).NoWrap().Shrink(0)
							}
						})
					})
					if row.Clicked() {
						w.widgets.rewind.selectedRunID = r.ID
						w.widgets.rewind.diffText = ""
						w.widgets.rewind.diffForID = ""
						w.widgets.rewind.diffErr = ""
					}
				}
			})
		})

		selectedCPID := gitwork.RunCheckpointID(selectedEntry.run.ID)
		_, hasSelectedCP := w.widgets.rewind.checkpointsMap[selectedCPID]

		if hasSelectedCP {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, w.tr("快照 ID：", "Snapshot: ")+selectedCPID).Font("Menlo").FontSize(11).TextColor(p.faint).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
				diffLabel := w.tr("查看文件差异", "Preview file diff")
				if w.widgets.rewind.diffForID == selectedCPID && w.widgets.rewind.diffLoading {
					diffLabel = w.tr("正在计算差异…", "Loading diff…")
				}
				diffBtn := quietButton(c, p, "rewind-preview-diff", diffLabel).Height(24).Disabled(w.widgets.rewind.diffLoading)
				if diffBtn.Clicked() {
					w.loadCheckpointDiff(threadID, selectedCPID)
				}
			})
		} else if w.widgets.rewind.loading {
			ui.Text(c, w.tr("正在检索 Git 文件快照…", "Checking Git checkpoints…")).FontSize(11).TextColor(p.faint)
		}

		if w.widgets.rewind.diffErr != "" {
			ui.Text(c, w.widgets.rewind.diffErr).FontSize(11).TextColor(p.danger).Wrap()
		}
		if w.widgets.rewind.diffForID == selectedCPID && !w.widgets.rewind.diffLoading {
			preview := w.widgets.rewind.diffText
			if strings.TrimSpace(preview) == "" && w.widgets.rewind.diffErr == "" {
				preview = w.tr("当前工作区与该轮次前快照无文件差异。", "No file differences between current workspace and this snapshot.")
			}
			if preview != "" {
				ui.Scroll(c.Key("rewind-diff-scroll")).FillWidth().MaxHeight(150).Padding(8, 10).Radius(8).Background(p.code).Border(1, p.lineSoft).Children(func() {
					ui.Text(c, preview).Font("Menlo").FontSize(11).LineHeight(1.45).TextColor(p.ink).Wrap().Selectable()
				})
			}
		}

		busy := w.widgets.rewind.executing || hasActive
		ui.Row(c).FillWidth().Gap(8).Wrap().AlignItems(ui.Center).Children(func() {
			if hasSelectedCP {
				both := ui.PrimaryButton(c.Key("rewind-both"), w.tr("回退对话与文件", "Rewind conversation & files")).
					Height(30).Padding(0, 12).FontSize(12).Radius(8).Disabled(busy)
				if both.Clicked() {
					w.executeRewind(threadID, selectedEntry.run, true, true)
				}
			}
			var convOnly ui.Element
			if hasSelectedCP {
				convOnly = quietButton(c, p, "rewind-conv", w.tr("仅回退对话", "Rewind conversation only")).
					Height(30).Padding(0, 12).FontSize(12).Radius(8).Disabled(busy)
			} else {
				convOnly = ui.PrimaryButton(c.Key("rewind-conv"), w.tr("仅回退对话", "Rewind conversation only")).
					Height(30).Padding(0, 12).FontSize(12).Radius(8).Disabled(busy)
			}
			if convOnly.Clicked() {
				w.executeRewind(threadID, selectedEntry.run, false, true)
			}
			if hasSelectedCP {
				filesOnly := quietButton(c, p, "rewind-files", w.tr("仅恢复文件", "Restore files only")).
					Height(30).Padding(0, 12).FontSize(12).Radius(8).Disabled(busy)
				if filesOnly.Clicked() {
					w.executeRewind(threadID, selectedEntry.run, true, false)
				}
			}
			forkBtn := quietButton(c, p, "rewind-fork", w.tr("从此处分叉", "Fork from here")).
				Height(30).Padding(0, 12).FontSize(12).Radius(8).Disabled(w.widgets.rewind.executing)
			if forkBtn.Clicked() {
				w.widgets.rewind.open = false
				if selectedEntry.prevRunID != "" {
					w.forkThread(threadID, selectedEntry.prevRunID)
				} else {
					var ws *api.WorkspaceSpec
					if th, ok := w.threadByID(threadID); ok {
						ws = th.Workspace
					}
					w.newSessionIn(ws)
					if selectedEntry.run.Input != "" {
						w.setDraft(w.selected, strings.TrimPrefix(selectedEntry.run.Input, planModePrefix))
					}
				}
			}
		})

		ui.Text(c, w.tr("恢复文件前会自动保存 pre-restore 备份快照，可随时撤销。", "A pre-restore backup snapshot is saved automatically before restoring workspace files.")).
			FontSize(11).TextColor(p.faint).Wrap()

		var backups []rewindCheckpoint
		for _, cp := range w.widgets.rewind.checkpoints {
			if strings.HasPrefix(cp.ID, "pre-restore-") {
				backups = append(backups, cp)
				if len(backups) == 2 {
					break
				}
			}
		}
		if len(backups) > 0 {
			ui.Column(c).FillWidth().Padding(8, 10).Gap(4).Radius(8).Background(p.soft).Border(1, p.lineSoft).Children(func() {
				ui.Text(c, w.tr("最近恢复前备份", "Recent pre-restore backups")).FontSize(11).FontWeight(600).TextColor(p.muted)
				for _, bcp := range backups {
					bcp := bcp
					ui.Row(c.Key("backup:"+bcp.ID)).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, bcp.ID).Font("Menlo").FontSize(10.5).TextColor(p.muted).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
						restore := quietButton(c, p, "restore-backup:"+bcp.ID, w.tr("恢复", "Restore")).Height(22).FontSize(11).Disabled(busy)
						if restore.Clicked() {
							w.restoreBackupCheckpoint(threadID, bcp.ID)
						}
					})
				}
			})
		}
	})
}

func singleLineSummaryText(text string, maxRunes int) string {
	clean := strings.Join(strings.Fields(text), " ")
	if maxRunes <= 0 {
		return clean
	}
	runes := []rune(clean)
	if len(runes) <= maxRunes {
		return clean
	}
	return string(runes[:maxRunes]) + "…"
}
