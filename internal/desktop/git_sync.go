package desktop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	actionGitFetch = "git.fetch"
	actionGitPull  = "git.pull"
	actionGitPush  = "git.push"
	actionGitStash = "git.stash"
	actionPRStatus = "pr.status"
)

type prStatus struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Checks string `json:"checks"`
	URL    string `json:"url"`
}

var gitRemoteLabels = map[string]string{
	actionGitFetch: "Fetch",
	actionGitPull:  "Pull（仅快进）",
	actionGitPush:  "Push",
	actionGitStash: "Stash",
}

// gitRemote runs one sync action through Core, then re-reads the actual
// state. Pull is fast-forward only by contract; push only after confirmation.
func (w *workbench) gitRemote(action string) bool { return w.gitRemoteFor(w.selected, action) }

func (w *workbench) gitRemoteFor(target, action string) bool {
	label, known := gitRemoteLabels[action]
	if !known {
		return false
	}
	request, ok := w.gitRequest(target, action)
	if !ok {
		return false
	}
	if _, active := w.activeRun(target); active && action != actionGitFetch {
		w.setToolFeedback("git:"+target, notice{Text: "请等待任务结束后再执行 " + label + "。", Level: noticeWarn})
		return false
	}
	if state := w.features.git[target]; state != nil {
		request.Branch = state.Branch
	}
	if action == actionGitPull {
		request.Message = "--ff-only"
	}
	w.workbenchCall("git:"+target, request, func(response api.WorkbenchResponse, err error) {
		if w.features.gitOutputs == nil {
			w.features.gitOutputs = map[string]string{}
		}
		w.features.gitOutputs[target] = response.Output
		adopted := w.adoptGit(target, response)
		if err != nil {
			w.setToolFeedback("git:"+target, notice{Text: describe(label+" 未完成", err), Level: noticeError})
		} else {
			w.setToolFeedback("git:"+target, notice{Text: label + " 已完成；已刷新实际状态。", Level: noticeInfo})
		}
		if action == actionGitPush || action == actionGitFetch {
			delete(w.gitView.prLoaded, target)
		}
		if !adopted {
			w.refreshGit(target, true)
		}
	})
	return true
}

func decodePRStatus(response api.WorkbenchResponse) *prStatus {
	text := strings.TrimSpace(response.Output)
	if text == "" {
		text = strings.TrimSpace(string(response.Data))
	}
	if text == "" {
		return nil
	}
	var raw map[string]any
	if json.Unmarshal([]byte(text), &raw) != nil {
		return nil
	}
	if inner, ok := raw["pr"].(map[string]any); ok {
		raw = inner
	}
	status := &prStatus{}
	if n, ok := raw["number"].(float64); ok {
		status.Number = int(n)
	}
	str := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := raw[key].(string); ok && value != "" {
				return value
			}
		}
		return ""
	}
	status.Title, status.State, status.URL = str("title"), str("state"), str("url", "html_url")
	status.Checks = str("checks", "checksStatus", "status", "conclusion")
	if status.Number == 0 && status.URL == "" {
		return nil
	}
	return status
}

func (w *workbench) loadPR(target string) {
	if w.gitView.prLoaded == nil {
		w.gitView.prLoaded, w.gitView.pr = map[string]bool{}, map[string]*prStatus{}
	}
	request, ok := w.gitRequest(target, actionPRStatus)
	if w.gitView.prLoaded[target] || !ok {
		return
	}
	w.gitView.prLoaded[target] = true
	if state := w.features.git[target]; state != nil {
		request.Branch = state.Branch
	}
	w.workbenchCall("pr:"+target, request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			delete(w.gitView.pr, target)
			return
		}
		w.gitView.pr[target] = decodePRStatus(response)
	})
}

func (w *workbench) prChecksTone(p palette, checks string) (string, ui.Color) {
	switch strings.ToLower(checks) {
	case "success", "passed", "pass", "green":
		return w.tr3("检查通过", "Checks passed", "チェック通過"), p.success
	case "failure", "failed", "fail", "error", "red":
		return w.tr3("检查失败", "Checks failed", "チェック失敗"), p.danger
	case "pending", "running", "queued", "in_progress":
		return w.tr3("检查中", "Checks running", "チェック中"), p.running
	case "":
		return "", p.muted
	}
	return checks, p.muted
}

