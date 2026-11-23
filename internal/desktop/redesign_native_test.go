package desktop

import (
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func overlaps(a, b ui.Rect) bool {
	return a.X < b.X+b.W-0.5 && b.X < a.X+a.W-0.5 && a.Y < b.Y+b.H-0.5 && b.Y < a.Y+a.H-0.5
}

func TestLocaleSwitchesLiveAndPersists(t *testing.T) {
	state := filepath.Join(t.TempDir(), "desktop-state.json")
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	w.defaults.StatePath = state
	tt := ui.NewTester(w.view, 1280, 820)
	for _, text := range []string{"设置", "用量", "已归档"} {
		if !tt.HasText(text) {
			t.Fatalf("default locale must be Chinese; missing %q in %q", text, tt.Texts())
		}
	}
	if info, ok := tt.Access("搜索会话"); !ok || (!strings.Contains(info.Placeholder, "⌘K") && !tt.HasText("⌘K")) {
		t.Fatalf("search must be always visible with its shortcut hint: %+v %v", info, ok)
	}
	// The 中 / EN capsule is gone from the sidebar footer: language lives in Settings.
	for _, gone := range []string{"切换为 English", "切换为简体中文"} {
		if _, ok := tt.Access(gone); ok {
			t.Fatalf("locale capsule %q is still in the sidebar", gone)
		}
	}
	if tt.HasText("EN") {
		t.Fatalf("sidebar footer still shows the language capsule: %q", tt.Texts())
	}
	mustClick(t, tt, "设置")
	// Language uses a Select dropdown offering 简体中文, English, and 日本語.
	mustClick(t, tt, "简体中文")
	for _, option := range []string{"简体中文", "English", "日本語"} {
		if !tt.HasText(option) {
			t.Fatalf("locale select popup missing %q: %q", option, tt.Texts())
		}
	}
	mustClick(t, tt, "English")
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	for _, text := range []string{"Settings", "Usage", "Settled", "New thread", "Search threads", "Choose model", "Permissions", "Plan mode"} {
		if !tt.HasText(text) {
			t.Errorf("English UI missing %q: %q", text, tt.Texts())
		}
	}
	for _, text := range []string{"设置", "用量", "已归档", "消息输入框"} {
		if tt.HasText(text) {
			t.Errorf("Chinese %q left in English UI", text)
		}
	}
	restored := newWorkbench(&fakeService{})
	restored.defaults.StatePath = state
	if err := restored.restoreState(); err != nil || !restored.english() {
		t.Fatalf("locale not persisted: err=%v locale=%q", err, restored.widgets.locale)
	}
	mustClick(t, tt, "Settings")
	mustClick(t, tt, "English")
	mustClick(t, tt, "日本語")
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	for _, text := range []string{"設定", "使用量", "アーカイブ済み", "新規スレッド"} {
		if !tt.HasText(text) {
			t.Errorf("Japanese UI missing %q: %q", text, tt.Texts())
		}
	}
	restoredJA := newWorkbench(&fakeService{})
	restoredJA.defaults.StatePath = state
	if err := restoredJA.restoreState(); err != nil || !restoredJA.japanese() {
		t.Fatalf("Japanese locale not persisted: err=%v locale=%q", err, restoredJA.widgets.locale)
	}
	mustClick(t, tt, "設定")
	mustClick(t, tt, "日本語")
	mustClick(t, tt, "简体中文")
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if !tt.HasText("设置") || tt.HasText("Settings") || tt.HasText("設定") {
		t.Fatalf("switching back to Chinese failed: %q", tt.Texts())
	}
}

func TestSettingsDialogGroupsPreferences(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	tt := ui.NewTester(w.view, 1080, 760)
	mustClick(t, tt, "设置")
	if !w.widgets.settingsOpen {
		t.Fatal("settings did not open")
	}
	// Settings use section navigation beside one page of grouped rows.
	for _, text := range []string{"通用", "外观", "Agent 与权限", "环境", "关于与更新", "界面语言", "发送快捷键", "子任务自动续行"} {
		if !tt.HasText(text) {
			t.Errorf("settings missing %q", text)
		}
	}
	mustClick(t, tt, "外观")
	if !tt.HasText("跟随系统") {
		t.Fatalf("appearance page missing theme choices: %q", tt.Texts())
	}
	mustClick(t, tt, "浅色")
	if !w.widgets.themeSet || w.widgets.dark {
		t.Fatalf("theme not forced light: set=%v dark=%v", w.widgets.themeSet, w.widgets.dark)
	}
	mustClick(t, tt, "Agent 与权限")
	if !tt.HasText("新会话默认工具权限") {
		t.Fatalf("agent page missing permissions: %q", tt.Texts())
	}
	mustClick(t, tt, "自动改文件 · 命令询问")
	if w.defaults.ApprovalPolicy != "auto-accept-edits" {
		t.Fatalf("approval = %q", w.defaults.ApprovalPolicy)
	}
	mustClick(t, tt, "通用")
	mustClick(t, tt, "简体中文")
	mustClick(t, tt, "English")
	tt.Frame()
	mustClick(t, tt, "Agents & permissions")
	if !w.english() || !tt.HasText("Default tool permissions") {
		t.Fatalf("language switch inside settings: %q", tt.Texts())
	}
	mustClick(t, tt, "Environments")
	mustClick(t, tt, "Manage SSH environments")
	if w.widgets.settingsOpen || w.features.panel != "terminal" || w.termTab.active != terminalHostsTab {
		t.Fatalf("remote entry: open=%v panel=%q", w.widgets.settingsOpen, w.features.panel)
	}
	tt.Key(ui.Cmd, ui.KeyComma)
	if !w.widgets.settingsOpen {
		t.Fatal("⌘, did not open settings")
	}
}

func TestUsageDialogUsesCoreSummaryAndModelBars(t *testing.T) {
	var requests []api.WorkbenchRequest
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if r.Action == "models" { // background thinking-level preload
			return api.WorkbenchResponse{}, nil
		}
		requests = append(requests, r)
		raw, _ := json.Marshal(map[string]any{"threads": 12, "runs": 40, "activeRuns": 2, "completedRuns": 35, "failedRuns": 3,
			"models": []map[string]any{{"model": "magpie/deepseek-v4.1-flash", "runs": 30, "completed": 28, "outputChars": 52000}, {"model": "kimi/kimi-for-coding", "runs": 10, "completed": 7, "outputChars": 9000}}})
		return api.WorkbenchResponse{Output: string(raw)}, nil
	}}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("t1", "Thread", 0)}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "q", "a", 0)}})
	tt := ui.NewTester(w.view, 1280, 820)
	mustClick(t, tt, "用量")
	tt.Frame()
	if len(requests) != 1 || requests[0].Action != "usage.summary" {
		t.Fatalf("usage request = %+v", requests)
	}
	for _, name := range []string{"总会话 12", "总运行轮次 40", "已完成 35", "进行中 2", "magpie/deepseek-v4.1-flash", "kimi/kimi-for-coding"} {
		if _, ok := tt.Access(name); !ok {
			t.Errorf("usage missing %q: %q", name, tt.Texts())
		}
	}
	first, _ := tt.Access("magpie/deepseek-v4.1-flash")
	second, _ := tt.Access("kimi/kimi-for-coding")
	if first.Bounds.Y >= second.Bounds.Y {
		t.Fatal("models must be ordered by turns")
	}
	tt.Key(0, ui.KeyEscape)
	if w.widgets.usageOpen {
		t.Fatal("Escape did not close usage")
	}
}

