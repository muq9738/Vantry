package core

import (
	"context"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func TestLaterCohortDoesNotConsumePriorResultsAgain(t *testing.T) {
	p := &routeProvider{fn: func(_ context.Context, request provider.Request, emit func(string) error) error {
		switch request.Message {
		case "child-one":
			return emit("FIRST_RESULT")
		case "child-two":
			return emit("SECOND_RESULT")
		default:
			return emit("parent reply")
		}
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "root")
	parent := mustSend(t, c, "parent", threadID, "parent initial")
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	for index, message := range []string{"child-one", "child-two"} {
		current := latestAttempt(t, c, parent.RunID)
		receipt := dispatchOrFail(t, c, delegateCommand(message, parent.RunID, current.AttemptID, message, "auto"))
		waitTaskDelivery(t, c, receipt.TaskID, api.DeliveryAcknowledged)
		waitRunStatus(t, c, parent.RunID, api.RunCompleted)
		if got := attemptCount(t, c, parent.RunID); got != index+2 {
			t.Fatalf("attempts=%d", got)
		}
	}
	requests := p.requestsForRun(parent.RunID)
	if len(requests) != 3 {
		t.Fatalf("requests=%+v", requests)
	}
	last := requests[2].Message
	if !strings.Contains(last, "SECOND_RESULT") || strings.Contains(last, "FIRST_RESULT") {
		t.Fatalf("later cohort replayed an already consumed result: %s", last)
	}
}

func TestFollowupCannotReplaceResultBeforeParentInputIsQueued(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	p := &routeProvider{fn: func(ctx context.Context, request provider.Request, emit func(string) error) error {
		if request.Message == "parent initial" {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return emit("result")
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "root")
	parent := mustSend(t, c, "parent", threadID, "parent initial")
	waitRunStatus(t, c, parent.RunID, api.RunRunning)
	current := latestAttempt(t, c, parent.RunID)
	child := dispatchOrFail(t, c, delegateCommand("a", parent.RunID, current.AttemptID, "child", "auto"))
	waitTaskDelivery(t, c, child.TaskID, api.DeliveryAcknowledged)
	_, err := c.Dispatch(context.Background(), api.Command{Version: 1, CommandID: "too-early", Kind: api.FollowUpTask,
		TaskID: child.TaskID, Generation: 1, Message: "replace result"})
	if api.CodeOf(err) != api.Conflict {
		t.Fatalf("unscheduled parent result was overwritten: %v", err)
	}
	if task := findTask(t, c, child.TaskID); task.Generation != 1 || task.Result != "result" {
		t.Fatalf("result changed: %+v", task)
	}
}

func TestSiblingFollowupsJoinOneNewCohortAndRejectStaleGeneration(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	p := &routeProvider{fn: func(ctx context.Context, request provider.Request, emit func(string) error) error {
		if strings.HasPrefix(request.Message, "follow-") {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return emit("result")
	}}
	c := openTestCore(t, p)
	threadID := mustCreateThread(t, c, "root")
	parent := mustSend(t, c, "parent", threadID, "parent initial")
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	current := latestAttempt(t, c, parent.RunID)
	a := dispatchOrFail(t, c, delegateCommand("a", parent.RunID, current.AttemptID, "child-a", "manual"))
	b := dispatchOrFail(t, c, delegateCommand("b", parent.RunID, current.AttemptID, "child-b", "manual"))
	waitTaskDelivery(t, c, b.TaskID, api.DeliveryDelivered)
	dispatchOrFail(t, c, api.Command{Version: 1, CommandID: "ack", Kind: api.AcknowledgeTask,
		ParentRunID: parent.RunID, Generation: a.Generation, DeliveryMessageID: a.DeliveryMessageID})
	waitRunStatus(t, c, parent.RunID, api.RunCompleted)
	first := dispatchOrFail(t, c, api.Command{Version: 1, CommandID: "follow-a", Kind: api.FollowUpTask,
		TaskID: a.TaskID, Generation: 1, Message: "follow-a"})
	second := dispatchOrFail(t, c, api.Command{Version: 1, CommandID: "follow-b", Kind: api.FollowUpTask,
		TaskID: b.TaskID, Generation: 1, Message: "follow-b"})
	if first.Generation != 2 || second.Generation != 2 || first.DeliveryMessageID != second.DeliveryMessageID {
		t.Fatalf("sibling followups did not share one generation: %+v / %+v", first, second)
	}
	_, err := c.Dispatch(context.Background(), api.Command{Version: 1, CommandID: "stale-follow", Kind: api.FollowUpTask,
		TaskID: a.TaskID, Generation: 1, Message: "stale"})
	if api.CodeOf(err) != api.Conflict {
		t.Fatalf("stale followup accepted: %v", err)
	}
}
