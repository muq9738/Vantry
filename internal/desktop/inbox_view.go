package desktop

import (
	"fmt"
	"sort"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

var inboxIcon = lucide("inbox")

type inboxItem struct {
	scope     string
	owner     *workbench
	threadID  string
	title     string
	kind      string // "approval" | "failed" | "unread"
	summary   string
	request   *api.ProviderRequest
	updatedAt time.Time
}

func (w *workbench) inboxItems() []inboxItem {
	type scopeView struct {
		scope string
		view  *workbench
	}
	var views []scopeView
	if h := w.environments; h != nil {
		for _, rec := range h.records {
			if rec.view != nil {
				views = append(views, scopeView{scope: rec.identity(), view: rec.view})
			}
		}
	} else {
		views = []scopeView{{scope: "local", view: w}}
	}

	var items []inboxItem
	seenApprovalThread := map[string]bool{}
	for _, sv := range views {
		owner := sv.view
		for i := range owner.requests {
			req := owner.requests[i]
			if req.Status != "pending" {
				continue
			}
			key := sv.scope + scopeSep + req.ThreadID + scopeSep + req.ID
			seenApprovalThread[key] = true
			reqCopy := req
			items = append(items, inboxItem{
				scope:     sv.scope,
				owner:     owner,
				threadID:  req.ThreadID,
				title:     owner.displayTitleFor(req.ThreadID),
				kind:      "approval",
				summary:   singleLineSummaryText(req.Title, 64),
				request:   &reqCopy,
				updatedAt: req.CreatedAt,
			})
		}
		for _, thread := range owner.threads {
			if !owner.isUnread(thread.ID) {
				continue
			}
			kind := "unread"
			summary := owner.tr("已完成并有新回复", "Completed with new reply")
			at := owner.sessionActivity(thread.ID)
			if latest, ok := owner.latestRun(thread.ID); ok {
				if latest.Status == api.RunFailed || latest.Status == api.RunInterrupted {
					kind = "failed"
					summary = nonEmptyString(singleLineSummaryText(latest.Error, 64), owner.tr("任务失败或中断", "Turn failed or interrupted"))
				} else if line := singleLineSummaryText(latest.Output, 64); line != "" {
					summary = line
				}
				if latest.UpdatedAt.After(at) {
					at = latest.UpdatedAt
				}
			}
			items = append(items, inboxItem{
				scope:     sv.scope,
				owner:     owner,
				threadID:  thread.ID,
				title:     owner.displayTitleFor(thread.ID),
				kind:      kind,
				summary:   summary,
				updatedAt: at,
			})
		}
	}
	_ = seenApprovalThread

	rank := func(kind string) int {
		switch kind {
		case "approval":
			return 0
		case "failed":
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if rank(items[i].kind) != rank(items[j].kind) {
			return rank(items[i].kind) < rank(items[j].kind)
		}
		if !items[i].updatedAt.Equal(items[j].updatedAt) {
			return items[i].updatedAt.After(items[j].updatedAt)
		}
		return items[i].title < items[j].title
	})
	return items
}

func (w *workbench) inboxChromeBadge(c *ui.Context, p palette) {
	items := w.inboxItems()
	if len(items) == 0 {
		w.widgets.inboxOpen = false
		return
	}
	tone := p.accent
	hasApproval, hasFailed := false, false
	for _, item := range items {
		switch item.kind {
		case "approval":
			hasApproval = true
		case "failed":
			hasFailed = true
		}
	}
	if hasApproval {
		tone = p.warning
	} else if hasFailed {
		tone = p.danger
	}

	badge := ui.Button(c.Key("inbox-badge"), "").
		Label(w.tr("待处理收件箱", "Attention inbox")).
		Tooltip(fmt.Sprintf(w.tr("待处理事项 · %d", "Attention items · %d"), len(items))).
		Expanded(w.widgets.inboxOpen).
		Height(24).Padding(0, 8).Gap(5).Radius(12).Shrink(0).
		Background(tone.Alpha(0.14)).Border(1, tone.Alpha(0.38)).TextColor(tone)
	if badge.Hovered() || w.widgets.inboxOpen {
		badge.Background(tone.Alpha(0.22))
	}
	badge.Children(func() {
		ui.Icon(c, inboxIcon).FontSize(12).TextColor(tone)
		ui.Text(c, fmt.Sprint(len(items))).Font("Menlo").FontSize(11).FontWeight(600).TextColor(tone).NoWrap()
	})
	if badge.Clicked() {
		w.widgets.inboxOpen = !w.widgets.inboxOpen
	}

	width, _ := c.Size()
	ui.PopoverBase(c, badge, &w.widgets.inboxOpen, func(panel ui.Element) {
		panel.AttachTo(badge, ui.AnchorBottomEnd, ui.AnchorTopEnd).Margin(6, 0, 0).
			Width(composerPopoverWidth(width, 340)).Padding(10).Radius(14).Background(p.raised).Border(1, p.line).
			Shadow(0, 14, 40, 0, ui.Hex("#00000052"))
		if panel.Shortcut(0, ui.KeyEscape) {
			w.widgets.inboxOpen = false
			return
		}
		ui.Column(c).FillWidth().Gap(8).Children(func() {
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf(w.tr("待处理收件箱 · %d", "Attention inbox · %d"), len(items))).FontSize(12.5).FontWeight(600).TextColor(p.ink).Grow(1)
				hasUnread := false
				for _, item := range items {
					if item.kind != "approval" {
						hasUnread = true
						break
					}
				}
				if hasUnread {
					markAll := quietButton(c, p, "inbox-read-all", w.tr("全部已读", "Mark all read")).Height(22).FontSize(11)
					if markAll.Clicked() {
						w.markInboxAllRead(items)
					}
				}
			})
			ui.Scroll(c.Key("inbox-list")).FillWidth().MaxHeight(320).Children(func() {
				ui.Column(c).FillWidth().Gap(6).Children(func() {
					for idx, item := range items {
						w.inboxItemRow(c, p, idx, item)
					}
				})
			})
		})
	})
}

