package desktop

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestComposerPasteImagesAndPDFInlinePositionAndSize(t *testing.T) {
	pngBytes := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte("a"), 49348)...) // ~48.2 KB
	pdfBytes := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("b"), 319991)...)                                  // 312.5 KB

	var nextPaste *clipboardPastePayload
	seq := 0
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if req.Action == "models" {
			return api.WorkbenchResponse{}, nil
		}
		if req.Action == "attachment.import" {
			seq++
			mime := "application/octet-stream"
			if strings.HasSuffix(req.Name, ".png") {
				mime = "image/png"
			} else if strings.HasSuffix(req.Name, ".pdf") {
				mime = "application/pdf"
			}
			return api.WorkbenchResponse{Attachment: &api.Attachment{
				ID:   "file_test_" + string(rune('0'+seq)),
				Name: req.Name,
				MIME: mime,
				Size: int64(len(req.Data)),
			}}, nil
		}
		return api.WorkbenchResponse{}, nil
	}}

	w := newWorkbench(service)
	w.clipboardProbe = func(pasted string) (clipboardPastePayload, bool) {
		if nextPaste != nil {
			p := *nextPaste
			nextPaste = nil
			return p, true
		}
		return clipboardPastePayload{}, false
	}
	id := w.newSession()
	tt := ui.NewTester(w.view, 1080, 760)

	mustClick(t, tt, "消息输入框")
	tt.Type("请先看截图 ")

	// 1. Paste PNG screenshot in the middle of text.
	nextPaste = &clipboardPastePayload{
		Items: []clipboardPasteItem{{Name: "pasted-image.png", MIME: "image/png", Data: pngBytes}},
	}
	tt.SetClipboard("")
	tt.Command("paste")
	tt.Frame()

	comp := w.composerFor(id)
	// Keep the thumbnail and a compact reference at the paste position.
	if comp.draft != "请先看截图 @pasted-image-1.png" || len(comp.attachments) != 1 || comp.attachments[0].Name != "pasted-image-1.png" {
		t.Fatalf("after PNG paste: draft=%q attachments=%+v", comp.draft, comp.attachments)
	}

	// Continue typing after the pasted image, then paste a PDF inline.
	tt.Type(" 再对照需求文档 ")
	nextPaste = &clipboardPastePayload{
		Items: []clipboardPasteItem{{Name: "pasted-document.pdf", MIME: "application/pdf", Data: pdfBytes}},
	}
	tt.SetClipboard("")
	tt.Command("paste")
	tt.Frame()
	tt.Type(" 给出修改方案")
	tt.Frame()

	wantFull := "请先看截图 @pasted-image-1.png 再对照需求文档 @pasted-document-2.pdf 给出修改方案"
	if comp.draft != wantFull {
		t.Fatalf("inline paste positions mismatch:\n got: %q\nwant: %q", comp.draft, wantFull)
	}
	if len(comp.attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %+v", comp.attachments)
	}

	// Plain text paste still passes straight through normally.
	tt.SetClipboard("！")
	tt.Command("paste")
	tt.Frame()
	if !strings.HasSuffix(comp.draft, "给出修改方案！") {
		t.Fatalf("plain text paste broken: %q", comp.draft)
	}

	// The thumbnail's × removes its own reference, preserving surrounding text.
	before := comp.draft
	mustClick(t, tt, "移除附件 pasted-image-1.png")
	tt.Frame()
	if comp.draft != strings.ReplaceAll(before, "@pasted-image-1.png", "") || len(comp.attachments) != 1 || comp.attachments[0].Name != "pasted-document-2.pdf" {
		t.Fatalf("removing attachment changed text or kept the image: draft=%q attachments=%+v", comp.draft, comp.attachments)
	}
	mustClick(t, tt, "移除附件 pasted-document-2.pdf")
	tt.Frame()
	if len(comp.attachments) != 0 {
		t.Fatalf("removing PDF chip kept it: %+v", comp.attachments)
	}
}

