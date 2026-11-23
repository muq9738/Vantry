package pi

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/provider"
)

//go:embed approval.ts
var approvalExtension []byte

const (
	// User extensions initialize before Pi starts reading RPC commands. Allow
	// their startup work (for example FFF's 15-second initial scan) to finish.
	rpcStartupTimeout = 30 * time.Second
	rpcTimeout        = 10 * time.Second
	// Disabled by default (0) to match Pi and T3 Code: bash and other tools
	// manage their own per-command timeouts and can legitimately run silently
	// (e.g. block-buffered stdout or long downloads) for >5m. Users can still
	// opt in via VANTRY_PI_TOOL_IDLE_TIMEOUT.
	defaultToolIdleTimeout   = 0
	defaultStreamIdleTimeout = 3 * time.Minute
)

func parseWatchdogTimeout(envKey string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "0", "off", "false", "disabled", "none":
		return 0
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return 0
		}
		return d
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	return fallback
}

type rpcRecord struct {
	Type            string          `json:"type"`
	ID              string          `json:"id"`
	Command         string          `json:"command"`
	Success         bool            `json:"success"`
	Error           string          `json:"error"`
	Data            json.RawMessage `json:"data"`
	Method          string          `json:"method"`
	Timeout         int64           `json:"timeout"`
	Options         []string        `json:"options"`
	Message         json.RawMessage `json:"message"`
	ToolCallID      string          `json:"toolCallId"`
	ToolCallIDSnake string          `json:"tool_call_id"`
	ToolName        string          `json:"toolName"`
	ToolNameSnake   string          `json:"tool_name"`
	DelayMs         int64           `json:"delayMs"`
}

func (r rpcRecord) toolCallID() string {
	if r.ToolCallID != "" {
		return r.ToolCallID
	}
	return r.ToolCallIDSnake
}

func (r rpcRecord) toolName() string {
	if r.ToolName != "" {
		return r.ToolName
	}
	return r.ToolNameSnake
}

type activeRPCTool struct {
	name       string
	startedAt  time.Time
	lastUpdate time.Time
}

type rpcLine struct {
	data []byte
	err  error
}

