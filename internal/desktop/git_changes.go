package desktop

import (
	"encoding/json"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

type gitWorktreeStatus struct {
	IsWorktree      bool   `json:"isWorktree"`
	Managed         bool   `json:"managed"`
	Exists          bool   `json:"exists"`
	Path            string `json:"path,omitempty"`
	Repository      string `json:"repository,omitempty"`
	Branch          string `json:"branch,omitempty"`
	BaseBranch      string `json:"baseBranch,omitempty"`
	UnmergedCommits int    `json:"unmergedCommits"`
	Dirty           bool   `json:"dirty"`
	CanMerge        bool   `json:"canMerge"`
	CanCleanup      bool   `json:"canCleanup"`
	Note            string `json:"note,omitempty"`
}

type gitViewState struct {
	selected    map[string]string
	messages    map[string]string
	commitOpen  bool
	expanded    bool
	pushConfirm string
	pr          map[string]*prStatus
	prLoaded    map[string]bool

	// JetBrains-style tool window state.
	tab          int // 0 commit, 1 log
	checked      map[string]map[string]bool
	amend        bool
	closed       map[string]bool
	layout       int // -1 follows the width; else 0 unified, 1 split
	layoutSet    bool
	messageFocus ui.Handle
	branchOpen   bool
	newBranch    string
	branchQuery  string
	pushAfter    map[string]bool
	logQuery     string
	logs         map[string][]api.GitCommit
	logLoaded    map[string]string
	commit       map[string]string
	details      map[string]*gitCommitDetail
	detailFile   map[string]string
	discard      map[string]string

	// IDEA log page.
	filter      gitLogFilter
	prompt      gitPrompt
	treeHidden  bool
	treeOpen    map[string]bool
	treeQuery   string
	pathOpen    bool
	pathDraft   string
	clipboard   string
	graphKey    string
	graphRows   []graphRow
	graphLanes  int
	dirClosed       map[string]bool
	allBranches     bool
	worktree        map[string]*gitWorktreeStatus
	worktreeConfirm map[string]string
}

func (w *workbench) gitDialog(c *ui.Context, p palette) {
	if !w.gitView.expanded {
		return
	}
	width, height := c.Size()
	ui.DialogBase(c, &w.gitView.expanded, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000066"))
		panel.Width(min(1200, width-32)).Height(height-40).Padding(16).Radius(12).Background(p.surface).Border(1, p.line)
		ui.Column(c).Fill().Gap(10).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, branchIcon).FontSize(15).TextColor(p.muted)
				ui.Text(c, "Git · "+w.titleFor(w.selected)).FontSize(14).Bold().Grow(1).SingleLine().Ellipsis("…")
				if iconButton(c, p, "close-git-review", "关闭 Git 变更", closeIcon).Clicked() {
					w.gitView.expanded = false
				}
			})
			ui.Column(c).Grow(1).MinHeight(0).Gap(8).Children(func() { w.ideaGitPanel(c, p, dockMaximized) })
			w.noticeLine(c, p, w.features.feedback["git:"+w.selected])
		})
	})
}

// gitRequest addresses a Git action: a committed thread's checkout, or the
// project folder chosen for a draft that has not sent its first message.
func (w *workbench) gitRequest(target, action string) (api.WorkbenchRequest, bool) {
	if w.hasThread(target) && !isLocalSession(target) {
		return api.WorkbenchRequest{Action: action, ThreadID: target}, true
	}
	if target == w.selected {
		if path := w.selectedWorkspacePath(); path != "" {
			return api.WorkbenchRequest{Action: action, Workspace: path}, true
		}
	}
	return api.WorkbenchRequest{}, false
}

// commitIntent is what the non-modal commit box asks Core for.
type commitIntent struct {
	message string
	paths   []string
	amend   bool
	push    bool
}

func (w *workbench) changeGit(action string, paths []string, message string) {
	w.changeGitWith(action, paths, commitIntent{message: message})
}

func (w *workbench) changeGitWith(action string, paths []string, intent commitIntent) {
	target := w.selected
	state := w.features.git[target]
	if state == nil {
		return
	}
	if _, active := w.activeRun(target); active {
		w.setToolFeedback("git:"+target, notice{Text: "请等待任务结束后再修改 Git。", Level: noticeWarn})
		return
	}
	request, ok := w.gitRequest(target, action)
	if !ok {
		return
	}
	labels := map[string]string{"git.stage": "暂存", "git.unstage": "取消暂存", "git.commit": "本地提交", actionGitDiscard: "回滚"}
	label := labels[action]
	if action == "git.commit" && intent.amend {
		label = "修正提交"
	}
	request.Branch, request.Paths, request.Message, request.Amend = state.Branch, append([]string(nil), paths...), intent.message, intent.amend
	w.workbenchCall("git:"+target, request, func(response api.WorkbenchResponse, err error) {
		if w.features.gitOutputs == nil {
			w.features.gitOutputs = map[string]string{}
		}
		w.features.gitOutputs[target] = response.Output
		if err != nil {
			w.setToolFeedback("git:"+target, notice{Text: describe(label+"结果需检查", err), Level: noticeError})
			w.refreshGit(target, true)
			return
		}
		w.setToolFeedback("git:"+target, notice{Text: label + "已完成；正在刷新实际状态。", Level: noticeInfo})
		if action == "git.commit" {
			delete(w.gitView.messages, target)
			delete(w.gitView.checked, target)
			w.gitView.amend = false
			delete(w.gitView.logLoaded, target)
		}
		if action == actionGitDiscard {
			delete(w.gitView.selected, target)
		}
		if action == "git.commit" && intent.push {
			// Commit & Push: the push names the same branch, once the commit
			// is confirmed; the refresh after it reads the actual result.
			w.gitRemoteFor(target, actionGitPush)
			return
		}
		if !w.adoptGit(target, response) {
			w.refreshGit(target, true)
		} else if w.shouldInspectWorktree(target) {
			w.loadWorktreeStatus(target)
		}
	})
}

