package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

type fakeWorkbenchService struct {
	*fakeService
	workbench func(api.WorkbenchRequest) (api.WorkbenchResponse, error)
}

func (f *fakeWorkbenchService) Workbench(_ context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	return f.workbench(request)
}

func TestModelCatalogAndThinkingIgnoreOutOfOrderResponses(t *testing.T) {
	var response api.WorkbenchResponse
	var failure error
	var requests []api.WorkbenchRequest
	f := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		requests = append(requests, r)
		return response, failure
	}}
	w := newWorkbench(f)
	held := holdWork(w)
	a, b := api.ModelInfo{Provider: "test", ID: "family/a"}, api.ModelInfo{Provider: "test", ID: "b"}
	w.loadModels()
	response.Catalog = &api.ModelCatalog{Default: a.Key(), Models: []api.ModelInfo{a, b}, ThinkingLevels: []string{"off", "high"}}
	held.calls[0]()
	if base, _ := w.modelSelection(); base != a.Key() || w.features.thinkingModel != a.Key() {
		t.Fatalf("default selection %q, levels for %q", base, w.features.thinkingModel)
	}
	w.selectModel(a)
	w.selectModel(b)
	w.selectModel(a)
	if len(held.calls) != 4 {
		t.Fatalf("A→B→A must issue all three generations, got %d calls", len(held.calls))
	}
	response.Catalog = &api.ModelCatalog{ThinkingLevels: []string{"low", "high"}}
	held.calls[3]()
	w.setThinking("high")
	response.Catalog = &api.ModelCatalog{ThinkingLevels: []string{"off"}}
	held.calls[1]() // old A returning after new A
	failure = errors.New("stale B failure")
	held.calls[2]()
	if w.defaults.Model != a.Key()+":high" || !reflect.DeepEqual(w.features.thinkingLevels, []string{"low", "high"}) || w.features.modelFeedback.valid() {
		t.Fatalf("late thinking response changed selection: model=%s levels=%v feedback=%+v", w.defaults.Model, w.features.thinkingLevels, w.features.modelFeedback)
	}
	if len(w.features.catalog.Models) != 2 {
		t.Fatal("thinking-only response replaced the catalog")
	}
	if requests[1].Model != a.Key() {
		t.Fatalf("catalog RPC received an effort suffix: %+v", requests)
	}
}

func TestModelRefreshAndWorkspaceChangeKeepCurrentSelection(t *testing.T) {
	var response api.WorkbenchResponse
	f := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(api.WorkbenchRequest) (api.WorkbenchResponse, error) { return response, nil }}
	w := newWorkbench(f)
	held := holdWork(w)
	w.defaults.Workspace = &api.WorkspaceSpec{Path: "/old"}
	w.loadModels()
	w.defaults.Workspace = &api.WorkspaceSpec{Path: "/new"}
	w.invalidateModels()
	w.loadModels()
	model := api.ModelInfo{Provider: "new", ID: "model:variant"}
	response.Catalog = &api.ModelCatalog{Default: model.Key(), Models: []api.ModelInfo{model}, ThinkingLevels: []string{"low", "high"}}
	held.calls[1]()
	w.defaults.Model = model.Key() + ":high"
	w.setThinking("low")
	w.setThinking("high")
	response.Catalog = &api.ModelCatalog{Default: "old/model"}
	held.calls[0]()
	if w.features.catalog.Default != model.Key() || w.defaults.Model != model.Key()+":high" {
		t.Fatalf("workspace or suffix mismatch: %+v, %s", w.features.catalog, w.defaults.Model)
	}
	w.loadModels()
	response.Catalog = &api.ModelCatalog{Default: model.Key(), Models: []api.ModelInfo{model}, ThinkingLevels: []string{"low", "high"}}
	held.calls[2]()
	if w.defaults.Model != model.Key()+":high" {
		t.Fatal("refresh lost the selected level")
	}
}

