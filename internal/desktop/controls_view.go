package desktop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"github.com/muq9738/Vantry/pkg/api"
)

func (w *workbench) inputsView(c *ui.Context, p palette) {
	var inputs []api.Input
	for _, input := range w.inputs {
		if input.ThreadID == w.selected && (input.Status == "queued" || input.Status == "pending") {
			inputs = append(inputs, input)
		}
	}
	if len(inputs) == 0 {
		return
	}
	ui.Column(c.Key("pending-inputs")).FillWidth().Gap(5).Padding(0, 4, 7).Children(func() {
		label := fmt.Sprintf(w.tr("%d 条待发送消息", "%d queued messages"), len(inputs))
		trigger := quietButton(c, p, "queue-summary", label).Label(w.tr("查看队列", "View queue")).Height(22).FontSize(11).Padding(0, 2).AlignSelf(ui.End)
		trigger.Description(w.tr("排队消息直接显示在输入框上方", "Queued messages are shown above the composer"))
		if trigger.Clicked() {
			w.widgets.queueOpen = !w.widgets.queueOpen
		}
		_, height := c.Size()
		limit := min(float32(228), height*0.28)
		if w.widgets.queueOpen {
			limit = height * 0.42
		}
		ui.Scroll(c.Key("inline-queue")).FillWidth().MaxHeight(limit).MinHeight(0).Shrink(0).Children(func() { w.inputRows(c, p, inputs) })
	})
}

func (w *workbench) inputRows(c *ui.Context, p palette, inputs []api.Input) {
	ui.Column(c).FillWidth().AlignItems(ui.End).Gap(6).Children(func() {
		for i, input := range inputs {
			w.queueCard(c, p, input, i, len(inputs))
		}
	})
}

func (w *workbench) steerHistoryView(c *ui.Context, p palette, runID string) {
	for _, input := range w.inputs {
		if input.RunID != runID || input.Kind != "steer" || input.Status != "delivered" && input.Status != "failed" && input.Status != "unknown" {
			continue
		}
		input := input
		isErr := input.Status == "failed" || input.Status == "unknown"
		ui.Column(c.Key("steer-history:"+input.ID)).FillWidth().Gap(3).Padding(4, 0, 2).AlignItems(ui.End).Children(func() {
			bubble := ui.Column(c).MaxWidth(min(float32(540), readingWidth*0.78)).Padding(8, 13).Gap(4).Radius(16).Background(p.user).Border(1, p.lineSoft)
			if isErr {
				bubble.BorderColor(p.danger.Alpha(0.4))
			}
			bubble.Children(func() {
				ui.Text(c, strings.TrimSpace(input.Message)).FontSize(13.5).LineHeight(1.45).TextColor(p.ink).Wrap().Selectable()
				for _, attachment := range input.Attachments {
					ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, fileIcon).FontSize(11).TextColor(p.faint)
						ui.Text(c, attachment.Name).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…")
					})
				}
			})
			metaColor := p.faint
			if isErr {
				metaColor = p.danger
			}
			ui.Row(c).Gap(5).Padding(0, 4).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, steerIcon).FontSize(10.5).TextColor(metaColor)
				ui.Text(c, inputStatusLabel(input)).FontSize(11).TextColor(metaColor).NoWrap()
				if isErr && input.Error != "" {
					ui.Text(c, "· "+input.Error).FontSize(11).TextColor(p.danger).SingleLine().Ellipsis("…").MaxWidth(360)
				}
			})
		})
	}
}

func toolSummary(tool api.ToolCall) string {
	var args map[string]any
	if json.Unmarshal(tool.Arguments, &args) == nil {
		for _, key := range []string{"command", "path", "file_path"} {
			if text, ok := args[key].(string); ok && text != "" {
				return strings.Join(strings.Fields(text), " ")
			}
		}
	}
	return ""
}

func displayJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "（无参数）"
	}
	var out bytes.Buffer
	if json.Indent(&out, raw, "", "  ") == nil {
		return out.String()
	}
	return string(raw)
}

func requestMessage(request api.ProviderRequest) string {
	var args map[string]any
	if json.Unmarshal([]byte(request.Message), &args) == nil {
		if command, ok := args["command"].(string); ok {
			return command
		}
	}
	return request.Message
}