func (w *workbench) shouldInspectWorktree(target string) bool {
	if target == "" {
		return false
	}
	if wt := w.gitView.worktree[target]; wt != nil && wt.IsWorktree {
		return true
	}
	if th, ok := w.threadByID(target); ok && th.Workspace != nil && th.Workspace.Kind == "worktree" {
		return true
	}
	return false
}

func (w *workbench) changeGitHunk(file string, staged bool, hunkIndex int, action string) {
	target := w.selected
	state := w.features.git[target]
	if state == nil || strings.TrimSpace(file) == "" {
		return
	}
	if _, active := w.activeRun(target); active {
		w.setToolFeedback("git:"+target, notice{Text: "请等待任务结束后再修改 Git。", Level: noticeWarn})
		return
	}
	gitAction := "git.hunk." + action
	request, ok := w.gitRequest(target, gitAction)
	if !ok {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"branch":    state.Branch,
		"file":      file,
		"staged":    staged,
		"hunkIndex": hunkIndex,
		"action":    action,
	})
	if err != nil {
		return
	}
	request.Branch = state.Branch
	request.Paths = []string{file}
	request.Input = payload

	label := "暂存块"
	switch action {
	case "unstage":
		label = "取消暂存块"
	case "revert":
		label = "撤回块"
	}
	w.workbenchCall("git:"+target, request, func(response api.WorkbenchResponse, err error) {
		if w.features.gitOutputs == nil {
			w.features.gitOutputs = map[string]string{}
		}
		w.features.gitOutputs[target] = response.Output
		if err != nil {
			w.setToolFeedback("git:"+target, notice{Text: describe(label+"结果需检查", err), Level: noticeError})
			w.refreshGit(target, true)
			return
		}
		w.setToolFeedback("git:"+target, notice{Text: label + "已完成。", Level: noticeInfo})
		if !w.adoptGit(target, response) {
			w.refreshGit(target, true)
		} else if w.shouldInspectWorktree(target) {
			w.loadWorktreeStatus(target)
		}
	})
}

func (w *workbench) loadWorktreeStatus(target string) {
	if target == "" {
		return
	}
	request, ok := w.gitRequest(target, actionGitWorktreeStatus)
	if !ok {
		return
	}
	w.workbenchCall("git-wt:"+target, request, func(response api.WorkbenchResponse, err error) {
		if err != nil || strings.TrimSpace(response.Output) == "" {
			return
		}
		var st gitWorktreeStatus
		if json.Unmarshal([]byte(response.Output), &st) != nil {
			return
		}
		if w.gitView.worktree == nil {
			w.gitView.worktree = map[string]*gitWorktreeStatus{}
		}
		w.gitView.worktree[target] = &st
	})
}

func (w *workbench) mergeAndCleanupWorktree(target string) {
	if target == "" {
		return
	}
	if _, active := w.activeRun(target); active {
		w.setToolFeedback("git:"+target, notice{Text: "请等待任务结束后再合并 Worktree。", Level: noticeWarn})
		return
	}
	request, ok := w.gitRequest(target, actionGitWorktreeMerge)
	if !ok {
		return
	}
	if w.gitView.worktreeConfirm != nil {
		delete(w.gitView.worktreeConfirm, target)
	}
	w.workbenchCall("git:"+target, request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.setToolFeedback("git:"+target, notice{Text: describe("合并并清理 Worktree 失败", err), Level: noticeError})
			w.loadWorktreeStatus(target)
			return
		}
		w.setToolFeedback("git:"+target, notice{Text: "Worktree 分支已合并并清理完成。", Level: noticeInfo})
		if !w.adoptGit(target, response) {
			w.refreshGit(target, true)
		}
		w.loadWorktreeStatus(target)
	})
}

func (w *workbench) cleanupWorktreeOnly(target string, force bool) {
	if target == "" {
		return
	}
	if _, active := w.activeRun(target); active {
		w.setToolFeedback("git:"+target, notice{Text: "请等待任务结束后再清理 Worktree。", Level: noticeWarn})
		return
	}
	request, ok := w.gitRequest(target, actionGitWorktreeClean)
	if !ok {
		return
	}
	request.Amend = force
	if w.gitView.worktreeConfirm != nil {
		delete(w.gitView.worktreeConfirm, target)
	}
	w.workbenchCall("git:"+target, request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.setToolFeedback("git:"+target, notice{Text: describe("清理 Worktree 失败", err), Level: noticeError})
			w.loadWorktreeStatus(target)
			return
		}
		w.setToolFeedback("git:"+target, notice{Text: "独立 Worktree 已清理。", Level: noticeInfo})
		w.loadWorktreeStatus(target)
	})
}
