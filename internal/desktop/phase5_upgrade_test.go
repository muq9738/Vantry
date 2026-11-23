package desktop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestPhase5ForkSlashCommandDialogAndOriginChip(t *testing.T) {
	now := time.Now()
	var captured []api.WorkbenchRequest
	f := &fakeWorkbenchService{
		fakeService: &fakeService{
			receipt: func(cmd api.Command) (api.Receipt, error) {
				if cmd.Kind == api.ForkThread {
					return api.Receipt{Accepted: true, ThreadID: "t-forked"}, nil
				}
				return api.Receipt{Accepted: true, ThreadID: cmd.ThreadID}, nil
			},
		},
		workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
			captured = append(captured, req)
			switch req.Action {
			case actionSkillsList:
				return api.WorkbenchResponse{Output: `{"skills":[]}`}, nil
			case "models":
				return api.WorkbenchResponse{Catalog: &api.ModelCatalog{
					Models: []api.ModelInfo{
						{Provider: "openai", ID: "gpt-5", Name: "GPT-5"},
						{Provider: "anthropic", ID: "claude-sonnet-4", Name: "Claude Sonnet 4"},
					},
				}}, nil
			default:
				return api.WorkbenchResponse{}, nil
			}
		},
	}

	w := newWorkbench(f)
	w.applySnapshot(api.Snapshot{
		Sequence: 1,
		Threads: []api.Thread{
			{
				ID:        "t1",
				Title:     "主会话",
				Model:     "openai/gpt-5",
				Workspace: &api.WorkspaceSpec{Kind: "directory", Path: "/tmp/demo-repo"},
				CreatedAt: now,
			},
			{
				ID:                 "t2",
				Title:              "子分支",
				ForkedFromThreadID: "t1",
				CreatedAt:          now.Add(time.Minute),
			},
		},
		Runs: []api.Run{
			{ID: "r1", ThreadID: "t1", Status: api.RunCompleted, Input: "hello", Output: "world", CreatedAt: now},
		},
	})
	w.selectThread("t1")

	tt := ui.NewTester(w.view, 1280, 900)
	tt.Advance(time.Second)

	// 1. Built-in /fork picker shows up cleanly when typing "/fo" even with 0 Core skills.
	mustClick(t, tt, "消息输入框")
	tt.Type("/fo")
	if !tt.HasText("/fork") {
		t.Fatalf("picker did not suggest /fork: %v", tt.Texts())
	}

	// 2. Sending "/fork anthropic/claude-sonnet-4" opens forkDialog locally without SendMessage.
	w.setDraft("t1", "/fork anthropic/claude-sonnet-4")
	tt.Frame()
	if !w.send("t1") {
		t.Fatal("send(/fork) returned false")
	}
	if len(f.dispatched()) != 0 {
		t.Fatalf("/fork must not dispatch SendMessage, got %+v", f.dispatched())
	}
	if !w.widgets.fork.open || w.widgets.fork.model != "anthropic/claude-sonnet-4" {
		t.Fatalf("unexpected fork dialog state: %+v", w.widgets.fork)
	}
	tt.Frame()

	// Toggle Worktree isolation and set branch name, then click confirm.
	if err := tt.Click("在独立 Git Worktree 中分叉"); err != nil {
		t.Fatalf("click worktree toggle failed: %v", err)
	}
	w.widgets.fork.branch = "feat/fork-experiment"
	tt.Frame()
	if err := tt.Click("创建分叉会话"); err != nil {
		t.Fatalf("click confirm fork failed: %v", err)
	}
	tt.Frame()

	cmds := f.dispatched()
	if len(cmds) != 1 || cmds[0].Kind != api.ForkThread {
		t.Fatalf("expected 1 ForkThread command, got %+v", cmds)
	}
	if cmds[0].Model != "anthropic/claude-sonnet-4" ||
		cmds[0].Workspace == nil || cmds[0].Workspace.Kind != "worktree" ||
		cmds[0].Workspace.Repository != "/tmp/demo-repo" ||
		cmds[0].Workspace.Branch != "feat/fork-experiment" {
		t.Fatalf("ForkThread command missing model or worktree options: %+v", cmds[0])
	}

	// 3. originChip displays ForkedFromThreadID and navigates back to t1 when clicked.
	w.selectThread("t2")
	tt.Frame()
	if !tt.HasText("主会话") {
		t.Fatalf("originChip for ForkedFromThreadID missing parent title: %v", tt.Texts())
	}
	if err := tt.Click("分叉自"); err != nil {
		t.Fatalf("click originChip failed: %v", err)
	}
	if w.selected != "t1" {
		t.Fatalf("clicking ForkedFrom originChip selected %q, want t1", w.selected)
	}
	_ = captured
}