func splitApprovalTitle(request api.ProviderRequest) (headline, body string) {
	if request.Message != "" {
		return request.Title, requestMessage(request)
	}
	if first, rest, ok := strings.Cut(request.Title, "\n"); ok {
		fake := api.ProviderRequest{Message: strings.TrimSpace(rest)}
		return strings.TrimSpace(first), requestMessage(fake)
	}
	return request.Title, ""
}

func parseApprovalSelect(request api.ProviderRequest) (allowOnce, allowSession, deny string, ok bool) {
	if request.Method != "select" || len(request.Options) < 2 {
		return "", "", "", false
	}
	for _, opt := range request.Options {
		switch {
		case opt == "允许本次":
			allowOnce = opt
		case strings.HasPrefix(opt, "本会话允许 "):
			allowSession = opt
		case opt == "拒绝":
			deny = opt
		}
	}
	ok = allowOnce != "" && deny != ""
	return allowOnce, allowSession, deny, ok
}

func (w *workbench) requestListView(c *ui.Context, p palette, runID string) {
	if w.widgets.requestValues == nil {
		w.widgets.requestValues = map[string]string{}
	}
	for _, request := range w.requests {
		if request.RunID != runID || request.Status != "pending" && request.Status != "responding" && request.Status != "failed" {
			continue
		}
		request := request
		if termPayload, isTermRun := parseTerminalRunRequest(request); isTermRun {
			w.terminalTaskRequestCard(c, p, request, termPayload, w.controlBusy("request:"+request.ID))
			continue
		}
		// The approval card is the turn's live state while it waits: a warm
		// amber surface and border, a clear headline, the command in a code
		// well, and Deny / Allow once on the right.
		tone := p.warning
		if request.Status == "failed" {
			tone = p.danger
		}
		cardBg := tone.Mix(p.raised, 0.94)
		if p.dark {
			cardBg = tone.Mix(p.raised, 0.90)
		}
		headline, body := splitApprovalTitle(request)
		card := ui.Column(c.Key("request:"+request.ID)).Gap(10).Padding(12, 14, 12).Background(cardBg).Border(1, tone.Alpha(0.48)).Radius(12).Role(ui.RoleGroup).Label(headline)
		if !p.dark {
			card.Shadow(0, 3, 12, 0, tone.Alpha(0.10))
		}
		card.Children(func() {
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, shieldIcon).FontSize(14).TextColor(tone)
				state := w.tr("需要你的确认", "Needs your confirmation")
				if request.Status != "pending" {
					state = requestStatusLabel(request.Status)
				}
				ui.Text(c, state).FontSize(12).FontWeight(600).TextColor(tone).NoWrap()
			})
			ui.Text(c, headline).FontSize(14).FontWeight(500).TextColor(p.ink).Wrap()
			if body != "" {
				ui.Column(c).FillWidth().Padding(8, 11).Radius(8).Background(p.code).Border(1, p.lineSoft).Children(func() {
					ui.Text(c, body).Font("Menlo").FontSize(12).LineHeight(1.5).TextColor(p.ink.Mix(p.code, 0.1)).Wrap().Selectable()
				})
			}
			if request.Error != "" {
				ui.Text(c, request.Error).FontSize(11).TextColor(p.danger).Wrap()
			}
			if request.Status != "pending" {
				return
			}
			key := "request:" + request.ID
			busy := w.controlBusy(key)
			if allowOnce, allowSession, deny, isApprovalSel := parseApprovalSelect(request); isApprovalSel {
				ui.Row(c).FillWidth().Gap(8).Wrap().Justify(ui.End).AlignItems(ui.Center).Children(func() {
					denyBtn := ui.Button(c.Key("cancel"), deny).FontSize(12.5).Height(30).Padding(0, 14).Radius(8).
						Background(ui.Transparent).Border(1, p.line).TextColor(p.ink).Disabled(busy)
					if denyBtn.Hovered() {
						denyBtn.Background(p.soft)
					}
					if denyBtn.Clicked() {
						w.respondRequest(request, deny, false, false)
					}
					if allowSession != "" {
						sessionBtn := ui.Button(c.Key("allow-session"), allowSession).FontSize(12.5).Height(30).Padding(0, 12).Radius(8).
							Background(p.soft).Border(1, p.line).TextColor(p.ink).Disabled(busy).
							Tooltip(w.tr("在本会话后续同类调用中自动放行", "Auto-allow matching calls for the rest of this thread"))
						if sessionBtn.Clicked() {
							w.respondRequest(request, allowSession, false, false)
						}
					}
					allowBtn := ui.PrimaryButton(c.Key("confirm"), allowOnce).FontSize(12.5).Height(30).Padding(0, 16).Radius(8).Disabled(busy)
					if allowBtn.Clicked() {
						w.respondRequest(request, allowOnce, false, false)
					}
				})
				if w.controlRetry(key) {
					ui.Text(c, w.tr("响应未确认，重试将沿用原来的回答。", "Response unconfirmed; a retry reuses the same answer.")).FontSize(11).TextColor(p.warning)
				}
				return
			}
			value, initialized := w.widgets.requestValues[request.ID]
			if !initialized {
				value = request.Prefill
				if request.Method == "select" && value == "" && len(request.Options) > 0 {
					value = request.Options[0]
				}
			}
			switch request.Method {
			case "select":
				options := append([]string(nil), request.Options...)
				if !containsString(options, value) {
					value = ""
					if len(options) > 0 {
						value = options[0]
					}
				}
				ui.Select(c.Key("value"), &value, options).Label(w.tr("审批选项", "Approval option")).FillWidth().Disabled(busy)
			case "input":
				ui.TextInput(c.Key("value"), &value).Label(w.tr("审批输入", "Approval input")).Placeholder(w.tr("输入回复", "Type a reply")).FillWidth().Disabled(busy)
			case "editor":
				ui.TextAreaBase(c.Key("value"), &value).Lines(3, 8).Label(w.tr("审批内容", "Approval content")).FillWidth().Disabled(busy)
			}
			w.widgets.requestValues[request.ID] = value
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Spacer(c)
				cancelLabel := w.tr("取消请求", "Cancel request")
				if request.Method == "confirm" {
					cancelLabel = w.tr("拒绝", "Deny")
				}
				cancel := ui.Button(c.Key("cancel"), cancelLabel).FontSize(12.5).Height(30).Padding(0, 14).Radius(8).
					Background(ui.Transparent).Border(1, p.line).TextColor(p.ink).Disabled(busy)
				if cancel.Hovered() {
					cancel.Background(p.soft)
				}
				if cancel.Clicked() {
					w.respondRequest(request, "", false, request.Method != "confirm")
				}
				label := w.tr("提交响应", "Submit")
				visibleLabel := label
				if request.Method == "confirm" {
					label = w.tr("允许执行", "Allow")
					visibleLabel = w.tr3("仅允许本次", "Allow once", "今回のみ許可")
				}
				submit := ui.PrimaryButton(c.Key("confirm"), visibleLabel).Label(label).FontSize(12.5).Height(30).Padding(0, 16).Radius(8).Disabled(busy)
				if submit.Clicked() {
					if request.Method == "confirm" {
						value = ""
					}
					w.respondRequest(request, value, request.Method == "confirm", false)
				}
			})
			if w.controlRetry(key) {
				ui.Text(c, w.tr("响应未确认，重试将沿用原来的回答。", "Response unconfirmed; a retry reuses the same answer.")).FontSize(11).TextColor(p.warning)
			}
		})
	}
}

