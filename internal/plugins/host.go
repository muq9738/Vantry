// Package plugins discovers declarative native panels and bounded process plugins.
// Existing JavaScript files are preserved as inactive legacy installations.
package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/muq9738/Vantry/pkg/api"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	maxManifestBytes      = 64 << 10
	maxSourceBytes        = 1 << 20
	maxJSONBytes          = 1 << 20
	maxWorkspaceReadBytes = 256 << 10
	maxWorkspaceListItems = 4096
	invocationLimit       = 2 * time.Second
	defaultAsyncTimeout   = 10 * time.Second
	defaultTimeoutMs      = 2000
	maxExtendedTimeoutMs  = 30000
	runtimeNative         = "native"
	runtimeProcess        = "process"
	permWorkspaceRead     = "workspace:read"
)

var defaultPluginHTTPClient = &http.Client{Timeout: 25 * time.Second}

// Host discovers, validates, and executes plugins from Directory.
type Host struct {
	Directory       string
	DataDir         string
	WorkspaceDir    string
	Locale          string
	AppVersion      string
	HTTPClient      *http.Client
	GitHubAPIBase   string
	IncludeBuiltins bool
}

type pluginPermissions struct {
	WorkspaceRead bool
	Network       []string
	Process       []string
}

type definition struct {
	ID            string
	Name          string
	Version       string
	MinAppVersion string
	Description   string
	Entry         string
	Main          string
	Runtime       string
	Command       []string
	Async         bool
	TimeoutMs     int
	Repo          string
	Builtin       bool
	Settings      []api.PluginSettingSpec
	Navigation    []api.PluginNavItem
	Pages         map[string]json.RawMessage

	perms       pluginPermissions
	isMyGoStyle bool
	pluginDir   string
	origin      Origin
	hasOrigin   bool
	err         error
}

type rawManifest struct {
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Version       string                     `json:"version"`
	MinAppVersion string                     `json:"minAppVersion"`
	Description   string                     `json:"description"`
	Entry         string                     `json:"entry"`
	Main          string                     `json:"main"`
	Runtime       string                     `json:"runtime"`
	Command       []string                   `json:"command"`
	Async         bool                       `json:"async"`
	TimeoutMs     int                        `json:"timeoutMs"`
	Repo          string                     `json:"repo"`
	Builtin       bool                       `json:"builtin"`
	Permissions   json.RawMessage            `json:"permissions"`
	Settings      []api.PluginSettingSpec    `json:"settings"`
	Navigation    []api.PluginNavItem        `json:"navigation"`
	Pages         map[string]json.RawMessage `json:"pages"`
}

func (h Host) effectiveHTTPClient() *http.Client {
	if h.HTTPClient != nil {
		return h.HTTPClient
	}
	return defaultPluginHTTPClient
}

func (d definition) effectiveRuntime() string {
	if d.Runtime == "" && d.Entry == "" && d.Main == "" {
		return runtimeNative
	}
	return d.Runtime
}

func (d definition) effectiveEntry() string {
	if d.Entry != "" {
		return d.Entry
	}
	if d.Main != "" {
		return d.Main
	}
	if d.isMyGoStyle && d.pluginDir != "" {
		if root, err := os.OpenRoot(d.pluginDir); err == nil {
			defer root.Close()
			for _, candidate := range []string{"index.js", "index.ts"} {
				if info, err := lstatNoSymlink(root, candidate); err == nil && info.Mode().IsRegular() {
					return candidate
				}
			}
		}
		return "index.js"
	}
	return ""
}

func (d definition) isEventLoopPlugin() bool {
	return d.Async ||
		d.isMyGoStyle ||
		len(d.perms.Network) > 0 ||
		len(d.perms.Process) > 0 ||
		len(d.Settings) > 0 ||
		len(d.Navigation) > 0
}

func (d definition) requiresRunExport() bool {
	return !d.isMyGoStyle && len(d.Navigation) == 0
}

func (d definition) effectiveTimeout() time.Duration {
	if d.TimeoutMs > 0 {
		return time.Duration(d.TimeoutMs) * time.Millisecond
	}
	if d.isMyGoStyle || len(d.perms.Network) > 0 || len(d.perms.Process) > 0 {
		return defaultAsyncTimeout
	}
	return invocationLimit
}