func TestUsageFallsBackToThisWindow(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0), run("r2", "t1", api.RunRunning, "继续", "", time.Minute))
	w.threads[0].Model = "pi/model-x"
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "用量")
	tt.Frame()
	for _, name := range []string{"总会话 1", "总运行轮次 2", "已完成 1", "进行中 1", "pi/model-x"} {
		if _, ok := tt.Access(name); !ok {
			t.Errorf("local usage missing %q: %q", name, tt.Texts())
		}
	}
}

func TestSettledShelfFoldsAndUnsettles(t *testing.T) {
	w, service := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	old := thread("t2", "旧会话", time.Minute)
	old.Archived = true
	w.mergeThread(old)
	w.mergeRun(run("r2", "t2", api.RunCompleted, "旧会话", "", time.Minute))
	tt := ui.NewTester(w.view, 1280, 820)
	if tt.HasText("旧会话") {
		t.Fatal("settled shelf must start folded")
	}
	mustClick(t, tt, "已归档")
	tt.Frame()
	if _, ok := tt.AccessByRole(ui.RoleTreeItem, "旧会话"); !ok {
		t.Fatalf("expanded shelf missing settled thread: %q", tt.Texts())
	}
	if err := tt.RightClick("旧会话"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("移回活跃"); err != nil {
		t.Fatalf("unsettle: %v menu=%q", err, tt.Menu())
	}
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].ThreadID != "t2" || commands[0].Archived == nil || *commands[0].Archived {
		t.Fatalf("unsettle command = %+v", commands)
	}
	if err := tt.RightClick("第一条线程"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("归档会话"); err != nil {
		t.Fatalf("settle: %v menu=%q", err, tt.Menu())
	}
	commands = service.dispatched()
	if len(commands) != 2 || commands[1].ThreadID != "t1" || commands[1].Archived == nil || !*commands[1].Archived {
		t.Fatalf("settle command = %+v", commands)
	}
}

func TestSidebarRowsShowBranchAndProjectActions(t *testing.T) {
	w := nativeReviewWorkbench()
	tt := ui.NewTester(w.view, 1440, 900)
	row, ok := tt.AccessByRole(ui.RoleTreeItem, "原生交互 · 模型选择与输入体验")
	if !ok || !strings.Contains(row.Description, "desktop-interactions") {
		t.Fatalf("thread row lacks its branch: %+v %v", row, ok)
	}
	// The flat list carries the project's actions on the row itself.
	if err := tt.RightClick("原生交互 · 模型选择与输入体验"); err != nil {
		t.Fatalf("row menu: %v", err)
	}
	if err := tt.ChooseMenuItem("在此项目新建会话"); err != nil {
		t.Fatalf("project item: %v menu=%q", err, tt.Menu())
	}
	tt.Frame()
	if !isLocalSession(w.selected) || w.defaults.Workspace == nil || w.defaults.Workspace.Path == "" {
		t.Fatalf("project + did not start a project thread: selected=%q ws=%+v", w.selected, w.defaults.Workspace)
	}
}

func TestComposerIsMinimalWithContextStripAtEverySize(t *testing.T) {
	for _, size := range [][2]int{{1440, 900}, {1080, 760}, {640, 600}} {
		for _, locale := range []string{localeZH, localeEN} {
			t.Run(fmt.Sprintf("%dx%d-%s", size[0], size[1], locale), func(t *testing.T) {
				w := nativeReviewWorkbench()
				w.runs[0].Status = api.RunCompleted
				w.widgets.locale = locale
				tt := ui.NewTester(w.view, size[0], size[1])
				name := func(zh, en string) string { return w.tr(zh, en) }
				footer := []string{name("添加附件", "Add attachment"), name("规划模式", "Plan mode"), name("选择模型", "Choose model"), name("思考强度", "Thinking effort"), name("发送", "Send")}
				var rects []ui.Rect
				for _, control := range footer {
					assertControlInWindow(t, tt, control, size[0], size[1])
					info, _ := tt.Access(control)
					rects = append(rects, info.Bounds)
				}
				for i := range rects {
					for j := i + 1; j < len(rects); j++ {
						if overlaps(rects[i], rects[j]) {
							t.Errorf("%q overlaps %q: %+v %+v", footer[i], footer[j], rects[i], rects[j])
						}
					}
				}
				// The removed controls stay out of the card.
				for _, gone := range []string{"执行设置", "选择项目目录", "派出子任务", "切换发送快捷键"} {
					if _, ok := tt.Access(gone); ok {
						t.Errorf("%q is back in the composer", gone)
					}
				}
				field, _ := tt.Access(name("消息输入框", "Message"))
				project := name("项目与工作区", "Project & workspace")
				assertControlInWindow(t, tt, project, size[0], size[1])
				strip, _ := tt.Access(project)
				if strip.Bounds.Y < field.Bounds.Y+field.Bounds.H || strip.Bounds.H > 24.5 {
					t.Errorf("context strip must sit under the card: field=%+v strip=%+v", field.Bounds, strip.Bounds)
				}
				branch := name("分支 ", "Branch ") + "desktop-interactions"
				assertControlInWindow(t, tt, branch, size[0], size[1])
				if b, _ := tt.Access(branch); overlaps(b.Bounds, strip.Bounds) {
					t.Errorf("branch overlaps project: %+v %+v", b.Bounds, strip.Bounds)
				}
				if !strings.Contains(strip.Description, name("本地目录", "Local")) {
					t.Errorf("strip does not say Local: %q", strip.Description)
				}
				// Permissions sit as a subtle tinted pill inside the composer footer
				// beside the unified model/effort pill, never overlapping other controls.
				permissions := name("工具权限", "Permissions")
				assertControlInWindow(t, tt, permissions, size[0], size[1])
				perm, _ := tt.Access(permissions)
				if perm.Bounds.Y < field.Bounds.Y+field.Bounds.H || perm.Bounds.H > 28 {
					t.Errorf("permissions must sit in the composer footer: field=%+v chip=%+v", field.Bounds, perm.Bounds)
				}
				b, _ := tt.Access(branch)
				for _, other := range append([]ui.Rect{strip.Bounds, b.Bounds}, rects...) {
					if overlaps(perm.Bounds, other) {
						t.Errorf("permissions overlap another control: %+v %+v", perm.Bounds, other)
					}
				}
				for _, control := range []string{name("设置", "Settings"), name("用量", "Usage"), name("切换浅色", "Light appearance"), name("切换深色", "Dark appearance")} {
					if _, ok := tt.Access(control); ok {
						assertControlInWindow(t, tt, control, size[0], size[1])
					}
				}
				if size[0] >= 760 {
					for _, tool := range headerTools {
						assertControlInWindow(t, tt, name(tool.zh, tool.en), size[0], size[1])
					}
				} else {
					assertControlInWindow(t, tt, name("工具", "Tools"), size[0], size[1])
				}
			})
		}
	}
}

func TestHeaderToolsOpenDrawersWithoutToolstrip(t *testing.T) {
	w := nativeReviewWorkbench()
	w.runs[0].Status = api.RunCompleted
	tt := ui.NewTester(w.view, 1440, 900)
	if _, ok := tt.Access("本机"); ok {
		t.Fatal("the old toolstrip is still rendered")
	}
	mustClick(t, tt, "Git")
	if w.features.panel != "git" {
		t.Fatalf("panel=%q", w.features.panel)
	}
	tt.Frame()
	tt.Key(0, ui.KeyEscape)
	if w.features.panel != "" {
		t.Fatal("Escape did not close the drawer")
	}
	tt.SetSize(640, 600)
	tt.Frame()
	mustClick(t, tt, "工具")
	mustClick(t, tt, "插件")
	if w.features.panel != "plugins" {
		t.Fatalf("narrow tools menu panel=%q", w.features.panel)
	}
}

func TestPermissionChipSwitchesTiers(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "问题", "回答", 0))
	tt := ui.NewTester(w.view, 1080, 760)
	mustClick(t, tt, "工具权限")
	mustClick(t, tt, "每次询问")
	if w.defaults.ApprovalPolicy != "ask" {
		t.Fatalf("chip approval = %q", w.defaults.ApprovalPolicy)
	}
	if info, _ := tt.Access("工具权限"); !strings.Contains(info.Description, "每次询问") {
		t.Fatalf("chip description = %q", info.Description)
	}
}

