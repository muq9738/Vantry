package t3compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/muq9738/Vantry/pkg/api"
)

func projectionFixture(t *testing.T) (*Server, api.Snapshot) {
	t.Helper()
	s := &Server{opts: Options{DataDir: "/workspace", Provider: "pi", Model: "test"}}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snapshot := api.Snapshot{Sequence: 100, Threads: []api.Thread{{ID: "thread", Title: "history", CreatedAt: at}}}
	for i := range 41 {
		snapshot.Runs = append(snapshot.Runs, api.Run{ID: fmt.Sprintf("run-%d", i), ThreadID: "thread", Status: api.RunCompleted, Input: "question", Output: strings.Repeat("historical answer 中文 ", 100), CreatedAt: at.Add(time.Duration(i) * time.Minute), UpdatedAt: at.Add(time.Duration(i) * time.Minute)})
	}
	snapshot.Runs[40].Status = api.RunRunning
	return s, snapshot
}

func normalized(t *testing.T, value any) Object {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result Object
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// Apply the protocol's replacement semantics, including the local visible item
// mirror. Validate full reconstructed state, not just the list of changed IDs.
func applyThreadChanges(t *testing.T, projection Object, changes []Object, cursor int64) Object {
	t.Helper()
	result := normalized(t, projection)
	keys := map[string]string{"run.updated": "runs", "node.updated": "nodes", "runtime-request.updated": "runtimeRequests", "message.updated": "messages", "turn-item.updated": "turnItems"}
	for _, change := range changes {
		sequence := number(change["sequence"])
		if sequence <= cursor {
			t.Fatal("non-increasing cursor")
		}
		cursor = sequence
		event := object(change["event"])
		payload := normalized(t, event["payload"])
		if event["type"] == "thread.metadata-updated" {
			result["thread"] = payload
		} else {
			key := keys[text(event, "type")]
			if key == "" {
				t.Fatal(event)
			}
			found := false
			for i, row := range rows(result[key]) {
				if text(object(row), "id") == text(payload, "id") {
					result[key].([]any)[i] = payload
					found = true
					break
				}
			}
			if !found {
				t.Fatal("unknown row")
			}
			if key == "turnItems" {
				for _, row := range rows(result["visibleTurnItems"]) {
					if text(object(row), "sourceItemId") == text(payload, "id") {
						object(row)["item"] = payload
					}
				}
			}
		}
		result["updatedAt"] = event["occurredAt"]
	}
	return result
}

func TestProjectionDeltaRoundTripAndSize(t *testing.T) {
	s, snapshot := projectionFixture(t)
	before, err := s.project(snapshot, "thread")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Sequence += 10
	snapshot.Runs[40].Output += "new token 中文"
	snapshot.Runs[40].UpdatedAt = snapshot.Runs[40].UpdatedAt.Add(time.Second)
	after, err := s.project(snapshot, "thread")
	if err != nil {
		t.Fatal(err)
	}
	changes, ok := projectionDelta(before, after, 100, snapshot.Sequence, false)
	if !ok || len(changes) == 0 {
		t.Fatal("expected incremental updates")
	}
	actual := applyThreadChanges(t, before, changes, 100)
	if !reflect.DeepEqual(actual, normalized(t, after)) {
		t.Fatal("incremental reconstruction differs from authoritative projection")
	}
	full, _ := json.Marshal(after)
	delta, _ := json.Marshal(changes)
	if len(delta) >= len(full)/4 {
		t.Fatalf("delta too large: %d/%d", len(delta), len(full))
	}
	t.Logf("41-run projection: %d -> %d bytes (%.1f%%), %d row events", len(full), len(delta), 100*float64(len(delta))/float64(len(full)), len(changes))
	if len(changes) != 2 {
		t.Fatalf("assistant text should change exactly two rows, got %d", len(changes))
	}
	for _, change := range changes {
		if text(object(object(change["event"])["payload"]), "id") != "run-40/assistant" {
			t.Fatal("user message changed during assistant output")
		}
	}
	for _, gap := range []int64{1, 2, 3} {
		delta, ok := projectionDelta(before, after, 100, 100+gap, false)
		if !ok || len(delta) != 2 {
			t.Fatalf("gap %d rejected a lossless delta", gap)
		}
		if !reflect.DeepEqual(applyThreadChanges(t, before, delta, 100), normalized(t, after)) {
			t.Fatalf("gap %d lost projected state", gap)
		}
	}
}

func TestProjectionDeltaFallsBackForStructuralChanges(t *testing.T) {
	for _, kind := range []string{"remove", "reorder", "visibility", "run-transition", "new-field"} {
		t.Run(kind, func(t *testing.T) {
			s, snapshot := projectionFixture(t)
			before, _ := s.project(snapshot, "thread")
			after, _ := s.project(snapshot, "thread")
			switch kind {
			case "remove":
				after["turnItems"] = rows(after["turnItems"])[1:]
			case "reorder":
				items := rows(after["turnItems"])
				items[0], items[1] = items[1], items[0]
			case "visibility":
				object(rows(after["visibleTurnItems"])[0])["visibility"] = "hidden"
			case "run-transition":
				object(rows(after["runs"])[40])["status"] = "completed"
			case "new-field":
				after["futureProtocolField"] = true
			}
			if _, ok := projectionDelta(before, after, 100, 1000, false); ok {
				t.Fatal("structural change must use snapshot")
			}
		})
	}
}

func TestShellDeltaArchiveRemovalAndStableProject(t *testing.T) {
	s, snapshot := projectionFixture(t)
	s.opts.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: "/workspace"}
	before := s.shellSnapshot(snapshot)
	unchanged := s.shellSnapshot(snapshot)
	if changes, ok := projectionDelta(before, unchanged, 100, 101, true); !ok || len(changes) != 0 {
		t.Fatal("unchanged shell generated traffic")
	}
	snapshot.Threads[0].Archived = true
	after := s.shellSnapshot(snapshot)
	if _, ok := projectionDelta(before, after, 100, 110, true); ok {
		t.Fatal("archive transition must use snapshot for newer T3 reducers")
	}
	if _, ok := projectionDelta(after, before, 110, 120, true); ok {
		t.Fatal("unarchive transition must use snapshot")
	}
	snapshot.Threads[0].Archived = false
	snapshot.Threads[0].Title = "renamed"
	renamed := s.shellSnapshot(snapshot)
	changes, ok := projectionDelta(before, renamed, 100, 101, true)
	if !ok || len(changes) != 1 || changes[0]["kind"] != "thread.updated" {
		t.Fatal(changes, ok)
	}
	snapshot.Threads = nil
	removed := s.shellSnapshot(snapshot)
	// Keep project metadata stable to isolate the thread removal.
	removed["projects"] = before["projects"]
	changes, ok = projectionDelta(before, removed, 100, 101, true)
	if !ok || len(changes) != 1 || changes[0]["kind"] != "thread.removed" {
		t.Fatal(changes, ok)
	}
}

