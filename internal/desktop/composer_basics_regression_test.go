package desktop

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestSkillPickerWorksBeforeChoosingProject(t *testing.T) {
	w, service := newSkillsWorkbench()
	service.byDir[""] = []skillEntry{{Name: "global-docs", Description: "Global skill", Command: "skill:global-docs"}}
	w.defaults.Workspace = nil
	w.newSession()
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("/global")
	if !tt.HasText("global-docs") {
		t.Fatalf("projectless draft cannot choose global skills: %v", tt.Texts())
	}
	tt.Key(0, ui.KeyEnter)
	if comp := w.composerFor(w.selected); comp.draft != "/global-docs " || comp.outgoing() != "/skill:global-docs " {
		got := comp.draft + " → " + comp.outgoing()
		t.Fatalf("global skill selection = %q", got)
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("choosing a skill submitted the projectless draft")
	}
	requests := service.skillRequests()
	if len(requests) != 1 || requests[0].ThreadID != "" || requests[0].Workspace != "" {
		t.Fatalf("global skill query used another thread or workspace: %+v", requests)
	}
}

func TestSkillMenuEnterDuringLoadingNeverSends(t *testing.T) {
	w, service := newSkillsWorkbench()
	w.start = func(func()) {} // Leave Core requests in flight, as on a slow link.
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("/docx")
	tt.Key(0, ui.KeyEnter)
	if got := w.composerFor("t1").draft; got != "/docx" {
		t.Fatalf("Enter changed a still-loading skill token: %q", got)
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("Enter sent the token while its skill menu was still loading")
	}
}

func TestComposerIMEConfirmationDoesNotPickSkill(t *testing.T) {
	w, service := newSkillsWorkbench()
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("/d")
	w.observeComposerInput("t1", ui.InputEvent{Kind: ui.InputCompose, Text: "zhong"})
	tt.Key(0, ui.KeyEnter)
	tt.Key(ui.Super, ui.KeyEnter)
	if got := w.composerFor("t1").draft; got != "/d" {
		t.Fatalf("IME confirmation chose a skill or inserted a newline: %q", got)
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("IME confirmation sent a message")
	}
}

func TestComposerPlainEnterSendsByDefault(t *testing.T) {
	w, service := newSkillsWorkbench()
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("一条完整消息")
	tt.Key(0, ui.KeyEnter)
	commands := service.dispatched()
	if len(commands) != 1 || commands[0].Message != "一条完整消息" {
		t.Fatalf("plain Enter did not submit exactly one complete message: %+v", commands)
	}
}
