package core

import (
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestDurableEventCommitRetryDoesNotFailOrRepeatExternalRun(t *testing.T) {
	p := &durableProvider{recoverable: true}
	c := openTestCore(t, p)
	var injected atomic.Bool
	c.setTestHookPreCommit(func(tx *sql.Tx) error {
		var cursor int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(provider_cursor), 0) FROM attempts`).Scan(&cursor); err != nil {
			return err
		}
		if cursor > 0 && injected.CompareAndSwap(false, true) {
			return errors.New("temporary event commit failure")
		}
		return nil
	})
	threadID := mustCreateThread(t, c, "root")
	receipt := mustSend(t, c, "send", threadID, "external work")
	final := waitRunStatus(t, c, receipt.RunID, api.RunCompleted)
	if !injected.Load() {
		t.Fatal("event commit failure was not injected")
	}
	if final.Output != "ok" || len(p.requests()) != 1 {
		t.Fatalf("retry repeated execution or output: %+v requests=%d", final, len(p.requests()))
	}
}
