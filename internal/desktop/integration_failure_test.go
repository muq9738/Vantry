package desktop

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestModelPickerHidesCatalogNoticesAndLaterMessagesHint(t *testing.T) {
	w, _ := newViewWorkbench()
	w.widgets.modelPickerOpen = true
	w.features.catalog = &api.ModelCatalog{
		Default: "magpie/available", ConfiguredDefault: "extension/default",
		Warnings: []string{"目录仅包含 Pi 内置模型与当前资源策略可用的 models.json 配置。"},
	}
	tt := ui.NewTester(w.view, 1080, 700)
	for _, text := range []string{
		"目录仅包含 Pi 内置模型与当前资源策略可用的 models.json 配置。",
		"已配置的默认模型为 extension/default；当前可用默认模型为 magpie/available。",
		"请确认本次使用的模型。",
		"应用于后续消息",
	} {
		if tt.HasText(text) {
			t.Errorf("model picker shows hint %q: %q", text, tt.Texts())
		}
	}
	if _, ok := tt.Access("重新载入"); !ok {
		t.Fatal("reload must stay reachable in the picker header")
	}
}

func TestGitMergeAlwaysRefreshesAndKeepsActualOutput(t *testing.T) {
	for _, test := range []struct {
		name     string
		mergeErr error
	}{{"success", nil}, {"error-with-output", errors.New("merge command returned an error")}} {
		t.Run(test.name, func(t *testing.T) {
			mergeCalls, statusCalls := 0, 0
			f := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
				switch request.Action {
				case "git.merge":
					mergeCalls++
					return api.WorkbenchResponse{Output: "提交已创建；post-commit hook 留下未暂存改动。"}, test.mergeErr
				case "git.status":
					statusCalls++
					return api.WorkbenchResponse{Git: &api.GitState{Branch: "main", Status: " M hook.txt", Diff: "+from-hook", Branches: []string{"main", "feature"}}}, nil
				default:
					return api.WorkbenchResponse{}, fmt.Errorf("unexpected action %s", request.Action)
				}
			}}
			w := newWorkbench(f)
			w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("t1", "Thread", 0)}})
			w.features.git = map[string]*api.GitState{"t1": {Branch: "main", Diff: "old-diff"}}
			w.features.gitBranches = map[string]string{"t1": "feature"}
			held := holdWork(w)
			w.mergeBranch()
			held.release()
			if mergeCalls != 1 || statusCalls != 1 {
				t.Fatalf("merge/status calls = %d/%d", mergeCalls, statusCalls)
			}
			if got := w.features.git["t1"].Diff; got != "+from-hook" {
				t.Fatalf("old diff survived actual refresh: %q", got)
			}
			feedback := w.features.feedback["git:t1"]
			if !strings.Contains(w.features.gitOutputs["t1"], "提交已创建") || test.mergeErr != nil && feedback.Level != noticeError || test.mergeErr == nil && feedback.Level != noticeInfo {
				t.Fatalf("merge result was lost: output=%q feedback=%+v", w.features.gitOutputs["t1"], feedback)
			}
			w.features.panel = "git"
			tt := ui.NewTester(w.view, 1080, 700)
			if !tt.HasText("提交已创建；post-commit hook 留下未暂存改动。") || !tt.HasText("+from-hook") {
				t.Fatalf("actual merge output/state not rendered: %q", tt.Texts())
			}
		})
	}
}

func TestGitMergeRefreshFailureDoesNotEraseOutputOrFeedback(t *testing.T) {
	f := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if request.Action == "git.merge" {
			return api.WorkbenchResponse{Output: "merge may have committed"}, errors.New("receipt unavailable")
		}
		return api.WorkbenchResponse{}, errors.New("status unavailable")
	}}
	w := newWorkbench(f)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("t1", "Thread", 0)}})
	w.features.gitBranches = map[string]string{"t1": "feature"}
	held := holdWork(w)
	w.mergeBranch()
	held.release()
	feedback := w.features.feedback["git:t1"].Text
	if w.features.gitOutputs["t1"] != "merge may have committed" || !strings.Contains(feedback, "合并结果需检查") || !strings.Contains(feedback, "刷新实际 Git 状态失败") {
		t.Fatalf("failed refresh erased merge evidence: output=%q feedback=%q", w.features.gitOutputs["t1"], feedback)
	}
}

func TestRemoteFailureHelperProcess(t *testing.T) {
	if os.Getenv("VANTRY_DESKTOP_REMOTE_HELPER") != "fail" {
		return
	}
	fmt.Fprintln(os.Stderr, "controlled remote startup diagnostic")
	os.Exit(23)
}

func TestRemoteProcessFailureIsAsynchronousFeedbackNotWorkbenchExit(t *testing.T) {
	w, _ := newViewWorkbench()
	command := exec.Command(os.Args[0], "-test.run=^TestRemoteFailureHelperProcess$")
	command.Env = append(os.Environ(), "VANTRY_DESKTOP_REMOTE_HELPER=fail")
	w.runRemoteProcess(command)
	feedback := w.features.feedback["remote"]
	if feedback.Level != noticeError || !strings.Contains(feedback.Text, "退出码 23") || !strings.Contains(feedback.Text, "controlled remote startup diagnostic") {
		t.Fatalf("child stderr/exit was not reported: %+v", feedback)
	}
	if w.ctx.Err() != nil || w.selected != "t1" {
		t.Fatal("remote window failure closed the originating workbench")
	}
}

func TestRemoteProcessStartFailureIsVisible(t *testing.T) {
	w, _ := newViewWorkbench()
	w.runRemoteProcess(exec.Command("/vantry-missing-executable"))
	if feedback := w.features.feedback["remote"]; feedback.Level != noticeError || !strings.Contains(feedback.Text, "打开远程窗口失败") {
		t.Fatalf("start failure not visible: %+v", feedback)
	}
}
