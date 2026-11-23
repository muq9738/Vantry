package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type MCPProbeServer struct {
	Name      string   `json:"name"`
	Scope     string   `json:"scope"`
	Source    string   `json:"source"`
	Enabled   bool     `json:"enabled"`
	Exposure  string   `json:"exposure"`
	Transport string   `json:"transport"`
	State     string   `json:"state"`
	Tools     []string `json:"tools"`
	Error     string   `json:"error"`
}

type MCPProbeOutput struct {
	Servers []MCPProbeServer  `json:"servers"`
	Errors  []json.RawMessage `json:"errors"`
}

// ProbeMCP invokes Pi's native `pi mcp list --json` check so Vantry reports
// live MCP state and discovered tools without implementing its own MCP client.
// Note: Pi exits with status 1 whenever any enabled server fails or needs
// auth while still writing complete structured JSON to stdout.
func ProbeMCP(ctx context.Context, executable, workspace string) (MCPProbeOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	executable = resolvePiExecutable(executable)
	cmd, err := newPiQueryCommand(ctx, executable, "mcp", "list", "--json")
	if err != nil {
		return MCPProbeOutput{}, err
	}
	defer cmd.Close()
	cmd.Env = piEnvironment(executable)
	if workspace != "" {
		cmd.Dir = workspace
	}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	trimmed := bytes.TrimSpace(stdout.Bytes())
	var out MCPProbeOutput
	if len(trimmed) > 0 {
		if decodeErr := json.Unmarshal(trimmed, &out); decodeErr == nil {
			for i := range out.Servers {
				out.Servers[i].Transport = sanitizeProbeTransport(out.Servers[i].Transport)
			}
			return out, nil
		}
	}
	if runErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return MCPProbeOutput{}, fmt.Errorf("pi mcp list: %w (%s)", runErr, detail)
		}
		return MCPProbeOutput{}, fmt.Errorf("pi mcp list: %w", runErr)
	}
	return MCPProbeOutput{}, errors.New("pi mcp list returned invalid JSON output")
}

func sanitizeProbeTransport(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	}
	return trimmed
}
