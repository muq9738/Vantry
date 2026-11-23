package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

type commandRecorder struct{ commands []api.Command }

func (r *commandRecorder) Health(context.Context) (api.Health, error) {
	return api.Health{Version: 1, State: "ready"}, nil
}
func (r *commandRecorder) Snapshot(context.Context) (api.Snapshot, error) {
	return api.Snapshot{}, nil
}
func (r *commandRecorder) Subscribe(context.Context, int64) (*api.Subscription, error) {
	return nil, errors.New("unused in this test")
}
func (r *commandRecorder) Dispatch(_ context.Context, command api.Command) (api.Receipt, error) {
	r.commands = append(r.commands, command)
	return api.Receipt{CommandID: command.CommandID, Accepted: true, ThreadID: command.ThreadID, Sequence: 1}, nil
}

func TestCreateRetryKeepsCompletePayload(t *testing.T) {
	recorder := &commandRecorder{}
	args := []string{"create", "--command-id", "retry-create", "--title", "可重试线程"}
	var first, second bytes.Buffer
	if err := control(context.Background(), recorder, args, &first, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := control(context.Background(), recorder, args, &second, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(recorder.commands) != 2 || !reflect.DeepEqual(recorder.commands[0], recorder.commands[1]) || recorder.commands[0].ThreadID == "" {
		t.Fatalf("CLI generated a different payload for the same command: %+v", recorder.commands)
	}
	if first.String() != second.String() {
		t.Fatalf("retry changed its JSON receipt: %s != %s", first.String(), second.String())
	}
}
