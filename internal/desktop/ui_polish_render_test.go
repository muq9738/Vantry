package desktop

import (
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

func savePNG(t *testing.T, tt *ui.Tester, dir, name string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = png.Encode(f, tt.Image()); err != nil {
		t.Fatal(err)
	}
}

func TestTimelineMinimapRender(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR")
	}
	var runs []api.Run
	for i := 0; i < 12; i++ {
		runs = append(runs, run(fmt.Sprintf("r%d", i), "t1", api.RunCompleted, fmt.Sprintf("第 %d 轮问题：梳理编排", i+1), "刚刚通过 `rclone` 核实了云盘，视频其实在 **`2026-11-17`** 已经上传到子目录里（按日期排在 `2026-11-18-*` 几个目录前面）：\n- **云盘路径**：`X/视频草稿/2026-11-17-mushenji-tea/r02.mp4`\n- **在线看片**：https://drive.google.com/file/d/xyz/view\n\n"+strings.Repeat("回复内容段落。\n\n", 5), time.Duration(i)*time.Minute))
	}
	for _, dark := range []bool{false, true} {
		w, _ := newViewWorkbench(runs...)
		tt := ui.NewTesterWith(w.view, 1280, 820, ui.TesterOptions{Dark: dark})
		tt.Advance(time.Second)
		name := map[bool]string{false: "minimap-light.png", true: "minimap-dark-hover.png"}[dark]
		if dark {
			if r, ok := tt.Find("跳到第 6 轮"); ok {
				tt.Move(r.X+r.W-2, r.Y+r.H/2)
				tt.Advance(time.Second)
			} else {
				t.Fatalf("minimap tick not found: %v", tt.Texts())
			}
		}
		savePNG(t, tt, output, name)
	}
}

func TestProviderCardsRender(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR")
	}
	for _, tc := range []struct {
		name string
		dark bool
	}{{"providers-light", false}, {"providers-dark", true}} {
		w := newWorkbench(newFakeQueryService())
		w.defaults.StatePath = filepath.Join(t.TempDir(), "desktop-state.json")
		w.providers.health = &piHealth{OK: true, Version: "1.5.2", Detail: "Pi CLI 1.5.2 · 2 个模型就绪", ModelCount: 2}
		w.applySnapshot(api.Snapshot{})
		w.widgets.settingsOpen = true
		w.widgets.settingsPage = "providers"
		tt := ui.NewTesterWith(w.view, 1180, 900, ui.TesterOptions{Dark: tc.dark})
		tt.Advance(time.Second)
		savePNG(t, tt, output, tc.name+".png")
	}
}

func TestReplyTableRender(t *testing.T) {
	output := os.Getenv("VANTRY_DESKTOP_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_DESKTOP_RENDER_DIR")
	}
	reply := "对比如下：\n\n| 维度 | Vantry | t3code | Claude Desktop |\n|---|---|---|---|\n" +
		"| 跨机派发与统一身份 | ✅ 强（environmentId 贯通、TLS/中继/SSH、T3 协议互通） | ✅ 强 | ◐ 仅本机 |\n" +
		"| 父子任务合同（交付、追问、级联停止） | ✅ **最完整** | ◐ 并行 + 手动汇总 | — |\n" +
		"| checkpoint / 回滚 / fork | ✕（文档自认未完成） | — | ✅ `checkpoint` |\n" +
		"| 分发成熟度 | ✕ 未公证、Windows 预览未验证、无 Linux | ✅ 三端 | ✅ 三端 |\n\n（— 表示公开页未说明）"
	for _, tc := range []struct {
		name  string
		width int
		dark  bool
	}{{"table-wide-dark", 1280, true}, {"table-narrow-light", 820, false}} {
		w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "对比一下", reply, 0))
		tt := ui.NewTesterWith(w.view, tc.width, 820, ui.TesterOptions{Dark: tc.dark})
		tt.Advance(time.Second)
		if !tt.HasText("分发成熟度") {
			t.Fatalf("table text missing: %v", tt.Texts())
		}
		savePNG(t, tt, output, tc.name+".png")
	}
}

func TestSentImageOpensLightbox(t *testing.T) {
	r := run("r1", "t1", api.RunCompleted, "看图", "好的", 0)
	r.Attachments = []api.Attachment{{ID: "a1", Name: "shot-1.png", MIME: "image/png", Size: 10}, {ID: "a2", Name: "shot-2.png", MIME: "image/png", Size: 10}}
	w, _ := newViewWorkbench(r)
	tt := ui.NewTester(w.view, 1180, 820)
	tt.Advance(time.Second)
	mustClick(t, tt, "查看图片 shot-2.png")
	tt.Frame()
	if !tt.HasText("2 / 2 · shot-2.png") {
		t.Fatalf("lightbox not shown: %v", tt.Texts())
	}
	mustClick(t, tt, "下一张")
	tt.Frame()
	if !tt.HasText("1 / 2 · shot-1.png") {
		t.Fatalf("lightbox did not step: %v", tt.Texts())
	}
	mustClick(t, tt, "关闭")
	tt.Frame()
	if len(w.visuals.lightbox) != 0 {
		t.Fatal("lightbox stayed open")
	}
}