func (w *workbench) inboxItemRow(c *ui.Context, p palette, idx int, item inboxItem) {
	owner := item.owner
	if owner == nil {
		owner = w
	}
	tone := p.accent
	tag := w.tr("新回复", "Unread")
	switch item.kind {
	case "approval":
		tone = p.warning
		tag = w.tr("待确认", "Pending")
	case "failed":
		tone = p.danger
		tag = w.tr("异常", "Failed")
	}

	card := ui.Column(c.Key(fmt.Sprintf("inbox-item:%d:%s", idx, item.threadID))).
		FillWidth().Padding(8, 10).Gap(6).Radius(10).Background(p.soft).Border(1, p.lineSoft)
	card.Children(func() {
		jump := quietButton(c, p, "jump", "").Label(item.title).FillWidth().Padding(2, 4).Justify(ui.Start)
		jump.Children(func() {
			ui.Column(c).FillWidth().Gap(3).Children(func() {
				ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Text(c, item.title).FontSize(12.5).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
					pill(c, p, tag, tone)
				})
				if item.summary != "" {
					ui.Text(c, item.summary).FontSize(11.5).TextColor(p.muted).SingleLine().Ellipsis("…")
				}
			})
		})
		if jump.Clicked() {
			owner.clearUnread(item.threadID)
			w.widgets.inboxOpen = false
			if w.environments != nil && item.scope != "" {
				w.openScopedSession(scopedSessionID(item.scope, item.threadID))
			} else if _, isSub := owner.existingParentID(item.threadID); isSub {
				if parent, ok := owner.existingParentID(item.threadID); ok && owner.selected == parent {
					owner.openSubagentPanel(item.threadID)
				} else {
					owner.selectThread(item.threadID)
				}
			} else {
				owner.selectThread(item.threadID)
			}
			w.widgets.composerFocus.Focus()
		}

		if item.request != nil {
			req := *item.request
			busy := owner.controlBusy("request:" + req.ID)
			allowOpt, _, denyOpt, isSelectApproval := parseApprovalSelect(req)
			if req.Method == "confirm" || isSelectApproval {
				ui.Row(c).FillWidth().Gap(6).Justify(ui.End).Children(func() {
					deny := ui.Button(c.Key("inbox-deny"), w.tr("拒绝", "Deny")).FontSize(11).Height(24).Padding(0, 10).Radius(6).Disabled(busy)
					if deny.Clicked() {
						if isSelectApproval {
							owner.respondRequest(req, denyOpt, false, false)
						} else {
							owner.respondRequest(req, "", false, false)
						}
					}
					allow := ui.PrimaryButton(c.Key("inbox-allow"), w.tr("允许", "Allow")).FontSize(11).Height(24).Padding(0, 10).Radius(6).Disabled(busy)
					if allow.Clicked() {
						if isSelectApproval {
							owner.respondRequest(req, allowOpt, false, false)
						} else {
							owner.respondRequest(req, "", true, false)
						}
					}
				})
			}
		}
	})
}

func (w *workbench) markInboxAllRead(items []inboxItem) {
	for _, item := range items {
		if item.kind != "approval" && item.owner != nil {
			item.owner.clearUnread(item.threadID)
		}
	}
}
