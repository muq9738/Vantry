package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/adapters/transport/sshrelay"
	"github.com/muq9738/Vantry/pkg/api"
)

type workbenchTools struct {
	busy            map[string]bool
	catalog         *api.ModelCatalog
	catalogPath     string
	catalogRequest  uint64
	catalogLoading  bool
	catalogAuto     string // workspace whose details were fetched in the background
	thinkingRequest uint64
	thinkingLoading bool
	thinkingModel   string
	thinkingLevels  []string
	modelFeedback   notice
	panel           string
	git             map[string]*api.GitState
	gitBranches     map[string]string
	gitOutputs      map[string]string
	feedback        map[string]notice
	terminals       map[string]*terminal.Terminal
	plugins         []api.PluginInfo
	pluginOutputs   map[string]string
	rename          string
	showRename      bool
	projectOpen     map[string]*bool
	remoteHost      string
	remoteSocket    string
}

func (w *workbench) workbenchCall(key string, request api.WorkbenchRequest, done func(api.WorkbenchResponse, error)) {
	if w.features.busy == nil {
		w.features.busy = map[string]bool{}
	}
	if w.features.busy[key] {
		return
	}
	service, ok := w.service.(api.WorkbenchService)
	if !ok {
		done(api.WorkbenchResponse{}, fmt.Errorf("workbench service unavailable"))
		return
	}
	w.features.busy[key] = true
	w.start(func() {
		timeout := 35 * time.Second
		switch request.Action {
		case actionUpdateInstall:
			timeout = 16 * time.Minute
		case "pi.check_update":
			timeout = 4 * time.Minute
		case "pi.upgrade":
			timeout = 11 * time.Minute
		}
		ctx, cancel := context.WithTimeout(w.ctx, timeout)
		defer cancel()
		response, err := service.Workbench(ctx, request)
		w.post(func() { delete(w.features.busy, key); done(response, err) })
	})
}

func (w *workbench) setToolFeedback(key string, value notice) {
	if w.features.feedback == nil {
		w.features.feedback = map[string]notice{}
	}
	w.features.feedback[key] = value
}

func (w *workbench) panelFeedback() notice {
	key := w.features.panel + ":" + w.selected
	if w.features.panel == "remote" || w.features.panel == "terminal" && w.termTab.active == terminalHostsTab {
		key = "remote"
	}
	return w.features.feedback[key]
}

func (w *workbench) updateThread(command api.Command) {
	command.Kind = api.UpdateThread
	if command.ThreadID == "" {
		command.ThreadID = w.selected
	}
	pin, archive := "unchanged", "unchanged"
	if command.Pinned != nil {
		pin = fmt.Sprint(*command.Pinned)
	}
	if command.Archived != nil {
		archive = fmt.Sprint(*command.Archived)
	}
	snooze := "unchanged"
	if command.SnoozedUntil != nil {
		snooze = command.SnoozedUntil.UTC().Format(time.RFC3339)
	}
	w.taskAction("thread:"+command.ThreadID+":"+command.Title+":"+command.Model+":"+pin+":"+archive+":"+snooze, command)
}

func (w *workbench) foldMessage(event api.Event) bool {
	var payload struct {
		Message api.QueuedMessage `json:"message"`
	}
	if json.Unmarshal(event.Data, &payload) != nil || payload.Message.ID == "" {
		return false
	}
	for i, m := range w.messages {
		if m.ID == payload.Message.ID {
			w.messages[i] = payload.Message
			return true
		}
	}
	w.messages = append(w.messages, payload.Message)
	return true
}

func (w *workbench) pickAttachments() {
	target := w.selected
	comp := w.composerFor(target)
	if target == "" || comp.uploading {
		return
	}
	comp.uploading = true
	w.start(func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Title: "添加附件", Multiple: true})
		w.post(func() {
			comp.uploading = false
			if err != nil {
				comp.feedback = notice{Text: describe("选择附件失败", err), Level: noticeError}
				return
			}
			w.importAttachments(target, paths)
		})
	})
}

