package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/muq9738/Vantry/internal/hostprocess"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	maxLocaleBytes      = 256 << 10
	maxStorageFileBytes = 4 << 20
	maxExecStdoutBytes  = 1 << 20
	maxExecStderrBytes  = 64 << 10
	pluginDataDirName   = ".plugin-data"
	originFileName      = ".origin.json"
	builtinFileName     = ".builtin.json"
)

// Origin records repository provenance and version metadata for installed plugins.
type Origin struct {
	Repo          string `json:"repo"`
	Version       string `json:"version"`
	LatestVersion string `json:"latestVersion,omitempty"`
}

type builtinMarker struct {
	Version string `json:"version"`
}

type grantedState struct {
	Granted bool `json:"granted"`
}

var pluginStateMu sync.Mutex

func isValidPluginID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	if strings.HasPrefix(id, ".") || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		if r == '/' || r == '\\' || r == 0 || unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func (h Host) effectiveDataRoot() string {
	if strings.TrimSpace(h.DataDir) != "" {
		return filepath.Join(h.DataDir, pluginDataDirName)
	}
	return filepath.Join(h.Directory, pluginDataDirName)
}

func (h Host) pluginDataDir(pluginID string) (string, error) {
	if !isValidPluginID(pluginID) {
		return "", fmt.Errorf("非法插件 ID：%q", pluginID)
	}
	base := h.effectiveDataRoot()
	if strings.TrimSpace(base) == "" || base == pluginDataDirName {
		return "", errors.New("未配置插件数据目录")
	}
	return filepath.Join(base, pluginID), nil
}

func (h Host) effectiveLocale() string {
	loc := strings.TrimSpace(h.Locale)
	if loc == "" {
		loc = strings.TrimSpace(os.Getenv("VANTRY_LOCALE"))
	}
	if loc == "" {
		loc = "zh-CN"
	}
	return normalizeLocaleTag(loc)
}

