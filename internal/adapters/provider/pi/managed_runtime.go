package pi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	RuntimeSourceSystem  = "system"
	RuntimeSourceCustom  = "custom"
	RuntimeSourceMissing = "missing"
)

// RuntimeSettings persists external Pi path and npm mirror preferences inside
// Vantry's private DataDir. Unknown legacy fields from older configs are
// ignored safely on read.
type RuntimeSettings struct {
	CustomPath  string `json:"customPath,omitempty"`
	MirrorMode  string `json:"mirrorMode"` // auto | cn | direct
	NPMRegistry string `json:"npmRegistry,omitempty"`
}

// PiRuntimeResolution describes the discovered external Pi runtime and the
// active executable selection.
type PiRuntimeResolution struct {
	Settings      RuntimeSettings
	ActiveSource  string
	Path          string
	ActiveVersion string
	CustomPath    string
	SystemPath    string
	SystemVersion string
	SystemIsNPM   bool
	ActiveIsNPM   bool
}

func defaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{
		MirrorMode: MirrorModeAuto,
	}
}

func runtimeSettingsPath(dataDir string) string {
	return filepath.Join(dataDir, "pi-runtime.json")
}

// LoadRuntimeSettings reads saved external Pi preferences, returning safe
// defaults when unset or unreadable.
func LoadRuntimeSettings(dataDir string) RuntimeSettings {
	defaults := defaultRuntimeSettings()
	if strings.TrimSpace(dataDir) == "" {
		return defaults
	}
	raw, err := os.ReadFile(runtimeSettingsPath(dataDir))
	if err != nil {
		return defaults
	}
	var loaded RuntimeSettings
	if json.Unmarshal(raw, &loaded) != nil {
		return defaults
	}
	loaded.CustomPath = strings.TrimSpace(loaded.CustomPath)
	loaded.MirrorMode = normalizeMirrorMode(loaded.MirrorMode)
	if v, err := normalizeBaseURL(loaded.NPMRegistry); err == nil {
		loaded.NPMRegistry = v
	} else {
		loaded.NPMRegistry = ""
	}
	return loaded
}

// SaveRuntimeSettings validates and atomically persists external Pi preferences.
func SaveRuntimeSettings(dataDir string, input RuntimeSettings) (RuntimeSettings, error) {
	if strings.TrimSpace(dataDir) == "" {
		return RuntimeSettings{}, errors.New("dataDir 不能为空")
	}
	if err := makePrivateDir(dataDir); err != nil {
		return RuntimeSettings{}, err
	}
	settings := RuntimeSettings{
		MirrorMode: normalizeMirrorMode(input.MirrorMode),
	}
	custom := strings.TrimSpace(input.CustomPath)
	if custom != "" && custom != "pi" {
		resolved, ok := resolveExistingExecutable(custom)
		if !ok {
			return RuntimeSettings{}, errors.New("指定的 Pi 可执行文件不存在或不可执行：" + custom)
		}
		settings.CustomPath = resolved
	}
	if strings.TrimSpace(input.NPMRegistry) != "" {
		normalized, err := normalizeBaseURL(input.NPMRegistry)
		if err != nil {
			return RuntimeSettings{}, err
		}
		settings.NPMRegistry = normalized
	}
	if err := writeJSONFile(runtimeSettingsPath(dataDir), settings); err != nil {
		return RuntimeSettings{}, err
	}
	InvalidateMirrorRouteCache()
	return settings, nil
}

