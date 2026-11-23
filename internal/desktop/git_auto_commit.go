package desktop

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/api"
)

const maxAutoCommitRunes = 20

func truncateCommitRunes(s string, limit int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit <= 1 {
		return string(runes[:max(0, limit)])
	}
	return string(runes[:limit-1]) + "…"
}

func shortCommitTargetName(filePath string, maxRunes int) string {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(filePath), "\\", "/"))
	if base == "" || base == "." || base == "/" {
		base = "files"
	}
	if utf8.RuneCountInString(base) <= maxRunes {
		return base
	}
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if stem != "" && utf8.RuneCountInString(stem) <= maxRunes {
		return stem
	}
	return truncateCommitRunes(base, maxRunes)
}

func commonCommitScope(files []api.GitFile, maxRunes int) string {
	if len(files) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, f := range files {
		clean := strings.ReplaceAll(strings.TrimSpace(f.Path), "\\", "/")
		dir := path.Dir(clean)
		if dir != "." && dir != "/" && dir != "" {
			seg := path.Base(dir)
			if seg != "" && seg != "." {
				counts[seg]++
			}
		}
	}
	var best string
	var bestCount int
	for seg, n := range counts {
		if n > bestCount || (n == bestCount && seg < best) {
			best, bestCount = seg, n
		}
	}
	if best != "" && bestCount >= 2 {
		return truncateCommitRunes(best, maxRunes)
	}
	return shortCommitTargetName(files[0].Path, maxRunes)
}

// autoCommitSummaryForFiles produces a concise commit message strictly <= 20
// runes matching the active interface language (zh / en / ja).
func autoCommitSummaryForFiles(files []api.GitFile, amend bool, locale string) string {
	locale = normalizeLocale(locale)
	if len(files) == 0 {
		switch locale {
		case localeEN:
			if amend {
				return "Amend last commit"
			}
			return "Update workspace"
		case localeJA:
			if amend {
				return "直前のコミットを整理"
			}
			return "変更を更新"
		default:
			if amend {
				return "整理最近提交"
			}
			return "更新项目内容"
		}
	}

	added, deleted := 0, 0
	for _, f := range files {
		code := strings.TrimSpace(f.Index + f.Worktree)
		switch {
		case f.Index == "?" || strings.Contains(code, "A"):
			added++
		case strings.Contains(code, "D"):
			deleted++
		}
	}

	n := len(files)
	switch locale {
	case localeEN:
		verb := "Update"
		if added == n {
			verb = "Add"
		} else if deleted == n {
			verb = "Remove"
		}
		if n == 1 {
			budget := maxAutoCommitRunes - len(verb) - 1
			return truncateCommitRunes(verb+" "+shortCommitTargetName(files[0].Path, budget), maxAutoCommitRunes)
		}
		suffix := fmt.Sprintf(" +%d", n-1)
		budget := maxAutoCommitRunes - len(verb) - 1 - len(suffix)
		if budget >= 4 {
			scope := commonCommitScope(files, budget)
			cand := fmt.Sprintf("%s %s%s", verb, scope, suffix)
			if utf8.RuneCountInString(cand) <= maxAutoCommitRunes {
				return cand
			}
		}
		return truncateCommitRunes(fmt.Sprintf("%s %d files", verb, n), maxAutoCommitRunes)

	case localeJA:
		action := "を更新"
		if added == n {
			action = "を追加"
		} else if deleted == n {
			action = "を削除"
		}
		if n == 1 {
			budget := maxAutoCommitRunes - utf8.RuneCountInString(action) - 1
			target := shortCommitTargetName(files[0].Path, budget)
			return truncateCommitRunes(target+" "+action, maxAutoCommitRunes)
		}
		tail := fmt.Sprintf("他%d件%s", n-1, action)
		budget := maxAutoCommitRunes - utf8.RuneCountInString(tail) - 1
		if budget >= 3 {
			scope := commonCommitScope(files, budget)
			cand := scope + " " + tail
			if utf8.RuneCountInString(cand) <= maxAutoCommitRunes {
				return cand
			}
		}
		return truncateCommitRunes(fmt.Sprintf("%d件のファイル%s", n, action), maxAutoCommitRunes)

	default: // localeZH
		verb := "更新"
		if added == n {
			verb = "新增"
		} else if deleted == n {
			verb = "移除"
		}
		if n == 1 {
			budget := maxAutoCommitRunes - utf8.RuneCountInString(verb) - 1
			target := shortCommitTargetName(files[0].Path, budget)
			return truncateCommitRunes(verb+" "+target, maxAutoCommitRunes)
		}
		tail := fmt.Sprintf("等%d处", n)
		budget := maxAutoCommitRunes - utf8.RuneCountInString(verb) - utf8.RuneCountInString(tail) - 2
		if budget >= 3 {
			scope := commonCommitScope(files, budget)
			cand := fmt.Sprintf("%s %s %s", verb, scope, tail)
			if utf8.RuneCountInString(cand) <= maxAutoCommitRunes {
				return cand
			}
		}
		return truncateCommitRunes(fmt.Sprintf("%s%d个文件", verb, n), maxAutoCommitRunes)
	}
}

