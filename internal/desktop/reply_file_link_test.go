package desktop

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestReplyFileLinkClickOpensRightDockAndReadsContent(t *testing.T) {
	for _, address := range []string{
		"/p/docs/task-orchestration.md",
		"docs/task-orchestration.md",
		"file:///p/docs/task-orchestration.md",
		"/p/docs/task-orchestration.md:3",
		"/p/docs/task-orchestration.md#L3",
	} {
		t.Run(address, func(t *testing.T) {
			var requests []api.WorkbenchRequest
			service := filesService(t, &requests)
			w := newWorkbench(service)
			th := thread("t1", "Thread", 0)
			th.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/p"}
			w.applySnapshot(api.Snapshot{Threads: []api.Thread{th}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "show the file", "规则见 [任务编排说明]("+address+")，点击阅读。", 0)}})
			opened := ""
			options := w.replyMarkdown("t1")
			originalLink := options.OnLink
			options.OnLink = func(address string) { opened = address; originalLink(address) }
			tt := ui.NewTester(w.view, 1440, 900)
			tt.Advance(time.Second)
			// Click the rendered Markdown link, not the Go callback directly.
			mustClick(t, tt, "任务编排说明")
			tt.Frame()
			if w.features.panel != "files" || w.files.open["t1"] != "docs/task-orchestration.md" {
				t.Fatalf("file link did not open right dock: panel=%q file=%q callback=%q notice=%q", w.features.panel, w.files.open["t1"], opened, w.actionNotice.Text)
			}
			read := false
			for _, req := range requests {
				if req.Action == actionFilesRead && req.ThreadID == "t1" && len(req.Paths) == 1 && req.Paths[0] == "docs/task-orchestration.md" {
					read = true
				}
			}
			if !read {
				t.Fatalf("no scoped file read: %+v", requests)
			}
			key := listingKey("thread:t1", "docs/task-orchestration.md")
			state := w.files.views[key]
			if state == nil || state.Buffer.String() != "package main\n\nfunc main() {}\n" {
				t.Fatal("file content did not reach the right-hand reader")
			}
			if strings.HasSuffix(address, ":3") || strings.HasSuffix(address, "#L3") {
				if got := state.Buffer.Pos(state.Selection().Head).Line; got != 2 {
					t.Fatalf("line target = %d, want 2", got)
				}
			}
			if len(tt.OpenedURLs()) != 0 {
				t.Fatalf("file link left the app: %v", tt.OpenedURLs())
			}
		})
	}
}

func TestReplyFileLinkReadFailureIsVisible(t *testing.T) {
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if req.Action == actionFilesRead {
			return api.WorkbenchResponse{}, errors.New("文件不存在")
		}
		return api.WorkbenchResponse{}, nil
	}}
	w := newWorkbench(service)
	th := thread("t1", "Thread", 0)
	th.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/p"}
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{th}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "show the file", "查看 [不存在的说明](/p/missing.md)。", 0)}})
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	mustClick(t, tt, "不存在的说明")
	tt.Frame()
	key := listingKey("thread:t1", "missing.md")
	failure := w.files.readErr[key]
	if w.features.panel != "files" || !strings.Contains(failure, "读取文件失败") || !tt.HasText(failure) {
		t.Fatalf("read error was hidden: panel=%q error=%q", w.features.panel, failure)
	}
}

func TestReplyFileLinkRejectsEscapesAndOtherSchemes(t *testing.T) {
	for _, address := range []string{"/outside/secret.txt", "../secret.txt", "file://another-host/p/file.md", "javascript:alert(1)"} {
		t.Run(address, func(t *testing.T) {
			var requests []api.WorkbenchRequest
			w := newWorkbench(filesService(t, &requests))
			th := thread("t1", "Thread", 0)
			th.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/p"}
			w.applySnapshot(api.Snapshot{Threads: []api.Thread{th}})
			w.replyLink(address)
			if len(requests) != 0 || w.features.panel != "" {
				t.Fatalf("invalid path caused a file request: %v", requests)
			}
		})
	}
}
