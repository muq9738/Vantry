package pi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

func TestProjectlessChatRunsInItsOwnDirectoryAndReattaches(t *testing.T) {
	p, request, fixture := newRPCProvider(t, "chat")
	p.options.DefaultWorkspace = nil
	request.Workspace = nil
	var directory string
	emit := func(event provider.Event) error {
		if event.Type == "fixture_cwd" {
			var record struct{ Path string }
			if err := json.Unmarshal(event.Data, &record); err != nil {
				return err
			}
			directory = record.Path
		}
		return nil
	}
	if err := p.RunEvents(context.Background(), request, emit); err != nil {
		t.Fatalf("projectless chat failed: %v", err)
	}
	planned, err := workspace.ChatSpec(p.options.DataDir, request.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := filepath.EvalSymlinks(planned.Path)
	if err != nil {
		t.Fatal(err)
	}
	if directory != expected {
		t.Fatalf("chat ran in another directory: got %q want %q", directory, expected)
	}
	info, err := os.Stat(expected)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("chat directory is not private: %v %v", info, err)
	}
	if path, err := workspace.ThreadDirectory(context.Background(), p.options.DataDir, nil, request.ThreadID); err != nil || path != expected {
		t.Fatalf("terminal/query resolved another chat directory: %q %v", path, err)
	}
	// Changes to new-conversation defaults or current model selection cannot
	// redirect or reject an already accepted job during observer reattachment.
	p.options.DefaultWorkspace = &api.WorkspaceSpec{Kind: "directory", Path: filepath.Join(p.options.DataDir, "different-project")}
	p.options.DefaultModel = "changed/model"
	p.options.ResolveModel = func(string) (string, error) { return "", errors.New("current selection changed") }
	if err := p.RunEvents(context.Background(), request, emit); err != nil {
		t.Fatalf("accepted job did not retain its resolved defaults: %v", err)
	}
	calls, err := os.ReadFile(fixture.Calls)
	if err != nil || strings.Count(string(calls), "x") != 1 {
		t.Fatalf("chat was resubmitted after default changes: %q %v", calls, err)
	}
}

func TestExplicitMissingProjectDoesNotFallBackToChat(t *testing.T) {
	p, request, fixture := newRPCProvider(t, "chat")
	p.options.DefaultWorkspace = nil
	missing := filepath.Join(p.options.DataDir, "missing-project")
	request.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: missing}
	if err := p.RunEvents(context.Background(), request, func(provider.Event) error { return nil }); err == nil {
		t.Fatal("missing explicit project was silently replaced")
	}
	if _, err := os.Stat(fixture.Calls); !os.IsNotExist(err) {
		t.Fatalf("Pi started after explicit project failed: %v", err)
	}
	chat, _ := workspace.ChatSpec(p.options.DataDir, request.ThreadID)
	if _, err := os.Stat(chat.Path); !os.IsNotExist(err) {
		t.Fatal("explicit project failure created a chat fallback")
	}
}

func TestRejectedModelNeverLaunchesPiOrCreatesChat(t *testing.T) {
	p, request, fixture := newRPCProvider(t, "chat")
	p.options.DefaultWorkspace = nil
	p.options.ResolveModel = func(string) (string, error) {
		return "", &api.Error{Code: api.InvalidCommand, Message: "model is outside Magpie selection"}
	}
	if err := p.RunEvents(context.Background(), request, func(provider.Event) error { return nil }); api.CodeOf(err) != api.InvalidCommand {
		t.Fatalf("scope rejection lost: %v", err)
	}
	if _, err := os.Stat(fixture.Calls); !os.IsNotExist(err) {
		t.Fatal("rejected model reached Pi")
	}
	chat, _ := workspace.ChatSpec(p.options.DataDir, request.ThreadID)
	if _, err := os.Stat(chat.Path); !os.IsNotExist(err) {
		t.Fatal("rejected model prepared an execution directory")
	}
}