// TestRedesignRenders exports the Codex / Qoder palettes in both languages.
func TestRedesignRenders(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR to export native review images")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{1440, 900}, {1080, 760}, {640, 600}} {
		for _, dark := range []bool{true, false} {
			for _, locale := range []string{localeZH, localeEN} {
				for _, scene := range []string{"thread", "home", "settings", "usage", "settled"} {
					theme := "light"
					if dark {
						theme = "dark"
					}
					name := fmt.Sprintf("workbench-%d-%s-%s-%s", size[0], theme, locale, scene)
					t.Run(name, func(t *testing.T) {
						w := nativeReviewWorkbench()
						w.widgets.locale = locale
						w.widgets.themeSet, w.widgets.dark = true, dark
						settled := thread("old", "已归拢的旧会话", 2*time.Hour)
						settled.Archived = true
						w.mergeThread(settled)
						w.mergeRun(run("r-old", "old", api.RunCompleted, "旧问题", "旧回答", 2*time.Hour))
						w.sessions.unread = map[string]bool{"other": true}
						switch scene {
						case "home":
							w.newSession()
						case "settings":
							w.widgets.settingsOpen = true
						case "usage":
							w.widgets.usageOpen = true
						case "settled":
							*w.projectExpanded(sectionSettled) = true
							w.widgets.search = ""
						}
						tt := ui.NewTesterWith(w.view, size[0], size[1], ui.TesterOptions{Dark: dark})
						tt.Advance(time.Second)
						file, err := os.Create(filepath.Join(output, name+".png"))
						if err != nil {
							t.Fatal(err)
						}
						defer file.Close()
						if err := png.Encode(file, tt.Image()); err != nil {
							t.Fatal(err)
						}
					})
				}
			}
		}
	}
}

