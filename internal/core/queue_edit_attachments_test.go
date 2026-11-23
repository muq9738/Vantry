package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func TestQueuedComposerEditReplacesAndRemovesValidatedAttachments(t *testing.T) {
	p := &funcProvider{runFn: func(ctx context.Context, _ provider.Request, _ func(string) error) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	c := openTestCore(t, p)
	c.validateAttachments = func(items []api.Attachment) error {
		for _, item := range items {
			if item.ID != "old" && item.ID != "new" {
				return errors.New("invalid attachment")
			}
		}
		return nil
	}
	threadID := mustCreateThread(t, c, "thread")
	first := mustQueue(t, c, "first", threadID, "running")
	waitRunStatus(t, c, first.RunID, api.RunRunning)
	queue := lifecycleCommand("queued", api.QueueMessage, threadID)
	queue.Message = "original"
	queue.Attachments = []api.Attachment{{ID: "old", Name: "old.png"}}
	accepted := dispatchOrFail(t, c, queue)
	before := waitInputStatus(t, c, accepted.InputID, inputQueued)
	edit := lifecycleCommand("replace", api.EditInput, threadID)
	edit.InputID = accepted.InputID
	edit.Message = "edited [🖼 new.png]"
	edit.Value = "replace-attachments"
	edit.Attachments = []api.Attachment{{ID: "new", Name: "new.png"}}
	dispatchOrFail(t, c, edit)
	after := waitInputStatus(t, c, accepted.InputID, inputQueued)
	if !reflect.DeepEqual(after.Attachments, edit.Attachments) || after.Message != edit.Message || !reflect.DeepEqual(after.Configuration, before.Configuration) {
		t.Fatalf("edit did not persist: %+v", after)
	}
	invalid := edit
	invalid.CommandID = "invalid"
	invalid.Attachments = []api.Attachment{{ID: "invalid"}}
	if _, err := c.Dispatch(context.Background(), invalid); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	remove := edit
	remove.CommandID = "remove"
	remove.Message = "text only"
	remove.Attachments = nil
	dispatchOrFail(t, c, remove)
	after = waitInputStatus(t, c, accepted.InputID, inputQueued)
	if len(after.Attachments) != 0 || after.Message != "text only" {
		t.Fatalf("removed attachment survived: %+v", after)
	}
}
