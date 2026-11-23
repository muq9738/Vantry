package desktop

import (
	"reflect"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestGitControlsSubmitExactFileAndCommitIntent(t *testing.T) {
	for _, test := range []struct {
		name, action string
		paths        []string
		commit       bool
	}{
		{"暂存 unstaged.go", "git.stage", []string{"unstaged.go"}, false},
		{"暂存全部", "git.stage", []string{"unstaged.go"}, false},
		{"取消全部暂存", "git.unstage", []string{"staged.go"}, false},
		{"提交已暂存", "git.commit", nil, true},
	} {
		t.Run(test.action+test.name, func(t *testing.T) {
			var requests []api.WorkbenchRequest
			state := &api.GitState{Branch: "feature", Branches: []string{"main", "feature"}, Files: []api.GitFile{
				{Path: "unstaged.go", Index: " ", Worktree: "M"}, {Path: "staged.go", Index: "M", Worktree: " "},
			}}
			service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
				if req.Action == "models" { // background thinking-level preload
					return api.WorkbenchResponse{}, nil
				}
				requests = append(requests, req)
				return api.WorkbenchResponse{Git: state}, nil
			}}
			w := newWorkbench(service)
			w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("t1", "Thread", 0)}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "q", "a", 0)}})
			w.features.panel = "git"
			w.features.git = map[string]*api.GitState{"t1": state}
			held := holdWork(w)
			tt := ui.NewTester(w.view, 640, 600)
			mustClick(t, tt, test.name)
			if test.commit {
				if !tt.Focused("提交说明") {
					t.Fatal("commit dialog did not focus message")
				}
				tt.Type("local change")
				mustClick(t, tt, "创建本地提交")
			}
			held.release()
			// Core answers a change with the state it read afterwards, so the
			// desktop adopts it instead of reading Git a second time.
			if len(requests) != 1 || requests[0].Action != test.action {
				t.Fatalf("write/refresh=%+v", requests)
			}
			req := requests[0]
			if req.ThreadID != "t1" || req.Branch != "feature" || !reflect.DeepEqual(req.Paths, test.paths) {
				t.Fatalf("wrong target=%+v", req)
			}
			if test.commit && req.Message != "local change" {
				t.Fatalf("message=%q", req.Message)
			}
		})
	}
}
