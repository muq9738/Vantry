package desktop

import (
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

func (w *workbench) refreshWorkspace() bool {
	threadID := w.selected
	if threadID == "" || isLocalSession(threadID) {
		w.workspaceErr = "请先选择已创建的会话。"
		return false
	}
	service, ok := w.service.(api.WorkbenchQueries)
	if !ok {
		w.workspaceErr = "当前服务未提供工作区检查。"
		return false
	}
	if w.workspaceLoading && w.workspaceThread == threadID {
		return false
	}
	w.workspaceVersion++
	w.workspaceThread = threadID
	version := w.workspaceVersion
	w.workspaceLoading = true
	w.workspaceErr = ""
	w.workspaceNotice = notice{}
	ctx := w.ctx
	w.start(func() {
		changes, err := service.InspectWorkspace(ctx, threadID)
		w.post(func() {
			// A late callback belongs to the thread and refresh that created it;
			// it must not replace a newly selected thread's panel.
			if w.workspaceVersion != version {
				return
			}
			w.workspaceLoading = false
			if w.selected != threadID || (!w.widgets.workspaceOpen && !w.widgets.diffOpen) {
				return
			}
			if err != nil {
				w.workspaceErr = describe("工作区检查失败", err)
				return
			}
			w.workspaceChanges = &changes
		})
	})
	return true
}

func (w *workbench) openSystemTerminal() bool {
	threadID := w.selected
	if threadID == "" || w.terminalCalls[threadID] {
		return false
	}
	if isLocalSession(threadID) {
		path := strings.TrimSpace(w.selectedWorkspacePath())
		if path == "" || w.defaults.RemotePeer {
			return false
		}
		w.terminalCalls[threadID] = true
		w.start(func() {
			openLocalFolderTerminal(path)
			w.post(func() { delete(w.terminalCalls, threadID) })
		})
		return true
	}
	service, ok := w.service.(api.WorkbenchQueries)
	if !ok {
		w.workspaceErr = "当前服务未提供系统终端入口。"
		return false
	}
	w.terminalCalls[threadID] = true
	w.workspaceNotice = notice{Text: "正在打开系统终端…", Level: noticeInfo}
	w.workspaceErr = ""
	ctx := w.ctx
	w.start(func() {
		err := service.OpenTerminal(ctx, threadID)
		w.post(func() {
			delete(w.terminalCalls, threadID)
			if w.selected != threadID || !w.widgets.workspaceOpen {
				return
			}
			if err != nil {
				w.workspaceErr = describe("系统终端打开失败", err)
				w.workspaceNotice = notice{}
				return
			}
			w.workspaceNotice = notice{Text: "已请求系统终端打开到当前工作区。", Level: noticeInfo}
		})
	})
	return true
}

func openLocalFolderTerminal(path string) {
	if runtime.GOOS == "darwin" {
		for _, app := range []string{"/Applications/Ghostty.app", "/Applications/iTerm.app", "/Applications/Warp.app", "/System/Applications/Utilities/Terminal.app"} {
			if st, err := os.Stat(app); err == nil && st.IsDir() {
				if exec.Command("open", "-a", app, path).Start() == nil {
					return
				}
			}
		}
		_ = exec.Command("open", "-a", "Terminal", path).Start()
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("cmd", "/c", "start", "cmd", "/k", "cd", "/d", path).Start()
		return
	}
	for _, bin := range []string{"x-terminal-emulator", "gnome-terminal", "konsole", "xfce4-terminal", "alacritty", "kitty"} {
		if p, err := exec.LookPath(bin); err == nil {
			cmd := exec.Command(p)
			cmd.Dir = path
			if cmd.Start() == nil {
				return
			}
		}
	}
}
