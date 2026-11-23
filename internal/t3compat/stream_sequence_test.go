package t3compat

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestSubscriptionSeparatesCoreCoverageFromWireSequence(t *testing.T) {
	server, service, httpServer := testServer(t)
	_, initial := projectionFixture(t)
	stub := &projectionService{Service: service, snapshot: initial, events: make(chan api.Event, 32), subscribeAfter: make(chan int64, 4), subscriptionClosed: make(chan struct{}, 4)}
	server.service = stub
	token := pairClient(t, server, httpServer.URL)
	conn := openRPC(t, httpServer.URL, token)
	sendRPC(t, conn, "live", "orchestration.subscribeThread", Object{"threadId": "thread"})
	first := object(rows(responseRPC(t, conn, "live")["values"])[0])
	state := object(first["projection"])
	cursor := number(first["snapshotSequence"])
	if after := <-stub.subscribeAfter; after != 100 {
		t.Fatalf("Core subscription cursor=%d", after)
	}
	responseRPC(t, conn, "live") // synchronized

	// Every output step has one committed event but two changed rows. Repeated
	// updates make the wire cursor run ahead of Core, then force a snapshot.
	for step := range 6 {
		stub.mu.Lock()
		stub.snapshot.Sequence++
		coreCursor := stub.snapshot.Sequence
		if step == 3 {
			stub.snapshot.Threads[0].Archived = true
		} else {
			stub.snapshot.Runs[40].Output += fmt.Sprintf(" token %d", step)
			stub.snapshot.Runs[40].UpdatedAt = stub.snapshot.Runs[40].UpdatedAt.Add(time.Second)
		}
		// A run transition forces the full thread projection fallback.
		if step == 3 {
			stub.snapshot.Runs[40].Status = api.RunCompleted
		}
		want, err := server.project(stub.snapshot, "thread")
		stub.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		stub.events <- api.Event{Sequence: coreCursor, ThreadID: "thread"}
		values := rows(responseRPC(t, conn, "live")["values"])
		if len(values) == 0 {
			t.Fatal("empty update")
		}
		frame := object(values[0])
		if step == 3 || step > 3 {
			// Editing a completed fixture also changes completion metadata; either
			// representation is valid, but the explicit transition must be a snapshot.
			if step == 3 && frame["kind"] != "snapshot" {
				t.Fatal("missing structural snapshot")
			}
		} else if frame["kind"] != "event" {
			t.Fatalf("single-event update fell back at step %d", step)
		}
		if frame["kind"] == "snapshot" {
			next := number(frame["snapshotSequence"])
			if next <= cursor {
				t.Fatalf("snapshot cursor regressed: %d <= %d", next, cursor)
			}
			cursor = next
			state = object(frame["projection"])
		} else {
			changes := make([]Object, len(values))
			for i, value := range values {
				changes[i] = object(value)
			}
			state = applyThreadChanges(t, state, changes, cursor)
			cursor = number(changes[len(changes)-1]["sequence"])
		}
		if !reflect.DeepEqual(state, normalized(t, want)) {
			t.Fatalf("state lost at step %d", step)
		}
	}
	if cursor <= stub.snapshot.Sequence {
		t.Fatal("test did not exercise a wire cursor ahead of Core")
	}
	oldCursor := cursor
	_ = conn.Close()
	select {
	case <-stub.subscriptionClosed:
	case <-time.After(time.Second):
		t.Fatal("old subscription not released")
	}
	reconnected := openRPC(t, httpServer.URL, token)
	sendRPC(t, reconnected, "resume", "orchestration.subscribeThread", Object{"threadId": "thread", "afterSequence": oldCursor})
	frame := object(rows(responseRPC(t, reconnected, "resume")["values"])[0])
	if frame["kind"] != "snapshot" {
		t.Fatal("reconnect must establish a new snapshot epoch")
	}
	if after := <-stub.subscribeAfter; after != stub.snapshot.Sequence {
		t.Fatal("wire resume cursor was passed to Core", after)
	}
	if !reflect.DeepEqual(object(frame["projection"]), state) {
		t.Fatal("reconnect lost history")
	}
	cursor = number(frame["snapshotSequence"])
	if cursor != stub.snapshot.Sequence {
		t.Fatal("snapshot epoch did not reset to Core")
	}
	responseRPC(t, reconnected, "resume")
	stub.mu.Lock()
	stub.snapshot.Sequence++
	stub.snapshot.Threads[0].Title = "renamed after reconnect"
	want, _ := server.project(stub.snapshot, "thread")
	coreCursor := stub.snapshot.Sequence
	stub.mu.Unlock()
	stub.events <- api.Event{Sequence: coreCursor, ThreadID: "thread"}
	values := rows(responseRPC(t, reconnected, "resume")["values"])
	changes := make([]Object, len(values))
	for i, value := range values {
		changes[i] = object(value)
	}
	if changes[0]["kind"] != "event" {
		t.Fatal("post-reconnect update not delivered as delta")
	}
	if !reflect.DeepEqual(applyThreadChanges(t, state, changes, cursor), normalized(t, want)) {
		t.Fatal("post-reconnect state lost")
	}
}

