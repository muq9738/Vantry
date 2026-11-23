package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestDelegateCLIForwardsParentOwnershipAndWorkspace(t *testing.T) {
	recorder := &commandRecorder{}
	repository := t.TempDir()
	err := control(context.Background(), recorder, []string{"delegate", "--command-id", "delegate-once",
		"--parent-run", "parent", "--parent-attempt", "attempt", "--message", "review cancellation",
		"--provider", "pi", "--model", "example-provider/deepseek-v4.1-flash",
		"--resume-policy", "manual", "--workspace-kind", "worktree", "--repository", repository,
		"--branch", "vantry/review"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorder.commands) != 1 {
		t.Fatal("delegate was not dispatched")
	}
	command := recorder.commands[0]
	if command.Kind != api.DelegateTask || command.ParentRunID != "parent" || command.ParentAttemptID != "attempt" || command.ResumePolicy != "manual" {
		t.Fatalf("lost task ownership: %+v", command)
	}
	if command.Workspace == nil || command.Workspace.Repository != filepath.Clean(repository) || command.Workspace.Kind != "worktree" || command.Workspace.Branch != "vantry/review" {
		t.Fatalf("lost workspace configuration: %+v", command.Workspace)
	}
}

func TestDeliveryCLIRejectsIncompleteIdentity(t *testing.T) {
	for _, arguments := range [][]string{
		{"ack", "--parent-run", "parent", "--generation", "2"},
		{"ack", "--parent-run", "parent", "--delivery", "message"},
		{"dispose", "--task", "child", "--generation", "2"},
		{"delegate", "--message", "no parent"},
	} {
		recorder := &commandRecorder{}
		if err := control(context.Background(), recorder, arguments, io.Discard, io.Discard); err == nil {
			t.Errorf("accepted incomplete command %v", arguments)
		}
		if len(recorder.commands) != 0 {
			t.Errorf("invalid command reached Core: %+v", recorder.commands)
		}
	}
}