func TestUsageDialogShowsRealTokensAndCostWhenAvailable(t *testing.T) {
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if r.Action == "models" {
			return api.WorkbenchResponse{}, nil
		}
		raw, _ := json.Marshal(map[string]any{
			"threads": 2, "runs": 5, "activeRuns": 0, "completedRuns": 5, "failedRuns": 0,
			"inputTokens": 8000, "outputTokens": 3000, "cacheReadTokens": 4000, "totalTokens": 15000,
			"cacheHitRate": 0.5, "totalCostUsd": 0.042, "avgTtftMs": 600,
			"models": []map[string]any{
				{"model": "magpie/deepseek-v4", "runs": 5, "completed": 5, "totalTokens": 15000, "costUsd": 0.042},
			},
		})
		return api.WorkbenchResponse{Output: string(raw)}, nil
	}}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("t1", "Thread", 0)}, Runs: []api.Run{run("r1", "t1", api.RunCompleted, "q", "a", 0)}})
	tt := ui.NewTester(w.view, 1280, 820)
	mustClick(t, tt, "用量")
	tt.Frame()

	joined := strings.Join(tt.Texts(), "\n")
	for _, want := range []string{"Tokens 15k", "缓存命中 50%", "$0.042", "0.6s", "15k tok"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("usage dialog missing %q in:\n%s", want, joined)
		}
	}
}
