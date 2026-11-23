package desktop

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestUserMessageCopyButtonPreservesAttachmentPosition(t *testing.T) {
	message := "先检查 [🖼 screenshot.png]，然后看这段文字。"
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, message, "收到", 0))
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	button, ok := tt.AccessByRole(ui.RoleButton, "复制消息")
	if !ok {
		t.Fatal("copy user message action missing")
	}
	tt.ClickAt(button.Bounds.X+button.Bounds.W/2, button.Bounds.Y+button.Bounds.H/2)
	if got, want := tt.Clipboard(), "先检查 @screenshot.png，然后看这段文字。"; got != want {
		t.Fatalf("copied = %q, want %q", got, want)
	}
}

func TestConversationSelectionSpansUserAndAssistant(t *testing.T) {
	w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "用户消息可以选中复制", "助手回复可以选中复制", 0))
	tt := ui.NewTester(w.view, 1440, 900)
	tt.Advance(time.Second)
	user, ok := tt.AccessByRole(ui.RoleText, "用户消息可以选中复制")
	if !ok {
		t.Fatal("user text missing")
	}
	assistant, ok := tt.AccessByRole(ui.RoleText, "助手回复可以选中复制")
	if !ok {
		t.Fatal("assistant text missing")
	}
	a, b := user.Bounds, assistant.Bounds
	tt.Press(a.X, a.Y+a.H/2)
	// Markdown's paragraph spans the reading column; end the drag over the
	// actual glyphs rather than the blank margin at its far right.
	endX := min(b.X+b.W, b.X+a.W+12)
	tt.Move(endX, b.Y+b.H/2)
	tt.Release(endX, b.Y+b.H/2)
	tt.Key(ui.Cmd, ui.KeyC)
	got := tt.Clipboard()
	if !strings.Contains(got, "用户消息可以选中复制") || !strings.Contains(got, "助手回复可以选中复制") {
		t.Fatalf("cross-message selection lost text: %q", got)
	}
}

func TestConversationTextDragAndCopy(t *testing.T) {
	for _, content := range []struct{ name, text string }{
		{"user", "用户消息可以选中复制"},
		{"assistant", "助手回复可以选中复制"},
	} {
		t.Run(content.name, func(t *testing.T) {
			w, _ := newViewWorkbench(run("r1", "t1", api.RunCompleted, "用户消息可以选中复制", "助手回复可以选中复制", 0))
			tt := ui.NewTester(w.view, 1440, 900)
			tt.Advance(time.Second)
			info, ok := tt.AccessByRole(ui.RoleText, content.text)
			if !ok {
				t.Fatal("message text not found")
			}
			b := info.Bounds
			tt.Press(b.X+1, b.Y+b.H/2)
			tt.Move(b.X+b.W-1, b.Y+b.H/2)
			tt.Release(b.X+b.W-1, b.Y+b.H/2)
			tt.Key(ui.Cmd, ui.KeyC)
			if !strings.Contains(tt.Clipboard(), "可以选中复制") {
				t.Fatalf("drag and Cmd+C did not copy %s text: %q bounds=%+v", content.name, tt.Clipboard(), b)
			}
		})
	}
}
