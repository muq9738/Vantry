package desktop

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	vantryTerminalRequestKind = "vantry_terminal_run"
	terminalTaskTabPrefix     = "\x00task:"
)

type terminalRunPayload struct {
	Kind    string `json:"kind"`
	Version int    `json:"version"`
	Title   string `json:"title"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
}

type terminalRunResult struct {
	Status   string `json:"status"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Note     string `json:"note,omitempty"`
}

type terminalTaskState struct {
	sessions map[string]*terminalTaskSession
	order    []string
	launched map[string]bool
}

type terminalTaskSession struct {
	request  api.ProviderRequest
	payload  terminalRunPayload
	term     *terminal.Terminal
	run      *programRun
	started  time.Time
	exited   bool
	exitCode int
	errText  string
	answered bool
}

func (s *terminalTaskSession) tabKey() string {
	return terminalTaskTabPrefix + s.request.ID
}

func (s *terminalTaskSession) tabLabel() string {
	label := strings.TrimSpace(s.payload.Title)
	if label == "" {
		label = strings.TrimSpace(s.payload.Command)
	}
	if label == "" {
		return "交互终端"
	}
	return label
}

func (s *terminalTaskSession) close() error {
	if s.term != nil {
		return s.term.Close()
	}
	return nil
}

// parseTerminalRunRequest recognizes Vantry's structured interactive terminal
// request carried over Pi's editor UI request channel.
func parseTerminalRunRequest(request api.ProviderRequest) (terminalRunPayload, bool) {
	if request.Method != "editor" || strings.TrimSpace(request.Prefill) == "" {
		return terminalRunPayload{}, false
	}
	var payload terminalRunPayload
	if err := json.Unmarshal([]byte(request.Prefill), &payload); err != nil {
		return terminalRunPayload{}, false
	}
	if payload.Kind != vantryTerminalRequestKind || strings.TrimSpace(payload.Command) == "" {
		return terminalRunPayload{}, false
	}
	if strings.TrimSpace(payload.Title) == "" {
		payload.Title = strings.TrimSpace(strings.TrimPrefix(request.Title, "交互终端 · "))
	}
	return payload, true
}

func terminalTaskShellArgv(command string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/C", command}
	}
	var candidates []string
	if s := strings.TrimSpace(os.Getenv("SHELL")); s != "" {
		candidates = append(candidates, s)
	}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "/bin/zsh", "/bin/bash", "/bin/sh")
	} else {
		candidates = append(candidates, "/bin/bash", "/bin/zsh", "/bin/sh")
	}
	for _, cand := range candidates {
		if p, err := exec.LookPath(cand); err == nil && p != "" {
			return []string{p, "-lc", command}
		}
	}
	return []string{"/bin/sh", "-c", command}
}

func (w *workbench) terminalTaskSessions() []*terminalTaskSession {
	var out []*terminalTaskSession
	for _, id := range w.terminalTasks.order {
		if session := w.terminalTasks.sessions[id]; session != nil {
			out = append(out, session)
		}
	}
	return out
}

func (w *workbench) terminalTaskForTab(active string) *terminalTaskSession {
	if !strings.HasPrefix(active, terminalTaskTabPrefix) {
		return nil
	}
	id := strings.TrimPrefix(active, terminalTaskTabPrefix)
	return w.terminalTasks.sessions[id]
}

func (w *workbench) maybeAutoLaunchTerminalTask(request api.ProviderRequest) {
	if request.Status != "pending" {
		if session := w.terminalTasks.sessions[request.ID]; session != nil {
			session.request = request
			session.answered = true
		}
		return
	}
	payload, ok := parseTerminalRunRequest(request)
	if !ok {
		return
	}
	if w.terminalTasks.launched == nil {
		w.terminalTasks.launched = map[string]bool{}
	}
	if w.terminalTasks.launched[request.ID] {
		if session := w.terminalTasks.sessions[request.ID]; session != nil {
			session.request = request
		}
		return
	}
	// Only auto-open the dock when the user is viewing this thread locally;
	// background or proxied threads keep a clickable card in the timeline.
	if request.ThreadID != w.selected || w.terminalRemoteMachine() != "" {
		return
	}
	w.startTerminalTask(request, payload)
}

