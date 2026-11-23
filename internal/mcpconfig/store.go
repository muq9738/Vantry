// Package mcpconfig manages Pi's user and project MCP configuration as well as
// project trust. Pi owns MCP transport, tool discovery, OAuth, codemode QuickJS
// execution, and server process lifetimes.
package mcpconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/muq9738/Vantry/pkg/api"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var writeMu sync.Mutex

const maxBytes = 1 << 20

const (
	ScopeProject         = "project"
	ScopeUser            = "user"
	ScopeProjectOverride = "project-override"
)

type Store struct{ HomeDir, Workspace, AgentDir string }

func (s Store) userFile() string {
	return filepath.Join(s.agentDir(), "mcp.json")
}

func (s Store) projectFile() (string, error) {
	if !filepath.IsAbs(s.Workspace) {
		return "", errors.New("请选择 Core 所在机器的项目目录")
	}
	st, err := os.Stat(s.Workspace)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New("工作区不是目录")
	}
	return filepath.Join(s.Workspace, ".pi", "mcp.json"), nil
}

type document map[string]json.RawMessage

func read(file string) (document, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return document{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > maxBytes {
		return nil, errors.New("MCP 配置超过 1 MiB")
	}
	var d document
	if json.Unmarshal(b, &d) != nil || d == nil {
		return nil, errors.New("MCP 配置不是 JSON 对象")
	}
	return d, nil
}

func servers(d document) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if raw, ok := d["mcpServers"]; ok {
		if json.Unmarshal(raw, &m) != nil || m == nil {
			return nil, errors.New("mcpServers 必须是对象")
		}
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

func normalized(name string) string { return strings.ReplaceAll(name, "-", "_") }

func config(raw json.RawMessage) (document, error) {
	var d document
	if json.Unmarshal(raw, &d) != nil || d == nil {
		return nil, errors.New("服务配置必须是 JSON 对象")
	}
	return d, nil
}

func str(d document, key string) string { var v string; _ = json.Unmarshal(d[key], &v); return v }

func normalizeExposure(value string) string {
	switch value {
	case "", "codemode", "codemode-deferred":
		return "codemode"
	default:
		return value
	}
}

func validExposure(value string) bool {
	switch value {
	case "codemode", "codemode-deferred", "deferred", "direct", "hidden":
		return true
	default:
		return false
	}
}

func unwrapServerInput(name string, raw json.RawMessage) (document, error) {
	d, err := config(raw)
	if err != nil {
		return nil, err
	}
	msRaw, hasWrapper := d["mcpServers"]
	if !hasWrapper || str(d, "command") != "" || str(d, "url") != "" || str(d, "type") != "" {
		return d, nil
	}
	var ms map[string]json.RawMessage
	if json.Unmarshal(msRaw, &ms) != nil || ms == nil {
		return nil, errors.New("mcpServers 必须是对象")
	}
	entry, ok := ms[name]
	if !ok || len(ms) != 1 {
		return nil, errors.New("请填入单个服务对象，或仅包含当前服务名的 mcpServers 包装")
	}
	return config(entry)
}

func validate(name string, d document, inherited bool) error {
	if !namePattern.MatchString(name) {
		return errors.New("服务名只接受字母、数字、横线和下划线")
	}
	command, address, kind := str(d, "command"), str(d, "url"), str(d, "type")
	if command != "" && address != "" {
		return errors.New("command 与 url 只能选一种")
	}
	if kind != "" && kind != "stdio" && kind != "http" && kind != "streamable-http" {
		return errors.New("使用 stdio 或 streamable HTTP，不支持旧 SSE")
	}
	if command == "" && address == "" && !inherited {
		return errors.New("新服务需要 command 或 url")
	}
	if address != "" {
		u, e := url.Parse(address)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("url 必须为完整 HTTP/HTTPS MCP 地址")
		}
	}
	if command != "" && kind != "" && kind != "stdio" || address != "" && kind == "stdio" {
		return errors.New("type 与传输配置不一致")
	}
	for _, key := range []string{"description", "cwd"} {
		if raw, ok := d[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return fmt.Errorf("%s 必须是字符串", key)
			}
		}
	}
	if raw, ok := d["timeout"]; ok {
		var timeout float64
		if json.Unmarshal(raw, &timeout) != nil || timeout <= 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("timeout 必须是大于 0 的数字")
		}
	}
	for _, key := range []string{"args"} {
		if raw, ok := d[key]; ok {
			var a []string
			if json.Unmarshal(raw, &a) != nil {
				return fmt.Errorf("%s 必须是字符串数组", key)
			}
		}
	}
	for _, key := range []string{"headers", "env"} {
		if raw, ok := d[key]; ok {
			var m map[string]string
			if json.Unmarshal(raw, &m) != nil {
				return fmt.Errorf("%s 必须是字符串对象", key)
			}
		}
	}
	if raw, ok := d["oauth"]; ok {
		var o map[string]json.RawMessage
		if json.Unmarshal(raw, &o) != nil || o == nil {
			return errors.New("oauth 必须是 JSON 对象")
		}
	}
	if raw, ok := d["enabled"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("enabled 必须为布尔值")
		}
	}
	if raw, ok := d["exposure"]; ok {
		var exp string
		if json.Unmarshal(raw, &exp) != nil || !validExposure(exp) {
			return errors.New("exposure 无效")
		}
	}
	if raw, ok := d["toolExposure"]; ok {
		var m map[string]string
		if json.Unmarshal(raw, &m) != nil || m == nil {
			return errors.New("toolExposure 必须是字符串对象")
		}
		for _, v := range m {
			if !validExposure(v) {
				return errors.New("toolExposure 包含无效的暴露模式")
			}
		}
	}
	return nil
}

