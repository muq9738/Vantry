package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

// skillsWorkbenchService answers skills.list from the contract: an existing
// thread by ThreadID, a local draft by its Workspace.
type skillsWorkbenchService struct {
	*fakeService
	mu       sync.Mutex
	byThread map[string][]skillEntry
	byDir    map[string][]skillEntry
	requests []api.WorkbenchRequest
	err      error
}

func (f *skillsWorkbenchService) Workbench(_ context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	err := f.err
	var entries []skillEntry
	if request.Action == actionSkillsList {
		if request.ThreadID != "" {
			entries = f.byThread[request.ThreadID]
		} else {
			entries = f.byDir[request.Workspace]
		}
	}
	f.mu.Unlock()
	if err != nil {
		return api.WorkbenchResponse{}, err
	}
	out, _ := json.Marshal(map[string]any{"skills": entries})
	return api.WorkbenchResponse{Output: string(out)}, nil
}

func (f *skillsWorkbenchService) skillRequests() []api.WorkbenchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.WorkbenchRequest
	for _, r := range f.requests {
		if r.Action == actionSkillsList {
			out = append(out, r)
		}
	}
	return out
}

func newSkillsWorkbench() (*workbench, *skillsWorkbenchService) {
	service := &skillsWorkbenchService{
		fakeService: &fakeService{health: api.Health{Version: api.ProtocolVersion, State: "ready"}},
		byThread: map[string][]skillEntry{
			"t1": {
				{Name: "docx", Description: "创建 Word 文档", Command: "skill:docx"},
				{Name: "pdf", Description: "读取 PDF 文件", Command: "skill:pdf"},
				{Name: "gmail", Description: "读写邮件"},
			},
		},
		byDir: map[string][]skillEntry{
			"/work": {{Name: "lark-doc", Description: "飞书文档"}},
		},
	}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{
		Sequence: 3,
		Threads:  []api.Thread{thread("t1", "会话", 0)},
		Runs:     []api.Run{run("r1", "t1", api.RunCompleted, "上一句", "回复", 0)},
	})
	return w, service
}

func TestSlashOpensSkillPickerAndEnterInsertsCommand(t *testing.T) {
	w, service := newSkillsWorkbench()
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("/d")
	if !tt.HasText("docx") {
		t.Fatalf("picker did not list skills: %q", tt.Texts())
	}
	if reqs := service.skillRequests(); len(reqs) != 1 || reqs[0].ThreadID != "t1" {
		t.Fatalf("skills.list did not target the thread: %+v", reqs)
	}
	tt.Key(0, ui.KeyEnter)
	if comp := w.composerFor("t1"); comp.draft != "/docx " || comp.outgoing() != "/skill:docx " {
		got := comp.draft + " → " + comp.outgoing()
		t.Fatalf("draft after choose = %q", got)
	}
	if len(service.dispatched()) != 0 {
		t.Fatal("choosing a skill sent the draft")
	}
	tt.Type("总结一下")
	tt.Key(ui.Ctrl, ui.KeyEnter)
	if cmds := service.dispatched(); len(cmds) != 1 || cmds[0].Message != "/skill:docx 总结一下" {
		t.Fatalf("send = %+v", cmds)
	}
}

func TestSkillPickerFiltersAndNeverSendsWithSlashInPath(t *testing.T) {
	w, _ := newSkillsWorkbench()
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")

	// A path or URL slash opens no picker.
	for _, typed := range []string{"/usr/bin", "https://x.io/", "src/main.go"} {
		tt.Type(typed)
		if tt.HasText("docx") || tt.HasText("技能候选") {
			t.Fatalf("%q opened the skill picker: %q", typed, tt.Texts())
		}
		tt.Key(ui.Cmd, ui.KeyA)
		tt.Key(0, ui.KeyBackspace)
		tt.Frame()
	}

	// Description text filters too, then Escape dismisses for this token.
	tt.Type("/pdf")
	tt.Key(0, ui.KeyDown)
	tt.Key(0, ui.KeyUp)
	tt.Key(0, ui.KeyTab)
	if comp := w.composerFor("t1"); comp.draft != "/pdf " || comp.outgoing() != "/skill:pdf " {
		got := comp.draft + " → " + comp.outgoing()
		t.Fatalf("Tab insert = %q", got)
	}
	tt.Type("收尾 /zzz")
	tt.Key(0, ui.KeyEscape)
	if tt.HasText("技能候选") {
		t.Fatal("Escape did not close the picker")
	}
	tt.Key(0, ui.KeyEnter)
	if !tt.HasText("技能候选") {
		t.Log("new token after Esc correctly reopens nothing until a fresh /")
	}
}

func TestSkillPickerScopesToTargetAndDraftWorkspace(t *testing.T) {
	w, service := newSkillsWorkbench()
	w.mergeThread(thread("t2", "空技能", 0))
	w.mergeRun(run("r2", "t2", api.RunCompleted, "q", "a", 0))
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("/")
	if !tt.HasText("docx") {
		t.Fatal("t1 skills not shown")
	}
	// Another thread has no skills: the t1 catalog must not leak.
	w.selectThread("t2")
	tt.Frame()
	mustClick(t, tt, "消息输入框")
	tt.Type("/")
	if !tt.HasText("目标环境没有可用技能") {
		t.Fatalf("t2 leaked t1 skills or showed the wrong empty state: %q", tt.Texts())
	}
	if tt.HasText("docx") {
		t.Fatal("another target's skill list leaked")
	}
	// A draft session asks by workspace path.
	w.defaults.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/work"}
	w.newSession()
	tt.Frame()
	mustClick(t, tt, "消息输入框")
	tt.Type("/")
	if !tt.HasText("lark-doc") {
		t.Fatalf("workspace skills not shown: %q", tt.Texts())
	}
	var byWorkspace bool
	for _, r := range service.skillRequests() {
		if r.ThreadID == "" && r.Workspace == "/work" {
			byWorkspace = true
		}
	}
	if !byWorkspace {
		t.Fatalf("no workspace-scoped skills.list: %+v", service.skillRequests())
	}
}