func (w *workbench) startTerminalTask(request api.ProviderRequest, payload terminalRunPayload) bool {
	if w.terminalTasks.sessions == nil {
		w.terminalTasks.sessions = map[string]*terminalTaskSession{}
	}
	if w.terminalTasks.launched == nil {
		w.terminalTasks.launched = map[string]bool{}
	}
	w.terminalTasks.launched[request.ID] = true
	if existing := w.terminalTasks.sessions[request.ID]; existing != nil && !existing.exited && existing.errText == "" {
		w.termTab.active = existing.tabKey()
		w.features.panel = "terminal"
		w.stage = ""
		return true
	}

	dir := strings.TrimSpace(payload.Cwd)
	if dir == "" {
		dir = w.selectedWorkspacePath()
	}
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	argv := terminalTaskShellArgv(payload.Command)
	options := terminal.Options{Dir: dir, Scrollback: 4 << 20}
	if remote := strings.TrimSpace(w.defaults.RemoteHost); remote != "" {
		options.Dir = ""
		argv = []string{"ssh", "-tt", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "--", remote, "cd -- " + quoteShell(dir) + " && " + payload.Command}
	}

	session := &terminalTaskSession{
		request: request,
		payload: payload,
		started: time.Now(),
	}
	term, run, err := w.launchTerminal(options, argv, func(code int) string {
		if code == 0 {
			return w.tr("交互命令已完成（退出码 0），正在继续任务…", "Interactive command finished (exit code 0); resuming…")
		}
		return fmt.Sprintf(w.tr("交互命令已退出（退出码 %d）", "Interactive command exited (code %d)"), code)
	})
	if err != nil {
		session.exited = true
		session.errText = describe(w.tr("启动内置终端失败", "Failed to start built-in terminal"), err)
		w.terminalTasks.sessions[request.ID] = session
		if !containsString(w.terminalTasks.order, request.ID) {
			w.terminalTasks.order = append(w.terminalTasks.order, request.ID)
		}
		w.setToolFeedback("terminal:"+request.ThreadID, notice{Text: session.errText, Level: noticeError})
		return false
	}

	session.term = term
	session.run = run
	w.terminalTasks.sessions[request.ID] = session
	if !containsString(w.terminalTasks.order, request.ID) {
		w.terminalTasks.order = append(w.terminalTasks.order, request.ID)
	}
	w.termTab.active = session.tabKey()
	w.features.panel = "terminal"
	w.stage = ""
	w.setToolFeedback("terminal:"+request.ThreadID, notice{})

	if run != nil {
		requestID := request.ID
		cwd := dir
		go func() {
			<-run.Exited()
			time.Sleep(150 * time.Millisecond)
			code := run.Code()
			w.post(func() {
				w.finishTerminalTaskOnExit(requestID, code, cwd)
			})
		}()
	}
	return true
}

func (w *workbench) finishTerminalTaskOnExit(requestID string, exitCode int, cwd string) {
	session := w.terminalTasks.sessions[requestID]
	if session == nil {
		return
	}
	session.exited = true
	session.exitCode = exitCode
	if session.answered {
		return
	}
	req, ok := w.pendingRequestByID(requestID)
	if !ok {
		session.answered = true
		return
	}
	session.answered = true
	code := exitCode
	raw, _ := json.Marshal(terminalRunResult{
		Status:   "exited",
		ExitCode: &code,
		Cwd:      cwd,
	})
	w.respondRequest(req, string(raw), false, false)
}

func (w *workbench) completeTerminalTaskManually(request api.ProviderRequest, cwd string) bool {
	if session := w.terminalTasks.sessions[request.ID]; session != nil {
		session.answered = true
	}
	raw, _ := json.Marshal(terminalRunResult{
		Status: "completed",
		Cwd:    cwd,
		Note:   "User marked interactive terminal step completed.",
	})
	return w.respondRequest(request, string(raw), false, false)
}

func (w *workbench) cancelTerminalTask(request api.ProviderRequest) bool {
	if session := w.terminalTasks.sessions[request.ID]; session != nil {
		session.answered = true
		_ = session.close()
		session.exited = true
	}
	return w.respondRequest(request, "", false, true)
}