func (w *workbench) workspaceView(c *ui.Context, p palette) {
	if !w.widgets.workspaceOpen {
		return
	}
	_, height := c.Size()
	card := ui.Column(c.Key("workspace-panel")).Width(308).Shrink(0).AlignSelf(ui.Stretch).Margin(10, 12, 12, 0).Padding(10).Gap(8).MaxHeight(height-68).
		Radius(14).Background(p.sidebar).Border(1, p.lineSoft)
	if p.dark {
		card.Shadow(0, 8, 24, 0, ui.Hex("#00000048"))
	} else {
		card.Shadow(0, 4, 18, 0, ui.Hex("#1D1D1F10"))
	}
	card.Children(func() { w.workspaceContents(c, p) })
}

func (w *workbench) activeRightPanelTab() string {
	if w.widgets.rightPanelTab == "subtasks" {
		return "subtasks"
	}
	return "project"
}

func (w *workbench) workspaceContents(c *ui.Context, p palette) {
	rootID := w.selected
	if th, ok := w.threadByID(w.selected); ok && th.ParentThreadID != "" {
		rootID = th.ParentThreadID
	}
	subtasks := w.subagentNavItems(rootID)
	activeTab := w.activeRightPanelTab()

	// Right panel header: segmented tabs (项目 / 子任务 N) + close.
	ui.Row(c.Key("right-panel-tabs-row")).FillWidth().Gap(6).AlignItems(ui.Center).Shrink(0).Children(func() {
		subLabel := w.tr3("子任务", "Subtasks", "サブタスク")
		if len(subtasks) > 0 {
			subLabel = fmt.Sprintf("%s %d", subLabel, len(subtasks))
		}
		projLabel := w.tr3("项目", "Project", "プロジェクト")

		ui.Row(c.Key("right-panel-tabs")).Grow(1).MinWidth(0).Height(30).Padding(2).Gap(2).Radius(10).
			Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center).
			Role(ui.RoleTabList).Label(w.tr3("右侧面板标签", "Inspector tabs", "インスペクタタブ")).Children(func() {
			tabs := []struct{ id, label string }{
				{"project", projLabel},
				{"subtasks", subLabel},
			}
			for _, tab := range tabs {
				tab := tab
				chosen := activeTab == tab.id
				btn := ui.Button(c.Key("rp-tab:"+tab.id), "").Label(tab.label).Role(ui.RoleTab).Selected(chosen).
					Grow(1).MinWidth(0).Height(26).Padding(0, 6).Radius(8).Border(0, p.line)
				styleSegment(btn, p, chosen)
				btn.Children(func() {
					weight := 400
					if chosen {
						weight = 600
					}
					ui.Text(c, tab.label).FontSize(11.5).FontWeight(weight).SingleLine().Ellipsis("…")
				})
				if btn.Clicked() {
					w.widgets.rightPanelTab = tab.id
				}
			}
		})
		if iconButton(c, p, "close-workspace", w.tr3("收起工作区", "Close workspace", "ワークスペースを閉じる"), closeIcon).Size(24, 24).Padding(4).Clicked() {
			w.widgets.workspaceOpen = false
		}
	})

	if activeTab == "subtasks" {
		w.rightPanelSubtasksTab(c, p, subtasks)
		return
	}
	ui.Scroll(c.Key("workspace-scroll")).FillWidth().Grow(1).Shrink(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Gap(2).Children(func() {
			w.threadPanelRows(c, p, true)
		})
	})
}