func TestRPCCompressionNegotiatesWithoutChangingSnapshot(t *testing.T) {
	s, core, server := testServer(t)
	token := pairClient(t, s, server.URL)
	dialer := websocket.Dialer{EnableCompression: true}
	conn, resp, err := dialer.Dial(strings.Replace(server.URL, "http", "ws", 1)+"/ws", http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
		t.Fatal("compression not negotiated")
	}
	sendRPC(t, conn, "snapshot", "orchestration.subscribeThread", Object{"threadId": "seed"})
	frame := object(rows(responseRPC(t, conn, "snapshot")["values"])[0])
	snapshot, err := core.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, err := s.project(snapshot, "seed")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(object(frame["projection"]), normalized(t, want)) {
		t.Fatal("compressed snapshot changed")
	}
}

type projectionService struct {
	api.Service
	mu                 sync.Mutex
	snapshot           api.Snapshot
	events             chan api.Event
	calls              atomic.Int32
	subscribeAfter     chan int64
	subscriptionClosed chan struct{}
}

func (s *projectionService) Snapshot(context.Context) (api.Snapshot, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Match Core's snapshot ownership: callers must not observe later fixture
	// mutations through shared slices while they construct projections.
	data, err := json.Marshal(s.snapshot)
	if err != nil {
		return api.Snapshot{}, err
	}
	var snapshot api.Snapshot
	err = json.Unmarshal(data, &snapshot)
	return snapshot, err
}
func (s *projectionService) Subscribe(_ context.Context, after int64) (*api.Subscription, error) {
	if s.subscribeAfter != nil {
		s.subscribeAfter <- after
	}
	return &api.Subscription{Events: s.events, Close: func() {
		if s.subscriptionClosed != nil {
			s.subscriptionClosed <- struct{}{}
		}
	}}, nil
}

func TestRPCPublishesDeltaAndSkipsCoveredReplay(t *testing.T) {
	server, service, httpServer := testServer(t)
	_, initial := projectionFixture(t)
	stub := &projectionService{Service: service, snapshot: initial, events: make(chan api.Event, 32)}
	server.service = stub
	token := pairClient(t, server, httpServer.URL)
	conn := openRPC(t, httpServer.URL, token)
	sendRPC(t, conn, "live", "orchestration.subscribeThread", Object{"threadId": "thread"})
	first := object(rows(responseRPC(t, conn, "live")["values"])[0])
	if first["kind"] != "snapshot" {
		t.Fatal(first)
	}
	if object(rows(responseRPC(t, conn, "live")["values"])[0])["kind"] != "synchronized" {
		t.Fatal("missing synchronization")
	}
	stub.mu.Lock()
	stub.snapshot.Sequence = 110
	stub.snapshot.Runs[40].Output += " another token"
	stub.snapshot.Runs[40].UpdatedAt = stub.snapshot.Runs[40].UpdatedAt.Add(time.Second)
	stub.mu.Unlock()
	stub.events <- api.Event{Sequence: 101, ThreadID: "thread"}
	response := responseRPC(t, conn, "live")
	var changes []Object
	for _, value := range rows(response["values"]) {
		change := object(value)
		if change["kind"] != "event" {
			t.Fatal("expected row events", change["kind"])
		}
		changes = append(changes, change)
	}
	stub.mu.Lock()
	want, err := server.project(stub.snapshot, "thread")
	stub.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applyThreadChanges(t, object(first["projection"]), changes, 100), normalized(t, want)) {
		t.Fatal("live delta lost state")
	}
	// These queued events were already included in snapshot 110. They must not
	// trigger another full projection or retransmit old rows.
	for sequence := int64(102); sequence <= 110; sequence++ {
		stub.events <- api.Event{Sequence: sequence, ThreadID: "thread"}
	}
	time.Sleep(120 * time.Millisecond)
	if calls := stub.calls.Load(); calls != 2 {
		t.Fatalf("covered events caused %d snapshot reads, want 2", calls)
	}
}