func TestAutoCommitSummaryMatchesLocaleAndStaysWithin20Chars(t *testing.T) {
	cases := []struct {
		name  string
		files []api.GitFile
		amend bool
	}{
		{"empty", nil, false},
		{"amend-empty", nil, true},
		{"single-modified", []api.GitFile{{Path: "internal/desktop/composer_view.go", Index: " ", Worktree: "M"}}, false},
		{"single-added-long-name", []api.GitFile{{Path: "internal/adapters/provider/pi/super_extra_long_attachment_handler_module.go", Index: "?", Worktree: "?"}}, false},
		{"single-deleted", []api.GitFile{{Path: "legacy_unused_helper.go", Index: "D", Worktree: " "}}, false},
		{"multi-files-same-dir", []api.GitFile{
			{Path: "internal/desktop/git_idea.go", Index: " ", Worktree: "M"},
			{Path: "internal/desktop/git_sync.go", Index: " ", Worktree: "M"},
			{Path: "internal/desktop/git_log.go", Index: " ", Worktree: "M"},
		}, false},
		{"many-files-mixed", []api.GitFile{
			{Path: "a.go", Index: "?", Worktree: "?"},
			{Path: "b.go", Index: " ", Worktree: "M"},
			{Path: "c.go", Index: " ", Worktree: "D"},
			{Path: "d.go", Index: "M", Worktree: " "},
			{Path: "e.go", Index: "M", Worktree: " "},
		}, false},
	}

	for _, locale := range []string{localeZH, localeEN, localeJA} {
		for _, tc := range cases {
			msg := autoCommitSummaryForFiles(tc.files, tc.amend, locale)
			n := utf8.RuneCountInString(msg)
			if msg == "" || n > 20 {
				t.Errorf("locale=%s case=%s produced %q (%d runes, want 1..20)", locale, tc.name, msg, n)
			}
		}
	}
}

func TestTopRightPanelOneClickCommitPushAndEditorFallback(t *testing.T) {
	var actions []string
	var commitReq api.WorkbenchRequest
	gitState := &api.GitState{
		Branch: "vantry/feature",
		Files: []api.GitFile{
			{Path: "internal/desktop/view.go", Index: " ", Worktree: "M"},
			{Path: "internal/desktop/new_file.go", Index: "?", Worktree: "?"},
		},
	}
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if req.Action == "models" {
			return api.WorkbenchResponse{}, nil
		}
		actions = append(actions, req.Action)
		switch req.Action {
		case "git.status":
			return api.WorkbenchResponse{Git: gitState}, nil
		case "git.commit":
			commitReq = req
			return api.WorkbenchResponse{Git: &api.GitState{Branch: "vantry/feature", Files: []api.GitFile{}, Ahead: 1}}, nil
		case "git.push":
			return api.WorkbenchResponse{Git: &api.GitState{Branch: "vantry/feature", Files: []api.GitFile{}, Ahead: 0}}, nil
		}
		return api.WorkbenchResponse{}, nil
	}}

	w := newWorkbench(service)
	statePath := filepath.Join(t.TempDir(), "desktop-state.json")
	w.defaults.StatePath = statePath
	w.defaults.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry", Branch: "vantry/feature"}

	// Case 1: Machine without any code editor falls back to 访达 (Finder).
	var openedBy string
	w.detectedEditors = []editorOption{
		{ID: "finder", Name: "Finder", ZHName: "访达", JAName: "Finder", Icon: folderIcon, IsFileManager: true, Open: func(p string) error {
			openedBy = "finder:" + p
			return nil
		}},
	}
	w.newSession()
	tt := ui.NewTester(w.view, 1440, 900)
	if !tt.HasText("在访达中打开") || tt.HasText("在 VS Code 中打开") || tt.HasText("Git 同步") {
		t.Fatalf("expected Finder fallback and Commit & push, got texts=%q", tt.Texts())
	}
	mustClick(t, tt, "在访达中打开")
	if openedBy != "finder:/Projects/Vantry" {
		t.Fatalf("Finder fallback opened %q", openedBy)
	}

	// Case 2: Multiple installed editors allow switching and persisting preference.
	w.detectedEditors = []editorOption{
		{ID: "cursor", Name: "Cursor", ZHName: "Cursor", JAName: "Cursor", Icon: codeEditorIcon, Open: func(p string) error {
			openedBy = "cursor:" + p
			return nil
		}},
		{ID: "idea", Name: "IntelliJ IDEA", ZHName: "IntelliJ IDEA", JAName: "IntelliJ IDEA", Icon: codeEditorIcon, Open: func(p string) error {
			openedBy = "idea:" + p
			return nil
		}},
		{ID: "finder", Name: "Finder", ZHName: "访达", JAName: "Finder", Icon: folderIcon, IsFileManager: true},
	}
	tt.Frame()
	if !tt.HasText("在 Cursor 中打开") {
		t.Fatalf("expected first detected editor Cursor, got %q", tt.Texts())
	}
	mustClick(t, tt, "选择打开方式")
	mustClick(t, tt, "在 IntelliJ IDEA 中打开")
	tt.Frame()
	if w.widgets.preferredEditor != "idea" || openedBy != "idea:/Projects/Vantry" {
		t.Fatalf("editor picker did not persist/open IntelliJ IDEA: pref=%q opened=%q", w.widgets.preferredEditor, openedBy)
	}

	// Case 3: Switch language to Japanese and trigger 1-click Commit & push from top-right panel.
	w.setLocale(localeJA)
	tt.Frame()
	if !tt.HasText("コミット＆プッシュ") {
		t.Fatalf("Japanese top-right Commit & push label missing: %q", tt.Texts())
	}
	actions = nil
	mustClick(t, tt, "コミット＆プッシュ")
	if !reflect.DeepEqual(actions, []string{"git.status", "git.commit", "git.push"}) {
		t.Fatalf("1-click Commit & push sequence = %v", actions)
	}
	if utf8.RuneCountInString(commitReq.Message) == 0 || utf8.RuneCountInString(commitReq.Message) > 20 || !strings.Contains(commitReq.Message, "更新") {
		t.Fatalf("expected <=20-char Japanese commit summary, got %q", commitReq.Message)
	}
	if !reflect.DeepEqual(commitReq.Paths, []string{"internal/desktop/new_file.go", "internal/desktop/view.go"}) {
		t.Fatalf("1-click commit did not stage all dirty & untracked files: %+v", commitReq.Paths)
	}
}

