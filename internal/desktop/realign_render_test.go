package desktop

import (
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// TestRealignRenders exports the surfaces of the T3 realignment for visual
// review: work log, subagents, composer mentions, rename, folder palette.
func TestRealignRenders(t *testing.T) {
	output := os.Getenv("VANTRY_REALIGN_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_REALIGN_RENDER_DIR to export review frames")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	save := func(t *testing.T, tt *ui.Tester, name string) {
		file, err := os.Create(filepath.Join(output, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := png.Encode(file, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}
	fixture := func() *workbench {
		r := run("r1", "t1", api.RunCompleted, "并行实现四个模块", "四个子代理已启动，分别负责 A、B、C、D。\n\n整合时发现两个会误导判断的问题，正在修正。", 0)
		w, _ := newViewWorkbench(r)
		w.threads[0].Title = "情报预测与兑现计算"
		w.threads[0].Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/stock", Branch: "main"}
		w.tools = []api.ToolCall{
			{ID: "a", ThreadID: "t1", RunID: "r1", Name: "bash", Arguments: json.RawMessage(`{"command":"cd /Users/demo/Develop/DemoApp && git reset -q --soft HEAD~1"}`), Status: "completed"},
			{ID: "b", ThreadID: "t1", RunID: "r1", Name: "bash", Arguments: json.RawMessage(`{"command":"du -sb build/compare/manifold"}`), Status: "failed", Output: "du: invalid option -- b"},
			{ID: "c", ThreadID: "t1", RunID: "r1", Name: "read", Arguments: json.RawMessage(`{"path":"/Projects/demo-app/build/compare/light.png"}`), Status: "completed"},
			{ID: "d", ThreadID: "t1", RunID: "r1", Name: "edit", Arguments: json.RawMessage(`{"path":"/Projects/demo-app/scripts/brief/card.py","oldText":"a","newText":"b\nc"}`), Status: "completed"},
			{ID: "e", ThreadID: "t1", RunID: "r1", Name: "ffgrep", Arguments: json.RawMessage(`{"pattern":"opportunity"}`), Status: "completed"},
		}
		now := baseTime.Add(10 * time.Minute)
		w.tasks = []api.Task{
			{ID: "k1", ParentThreadID: "t1", ParentRunID: "r1", ChildThreadID: "c1", Title: "DeepSeek D：简洁情景卡与证据展开", Status: api.RunCompleted, Result: "已完成独占文件 D 的实现与验证。## 文件清单（仅这两个新文件）", CreatedAt: baseTime, UpdatedAt: now},
			{ID: "k2", ParentThreadID: "t1", ParentRunID: "r1", ChildThreadID: "c2", Title: "DeepSeek B：情报预测与兑现计算", Status: api.RunCompleted, Result: "全部完成，只在我工作树内新增两个文件，未提交/推送/部署。", CreatedAt: baseTime, UpdatedAt: now.Add(3 * time.Minute)},
			{ID: "k3", ParentThreadID: "t1", ParentRunID: "r1", ChildThreadID: "c3", Title: "DeepSeek A：公开证据与公司事实", Status: api.RunRunning, CreatedAt: baseTime, UpdatedAt: now},
		}
		comp := w.composerFor("t1")
		draft := "看下 [Improve Buy Recommendation Decisiveness](vantry-thread://th_1) "
		collapsed, _, _ := comp.collapseThreadLinks(draft, 0)
		comp.addMention(skillMention(skillEntry{Name: "brave"}))
		w.setDraft("t1", collapsed+"/brave ")
		return w
	}
	for _, dark := range []bool{true, false} {
		suffix := "-light"
		if dark {
			suffix = "-dark"
		}
		t.Run("timeline"+suffix, func(t *testing.T) {
			w := fixture()
			tt := ui.NewTesterWith(w.view, 1280, 860, ui.TesterOptions{Dark: dark})
			tt.Advance(time.Second)
			save(t, tt, "timeline"+suffix)
			w.widgets.workOpen = map[string]bool{"r1:0": true}
			tt.Advance(time.Second)
			save(t, tt, "timeline-open"+suffix)
		})
		t.Run("rename"+suffix, func(t *testing.T) {
			w := fixture()
			w.threads[0].Title = "[项目协调员 · demo-app](vantry-thread://long) 很长的标题"
			tt := ui.NewTesterWith(w.view, 1280, 860, ui.TesterOptions{Dark: dark})
			tt.Frame()
			w.beginRename("t1")
			tt.Advance(time.Second)
			save(t, tt, "rename"+suffix)
		})
		t.Run("gitlog"+suffix, func(t *testing.T) {
			state := fixtureGitState()
			state.Branches = append(state.Branches, "release-demo-merge-20261119", "feat/canvas-shadow-v1", "feature/20240831", "feature/20251223-master-ready", "backup/old", "develop", "t3/a", "t3/b")
			state.Branch = "release-demo-merge-20261119"
			state.RemoteBranches = append(state.RemoteBranches, "origin/release-main", "origin/feature/x", "upstream/main")
			w, _ := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
				switch r.Action {
				case actionGitLog:
					return api.WorkbenchResponse{Git: &api.GitState{Commits: state.Commits}}, nil
				case actionGitCommitDetail:
					detail, _ := json.Marshal(map[string]any{
						"commit": state.Commits[0], "authorEmail": "dev@example.com", "body": "feat: 按住 Ctrl 在画布、右侧树和弹窗树操作时不弹浏览器原生菜单\n\n细节说明第二段。",
						"branches": []string{"release-demo-merge-20261119", "origin/release-demo-merge-20261119", "main", "feature/dock"},
						"files":    []api.GitFile{{Path: "src/views/canvas/treeList/index.vue", Index: "M"}, {Path: "src/views/canvas/treeList/TreeDesktop.vue", Index: "M"}, {Path: "src/views/canvas/MeshView.vue", Index: "A"}, {Path: ".agents/skills/testing/component.md", Index: "M"}},
						"diff":     "diff --git a/src/views/canvas/treeList/index.vue b/src/views/canvas/treeList/index.vue\n--- a/src/views/canvas/treeList/index.vue\n+++ b/src/views/canvas/treeList/index.vue\n@@ -1,2 +1,2 @@\n <template>\n-  <div>old</div>\n+  <div @contextmenu.prevent>new</div>\n",
					})
					return api.WorkbenchResponse{Output: string(detail)}, nil
				}
				return api.WorkbenchResponse{Git: state}, nil
			})
			w.features.panel = "git"
			w.features.git = map[string]*api.GitState{"t1": state}
			w.gitView.tab = 1
			w.dock.maximized = true
			tt := ui.NewTesterWith(w.view, 1680, 900, ui.TesterOptions{Dark: dark})
			tt.Advance(time.Second)
			w.selectCommit(state.Commits[0].ID)
			tt.Advance(time.Second)
			tt.Frame()
			save(t, tt, "gitlog"+suffix)
		})
		t.Run("remote"+suffix, func(t *testing.T) {
			w, _ := v3Workbench(t, nil)
			w.features.plugins = append(w.features.plugins, api.PluginInfo{ID: "rd", Name: "远程桌面", Navigation: []api.PluginNavItem{{Title: "远程桌面", Page: "desktop", NativePanel: "remote-desktop"}},
				Settings: []api.PluginSettingSpec{{Key: "relayURL", Value: "https://relay.mine.example"}}})
			w.defaults.RemoteDesktop = &fakeRemoteDesktop{}
			tt := ui.NewTesterWith(w.view, 1280, 820, ui.TesterOptions{Dark: dark})
			mustClick(t, tt, "远程桌面")
			tt.Advance(time.Second)
			w.access.status.Relay = "https://relay.other.example"
			tt.Frame()
			save(t, tt, "remote"+suffix)
		})
		t.Run("palette"+suffix, func(t *testing.T) {
			listing := api.EnvironmentDirectories{Path: "/home/me/Develop", Parent: "/home/me", Entries: []api.EnvironmentDirectory{
				{Name: "AI", Path: "/home/me/Develop/AI"}, {Name: "DemoApp", Path: "/home/me/Develop/DemoApp"}, {Name: "Docker", Path: "/home/me/Develop/Docker"},
				{Name: "Flutter", Path: "/home/me/Develop/Flutter"}, {Name: "IOS", Path: "/home/me/Develop/IOS"}, {Name: "Vantry", Path: "/home/me/Develop/Vantry"},
			}}
			service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
				data, _ := json.Marshal(listing)
				return api.WorkbenchResponse{Output: string(data)}, nil
			}}
			w := newWorkbench(service)
			w.defaults.RemotePeer = true
			w.environmentInfo.Home = "/home/me"
			w.newSession()
			tt := ui.NewTesterWith(w.view, 1280, 860, ui.TesterOptions{Dark: dark})
			w.openRemoteDirectory("/home/me/Develop")
			tt.Advance(time.Second)
			save(t, tt, "palette"+suffix)
		})
	}
}
