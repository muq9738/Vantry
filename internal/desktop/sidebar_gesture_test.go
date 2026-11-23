package desktop

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func sidebarGestureWorkbench() (*workbench, *fakeService) {
	w, service := projectWorkbench()
	w.selectThread("t2")
	w.widgets.themeSet, w.widgets.dark = true, true
	return w, service
}

func sidebarGestureRow(t *testing.T, tt *ui.Tester) ui.Rect {
	t.Helper()
	row, ok := tt.AccessByRole(ui.RoleTreeItem, "第二条线程")
	if !ok {
		t.Fatal("sidebar row missing")
	}
	return row.Bounds
}

func TestSidebarHoverPreviewWaitsAndDismisses(t *testing.T) {
	w, _ := sidebarGestureWorkbench()
	tt := ui.NewTester(w.view, 1399, 820)
	hoverRow(t, tt, "第二条线程")
	if _, ok := tt.Find("会话预览"); ok {
		t.Fatal("moving across a row must not immediately cover the conversation")
	}
	tt.Advance(500 * time.Millisecond)
	if _, ok := tt.Find("会话预览"); !ok {
		t.Fatal("a deliberate hover should reveal the thread preview")
	}
	tt.Move(900, 400)
	if _, ok := tt.Find("会话预览"); ok {
		t.Fatal("preview remained after leaving the sidebar")
	}
}

func TestSidebarClickDoesNotShowDragDestinations(t *testing.T) {
	w, service := sidebarGestureWorkbench()
	tt := ui.NewTester(w.view, 1399, 820)
	row := sidebarGestureRow(t, tt)
	x, y := row.X+50, row.Y+row.H/2
	tt.Press(x, y)
	tt.Move(x+2, y+1)
	tt.Release(x+2, y+1)
	if w.widgets.draggingThread {
		t.Fatal("ordinary row selection became a drag")
	}
	if _, ok := tt.Find("拖放会话"); ok {
		t.Fatal("drag destinations must not be permanent sidebar actions")
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("selecting a row changed its lifecycle")
	}
}

func TestSidebarDragHidesSourceAndDoesNotShiftFooter(t *testing.T) {
	w, _ := sidebarGestureWorkbench()
	tt := ui.NewTester(w.view, 1399, 820)
	row := sidebarGestureRow(t, tt)
	footer, _ := tt.Find("设置")
	x, y := row.X+45, row.Y+row.H/2
	tt.Press(x, y)
	tt.Move(x+80, y+90)
	if !w.widgets.draggingThread {
		t.Fatal("deliberate pointer movement did not start a drag")
	}
	if _, ok := tt.Find("拖放会话"); !ok {
		t.Fatal("drag destinations lack an explicit gesture label")
	}
	if _, ok := tt.Find("会话预览"); ok {
		t.Fatal("hover preview must not compete with a drag")
	}
	after, _ := tt.Find("设置")
	if after != footer {
		t.Fatalf("drag shifted the utility footer: before=%+v after=%+v", footer, after)
	}
	_, p := workbenchTheme(true, true, true)
	// The preview has moved entirely below the source. Its original selected
	// card must not remain painted underneath as a duplicate conversation.
	image := tt.Image()
	at := image.RGBAAt(int(row.X+row.W/2), int(row.Y+row.H/2))
	if at.R != p.sidebar.R || at.G != p.sidebar.G || at.B != p.sidebar.B {
		t.Fatalf("drag source still paints a second card: pixel=%v sidebar=%v", at, p.sidebar)
	}
}

func TestSidebarDragCancelAndReleaseClearTargets(t *testing.T) {
	for _, ending := range []string{"release", "escape", "window-cancel"} {
		t.Run(ending, func(t *testing.T) {
			w, service := sidebarGestureWorkbench()
			tt := ui.NewTester(w.view, 1399, 820)
			row := sidebarGestureRow(t, tt)
			x, y := row.X+45, row.Y+row.H/2
			tt.Press(x, y)
			tt.Move(x+80, y+90)
			switch ending {
			case "release":
				tt.Release(800, 400)
			case "escape":
				tt.Key(0, ui.KeyEscape)
				tt.Release(800, 400)
			case "window-cancel":
				tt.CancelPress()
			}
			tt.Move(900, 400)
			tt.Frame()
			if w.widgets.draggingThread {
				t.Fatal("dragging state survived the end of the gesture")
			}
			if _, ok := tt.Find("拖放会话"); ok {
				t.Fatal("drag destinations survived the end of the gesture")
			}
			if len(service.dispatched()) != 0 {
				t.Fatal("an abandoned drag changed thread state")
			}
		})
	}
}

func TestSidebarDragDestinationsDispatchOnlyOnDrop(t *testing.T) {
	for _, destination := range []string{"置顶", "归档"} {
		t.Run(destination, func(t *testing.T) {
			w, service := sidebarGestureWorkbench()
			tt := ui.NewTester(w.view, 1399, 820)
			row := sidebarGestureRow(t, tt)
			x, y := row.X+45, row.Y+row.H/2
			tt.Press(x, y)
			tt.Move(x+80, y+90)
			target, ok := tt.Find("拖放至" + destination)
			if !ok {
				t.Fatalf("missing %s drop target", destination)
			}
			x, y = target.X+target.W/2, target.Y+target.H/2
			tt.Move(x, y)
			if len(service.dispatched()) != 0 {
				t.Fatal("hovering a destination changed state before releasing")
			}
			tt.Release(x, y)
			commands := service.dispatched()
			if len(commands) != 1 || commands[0].Kind != api.UpdateThread || commands[0].ThreadID != "t2" {
				t.Fatalf("drop commands=%+v", commands)
			}
			if destination == "置顶" && (commands[0].Pinned == nil || !*commands[0].Pinned) {
				t.Fatal("pin target did not pin")
			}
			if destination == "归档" && (commands[0].Archived == nil || !*commands[0].Archived) {
				t.Fatal("settle target did not settle")
			}
			if w.widgets.draggingThread {
				t.Fatal("drop targets remained after committing the drop")
			}
		})
	}
}

// Real MyGo rendering, using only local fixtures. Export idle/drag/cancel frames
// for visual inspection without starting a window or invoking a model.
func TestSidebarGestureRenders(t *testing.T) {
	output := os.Getenv("VANTRY_SIDEBAR_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_SIDEBAR_RENDER_DIR to export native gesture frames")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dark := range []bool{true, false} {
		w, _ := sidebarGestureWorkbench()
		w.widgets.dark = dark
		tt := ui.NewTesterWith(w.view, 1399, 820, ui.TesterOptions{Dark: dark})
		row := sidebarGestureRow(t, tt)
		x, y := row.X+45, row.Y+row.H/2
		theme := "light"
		if dark {
			theme = "dark"
		}
		capture := func(scene string) {
			t.Helper()
			file, err := os.Create(filepath.Join(output, theme+"-"+scene+".png"))
			if err != nil {
				t.Fatal(err)
			}
			if err := png.Encode(file, tt.Image()); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		capture("idle")
		tt.Press(x, y)
		tt.Move(x+8, y+8)
		capture("drag-near-source")
		tt.Move(x+80, y+90)
		capture("drag-away")
		tt.Key(0, ui.KeyEscape)
		tt.Release(900, 400)
		tt.Move(900, 400)
		capture("cancel")
	}
}