func (d definition) hasWorkspaceRead() bool {
	return d.perms.WorkspaceRead
}

func parsePluginPermissions(raw json.RawMessage) (pluginPermissions, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return pluginPermissions{}, false, nil
	}
	switch trimmed[0] {
	case '[':
		var list []string
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return pluginPermissions{}, false, fmt.Errorf("解析 permissions 数组：%w", err)
		}
		var out pluginPermissions
		for _, item := range list {
			switch {
			case item == permWorkspaceRead:
				out.WorkspaceRead = true
			case strings.HasPrefix(item, "network:"):
				hostPat := strings.TrimSpace(strings.TrimPrefix(item, "network:"))
				if err := validateNetworkPattern(hostPat); err != nil {
					return pluginPermissions{}, false, err
				}
				if !slices.Contains(out.Network, hostPat) {
					out.Network = append(out.Network, hostPat)
				}
			case strings.HasPrefix(item, "process:"):
				cmd := strings.TrimSpace(strings.TrimPrefix(item, "process:"))
				if err := validateProcessAllowItem(cmd); err != nil {
					return pluginPermissions{}, false, err
				}
				if !slices.Contains(out.Process, cmd) {
					out.Process = append(out.Process, cmd)
				}
			default:
				return pluginPermissions{}, false, fmt.Errorf("不支持的插件权限：%q", item)
			}
		}
		return out, false, nil

	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &fields); err != nil {
			return pluginPermissions{}, true, fmt.Errorf("解析 permissions 对象：%w", err)
		}
		var out pluginPermissions
		for k, v := range fields {
			vTrim := bytes.TrimSpace(v)
			switch k {
			case "workspaceRead", permWorkspaceRead:
				var allow bool
				if err := json.Unmarshal(vTrim, &allow); err != nil {
					return pluginPermissions{}, true, fmt.Errorf("permissions.%s 必须是布尔值", k)
				}
				if allow {
					out.WorkspaceRead = true
				}
			case "network":
				if bytes.Equal(vTrim, []byte("null")) {
					continue
				}
				if bytes.Equal(vTrim, []byte("true")) {
					if !slices.Contains(out.Network, "*") {
						out.Network = append(out.Network, "*")
					}
					continue
				}
				if bytes.Equal(vTrim, []byte("false")) {
					continue
				}
				var hosts []string
				if err := json.Unmarshal(vTrim, &hosts); err != nil {
					return pluginPermissions{}, true, fmt.Errorf("permissions.network 必须是字符串数组：%w", err)
				}
				for _, hp := range hosts {
					hp = strings.TrimSpace(hp)
					if err := validateNetworkPattern(hp); err != nil {
						return pluginPermissions{}, true, err
					}
					if !slices.Contains(out.Network, hp) {
						out.Network = append(out.Network, hp)
					}
				}
			case "process":
				if bytes.Equal(vTrim, []byte("null")) {
					continue
				}
				var cmds []string
				if err := json.Unmarshal(vTrim, &cmds); err != nil {
					return pluginPermissions{}, true, fmt.Errorf("permissions.process 必须是字符串数组：%w", err)
				}
				for _, cmd := range cmds {
					cmd = strings.TrimSpace(cmd)
					if err := validateProcessAllowItem(cmd); err != nil {
						return pluginPermissions{}, true, err
					}
					if !slices.Contains(out.Process, cmd) {
						out.Process = append(out.Process, cmd)
					}
				}
			default:
				return pluginPermissions{}, true, fmt.Errorf("不支持的插件权限：%q", k)
			}
		}
		return out, true, nil

	default:
		return pluginPermissions{}, false, errors.New("permissions 必须是字符串数组或对象")
	}
}

func validateNetworkPattern(pat string) error {
	if pat == "" || strings.ContainsAny(pat, " \t\r\n/\x00") {
		return fmt.Errorf("非法的网络域名白名单项：%q", pat)
	}
	if pat == "*" {
		return nil
	}
	if strings.HasPrefix(pat, "*.") {
		rest := pat[2:]
		if rest == "" || strings.Contains(rest, "*") {
			return fmt.Errorf("非法的通配网络域名：%q", pat)
		}
		return nil
	}
	if strings.Contains(pat, "*") {
		return fmt.Errorf("非法的网络域名通配符：%q", pat)
	}
	return nil
}

