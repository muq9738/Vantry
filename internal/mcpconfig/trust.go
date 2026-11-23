package mcpconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func (s Store) agentDir() string {
	if s.AgentDir != "" {
		return s.AgentDir
	}
	return filepath.Join(s.HomeDir, ".pi", "agent")
}

// TrustFile returns Pi's per-directory trust store path (`trust.json`).
func (s Store) TrustFile() string {
	return filepath.Join(s.agentDir(), "trust.json")
}

func (s Store) settingsFile() string {
	return filepath.Join(s.agentDir(), "settings.json")
}

func canonicalPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		abs = filepath.Clean(trimmed)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	// Resolve the deepest existing ancestor symlink (e.g. macOS /var -> /private/var)
	// even if the leaf directory has not been created yet.
	cur := abs
	var tail []string
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			parts := []string{resolved}
			for i := len(tail) - 1; i >= 0; i-- {
				parts = append(parts, tail[i])
			}
			return filepath.Clean(filepath.Join(parts...))
		}
	}
}

func readTrustMap(path string) (map[string]bool, map[string]any, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, map[string]any{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(b) > maxBytes {
		return nil, nil, errors.New("Pi trust.json 超过 1 MiB")
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return nil, nil, errors.New("Pi trust.json 格式无效，拒绝覆盖")
	}
	decisions := make(map[string]bool, len(raw))
	for k, v := range raw {
		if bval, ok := v.(bool); ok {
			decisions[k] = bval
		}
	}
	return decisions, raw, nil
}

// IsTrusted reports whether Pi RPC mode will load project-local `.pi/mcp.json`
// for Store.Workspace, matching Pi's nearest-ancestor lookup and defaultProjectTrust.
func (s Store) IsTrusted() bool {
	if strings.TrimSpace(s.Workspace) == "" || !filepath.IsAbs(s.Workspace) {
		return false
	}
	decisions, _, err := readTrustMap(s.TrustFile())
	if err == nil {
		byCanonical := make(map[string]bool, len(decisions))
		for k, v := range decisions {
			byCanonical[canonicalPath(k)] = v
		}
		cur := canonicalPath(s.Workspace)
		for cur != "" {
			if decision, ok := byCanonical[cur]; ok {
				return decision
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
	}
	b, err := os.ReadFile(s.settingsFile())
	if err != nil {
		return false
	}
	var settings struct {
		DefaultProjectTrust string `json:"defaultProjectTrust"`
	}
	if json.Unmarshal(b, &settings) != nil {
		return false
	}
	return settings.DefaultProjectTrust == "always"
}

// SetTrusted records an explicit user trust decision for Store.Workspace in
// Pi's `trust.json`, preserving all other project entries and refusing to
// overwrite malformed JSON.
func (s Store) SetTrusted(trusted bool) error {
	if _, err := s.projectFile(); err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()

	trustPath := s.TrustFile()
	_, raw, err := readTrustMap(trustPath)
	if err != nil {
		return err
	}
	target := canonicalPath(s.Workspace)
	for k := range raw {
		if canonicalPath(k) == target {
			delete(raw, k)
		}
	}
	raw[target] = trusted
	return writeJSONObject0600(trustPath, raw)
}
