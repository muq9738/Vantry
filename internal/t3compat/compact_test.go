package t3compat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// Mirrors restoreLocalVisibleTurnItems: only the duplicate turnItems prefix is
// omitted on the wire, and local visible rows are prepended before reduction.
func restoreCompactSnapshot(t *testing.T, frame Object) Object {
	t.Helper()
	projection := normalized(t, frame["projection"])
	if frame["turnItemsOmitLocalVisible"] != true {
		return projection
	}
	items := []any{}
	for _, value := range rows(projection["visibleTurnItems"]) {
		row := object(value)
		if row["visibility"] == "local" || row["sourceThreadId"] == object(projection["thread"])["id"] {
			items = append(items, row["item"])
		}
	}
	projection["turnItems"] = append(items, rows(projection["turnItems"])...)
	return projection
}

func TestCompactProjectionRestoresExactStateWithoutMutation(t *testing.T) {
	s, snapshot := projectionFixture(t)
	projection, err := s.project(snapshot, "thread")
	if err != nil {
		t.Fatal(err)
	}
	original := normalized(t, projection)
	compact, ok := compactThreadProjection(projection)
	if !ok || len(rows(compact["turnItems"])) != 0 {
		t.Fatal("expected duplicate local items to be omitted")
	}
	frame := Object{"projection": compact, "turnItemsOmitLocalVisible": true}
	if !reflect.DeepEqual(restoreCompactSnapshot(t, frame), original) {
		t.Fatal("compaction lost state")
	}
	if !reflect.DeepEqual(normalized(t, projection), original) {
		t.Fatal("authoritative projection mutated")
	}
	fullBytes, _ := json.Marshal(Object{"projection": projection})
	compactBytes, _ := json.Marshal(frame)
	if len(compactBytes) >= len(fullBytes) {
		t.Fatal("no byte savings")
	}
	t.Logf("snapshot projection envelope: %d -> %d bytes (%.1f%% saved)", len(fullBytes), len(compactBytes), 100*(1-float64(len(compactBytes))/float64(len(fullBytes))))
	// Optional artifact for running the actual upstream reconstruction function.
	if path := os.Getenv("VANTRY_COMPACT_FIXTURE"); path != "" {
		data, err := json.Marshal(Object{"full": projection, "frame": frame})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Clean(path), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompactProjectionPreservesToolsRequestsAndQueuedInputs(t *testing.T) {
	s, snapshot := projectionFixture(t)
	at := snapshot.Runs[40].UpdatedAt
	snapshot.Runs[40].Reasoning = "reasoning 中文"
	snapshot.Tools = []api.ToolCall{{ID: "tool", ThreadID: "thread", RunID: "run-40", Name: "read", Arguments: json.RawMessage(`{"path":"example.txt"}`), Output: strings.Repeat("large tool output 中文\n", 4000), Status: "completed", UpdatedAt: at}}
	snapshot.Requests = []api.ProviderRequest{{ID: "approval", ThreadID: "thread", RunID: "run-40", Method: "confirm", Message: "Allow edit", Status: "pending", CreatedAt: at, UpdatedAt: at}}
	snapshot.Inputs = []api.Input{{ID: "queued", ThreadID: "thread", Kind: "queue", Message: "Next turn", Status: "queued", CreatedAt: at, UpdatedAt: at}, {ID: "steer", ThreadID: "thread", RunID: "run-40", Kind: "steer", Message: "Adjustment", Status: "delivered", CreatedAt: at, UpdatedAt: at}}
	projection, err := s.project(snapshot, "thread")
	if err != nil {
		t.Fatal(err)
	}
	compact, ok := compactThreadProjection(projection)
	if !ok {
		t.Fatal("mixed projection did not compact")
	}
	frame := Object{"projection": compact, "turnItemsOmitLocalVisible": true}
	if !reflect.DeepEqual(restoreCompactSnapshot(t, frame), normalized(t, projection)) {
		t.Fatal("mixed projection lost data or ordering")
	}
	if path := os.Getenv("VANTRY_COMPACT_FIXTURE"); path != "" {
		data, err := json.Marshal(Object{"full": projection, "frame": frame})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Clean(path)+"-mixed", data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompactProjectionPrefixAndLocalRowRules(t *testing.T) {
	first := Object{"id": "one", "text": "first"}
	second := Object{"id": "two", "text": "second"}
	remote := Object{"id": "remote", "text": "inherited"}
	newProjection := func() Object {
		return Object{"thread": Object{"id": "thread"}, "turnItems": []any{first, second}, "visibleTurnItems": []any{Object{"visibility": "inherited", "sourceThreadId": "other", "item": remote}, Object{"visibility": "inherited", "sourceThreadId": "thread", "item": first}}}
	}
	projection := newProjection()
	compact, ok := compactThreadProjection(projection)
	if !ok || len(rows(compact["turnItems"])) != 1 {
		t.Fatal("partial prefix not retained")
	}
	if !reflect.DeepEqual(restoreCompactSnapshot(t, Object{"projection": compact, "turnItemsOmitLocalVisible": true}), normalized(t, projection)) {
		t.Fatal("partial prefix or inherited rows lost")
	}
	for _, kind := range []string{"order", "body", "too-many", "empty", "no-local"} {
		t.Run(kind, func(t *testing.T) {
			p := newProjection()
			row := object(rows(p["visibleTurnItems"])[1])
			switch kind {
			case "order":
				row["item"] = second
			case "body":
				row["item"] = Object{"id": "one", "text": "stale"}
			case "too-many":
				p["turnItems"] = []any{}
			case "empty":
				p["visibleTurnItems"] = []any{}
				p["turnItems"] = []any{}
			case "no-local":
				row["sourceThreadId"] = "other"
			}
			if _, ok := compactThreadProjection(p); ok {
				t.Fatal("unsafe or empty prefix compacted")
			}
		})
	}
}

func TestCompactSubscriptionNegotiationFallbackDeltaAndReconnect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offer   any
		compact bool
	}{
		{"legacy", nil, false}, {"declined", false, false}, {"string-is-not-opt-in", "true", false}, {"opted-in", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, service, httpServer := testServer(t)
			_, initial := projectionFixture(t)
			stub := &projectionService{Service: service, snapshot: initial, events: make(chan api.Event, 8), subscriptionClosed: make(chan struct{}, 4)}
			server.service = stub
			token := pairClient(t, server, httpServer.URL)
			conn := openRPC(t, httpServer.URL, token)
			input := Object{"threadId": "thread"}
			if tc.offer != nil {
				input["acceptCompactTurnItems"] = tc.offer
			}
			sendRPC(t, conn, "live", "orchestration.subscribeThread", input)
			frame := object(rows(responseRPC(t, conn, "live")["values"])[0])
			verifySnapshot := func(frame Object) Object {
				t.Helper()
				if frame["kind"] != "snapshot" {
					t.Fatal("expected snapshot")
				}
				_, marked := frame["turnItemsOmitLocalVisible"]
				if marked != tc.compact || tc.compact && frame["turnItemsOmitLocalVisible"] != true {
					t.Fatal("incorrect capability negotiation")
				}
				stub.mu.Lock()
				want, err := server.project(stub.snapshot, "thread")
				stub.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				restored := restoreCompactSnapshot(t, frame)
				if !reflect.DeepEqual(restored, normalized(t, want)) {
					t.Fatal("snapshot not losslessly restored")
				}
				return restored
			}
			state := verifySnapshot(frame)
			cursor := number(frame["snapshotSequence"])
			responseRPC(t, conn, "live")
			stub.mu.Lock()
			stub.snapshot.Sequence++
			stub.snapshot.Runs[40].Output += " new output"
			stub.snapshot.Runs[40].UpdatedAt = stub.snapshot.Runs[40].UpdatedAt.Add(time.Second)
			want, _ := server.project(stub.snapshot, "thread")
			sequence := stub.snapshot.Sequence
			stub.mu.Unlock()
			stub.events <- api.Event{Sequence: sequence, ThreadID: "thread"}
			values := rows(responseRPC(t, conn, "live")["values"])
			changes := make([]Object, len(values))
			for i, value := range values {
				changes[i] = object(value)
				if changes[i]["kind"] != "event" {
					t.Fatal("compacted baseline damaged incremental comparison")
				}
				if _, marked := changes[i]["turnItemsOmitLocalVisible"]; marked {
					t.Fatal("snapshot marker leaked to delta")
				}
			}
			state = applyThreadChanges(t, state, changes, cursor)
			cursor = number(changes[len(changes)-1]["sequence"])
			if !reflect.DeepEqual(state, normalized(t, want)) {
				t.Fatal("delta after compact snapshot lost data")
			}
			stub.mu.Lock()
			stub.snapshot.Sequence++
			stub.snapshot.Runs[40].Status = api.RunCompleted
			sequence = stub.snapshot.Sequence
			stub.mu.Unlock()
			stub.events <- api.Event{Sequence: sequence, ThreadID: "thread"}
			frame = object(rows(responseRPC(t, conn, "live")["values"])[0])
			state = verifySnapshot(frame)
			if number(frame["snapshotSequence"]) <= cursor {
				t.Fatal("fallback sequence regressed")
			}
			cursor = number(frame["snapshotSequence"])
			_ = conn.Close()
			select {
			case <-stub.subscriptionClosed:
			case <-time.After(time.Second):
				t.Fatal("subscription did not close")
			}
			resumed := openRPC(t, httpServer.URL, token)
			input["afterSequence"] = cursor
			sendRPC(t, resumed, "resume", "orchestration.subscribeThread", input)
			frame = object(rows(responseRPC(t, resumed, "resume")["values"])[0])
			if !reflect.DeepEqual(verifySnapshot(frame), state) {
				t.Fatal("reconnect changed state")
			}
		})
	}
}