func resolveExistingExecutable(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	candidate := trimmed
	if !filepath.IsAbs(candidate) && !strings.ContainsAny(candidate, `/\`) {
		if hit, err := exec.LookPath(candidate); err == nil && hit != "" {
			candidate = hit
		}
	}
	if abs, err := filepath.Abs(candidate); err == nil {
		candidate = abs
	}
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
		candidate = resolved
	}
	if !isExecutableFile(candidate) {
		return "", false
	}
	return candidate, true
}

func isExecutableFile(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

func findSystemPiExecutable() (string, bool) {
	var candidates []string
	if pathHit, err := exec.LookPath("pi"); err == nil && pathHit != "" {
		candidates = append(candidates, pathHit)
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			candidates = append(candidates, filepath.Join(appData, "npm", "pi.cmd"))
		}
		if home != "" {
			candidates = append(candidates,
				filepath.Join(home, "AppData", "Roaming", "npm", "pi.cmd"),
				filepath.Join(home, "scoop", "shims", "pi.cmd"),
			)
		}
		if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
			candidates = append(candidates, filepath.Join(localApp, "Programs", "pi", "pi.exe"))
		}
	} else {
		if home != "" {
			candidates = append(candidates, filepath.Join(home, ".local", "bin", "pi"))
		}
		candidates = append(candidates, "/opt/homebrew/bin/pi", "/usr/local/bin/pi")
		if home != "" {
			nvm, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin", "pi"))
			sort.Sort(sort.Reverse(sort.StringSlice(nvm)))
			candidates = append(candidates, nvm...)
		}
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		resolved := candidate
		if abs, err := filepath.Abs(resolved); err == nil {
			resolved = abs
		}
		if linkResolved, err := filepath.EvalSymlinks(resolved); err == nil {
			resolved = linkResolved
		}
		if !isExecutableFile(resolved) {
			continue
		}
		return resolved, isNPMInstallPath(resolved)
	}
	return "", false
}

func findSiblingTool(piPath, unixName, winName string) string {
	name := unixName
	if runtime.GOOS == "windows" {
		name = winName
	}
	if strings.TrimSpace(piPath) != "" && filepath.IsAbs(piPath) {
		dir := filepath.Dir(piPath)
		for i := 0; i < 8; i++ {
			for _, cand := range []string{filepath.Join(dir, name), filepath.Join(dir, "bin", name)} {
				if isExecutableFile(cand) {
					return cand
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if hit, err := exec.LookPath(name); err == nil && hit != "" {
		return hit
	}
	home, _ := os.UserHomeDir()
	var fallbacks []string
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			fallbacks = append(fallbacks, filepath.Join(pf, "nodejs", name))
		}
		if appData := os.Getenv("APPDATA"); appData != "" {
			fallbacks = append(fallbacks, filepath.Join(appData, "npm", name))
		}
		if home != "" {
			fallbacks = append(fallbacks,
				filepath.Join(home, "AppData", "Roaming", "npm", name),
				filepath.Join(home, "scoop", "shims", name),
			)
		}
	} else {
		fallbacks = append(fallbacks, "/opt/homebrew/bin/"+name, "/usr/local/bin/"+name)
		if home != "" {
			nvm, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin", name))
			sort.Sort(sort.Reverse(sort.StringSlice(nvm)))
			fallbacks = append(fallbacks, nvm...)
		}
	}
	for _, cand := range fallbacks {
		if isExecutableFile(cand) {
			return cand
		}
	}
	return ""
}

func findSiblingNPM(piPath string) string {
	return findSiblingTool(piPath, "npm", "npm.cmd")
}

func findSiblingNode(piPath string) string {
	return findSiblingTool(piPath, "node", "node.exe")
}

func detectNodeVersion(ctx context.Context, piPath string) string {
	nodeExe := findSiblingNode(piPath)
	if nodeExe == "" {
		return ""
	}
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(runCtx, nodeExe, "--version").Output()
	if err != nil {
		return ""
	}
	ver := strings.TrimSpace(string(out))
	ver = strings.TrimPrefix(strings.TrimPrefix(ver, "v"), "V")
	return ver
}

func isNPMInstallPath(resolvedPath string) bool {
	slash := filepath.ToSlash(resolvedPath)
	if strings.Contains(slash, "/node_modules/@earendil-works/pi-coding-agent/") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(resolvedPath))
	if ext == ".cmd" || ext == ".bat" {
		if raw, err := os.ReadFile(resolvedPath); err == nil {
			if strings.Contains(string(raw), "pi-coding-agent") {
				return true
			}
		}
	}
	return false
}

func readPiPackageVersion(executable string) string {
	if strings.TrimSpace(executable) == "" {
		return ""
	}
	dir := filepath.Dir(executable)
	for _, candidate := range []string{
		filepath.Join(dir, "package.json"),
		filepath.Join(dir, "..", "package.json"),
		filepath.Join(dir, "..", "..", "package.json"),
	} {
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var pkg struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if json.Unmarshal(raw, &pkg) == nil && strings.TrimSpace(pkg.Version) != "" {
			if pkg.Name == "" || pkg.Name == PiNPMPackage {
				return strings.TrimPrefix(strings.TrimSpace(pkg.Version), "v")
			}
		}
	}
	return ""
}

type runtimeResolveOptions struct {
	Requested   string
	DataDir     string
	SystemProbe func() (string, bool)
}

// ResolvePiRuntime resolves the active external Pi executable and system Pi discovery.
func ResolvePiRuntime(requested, dataDir string) PiRuntimeResolution {
	return resolvePiRuntimeWith(runtimeResolveOptions{
		Requested:   requested,
		DataDir:     dataDir,
		SystemProbe: findSystemPiExecutable,
	})
}

func resolvePiRuntimeWith(opts runtimeResolveOptions) PiRuntimeResolution {
	settings := LoadRuntimeSettings(opts.DataDir)
	req := strings.TrimSpace(opts.Requested)
	if req == "" {
		req = strings.TrimSpace(os.Getenv("VANTRY_PI_EXECUTABLE"))
	}
	res := PiRuntimeResolution{
		Settings:   settings,
		CustomPath: settings.CustomPath,
	}

	probe := opts.SystemProbe
	if probe == nil {
		probe = findSystemPiExecutable
	}
	res.SystemPath, res.SystemIsNPM = probe()
	if res.SystemPath != "" {
		res.SystemVersion = readPiPackageVersion(res.SystemPath)
	}

	// 1. Explicit CLI/env override always wins (including test stubs).
	if req != "" && req != "pi" {
		customPath := req
		if resolved, err := exec.LookPath(req); err == nil {
			customPath = resolved
		}
		if abs, err := filepath.Abs(customPath); err == nil {
			customPath = abs
		}
		if resolved, err := filepath.EvalSymlinks(customPath); err == nil {
			customPath = resolved
		}
		res.ActiveSource = RuntimeSourceCustom
		res.Path = customPath
		res.ActiveVersion = readPiPackageVersion(customPath)
		res.ActiveIsNPM = isNPMInstallPath(customPath)
		return res
	}

	// 2. Saved custom external Pi path in Settings.
	if settings.CustomPath != "" {
		if resolved, ok := resolveExistingExecutable(settings.CustomPath); ok {
			res.ActiveSource = RuntimeSourceCustom
			res.Path = resolved
			res.CustomPath = resolved
			res.ActiveVersion = readPiPackageVersion(resolved)
			res.ActiveIsNPM = isNPMInstallPath(resolved)
			return res
		}
	}

	// 3. Discovered system external Pi CLI.
	if res.SystemPath != "" {
		res.ActiveSource = RuntimeSourceSystem
		res.Path = res.SystemPath
		res.ActiveVersion = res.SystemVersion
		res.ActiveIsNPM = res.SystemIsNPM
		return res
	}

	res.ActiveSource = RuntimeSourceMissing
	res.Path = "pi"
	return res
}

// ResolveExecutable resolves the active external Pi executable path.
func ResolveExecutable(requested, dataDir string) string {
	return ResolvePiRuntime(requested, dataDir).Path
}

// CompareSemver compares semantic version strings (e.g. "1.6.0", "v1.5.2").
func CompareSemver(a, b string) int {
	aCore, aPre := splitSemver(a)
	bCore, bPre := splitSemver(b)
	for i := range aCore {
		if aCore[i] != bCore[i] {
			if aCore[i] < bCore[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	}
	return strings.Compare(aPre, bPre)
}

func splitSemver(version string) ([3]int, string) {
	version = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(version), "v"), "V")
	if i := strings.IndexByte(version, '+'); i >= 0 {
		version = version[:i]
	}
	pre := ""
	if i := strings.IndexByte(version, '-'); i >= 0 {
		version, pre = version[:i], version[i+1:]
	}
	var core [3]int
	for i, part := range strings.SplitN(version, ".", 3) {
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
			if n > 1<<30 {
				n = 1 << 30
				break
			}
		}
		core[i] = n
	}
	return core, pre
}
