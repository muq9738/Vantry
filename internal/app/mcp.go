package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"

	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/internal/mcpconfig"
	"github.com/muq9738/Vantry/pkg/api"
)

func (s *workbenchService) mcp(ctx context.Context, r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	var out api.WorkbenchResponse
	root, err := s.resolveRoot(ctx, r)
	if err != nil {
		if api.CodeOf(err) != api.NotFound {
			return out, err
		}
		root = ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return out, err
	}
	store := mcpconfig.Store{HomeDir: home, Workspace: root, AgentDir: os.Getenv("PI_CODING_AGENT_DIR")}

	switch r.Action {
	case "mcp.upsert":
		scope, configRaw := parseUpsertEnvelope(r.Input)
		if err = store.UpsertInScope(scope, r.Name, configRaw); err != nil {
			return out, err
		}
	case "mcp.remove":
		scope, err := parseScopeInput(r.Input)
		if err != nil {
			return out, err
		}
		if err = store.RemoveInScope(scope, r.Name); err != nil {
			return out, err
		}
	case "mcp.trust":
		var payload struct {
			Trusted *bool `json:"trusted"`
		}
		if err = json.Unmarshal(r.Input, &payload); err != nil || payload.Trusted == nil {
			return out, &api.Error{Code: api.InvalidCommand, Message: "mcp.trust 需要明确的 trusted 布尔值"}
		}
		if err = store.SetTrusted(*payload.Trusted); err != nil {
			return out, err
		}
	}

	catalog, err := store.List()
	if err != nil {
		return out, err
	}
	if r.Action == "mcp.probe" {
		probeOut, probeErr := piprovider.ProbeMCP(ctx, s.effectivePiExecutable(), root)
		if probeErr != nil {
			catalog.ProbeError = probeErr.Error()
		} else {
			catalog.Probed = true
			mergeProbeIntoCatalog(&catalog, probeOut)
		}
	}
	out.MCP = &catalog
	return out, nil
}

func parseScopeInput(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return mcpconfig.ScopeProject, nil
	}
	var payload struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return "", &api.Error{Code: api.InvalidCommand, Message: "无效的作用域参数"}
	}
	if payload.Scope == "" {
		return mcpconfig.ScopeProject, nil
	}
	return payload.Scope, nil
}

// parseUpsertEnvelope unwraps {"scope":..., "config":{...}} when callers
// target a specific scope, while remaining 100% compatible with raw server JSON.
func parseUpsertEnvelope(raw json.RawMessage) (string, json.RawMessage) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return mcpconfig.ScopeProject, raw
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(trimmed, &obj) != nil || obj == nil {
		return mcpconfig.ScopeProject, raw
	}
	configRaw, hasConfig := obj["config"]
	if !hasConfig || len(obj) > 2 {
		return mcpconfig.ScopeProject, raw
	}
	for k := range obj {
		if k != "scope" && k != "config" {
			return mcpconfig.ScopeProject, raw
		}
	}
	if !bytes.HasPrefix(bytes.TrimSpace(configRaw), []byte("{")) {
		return mcpconfig.ScopeProject, raw
	}
	scope := mcpconfig.ScopeProject
	if scopeRaw, ok := obj["scope"]; ok {
		var s string
		if json.Unmarshal(scopeRaw, &s) == nil && s != "" {
			scope = s
		}
	}
	return scope, configRaw
}

func mergeProbeIntoCatalog(catalog *api.MCPCatalog, probe piprovider.MCPProbeOutput) {
	byName := make(map[string]piprovider.MCPProbeServer, len(probe.Servers))
	for _, srv := range probe.Servers {
		byName[srv.Name] = srv
	}
	seen := make(map[string]bool, len(catalog.Servers))
	for i := range catalog.Servers {
		srv := &catalog.Servers[i]
		seen[srv.Name] = true
		live, ok := byName[srv.Name]
		if !ok {
			continue
		}
		srv.LiveState = live.State
		srv.Tools = append([]string(nil), live.Tools...)
		srv.LiveError = live.Error
		if srv.Exposure == "" && live.Exposure != "" {
			srv.Exposure = live.Exposure
		}
	}
	for _, live := range probe.Servers {
		if seen[live.Name] {
			continue
		}
		transport := "stdio"
		endpoint := live.Transport
		if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
			transport = "http"
		}
		exposure := live.Exposure
		if exposure == "" {
			exposure = "codemode"
		}
		catalog.Servers = append(catalog.Servers, api.MCPServerInfo{
			Name:      live.Name,
			Scope:     live.Scope,
			Transport: transport,
			Endpoint:  endpoint,
			Enabled:   live.Enabled,
			Exposure:  exposure,
			LiveState: live.State,
			Tools:     append([]string(nil), live.Tools...),
			LiveError: live.Error,
		})
	}
}
