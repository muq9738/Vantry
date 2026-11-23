package core

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func TestQueueSurvivesRestartAndDoesNotDuplicateSubmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := &funcProvider{runFn: func(ctx context.Context, _ provider.Request, _ func(string) error) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	item := api.Attachment{ID: "file_test", Name: "note.txt", MIME: "text/plain", Size: 4}
	validate := func(items []api.Attachment) error {
		if !reflect.DeepEqual(items, []api.Attachment{item}) {
			t.Errorf("unexpected attachments: %+v", items)
		}
		return nil
	}
	c, err := Open(context.Background(), Options{DBPath: path, Provider: first, ValidateAttachments: validate})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	threadID := mustCreateThread(t, c, "create")
	mustSend(t, c, "first", threadID, "first")
	waitFor(t, "first provider", func() bool { return first.callCount() == 1 })
	command := api.Command{Version: 1, CommandID: "queue", Kind: api.QueueMessage, ThreadID: threadID, Message: "second", Attachments: []api.Attachment{item}}
	receipt, err := c.Dispatch(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := c.Dispatch(context.Background(), command)
	if err != nil || receipt != retry || receipt.MessageID == "" {
		t.Fatalf("queue receipt retry: %+v %+v %v", receipt, retry, err)
	}
	archive := true
	if _, err = c.Dispatch(context.Background(), api.Command{Version: 1, CommandID: "archive", Kind: api.UpdateThread, ThreadID: threadID, Archived: &archive}); api.CodeOf(err) != api.Conflict {
		t.Fatalf("archived active queue: %v", err)
	}
	if first.callCount() != 1 {
		t.Fatal("queued prompt started before current run ended")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	second := &funcProvider{}
	reopened, err := Open(context.Background(), Options{DBPath: path, Provider: second, ValidateAttachments: validate})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	waitFor(t, "queue after restart", func() bool { return second.callCount() == 1 })
	waitFor(t, "completed queue run", func() bool {
		snap := snapshotOf(t, reopened)
		return len(snap.Runs) == 2 && snap.Runs[1].Status == api.RunCompleted
	})
	req := second.requestAt(0)
	if req.Message != "second" || !reflect.DeepEqual(req.Attachments, []api.Attachment{item}) {
		t.Fatalf("queue request changed: %+v", req)
	}
	snap := snapshotOf(t, reopened)
	if len(snap.Messages) != 1 || snap.Messages[0].Status != "started" || snap.Messages[0].RunID != req.RunID {
		t.Fatalf("queue projection: %+v", snap.Messages)
	}
	retry, err = reopened.Dispatch(context.Background(), command)
	if err != nil || retry != receipt {
		t.Fatalf("restart receipt changed: %+v %v", retry, err)
	}
	if len(snapshotOf(t, reopened).Runs) != 2 {
		t.Fatal("retry created another run")
	}
}

func TestThreadManagementAndQueueCancellation(t *testing.T) {
	p := &funcProvider{runFn: func(ctx context.Context, _ provider.Request, _ func(string) error) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	c := openTestCore(t, p)
	id := mustCreateThread(t, c, "create")
	pinned := true
	cmd := api.Command{Version: 1, CommandID: "rename", Kind: api.UpdateThread, ThreadID: id, Title: "重命名后的会话", Pinned: &pinned, Model: "test/new-model"}
	if _, err := c.Dispatch(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	thread := snapshotOf(t, c).Threads[0]
	if thread.Title != cmd.Title || !thread.Pinned || thread.Model != cmd.Model {
		t.Fatalf("thread not updated: %+v", thread)
	}
	mustSend(t, c, "start", id, "running")
	queue := api.Command{Version: 1, CommandID: "queue", Kind: api.QueueMessage, ThreadID: id, Message: "cancel me"}
	receipt, err := c.Dispatch(context.Background(), queue)
	if err != nil {
		t.Fatal(err)
	}
	cancel := api.Command{Version: 1, CommandID: "cancel", Kind: api.CancelMessage, ThreadID: id, MessageID: receipt.MessageID}
	a, err := c.Dispatch(context.Background(), cancel)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Dispatch(context.Background(), cancel)
	if err != nil || a != b {
		t.Fatal("cancel retry not idempotent")
	}
	if snapshotOf(t, c).Messages[0].Status != "cancelled" {
		t.Fatal("queue cancellation missing")
	}
	if _, err := c.Dispatch(context.Background(), api.Command{Version: 1, CommandID: "model", Kind: api.UpdateThread, ThreadID: id, Model: "test/other"}); api.CodeOf(err) != api.Conflict {
		t.Fatalf("changed model while active: %v", err)
	}
}
