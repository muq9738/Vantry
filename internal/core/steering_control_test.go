package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// The steering contract that survives the merge:
//   - message.steer stays a working alias of run.steer and returns a legacy
//     MessageID as well as the unified InputID;
//   - Pi acknowledging an RPC is not the model consuming the message;
//   - an outcome that cannot be confirmed is unknown and is never resent, not
//     even after a restart;
//   - a late outcome for a settled attempt is still recorded once, without any
//     second handoff.

func steerAliasCommand(commandID, threadID, runID, message string) api.Command {
	return api.Command{
		Version: api.ProtocolVersion, CommandID: commandID, Kind: api.SteerMessage,
		ThreadID: threadID, RunID: runID, Message: message,
	}
}

func TestSteerAliasResolvesAttemptAndKeepsLegacyReceipt(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID

	command := steerAliasCommand("steer-alias", threadID, sent.RunID, "change course")
	receipt, err := c.Dispatch(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy and unified identities name the same row.
	if receipt.InputID == "" || receipt.MessageID != receipt.InputID {
		t.Fatalf("steer receipt lost its ids: %+v", receipt)
	}
	waitFor(t, "control handoff", func() bool { return len(p.controlCalls()) == 1 })
	control := p.controlCalls()[0]
	if control.ID != receipt.InputID || control.Message != "change course" {
		t.Fatalf("unexpected control intent: %+v", control)
	}
	input, ok := findInput(snapshotOf(t, c), receipt.InputID)
	if !ok || input.AttemptID != attemptID || input.Kind != inputKindSteer {
		t.Fatalf("alias did not bind the current attempt: %+v", input)
	}
	// The legacy view of the same row carries the command id and status.
	if steering := snapshotOf(t, c).Steering; len(steering) != 1 ||
		steering[0].CommandID != command.CommandID || steering[0].Status != api.SteeringPending {
		t.Fatalf("legacy steering projection: %+v", steering)
	}

	// An identical retry is a receipt replay: no new input and no new handoff.
	repeated, err := c.Dispatch(context.Background(), command)
	if err != nil || repeated != receipt {
		t.Fatalf("alias retry: %+v %v", repeated, err)
	}
	if calls := len(p.controlCalls()); calls != 1 {
		t.Fatalf("retry handed the control over again: %d", calls)
	}
	changed := command
	changed.Message = "different"
	if _, err := c.Dispatch(context.Background(), changed); api.CodeOf(err) != api.Conflict {
		t.Fatalf("changed payload accepted: %v", err)
	}
	p.finish()
}

func TestUnconfirmedSteerStaysUnknownAcrossRestartWithoutResend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	p := newWorkbenchProvider(t)
	p.recover = true
	p.onControl = func(control provider.Control) *provider.ControlResult {
		return &provider.ControlResult{ID: control.ID, Status: "unknown", Error: "worker lost the acknowledgement"}
	}
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")
	sent := mustSend(t, first, "send-1", threadID, "hello")
	waitRunStatus(t, first, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, first, sent.RunID).AttemptID
	receipt := mustSteer(t, first, "steer-1", threadID, sent.RunID, attemptID, "adjust")
	input := waitInputStatus(t, first, receipt.InputID, inputUnknown)
	if input.Error == "" {
		t.Fatalf("unknown outcome has no reason: %+v", input)
	}
	if len(p.controlCalls()) != 1 {
		t.Fatalf("control handoffs=%d", len(p.controlCalls()))
	}

	// A restart keeps the unconfirmed outcome and never hands it over again.
	crashForTest(first)
	second := newWorkbenchProvider(t)
	second.recover = true
	reopened := openCoreAt(t, path, second)
	defer reopened.Close()
	waitFor(t, "steer input after restart", func() bool {
		current, ok := findInput(snapshotOf(t, reopened), receipt.InputID)
		return ok && current.Status == inputUnknown
	})
	if calls := len(second.controlCalls()); calls != 0 {
		t.Fatalf("restart resent an unconfirmed steer: %d", calls)
	}
}