func TestPhase5GitPerHunkActionsAndWorktreeBanner(t *testing.T) {
	now := time.Now()
	const sampleDiff = `[未暂存 / unstaged]
diff --git a/app.go b/app.go
index 1111111..2222222 100644
--- a/app.go
+++ b/app.go
@@ -1,3 +1,3 @@
 package main
-func old() {}
+func newFn() {}
 func keep() {}
`
	var requests []api.WorkbenchRequest
	f := &fakeWorkbenchService{
		fakeService: &fakeService{},
		workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
			requests = append(requests, req)
			switch req.Action {
			case "git.status":
				return api.WorkbenchResponse{Git: &api.GitState{
					Branch: "vantry/wt-1",
					Files:  []api.GitFile{{Path: "app.go", Index: " ", Worktree: "M"}},
					Diff:   sampleDiff,
				}}, nil
			case actionGitWorktreeStatus:
				raw, _ := json.Marshal(gitWorktreeStatus{
					IsWorktree:      true,
					Managed:         true,
					Exists:          true,
					Branch:          "vantry/wt-1",
					BaseBranch:      "main",
					UnmergedCommits: 2,
					CanMerge:        true,
					CanCleanup:      true,
				})
				return api.WorkbenchResponse{Output: string(raw)}, nil
			case actionGitWorktreeMerge:
				return api.WorkbenchResponse{
					Output: `{"cleanedUp":true,"branchDeleted":true}`,
					Git:    &api.GitState{Branch: "main"},
				}, nil
			case "git.hunk.stage":
				return api.WorkbenchResponse{Git: &api.GitState{
					Branch: "vantry/wt-1",
					Files:  []api.GitFile{{Path: "app.go", Index: "M", Worktree: " "}},
					Diff:   "",
				}}, nil
			default:
				return api.WorkbenchResponse{}, nil
			}
		},
	}

	w := newWorkbench(f)
	w.applySnapshot(api.Snapshot{
		Sequence: 1,
		Threads: []api.Thread{{
			ID:        "t-wt",
			Title:     "WT 会话",
			Workspace: &api.WorkspaceSpec{Kind: "worktree", Repository: "/tmp/repo", Branch: "vantry/wt-1"},
			CreatedAt: now,
		}},
		Runs: []api.Run{{ID: "r1", ThreadID: "t-wt", Status: api.RunCompleted, CreatedAt: now}},
	})
	w.selectThread("t-wt")
	w.openDock("git")

	tt := ui.NewTester(w.view, 1280, 900)
	tt.Advance(time.Second)

	// Worktree banner should be displayed with merge & cleanup controls.
	for _, want := range []string{"独立 Worktree：vantry/wt-1 → main", "2 个未合并提交", "合并到主分支并清理", "1 块"} {
		if !tt.HasText(want) {
			t.Fatalf("missing %q in Git panel texts: %v", want, tt.Texts())
		}
	}

	// Verify DiffState inside patch cache has HunkActions enabled & wired.
	files := w.patchCache.files
	if len(files) != 1 || files[0].state == nil || !files[0].state.HunkActions || files[0].state.RenderHunkActions == nil {
		t.Fatalf("expected HunkActions enabled on diff viewer, got %+v", files)
	}

	// Verify changeGitHunk dispatches git.hunk.stage accurately.
	w.changeGitHunk("app.go", false, 0, "stage")
	tt.Frame()
	var sawHunkStage bool
	for _, r := range requests {
		if r.Action == "git.hunk.stage" && len(r.Paths) == 1 && r.Paths[0] == "app.go" && strings.Contains(string(r.Input), `"hunkIndex":0`) {
			sawHunkStage = true
		}
	}
	if !sawHunkStage {
		t.Fatalf("did not observe git.hunk.stage request in %+v", requests)
	}

	// Reload worktree & click inline merge & cleanup confirmation.
	w.loadWorktreeStatus("t-wt")
	tt.Frame()
	if err := tt.Click("合并到主分支并清理"); err != nil {
		t.Fatalf("click merge worktree failed: %v", err)
	}
	tt.Frame()
	if err := tt.Click("确认合并并清理"); err != nil {
		t.Fatalf("click confirm merge worktree failed: %v", err)
	}
	tt.Frame()
	var sawMerge bool
	for _, r := range requests {
		if r.Action == actionGitWorktreeMerge {
			sawMerge = true
		}
	}
	if !sawMerge {
		t.Fatalf("expected %s workbench call, got %+v", actionGitWorktreeMerge, requests)
	}
}