func (w *workbench) closeTerminalTaskTab(requestID string) {
	session := w.terminalTasks.sessions[requestID]
	if session != nil {
		if !session.answered {
			if req, ok := w.pendingRequestByID(requestID); ok {
				session.answered = true
				w.respondRequest(req, "", false, true)
			}
		}
		_ = session.close()
		delete(w.terminalTasks.sessions, requestID)
	}
	for i, id := range w.terminalTasks.order {
		if id == requestID {
			w.terminalTasks.order = append(w.terminalTasks.order[:i:i], w.terminalTasks.order[i+1:]...)
			break
		}
	}
	if w.termTab.active == terminalTaskTabPrefix+requestID {
		w.termTab.active = ""
		if n := len(w.terminalTasks.order); n > 0 {
			w.termTab.active = terminalTaskTabPrefix + w.terminalTasks.order[n-1]
		} else if n := len(w.ssh.order); n > 0 {
			w.termTab.active = w.ssh.order[n-1]
		}
	}
}

func (w *workbench) closeTerminalTasks() {
	for _, session := range w.terminalTasks.sessions {
		_ = session.close()
	}
	w.terminalTasks.sessions = nil
	w.terminalTasks.order = nil
}

func (w *workbench) pendingRequestByID(id string) (api.ProviderRequest, bool) {
	for _, req := range w.requests {
		if req.ID == id && req.Status == "pending" {
			return req, true
		}
	}
	return api.ProviderRequest{}, false
}

func (w *workbench) terminalTaskDotColor(p palette, session *terminalTaskSession) ui.Color {
	if session.errText != "" || (session.exited && session.exitCode != 0) {
		return p.danger
	}
	if session.exited {
		return p.success
	}
	return p.accent
}

func (w *workbench) terminalTaskPane(c *ui.Context, p palette, session *terminalTaskSession) {
	ui.Row(c.Key("task-term-header:"+session.request.ID)).Gap(8).AlignItems(ui.Center).Shrink(0).Children(func() {
		if !session.exited && session.errText == "" {
			activityIndicator(c, p.accent).Size(10, 10).Shrink(0)
		} else {
			ui.Box(c).Size(7, 7).Radius(4).Background(w.terminalTaskDotColor(p, session)).Shrink(0)
		}
		where := session.tabLabel()
		if dir := strings.TrimSpace(session.payload.Cwd); dir != "" {
			where += " · " + filepath.Base(dir)
		}
		if session.exited {
			if session.errText != "" {
				where += w.tr(" · 启动失败", " · start failed")
			} else {
				where += fmt.Sprintf(w.tr(" · 已退出 (%d)", " · exited (%d)"), session.exitCode)
			}
		} else {
			where += w.tr(" · 交互运行中", " · running")
		}
		ui.Text(c, where).FontSize(11).TextColor(p.muted).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		if req, pending := w.pendingRequestByID(session.request.ID); pending && !session.answered {
			if quietButton(c, p, "task-term-done", w.tr("完成并继续", "Done & resume")).Height(24).Clicked() {
				w.completeTerminalTaskManually(req, session.payload.Cwd)
			}
			if quietButton(c, p, "task-term-cancel", w.tr("停止并取消", "Stop & cancel")).Height(24).Clicked() {
				w.cancelTerminalTask(req)
			}
		} else {
			if quietButton(c, p, "task-term-close", w.tr("关闭此终端", "Close terminal")).Height(24).Clicked() {
				w.closeTerminalTaskTab(session.request.ID)
				w.widgets.composerFocus.Focus()
			}
		}
	})
	if session.errText != "" || session.term == nil {
		ui.Column(c).Grow(1).MinHeight(0).Padding(12).Radius(8).Background(p.danger.Alpha(0.08)).Children(func() {
			ui.Text(c, session.errText).FontSize(12).TextColor(p.danger).Wrap().Selectable()
		})
		return
	}
	ui.Column(c.Key("task-term-body:"+session.request.ID)).Grow(1).MinHeight(0).Radius(8).Clip().Border(1, p.lineSoft).Children(func() {
		terminal.View(c, session.term).Grow(1).FillWidth().Label(w.tr("交互终端输入 ", "Interactive terminal ") + session.tabLabel())
	})
}

