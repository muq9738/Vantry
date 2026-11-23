package main

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestSteerCLIKeepsIdentityAndRunningAttemptTarget(t *testing.T) {
	recorder := &commandRecorder{}
	args := []string{"steer", "--command-id", "steer-once", "--thread", "thread-a", "--run", "run-a", "--message", "先检查取消操作"}
	for i := 0; i < 2; i++ {
		if err := control(context.Background(), recorder, args, &bytes.Buffer{}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if len(recorder.commands) != 2 || !reflect.DeepEqual(recorder.commands[0], recorder.commands[1]) {
		t.Fatalf("steering retry changed payload: %+v", recorder.commands)
	}
	command := recorder.commands[0]
	if command.Kind != api.SteerMessage || command.ThreadID != "thread-a" || command.RunID != "run-a" || command.Message != "先检查取消操作" || command.CommandID != "steer-once" {
		t.Fatalf("steering lost target identity: %+v", command)
	}
}

func TestSteerCLIRejectsIncompleteTarget(t *testing.T) {
	for _, args := range [][]string{{"steer", "--thread", "t", "--message", "text"}, {"steer", "--run", "r", "--message", "text"}, {"steer", "--thread", "t", "--run", "r", "--message", "  "}} {
		recorder := &commandRecorder{}
		if err := control(context.Background(), recorder, args, io.Discard, io.Discard); err == nil {
			t.Errorf("accepted incomplete steering: %v", args)
		}
		if len(recorder.commands) != 0 {
			t.Fatal("invalid steering reached service")
		}
	}
}