func TestOneClickCommitPushRefreshesStaleCacheAndIncludesRenames(t *testing.T) {
	var actions []string
	var commitReq api.WorkbenchRequest
	liveState := &api.GitState{
		Branch: "vantry/rename",
		Files: []api.GitFile{
			{Path: "internal/desktop/new_name.go", OriginalPath: "internal/desktop/old_name.go", Index: "R", Worktree: " "},
			{Path: "internal/desktop/added.go", Index: "?", Worktree: "?"},
		},
	}
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if req.Action == "models" {
			return api.WorkbenchResponse{}, nil
		}
		actions = append(actions, req.Action)
		switch req.Action {
		case "git.status":
			return api.WorkbenchResponse{Git: liveState}, nil
		case "git.commit":
			commitReq = req
			return api.WorkbenchResponse{Git: &api.GitState{Branch: "vantry/rename", Files: []api.GitFile{}, Ahead: 1}}, nil
		case "git.push":
			return api.WorkbenchResponse{Git: &api.GitState{Branch: "vantry/rename", Files: []api.GitFile{}, Ahead: 0}}, nil
		}
		return api.WorkbenchResponse{}, nil
	}}

	w := newWorkbench(service)
	w.defaults.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry", Branch: "vantry/rename"}
	id := w.newSession()
	// Seed a stale clean Git cache to prove oneClickCommitAndPush still reads fresh git.status.
	w.features.git = map[string]*api.GitState{
		id: {Branch: "vantry/rename", Files: []api.GitFile{}, Ahead: 0},
	}

	if !w.oneClickCommitAndPush(id) {
		t.Fatal("oneClickCommitAndPush did not start")
	}
	if !reflect.DeepEqual(actions, []string{"git.status", "git.commit", "git.push"}) {
		t.Fatalf("expected fresh git.status -> git.commit -> git.push despite stale clean cache, got %v", actions)
	}
	wantPaths := []string{"internal/desktop/added.go", "internal/desktop/new_name.go", "internal/desktop/old_name.go"}
	if !reflect.DeepEqual(commitReq.Paths, wantPaths) {
		t.Fatalf("expected rename origin and untracked file in commit paths:\n got: %v\nwant: %v", commitReq.Paths, wantPaths)
	}
}

