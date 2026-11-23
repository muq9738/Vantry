package desktop

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// effortPick is a reasoning effort chosen before the thread confirms it: the
// picker and composer pill show it at once, and the latest pick is sent as
// soon as the previous model change settles.
type effortPick struct {
	set          bool
	thread, base string
	level, sent  string
	at           time.Time
}

// effortPendingGrace bounds how long an unconfirmed pick is shown.
const effortPendingGrace = 4 * time.Second

// pickEffort shows level immediately and sends it, or queues it behind a
// model change still in flight, so clicks never wait on a round trip.
func (w *workbench) pickEffort(level string) {
	base, current := w.modelSelection()
	if base == "" {
		return
	}
	pick := &w.widgets.effortPending
	if !pick.set || pick.thread != w.selected || pick.base != base {
		*pick = effortPick{set: true, thread: w.selected, base: base, sent: current}
	}
	pick.level, pick.at = level, time.Now()
	w.flushEffortPick()
}

// flushEffortPick sends the latest pick once nothing else is being saved.
func (w *workbench) flushEffortPick() {
	pick := &w.widgets.effortPending
	if !pick.set || w.modelSaving || pick.sent == pick.level {
		return
	}
	pick.sent = pick.level
	w.setThinking(pick.level)
}

// displayedEffortLevel is the thread's effort, or the pick that is on its
// way; a confirmed, failed or stale pick is dropped.
func (w *workbench) displayedEffortLevel() string {
	base, level := w.modelSelection()
	pick := &w.widgets.effortPending
	if !pick.set {
		return level
	}
	if pick.thread != w.selected || pick.base != base {
		return level
	}
	w.flushEffortPick()
	stale := !w.modelSaving && time.Since(pick.at) > effortPendingGrace
	if (level == pick.level && !w.modelSaving) || w.modelErr != "" || stale {
		*pick = effortPick{}
		return level
	}
	return pick.level
}

// effortShortLabel returns a concise label for one supported thinking level.
func (w *workbench) effortShortLabel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "off":
		return w.tr3("关闭", "Off", "オフ")
	case "minimal":
		return w.tr3("极简", "Min", "最小")
	case "low":
		return w.tr3("低", "Low", "低")
	case "medium", "med":
		return w.tr3("中", "Med", "中")
	case "high":
		return w.tr3("高", "High", "高")
	case "xhigh":
		return w.tr3("超高", "XHigh", "超高")
	case "max":
		return w.tr3("最高", "Max", "最大")
	default:
		return w.thinkingLevelLabel(level)
	}
}

// effortSliderSection renders a lightweight, native segmented reasoning effort
// selector inside the model or thinking popover. It renders only the thinking
// levels actually supported by the active model and uses zero continuous
// animation frames or custom canvas rendering.
func (w *workbench) effortSliderSection(c *ui.Context, p palette, keyPrefix string, closeOnPick *bool) {
	base, _ := w.modelSelection()
	if base != "" && base != w.features.thinkingModel && !w.features.thinkingLoading && !w.features.catalogLoading && !w.features.modelFeedback.valid() {
		if _, ok := w.service.(api.WorkbenchService); ok {
			w.loadThinking()
		}
	}
	level := w.displayedEffortLevel()
	levels := w.features.thinkingLevels
	known := base != "" && base == w.features.thinkingModel && len(levels) > 0
	interactive := known && !w.features.thinkingLoading
	dotColor := effortDotColor(p, level)
	levelText := w.thinkingLevelLabel(level)

	ui.Column(c.Key(keyPrefix+"-card")).FillWidth().MinWidth(0).Padding(8, 10).Gap(6).Radius(10).
		Background(p.soft).Border(1, p.lineSoft).Children(func() {
		// Header: dot + "推理强度 · <level>" + optional "默认" reset
		ui.Row(c).FillWidth().MinWidth(0).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Box(c.Key(keyPrefix+"-dot")).Size(7, 7).Radius(3.5).Background(dotColor).Shrink(0)
			ui.Text(c, fmt.Sprintf("%s · %s", w.tr3("推理强度", "Reasoning effort", "推論強度"), levelText)).
				FontSize(12).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
			ui.Spacer(c)
			if known && level != "" {
				reset := ui.Button(c.Key(keyPrefix+"-reset"), "").Label(w.tr3("恢复默认强度", "Reset to default effort", "デフォルトに戻す")).
					Height(20).Padding(0, 6).Radius(6).Border(0, ui.Transparent).Background(ui.Transparent).TextColor(p.faint).Shrink(0)
				if reset.Hovered() {
					reset.Background(p.rowHover).TextColor(p.ink)
				}
				reset.Children(func() {
					ui.Text(c, w.tr3("默认", "Default", "既定")).FontSize(10.5).NoWrap()
				})
				if reset.Clicked() {
					w.pickEffort("")
				}
			}
		})

		if !known {
			sub := w.tr3("打开模型后可选择推理强度", "Select a model to configure reasoning effort", "モデル選択後に推論強度を設定できます")
			if w.features.thinkingLoading {
				sub = w.tr3("正在读取模型推理档位…", "Loading reasoning levels…", "推論レベルを読み込み中…")
			} else if base != "" && w.features.thinkingModel == base {
				sub = w.tr3("此模型未提供可调推理强度", "This model uses fixed reasoning effort", "このモデルは推論強度を固定しています")
			}
			ui.Text(c, sub).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…").MinWidth(0)
			return
		}

		// Native segmented buttons for the exact levels supported by this model.
		ui.Row(c.Key(keyPrefix+"-stages")).FillWidth().MinWidth(0).Padding(2).Gap(2).Radius(8).
			Background(p.canvas).Border(1, p.lineSoft).AlignItems(ui.Center).Children(func() {
			for _, lvl := range levels {
				lvl := lvl
				active := strings.EqualFold(level, lvl)
				label := w.effortShortLabel(lvl)
				accLabel := w.thinkingLevelLabel(lvl)
				btn := ui.Button(c.Key(fmt.Sprintf("%s-level-%s", keyPrefix, lvl)), "").Label(accLabel).
					Role(ui.RoleRadio).Checked(active).
					Grow(1).Basis(0).MinWidth(0).Height(26).Padding(0, 4).Radius(6).
					Border(1, ui.Transparent).Background(ui.Transparent).TextColor(p.muted)
				if active {
					btn.Background(p.card).Border(1, p.line).TextColor(p.ink)
				} else if btn.Hovered() && interactive {
					btn.Background(p.rowHover).TextColor(p.ink)
				}
				if !interactive {
					btn.Disabled(true)
				}
				btn.Children(func() {
					ui.Row(c).FillWidth().MinWidth(0).Justify(ui.Center).AlignItems(ui.Center).Children(func() {
						weight := 400
						if active {
							weight = 600
						}
						ui.Text(c, label).FontSize(11).FontWeight(weight).SingleLine().Ellipsis("…").MinWidth(0)
					})
				})
				if btn.Clicked() && interactive && slices.Contains(levels, lvl) {
					if lvl != level {
						w.pickEffort(lvl)
					}
					if closeOnPick != nil {
						*closeOnPick = false
					}
				}
			}
		})
	})
}