func (w *workbench) rightPanelSubtasksTab(c *ui.Context, p palette, items []subagentNavItem) {
	if len(items) == 0 {
		ui.Column(c.Key("rp-sub-empty")).Grow(1).FillWidth().Justify(ui.Center).AlignItems(ui.Center).Gap(8).Padding(20, 12).Children(func() {
			ui.Icon(c, agentsIcon).FontSize(22).TextColor(p.faint)
			ui.Text(c, w.tr3("当前暂无子任务", "No subtasks yet", "サブタスクはまだありません")).FontSize(12.5).FontWeight(600).TextColor(p.ink)
			ui.Text(c, w.tr3("派出的子代理及其执行进度会显示在这里。", "Delegated subagents and their live progress appear here.", "委譲されたサブエージェントと進捗がここに表示されます。")).
				FontSize(11.5).TextColor(p.muted).TextAlign(ui.Center).Wrap()
		})
		return
	}
	ui.Scroll(c.Key("rp-sub-list")).FillWidth().Grow(1).Shrink(1).MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().Gap(6).Padding(2, 0).Children(func() {
			for _, item := range items {
				item := item
				tone := taskStatusTone(p, item.status)
				title := stripInlineMarkdownTitle(item.title)
				openHere := w.subagentPanelShows(item.threadID)
				row := ui.Button(c.Key("rp-sub:"+item.threadID), "").Label(title).FillWidth().Padding(8, 10).Radius(10).
					Background(p.raised).Border(1, p.lineSoft).Justify(ui.Start)
				if openHere {
					row.BorderColor(p.accent.Alpha(0.45))
				} else if row.Hovered() {
					row.BorderColor(p.line)
				}
				row.Children(func() {
					ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Box(c).Size(22, 22).Shrink(0).Children(func() {
							agentAvatar(c, p, title, 22)
							ui.Box(c).Absolute().Right(-2).Bottom(-2).Size(8, 8).Radius(4).Background(tone).Border(1.5, p.raised)
						})
						ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
							ui.Text(c, title).FontSize(12).FontWeight(600).TextColor(p.ink).SingleLine().Ellipsis("…")
							state := w.tr(statusLabel(item.status), englishStatus(item.status))
							if elapsed, ok := w.subagentElapsed(item.threadID, item.status); ok {
								state += " · " + spanDuration(elapsed)
							}
							ui.Text(c, state).FontSize(11).TextColor(tone).SingleLine().Ellipsis("…")
						})
						ui.Icon(c, rightIcon).FontSize(11).TextColor(p.faint).Shrink(0)
					})
				})
				if row.Clicked() {
					w.openSubagentPanel(item.threadID)
				}
			}
		})
	})
}