func (w *workbench) autoCommitSummary(state *api.GitState, changes *api.WorkspaceChanges, locale string) string {
	if state != nil && len(state.Files) > 0 {
		return autoCommitSummaryForFiles(state.Files, w.gitView.amend, locale)
	}
	if changes != nil && len(changes.Files) > 0 {
		converted := make([]api.GitFile, 0, len(changes.Files))
		for _, f := range changes.Files {
			status := strings.TrimSpace(f.Status)
			idx, wt := " ", "M"
			switch status {
			case "??", "A":
				idx, wt = "?", "?"
			case "D":
				idx, wt = " ", "D"
			}
			converted = append(converted, api.GitFile{Path: f.Path, Index: idx, Worktree: wt})
		}
		return autoCommitSummaryForFiles(converted, false, locale)
	}
	return autoCommitSummaryForFiles(nil, w.gitView.amend, locale)
}

func (w *workbench) autoCommitSummaryForSelection(state *api.GitState, selectedPaths []string, locale string) string {
	if state == nil {
		return autoCommitSummaryForFiles(nil, w.gitView.amend, locale)
	}
	if len(selectedPaths) == 0 {
		groups := groupGitFiles(state.Files)
		if len(groups.staged) > 0 {
			return autoCommitSummaryForFiles(groups.staged, w.gitView.amend, locale)
		}
		return autoCommitSummaryForFiles(state.Files, w.gitView.amend, locale)
	}
	set := map[string]bool{}
	for _, p := range selectedPaths {
		set[p] = true
	}
	var picked []api.GitFile
	for _, f := range state.Files {
		if set[f.Path] {
			picked = append(picked, f)
		}
	}
	return autoCommitSummaryForFiles(picked, w.gitView.amend, locale)
}

