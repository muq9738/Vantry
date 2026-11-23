package pi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/muq9738/Vantry/pkg/provider"
)

// Control only hands off an immutable intent. The worker owns all Pi writes and
// emits the acknowledgement in the attempt's existing sequenced journal.
func (p *Provider) Control(ctx context.Context, request provider.Request, control provider.Control) error {
	if !p.SupportsControl(request) {
		return errors.New("provider does not support Pi controls")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(request.AttemptID); err != nil {
		return err
	}
	name, err := controlName(control.ID)
	if err != nil {
		return err
	}
	switch control.Kind {
	case "steer":
		if p.route(request) != ProviderPI {
			return errors.New("steer requires the Pi provider")
		}
		if strings.TrimSpace(control.Message) == "" {
			return errors.New("steer message is empty")
		}
	case "response":
		if control.RequestID == "" {
			return errors.New("Pi dialog ID is required")
		}
	default:
		return fmt.Errorf("unsupported Pi control %q", control.Kind)
	}
	dataDir, err := p.dataDir()
	if err != nil {
		return err
	}
	jobDir := filepath.Join(dataDir, "jobs", request.AttemptID)
	job, found, err := readJobRequest(jobDir)
	if err != nil {
		return err
	}
	if !found || job.Request.AttemptID != request.AttemptID || job.Request.RunID != request.RunID || job.Request.ThreadID != request.ThreadID {
		return errors.New("control does not match a persisted attempt")
	}
	lock, err := lockFileContext(ctx, filepath.Join(jobDir, "control.lock"))
	if err != nil {
		return err
	}
	defer unlock(lock)
	path := filepath.Join(jobDir, "controls", name)
	var existing provider.Control
	if found, err := readJSONFile(path, &existing); err != nil {
		return err
	} else if found {
		if !reflect.DeepEqual(existing, control) {
			return errors.New("control ID conflicts with its immutable intent")
		}
		return nil
	}
	for _, marker := range []string{"closing.json", "result.json", "stop.json"} {
		if found, err := fileExists(filepath.Join(jobDir, marker)); err != nil {
			return err
		} else if found {
			return errors.New("Pi attempt has settled or is stopping")
		}
	}
	if err := makePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	return writeJSONFile(path, control)
}

// API request IDs contain ':' and must not be passed through validID or used
// directly as paths. This encoding preserves identity without content hashes.
func controlName(id string) (string, error) {
	if id == "" || len(id) > 180 {
		return "", errors.New("control ID must contain 1 to 180 bytes")
	}
	return base64.RawURLEncoding.EncodeToString([]byte(id)) + ".json", nil
}

func readControls(jobDir string) ([]provider.Control, error) {
	entries, err := os.ReadDir(filepath.Join(jobDir, "controls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var controls []provider.Control
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var control provider.Control
		if _, err := readJSONFile(filepath.Join(jobDir, "controls", entry.Name()), &control); err != nil {
			return nil, err
		}
		name, err := controlName(control.ID)
		if err != nil || name != entry.Name() {
			return nil, errors.New("invalid persisted control identity")
		}
		controls = append(controls, control)
	}
	return controls, nil
}

// Control outcomes. A whole receive/send boundary is never replayable: when the
// adapter cannot tell whether Pi read a command, the outcome is unknown and the
// command is never resent. A command that provably never reached Pi's stdin is
// failed instead, so a client can distinguish "not sent" from "unconfirmed".
const (
	controlStatusUnknown = "unknown"
	unconfirmedControl   = "Pi control has no confirmed outcome; it will not be resent"
)

func appendControlResult(journal *eventJournal, id, status, message string) error {
	data, err := json.Marshal(provider.ControlResult{ID: id, Status: status, Error: message})
	if err != nil {
		return err
	}
	return journal.Append(provider.Event{Type: "control_result", Data: data})
}

func markClosing(jobDir string) error {
	lock, err := lockFileContext(context.Background(), filepath.Join(jobDir, "control.lock"))
	if err != nil {
		return err
	}
	defer unlock(lock)
	return writeJSONFile(filepath.Join(jobDir, "closing.json"), struct{}{})
}

// Called with worker.lock held, including by orphan recovery. Seal handoff and
// fail every intent without a durable result. A sent marker is never replayed.
func finalizeControls(jobDir, reason string) (int64, error) {
	lock, err := lockFileContext(context.Background(), filepath.Join(jobDir, "control.lock"))
	if err != nil {
		return 0, err
	}
	defer unlock(lock)
	if err := writeJSONFile(filepath.Join(jobDir, "closing.json"), struct{}{}); err != nil {
		return 0, err
	}
	last, err := journalLastSequence(jobDir)
	if err != nil {
		return 0, err
	}
	finished := map[string]bool{}
	if _, err := emitCommitted(jobDir, 1, func(event provider.Event) error {
		if event.Type == "control_result" {
			var result provider.ControlResult
			if err := json.Unmarshal(event.Data, &result); err != nil {
				return err
			}
			finished[result.ID] = true
		}
		return nil
	}); err != nil {
		return last, err
	}
	controls, err := readControls(jobDir)
	if err != nil {
		return last, err
	}
	file, err := os.OpenFile(filepath.Join(jobDir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return last, err
	}
	defer file.Close()
	journal := &eventJournal{file: file, sequence: last}
	for _, control := range controls {
		if finished[control.ID] {
			continue
		}
		name, err := controlName(control.ID)
		if err != nil {
			return journal.sequence, err
		}
		sent, err := fileExists(filepath.Join(jobDir, "control-sent", name))
		if err != nil {
			return journal.sequence, err
		}
		status, message := "failed", reason
		if sent {
			status, message = controlStatusUnknown, unconfirmedControl
		}
		if err := appendControlResult(journal, control.ID, status, message); err != nil {
			return journal.sequence, err
		}
	}
	return journal.sequence, nil
}

var _ provider.Controller = (*Provider)(nil)