func (w *workbench) diffDialog(c *ui.Context, p palette) {
	if !w.widgets.diffOpen {
		return
	}
	width, height := c.Size()
	ui.DialogBase(c, &w.widgets.diffOpen, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Hex("#00000066"))
		panel.Width(min(float32(960), width-36)).Height(min(float32(680), height-60)).Radius(14).Background(p.surface).Border(1, p.line)
		ui.Column(c).Fill().Children(func() {
			ui.Row(c).Height(48).Padding(0, 16).Gap(8).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(p.line).Children(func() {
				ui.Text(c, w.tr("文件变更", "Changes")).FontSize(14).Bold().Grow(1)
				ui.Segmented(c, &w.widgets.diffLayout, w.tr("并排", "Split"), w.tr("统一", "Unified")).Label(w.tr("差异布局", "Diff layout"))
				if iconButton(c, p, "close-diff", w.tr("关闭变更", "Close changes"), closeIcon).Clicked() {
					w.widgets.diffOpen = false
				}
			})
			text := ""
			if w.workspaceChanges != nil {
				text = w.workspaceChanges.Diff
			}
			if files := w.patchFiles(text); len(files) > 0 {
				ui.Scroll(c.Key("diff-files")).Grow(1).Children(func() {
					ui.Column(c).Padding(12, 16).Gap(14).Children(func() {
						for i, f := range files {
							ui.Column(c.Key("patch:" + strconv.Itoa(i))).Gap(6).Children(func() {
								ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
									ui.Icon(c, fileIcon).FontSize(12).TextColor(p.muted)
									ui.Text(c, f.name).Font("Menlo").FontSize(12).Bold().Grow(1).Selectable()
									diffCounts(c, p, f.added, f.removed)
								})
								if f.state == nil {
									ui.Text(c, w.tr("二进制文件，无文本差异", "Binary file, no text diff")).FontSize(11).TextColor(p.muted)
									return
								}
								ui.Column(c).FillWidth().Radius(8).Border(1, p.line).Children(func() {
									f.state.Layout = editor.DiffLayout(w.widgets.diffLayout)
									editor.DiffView(c, f.state).FillWidth()
								})
							})
						}
					})
				})
				return
			}
			lines := strings.Split(text, "\n")
			ui.List(c.Key("diff-lines"), &w.widgets.diffState, len(lines), func(i int) {
				color := p.ink
				if strings.HasPrefix(lines[i], "+") {
					color = p.success
				} else if strings.HasPrefix(lines[i], "-") {
					color = p.danger
				} else if strings.HasPrefix(lines[i], "@@") {
					color = p.accentInk
				}
				ui.Row(c).Gap(14).Padding(1, 16).Children(func() {
					ui.Text(c, fmt.Sprint(i+1)).Font("Menlo").FontSize(11).TextColor(p.muted).Width(32)
					ui.Text(c, lines[i]).Font("Menlo").FontSize(12).TextColor(color).Wrap().Selectable().Grow(1)
				})
			}).Grow(1).Padding(12, 0)
		})
	})
}

type patchFile struct {
	name           string
	added, removed int
	fp             editor.FilePatch
	state          *editor.DiffState
}

// patchFiles parses a git diff into one viewer per file, once per diff text.
func (w *workbench) patchFiles(text string) []patchFile {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if w.patchCache.text == text {
		return w.patchCache.files
	}
	parsed, err := editor.ParsePatch(text)
	var files []patchFile
	if err == nil {
		for _, fp := range parsed {
			f := patchFile{name: nonEmptyString(fp.NewName, fp.OldName), fp: fp}
			for _, h := range fp.Hunks {
				for _, line := range h.Lines {
					switch {
					case strings.HasPrefix(line, "+"):
						f.added++
					case strings.HasPrefix(line, "-"):
						f.removed++
					}
				}
			}
			if !fp.Binary && len(fp.Hunks) > 0 {
				state := &editor.DiffState{Layout: editor.DiffUnified, FitContent: true, Wrap: true}
				state.SetPatch(fp)
				if lang := editor.LanguageFor(f.name); lang != nil {
					state.Old.SetLanguage(lang)
					state.New.SetLanguage(lang)
				}
				f.state = state
			}
			files = append(files, f)
		}
	}
	w.patchCache.text, w.patchCache.files = text, files
	return files
}