// oneClickCommitAndPush implements T3 Code's 1-click "Commit & push" button:
// it reads fresh Git status if needed, stages all working-tree changes when
// dirty, generates a <=20-rune commit summary in the active UI language when
// no manual commit message is entered, commits, and pushes in one flow.
func (w *workbench) oneClickCommitAndPush(target string) bool {
	if target == "" {
		target = w.selected
	}
	if _, active := w.activeRun(target); active {
		msg := w.tr3("请等待当前任务结束后再提交并推送。", "Wait for the current turn to finish before committing & pushing.", "実行中のターンが完了してからコミット＆プッシュしてください。")
		w.workspaceNotice = notice{Text: msg, Level: noticeWarn}
		w.setToolFeedback("git:"+target, notice{Text: msg, Level: noticeWarn})
		return false
	}
	statusReq, ok := w.gitRequest(target, "git.status")
	if !ok {
		return false
	}
	service, ok := w.service.(api.WorkbenchService)
	if !ok {
		return false
	}
	if w.features.busy == nil {
		w.features.busy = map[string]bool{}
	}
	if w.features.busy["git:"+target] {
		return false
	}

	manualMsg := strings.TrimSpace(w.gitView.messages[target])
	locale := w.widgets.locale

	w.features.busy["git:"+target] = true
	progress := w.tr3("正在提交并推送…", "Committing & pushing…", "コミット＆プッシュ中…")
	w.workspaceNotice = notice{Text: progress, Level: noticeInfo}
	w.setToolFeedback("git:"+target, notice{Text: progress, Level: noticeInfo})

	w.start(func() {
		ctx, cancel := context.WithTimeout(w.ctx, 45*time.Second)
		defer cancel()

		resp, err := service.Workbench(ctx, statusReq)
		if err != nil {
			w.post(func() {
				delete(w.features.busy, "git:"+target)
				errMsg := describe(w.tr3("读取 Git 状态失败", "Failed to inspect Git status", "Git 状態の取得に失敗しました"), err)
				w.workspaceNotice = notice{Text: errMsg, Level: noticeError}
				w.setToolFeedback("git:"+target, notice{Text: errMsg, Level: noticeError})
			})
			return
		}
		state := resp.Git
		if state == nil {
			w.post(func() {
				delete(w.features.busy, "git:"+target)
				errMsg := w.tr3("当前目录不是 Git 仓库", "Workspace is not a Git repository", "Git リポジトリではありません")
				w.workspaceNotice = notice{Text: errMsg, Level: noticeWarn}
				w.setToolFeedback("git:"+target, notice{Text: errMsg, Level: noticeWarn})
			})
			return
		}
		if state.Conflicts {
			w.post(func() {
				delete(w.features.busy, "git:"+target)
				errMsg := w.tr3("请先解决合并冲突", "Resolve merge conflicts first", "先にコンフリクトを解消してください")
				w.workspaceNotice = notice{Text: errMsg, Level: noticeError}
				w.setToolFeedback("git:"+target, notice{Text: errMsg, Level: noticeError})
			})
			return
		}

		committedMsg := ""
		if len(state.Files) > 0 {
			msg := manualMsg
			if msg == "" {
				msg = autoCommitSummaryForFiles(state.Files, false, locale)
			}
			var allPaths []string
			groups := groupGitFiles(state.Files)
			// When unstaged or untracked files exist, include all changed paths (and rename origins) so 1-click commits everything cleanly.
			if len(groups.changes) > 0 || len(groups.untracked) > 0 {
				allPaths = commitPaths(state.Files)
			}
			commitReq := statusReq
			commitReq.Action = "git.commit"
			commitReq.Branch = state.Branch
			commitReq.Paths = allPaths
			commitReq.Message = msg
			commitResp, err := service.Workbench(ctx, commitReq)
			if err != nil {
				w.post(func() {
					delete(w.features.busy, "git:"+target)
					errMsg := describe(w.tr3("提交失败", "Commit failed", "コミットに失敗しました"), err)
					w.workspaceNotice = notice{Text: errMsg, Level: noticeError}
					w.setToolFeedback("git:"+target, notice{Text: errMsg, Level: noticeError})
					w.refreshGit(target, true)
				})
				return
			}
			committedMsg = msg
			if commitResp.Git != nil {
				state = commitResp.Git
			}
		} else if state.Ahead == 0 {
			w.post(func() {
				delete(w.features.busy, "git:"+target)
				w.adoptGit(target, api.WorkbenchResponse{Git: state})
				cleanMsg := w.tr3("工作区干净，无需提交或推送。", "Working tree clean; nothing to commit or push.", "変更はなく、同期済みです。")
				w.workspaceNotice = notice{Text: cleanMsg, Level: noticeInfo}
				w.setToolFeedback("git:"+target, notice{Text: cleanMsg, Level: noticeInfo})
			})
			return
		}

		pushReq := statusReq
		pushReq.Action = actionGitPush
		pushReq.Branch = state.Branch
		pushResp, pushErr := service.Workbench(ctx, pushReq)

		w.post(func() {
			delete(w.features.busy, "git:"+target)
			if committedMsg != "" {
				delete(w.gitView.messages, target)
				delete(w.gitView.checked, target)
				w.gitView.amend = false
				delete(w.gitView.logLoaded, target)
			}
			delete(w.gitView.prLoaded, target)
			adopted := w.adoptGit(target, pushResp)
			if !adopted {
				w.refreshGit(target, true)
			}
			if w.hasThread(target) {
				w.refreshWorkspace()
			}
			if pushErr != nil {
				prefix := w.tr3("推送未完成", "Push failed", "プッシュに失敗しました")
				if committedMsg != "" {
					prefix = fmt.Sprintf(w.tr3("已提交「%s」，但推送未完成", "Committed %q, but push failed", "「%s」をコミットしましたが、プッシュに失敗しました"), committedMsg)
				}
				errMsg := describe(prefix, pushErr)
				w.workspaceNotice = notice{Text: errMsg, Level: noticeError}
				w.setToolFeedback("git:"+target, notice{Text: errMsg, Level: noticeError})
				return
			}
			doneMsg := w.tr3("已推送至远程。", "Pushed to remote.", "リモートへプッシュしました。")
			if committedMsg != "" {
				doneMsg = fmt.Sprintf(w.tr3("已提交「%s」并推送。", "Committed %q & pushed.", "「%s」をコミット＆プッシュしました。"), committedMsg)
			}
			w.workspaceNotice = notice{Text: doneMsg, Level: noticeInfo}
			w.setToolFeedback("git:"+target, notice{Text: doneMsg, Level: noticeInfo})
		})
	})
	return true
}
