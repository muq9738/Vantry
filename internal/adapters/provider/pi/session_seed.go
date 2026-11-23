package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type parsedSessionEntry struct {
	raw         []byte
	entryType   string
	id          string
	parentID    *string
	targetID    string
	messageRole string
}

// CloneSessionFile copies (and optionally truncates) the active conversation
// branch of sourceThreadID's Pi JSONL session file into targetThreadID's
// session file so a forked thread can resume with authentic Pi history.
//
// When maxAssistantTurns <= 0, the entire active branch is cloned.
// When maxAssistantTurns > 0, at most maxAssistantTurns assistant turns (each
// including its trailing toolResult/metadata entries) are retained.
func CloneSessionFile(dataDir, sourceThreadID, targetThreadID string, maxAssistantTurns int) error {
	if strings.TrimSpace(dataDir) == "" {
		return errors.New("pi session data directory is required")
	}
	if err := validID(sourceThreadID); err != nil {
		return fmt.Errorf("invalid source thread ID: %w", err)
	}
	if err := validID(targetThreadID); err != nil {
		return fmt.Errorf("invalid target thread ID: %w", err)
	}
	if sourceThreadID == targetThreadID {
		return errors.New("source and target thread IDs must differ")
	}

	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	sessionsDir := filepath.Join(absDataDir, "sessions")
	if err := makePrivateDir(sessionsDir); err != nil {
		return err
	}

	sourcePath := filepath.Join(sessionsDir, sourceThreadID+".jsonl")
	headerMap, entries, empty, err := parseSessionFile(sourcePath)
	if err != nil || empty {
		return err
	}

	activePath := resolveActiveSessionBranch(entries)
	kept := truncateSessionEntries(activePath, maxAssistantTurns)

	if headerMap == nil {
		headerMap = map[string]any{
			"version": 3,
		}
	}
	headerMap["type"] = "session"
	if _, hasVersion := headerMap["version"]; !hasVersion {
		headerMap["version"] = 3
	}
	headerMap["id"] = buildClonedSessionID(targetThreadID)
	headerMap["timestamp"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	headerMap["parentSession"] = sourcePath

	headerBytes, err := json.Marshal(headerMap)
	if err != nil {
		return fmt.Errorf("encode cloned session header: %w", err)
	}

	targetPath := filepath.Join(sessionsDir, targetThreadID+".jsonl")
	return writeSessionJSONL(targetPath, headerBytes, kept)
}

// TruncateSessionFile rewrites threadID's Pi JSONL session file in place to
// retain at most keepAssistantTurns completed assistant turns along the active
// branch. When keepAssistantTurns <= 0, the session file is removed so the next
// turn starts fresh.
func TruncateSessionFile(dataDir, threadID string, keepAssistantTurns int) error {
	if strings.TrimSpace(dataDir) == "" {
		return errors.New("pi session data directory is required")
	}
	if err := validID(threadID); err != nil {
		return fmt.Errorf("invalid thread ID: %w", err)
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	sessionsDir := filepath.Join(absDataDir, "sessions")
	sessionPath := filepath.Join(sessionsDir, threadID+".jsonl")
	if keepAssistantTurns <= 0 {
		if err := os.Remove(sessionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}

	headerMap, entries, empty, err := parseSessionFile(sessionPath)
	if err != nil || empty {
		return err
	}
	activePath := resolveActiveSessionBranch(entries)
	kept := truncateSessionEntries(activePath, keepAssistantTurns)

	if headerMap == nil {
		headerMap = map[string]any{
			"version": 3,
		}
	}
	headerMap["type"] = "session"
	if _, hasVersion := headerMap["version"]; !hasVersion {
		headerMap["version"] = 3
	}
	headerBytes, err := json.Marshal(headerMap)
	if err != nil {
		return fmt.Errorf("encode truncated session header: %w", err)
	}
	return writeSessionJSONL(sessionPath, headerBytes, kept)
}

func parseSessionFile(sourcePath string) (map[string]any, []parsedSessionEntry, bool, error) {
	raw, err := os.ReadFile(sourcePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, true, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, true, nil
	}

	var headerMap map[string]any
	var entries []parsedSessionEntry

	for lineNumber, line := range bytes.Split(raw, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(bytes.TrimSuffix(line, []byte{'\r'}))
		if len(trimmed) == 0 {
			continue
		}
		var probe struct {
			Type     string  `json:"type"`
			ID       string  `json:"id"`
			ParentID *string `json:"parentId"`
			TargetID string  `json:"targetId"`
			Message  struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if err := json.Unmarshal(trimmed, &probe); err != nil {
			return nil, nil, false, fmt.Errorf("decode session %s line %d: %w", sourcePath, lineNumber+1, err)
		}
		if probe.Type == "" {
			return nil, nil, false, fmt.Errorf("session %s line %d has no entry type", sourcePath, lineNumber+1)
		}
		if probe.Type == "session" && headerMap == nil {
			if err := json.Unmarshal(trimmed, &headerMap); err != nil {
				return nil, nil, false, fmt.Errorf("decode session header %s: %w", sourcePath, err)
			}
			continue
		}
		rawCopy := append([]byte(nil), trimmed...)
		entries = append(entries, parsedSessionEntry{
			raw:         rawCopy,
			entryType:   probe.Type,
			id:          probe.ID,
			parentID:    probe.ParentID,
			targetID:    probe.TargetID,
			messageRole: probe.Message.Role,
		})
	}
	return headerMap, entries, false, nil
}

func resolveActiveSessionBranch(entries []parsedSessionEntry) []parsedSessionEntry {
	if len(entries) == 0 {
		return nil
	}
	allHaveID := true
	byID := make(map[string]parsedSessionEntry, len(entries))
	for _, entry := range entries {
		if entry.id == "" {
			allHaveID = false
			break
		}
		byID[entry.id] = entry
	}
	if !allHaveID {
		return entries
	}

	leaf := entries[len(entries)-1]
	reversed := make([]parsedSessionEntry, 0, len(entries))
	visited := make(map[string]bool, len(entries))
	current := leaf
	for {
		if visited[current.id] {
			break
		}
		visited[current.id] = true
		reversed = append(reversed, current)
		if current.parentID == nil || *current.parentID == "" || *current.parentID == current.id {
			break
		}
		parent, ok := byID[*current.parentID]
		if !ok {
			break
		}
		current = parent
	}

	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

func truncateSessionEntries(entries []parsedSessionEntry, maxAssistantTurns int) []parsedSessionEntry {
	if maxAssistantTurns <= 0 || len(entries) == 0 {
		return entries
	}
	kept := make([]parsedSessionEntry, 0, len(entries))
	keptIDs := make(map[string]bool, len(entries))
	assistantTurns := 0

	for _, entry := range entries {
		if assistantTurns < maxAssistantTurns {
			kept = append(kept, entry)
			if entry.id != "" {
				keptIDs[entry.id] = true
			}
			if entry.entryType == "message" && entry.messageRole == "assistant" {
				assistantTurns++
			}
			continue
		}

		// After reaching maxAssistantTurns, keep trailing entries that belong
		// to the final retained assistant turn (notably toolResult records so
		// toolCall blocks never remain dangling) and stop before the next turn.
		if isTrailingTurnAttachment(entry, keptIDs) {
			kept = append(kept, entry)
			if entry.id != "" {
				keptIDs[entry.id] = true
			}
			continue
		}
		break
	}
	return kept
}

func isTrailingTurnAttachment(entry parsedSessionEntry, keptIDs map[string]bool) bool {
	switch entry.entryType {
	case "message":
		return entry.messageRole == "toolResult"
	case "usage", "custom", "session_info":
		return true
	case "label", "context_edit":
		return entry.targetID == "" || keptIDs[entry.targetID]
	default:
		return false
	}
}

func buildClonedSessionID(targetThreadID string) string {
	stamp := strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
	return "vantry-" + targetThreadID + "-" + stamp
}

func writeSessionJSONL(targetPath string, headerLine []byte, entries []parsedSessionEntry) error {
	dir := filepath.Dir(targetPath)
	file, err := os.CreateTemp(dir, ".tmp-session-")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)

	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(headerLine); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write([]byte{'\n'}); err != nil {
		_ = file.Close()
		return err
	}
	for _, entry := range entries {
		if _, err := file.Write(entry.raw); err != nil {
			_ = file.Close()
			return err
		}
		if _, err := file.Write([]byte{'\n'}); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		return err
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirHandle.Close()
	return dirHandle.Sync()
}