func TestShellSnapshotCursorDoesNotRegressAfterDelta(t *testing.T) {
	server, service, httpServer := testServer(t)
	_, initial := projectionFixture(t)
	for i := range 12 {
		initial.Threads = append(initial.Threads, api.Thread{ID: fmt.Sprintf("extra-%d", i), Title: "unchanged", CreatedAt: initial.Threads[0].CreatedAt})
	}
	stub := &projectionService{Service: service, snapshot: initial, events: make(chan api.Event, 8)}
	server.service = stub
	token := pairClient(t, server, httpServer.URL)
	conn := openRPC(t, httpServer.URL, token)
	sendRPC(t, conn, "shell", "orchestration.subscribeShell", Object{})
	responseRPC(t, conn, "shell")
	responseRPC(t, conn, "shell")
	stub.mu.Lock()
	stub.snapshot.Sequence = 101
	stub.snapshot.Threads[1].Title = "first change"
	stub.snapshot.Threads[2].Title = "second change"
	stub.mu.Unlock()
	stub.events <- api.Event{Sequence: 101, ThreadID: "extra-0"}
	values := rows(responseRPC(t, conn, "shell")["values"])
	if len(values) != 2 {
		t.Fatal("expected two shell updates", len(values))
	}
	cursor := int64(100)
	for _, value := range values {
		frame := object(value)
		if frame["kind"] != "thread.updated" || number(frame["sequence"]) <= cursor {
			t.Fatal(frame)
		}
		cursor = number(frame["sequence"])
	}
	stub.mu.Lock()
	stub.snapshot.Sequence = 102
	stub.snapshot.Threads[1].Archived = true
	stub.mu.Unlock()
	stub.events <- api.Event{Sequence: 102, ThreadID: "extra-0"}
	frame := object(rows(responseRPC(t, conn, "shell")["values"])[0])
	if frame["kind"] != "snapshot" || number(object(frame["snapshot"])["snapshotSequence"]) <= cursor {
		t.Fatal("shell snapshot cursor regressed", frame)
	}
	cursor = number(object(frame["snapshot"])["snapshotSequence"])
	stub.mu.Lock()
	stub.snapshot.Sequence = 103
	stub.snapshot.Threads[2].Title = "third change"
	stub.mu.Unlock()
	stub.events <- api.Event{Sequence: 103, ThreadID: "extra-1"}
	frame = object(rows(responseRPC(t, conn, "shell")["values"])[0])
	if frame["kind"] != "thread.updated" || number(frame["sequence"]) <= cursor {
		t.Fatal("Core change skipped after shell fallback", frame)
	}
}

func TestRealCoreStreamingUsesDeltasAndReconstructsFinalProjection(t *testing.T) {
	server, service, httpServer := testServer(t)
	token := pairClient(t, server, httpServer.URL)
	conn := openRPC(t, httpServer.URL, token)
	sendRPC(t, conn, "stream", "orchestration.subscribeThread", Object{"threadId": "seed"})
	first := object(rows(responseRPC(t, conn, "stream")["values"])[0])
	state := object(first["projection"])
	cursor := number(first["snapshotSequence"])
	responseRPC(t, conn, "stream")
	_, err := service.Dispatch(context.Background(), api.Command{Version: 1, CommandID: "cadence", Kind: api.SendMessage, ThreadID: "seed", Message: strings.Repeat("stream cadence review 中文 ", 80)})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, events, bytes := 0, 0, 0
	for {
		response := responseRPC(t, conn, "stream")
		for _, value := range rows(response["values"]) {
			frame := object(value)
			data, _ := json.Marshal(frame)
			bytes += len(data)
			if frame["kind"] == "snapshot" {
				snapshots++
				next := number(frame["snapshotSequence"])
				if next <= cursor {
					t.Fatal("snapshot cursor regression")
				}
				cursor = next
				state = object(frame["projection"])
			} else if frame["kind"] == "event" {
				events++
				state = applyThreadChanges(t, state, []Object{frame}, cursor)
				cursor = number(frame["sequence"])
			} else {
				t.Fatal("unexpected stream item", frame["kind"])
			}
		}
		runs := rows(state["runs"])
		if len(runs) > 0 && text(object(runs[0]), "status") == "completed" {
			break
		}
	}
	if events < 10 || snapshots > 4 {
		t.Fatalf("streaming still falls back: %d snapshots, %d events", snapshots, events)
	}
	snapshot, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, err := server.project(snapshot, "seed")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, normalized(t, want)) {
		t.Fatal("final projection lost data")
	}
	t.Logf("real Core: snapshots=%d events=%d decodedPayloadBytes=%d", snapshots, events, bytes)
}