func (w *workbench) terminalTaskRequestCard(c *ui.Context, p palette, request api.ProviderRequest, payload terminalRunPayload, busy bool) {
	session := w.terminalTasks.sessions[request.ID]
	running := session != nil && !session.exited && session.errText == ""
	tone := p.accent
	if request.Status == "failed" || (session != nil && session.errText != "") {
		tone = p.danger
	}
	cardBg := tone.Mix(p.raised, 0.94)
	if p.dark {
		cardBg = tone.Mix(p.raised, 0.90)
	}
	title := payload.Title
	if strings.TrimSpace(title) == "" {
		title = w.tr("交互式终端命令", "Interactive terminal command")
	}
	card := ui.Column(c.Key("request:"+request.ID)).Gap(10).Padding(12, 14, 12).Background(cardBg).Border(1, tone.Alpha(0.45)).Radius(12).Role(ui.RoleGroup).Label(title)
	if !p.dark {
		card.Shadow(0, 3, 12, 0, tone.Alpha(0.08))
	}
	card.Children(func() {
		ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
			if running {
				activityIndicator(c, tone).Size(12, 12).Shrink(0)
			} else {
				ui.Icon(c, terminalIcon).FontSize(14).TextColor(tone)
			}
			state := w.tr("内置终端交互任务", "Built-in terminal task")
			if running {
				state = w.tr("正在内置终端运行，退出后自动继续…", "Running in built-in Terminal; resumes on exit…")
			} else if request.Status != "pending" {
				state = requestStatusLabel(request.Status)
			}
			ui.Text(c, state).FontSize(12).FontWeight(600).TextColor(tone).NoWrap()
		})
		ui.Text(c, title).FontSize(14).FontWeight(500).TextColor(p.ink).Wrap()
		ui.Column(c).FillWidth().Padding(8, 11).Radius(8).Background(p.code).Border(1, p.lineSoft).Gap(4).Children(func() {
			ui.Text(c, payload.Command).Font("Menlo").FontSize(12).LineHeight(1.5).TextColor(p.ink.Mix(p.code, 0.1)).Wrap().Selectable()
			if dir := strings.TrimSpace(payload.Cwd); dir != "" {
				ui.Text(c, w.tr("工作目录：", "Working dir: ")+dir).Font("Menlo").FontSize(10.5).TextColor(p.muted).SingleLine().Ellipsis("…")
			}
		})
		if session != nil && session.errText != "" {
			ui.Text(c, session.errText).FontSize(11).TextColor(p.danger).Wrap()
		}
		if request.Error != "" {
			ui.Text(c, request.Error).FontSize(11).TextColor(p.danger).Wrap()
		}
		if request.Status != "pending" {
			return
		}
		ui.Row(c).FillWidth().Gap(8).Wrap().Justify(ui.End).AlignItems(ui.Center).Children(func() {
			cancelBtn := ui.Button(c.Key("cancel"), w.tr("取消", "Cancel")).FontSize(12.5).Height(30).Padding(0, 14).Radius(8).
				Background(ui.Transparent).Border(1, p.line).TextColor(p.ink).Disabled(busy)
			if cancelBtn.Hovered() {
				cancelBtn.Background(p.soft)
			}
			if cancelBtn.Clicked() {
				w.cancelTerminalTask(request)
			}
			if running {
				doneBtn := ui.Button(c.Key("done"), w.tr("完成并继续", "Done & resume")).FontSize(12.5).Height(30).Padding(0, 12).Radius(8).
					Background(p.soft).Border(1, p.line).TextColor(p.ink).Disabled(busy)
				if doneBtn.Clicked() {
					w.completeTerminalTaskManually(request, payload.Cwd)
				}
				focusBtn := ui.PrimaryButton(c.Key("focus-terminal"), w.tr("切换到内置终端", "Focus Terminal")).FontSize(12.5).Height(30).Padding(0, 14).Radius(8).Disabled(busy)
				if focusBtn.Clicked() {
					w.termTab.active = session.tabKey()
					w.features.panel = "terminal"
					w.stage = ""
				}
			} else {
				runBtn := ui.PrimaryButton(c.Key("run-terminal"), w.tr("在内置终端运行", "Run in Terminal")).FontSize(12.5).Height(30).Padding(0, 16).Radius(8).Disabled(busy)
				if runBtn.Clicked() {
					w.startTerminalTask(request, payload)
				}
			}
		})
	})
}