func TestWorktreePanelNeverFallsBackToRepositoryAndLoadsDiffFromCard(t *testing.T) {
	service := newFakeQueryService()
	service.changes["wt1"] = api.WorkspaceChanges{
		Path:   "/Data/workspaces/wt1/worktree",
		Branch: "vantry/wt1",
		IsGit:  true,
		Files:  []api.WorkspaceFile{{Path: "strategy.py", Status: "??"}},
		Diff:   "diff --git a/strategy.py b/strategy.py\n--- /dev/null\n+++ b/strategy.py\n@@ -0,0 +1,2 @@\n+x = 1\n+y = 2",
	}
	w := newWorkbench(service)
	var openedPath string
	w.detectedEditors = []editorOption{
		{ID: "finder", Name: "Finder", ZHName: "访达", JAName: "Finder", Icon: folderIcon, IsFileManager: true, Open: func(p string) error {
			openedPath = p
			return nil
		}},
	}
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{{
		ID:    "wt1",
		Title: "Migrated worktree",
		Workspace: &api.WorkspaceSpec{
			Kind:       "worktree",
			Repository: "/MainRepo/should-never-open-this",
			Branch:     "vantry/wt1",
		},
	}}})
	w.selectThread("wt1")

	// Before InspectWorkspace runs, selectedWorkspacePath must NOT leak Repository.
	if got := w.selectedWorkspacePath(); got != "" {
		t.Fatalf("worktree without resolved path leaked %q", got)
	}

	tt := ui.NewTester(w.view, 1440, 900)
	// Clicking Changes on projectCard (while workspaceOpen == false) loads and renders diff.
	mustClick(t, tt, "变更")
	tt.Frame()
	if w.workspaceChanges == nil || w.workspaceChanges.Path != "/Data/workspaces/wt1/worktree" || !tt.HasText("strategy.py") {
		t.Fatalf("projectCard Changes did not load worktree diff: changes=%+v texts=%q", w.workspaceChanges, tt.Texts())
	}
	mustClick(t, tt, "关闭变更")
	tt.Frame()

	// Reset cached changes and verify Open in Finder resolves the isolated worktree via InspectWorkspace.
	w.workspaceChanges = nil
	mustClick(t, tt, "在访达中打开")
	if openedPath != "/Data/workspaces/wt1/worktree" {
		t.Fatalf("expected isolated worktree path, opened %q", openedPath)
	}
}

func TestGitStageAllIncludesUntrackedFiles(t *testing.T) {
	var stagedReq api.WorkbenchRequest
	gitState := &api.GitState{
		Branch: "main",
		Files: []api.GitFile{
			{Path: "tracked.go", Index: " ", Worktree: "M"},
			{Path: "untracked.go", Index: "?", Worktree: "?"},
		},
	}
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if req.Action == "git.stage" {
			stagedReq = req
			return api.WorkbenchResponse{Git: gitState}, nil
		}
		if req.Action == "git.status" {
			return api.WorkbenchResponse{Git: gitState}, nil
		}
		return api.WorkbenchResponse{}, nil
	}}
	w := newWorkbench(service)
	w.defaults.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry"}
	id := w.newSession()
	w.features.git = map[string]*api.GitState{id: gitState}
	w.openDock("git")

	tt := ui.NewTester(w.view, 1280, 820)
	mustClick(t, tt, "暂存全部")
	if !reflect.DeepEqual(stagedReq.Paths, []string{"tracked.go", "untracked.go"}) {
		t.Fatalf("Stage all did not include untracked files: %v", stagedReq.Paths)
	}
}

func TestClipboardMenuPasteInsertsAtCaretAndReportsEmptyClipboard(t *testing.T) {
	pngBytes := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte("z"), 2040)...)
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if req.Action == "attachment.import" {
			return api.WorkbenchResponse{Attachment: &api.Attachment{
				ID: "file_menu_1", Name: req.Name, MIME: "image/png", Size: int64(len(req.Data)),
			}}, nil
		}
		return api.WorkbenchResponse{}, nil
	}}
	w := newWorkbench(service)
	hasMedia := false
	w.clipboardProbe = func(string) (clipboardPastePayload, bool) {
		if hasMedia {
			return clipboardPastePayload{Items: []clipboardPasteItem{{Name: "pasted-image.png", MIME: "image/png", Data: pngBytes}}}, true
		}
		return clipboardPastePayload{}, false
	}
	w.hostClipboardMedia = func(_ context.Context) ([]byte, string, string, bool, error) {
		return nil, "", "", false, nil
	}

	id := w.newSession()
	comp := w.composerFor(id)
	w.setDraft(id, "前半句 后半句")
	comp.liveAnchor, comp.liveCaret = 4, 4 // right after "前半句 "

	// 1. Empty clipboard from menu reports clear warning instead of silent no-op.
	w.pasteFromClipboardMenu(id)
	if comp.feedback.Level != noticeWarn || !strings.Contains(comp.feedback.Text, "剪贴板") {
		t.Fatalf("expected empty clipboard warning from menu paste, got %+v", comp.feedback)
	}

	// 2. Media paste from menu inserts at caret (offset 4), not at end of draft.
	hasMedia = true
	w.pasteFromClipboardMenu(id)
	if comp.draft != "前半句 @pasted-image-1.png后半句" || len(comp.attachments) != 1 || comp.caretAfter != len([]rune("前半句 @pasted-image-1.png")) {
		t.Fatalf("menu paste must insert its reference at the caret: draft=%q caret=%d attachments=%+v", comp.draft, comp.caretAfter, comp.attachments)
	}
}