func (w *workbench) importAttachments(target string, paths []string) {
	comp := w.composerFor(target)
	if target == "" || len(paths) == 0 || comp.uploading {
		return
	}
	if len(paths)+len(comp.attachments) > 8 {
		comp.feedback = notice{Text: w.tr("每条消息最多添加 8 个附件。", "Up to 8 attachments per message."), Level: noticeWarn}
		return
	}
	service, ok := w.service.(api.WorkbenchService)
	if !ok {
		comp.feedback = notice{Text: w.tr("当前服务不支持添加附件，请更新本地服务。", "The current service does not support attachments."), Level: noticeError}
		return
	}
	anchor, caret := comp.liveAnchor, comp.liveCaret
	comp.uploading = true
	w.start(func() {
		var imported []api.Attachment
		var failure string
		for _, path := range paths {
			att, errMsg := w.readAndUploadLocalFile(service, path, true)
			if errMsg != "" {
				failure = errMsg
				break
			}
			imported = append(imported, att)
		}
		w.post(func() {
			comp.uploading = false
			var inlineMedia, otherFiles []api.Attachment
			for _, item := range imported {
				switch classifyAttachment(item) {
				case attachImage, attachPDF:
					inlineMedia = append(inlineMedia, item)
				default:
					otherFiles = append(otherFiles, item)
				}
			}
			if len(otherFiles) > 0 {
				comp.attachments = append(comp.attachments, otherFiles...)
			}
			if len(inlineMedia) > 0 {
				w.insertPastedAttachmentsAt(target, inlineMedia, anchor, caret)
			} else if len(otherFiles) > 0 {
				w.saveState()
			}
			comp.feedback = notice{}
			if failure != "" {
				comp.feedback = notice{Text: failure + w.tr(" 草稿和已添加的附件已保留。", " Draft and added attachments were preserved."), Level: noticeError}
			}
		})
	})
}

func (w *workbench) chooseProject() {
	w.start(func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Title: "打开项目", Directory: true})
		w.post(func() {
			if err != nil {
				w.setAction(describe("选择项目失败", err))
				return
			}
			if len(paths) == 0 {
				return
			}
			w.defaults.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: paths[0]}
			w.newSession()
			w.invalidateModels()
		})
	})
}

func (w *workbench) loadGit() {
	w.refreshGit(w.selected, false)
	delete(w.gitView.prLoaded, w.selected) // an explicit refresh re-reads the PR too
	w.loadPR(w.selected)
}

// adoptGit takes the state Core read right after an action. Every mutating
// Git action already answers with a full inspection, so reading it again
// would double the slowest part of the round trip.
func (w *workbench) adoptGit(target string, response api.WorkbenchResponse) bool {
	if response.Git == nil || response.Git.Files == nil {
		return false
	}
	if w.features.git == nil {
		w.features.git = map[string]*api.GitState{}
	}
	w.features.git[target] = response.Git
	w.pruneGitSelection(target, response.Git)
	return true
}

// refreshGit always replaces the displayed state from Core's current answer.
// After a merge it preserves the merge feedback/output instead of hiding a
// completed commit or hook-created changes behind a routine refresh.
func (w *workbench) refreshGit(target string, preserveFeedback bool) {
	request, ok := w.gitRequest(target, "git.status")
	if !ok {
		return
	}
	w.workbenchCall("git:"+target, request, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			delete(w.features.git, target) // partial results must not look like a clean checkout
			if preserveFeedback {
				current := w.features.feedback["git:"+target]
				current.Text += "；" + describe("刷新实际 Git 状态失败", err)
				current.Level = noticeError
				w.setToolFeedback("git:"+target, current)
			} else {
				w.setToolFeedback("git:"+target, notice{Text: describe("读取 Git 失败", err), Level: noticeError})
			}
			return
		}
		if response.Git == nil {
			delete(w.features.git, target)
			w.setToolFeedback("git:"+target, notice{Text: "本地服务未返回 Git 状态，请刷新重试。", Level: noticeError})
			return
		}
		if w.features.git == nil {
			w.features.git = map[string]*api.GitState{}
		}
		w.features.git[target] = response.Git
		w.pruneGitSelection(target, response.Git)
		if !preserveFeedback {
			w.setToolFeedback("git:"+target, notice{})
		}
		if w.shouldInspectWorktree(target) {
			w.loadWorktreeStatus(target)
		}
	})
}

func (w *workbench) mergeBranch() {
	target, branch := w.selected, w.features.gitBranches[w.selected]
	request, ok := w.gitRequest(target, "git.merge")
	if branch == "" || !ok {
		return
	}
	request.Branch = branch
	w.workbenchCall("git:"+target, request, func(response api.WorkbenchResponse, err error) {
		if w.features.gitOutputs == nil {
			w.features.gitOutputs = map[string]string{}
		}
		w.features.gitOutputs[target] = response.Output
		if response.Git != nil {
			if w.features.git == nil {
				w.features.git = map[string]*api.GitState{}
			}
			w.features.git[target] = response.Git
		}
		if err != nil {
			w.setToolFeedback("git:"+target, notice{Text: describe("合并结果需检查", err), Level: noticeError})
		} else {
			w.setToolFeedback("git:"+target, notice{Text: "本地分支合并完成；已刷新实际工作区状态。", Level: noticeInfo})
		}
		// App versions that omit a response with an error are still safe: this
		// independent read updates conflicts, committed state and hook changes.
		if !w.adoptGit(target, response) {
			w.refreshGit(target, true)
		}
	})
}

