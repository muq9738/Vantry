package desktop

import (
	"strings"

	"github.com/egoist/mygo/ui"
)

// planModePrefix asks the agent for a plan only. It is plain message text, so
// every provider honors it today and the user sees exactly what was sent.
const planModePrefix = "【规划模式】请先只给出实施计划（目标、步骤、涉及文件与风险），不要修改文件或执行命令，等我确认后再动手。\n\n"

func applyInteractionMode(plan bool, text string) string {
	if !plan || strings.HasPrefix(text, planModePrefix) {
		return text
	}
	return planModePrefix + text
}

// approvalTier is one of the three tool-permission levels a new session can
// start with, from the most autonomous to the most supervised.
type approvalTier struct {
	policy, label, short, detail string
}

var approvalTiers = []approvalTier{
	{"auto", "自动执行", "自动", "直接运行命令与修改文件，适合可信项目。"},
	{"auto-accept-edits", "自动改文件 · 命令询问", "改文件自动", "文件修改自动应用；运行命令前询问你。"},
	{"ask", "每次询问", "每次询问", "运行命令、修改文件前都会询问你。"},
}

func approvalTierFor(policy string) approvalTier {
	for _, tier := range approvalTiers {
		if tier.policy == policy {
			return tier
		}
	}
	return approvalTiers[0]
}

func approvalLabels() []string {
	labels := make([]string, len(approvalTiers))
	for i, tier := range approvalTiers {
		labels[i] = tier.label
	}
	return labels
}

func approvalPolicyForLabel(label string) string {
	for _, tier := range approvalTiers {
		if tier.label == label {
			return tier.policy
		}
	}
	return "auto"
}

// modeCapsule is the composer's 对话 / 规划 switch: two small segments in one
// pill, the chosen one raised. It changes only how the next send is framed.
func (w *workbench) modeCapsule(c *ui.Context, p palette, comp *composer, compact bool) {
	ui.Row(c.Key("mode-capsule")).Height(26).Padding(2).Gap(2).Radius(13).Background(p.code).Border(1, p.lineSoft).AlignItems(ui.Center).Shrink(0).
		Role(ui.RoleRadioGroup).Label("交互模式").Children(func() {
		for _, mode := range []struct {
			plan        bool
			label, text string
			icon        *ui.SVG
		}{{false, "对话模式", "对话", chatIcon}, {true, "规划模式", "规划", planIcon}} {
			mode := mode
			chosen := comp.plan == mode.plan
			segment := ui.Button(c.Key("mode:"+mode.text), "").Label(mode.label).Role(ui.RoleRadio).AccessSelected(chosen).
				Height(22).Padding(0, 8).Radius(11).Border(0, p.line).Background(ui.Transparent).TextColor(p.muted)
			styleSegment(segment, p, chosen)
			if chosen && mode.plan {
				segment.TextColor(p.accentInk)
			}
			if mode.plan {
				segment.Tooltip("规划模式：先给出计划，不修改文件或执行命令")
			} else {
				segment.Tooltip("对话模式：直接执行")
			}
			segment.Children(func() {
				ui.Icon(c, mode.icon).FontSize(12)
				if !compact {
					ui.Text(c, mode.text).FontSize(11)
				}
			})
			if segment.Clicked() {
				comp.plan = mode.plan
				w.widgets.composerFocus.Focus()
			}
		}
	})
}