func merge(base, override document) document {
	out := document{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

func inheritedOnly(d document) bool {
	return str(d, "command") == "" && str(d, "url") == "" && str(d, "type") == ""
}

func parseToolExposure(d document) map[string]string {
	raw, ok := d["toolExposure"]
	if !ok {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = normalizeExposure(v)
	}
	return out
}

func autoEnableCodemode(user, local document) bool {
	result := true
	for _, doc := range []document{user, local} {
		if raw, ok := doc["autoEnableCodemode"]; ok {
			var b bool
			if json.Unmarshal(raw, &b) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				result = b
			}
		}
	}
	return result
}

func (s Store) List() (api.MCPCatalog, error) {
	userFile := s.userFile()
	user, err := read(userFile)
	if err != nil {
		return api.MCPCatalog{}, fmt.Errorf("读取用户 MCP 配置：%w", err)
	}
	global, err := servers(user)
	if err != nil {
		return api.MCPCatalog{}, err
	}

	var projectFile string
	localDoc := document{}
	local := map[string]json.RawMessage{}
	trusted := false
	if strings.TrimSpace(s.Workspace) != "" {
		projectFile, err = s.projectFile()
		if err != nil {
			return api.MCPCatalog{}, err
		}
		localDoc, err = read(projectFile)
		if err != nil {
			return api.MCPCatalog{}, err
		}
		local, err = servers(localDoc)
		if err != nil {
			return api.MCPCatalog{}, err
		}
		trusted = s.IsTrusted()
	}

	names := map[string]bool{}
	for n := range global {
		names[n] = true
	}
	for n := range local {
		names[n] = true
	}
	out := api.MCPCatalog{
		Workspace:       s.Workspace,
		UserFile:        userFile,
		ProjectFile:     projectFile,
		ProjectTrusted:  trusted,
		CodemodeDefault: autoEnableCodemode(user, localDoc),
		Servers:         []api.MCPServerInfo{},
		Notice:          "默认采用 Pi 原生 codemode（QuickJS 沙盒按需发现并编排 MCP 工具）。此列表仅反映配置，点击「检测连接」才会调用 pi mcp list 探活。",
	}
	for name := range names {
		raw, scope := global[name], ScopeUser
		base, _ := config(raw)
		if override, ok := local[name]; ok {
			scope = ScopeProject
			d, e := config(override)
			if e != nil {
				out.Servers = append(out.Servers, api.MCPServerInfo{Name: name, Scope: scope, Error: e.Error()})
				continue
			}
			if inheritedOnly(d) {
				if _, inGlobal := global[name]; inGlobal {
					scope = ScopeProjectOverride
				}
				raw, _ = json.Marshal(merge(base, d))
			} else {
				raw = override
			}
		}
		d, e := config(raw)
		if e != nil {
			out.Servers = append(out.Servers, api.MCPServerInfo{Name: name, Scope: scope, Error: e.Error()})
			continue
		}
		info := api.MCPServerInfo{
			Name:         name,
			Scope:        scope,
			Enabled:      true,
			Exposure:     normalizeExposure(str(d, "exposure")),
			ToolExposure: parseToolExposure(d),
			Description:  str(d, "description"),
		}
		if e = validate(name, d, false); e != nil {
			info.Error = e.Error()
		}
		if raw, ok := d["enabled"]; ok {
			_ = json.Unmarshal(raw, &info.Enabled)
		}
		if address := str(d, "url"); address != "" {
			info.Transport = "http"
			if u, e := url.Parse(address); e == nil {
				u.User = nil
				u.RawQuery = ""
				u.Fragment = ""
				info.Endpoint = u.String()
			}
		} else {
			info.Transport = "stdio"
			info.Endpoint = str(d, "command")
		}
		out.Servers = append(out.Servers, info)
	}
	sort.Slice(out.Servers, func(i, j int) bool { return out.Servers[i].Name < out.Servers[j].Name })
	return out, nil
}

// Upsert merges a single server into the current project's `.pi/mcp.json`.
func (s Store) Upsert(name string, raw json.RawMessage) error {
	return s.UpsertInScope(ScopeProject, name, raw)
}

// UpsertInScope merges a single server into either `.pi/mcp.json` (project)
// or `~/.pi/agent/mcp.json` (user), preserving other servers and top-level Pi
// options. New full server definitions default to `"exposure": "codemode"`.
// Project-only enabled/exposure overrides inherit user connections without
// copying credentials.
func (s Store) UpsertInScope(scope, name string, raw json.RawMessage) error {
	if scope == "" {
		scope = ScopeProject
	}
	if scope != ScopeProject && scope != ScopeUser {
		return fmt.Errorf("无效的作用域 %q", scope)
	}
	writeMu.Lock()
	defer writeMu.Unlock()

	var targetFile string
	var err error
	if scope == ScopeUser {
		targetFile = s.userFile()
	} else {
		targetFile, err = s.projectFile()
		if err != nil {
			return err
		}
	}

	targetDoc, err := read(targetFile)
	if err != nil {
		return err
	}
	targetServers, err := servers(targetDoc)
	if err != nil {
		return err
	}

	var global map[string]json.RawMessage
	if scope == ScopeProject {
		user, err := read(s.userFile())
		if err != nil {
			return err
		}
		global, err = servers(user)
		if err != nil {
			return err
		}
		for n := range global {
			if n != name && normalized(n) == normalized(name) {
				return errors.New("服务名与用户服务归一后冲突")
			}
		}
	}
	for n := range targetServers {
		if n != name && normalized(n) == normalized(name) {
			return errors.New("服务名横线与下划线归一后发生冲突")
		}
	}

	incoming, err := unwrapServerInput(name, raw)
	if err != nil {
		return err
	}
	if existing, ok := targetServers[name]; ok {
		prev, e := config(existing)
		if e != nil {
			return e
		}
		incoming = merge(prev, incoming)
	}
	inheritedFromUser := false
	if scope == ScopeProject {
		_, inheritedFromUser = global[name]
	}
	if err = validate(name, incoming, inheritedFromUser); err != nil {
		return err
	}
	if !inheritedOnly(incoming) && str(incoming, "exposure") == "" {
		incoming["exposure"] = json.RawMessage(`"codemode"`)
	}
	targetServers[name], err = json.Marshal(incoming)
	if err != nil {
		return err
	}
	targetDoc["mcpServers"], err = json.Marshal(targetServers)
	if err != nil {
		return err
	}
	return write(targetFile, targetDoc)
}

// RemoveInScope deletes a single server entry (or project override) without
// altering other servers or top-level Pi options.
func (s Store) RemoveInScope(scope, name string) error {
	if scope == "" {
		scope = ScopeProject
	}
	if scope != ScopeProject && scope != ScopeUser {
		return fmt.Errorf("无效的作用域 %q", scope)
	}
	if !namePattern.MatchString(name) {
		return errors.New("服务名只接受字母、数字、横线和下划线")
	}
	writeMu.Lock()
	defer writeMu.Unlock()

	var targetFile string
	var err error
	if scope == ScopeUser {
		targetFile = s.userFile()
	} else {
		targetFile, err = s.projectFile()
		if err != nil {
			return err
		}
	}
	if _, statErr := os.Stat(targetFile); errors.Is(statErr, os.ErrNotExist) {
		return nil
	}
	doc, err := read(targetFile)
	if err != nil {
		return err
	}
	m, err := servers(doc)
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return nil
	}
	delete(m, name)
	doc["mcpServers"], err = json.Marshal(m)
	if err != nil {
		return err
	}
	return write(targetFile, doc)
}

func write(file string, d document) error {
	return writeJSONObject0600(file, d)
}

func writeJSONObject0600(file string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxBytes {
		return errors.New("配置文件超过 1 MiB")
	}
	if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