func TestToolResponsesRemainInTheirOriginatingSession(t *testing.T) {
	f := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if r.Action == "git.status" {
			return api.WorkbenchResponse{}, errors.New("git failed")
		}
		return api.WorkbenchResponse{Output: "result belonging to first"}, nil
	}}
	w := newWorkbench(f)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("first", "First", 0), thread("second", "Second", time.Hour)}})
	held := holdWork(w)
	w.setDraft("first", "source draft")
	w.runPlugin("fixture")
	w.loadGit()
	w.selectThread("second")
	w.setDraft("second", "other draft")
	held.release()
	w.features.panel = "git"
	if w.panelFeedback().valid() || w.features.pluginOutputs["second"] != "" || w.composerFor("second").draft != "other draft" {
		t.Fatal("a background callback leaked into the selected session")
	}
	w.selectThread("first")
	if !w.panelFeedback().valid() || w.features.pluginOutputs["first"] != "result belonging to first" {
		t.Fatal("the originating session lost its result/error")
	}
}

func TestPluginResultFollowsFirstSendSessionMigration(t *testing.T) {
	f := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		return api.WorkbenchResponse{Output: "original session result"}, nil
	}}
	w := newWorkbench(f)
	local := w.newSession()
	w.setDraft(local, "source")
	held := holdWork(w)
	w.runPlugin("fixture")
	w.migrateSession(local, "created-thread")
	w.newSession()
	held.release()
	if w.features.pluginOutputs["created-thread"] != "original session result" || w.features.pluginOutputs[w.selected] != "" {
		t.Fatal("plugin result did not follow its originating composer")
	}
}

func TestAttachmentPartialFailureAndSessionSwitchPreserveDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("local fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		return api.WorkbenchResponse{Attachment: &api.Attachment{ID: "core-copy", Name: r.Name, Size: int64(len(r.Data))}}, nil
	}}
	w := newWorkbench(f)
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{thread("first", "First", 0), thread("second", "Second", time.Hour)}})
	w.setDraft("first", "keep this draft")
	held := holdWork(w)
	w.importAttachments("first", []string{path, filepath.Join(t.TempDir(), "missing.txt")})
	if w.send("first") {
		t.Fatal("send must wait for attachment import")
	}
	w.selectThread("second")
	held.release()
	comp := w.composerFor("first")
	if comp.draft != "keep this draft" || comp.uploading || len(comp.attachments) != 1 || !strings.Contains(comp.feedback.Text, "missing.txt") {
		t.Fatalf("partial import lost content: %+v", comp)
	}
	if len(w.composerFor("second").attachments) != 0 {
		t.Fatal("attachment moved to the selected session")
	}
}

func TestNativeFileDropImportsCoreOwnedAttachment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drop.txt")
	if err := os.WriteFile(path, []byte("drop fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if request.Action == "models" { // background thinking-level preload
			return api.WorkbenchResponse{}, nil
		}
		if request.Action != "attachment.import" || request.Name != "drop.txt" || string(request.Data) != "drop fixture" {
			t.Fatalf("unexpected import %+v", request)
		}
		return api.WorkbenchResponse{Attachment: &api.Attachment{ID: "core-owned-id", Name: request.Name, Size: int64(len(request.Data))}}, nil
	}}
	w := newWorkbench(f)
	id := w.newSession()
	w.setDraft(id, "draft before drop")
	tt := ui.NewTester(w.view, 640, 600)
	field, _ := tt.Access("消息输入框")
	if !tt.DropFiles(field.Bounds.X+10, field.Bounds.Y+10, path) {
		t.Fatal("composer did not accept a native file drop")
	}
	comp := w.composerFor(id)
	if comp.draft != "draft before drop" || len(comp.attachments) != 1 || comp.attachments[0].ID != "core-owned-id" {
		t.Fatalf("drop state %+v", comp)
	}
	if !w.send(id) {
		t.Fatal("draft with imported attachment was not sent")
	}
	commands := f.dispatched()
	if len(commands) != 2 || commands[1].Attachments[0].ID != "core-owned-id" {
		t.Fatalf("send did not use Core's attachment identity: %+v", commands)
	}
}

func TestAttachmentMissingReceiptShowsFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeWorkbenchService{fakeService: &fakeService{}, workbench: func(api.WorkbenchRequest) (api.WorkbenchResponse, error) { return api.WorkbenchResponse{}, nil }}
	w := newWorkbench(f)
	id := w.newSession()
	w.setDraft(id, "draft")
	w.importAttachments(id, []string{path})
	if w.composerFor(id).feedback.Level != noticeError || w.composerFor(id).draft != "draft" {
		t.Fatal("an empty import receipt must not silently succeed")
	}
}

func TestAttachmentRetryKeepsFrozenPayloadAndLaterDraft(t *testing.T) {
	w, f := newViewWorkbench()
	f.receiptErr = errors.New("lost receipt")
	comp := w.composerFor("t1")
	comp.attachments = []api.Attachment{{ID: "first-file", Name: "first.txt"}}
	w.setDraft("t1", "original")
	if !w.send("t1") {
		t.Fatal("send did not dispatch")
	}
	w.setDraft("t1", "later draft")
	comp.attachments = []api.Attachment{{ID: "later-file", Name: "later.txt"}}
	f.receiptErr = nil
	w.retryPending("t1")
	commands := f.dispatched()
	if len(commands) != 2 || !reflect.DeepEqual(commands[0], commands[1]) {
		t.Fatalf("retry changed original command: %+v", commands)
	}
	if comp.draft != "later draft" || comp.attachments[0].ID != "later-file" {
		t.Fatal("receipt consumed the new draft/attachment")
	}
}

func TestThreadTitleAndRecentFamilyNavigation(t *testing.T) {
	w := newWorkbench(&fakeService{})
	parent := thread("parent", newSessionTitle, 0)
	parent.Workspace = &api.WorkspaceSpec{Path: "/Projects/First"}
	child := thread("child", "Child", time.Minute)
	child.Workspace, child.ParentThreadID = cloneWorkspace(parent.Workspace), parent.ID
	other := thread("other", "Other", time.Hour)
	other.Workspace = &api.WorkspaceSpec{Path: "/Projects/Other"}
	w.applySnapshot(api.Snapshot{Threads: []api.Thread{parent, child, other}, Runs: []api.Run{
		run("parent-run", "parent", api.RunCompleted, "must not replace renamed title", "", 0),
		run("child-run", "child", api.RunCompleted, "activity", "", 2*time.Hour),
	}})
	if w.titleFor("parent") != newSessionTitle {
		t.Fatal("the first message replaced a persisted title")
	}
	// Subagents stay under their parent instead of cluttering the sidebar as
	// separate rows, while lifting the parent's family activity ahead of Other.
	groups := w.navigationGroups()
	if len(groups) != 1 || groups[0].key != sectionActive || !reflect.DeepEqual(groups[0].ids, []string{"parent", "other"}) {
		t.Fatalf("recent family order %+v", groups)
	}
	orphan := thread("orphan", "Orphan child", 3*time.Hour)
	orphan.ParentThreadID = "missing-parent"
	orphan.Workspace = &api.WorkspaceSpec{Path: "/Projects/First"}
	w.mergeThread(orphan)
	groups = w.navigationGroups()
	if !reflect.DeepEqual(groups[0].ids, []string{"orphan", "parent", "other"}) {
		t.Fatalf("orphan subagent must stay reachable: %+v", groups)
	}
	archived := other
	archived.Archived = true
	w.mergeThread(archived)
	w.newSession()
	for _, group := range w.navigationGroups() {
		if group.key == sectionSettled && !reflect.DeepEqual(group.ids, []string{"other"}) {
			t.Fatalf("settled shelf %+v", group.ids)
		}
		if group.key == sectionDraft && len(group.ids) != 1 {
			t.Fatalf("draft section %+v", group.ids)
		}
	}
}
