package core

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestDisposingLastResultReleasesParentAndRepairsOldWaitingState(t *testing.T) {
	p := &routeProvider{}
	path := filepath.Join(t.TempDir(), "state.sqlite")
	first := openCoreAt(t, path, p)
	threadID := mustCreateThread(t, first, "root")
	parent := mustSend(t, first, "parent", threadID, "initial")
	waitRunStatus(t, first, parent.RunID, api.RunCompleted)
	current := latestAttempt(t, first, parent.RunID)
	child := dispatchOrFail(t, first, delegateCommand("child", parent.RunID, current.AttemptID, "review", "manual"))
	task := waitTaskDelivery(t, first, child.TaskID, api.DeliveryDelivered)
	dispatchOrFail(t, first, api.Command{Version: 1, CommandID: "dispose", Kind: api.DisposeTask,
		TaskID: task.ID, Generation: task.Generation, DeliveryMessageID: task.DeliveryMessageID})
	waitRunStatus(t, first, parent.RunID, api.RunCompleted)
	if got := attemptCount(t, first, parent.RunID); got != 1 {
		t.Fatalf("disposed result started a provider continuation: %d", got)
	}
	// Reconstruct the previously valid-but-stuck projection to verify startup
	// compensation, independently of the fixed live dispose command.
	if err := first.write(context.Background(), func(tx *txn) error {
		_, err := tx.exec(`UPDATE runs SET status = ? WHERE id = ?`, statusWaitingChildren, parent.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := openCoreAt(t, path, p)
	defer second.Close()
	waitRunStatus(t, second, parent.RunID, api.RunCompleted)
	next := mustSend(t, second, "next", threadID, "continue conversation")
	waitRunStatus(t, second, next.RunID, api.RunCompleted)
}
