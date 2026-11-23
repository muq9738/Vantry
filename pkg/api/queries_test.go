package api

import (
	"encoding/json"
	"testing"
)

func TestToolOffsetDistinguishesHistoricalRecordsAndStartOfReply(t *testing.T) {
	var old ToolCall
	if err := json.Unmarshal([]byte(`{"id":"old","name":"read"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.OutputOffset != -1 {
		t.Fatalf("historical offset=%d", old.OutputOffset)
	}
	encoded, err := json.Marshal(ToolCall{ID: "new", OutputOffset: 0})
	if err != nil {
		t.Fatal(err)
	}
	var next ToolCall
	if err := json.Unmarshal(encoded, &next); err != nil {
		t.Fatal(err)
	}
	if next.OutputOffset != 0 {
		t.Fatalf("real zero offset lost: %s", encoded)
	}
}
