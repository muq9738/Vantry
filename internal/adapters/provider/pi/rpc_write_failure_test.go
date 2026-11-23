package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/provider"
)

type partialControlPipe struct {
	io.WriteCloser
	writes int
}

func (p *partialControlPipe) Write(data []byte) (int, error) {
	var record struct{ Type string }
	if json.Unmarshal(data, &record) == nil && record.Type == "steer" {
		p.writes++
		n, err := p.WriteCloser.Write(data[:8])
		if err != nil {
			return n, err
		}
		return n, errors.New("simulated pipe failure after partial steering")
	}
	return p.WriteCloser.Write(data)
}

func TestRPCPartialControlWriteIsUnknownAndNotResubmitted(t *testing.T) {
	root := t.TempDir()
	p, request := newTestProvider(t, root, "unused-fixture")
	jobDir := filepath.Join(root, "jobs", request.AttemptID)
	if err := makePrivateDir(jobDir); err != nil {
		t.Fatal(err)
	}
	job := jobRequest{Request: request, DataDir: root}
	if err := writeJSONFile(filepath.Join(jobDir, "request.json"), job); err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(filepath.Join(jobDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	input, writeInput := io.Pipe()
	defer input.Close()
	output, writeOutput := io.Pipe()
	defer output.Close()
	pipe := &partialControlPipe{WriteCloser: writeInput}
	ready := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		defer writeOutput.Close()
		reader := bufio.NewReader(input)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if errors.Is(err, io.EOF) {
					serverDone <- nil
				} else {
					serverDone <- err
				}
				return
			}
			var record struct{ Type, ID string }
			if err := json.Unmarshal(line, &record); err != nil {
				serverDone <- err
				return
			}
			data := map[string]any{"isStreaming": false}
			if record.Type == "prompt" {
				data = map[string]any{"disposition": "started"}
			}
			if err := json.NewEncoder(writeOutput).Encode(map[string]any{"type": "response", "id": record.ID, "command": record.Type, "success": true, "data": data}); err != nil {
				serverDone <- err
				return
			}
			if record.Type == "prompt" {
				close(ready)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := runRPC(ctx, job, jobDir, pipe, output, journal); result <- err }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("fixture did not accept initial prompt")
	}
	control := provider.Control{ID: "partial-control", Kind: "steer", Message: "adjust"}
	if err := p.Control(ctx, request, control); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("partial write was treated as successful")
		}
	case <-ctx.Done():
		t.Fatal("partial RPC write did not settle")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("fixture pipe was not closed")
	}
	var events []provider.Event
	if _, err := emitCommitted(jobDir, 1, func(event provider.Event) error { events = append(events, event); return nil }); err != nil {
		t.Fatal(err)
	}
	outcomes := controlResults(t, events)
	if outcome := outcomes[control.ID]; outcome.Status != controlStatusUnknown {
		t.Fatalf("partial write was falsely classified as unsent: %+v", outcome)
	}
	if pipe.writes != 1 {
		t.Fatalf("partial control was resubmitted: writes=%d", pipe.writes)
	}
}