func TestImportAttachmentsMediaInlineAndDraftStatePersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "screenshot.png")
	pdfPath := filepath.Join(dir, "design-spec.pdf")
	pngData := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte("p"), 12280)...)
	pdfData := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("d"), 65527)...)
	if err := os.WriteFile(pngPath, pngData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pdfPath, pdfData, 0o600); err != nil {
		t.Fatal(err)
	}

	seq := 0
	service := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(req api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if req.Action == "attachment.import" {
			seq++
			mime := "image/png"
			if strings.HasSuffix(req.Name, ".pdf") {
				mime = "application/pdf"
			}
			return api.WorkbenchResponse{Attachment: &api.Attachment{
				ID:   "persisted_att_" + string(rune('0'+seq)),
				Name: req.Name,
				MIME: mime,
				Size: int64(len(req.Data)),
			}}, nil
		}
		return api.WorkbenchResponse{}, nil
	}}

	statePath := filepath.Join(dir, "desktop-state.json")
	w := newWorkbench(service)
	w.defaults = Options{StatePath: statePath, ProviderID: "pi"}
	local := w.newSession()
	w.setDraft(local, "请结合附件排查：")
	comp := w.composerFor(local)
	comp.liveAnchor, comp.liveCaret = 0, 0 // simulates dropping files without focusing first

	w.importAttachments(local, []string{pngPath, pdfPath})
	if comp.draft != "@screenshot.png @design-spec.pdf请结合附件排查：" {
		t.Fatalf("importing media must keep references at the insertion position: draft=%q", comp.draft)
	}
	if len(comp.attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %+v", comp.attachments)
	}

	// Simulate full window restart: restore into a fresh workbench and send.
	restoredFake := &fakeService{}
	restoredService := &fakeWorkbenchService{fakeService: restoredFake, workbench: service.workbench}
	restored := newWorkbench(restoredService)
	restored.defaults = Options{StatePath: statePath, ProviderID: "pi"}
	if err := restored.restoreState(); err != nil {
		t.Fatal(err)
	}
	rComp := restored.composerFor(local)
	if rComp.draft != comp.draft || len(rComp.attachments) != 2 {
		t.Fatalf("restored composer lost attachments: draft=%q attachments=%+v", rComp.draft, rComp.attachments)
	}
	if !restored.send(local) {
		t.Fatal("send after restart failed")
	}
	dispatched := restoredFake.dispatched()
	if len(dispatched) != 2 || len(dispatched[1].Attachments) != 2 || dispatched[1].Attachments[0].ID != "persisted_att_1" || dispatched[1].Attachments[1].ID != "persisted_att_2" {
		t.Fatalf("send after restart lost actual attachment items: %+v", dispatched)
	}
}