// Read only LF boundaries: Unicode separators are valid JSON string contents.
func rpcLines(ctx context.Context, reader io.Reader) <-chan rpcLine {
	lines := make(chan rpcLine)
	go func() {
		defer close(lines)
		buffer := bufio.NewReader(reader)
		for {
			line, err := buffer.ReadBytes('\n')
			if len(line) > 0 {
				if line[len(line)-1] != '\n' {
					err = errors.New("unterminated pi JSONL record")
				} else {
					line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte{'\n'}), []byte{'\r'})
				}
			}
			select {
			case lines <- rpcLine{line, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return lines
}

func formatInterSessionMessage(request provider.Request) string {
	senderID := strings.TrimSpace(request.SenderThreadID)
	if senderID == "" {
		return request.Message
	}
	senderTitle := strings.TrimSpace(request.SenderTitle)
	if senderTitle == "" {
		senderTitle = senderID
	}
	senderTitle = strings.NewReplacer("\r", " ", "\n", " ").Replace(senderTitle)
	return fmt.Sprintf(
		"[Inter-session message from Vantry Pi thread \"%s\" (threadId: %s) — you may reply or collaborate using vantry_thread_send / vantry_thread_read if needed]\n\n%s",
		senderTitle,
		senderID,
		request.Message,
	)
}

func framedPromptCommand(job jobRequest) (map[string]any, error) {
	framed := job
	framed.Request.Message = formatInterSessionMessage(job.Request)
	return promptCommand(framed)
}

func writeRPC(writer io.Writer, command any) error {
	data, err := json.Marshal(command)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(data, '\n'))
	return err
}

type rpcDialog struct {
	record  rpcRecord
	expires time.Time
}
type pendingControl struct {
	control  provider.Control
	deadline time.Time
}

func runRPC(ctx context.Context, job jobRequest, jobDir string, stdin io.WriteCloser, stdout io.Reader, journal *eventJournal) (piStreamState, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	writes, writeResults, writerDone := startRPCWriter(ctx, stdin)
	defer func() { cancel(); _ = stdin.Close(); <-writerDone }()
	outstandingWrites := 0
	queueWrite := func(command any, complete func(error) error) error {
		select {
		case writes <- rpcWriteRequest{command: command, complete: complete}:
			outstandingWrites++
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	finishWrite := func(result rpcWriteResult) error {
		outstandingWrites--
		if result.request.complete != nil {
			return result.request.complete(result.err)
		}
		return result.err
	}
	finishInput := func() error {
		_ = stdin.Close()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		for outstandingWrites > 0 {
			select {
			case result := <-writeResults:
				if err := finishWrite(result); err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return errors.New("Pi RPC input did not finish after stdout closed")
			}
		}
		return nil
	}
	lines := rpcLines(ctx, stdout)
	state := piStreamState{}
	dialogs := map[string]rpcDialog{}
	processed := map[string]bool{}
	pending := map[string]pendingControl{}
	sentRPC := map[string]bool{}
	promptAccepted, closing, approvalReady := false, false, false
	phase := "get_state"
	deadline := time.Now().Add(rpcStartupTimeout)
	toolIdleTimeout := parseWatchdogTimeout(EnvToolIdleTimeout, defaultToolIdleTimeout)
	streamIdleTimeout := parseWatchdogTimeout(EnvStreamIdleTimeout, defaultStreamIdleTimeout)
	lastActivity := time.Now()
	retryGraceUntil := time.Time{}
	activeTools := map[string]activeRPCTool{}
	if err := queueWrite(map[string]any{"id": "vantry:state", "type": "get_state"}, nil); err != nil {
		return state, err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	finish := func(control provider.Control, status, message string) error {
		if processed[control.ID] {
			return nil
		}
		if err := appendControlResult(journal, control.ID, status, message); err != nil {
			return err
		}
		processed[control.ID] = true
		return nil
	}
	shutdown := func() error {
		if closing {
			return nil
		}
		if err := markClosing(jobDir); err != nil {
			return err
		}
		closing = true
		deadline = time.Now().Add(5 * time.Second)
		return stdin.Close()
	}
	handleLine := func(line rpcLine) error {
		if errors.Is(line.err, io.EOF) && !promptAccepted {
			return errors.New("Pi exited without prompt acceptance")
		}
		if line.err != nil {
			return line.err
		}
		if len(bytes.TrimSpace(line.data)) == 0 {
			return nil
		}
		var record rpcRecord
		if err := json.Unmarshal(line.data, &record); err != nil {
			return fmt.Errorf("decode pi RPC: %w", err)
		}
		now := time.Now()
		lastActivity = now
		if record.Type == "response" {
			switch record.ID {
			case "vantry:state":
				if phase != "get_state" || record.Command != "get_state" {
					return errors.New("unexpected Pi handshake response")
				}
				if !record.Success {
					return fmt.Errorf("Pi get_state rejected: %s", record.Error)
				}
				if job.ExpectedModel != "" {
					var execution struct {
						Model struct {
							Provider string `json:"provider"`
							ID       string `json:"id"`
						} `json:"model"`
					}
					if json.Unmarshal(record.Data, &execution) != nil || execution.Model.Provider == "" || execution.Model.ID == "" {
						return errors.New("Pi 未确认所选模型，未发送消息；请重新载入 Magpie 模型配置")
					}
					if execution.Model.Provider+"/"+execution.Model.ID != job.ExpectedModel {
						return errors.New("Pi 实际模型与 Magpie 选择不一致，未发送消息；请检查并应用 Magpie 的 Pi 配置")
					}
				}
				if requiresApprovalExtension(job) && !approvalReady {
					return errors.New("Pi approval extension did not initialize")
				}
				phase = "prompt"
				deadline = time.Now().Add(rpcTimeout)
				prompt, err := framedPromptCommand(job)
				if err != nil {
					return err
				}
				return queueWrite(prompt, nil)
			case "vantry:prompt":
				if phase != "prompt" || record.Command != "prompt" {
					return errors.New("unexpected Pi prompt response")
				}
				if !record.Success {
					return fmt.Errorf("Pi prompt rejected: %s", record.Error)
				}
				promptAccepted, phase = true, "running"
				var data struct {
					Disposition string `json:"disposition"`
				}
				if err := json.Unmarshal(record.Data, &data); err != nil {
					return fmt.Errorf("decode Pi prompt acceptance: %w", err)
				}
				if data.Disposition == "handled" {
					state.settled = true
				}
				if state.settled {
					return shutdown()
				}
			default:
				if item, ok := pending[record.ID]; ok {
					delete(pending, record.ID)
					if record.Command != "steer" {
						return finish(item.control, "failed", "Pi returned an unexpected control response")
					}
					if !record.Success {
						return finish(item.control, "failed", nonEmpty(record.Error, "Pi rejected steer"))
					}
					return finish(item.control, "delivered", "")
				}
				if !sentRPC[record.ID] && !record.Success {
					return fmt.Errorf("Pi RPC %s rejected: %s", record.Command, record.Error)
				}
			}
			return nil
		}
		if record.Type == "extension_ui_request" {
			var message string
			_ = json.Unmarshal(record.Message, &message)
			if record.Method == "notify" && message == "vantry_approval_ready" {
				approvalReady = true
			}
			switch record.Method {
			case "confirm", "select", "input", "editor":
				dialog := rpcDialog{record: record}
				if record.Timeout > 0 {
					dialog.expires = time.Now().Add(time.Duration(record.Timeout) * time.Millisecond)
				}
				dialogs[record.ID] = dialog
			}
		}
		switch record.Type {
		case "tool_execution_start":
			if id := record.toolCallID(); id != "" {
				activeTools[id] = activeRPCTool{
					name:       nonEmpty(record.toolName(), "tool"),
					startedAt:  now,
					lastUpdate: now,
				}
			}
		case "tool_execution_update":
			if id := record.toolCallID(); id != "" {
				if tool, exists := activeTools[id]; exists {
					tool.lastUpdate = now
					if name := record.toolName(); name != "" {
						tool.name = name
					}
					activeTools[id] = tool
				} else {
					activeTools[id] = activeRPCTool{
						name:       nonEmpty(record.toolName(), "tool"),
						startedAt:  now,
						lastUpdate: now,
					}
				}
			}
		case "tool_execution_end":
			if id := record.toolCallID(); id != "" {
				delete(activeTools, id)
			}
		case "auto_retry_start", "summarization_retry_scheduled":
			if record.DelayMs > 0 {
				grace := now.Add(time.Duration(record.DelayMs)*time.Millisecond + 30*time.Second)
				if grace.After(retryGraceUntil) {
					retryGraceUntil = grace
				}
			}
		case "auto_retry_end", "summarization_retry_finished":
			retryGraceUntil = time.Time{}
		}
		event, err := state.consume(line.data)
		if err != nil {
			return err
		}
		if err := journal.Append(event); err != nil {
			return err
		}
		if state.settled {
			// No control may be handed off once the final boundary is observed.
			if err := markClosing(jobDir); err != nil {
				return err
			}
			for id, item := range pending {
				if err := finish(item.control, controlStatusUnknown, "Pi settled before acknowledging control; delivery is unconfirmed and will not be resent"); err != nil {
					return err
				}
				delete(pending, id)
			}
			if promptAccepted {
				return shutdown()
			}
		}
		return nil
	}
	for {
		if ctx.Err() != nil {
			return state, ctx.Err()
		}
		// Consume already available output before inspecting controls. In
		// particular, a settled boundary must win over new control writes.
		select {
		case result := <-writeResults:
			if err := finishWrite(result); err != nil {
				return state, err
			}
			continue
		case line := <-lines:
			if err := handleLine(line); err != nil {
				if errors.Is(err, io.EOF) {
					return state, finishInput()
				}
				return state, err
			}
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case result := <-writeResults:
			if err := finishWrite(result); err != nil {
				return state, err
			}
		case line := <-lines:
			if err := handleLine(line); err != nil {
				if errors.Is(err, io.EOF) {
					return state, finishInput()
				}
				return state, err
			}
		case now := <-ticker.C:
			if (phase != "running" || closing) && now.After(deadline) {
				if closing {
					return state, errors.New("Pi RPC shutdown timed out after 5s")
				}
				if phase == "get_state" {
					return state, fmt.Errorf("Pi RPC startup handshake (get_state) timed out after %s; check user extension initialization", rpcStartupTimeout)
				}
				return state, fmt.Errorf("Pi RPC prompt acceptance timed out after %s", rpcTimeout)
			}
			if phase == "running" && !state.settled && !closing {
				hasActiveDialog := false
				for _, dialog := range dialogs {
					if dialog.expires.IsZero() || now.Before(dialog.expires) {
						hasActiveDialog = true
						break
					}
				}
				if hasActiveDialog || (!retryGraceUntil.IsZero() && now.Before(retryGraceUntil)) {
					lastActivity = now
					for id, tool := range activeTools {
						tool.lastUpdate = now
						activeTools[id] = tool
					}
				} else if len(activeTools) > 0 {
					if toolIdleTimeout > 0 && now.Sub(lastActivity) > toolIdleTimeout {
						stalledName := "tool"
						var oldest time.Time
						for _, tool := range activeTools {
							if oldest.IsZero() || tool.lastUpdate.Before(oldest) {
								oldest = tool.lastUpdate
								stalledName = tool.name
							}
						}
						return state, fmt.Errorf("Pi tool %q produced no output for %s (stall watchdog); aborted so partial output is preserved", stalledName, toolIdleTimeout)
					}
				} else if streamIdleTimeout > 0 && now.Sub(lastActivity) > streamIdleTimeout {
					return state, fmt.Errorf("Pi model stream produced no events for %s (stall watchdog); aborted so partial output is preserved", streamIdleTimeout)
				}
			}
			for id, item := range pending {
				if now.After(item.deadline) {
					// A timeout cannot tell whether Pi read the command, so the
					// outcome is unknown and the control is never resent.
					if err := finish(item.control, controlStatusUnknown, unconfirmedControl); err != nil {
						return state, err
					}
					delete(pending, id)
				}
			}
			if !promptAccepted || state.settled || closing || outstandingWrites > 0 {
				continue
			}
			controls, err := readControls(jobDir)
			if err != nil {
				return state, err
			}
			for _, control := range controls {
				if processed[control.ID] {
					continue
				}
				name, _ := controlName(control.ID)
				rpcID := "vantry:control:" + name
				if _, ok := pending[rpcID]; ok {
					continue
				}
				sentPath := filepath.Join(jobDir, "control-sent", name)
				if sent, err := fileExists(sentPath); err != nil {
					return state, err
				} else if sent {
					// A send intent without an acknowledgement is unconfirmed:
					// report unknown and never resend.
					if err := finish(control, controlStatusUnknown, unconfirmedControl); err != nil {
						return state, err
					}
					continue
				}
				// A steer keeps Core-owned attachment identities; an attachment
				// Pi cannot take is refused before anything reaches stdin.
				command, commandErr := controlCommand(job, rpcID, control)
				if commandErr != nil {
					if err := finish(control, "failed", commandErr.Error()); err != nil {
						return state, err
					}
					continue
				}
				if control.Kind == "response" {
					dialog, exists := dialogs[control.RequestID]
					if !exists || (!dialog.expires.IsZero() && !time.Now().Before(dialog.expires)) {
						if err := finish(control, "failed", "Pi dialog is unknown, expired, or already answered"); err != nil {
							return state, err
						}
						continue
					}
					if dialog.record.Method == "select" && !control.Cancelled {
						valid := false
						for _, option := range dialog.record.Options {
							if option == control.Value {
								valid = true
							}
						}
						if !valid {
							if err := finish(control, "failed", "response is not a Pi dialog option"); err != nil {
								return state, err
							}
							continue
						}
					}
					command = map[string]any{"id": control.RequestID, "type": "extension_ui_response"}
					if control.Cancelled {
						command["cancelled"] = true
					} else if dialog.record.Method == "confirm" {
						command["confirmed"] = control.Confirmed
					} else {
						command["value"] = control.Value
					}
				}
				if err := makePrivateDir(filepath.Dir(sentPath)); err != nil {
					return state, err
				}
				// Persist the uncertain-send window before touching Pi's stdin.
				if err := writeJSONFile(sentPath, control); err != nil {
					return state, err
				}
				if control.Kind != "response" {
					// Correlate before enqueue: Pi can acknowledge the complete
					// record before the writer's completion reaches this loop.
					sentRPC[rpcID] = true
					pending[rpcID] = pendingControl{control, time.Now().Add(rpcTimeout)}
				}
				complete := func(writeErr error) error {
					if writeErr != nil {
						if err := finish(control, controlStatusUnknown, fmt.Sprintf("Pi control write is unconfirmed and will not be resent: %v", writeErr)); err != nil {
							return err
						}
						if state.settled {
							return nil
						}
						return writeErr
					}
					if control.Kind == "response" {
						dialog, exists := dialogs[control.RequestID]
						delete(dialogs, control.RequestID)
						lastActivity = time.Now()
						if !exists || (!dialog.expires.IsZero() && !time.Now().Before(dialog.expires)) {
							return finish(control, controlStatusUnknown, "Pi dialog expired while writing; response delivery is unconfirmed")
						}
						// Extension UI has no command acknowledgement. Record
						// delivery only after the complete write to a live dialog.
						return finish(control, "delivered", "")
					}
					return nil
				}
				if err := queueWrite(command, complete); err != nil {
					return state, err
				}
				break // Keep one control write in flight while draining stdout.
			}
		}
	}
}
