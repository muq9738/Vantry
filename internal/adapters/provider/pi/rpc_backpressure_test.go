package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/provider"
)

func TestRPCBackpressureKeepsReadingDuringPromptAndSteerWrites(t *testing.T) {
	p, request, fixture := newRPCProvider(t, "backpressure")
	request.Message = strings.Repeat("initial prompt ", 100000)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	err := p.RunEvents(ctx, request, func(event provider.Event) error {
		if event.Delta == "ready\u2028line" {
			cancel()
		}
		return nil
	})
	cancel()
	if !errors.Is(err, provider.ErrDetached) {
		t.Fatalf("prompt stalled behind provider stdout: %v", err)
	}
	control := provider.Control{ID: "large-steer", Kind: "steer", Message: strings.Repeat("steering update ", 100000)}
	if err := p.Control(context.Background(), request, control); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var events []provider.Event
	if err := p.RunEvents(ctx, request, func(event provider.Event) error { events = append(events, event); return nil }); err != nil {
		t.Fatalf("steer stalled behind provider stdout: %v", err)
	}
	results := controlResults(t, events)
	if result := results[control.ID]; result.Status != "delivered" {
		t.Fatalf("large control result: %+v", result)
	}
	file, err := os.Open(fixture.Commands)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	counts := map[string]int{}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 {
			break
		}
		var record struct{ Type, Message string }
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		counts[record.Type]++
		if record.Type == "prompt" && record.Message != request.Message {
			t.Fatal("initial prompt was partial or changed")
		}
		if record.Type == "steer" && record.Message != control.Message {
			t.Fatal("steering was partial or changed")
		}
		if err != nil {
			break
		}
	}
	if counts["get_state"] != 1 || counts["prompt"] != 1 || counts["steer"] != 1 {
		t.Fatalf("RPC commands repeated under backpressure: %+v", counts)
	}
}
