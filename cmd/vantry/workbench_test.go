package main

import (
	"context"
	"io"
	"reflect"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestInputCLIForwardsIdentityAndExplicitApproval(t *testing.T) {
	for _, test := range []struct {
		args  []string
		check func(api.Command) bool
	}{
		{[]string{"queue", "--thread", "thread", "--message", "next"}, func(c api.Command) bool { return c.Kind == api.QueueMessage && c.Message == "next" }},
		{[]string{"cancel-input", "--input", "queued-id"}, func(c api.Command) bool { return c.Kind == api.CancelInput && c.InputID == "queued-id" }},
		{[]string{"steer", "--thread", "thread", "--run", "run", "--attempt", "attempt", "--message", "change"}, func(c api.Command) bool {
			return c.Kind == api.SteerRun && c.RunID == "run" && c.ParentAttemptID == "attempt" && c.Message == "change"
		}},
		{[]string{"respond", "--request", "attempt:request", "--confirm"}, func(c api.Command) bool {
			return c.Kind == api.RespondRequest && c.RequestID == "attempt:request" && c.Confirmed
		}},
		{[]string{"respond", "--request", "attempt:request", "--confirm=false"}, func(c api.Command) bool { return c.Kind == api.RespondRequest && !c.Confirmed && !c.Cancelled }},
		{[]string{"respond", "--request", "attempt:request", "--value", "选择"}, func(c api.Command) bool { return c.Value == "选择" }},
		{[]string{"respond", "--request", "attempt:request", "--cancel"}, func(c api.Command) bool { return c.Cancelled }},
		{[]string{"create", "--thread", "thread", "--approval", "ask"}, func(c api.Command) bool { return c.ApprovalPolicy == "ask" }},
	} {
		recorder := &commandRecorder{}
		args := append(append([]string{}, test.args...), "--command-id", "stable-id")
		if err := control(context.Background(), recorder, args, io.Discard, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if err := control(context.Background(), recorder, args, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		if len(recorder.commands) != 2 || !test.check(recorder.commands[0]) || !reflect.DeepEqual(recorder.commands[0], recorder.commands[1]) {
			t.Fatalf("payload or retry changed for %v: %+v", args, recorder.commands)
		}
	}
}

func TestInputCLIRejectsMissingTargetOrResponse(t *testing.T) {
	for _, args := range [][]string{
		{"queue", "--message", "missing thread"},
		{"cancel-input"},
		{"steer", "--thread", "t", "--attempt", "a", "--message", "missing run"},
		{"respond", "--request", "id"},
		{"respond", "--confirm"},
		{"create", "--approval", "invalid"},
	} {
		recorder := &commandRecorder{}
		if err := control(context.Background(), recorder, args, io.Discard, io.Discard); err == nil || len(recorder.commands) != 0 {
			t.Fatalf("invalid command dispatched: %v %+v", args, recorder.commands)
		}
	}
}