func validateProcessAllowItem(cmd string) error {
	if cmd == "" || cmd == "." || cmd == ".." || strings.ContainsAny(cmd, "/\\\x00") {
		return fmt.Errorf("非法的进程白名单命令：%q", cmd)
	}
	return nil
}

func (h Host) definitions(ctx context.Context) (map[string]definition, error) {
	result := map[string]definition{}
	if strings.TrimSpace(h.Directory) != "" {
		entries, err := os.ReadDir(h.Directory)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("读取插件目录：%w", err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			id := name
			d := definition{ID: id, Name: id, pluginDir: filepath.Join(h.Directory, id)}
			if entry.Type()&os.ModeSymlink != 0 {
				d.err = errors.New("插件目录不能是符号链接")
			} else {
				d = h.definitionFromDir(id, filepath.Join(h.Directory, id))
			}
			result[id] = d
		}
	}

	if h.IncludeBuiltins {
		for _, d := range nativeBuiltins() {
			if _, exists := result[d.ID]; !exists {
				result[d.ID] = d
			}
		}
	}
	return result, nil
}

func (h Host) lookupDefinition(ctx context.Context, id string) (definition, bool) {
	if !isValidPluginID(id) {
		return definition{}, false
	}
	if strings.TrimSpace(h.Directory) != "" {
		dir := filepath.Join(h.Directory, id)
		if st, err := os.Lstat(dir); err == nil {
			if st.Mode()&os.ModeSymlink != 0 {
				return definition{ID: id, Name: id, pluginDir: dir, err: errors.New("插件目录不能是符号链接")}, true
			}
			if st.IsDir() {
				return h.definitionFromDir(id, dir), true
			}
		}
	}
	if h.IncludeBuiltins {
		for _, d := range nativeBuiltins() {
			if d.ID == id {
				return d, true
			}
		}
	}
	return definition{}, false
}

func (h Host) definition(id string) definition {
	return h.definitionFromDir(id, filepath.Join(h.Directory, id))
}

