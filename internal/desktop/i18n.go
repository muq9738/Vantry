package desktop

import (
	"fmt"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// Interface languages. Simplified Chinese stays the default so an empty or
// unknown persisted value keeps the long-standing behavior.
const (
	localeZH = "zh"
	localeEN = "en"
	localeJA = "ja"
)

type localeOption struct {
	code  string
	label string
}

var supportedLocales = []localeOption{
	{localeZH, "简体中文"},
	{localeEN, "English"},
	{localeJA, "日本語"},
}

func normalizeLocale(locale string) string {
	switch locale {
	case localeEN:
		return localeEN
	case localeJA:
		return localeJA
	default:
		return localeZH
	}
}

func localeDisplayLabel(locale string) string {
	code := normalizeLocale(locale)
	for _, opt := range supportedLocales {
		if opt.code == code {
			return opt.label
		}
	}
	return supportedLocales[0].label
}

func localeCodeFromLabel(label string) string {
	for _, opt := range supportedLocales {
		if opt.label == label {
			return opt.code
		}
	}
	return localeZH
}

func (w *workbench) english() bool  { return w.widgets.locale == localeEN }
func (w *workbench) japanese() bool { return w.widgets.locale == localeJA }

// tr3 picks the visible string for Chinese, English, or Japanese explicitly.
func (w *workbench) tr3(zh, en, ja string) string {
	switch w.widgets.locale {
	case localeEN:
		return en
	case localeJA:
		return ja
	default:
		return zh
	}
}

// tr picks the visible string for the current interface language.
func (w *workbench) tr(zh, en string) string {
	switch w.widgets.locale {
	case localeEN:
		return en
	case localeJA:
		return translateJA(zh, en)
	default:
		return zh
	}
}

// setLocale switches the language live. Cached sidebar groups carry section
// titles, so they are rebuilt on the next frame.
func (w *workbench) setLocale(locale string) {
	locale = normalizeLocale(locale)
	if w.widgets.locale == locale || w.widgets.locale == "" && locale == localeZH {
		w.widgets.locale = locale
		return
	}
	w.widgets.locale = locale
	w.navigation.valid = false
	w.saveState()
}

// turns is the English count of conversation turns.
func (w *workbench) turns(n int) string {
	if w.japanese() {
		return fmt.Sprintf("%d ターン", n)
	}
	if n == 1 {
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", n)
}

// formatAge is a row's age, short as the workbench's: 刚刚 / now / たった今, 5m, 3h, 2d, then the date.
func (w *workbench) formatAge(now, at time.Time) string {
	d := now.Sub(at)
	switch {
	case d < time.Minute:
		return w.tr3("刚刚", "now", "たった今")
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case at.Year() == now.Year():
		if w.english() {
			return at.Format("Jan 2")
		}
		return fmt.Sprintf("%d月%d日", at.Month(), at.Day())
	}
	return at.Format("2006/1/2")
}

func formatAge(now, at time.Time) string { return (&workbench{}).formatAge(now, at) }

func (w *workbench) statusText(status api.RunStatus) string {
	if w.japanese() {
		switch status {
		case api.RunQueued:
			return "待機中"
		case api.RunRunning:
			return "実行中"
		case api.RunWaiting:
			return "サブタスク待ち"
		case api.RunCompleted:
			return "完了"
		case api.RunCancelled:
			return "停止済み"
		case api.RunInterrupted:
			return "中断"
		case api.RunFailed:
			return "失敗"
		case "":
			return "不明"
		}
		return "状態 " + string(status)
	}
	if !w.english() {
		return statusLabel(status)
	}
	switch status {
	case api.RunQueued:
		return "Queued"
	case api.RunRunning:
		return "Running"
	case api.RunWaiting:
		return "Waiting on subtasks"
	case api.RunCompleted:
		return "Completed"
	case api.RunCancelled:
		return "Stopped"
	case api.RunInterrupted:
		return "Interrupted"
	case api.RunFailed:
		return "Failed"
	case "":
		return "Unknown"
	}
	return "Status " + string(status)
}

// approvalShort is the permission chip's word for a tier.
func (w *workbench) approvalShort(policy string) string {
	switch policy {
	case "ask":
		return w.tr3("询问", "Ask", "確認")
	case "auto-accept-edits":
		return w.tr3("改文件自动", "Auto edits", "編集自動")
	}
	return w.tr3("自动", "Auto", "全自動")
}

func (w *workbench) approvalLabel(tier approvalTier) string {
	if w.japanese() {
		switch tier.policy {
		case "ask":
			return "毎回確認する"
		case "auto-accept-edits":
			return "ファイル編集は自動・コマンドは確認"
		}
		return "フルオート"
	}
	if !w.english() {
		return tier.label
	}
	switch tier.policy {
	case "ask":
		return "Always ask"
	case "auto-accept-edits":
		return "Auto edits · ask for commands"
	}
	return "Full auto"
}

func (w *workbench) approvalDetail(tier approvalTier) string {
	if w.japanese() {
		switch tier.policy {
		case "ask":
			return "コマンド実行やファイル編集の前に確認します。"
		case "auto-accept-edits":
			return "ファイル編集は自動適用し、コマンド実行前に確認します。"
		}
		return "コマンド実行とファイル編集を直接行います。信頼できるプロジェクト向け。"
	}
	if !w.english() {
		return tier.detail
	}
	switch tier.policy {
	case "ask":
		return "Ask before running commands or editing files."
	case "auto-accept-edits":
		return "Apply file edits automatically; ask before commands."
	}
	return "Run commands and edit files directly. For trusted projects."
}
