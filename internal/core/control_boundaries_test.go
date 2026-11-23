package core

import (
	"context"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func TestPiRouterRejectsControlledSteerBeforeRecordingIntent(t *testing.T) {
	for _, route := range []string{pi.ProviderControlled, ""} {
		t.Run("route="+route, func(t *testing.T) {
			// Empty run configuration must also honor the router's default route.
			p := pi.New(pi.Options{DefaultProviderID: pi.ProviderControlled, ControlledDelay: time.Hour})
			c := openTestCore(t, p)
			thread := dispatchOrFail(t, c, api.Command{
				Version: api.ProtocolVersion, CommandID: "create", Kind: api.CreateThread, ProviderID: route,
			})
			sent := mustSend(t, c, "send", thread.ThreadID, "hello")
			run := waitRunStatus(t, c, sent.RunID, api.RunRunning)
			inputsBefore := countRows(t, c, "inputs")
			effectsBefore := countRows(t, c, "effect_outbox")
			receiptsBefore := countRows(t, c, "command_receipts")
			_, err := c.Dispatch(context.Background(), api.Command{
				Version: api.ProtocolVersion, CommandID: "steer", Kind: api.SteerRun,
				ThreadID: thread.ThreadID, RunID: run.ID, ParentAttemptID: run.AttemptID, Message: "go left",
			})
			requireCode(t, err, api.InvalidCommand)
			if countRows(t, c, "inputs") != inputsBefore || countRows(t, c, "effect_outbox") != effectsBefore ||
				countRows(t, c, "command_receipts") != receiptsBefore {
				t.Fatal("unsupported steer recorded an input, control effect or receipt")
			}
		})
	}
}

func TestControlResultOnlyChangesItsOwnRunAndAttempt(t *testing.T) {
	for _, kind := range []string{"steer", "request"} {
		for _, tc := range []struct {
			name, runID, attemptID, status string
			missing                        bool
			want                           string
		}{
			{name: "other run", runID: "run_other", want: "pending"},
			{name: "other attempt", attemptID: "att_old", want: "pending"},
			{name: "missing ID", missing: true, want: "pending"},
			{name: "already settled", status: "failed", want: "failed"},
			{name: "matching", want: "delivered"},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				p := newWorkbenchProvider(t)
				c := openTestCore(t, p)
				threadID := mustCreateThread(t, c, "create")
				sent := mustSend(t, c, "send", threadID, "hello")
				run := waitRunStatus(t, c, sent.RunID, api.RunRunning)
				runID, attemptID := run.ID, run.AttemptID
				if tc.runID != "" {
					runID = tc.runID
				}
				if tc.attemptID != "" {
					attemptID = tc.attemptID
				}
				status := firstNonEmpty(tc.status, "pending")
				const targetID = "control_target"
				if err := c.write(context.Background(), func(tx *txn) error {
					if kind == "steer" {
						return c.insertInput(tx, inputRecord{
							ID: targetID, ThreadID: threadID, RunID: runID, AttemptID: attemptID,
							Kind: inputKindSteer, Message: "test", Status: status,
						})
					}
					return c.insertRequest(tx, requestRecord{
						ID: targetID, ProviderRequestID: "ui_target", ThreadID: threadID,
						RunID: runID, AttemptID: attemptID, Method: "confirm", Status: status,
					})
				}); err != nil {
					t.Fatalf("seed target: %v", err)
				}
				eventsBefore := countRows(t, c, "events")
				resultID := targetID
				if tc.missing {
					resultID = "missing_control"
				}
				ev := p.add("control_result", provider.ControlResult{ID: resultID, Status: "delivered"})
				// Wait on the ingestion cursor, so an ignored result is tested only
				// after it has actually passed through the provider event transaction.
				waitFor(t, "control result ingestion", func() bool {
					var cursor int64
					err := c.store.db.QueryRow(`SELECT provider_cursor FROM attempts WHERE id = ?`, run.AttemptID).Scan(&cursor)
					return err == nil && cursor >= ev.Sequence
				})
				want := tc.want
				if kind == "request" && want == "delivered" {
					want = requestResponded
				}
				if kind == "steer" {
					waitInputStatus(t, c, targetID, want)
				} else {
					waitRequestStatus(t, c, targetID, want)
				}
				if tc.want != "delivered" && countRows(t, c, "events") != eventsBefore {
					t.Fatal("unrelated or expired control result emitted a projection update")
				}
				p.finish()
				waitRunStatus(t, c, sent.RunID, api.RunCompleted)
			})
		}
	}
}
