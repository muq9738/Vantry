package desktop

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestNewFollowUpDoesNotReplayAnEarlierRunCommand(t *testing.T) {
	for _, steer := range []bool{false, true} {
		t.Run(map[bool]string{false: "queue", true: "steer"}[steer], func(t *testing.T) {
			service := &fakeService{receiptErr: errors.New("receipt unavailable")}
			w := newWorkbench(service)
			w.applySnapshot(snapshotOneRun())
			w.setDraft("t1", "old instruction")
			if !w.queueDraft("t1", steer) {
				t.Fatal("first send failed")
			}
			old := service.dispatched()[0]
			w.applyEvent(statusEvent(4, run("r1", "t1", api.RunCompleted, "old", "done", 0)))
			w.applyEvent(statusEvent(5, run("r2", "t1", api.RunRunning, "new", "", time.Minute)))
			service.receiptErr = nil
			w.setDraft("t1", "new instruction")
			if !w.queueDraft("t1", steer) {
				t.Fatal("new send failed")
			}
			latest := service.dispatched()[1]
			if latest.Message != "new instruction" || latest.CommandID == old.CommandID {
				t.Fatalf("old request hijacked draft: %+v", latest)
			}
			if steer && (latest.RunID != "r2" || latest.ParentAttemptID != "r2-attempt") {
				t.Fatalf("wrong target: %+v", latest)
			}
			keys := w.retryKeys("t1")
			if len(keys) != 1 {
				t.Fatalf("old request lost: %v", keys)
			}
			w.setDraft("t1", "keep this draft")
			w.dispatchControl(keys[0], "retry", "", api.Command{}, false)
			if got := service.dispatched()[2]; !reflect.DeepEqual(got, old) {
				t.Fatalf("explicit retry replaced identity: %+v", got)
			}
			if w.composerFor("t1").draft != "keep this draft" {
				t.Fatal("retry cleared current draft")
			}
		})
	}
}

func TestPartialGitStatusNeverLooksClean(t *testing.T) {
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		return api.WorkbenchResponse{Git: &api.GitState{Branch: "main", Root: "/work", Files: []api.GitFile{}}}, errors.New("index unreadable")
	}}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("t1", "Thread", 0)}})
	w.features.git = map[string]*api.GitState{"t1": {Branch: "main", Status: " M old.go"}}
	w.features.panel = "git"
	w.refreshGit("t1", false)
	if w.features.git["t1"] != nil {
		t.Fatal("partial failed response cached")
	}
	tt := ui.NewTester(w.view, 1080, 700)
	if tt.HasText("工作区干净") {
		t.Fatal("failed status looked clean")
	}
	if w.features.feedback["git:t1"].Level != noticeError {
		t.Fatal("status failure not shown")
	}
}

func TestActiveRunDisablesMergeButAllowsRefresh(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunRunning, "working", "", 0))
	w.features.panel = "git"
	w.features.git = map[string]*api.GitState{"t1": {Branch: "main", Branches: []string{"main", "feature"}}}
	w.features.gitBranches = map[string]string{"t1": "feature"}
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "分支：main") // merge lives in the branch popover, as in IDEA
	mustClick(t, tt, "feature")
	merge, ok := tt.Access("自动合并到当前分支")
	if !ok || !merge.Disabled {
		t.Fatal("merge must be disabled")
	}
	refresh, ok := tt.Access("刷新差异")
	if !ok || refresh.Disabled {
		t.Fatal("read-only refresh must stay available")
	}
}

func TestWorktreeChoicePreservesDraftWithoutStartingWork(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "question", "answer", 0))
	w.setDraft("t1", "unfinished text")
	w.composerFor("t1").attachments = []api.Attachment{{ID: "a", Name: "note.txt"}}
	spec := &api.WorkspaceSpec{Kind: "worktree", Repository: "/repo", BaseRef: "main", Branch: "feature-ui"}
	w.configureWorkspaceSpec(spec)
	if !isLocalSession(w.selected) || !reflect.DeepEqual(w.defaults.Workspace, spec) {
		t.Fatal("worktree choice not saved")
	}
	if w.composerFor(w.selected).draft != "unfinished text" || len(w.composerFor(w.selected).attachments) != 1 {
		t.Fatal("draft lost")
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("project choice started work before send")
	}
}