func TestSideDockGitLogNoDoubleTabsNoOverlapAndWorkspacePopover(t *testing.T) {
	now := time.Now()
	state := &api.GitState{
		Root:           "/Projects/Vantry",
		Branch:         "integration/native-rtc",
		RemoteBranch:   "origin/integration/native-rtc",
		Ahead:          1,
		Behind:         0,
		Branches:       []string{"integration/native-rtc", "main", "ui/t3-realign"},
		RemoteBranches: []string{"origin/integration/native-rtc", "origin/main"},
		Files: []api.GitFile{
			{Path: "internal/desktop/git_log.go", Index: " ", Worktree: "M"},
			{Path: "internal/desktop/dock_view.go", Index: "M", Worktree: " "},
		},
		Commits: []api.GitCommit{
			{
				ID:         "2be66b0000000000000000000000000000000000",
				ShortID:    "2be66b0",
				Subject:    "chore(release): cut 0.1.18 with the staged updater",
				Author:     "Dev",
				AuthorTime: now.Add(-2 * time.Hour),
				Refs:       []string{"HEAD -> integration/native-rtc", "tag: v0.1.18", "origin/integration/native-rtc"},
				Parents:    []string{"722b468000000000000000000000000000000000"},
			},
			{
				ID:         "722b468000000000000000000000000000000000",
				ShortID:    "722b468",
				Subject:    "Merge origin/main into the release line",
				Author:     "Dev",
				AuthorTime: now.Add(-2 * time.Hour),
				Parents:    []string{"9fbfd9f000000000000000000000000000000000", "967fc1f000000000000000000000000000000000"},
			},
			{
				ID:         "9fbfd9f000000000000000000000000000000000",
				ShortID:    "9fbfd9f",
				Subject:    "Merge branch 'ui/t3-realign': T3 and IDEA realignment",
				Author:     "Dev",
				AuthorTime: now.Add(-2 * time.Hour),
				Parents:    []string{"967fc1f000000000000000000000000000000000"},
			},
			{
				ID:         "967fc1f000000000000000000000000000000000",
				ShortID:    "967fc1f",
				Subject:    "feat(git): IDEA-style log with branch tree, filters, and commit details",
				Author:     "Dev",
				AuthorTime: now.Add(-3 * time.Hour),
			},
		},
	}

	rdPlugin := api.PluginInfo{
		ID: "vantry-remote-desktop", Name: "远程桌面", Version: "0.3.1", Builtin: false,
		Description: "通过独立桌面共享码连接另一台 Vantry；画面与键鼠共享由本机单独授权。",
		Navigation:  []api.PluginNavItem{{Title: "远程桌面", Page: "desktop", NativePanel: "remote-desktop"}},
	}
	w, _ := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case actionGitLog:
			return api.WorkbenchResponse{Git: &api.GitState{Commits: state.Commits}}, nil
		case actionPRStatus:
			return api.WorkbenchResponse{Output: `{"number":42,"title":"Align Git side dock","state":"open","checks":"success","url":"https://github.com/example/repo/pull/42"}`}, nil
		case "plugins.list":
			return api.WorkbenchResponse{Plugins: []api.PluginInfo{rdPlugin}}, nil
		}
		return api.WorkbenchResponse{Git: state}, nil
	})
	w.detectedEditors = []editorOption{
		{ID: "finder", Name: "Finder", ZHName: "访达", JAName: "Finder", Icon: folderIcon, IsFileManager: true},
	}
	w.workspaceChanges = &api.WorkspaceChanges{
		Path:   "/Projects/Vantry",
		Branch: "integration/native-rtc",
		IsGit:  true,
		Files:  []api.WorkspaceFile{{Path: "internal/desktop/git_log.go", Status: "M"}},
		Diff:   "diff --git a/internal/desktop/git_log.go b/internal/desktop/git_log.go\n--- a/internal/desktop/git_log.go\n+++ b/internal/desktop/git_log.go\n@@ -1 +1 @@\n-old\n+new\n",
	}
	w.features.plugins = []api.PluginInfo{rdPlugin}
	w.features.git = map[string]*api.GitState{"t1": state}
	w.gitView.pr = map[string]*prStatus{
		"t1": {Number: 42, Title: "Align Git side dock", State: "open", Checks: "success", URL: "https://github.com/example/repo/pull/42"},
	}
	w.gitView.prLoaded = map[string]bool{"t1": true}

	tt := ui.NewTesterWith(w.view, 1280, 860, ui.TesterOptions{Dark: true})
	// Open Git dock via title bar button.
	mustClick(t, tt, "Git")
	tt.Frame()
	if w.dock.maximized {
		t.Fatal("opening Git must start in side dock, not maximized")
	}
	// Duplicate header tool-switcher must hide while dock tabs are open.
	if _, duplicated := tt.AccessByRole(ui.RoleTab, "远程"); duplicated {
		t.Fatal("header tool-switcher remained visible on top of dockTabStrip")
	}

	// Switch to Log tab: must stay in normal side-dock mode (not force-maximize).
	mustClick(t, tt, "Git 日志页")
	w.gitView.filter.branch = "integration/native-rtc"
	tt.Advance(300 * time.Millisecond)
	tt.Frame()
	if w.dock.maximized {
		t.Fatal("clicking Git Log tab must not hijack the window into dockMaximized")
	}
	if _, ok := tt.Access("会话内容"); !ok {
		t.Fatal("conversation must remain visible beside Git log side dock at 1280x860")
	}

	if outDir := os.Getenv("VANTRY_VERIFY_RENDER_DIR"); outDir != "" {
		_ = os.MkdirAll(outDir, 0o700)
		savePNG := func(name string) {
			f, err := os.Create(filepath.Join(outDir, name))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := png.Encode(f, tt.Image()); err != nil {
				t.Fatal(err)
			}
		}
		savePNG("side-dock-git-log-dark.png")

		// Also click top-right [◫] workspace button while Git side-dock is open!
		mustClick(t, tt, "工作区")
		tt.Frame()
		savePNG("side-dock-workspace-popover-dark.png")
	} else {
		mustClick(t, tt, "工作区")
		tt.Frame()
	}

	// Top-right workspace panel must open as a reachable popover even while Git side dock occupies the right split.
	if !w.widgets.workspaceOpen {
		t.Fatal("clicking workspace button did not open workspaceOpen")
	}
	for _, label := range []string{"在访达中打开", "提交并推送", "收起工作区"} {
		assertControlInWindow(t, tt, label, 1280, 860)
	}
	mustClick(t, tt, "收起工作区")
	tt.Frame()
	if w.widgets.workspaceOpen {
		t.Fatal("close workspace button in popover did not close workspaceOpen")
	}

	if outDir := os.Getenv("VANTRY_VERIFY_RENDER_DIR"); outDir != "" {
		mustClick(t, tt, "工具页 插件")
		tt.Frame()
		f, err := os.Create(filepath.Join(outDir, "side-dock-plugins-dark.png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, tt.Image()); err != nil {
			f.Close()
			t.Fatal(err)
		}
		_ = f.Close()
	}

	// Extreme narrow dockMinWidth (340px) with 4+ long refs and all 4 filters active simultaneously.
	state.Commits[0].Refs = []string{
		"HEAD -> integration/native-rtc",
		"tag: v0.1.18-rc2",
		"origin/integration/native-rtc",
		"upstream/integration/native-rtc",
	}
	w.dock.widths = map[string]float32{"git": dockMinWidth}
	w.openDock("git")
	w.gitView.tab = 1
	w.gitView.filter = gitLogFilter{
		branch: "integration/native-rtc",
		author: "Demo User",
		since:  "7d",
		path:   "internal/desktop/git_log.go",
	}
	w.gitView.logLoaded = map[string]string{"t1": w.logSignature()}
	w.gitView.logs = map[string][]api.GitCommit{"t1": state.Commits}
	tt.Frame()
	if !tt.HasText("重置筛选") || !tt.HasText("PR #42") {
		t.Fatalf("expected Reset filter button and PR #42 badge at 340px dockMinWidth, got texts=%q", tt.Texts())
	}
	if outDir := os.Getenv("VANTRY_VERIFY_RENDER_DIR"); outDir != "" {
		f, err := os.Create(filepath.Join(outDir, "side-dock-git-log-340-dark.png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, tt.Image()); err != nil {
			f.Close()
			t.Fatal(err)
		}
		_ = f.Close()
	}
}

func TestConfigureWorkspaceSpecPreservesInlineAttachmentMentions(t *testing.T) {
	service := newFakeQueryService()
	w := newWorkbench(service)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{{
		ID:        "th_existing",
		Title:     "Existing thread",
		Workspace: &api.WorkspaceSpec{Kind: "directory", Path: "/Projects/Old"},
	}}})
	w.selectThread("th_existing")
	w.insertPastedAttachmentsAt("th_existing", []api.Attachment{{
		ID: "att_inline_1", Name: "diagram.png", MIME: "image/png", Size: 4096,
	}}, 0, 0)

	w.configureWorkspaceSpec(&api.WorkspaceSpec{Kind: "directory", Path: "/Projects/New"})
	if !isLocalSession(w.selected) {
		t.Fatalf("expected new local session, got %q", w.selected)
	}
	nextComp := w.composerFor(w.selected)
	if len(nextComp.attachments) != 1 || nextComp.attachments[0].ID != "att_inline_1" {
		t.Fatalf("configureWorkspaceSpec lost attachments: attachments=%+v", nextComp.attachments)
	}
}
