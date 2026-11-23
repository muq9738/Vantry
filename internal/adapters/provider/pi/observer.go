package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/muq9738/Vantry/pkg/provider"
)

const workerLaunchGrace = 2 * time.Second

func (p *Provider) observe(ctx context.Context, jobDir, attemptID string, after int64, emit func(provider.Event) error) error {
	next := after + 1
	for {
		var err error
		next, err = emitCommitted(jobDir, next, emit)
		if err != nil {
			return err
		}
		var result jobResult
		found, err := readJSONFile(filepath.Join(jobDir, "result.json"), &result)
		if err != nil {
			return err
		}
		if found {
			if result.AttemptID != attemptID {
				return errors.New("provider result belongs to a different attempt")
			}
			// Result publication follows the final journal fsync. The first read
			// above may have raced that publication, so consume the now-complete
			// tail before classifying a durable sequence gap.
			next, err = emitCommitted(jobDir, next, emit)
			if err != nil {
				return err
			}
			if result.LastSequence < after {
				return fmt.Errorf("provider cursor %d is beyond durable result sequence %d", after, result.LastSequence)
			}
			if result.LastSequence >= next {
				return fmt.Errorf("provider event gap: cursor %d, result sequence %d", next-1, result.LastSequence)
			}
			if result.LastSequence != next-1 {
				return fmt.Errorf("provider cursor mismatch: next %d, result sequence %d", next, result.LastSequence)
			}
			return resultError(result)
		}
		if err := failExitedWorker(jobDir, attemptID); err != nil && !errors.Is(err, errOrphanPending) {
			return err
		}
		timer := time.NewTimer(40 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return provider.ErrDetached
		case <-timer.C:
		}
	}
}

func emitCommitted(jobDir string, next int64, emit func(provider.Event) error) (int64, error) {
	data, err := os.ReadFile(filepath.Join(jobDir, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return next, nil
	}
	if err != nil {
		return next, err
	}
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			// A worker appends and fsyncs one complete JSON record at a time.
			// An observer can nevertheless catch an in-progress write; retain
			// this unterminated suffix for the next poll.
			break
		}
		line := bytes.TrimSuffix(data[:newline], []byte{'\r'})
		data = data[newline+1:]
		if len(line) == 0 {
			return next, errors.New("empty record in provider event journal")
		}
		var event provider.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return next, fmt.Errorf("decode provider event journal: %w", err)
		}
		if event.Sequence <= 0 {
			return next, fmt.Errorf("invalid provider event sequence %d", event.Sequence)
		}
		if event.Sequence < next {
			// Re-reading the append-only file naturally encounters committed
			// prefixes. They are replay duplicates, not new Core events.
			continue
		}
		if event.Sequence != next {
			return next, fmt.Errorf("provider event gap: got %d, want %d", event.Sequence, next)
		}
		if err := emit(event); err != nil {
			return next, err
		}
		next++
	}
	return next, nil
}

func failExitedWorker(jobDir, attemptID string) error {
	return finalizeExitedWorker(jobDir, attemptID, false)
}

func finalizeExitedWorker(jobDir, attemptID string, stopped bool) error {
	var started startedMarker
	found, err := readJSONFile(filepath.Join(jobDir, "started.json"), &started)
	if err != nil || !found {
		return err
	}
	if !stopped && time.Since(started.UpdatedAt) < workerLaunchGrace {
		return nil
	}
	lock, acquired, err := tryLock(filepath.Join(jobDir, "worker.lock"))
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer unlock(lock)
	// The worker holds this lock through its final result rename. Lock
	// acquisition therefore proves that it exited without publishing a result.
	resultExists, err := fileExists(filepath.Join(jobDir, "result.json"))
	if err != nil || resultExists {
		return err
	}
	if err := cleanupOrphanedPi(jobDir, attemptID); err != nil {
		return err
	}
	last, err := finalizeControls(jobDir, "provider worker exited before control acknowledgement; control will not be resent")
	if err != nil {
		return err
	}
	result := jobResult{
		AttemptID: attemptID, Status: "failed", LastSequence: last,
		Error:     "provider worker exited without a result; the submitted prompt will not be resent",
		UpdatedAt: time.Now().UTC(),
	}
	if cancelled, _ := fileExists(filepath.Join(jobDir, "stop.json")); cancelled {
		result.Status, result.Error = "cancelled", ""
	}
	return writeJSONFile(filepath.Join(jobDir, "result.json"), result)
}

func journalLastSequence(jobDir string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(jobDir, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var last int64
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			return 0, errors.New("unterminated provider event after worker exit")
		}
		line := bytes.TrimSuffix(data[:newline], []byte{'\r'})
		data = data[newline+1:]
		var event provider.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return 0, err
		}
		if event.Sequence <= last {
			return 0, errors.New("provider event sequence is not strictly increasing")
		}
		last = event.Sequence
	}
	return last, nil
}