func (w *workbench) openTerminal() {
	target := w.selected
	if w.features.terminals[target] != nil {
		w.termTab.active = ""
		return
	}
	// A thread that runs on another machine has no shell here: a local pty
	// would open on the wrong computer. Reach that machine over SSH instead.
	if machine := w.terminalRemoteMachine(); machine != "" {
		if entry, ok := w.sshEntryForMachine(machine); ok {
			w.openSSHEntry(entry)
			return
		}
		w.termTab.active = terminalHostsTab
		w.setToolFeedback("remote", notice{Text: fmt.Sprintf(w.tr("这个会话运行在「%s」上。选择或添加它的 SSH 主机即可在那台电脑上打开终端。", "This thread runs on %s. Pick or add its SSH host to open a terminal on that computer."), machine), Level: noticeInfo})
		w.loadSSHHosts(false)
		return
	}
	w.termTab.active = ""
	if !w.hasThread(target) {
		// No thread yet: open the shell in the chosen project folder, or home.
		dir := w.selectedWorkspacePath()
		if dir == "" {
			dir, _ = os.UserHomeDir()
		}
		w.startLocalTerminal(target, dir)
		return
	}
	remote := w.defaults.RemoteHost
	w.workbenchCall("terminal:"+target, api.WorkbenchRequest{Action: "workspace", ThreadID: target}, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.setToolFeedback("terminal:"+target, notice{Text: describe("打开终端失败", err), Level: noticeError})
			return
		}
		if w.ctx.Err() != nil {
			return // window closed while the workspace request was in flight
		}
		options := terminal.Options{Dir: response.Workspace, Scrollback: 4 << 20, ShellIntegration: true}
		if remote != "" {
			options.Dir = ""
			options.ShellIntegration = false
			options.Command = []string{"ssh", "-tt", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "--", remote, "cd -- " + quoteShell(response.Workspace) + " && exec \"${SHELL:-/bin/sh}\" -l"}
		}
		w.installTerminal(target, options)
	})
}

// startLocalTerminal opens a login shell on this computer in dir.
func (w *workbench) startLocalTerminal(target, dir string) {
	w.installTerminal(target, terminal.Options{Dir: dir, Scrollback: 4 << 20, ShellIntegration: true})
}

func (w *workbench) installTerminal(target string, options terminal.Options) {
	term, err := w.makeTerminal(options)
	if err != nil {
		w.setToolFeedback("terminal:"+target, notice{Text: describe("初始化原生终端失败", err), Level: noticeError})
		return
	}
	if w.features.terminals == nil {
		w.features.terminals = map[string]*terminal.Terminal{}
	}
	w.features.terminals[target] = term
	w.setToolFeedback("terminal:"+target, notice{})
}

// terminalRemoteMachine names the computer the selected thread runs on when
// that is not this one: a remote environment view or a proxied task.
func (w *workbench) terminalRemoteMachine() string {
	if w.proxyEnvironmentID() != "" {
		return nonEmptyString(w.proxyEnvironmentLabel(), w.proxyEnvironmentID())
	}
	if w.defaults.RemotePeer {
		return nonEmptyString(strings.TrimSpace(w.defaults.RemoteName), w.tr("远端电脑", "the remote computer"))
	}
	return ""
}

// sshEntryForMachine finds the SSH host whose alias, name or address is the
// machine's name, ignoring case, spaces and dashes.
func (w *workbench) sshEntryForMachine(machine string) (sshEntry, bool) {
	norm := func(v string) string {
		v = strings.ToLower(strings.TrimSpace(v))
		v = strings.TrimSuffix(v, ".local")
		return strings.NewReplacer(" ", "", "-", "", "_", "", "'", "", "’", "").Replace(v)
	}
	want := norm(machine)
	if want == "" {
		return sshEntry{}, false
	}
	for _, entry := range w.sshEntries() {
		for _, candidate := range []string{entry.name, entry.target, entry.host.Alias, entry.host.HostName} {
			if candidate != "" && norm(candidate) == want {
				return entry, true
			}
		}
	}
	return sshEntry{}, false
}