func (h Host) definitionFromDir(id, pluginDir string) definition {
	d := definition{ID: id, Name: id, pluginDir: pluginDir}
	raw, err := readPluginFile(pluginDir, "plugin.json", maxManifestBytes)
	if err != nil {
		d.err = fmt.Errorf("读取 plugin.json：%w", err)
		return d
	}
	var parsed rawManifest
	if err := json.Unmarshal(raw, &parsed); err != nil {
		d.err = fmt.Errorf("解析 plugin.json：%w", err)
		return d
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		d.err = errors.New("plugin.json 的 id 必须与插件目录名相同")
		return d
	}
	if parsed.ID == "" && parsed.Main != "" {
		parsed.ID = id
	}
	if parsed.ID != id {
		d.err = errors.New("plugin.json 的 id 必须与插件目录名相同")
		return d
	}
	if strings.TrimSpace(parsed.Name) == "" {
		d.err = errors.New("plugin.json 的 name 不能为空")
		return d
	}

	d.Name = parsed.Name
	d.Version = strings.TrimSpace(parsed.Version)
	d.MinAppVersion = strings.TrimSpace(parsed.MinAppVersion)
	d.Description = parsed.Description
	d.Entry = parsed.Entry
	d.Main = parsed.Main
	d.Runtime = parsed.Runtime
	d.Command = parsed.Command
	d.Async = parsed.Async
	d.TimeoutMs = parsed.TimeoutMs
	d.Repo = strings.TrimSpace(parsed.Repo)
	d.Builtin = parsed.Builtin
	d.Settings = parsed.Settings
	d.Navigation = parsed.Navigation
	d.Pages = parsed.Pages
	if id == "vantry-remote-desktop" {
		applyNativeDesktop(&d)
	}

	if _, ok := readBuiltinMarker(pluginDir); ok && id != "vantry-remote-desktop" {
		d.Builtin = true
	}
	if orig, ok := readOrigin(pluginDir); ok {
		d.origin = orig
		d.hasOrigin = true
		if orig.Repo != "" {
			d.Repo = orig.Repo
		}
	}

	if d.Entry != "" && d.Main != "" && d.Entry != d.Main {
		d.err = errors.New("plugin.json 不能同时指定不同的 entry 与 main")
		return d
	}

	perms, permIsObj, permErr := parsePluginPermissions(parsed.Permissions)
	if permErr != nil {
		d.err = permErr
		return d
	}
	d.perms = perms
	d.isMyGoStyle = d.Main != "" ||
		permIsObj ||
		d.Version != "" ||
		d.MinAppVersion != "" ||
		len(d.Settings) > 0 ||
		len(d.Navigation) > 0

	rt := d.effectiveRuntime()
	if rt != runtimeNative && rt != runtimeProcess {
		d.err = errors.New("脚本插件已停用；原文件和设置已保留，请迁移为 JSON 原生页面或 process 插件")
		return d
	}
	if d.TimeoutMs < 0 {
		d.err = errors.New("plugin.json 的 timeoutMs 不能为负数")
		return d
	}
	if rt == runtimeProcess || d.isEventLoopPlugin() {
		if d.TimeoutMs > maxExtendedTimeoutMs {
			d.err = fmt.Errorf("plugin.json 的 timeoutMs 不能超过 %d", maxExtendedTimeoutMs)
			return d
		}
	} else if d.TimeoutMs > defaultTimeoutMs {
		d.err = fmt.Errorf("同步插件的 timeoutMs 不能超过 %d", defaultTimeoutMs)
		return d
	}
	if rt == runtimeNative && len(d.Command) > 0 {
		d.err = errors.New("原生页面不支持配置 command")
		return d
	}
	if d.MinAppVersion != "" && strings.TrimSpace(h.AppVersion) != "" {
		if cmp, ok := compareSemver(h.AppVersion, d.MinAppVersion); ok && cmp < 0 {
			d.err = fmt.Errorf("插件要求最低宿主版本 %s（当前版本 %s）", d.MinAppVersion, h.AppVersion)
			return d
		}
	}
	if err := validateSettingsSpecs(d.Settings); err != nil {
		d.err = err
		return d
	}
	if err := validateNavSpecs(d.Navigation); err != nil {
		d.err = err
		return d
	}
	return d
}

