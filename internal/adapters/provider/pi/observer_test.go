package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/pkg/provider"
)

func TestResultPublicationBetweenJournalReadsConsumesFinalTail(t *testing.T) {
	directory := t.TempDir()
	journalPath := filepath.Join(directory, "events.jsonl")
	first, _ := json.Marshal(provider.Event{Sequence: 1, Delta: "first"})
	if err := os.WriteFile(journalPath, append(first, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	var output string
	err := New(Options{}).observe(context.Background(), directory, "attempt", 0, func(event provider.Event) error {
		output += event.Delta
		if event.Sequence == 1 {
			// This append happens after the observer captured its first journal
			// snapshot but before it checks the terminal result file.
			tail, _ := json.Marshal(provider.Event{Sequence: 2, Delta: "last"})
			file, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = file.Write(append(tail, '\n'))
			if err == nil {
				err = file.Sync()
			}
			_ = file.Close()
			if err != nil {
				return err
			}
			return writeJSONFile(filepath.Join(directory, "result.json"), jobResult{AttemptID: "attempt", Status: "completed", LastSequence: 2})
		}
		return nil
	})
	if err != nil || output != "firstlast" {
		t.Fatalf("completed tail was lost or misclassified: output=%q err=%v", output, err)
	}
}