func quoteShell(text string) string { return "'" + strings.ReplaceAll(text, "'", "'\\''") + "'" }

func (w *workbench) closeTerminals() {
	for _, term := range w.features.terminals {
		_ = term.Close()
	}
	w.closeSSHTerminals()
	w.disconnectRemoteDesktop()
}

func (w *workbench) loadPlugins() {
	target := w.selected
	w.workbenchCall("plugins", api.WorkbenchRequest{Action: "plugins.list"}, func(response api.WorkbenchResponse, err error) {
		if err != nil {
			w.setToolFeedback("plugins:"+target, notice{Text: describe("加载插件失败", err), Level: noticeError})
			return
		}
		w.features.plugins = response.Plugins
		w.plugs.loaded = true
		w.setToolFeedback("plugins:"+target, notice{})
	})
}

func (w *workbench) runPlugin(id string) {
	target := w.selected
	origin := w.composerFor(target)
	input, _ := json.Marshal(map[string]any{"message": origin.draft, "threadId": target, "title": w.titleFor(target)})
	w.workbenchCall("plugin-run:"+target, api.WorkbenchRequest{Action: "plugins.run", PluginID: id, ThreadID: target, Workspace: w.selectedWorkspacePath(), Input: input}, func(response api.WorkbenchResponse, err error) {
		// First send can replace a local ID while the plugin is running. The
		// composer identity survives that migration and still owns this result.
		for sessionID, comp := range w.composers {
			if comp == origin {
				target = sessionID
				break
			}
		}
		if err != nil {
			w.setToolFeedback("plugins:"+target, notice{Text: describe("插件运行失败", err), Level: noticeError})
			return
		}
		if w.features.pluginOutputs == nil {
			w.features.pluginOutputs = map[string]string{}
		}
		w.features.pluginOutputs[target] = response.Output
		w.setToolFeedback("plugins:"+target, notice{})
	})
}

func (w *workbench) connectRemote() {
	host, socket := strings.TrimSpace(w.features.remoteHost), strings.TrimSpace(w.features.remoteSocket)
	if err := sshrelay.Validate(host, socket); err != nil {
		w.setToolFeedback("remote", notice{Text: err.Error(), Level: noticeError})
		return
	}
	executable, err := os.Executable()
	if err != nil {
		w.setToolFeedback("remote", notice{Text: describe("定位桌面程序失败", err), Level: noticeError})
		return
	}
	w.start(func() {
		command := exec.Command(executable, "desktop", "--remote", host, "--remote-socket", socket)
		w.runRemoteProcess(command)
	})
}

type remoteTail struct{ data []byte }

func (b *remoteTail) Write(p []byte) (int, error) {
	n := len(p)
	b.data = append(b.data, p...)
	const limit = 8 << 10
	if len(b.data) > limit {
		b.data = append([]byte(nil), b.data[len(b.data)-limit:]...)
	}
	return n, nil
}

// runRemoteProcess owns the complete child lifetime. A remote window is a
// separate client: its exit is reported here and never closes this workbench.
func (w *workbench) runRemoteProcess(command *exec.Cmd) {
	stderr := &remoteTail{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		w.post(func() {
			w.setToolFeedback("remote", notice{Text: describe("打开远程窗口失败", err), Level: noticeError})
		})
		return
	}
	w.post(func() {
		w.setToolFeedback("remote", notice{Text: "远程窗口已启动；关闭它不会退出当前工作台。", Level: noticeInfo})
	})
	err := command.Wait()
	output := strings.TrimSpace(string(stderr.data))
	w.post(func() {
		if err == nil {
			w.setToolFeedback("remote", notice{Text: "远程窗口已关闭；当前工作台和 Core 任务继续运行。", Level: noticeInfo})
			return
		}
		text := "远程窗口启动或连接失败：" + explain(err)
		if exitError, ok := err.(*exec.ExitError); ok {
			text = fmt.Sprintf("远程窗口启动或连接失败（退出码 %d）。", exitError.ExitCode())
		}
		if output != "" {
			text += "\n" + output
		}
		w.setToolFeedback("remote", notice{Text: text, Level: noticeError})
	})
}

func (w *workbench) motion() ui.ElementTransition {
	return ui.ElementTransition{Duration: 150 * time.Millisecond, Enter: &ui.Motion{Y: 4}, Exit: &ui.Motion{}}
}
