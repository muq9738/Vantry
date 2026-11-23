package desktop

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

type remoteKeyRecord struct {
	kind      string
	code      int
	text      string
	modifiers uint32
}

func TestRemoteKeyProtocolUsesMacCodesAndCGFlags(t *testing.T) {
	driver := &fakeRemoteDesktop{}
	w := newWorkbench(&fakeService{})
	w.defaults.RemoteDesktop = driver
	w.rd.connected = true
	w.remoteInput(ui.InputEvent{Kind: ui.InputKeyDown, Key: ui.KeyEnter, Code: ui.CodeEnter})
	w.remoteInput(ui.InputEvent{Kind: ui.InputKeyUp, Key: ui.KeyEnter, Code: ui.CodeEnter})
	w.remoteInput(ui.InputEvent{Kind: ui.InputKeyDown, Key: ui.KeyC, Code: ui.CodeKeyC, Mods: ui.Super})
	w.remoteInput(ui.InputEvent{Kind: ui.InputKeyUp, Key: ui.KeyC, Code: ui.CodeKeyC})
	if len(driver.keyEvents) != 4 || driver.keyEvents[0].code != 36 || driver.keyEvents[2].code != 8 || driver.keyEvents[2].modifiers != 1<<20 || driver.keyEvents[3].kind != "up" {
		t.Fatal("remote key events used local enum ordinals instead of the Mac protocol")
	}
}
func TestRemoteUnicodeIsCommittedOnceAndPreeditStaysLocal(t *testing.T) {
	driver := &fakeRemoteDesktop{}
	w := newWorkbench(&fakeService{})
	w.defaults.RemoteDesktop = driver
	w.rd.connected = true
	if w.remoteInput(ui.InputEvent{Kind: ui.InputKeyDown, Key: ui.KeyA, Code: ui.CodeKeyA}) {
		t.Fatal("printable key prevented the local input method")
	}
	w.remoteInput(ui.InputEvent{Kind: ui.InputCompose, Text: "zhong"})
	if len(driver.keyEvents) != 0 {
		t.Fatal("uncommitted composition reached the other machine")
	}
	w.remoteInput(ui.InputEvent{Kind: ui.InputText, Text: "中文🙂"})
	if len(driver.keyEvents) != 2 || driver.keyEvents[0].kind != "down" || driver.keyEvents[0].text != "中文🙂" || driver.keyEvents[1].kind != "up" {
		t.Fatal("committed Unicode was dropped, duplicated, or sent as an unsupported event kind")
	}
}
func TestRemoteHeldInputsAreReleasedOnFocusLoss(t *testing.T) {
	driver := &fakeRemoteDesktop{}
	w := newWorkbench(&fakeService{})
	w.defaults.RemoteDesktop = driver
	w.rd.connected = true
	w.rd.frameW, w.rd.frameH, w.rd.viewW, w.rd.viewH = 100, 100, 100, 100
	w.remoteInput(ui.InputEvent{Kind: ui.InputKeyDown, Key: ui.KeyC, Code: ui.CodeKeyC, Mods: ui.Super})
	w.remoteInput(ui.InputEvent{Kind: ui.InputPointerDown, X: 25, Y: 25, Button: 0})
	w.remoteInput(ui.InputEvent{Kind: ui.InputPointerMove, X: 50, Y: 60})
	w.releaseRemoteKeys()
	if len(w.rd.heldKeys) != 0 || len(w.rd.heldButtons) != 0 || driver.keyEvents[len(driver.keyEvents)-1].kind != "up" || driver.mouse[len(driver.mouse)-1] != "up:0.5,0.6" {
		t.Fatal("leaving the viewer retained a remote key or button")
	}
	foundDrag := false
	for _, event := range driver.mouse {
		if event == "drag:0.5,0.6" {
			foundDrag = true
		}
	}
	if !foundDrag {
		t.Fatal("pressed-button motion was sent as an ordinary mouse move")
	}
}
func TestRemoteViewerOwnsCommandNInsteadOfCreatingLocalThread(t *testing.T) {
	w, _ := v3Workbench(t, nil)
	driver := &fakeRemoteDesktop{}
	w.defaults.RemoteDesktop = driver
	remoteDesktopPluginFixture(t, w)
	w.openRemoteDesktop("test-viewer")
	if !w.connectRemoteDesktop() {
		t.Fatal("fake viewer did not connect")
	}
	tt := ui.NewTester(w.view, 1440, 900)
	viewport, ok := tt.Access("远程桌面画面")
	if !ok {
		t.Fatal("viewport missing")
	}
	tt.ClickAt(viewport.Bounds.X+viewport.Bounds.W/2, viewport.Bounds.Y+viewport.Bounds.H/2)
	before := w.selected
	drafts := len(w.localOrder)
	tt.Key(ui.Cmd, ui.KeyN)
	if w.selected != before || len(w.localOrder) != drafts {
		t.Fatal("a viewer shortcut created local work")
	}
	found := false
	for _, event := range driver.keyEvents {
		if event.kind == "down" && event.code == 45 && event.modifiers == 1<<20 {
			found = true
		}
	}
	if !found {
		t.Fatal("Command+N never reached the remote key protocol")
	}
}