func TestSteerRejectionKeepsItsReasonAndIsNotResent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	p := newWorkbenchProvider(t)
	p.recover = true
	p.onControl = func(control provider.Control) *provider.ControlResult {
		return &provider.ControlResult{ID: control.ID, Status: "failed", Error: "worker refused the instruction"}
	}
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "create-1")
	sent := mustSend(t, first, "send-1", threadID, "hello")
	waitRunStatus(t, first, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, first, sent.RunID).AttemptID
	receipt := mustSteer(t, first, "steer-1", threadID, sent.RunID, attemptID, "adjust")
	input := waitInputStatus(t, first, receipt.InputID, inputFailed)
	if input.Error != "worker refused the instruction" {
		t.Fatalf("rejection reason lost: %+v", input)
	}

	crashForTest(first)
	second := newWorkbenchProvider(t)
	second.recover = true
	reopened := openCoreAt(t, path, second)
	defer reopened.Close()
	waitFor(t, "steer input after restart", func() bool {
		current, ok := findInput(snapshotOf(t, reopened), receipt.InputID)
		return ok && current.Status == inputFailed
	})
	if calls := len(second.controlCalls()); calls != 0 {
		t.Fatalf("restart resent a refused steer: %d", calls)
	}
}

// TestLateControlResultOnSettledAttemptIsRecordedOnce proves the bounded late
// reconciliation: Pi's answer arrives after the attempt stopped, is applied to
// its own steer, and never causes another handoff.
func TestLateControlResultOnSettledAttemptIsRecordedOnce(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID
	receipt := mustSteer(t, c, "steer-1", threadID, sent.RunID, attemptID, "adjust")
	waitInputStatus(t, c, receipt.InputID, inputPending)
	waitFor(t, "control handoff", func() bool { return len(p.controlCalls()) == 1 })

	// The attempt settles first; the acknowledgement is journaled afterwards.
	if err := c.write(context.Background(), func(t *txn) error {
		if _, err := t.exec(`UPDATE runs SET status = ?, active_attempt_id = '' WHERE id = ?`,
			statusCompleted, sent.RunID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result := provider.Event{
		Type: "control_result",
		Data: mustJSON(t, provider.ControlResult{ID: receipt.InputID, Status: "delivered"}),
	}
	apply := func() {
		if err := c.write(context.Background(), func(t *txn) error {
			run, err := loadRun(t, sent.RunID)
			if err != nil {
				return err
			}
			return c.applyControlResult(t, run, attemptID, result)
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	waitInputStatus(t, c, receipt.InputID, inputDelivered)
	// Replaying the same late result cannot change it again.
	apply()
	if input := waitInputStatus(t, c, receipt.InputID, inputDelivered); input.Error != "" {
		t.Fatalf("delivered steer kept an error: %+v", input)
	}
	if calls := len(p.controlCalls()); calls != 1 {
		t.Fatalf("late result triggered another handoff: %d", calls)
	}
	p.finish()
}

func TestStopCancelsUndeliveredSteerWithoutAnotherHandoff(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	p.onControl = func(provider.Control) *provider.ControlResult { return nil }
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create-1")
	sent := mustSend(t, c, "send-1", threadID, "hello")
	waitRunStatus(t, c, sent.RunID, api.RunRunning)
	attemptID := latestAttempt(t, c, sent.RunID).AttemptID
	receipt := mustSteer(t, c, "steer-1", threadID, sent.RunID, attemptID, "adjust")
	waitFor(t, "control handoff", func() bool { return len(p.controlCalls()) == 1 })

	if _, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "stop", Kind: api.StopRun,
		ThreadID: threadID, RunID: sent.RunID,
	}); err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, c, sent.RunID, api.RunCancelled)
	if input := waitInputStatus(t, c, receipt.InputID, inputFailed); input.Error == "" {
		t.Fatalf("cancelled attempt left a pending steer: %+v", input)
	}
	if calls := len(p.controlCalls()); calls != 1 {
		t.Fatalf("stop handed the steer over again: %d", calls)
	}
	p.finish()
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAttachmentsReachProviderAndControls proves Core keeps only attachment
// identities and carries them into the provider request and the control intent.
func TestAttachmentsReachProviderAndControls(t *testing.T) {
	item := api.Attachment{ID: "att_keep", Name: "note.txt", MIME: "text/plain", Size: 4}
	steerItem := api.Attachment{ID: "att_steer", Name: "extra.txt", MIME: "text/plain", Size: 5}
	validated := 0
	validate := func(items []api.Attachment) error {
		validated++
		for _, candidate := range items {
			if candidate.ID == "att_missing" {
				return &api.Error{Code: api.InvalidCommand, Message: "attachment is unavailable"}
			}
		}
		return nil
	}
	path := filepath.Join(t.TempDir(), "core.db")
	p := newWorkbenchProvider(t)
	p.recover = true
	c, err := Open(context.Background(), Options{DBPath: path, Provider: p, ValidateAttachments: validate})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	threadID := mustCreateThread(t, c, "create-1")
	if _, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "send-1", Kind: api.SendMessage,
		ThreadID: threadID, Message: "hello", Attachments: []api.Attachment{item},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "provider call", func() bool { return p.callCount() == 1 })
	if request := p.calls[0]; !reflect.DeepEqual(request.Attachments, []api.Attachment{item}) {
		t.Fatalf("attachments lost on the initial request: %+v", request.Attachments)
	}

	// A queued message keeps its attachments until it becomes the run.
	runID := latestRun(t, c, threadID).ID
	waitRunStatus(t, c, runID, api.RunRunning)
	attemptID := latestAttempt(t, c, runID).AttemptID
	queued, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "queue-1", Kind: api.QueueMessage,
		ThreadID: threadID, Message: "next", Attachments: []api.Attachment{item},
	})
	if err != nil {
		t.Fatal(err)
	}
	if queued.InputID == "" {
		t.Fatalf("queue receipt lost its input id: %+v", queued)
	}
	if input, ok := findInput(snapshotOf(t, c), queued.InputID); !ok || !reflect.DeepEqual(input.Attachments, []api.Attachment{item}) {
		t.Fatalf("queued attachments lost: %+v", input)
	}

	// The steer carries its own attachments into the control intent.
	receipt := mustSteerWithAttachments(t, c, "steer-1", threadID, runID, attemptID, []api.Attachment{steerItem})
	waitFor(t, "steer handoff", func() bool { return len(p.controlCalls()) == 1 })
	control := p.controlCalls()[0]
	if control.ID != receipt.InputID || !reflect.DeepEqual(control.Attachments, []api.Attachment{steerItem}) {
		t.Fatalf("steer attachments lost: %+v", control)
	}

	// An unavailable attachment is refused before anything is recorded.
	if _, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: "queue-bad", Kind: api.QueueMessage,
		ThreadID: threadID, Message: "bad", Attachments: []api.Attachment{{ID: "att_missing", Name: "gone.txt", MIME: "text/plain", Size: 4}},
	}); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("unavailable attachment accepted: %v", err)
	}
	if validated == 0 {
		t.Fatal("attachment validation was never consulted")
	}
	p.finish()
	waitRunStatus(t, c, runID, api.RunCompleted)
}

func mustSteerWithAttachments(t *testing.T, c *Core, commandID, threadID, runID, attemptID string, items []api.Attachment) api.Receipt {
	t.Helper()
	receipt, err := c.Dispatch(context.Background(), api.Command{
		Version: api.ProtocolVersion, CommandID: commandID, Kind: api.SteerRun,
		ThreadID: threadID, RunID: runID, ParentAttemptID: attemptID, Message: "with files",
		Attachments: items,
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func latestRun(t *testing.T, c *Core, threadID string) api.Run {
	t.Helper()
	var latest api.Run
	for _, run := range snapshotOf(t, c).Runs {
		if run.ThreadID == threadID {
			latest = run
		}
	}
	if latest.ID == "" {
		t.Fatal("thread has no run")
	}
	return latest
}