func normalizeLocaleTag(raw string) string {
	s := strings.TrimSpace(raw)
	if idx := strings.IndexAny(s, ".@"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.ReplaceAll(s, "_", "-")
	if s == "" || strings.EqualFold(s, "C") || strings.EqualFold(s, "POSIX") {
		return "zh-CN"
	}
	return s
}

// writeAtomicFile writes data to target.part with perm (e.g. 0600) and renames into place.
func writeAtomicFile(target string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	partPath := target + ".part"
	f, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		_ = os.Remove(partPath)
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(partPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(partPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(partPath)
		return err
	}
	if err := os.Rename(partPath, target); err != nil {
		_ = os.Remove(partPath)
		return err
	}
	return nil
}

func readOrigin(pluginDir string) (Origin, bool) {
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	raw, err := readPluginFile(pluginDir, originFileName, maxManifestBytes)
	if err != nil {
		return Origin{}, false
	}
	var o Origin
	if err := json.Unmarshal(raw, &o); err != nil {
		return Origin{}, false
	}
	return o, o.Repo != "" || o.Version != ""
}

func writeOrigin(pluginDir string, o Origin) error {
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	raw, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeAtomicFile(filepath.Join(pluginDir, originFileName), raw, 0o600)
}

func readBuiltinMarker(pluginDir string) (builtinMarker, bool) {
	raw, err := readPluginFile(pluginDir, builtinFileName, maxManifestBytes)
	if err != nil {
		return builtinMarker{}, false
	}
	var m builtinMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return builtinMarker{}, false
	}
	return m, true
}

func writeBuiltinMarker(pluginDir, version string) error {
	raw, err := json.MarshalIndent(builtinMarker{Version: version}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeAtomicFile(filepath.Join(pluginDir, builtinFileName), raw, 0o600)
}

// Granted reports whether external process execution via host.exec has been granted to pluginID.
func (h Host) Granted(pluginID string) bool {
	dataDir, err := h.pluginDataDir(pluginID)
	if err != nil {
		return false
	}
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	raw, err := os.ReadFile(filepath.Join(dataDir, "granted.json"))
	if err != nil {
		return false
	}
	var st grantedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return false
	}
	return st.Granted
}

// SetGranted updates external process execution permission for pluginID in granted.json.
func (h Host) SetGranted(pluginID string, on bool) error {
	dataDir, err := h.pluginDataDir(pluginID)
	if err != nil {
		return err
	}
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	raw, err := json.MarshalIndent(grantedState{Granted: on}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeAtomicFile(filepath.Join(dataDir, "granted.json"), raw, 0o600)
}

func (h Host) readSettingsMap(pluginID string) map[string]string {
	dataDir, err := h.pluginDataDir(pluginID)
	if err != nil {
		return nil
	}
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	raw, err := os.ReadFile(filepath.Join(dataDir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) && h.DataDir != "" {
		raw, err = os.ReadFile(filepath.Join(h.Directory, pluginDataDirName, pluginID, "settings.json"))
	}
	if err != nil {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if pluginID == "vantry-remote-desktop" {
		if _, ok := out["quality"]; !ok && out["defaultQuality"] != "" {
			out["quality"] = out["defaultQuality"]
		}
		if _, ok := out["relayURL"]; !ok && strings.Contains(out["defaultRelayHost"], "://") {
			out["relayURL"] = out["defaultRelayHost"]
		}
	}
	return out
}

func (h Host) writeSettingValue(pluginID, key, value string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("插件设置项 key 不能为空")
	}
	dataDir, err := h.pluginDataDir(pluginID)
	if err != nil {
		return err
	}
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	settingsPath := filepath.Join(dataDir, "settings.json")
	current := map[string]string{}
	raw, err := os.ReadFile(settingsPath)
	if errors.Is(err, os.ErrNotExist) && h.DataDir != "" {
		raw, err = os.ReadFile(filepath.Join(h.Directory, pluginDataDirName, pluginID, "settings.json"))
	}
	if err == nil {
		_ = json.Unmarshal(raw, &current)
		if current == nil {
			current = map[string]string{}
		}
	}
	current[key] = value
	encoded, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxJSONBytes {
		return errors.New("插件 settings.json 超过 1 MiB 上限")
	}
	return writeAtomicFile(settingsPath, encoded, 0o600)
}

func effectiveSettingValue(d definition, stored map[string]string, key string) string {
	if stored != nil {
		if v, ok := stored[key]; ok {
			return v
		}
	}
	for _, spec := range d.Settings {
		if spec.Key == key {
			return spec.Default
		}
	}
	return ""
}

// GetSetting reads the effective setting value for pluginID (stored value in settings.json,
// falling back to the manifest default if declared).
func (h Host) GetSetting(pluginID, key string) (string, error) {
	if !isValidPluginID(pluginID) {
		return "", fmt.Errorf("非法插件 ID：%q", pluginID)
	}
	if strings.TrimSpace(key) == "" {
		return "", errors.New("插件设置项 key 不能为空")
	}
	stored := h.readSettingsMap(pluginID)
	if stored != nil {
		if v, ok := stored[key]; ok {
			return v, nil
		}
	}
	d, ok := h.lookupDefinition(context.Background(), pluginID)
	if ok && d.err == nil {
		return effectiveSettingValue(d, stored, key), nil
	}
	return "", nil
}

// SetSetting persists native settings. Changes never evaluate legacy scripts.
func (h Host) SetSetting(ctx context.Context, pluginID, key, value string) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return h.writeSettingValue(pluginID, key, value)
}

func populatePluginSettings(d definition, stored map[string]string, tr func(string) string) []api.PluginSettingSpec {
	if len(d.Settings) == 0 {
		return nil
	}
	out := make([]api.PluginSettingSpec, len(d.Settings))
	for i, s := range d.Settings {
		spec := s
		spec.Label = tr(spec.Label)
		if spec.Help != "" {
			spec.Help = tr(spec.Help)
		}
		if spec.Type == "" {
			spec.Type = "string"
		}
		spec.Value = effectiveSettingValue(d, stored, spec.Key)
		if len(s.Options) > 0 {
			opts := make([]api.PluginSettingOption, len(s.Options))
			for j, opt := range s.Options {
				opts[j] = api.PluginSettingOption{
					Value: opt.Value,
					Label: tr(opt.Label),
				}
			}
			spec.Options = opts
		}
		out[i] = spec
	}
	return out
}

func populatePluginNavigation(d definition, tr func(string) string) []api.PluginNavItem {
	if len(d.Navigation) == 0 {
		return nil
	}
	out := make([]api.PluginNavItem, len(d.Navigation))
	for i, nav := range d.Navigation {
		out[i] = api.PluginNavItem{
			Title:       tr(nav.Title),
			Page:        nav.Page,
			Icon:        nav.Icon,
			NativePanel: nav.NativePanel,
		}
	}
	return out
}

func (h Host) storageGet(pluginID, key string) (json.RawMessage, bool, error) {
	dataDir, err := h.pluginDataDir(pluginID)
	if err != nil {
		return nil, false, err
	}
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	raw, err := os.ReadFile(filepath.Join(dataDir, "storage.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var store map[string]json.RawMessage
	if err := json.Unmarshal(raw, &store); err != nil {
		return nil, false, err
	}
	val, ok := store[key]
	if !ok || len(val) == 0 {
		return nil, false, nil
	}
	return val, true, nil
}

func (h Host) storageSet(pluginID, key string, val json.RawMessage) error {
	if key == "" {
		return errors.New("storage key 不能为空")
	}
	dataDir, err := h.pluginDataDir(pluginID)
	if err != nil {
		return err
	}
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	storagePath := filepath.Join(dataDir, "storage.json")
	store := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(storagePath); err == nil {
		_ = json.Unmarshal(raw, &store)
		if store == nil {
			store = map[string]json.RawMessage{}
		}
	}
	store[key] = val
	encoded, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxStorageFileBytes {
		return fmt.Errorf("插件 storage.json 超过 %d 字节上限", maxStorageFileBytes)
	}
	return writeAtomicFile(storagePath, encoded, 0o600)
}

func (h Host) storageRemove(pluginID, key string) error {
	dataDir, err := h.pluginDataDir(pluginID)
	if err != nil {
		return err
	}
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	storagePath := filepath.Join(dataDir, "storage.json")
	raw, err := os.ReadFile(storagePath)
	if errors.Is(err, os.ErrNotExist) {
		return writeAtomicFile(storagePath, []byte("{}\n"), 0o600)
	}
	if err != nil {
		return err
	}
	store := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &store); err != nil || store == nil {
		store = map[string]json.RawMessage{}
	}
	delete(store, key)
	encoded, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeAtomicFile(storagePath, encoded, 0o600)
}

func (h Host) storageClear(pluginID string) error {
	dataDir, err := h.pluginDataDir(pluginID)
	if err != nil {
		return err
	}
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	return writeAtomicFile(filepath.Join(dataDir, "storage.json"), []byte("{}\n"), 0o600)
}

func (h Host) storageKeys(pluginID string) ([]string, error) {
	dataDir, err := h.pluginDataDir(pluginID)
	if err != nil {
		return nil, err
	}
	pluginStateMu.Lock()
	defer pluginStateMu.Unlock()

	raw, err := os.ReadFile(filepath.Join(dataDir, "storage.json"))
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var store map[string]json.RawMessage
	if err := json.Unmarshal(raw, &store); err != nil {
		return []string{}, nil
	}
	keys := make([]string, 0, len(store))
	for k := range store {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// loadPluginTranslator loads locales/<lang>.json from pluginDir safely via os.Root.
func loadPluginTranslator(pluginDir, locale string) func(string) string {
	identity := func(s string) string { return s }
	if strings.TrimSpace(pluginDir) == "" {
		return identity
	}
	root, err := os.OpenRoot(pluginDir)
	if err != nil {
		return identity
	}
	defer root.Close()

	info, err := lstatNoSymlink(root, "locales")
	if err != nil || !info.IsDir() {
		return identity
	}
	dirFile, err := root.Open("locales")
	if err != nil {
		return identity
	}
	defer dirFile.Close()

	entries, err := dirFile.ReadDir(128)
	if err != nil && len(entries) == 0 {
		return identity
	}

	target := normalizeLocaleTag(locale)
	targetLower := strings.ToLower(target)
	langPrefix := targetLower
	if idx := strings.IndexByte(langPrefix, '-'); idx > 0 {
		langPrefix = langPrefix[:idx]
	}

	var exactMatch, caseMatch, prefixMatch string
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 || e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		tag := name[:len(name)-5]
		tagLower := strings.ToLower(strings.ReplaceAll(tag, "_", "-"))
		switch {
		case tag == target:
			exactMatch = name
		case tagLower == targetLower && caseMatch == "":
			caseMatch = name
		case (tagLower == langPrefix || strings.HasPrefix(tagLower, langPrefix+"-")) && prefixMatch == "":
			prefixMatch = name
		}
	}

	chosen := exactMatch
	if chosen == "" {
		chosen = caseMatch
	}
	if chosen == "" {
		chosen = prefixMatch
	}
	if chosen == "" {
		return identity
	}

	raw, err := readRegularFileInRoot(root, "locales/"+chosen, maxLocaleBytes)
	if err != nil {
		return identity
	}
	var dict map[string]string
	if err := json.Unmarshal(raw, &dict); err != nil || len(dict) == 0 {
		return identity
	}
	return func(s string) string {
		if v, ok := dict[s]; ok && v != "" {
			return v
		}
		return s
	}
}

func interpolateTemplate(text string, vars map[string]any) string {
	if len(vars) == 0 || !strings.Contains(text, "{") {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		open := strings.IndexByte(text[i:], '{')
		if open < 0 {
			b.WriteString(text[i:])
			break
		}
		open += i
		b.WriteString(text[i:open])
		closeIdx := strings.IndexByte(text[open+1:], '}')
		if closeIdx < 0 {
			b.WriteString(text[open:])
			break
		}
		closeIdx += open + 1
		key := text[open+1 : closeIdx]
		if val, ok := vars[key]; ok {
			fmt.Fprint(&b, val)
		} else {
			b.WriteString(text[open : closeIdx+1])
		}
		i = closeIdx + 1
	}
	return b.String()
}

// compareSemver compares two dot-separated version tags (with optional "v" prefix).
// Returns (-1, true), (0, true), or (+1, true) when parseable.
func compareSemver(a, b string) (int, bool) {
	pa, oka := parseSemverParts(a)
	pb, okb := parseSemverParts(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1, true
		}
		if pa[i] > pb[i] {
			return 1, true
		}
	}
	return 0, true
}

func parseSemverParts(v string) ([3]int, bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V"))
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	if v == "" {
		return [3]int{}, false
	}
	segs := strings.Split(v, ".")
	if len(segs) > 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, seg := range segs {
		n, err := strconv.Atoi(seg)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

type hostExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

func runAllowlistedExec(
	ctx context.Context,
	allowed []string,
	cmdName string,
	args []string,
	stdin string,
	workDir string,
) (hostExecResult, error) {
	if err := ctx.Err(); err != nil {
		return hostExecResult{}, context.Cause(ctx)
	}
	cmdName = strings.TrimSpace(cmdName)
	if cmdName == "" || strings.ContainsAny(cmdName, "/\\\x00") {
		return hostExecResult{}, fmt.Errorf("非法进程命令名：%q", cmdName)
	}
	if !slices.Contains(allowed, cmdName) {
		return hostExecResult{}, fmt.Errorf("命令 %q 未在插件 permissions.process 白名单中", cmdName)
	}
	for i, a := range args {
		if strings.ContainsRune(a, 0) {
			return hostExecResult{}, fmt.Errorf("命令参数[%d] 包含非法空字符", i)
		}
	}
	exePath, err := exec.LookPath(cmdName)
	if err != nil {
		return hostExecResult{}, fmt.Errorf("查找命令 %q：%w", cmdName, err)
	}

	cmd := hostprocess.New(exePath, args...)
	if strings.TrimSpace(workDir) != "" {
		cmd.Dir = workDir
	}
	defer cmd.Close()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return hostExecResult{}, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return hostExecResult{}, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return hostExecResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return hostExecResult{}, fmt.Errorf("启动命令 %q：%w", cmdName, err)
	}

	execCtx, stopExec := context.WithCancel(ctx)
	defer stopExec()

	killed := make(chan struct{})
	stopKill := context.AfterFunc(execCtx, func() {
		_ = cmd.KillTree()
		close(killed)
	})

	var (
		wg             sync.WaitGroup
		stdoutBytes    []byte
		stdoutOverflow bool
		stderrBytes    []byte
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		if stdin != "" {
			_, _ = stdinPipe.Write([]byte(stdin))
		}
		_ = stdinPipe.Close()
	}()
	go func() {
		defer wg.Done()
		stdoutBytes, stdoutOverflow = readBoundedStopOnOverflow(stdoutPipe, maxExecStdoutBytes, stopExec)
	}()
	go func() {
		defer wg.Done()
		stderrBytes, _ = readBoundedDrain(stderrPipe, maxExecStderrBytes)
	}()

	wg.Wait()
	waitErr := cmd.Wait()

	if !stopKill() {
		<-killed
	}
	cmd.EnsureGone()

	if err := ctx.Err(); err != nil {
		return hostExecResult{}, context.Cause(ctx)
	}
	if stdoutOverflow {
		return hostExecResult{}, errors.New("命令标准输出超过 1 MiB 上限")
	}

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return hostExecResult{}, fmt.Errorf("命令 %q 执行失败：%w", cmdName, waitErr)
		}
	}
	return hostExecResult{
		ExitCode: exitCode,
		Stdout:   string(stdoutBytes),
		Stderr:   string(stderrBytes),
	}, nil
}
