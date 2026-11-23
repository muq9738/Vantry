package t3compat

import (
	"reflect"
	"strconv"
)

// T3 reducers require strictly increasing cursors for each subscription. One
// Core event can change several projected rows, so transport sequences may run
// ahead of Core. They must never be used to filter or subscribe to Core events.
// Reconnect starts a new sequence epoch with an authoritative snapshot; these
// coalesced projection events are not a separately replayable event log.
func projectionDelta(previous, current Object, after, sequence int64, shell bool) ([]Object, bool) {
	if len(previous) != len(current) {
		return nil, false
	}
	var changes []Object
	if shell {
		// New T3 shells ignore archive-located updates, and removals delete
		// from both locations. Preserve archive transitions with a snapshot.
		if !reflect.DeepEqual(previous["archivedThreads"], current["archivedThreads"]) {
			return nil, false
		}
		for key, value := range current {
			if key != "snapshotSequence" && key != "projects" && key != "threads" && !reflect.DeepEqual(previous[key], value) {
				return nil, false
			}
		}
		for _, group := range []struct{ key, kind, location string }{
			{"projects", "project", ""}, {"threads", "thread", "active"},
		} {
			old := make(map[string]Object)
			for _, value := range rows(previous[group.key]) {
				row := object(value)
				old[text(row, "id")] = row
			}
			present := make(map[string]bool)
			for _, value := range rows(current[group.key]) {
				row := object(value)
				id := text(row, "id")
				present[id] = true
				if !reflect.DeepEqual(old[id], row) {
					change := Object{"kind": group.kind + ".updated", group.kind: row}
					if group.location != "" {
						change["location"] = group.location
					}
					changes = append(changes, change)
				}
			}
			// Upserts retain existing row positions and append new IDs. If
			// authoritative ordering differs, only a snapshot can express it.
			var order, want []string
			for _, value := range rows(previous[group.key]) {
				id := text(object(value), "id")
				if present[id] {
					order = append(order, id)
				}
			}
			for _, value := range rows(current[group.key]) {
				id := text(object(value), "id")
				want = append(want, id)
				if old[id] == nil {
					order = append(order, id)
				}
			}
			if !reflect.DeepEqual(order, want) {
				return nil, false
			}
			for _, value := range rows(previous[group.key]) {
				id := text(object(value), "id")
				if !present[id] {
					change := Object{"kind": group.kind + ".removed", group.kind + "Id": id}
					if group.location != "" {
						change["location"] = group.location
					}
					changes = append(changes, change)
				}
			}
		}
	} else {
		// Structural changes need the snapshot's authoritative visibility/order.
		// The frequent streaming path only replaces existing rows.
		types := map[string]string{"nodes": "node.updated", "messages": "message.updated", "turnItems": "turn-item.updated", "runtimeRequests": "runtime-request.updated"}
		for key, value := range current {
			if key == "updatedAt" || key == "visibleTurnItems" || key == "thread" || types[key] != "" {
				continue
			}
			if !reflect.DeepEqual(previous[key], value) {
				return nil, false
			}
		}
		appendEvent := func(kind string, row Object) {
			event := Object{"type": kind, "threadId": text(object(current["thread"]), "id"), "occurredAt": current["updatedAt"], "payload": row}
			changes = append(changes, Object{"kind": "event", "event": event})
		}
		if !reflect.DeepEqual(previous["thread"], current["thread"]) {
			appendEvent("thread.metadata-updated", object(current["thread"]))
		}
		for _, key := range []string{"nodes", "runtimeRequests", "messages", "turnItems"} {
			before, next := rows(previous[key]), rows(current[key])
			if len(before) != len(next) {
				return nil, false
			}
			for i, value := range next {
				row, old := object(value), object(before[i])
				if text(row, "id") != text(old, "id") {
					return nil, false
				}
				if !reflect.DeepEqual(old, row) {
					appendEvent(types[key], row)
				}
			}
		}
		// Vantry exposes all local items with a stable position. Only their body
		// may differ; a visibility or position change requires a fresh snapshot.
		before, next := rows(previous["visibleTurnItems"]), rows(current["visibleTurnItems"])
		if len(before) != len(next) {
			return nil, false
		}
		for i, value := range next {
			row, old := object(value), object(before[i])
			if len(row) != len(old) {
				return nil, false
			}
			for key, value := range row {
				if key != "item" && !reflect.DeepEqual(value, old[key]) {
					return nil, false
				}
			}
		}
		if len(changes) == 0 && !reflect.DeepEqual(previous["updatedAt"], current["updatedAt"]) {
			return nil, false
		}
	}
	sequence = max(sequence, after+int64(len(changes)))
	for i, change := range changes {
		cursor := sequence - int64(len(changes)) + int64(i) + 1
		change["sequence"] = cursor
		if event, ok := change["event"].(Object); ok {
			event["id"] = text(object(current["thread"]), "id") + "/projection/" + strconv.FormatInt(cursor, 10)
		}
	}
	return changes, true
}
