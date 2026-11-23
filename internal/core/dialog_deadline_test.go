package core

import (
	"context"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestPiDialogDeadlineUsesJournalTimeWithoutInventingExpiry(t *testing.T) {
	p := newWorkbenchProvider(t)
	p.recover = true
	p.add("extension_ui_request", map[string]any{"id": "no-deadline", "method": "confirm", "title": "Allow bash?"})
	p.add("extension_ui_request", map[string]any{"id": "expired", "method": "confirm", "timeout": 1000})
	p.add("extension_ui_request", map[string]any{"id": "text", "method": "input", "placeholder": "Type a value"})
	// These events were produced while Core was unavailable. Replaying them
	// cannot renew a deadline or expire a dialog that Pi is still awaiting.
	occurred := time.Now().Add(-time.Hour).UTC()
	for i := range p.script {
		p.script[i].OccurredAt = occurred
	}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "create")
	run := mustSend(t, c, "send", threadID, "test")
	waitRunStatus(t, c, run.RunID, api.RunRunning)
	attempt := latestAttempt(t, c, run.RunID).AttemptID
	waitRequestStatus(t, c, attempt+":text", requestPending)
	waitRequestStatus(t, c, attempt+":expired", requestExpired)
	waitRequestStatus(t, c, attempt+":no-deadline", requestPending)
	var noDeadline, expired int64
	if err := c.store.db.QueryRow(`SELECT expires_at FROM provider_requests WHERE id = ?`, attempt+":no-deadline").Scan(&noDeadline); err != nil {
		t.Fatal(err)
	}
	if err := c.store.db.QueryRow(`SELECT expires_at FROM provider_requests WHERE id = ?`, attempt+":expired").Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if noDeadline != 0 || expired != occurred.Add(time.Second).UnixNano() {
		t.Fatalf("provider lifetime changed on replay: noDeadline=%d expired=%d", noDeadline, expired)
	}
	text, _ := findRequest(snapshotOf(t, c), attempt+":text")
	if text.Prefill != "" {
		t.Fatalf("placeholder became a response value: %+v", text)
	}
	_, err := c.Dispatch(context.Background(), api.Command{Version: api.ProtocolVersion,
		CommandID: "late", Kind: api.RespondRequest, RequestID: attempt + ":expired", Confirmed: true})
	requireCode(t, err, api.Conflict)
	mustRespond(t, c, "old-dialog-answer", attempt+":no-deadline", "", true, false)
	mustRespond(t, c, "empty-input-answer", attempt+":text", "", false, false)
	waitFor(t, "both live dialogs handed off", func() bool { return len(p.controlCalls()) == 2 })
	p.finish()
	waitRunStatus(t, c, run.RunID, api.RunCompleted)
}