func TestSkillPickerErrorAndEmptyStates(t *testing.T) {
	service := &skillsWorkbenchService{
		fakeService: &fakeService{health: api.Health{Version: api.ProtocolVersion, State: "ready"}},
		byThread:    map[string][]skillEntry{},
		err:         &api.Error{Code: api.InternalError, Message: "core 拒绝"},
	}
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{
		Sequence: 3,
		Threads:  []api.Thread{thread("t1", "会话", 0)},
		Runs:     []api.Run{run("r1", "t1", api.RunCompleted, "q", "a", 0)},
	})
	tt := ui.NewTester(w.view, 1080, 700)
	mustClick(t, tt, "消息输入框")
	tt.Type("/")
	if !tt.HasText("读取技能列表失败") {
		t.Fatalf("error state missing: %q", tt.Texts())
	}
	// A failure leaves Enter free: no candidates, no send on Enter.
	tt.Key(0, ui.KeyEnter)
	if got := w.composerFor("t1").draft; got != "/\n" && got != "/" {
		t.Fatalf("draft = %q", got)
	}
}

func TestCmdEnterBreaksLineAtCaretAndCtrlEnterSends(t *testing.T) {
	for _, enterSends := range []bool{false, true} {
		t.Run(fmt.Sprint(enterSends), func(t *testing.T) {
			w, service := newSkillsWorkbench()
			w.widgets.sendWithEnter = enterSends
			tt := ui.NewTester(w.view, 1080, 700)
			mustClick(t, tt, "消息输入框")
			tt.Type("ab")
			// Caret between a and b.
			tt.Key(0, ui.KeyLeft)
			tt.Key(ui.Super, ui.KeyEnter)
			tt.Frame()
			tt.Frame()
			if got := w.composerFor("t1").draft; got != "a\nb" {
				t.Fatalf("⌘+Enter at caret: draft %q", got)
			}
			if len(service.dispatched()) != 0 {
				t.Fatal("⌘+Enter sent a message")
			}
			// A second ⌘+Enter at the same spot adds another line.
			tt.Key(ui.Super, ui.KeyEnter)
			tt.Frame()
			if got := w.composerFor("t1").draft; got != "a\n\nb" {
				t.Fatalf("second ⌘+Enter: draft %q", got)
			}
			if enterSends {
				tt.Key(0, ui.KeyEnter)
			} else {
				tt.Key(ui.Ctrl, ui.KeyEnter)
			}
			if cmds := service.dispatched(); len(cmds) != 1 || cmds[0].Message != "a\n\nb" {
				t.Fatalf("send = %+v", cmds)
			}
		})
	}
}

func TestTextSelectionHasVisibleHighlight(t *testing.T) {
	for _, dark := range []bool{false, true} {
		t.Run(fmt.Sprintf("dark=%v", dark), func(t *testing.T) {
			w, _ := newSkillsWorkbench()
			w.widgets.themeSet, w.widgets.dark = true, dark
			tt := ui.NewTester(w.view, 1080, 700)
			mustClick(t, tt, "消息输入框")
			tt.Type("选中我")
			tt.Key(ui.Cmd, ui.KeyA) // select all of the draft
			tt.Frame()
			info, ok := tt.Access("消息输入框")
			if !ok {
				t.Fatal("composer field not found")
			}
			img := tt.Image()
			tinted, sampled := 0, 0
			for y := int(info.Bounds.Y) + 4; y < int(info.Bounds.Y)+int(info.Bounds.H)-2; y += 2 {
				for x := int(info.Bounds.X) + 4; x < int(info.Bounds.X)+int(info.Bounds.W)-4; x += 3 {
					r, _, b, _ := img.At(x, y).RGBA()
					sampled++
					// An accent-tinted selection pixel is markedly cooler (bluer than red)
					// under the default Graphite palette; neutral canvas/card rows are not.
					if int(b>>8)-int(r>>8) > 25 {
						tinted++
					}
				}
			}
			if tinted < 30 {
				r, g, b, _ := img.At(int(info.Bounds.X)+20, int(info.Bounds.Y)+10).RGBA()
				t.Fatalf("dark=%v: selection not visible (tinted %d/%d px; sample #%02x%02x%02x)", dark, tinted, sampled, r>>8, g>>8, b>>8)
			}
		})
	}
}

func TestPastedThreadLinkCollapsesToMentionAndExpandsOnSend(t *testing.T) {
	c := &composer{}
	link := "[修复登录](vantry-thread://th_1?environmentId=env)"
	draft := "看下 " + link + " 的结果"
	caret := len([]rune("看下 " + link))
	got, at, changed := c.collapseThreadLinks(draft, caret)
	if !changed || got != "看下 @修复登录 的结果" || at != len([]rune("看下 @修复登录")) {
		t.Fatalf("collapse = %q caret %d changed %v", got, at, changed)
	}
	c.draft = got
	if c.outgoing() != draft {
		t.Fatalf("outgoing = %q", c.outgoing())
	}
	ranges := c.mentionRanges(palette{dark: true})
	if len(ranges) != 1 || ranges[0].Start != 3 || ranges[0].End != 3+len([]rune("@修复登录")) {
		t.Fatalf("ranges = %+v", ranges)
	}
	c.draft = "看下 的结果"
	c.pruneMentions()
	if len(c.mentions) != 0 {
		t.Fatal("removed mention kept")
	}
}