// prBadge reserves the slot beside the branch: a live PR with its checks.
func (w *workbench) prBadge(c *ui.Context, p palette, target string) {
	pr := w.gitView.pr[target]
	if pr == nil {
		return
	}
	checks, tone := w.prChecksTone(p, pr.Checks)
	label := fmt.Sprintf("PR #%d", pr.Number)
	if pr.Number == 0 {
		label = "PR"
	}
	badge := quietButton(c, p, "pr-badge", "").Label(label+" "+checks).Height(22).Padding(0, 7).Background(tone.Alpha(0.1)).Shrink(1).MinWidth(0)
	tip := strings.TrimSpace(pr.Title)
	if pr.URL != "" {
		tip = strings.TrimSpace(tip + "\n" + w.tr3("点击复制链接：", "Click to copy URL: ", "クリックしてリンクをコピー：") + pr.URL)
	}
	if tip != "" {
		badge.Tooltip(tip)
	}
	badge.Children(func() {
		ui.Row(c).Gap(3).AlignItems(ui.Center).MinWidth(0).Children(func() {
			ui.Icon(c, prIcon).FontSize(11).TextColor(tone).Shrink(0)
			ui.Text(c, label).FontSize(10).Bold().TextColor(tone).NoWrap().Shrink(0)
			if checks != "" {
				ui.Text(c, "· "+checks).FontSize(10).TextColor(tone).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			}
			if pr.State != "" && !strings.EqualFold(pr.State, "open") {
				ui.Text(c, "· "+pr.State).FontSize(10).TextColor(p.muted).NoWrap().Shrink(0)
			}
		})
	})
	if badge.Clicked() && pr.URL != "" {
		c.WriteClipboard(pr.URL)
		w.setToolFeedback("git:"+target, notice{Text: w.tr3("已复制 PR 链接；可在浏览器中查看。", "PR link copied; open it in your browser.", "PR リンクをコピーしました。"), Level: noticeInfo})
	}
}

// gitSyncBar is the compact remote toolbar: Fetch, Pull, Push and Stash,
// with Pop once stashes exist. Push asks once more inline (gitPushConfirm),
// naming the branch, so a stray click never publishes.
func (w *workbench) gitSyncBar(c *ui.Context, p palette, state *api.GitState, compact, iconOnly bool) {
	target := w.selected
	_, active := w.activeRun(target)
	busy := w.features.busy["git:"+target]
	padH := float32(8)
	if iconOnly {
		padH = 6
	} else if compact {
		padH = 6.5
	}
	ui.Row(c.Key("git-sync")).Height(28).Padding(2).Gap(1).Radius(8).Background(p.soft).AlignItems(ui.Center).Shrink(0).
		Role(ui.RoleToolbar).Label(w.tr3("Git 同步", "Git sync", "Git 同期")).Children(func() {
		for _, item := range []struct {
			action, text, label string
			icon                *ui.SVG
			badge               string
			badgeTone           ui.Color
		}{
			{actionGitFetch, "Fetch", w.tr3("Fetch", "Fetch", "フェッチ"), refreshIcon, "", p.faint},
			{actionGitPull, "Pull", w.tr3("Pull（仅快进）", "Pull (fast-forward)", "プル (Fast-forward)"), pullIcon, func() string {
				if state != nil && state.Behind > 0 {
					return fmt.Sprintf("%d", state.Behind)
				}
				return ""
			}(), p.warning},
			{actionGitPush, "Push", w.tr3("Push", "Push", "プッシュ"), pushIcon, func() string {
				if state != nil && state.Ahead > 0 {
					return fmt.Sprintf("%d", state.Ahead)
				}
				return ""
			}(), p.success},
			{actionGitStash, "Stash", "Stash", stashIcon, "", p.faint},
		} {
			item := item
			button := quietButton(c, p, item.action, "").Label(item.label).Height(24).Padding(0, padH).Radius(6)
			button.Tooltip(item.label)
			button.Children(func() {
				ui.Row(c).Gap(3).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, item.icon).FontSize(11.5).Shrink(0)
					if !iconOnly {
						ui.Text(c, item.text).FontSize(11).NoWrap()
					}
					if item.badge != "" {
						ui.Text(c, item.badge).Font("Menlo").FontSize(9.5).FontWeight(600).TextColor(item.badgeTone).NoWrap()
					}
				})
			})
			if busy || active && item.action != actionGitFetch {
				button.Disabled(true)
			}
			if button.Clicked() {
				if item.action == actionGitPush {
					w.gitView.pushConfirm = target
				} else {
					w.gitRemote(item.action)
				}
			}
		}
		w.stashPopButton(c, p, state, busy || active)
	})
}

// gitPushConfirm asks once more, inline, before a push publishes the branch.
func (w *workbench) gitPushConfirm(c *ui.Context, p palette, state *api.GitState) {
	target := w.selected
	_, active := w.activeRun(target)
	busy := w.features.busy["git:"+target]
	if w.gitView.pushConfirm != target {
		return
	}
	branch := w.tr3("当前分支", "current branch", "現在のブランチ")
	if state != nil && state.Branch != "" {
		branch = state.Branch
	}
	ui.Row(c.Key("push-confirm")).Gap(6).Padding(4, 8).Radius(8).AlignItems(ui.Center).Background(p.warning.Alpha(0.1)).Border(1, p.warning.Alpha(0.35)).Children(func() {
		ui.Icon(c, pushIcon).FontSize(12).TextColor(p.warning)
		ui.Text(c, fmt.Sprintf(w.tr3("推送 %s 到远程？", "Push %s to remote?", "%s をリモートへプッシュしますか？"), branch)).FontSize(11).TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
		if quietButton(c, p, "cancel-push", w.tr3("取消", "Cancel", "キャンセル")).Height(22).NoWrap().Clicked() {
			w.gitView.pushConfirm = ""
		}
		confirm := ui.PrimaryButton(c.Key("confirm-push"), w.tr3("确认推送", "Confirm push", "プッシュを実行")).FontSize(11).Height(22).Padding(0, 9).NoWrap()
		confirm.Disabled(busy || active)
		if confirm.Clicked() {
			w.gitView.pushConfirm = ""
			w.gitRemote(actionGitPush)
		}
	})
}