func validateSettingsSpecs(specs []api.PluginSettingSpec) error {
	seen := make(map[string]struct{}, len(specs))
	for i, s := range specs {
		key := strings.TrimSpace(s.Key)
		if key == "" {
			return fmt.Errorf("settings[%d].key 不能为空", i)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("settings 包含重复 key %q", key)
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(s.Label) == "" {
			return fmt.Errorf("settings[%d].label 不能为空", i)
		}
		switch s.Type {
		case "", "string", "select", "boolean", "number", "password":
		default:
			return fmt.Errorf("settings[%d] 不支持的 type：%q", i, s.Type)
		}
	}
	return nil
}

func validateNavSpecs(items []api.PluginNavItem) error {
	for i, item := range items {
		if strings.TrimSpace(item.Page) == "" {
			return fmt.Errorf("navigation[%d].page 不能为空", i)
		}
		if strings.TrimSpace(item.Title) == "" {
			return fmt.Errorf("navigation[%d].title 不能为空", i)
		}
		if item.NativePanel != "" && item.NativePanel != "remote-desktop" && item.NativePanel != "remote-access" {
			return fmt.Errorf("navigation[%d].nativePanel 不支持 %q", i, item.NativePanel)
		}
	}
	return nil
}

func (h Host) buildPluginInfo(ctx context.Context, d definition) api.PluginInfo {
	tr := loadPluginTranslator(d.pluginDir, h.effectiveLocale())
	needsGrant := len(d.perms.Process) > 0
	granted := false
	if needsGrant {
		granted = h.Granted(d.ID)
	}
	storedSettings := h.readSettingsMap(d.ID)

	version := d.Version
	if version == "" && d.hasOrigin {
		version = d.origin.Version
	}

	hasUpdate := false
	latestVersion := ""
	if !d.Builtin && d.hasOrigin && d.origin.LatestVersion != "" {
		latestVersion = d.origin.LatestVersion
		currentCmp := d.origin.Version
		if currentCmp == "" {
			currentCmp = version
		}
		hasUpdate = currentCmp != "" && latestVersion != currentCmp
	}

	item := api.PluginInfo{
		Disabled:          effectiveSettingValue(d, storedSettings, "enabled") == "false",
		ID:                d.ID,
		Name:              tr(d.Name),
		Version:           version,
		Description:       tr(d.Description),
		Repo:              d.Repo,
		Builtin:           d.Builtin,
		HasUpdate:         hasUpdate,
		LatestVersion:     latestVersion,
		NeedsProcessGrant: needsGrant,
		Granted:           granted,
		Settings:          populatePluginSettings(d, storedSettings, tr),
		Navigation:        populatePluginNavigation(d, tr),
	}

	if item.Disabled {
		item.Navigation = nil
	}
	var checkErr error
	if d.err != nil {
		checkErr = d.err
	} else if d.effectiveRuntime() == runtimeProcess {
		_, checkErr = resolveProcessCommand(d.pluginDir, d)
	} else {
		checkErr = validateNative(d)
	}
	if checkErr != nil {
		item.Error = checkErr.Error()
	}
	return item
}

// List validates configuration, entry files and compilation without evaluating
// plugin code. A broken installation is an item with Error, not a missing item.
func (h Host) List() ([]api.PluginInfo, error) {
	return h.ListContext(context.Background())
}

// ListContext lets callers cancel discovery/compilation. List remains available
// for existing callers; each compilation has the same two-second ceiling.
func (h Host) ListContext(ctx context.Context) ([]api.PluginInfo, error) {
	if ctx == nil {
		return nil, errors.New("plugins: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defs, err := h.definitions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.PluginInfo, 0, len(defs))
	for _, d := range defs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, h.buildPluginInfo(ctx, d))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Limit reads before allocating the whole file. Root also prevents a file
// swapped for a symlink during the read from escaping the plugin directory.
func readPluginFile(directory, name string, limit int64) ([]byte, error) {
	cleanRel, ok := cleanPluginRelPath(name)
	if !ok {
		return nil, fmt.Errorf("非法相对路径 %q", name)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readRegularFileInRoot(root, cleanRel, limit)
}

func readRegularFileInRoot(root *os.Root, cleanRel string, limit int64) ([]byte, error) {
	info, err := lstatNoSymlink(root, cleanRel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("必须是 %d 字节以内的普通文件（不支持符号链接）", limit)
	}
	file, err := root.Open(filepath.FromSlash(cleanRel))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("文件超过 %d 字节", limit)
	}
	return raw, nil
}

func lstatNoSymlink(root *os.Root, cleanRel string) (os.FileInfo, error) {
	if cleanRel == "." {
		return root.Stat(".")
	}
	segments := strings.Split(cleanRel, "/")
	var info os.FileInfo
	for i := range segments {
		prefix := filepath.FromSlash(strings.Join(segments[:i+1], "/"))
		st, err := root.Lstat(prefix)
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("不支持符号链接")
		}
		if i < len(segments)-1 && !st.IsDir() {
			return nil, fmt.Errorf("%q 不是目录", prefix)
		}
		info = st
	}
	return info, nil
}

func cleanPluginRelPath(p string) (string, bool) {
	if p == "" || strings.ContainsRune(p, 0) || strings.ContainsRune(p, '\\') || filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", false
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return "", false
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, seg)
		}
	}
	if len(stack) == 0 {
		return "", false
	}
	return strings.Join(stack, "/"), true
}

func cleanRelDirPath(p string) (string, bool) {
	if p == "" || p == "." || p == "./" {
		return ".", true
	}
	if strings.ContainsRune(p, 0) || strings.ContainsRune(p, '\\') || filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", false
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return "", false
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, seg)
		}
	}
	if len(stack) == 0 {
		return ".", true
	}
	return strings.Join(stack, "/"), true
}
