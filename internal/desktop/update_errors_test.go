package desktop

import (
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestUpdateSettingsShowsActionableErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		action  string
		message string
	}{
		{"missing credential", actionUpdateCheck, "GitHub 仓库 muq9738/Vantry 不可见；私有更新请配置仅该仓库 Contents 只读的专用凭据"},
		{"credential permissions", actionUpdateCheck, "Vantry 更新凭据文件须仅当前用户可读写（0600）"},
		{"download", actionUpdateInstall, "GitHub 返回 403 Forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
				if r.Action == tc.action {
					return api.WorkbenchResponse{}, &api.Error{Code: api.InvalidCommand, Message: tc.message}
				}
				if r.Action == actionUpdateCheck {
					return api.WorkbenchResponse{Update: &api.AppUpdateInfo{CurrentVersion: "0.1.20", LatestVersion: "0.1.21", Available: true}}, nil
				}
				return api.WorkbenchResponse{}, nil
			})
			tt := ui.NewTester(w.view, 1280, 900)
			mustClick(t, tt, "设置")
			mustClick(t, tt, "关于与更新")
			mustClick(t, tt, "检查更新")
			prefix := "检查更新失败"
			if tc.action == actionUpdateInstall {
				mustClick(t, tt, "下载更新")
				prefix = "更新未完成"
			}
			tt.Frame()
			if !tt.HasText(prefix + "：" + tc.message) {
				t.Fatalf("actionable update error missing: %q", tt.Texts())
			}
		})
	}
}

func TestUpdateApplyBusyCoreDoesNotQuitAndRemoteDoesNotRestartLocal(t *testing.T) {
	t.Run("busy-core-stays-open", func(t *testing.T) {
		var quitCalls int
		w, _ := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
			switch r.Action {
			case actionUpdateCheck:
				return api.WorkbenchResponse{Update: &api.AppUpdateInfo{
					CurrentVersion: "0.1.27",
					LatestVersion:  "0.1.28",
					Available:      true,
					Staged:         true,
					StagedVersion:  "0.1.28",
					StagedPath:     "/Applications/.vantry-update-1/payload/Vantry.app",
					TargetPath:     "/Applications/Vantry.app",
					CanAutoRestart: true,
				}}, nil
			case actionUpdateApply:
				return api.WorkbenchResponse{}, &api.Error{Code: api.InvalidCommand, Message: "Core 仍有 1 个活动任务，请等待任务结束后再重启更新"}
			}
			return api.WorkbenchResponse{}, nil
		})
		w.quit = func() { quitCalls++ }
		tt := ui.NewTester(w.view, 1280, 900)
		mustClick(t, tt, "设置")
		mustClick(t, tt, "关于与更新")
		mustClick(t, tt, "检查更新")
		mustClick(t, tt, "重启并更新")
		tt.Frame()
		if quitCalls != 0 {
			t.Fatalf("desktop quit despite busy Core failure: quitCalls=%d", quitCalls)
		}
		if !tt.HasText("无法重启更新：Core 仍有 1 个活动任务，请等待任务结束后再重启更新") {
			t.Fatalf("busy Core error missing: %q", tt.Texts())
		}
	})

	t.Run("remote-peer-never-shows-restart-local", func(t *testing.T) {
		var quitCalls int
		w, service := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
			if r.Action == actionUpdateCheck {
				return api.WorkbenchResponse{Update: &api.AppUpdateInfo{
					CurrentVersion: "0.1.27",
					LatestVersion:  "0.1.28",
					Available:      true,
					Staged:         true,
					StagedVersion:  "0.1.28",
					StagedPath:     "/Applications/.vantry-update-1/payload/Vantry.app",
					TargetPath:     "/Applications/Vantry.app",
					InstallerPath:  "/Applications/.vantry-update-1/安装更新.command",
					CanAutoRestart: true,
				}}, nil
			}
			return api.WorkbenchResponse{}, nil
		})
		w.defaults.RemotePeer = true
		w.quit = func() { quitCalls++ }
		tt := ui.NewTester(w.view, 1280, 900)
		mustClick(t, tt, "设置")
		mustClick(t, tt, "关于与更新")
		mustClick(t, tt, "检查更新")
		tt.Frame()
		if tt.HasText("重启并更新") {
			t.Fatalf("remote peer unexpectedly offered Restart & Update: %q", tt.Texts())
		}
		if !tt.HasText("复制安装命令") {
			t.Fatalf("remote peer missing manual copy action: %q", tt.Texts())
		}
		summary := "v0.1.28 已下载并完成校验（完整包）"
		hint := "当前应用尚未替换，请先退出 Vantry 后手动安装。"
		path := "/Applications/.vantry-update-1/安装更新.command"
		r1, ok1 := tt.Find(summary)
		r2, ok2 := tt.Find(hint)
		r3, ok3 := tt.Find(path)
		if !ok1 || !ok2 || !ok3 {
			t.Fatalf("staged card text rects missing: ok=(%v,%v,%v) texts=%q", ok1, ok2, ok3, tt.Texts())
		}
		if r1.Y+r1.H > r2.Y+0.5 || r2.Y+r2.H > r3.Y+0.5 {
			t.Fatalf("staged card lines overlap vertically: summary=%+v hint=%+v path=%+v", r1, r2, r3)
		}
		if r1.W < 100 || r2.W < 100 || r3.X+r3.W > 1280 {
			t.Fatalf("staged card bounds clipped or overflowed: summary=%+v hint=%+v path=%+v", r1, r2, r3)
		}
		w.applyUpdate()
		if _, ok := service.find(actionUpdateApply); ok || quitCalls != 0 {
			t.Fatalf("remote peer triggered update.apply or local quit: quitCalls=%d", quitCalls)
		}
	})
}
