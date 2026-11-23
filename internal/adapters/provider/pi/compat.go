package pi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	MinSupportedPiVersion = "1.0.0"
	MaxSupportedPiMajor   = 1

	preflightCacheTTL    = 24 * time.Hour
	contractProbeTimeout = 20 * time.Second
	npmSandboxTimeout    = 90 * time.Second
)

// BlockedPiVersions holds explicit upstream versions known to break Vantry's
// RPC or extension contract.
var BlockedPiVersions = map[string]string{}

// PiPackageMetadata captures npm registry metadata relevant to compatibility.
type PiPackageMetadata struct {
	Version     string `json:"version"`
	EnginesNode string `json:"enginesNode,omitempty"`
	Registry    string `json:"registry,omitempty"`
}

type preflightCacheFile struct {
	Entries map[string]preflightCacheEntry `json:"entries"`
}

type preflightCacheEntry struct {
	CandidateVersion string    `json:"candidateVersion"`
	NodeVersion      string    `json:"nodeVersion,omitempty"`
	ContractHash     string    `json:"contractHash"`
	Verified         bool      `json:"verified"`
	Reason           string    `json:"reason,omitempty"`
	CheckedAt        time.Time `json:"checkedAt"`
}

func vantryContractHash() string {
	h := sha256.New()
	_, _ = h.Write(vantryToolsExtension)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(approvalExtension)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(fmt.Sprintf("min=%s;maxMajor=%d", MinSupportedPiVersion, MaxSupportedPiMajor)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// FetchLatestPiMetadata fetches version and engines.node from the selected npm
// registry (failing over across secondary registries).
func FetchLatestPiMetadata(ctx context.Context, route NetworkRoute, opts MirrorOptions) (PiPackageMetadata, error) {
	resolvedOpts := resolveMirrorOptions(opts)
	client := resolvedOpts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}

	registries := make([]string, 0, 3)
	seen := map[string]bool{}
	addReg := func(r string) {
		r = strings.TrimRight(strings.TrimSpace(r), "/")
		if r != "" && !seen[r] {
			seen[r] = true
			registries = append(registries, r)
		}
	}
	addReg(route.NPMRegistry)
	if resolvedOpts.Mode != MirrorModeDirect {
		addReg(resolvedOpts.ChinaNPM)
	}
	addReg(resolvedOpts.DirectNPM)

	var errs []string
	for _, reg := range registries {
		meta, err := fetchNPMPackageMetadata(ctx, client, reg, reg+"/"+PiNPMPackage+"/latest")
		if err == nil && meta.Version != "" {
			return meta, nil
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	return PiPackageMetadata{}, fmt.Errorf("检查 Pi 最新版本失败：%s", strings.Join(errs, "；"))
}

func fetchNPMPackageMetadata(ctx context.Context, client *http.Client, registry, endpoint string) (PiPackageMetadata, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return PiPackageMetadata{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Vantry-Pi-Updater/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return PiPackageMetadata{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return PiPackageMetadata{}, fmt.Errorf("%s 返回 %s", registry, resp.Status)
	}
	var payload struct {
		Version string `json:"version"`
		Engines struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&payload); err != nil {
		return PiPackageMetadata{}, err
	}
	ver := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(payload.Version), "v"), "V")
	if ver == "" {
		return PiPackageMetadata{}, fmt.Errorf("%s 未返回版本号", registry)
	}
	return PiPackageMetadata{
		Version:     ver,
		EnginesNode: strings.TrimSpace(payload.Engines.Node),
		Registry:    registry,
	}, nil
}

// CheckPiVersionPolicy enforces Gate 1: Vantry's supported semver window,
// stable-only channel, and explicit version blocklist.
func CheckPiVersionPolicy(version string) error {
	clean := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(version), "v"), "V")
	if clean == "" {
		return errors.New("目标 Pi 版本号为空")
	}
	core, pre := splitSemver(clean)
	if pre != "" {
		return fmt.Errorf("Pi v%s 为预发布版本，Vantry 仅开放稳定版升级", clean)
	}
	if reason, blocked := BlockedPiVersions[clean]; blocked {
		if reason == "" {
			reason = fmt.Sprintf("Pi v%s 已被标记为与当前 Vantry 不兼容", clean)
		}
		return errors.New(reason)
	}
	if CompareSemver(clean, MinSupportedPiVersion) < 0 {
		return fmt.Errorf("Pi v%s 低于 Vantry 最低兼容版本 v%s", clean, MinSupportedPiVersion)
	}
	if core[0] > MaxSupportedPiMajor {
		return fmt.Errorf("Pi v%s 为跨主版本更新（当前仅认证 %d.x），需等待 Vantry 适配后再开放升级", clean, MaxSupportedPiMajor)
	}
	return nil
}

// CheckNodeVersionSatisfies validates local Node.js version against npm's
// engines.node semver range constraint.
func CheckNodeVersionSatisfies(actual, constraint string) (bool, string) {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" {
		return true, ""
	}
	cleanActual := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(actual), "v"), "V")
	if cleanActual == "" {
		return false, "未检测到本机 Node.js 版本，Pi 要求 Node " + constraint
	}
	actualVer, ok := parseExactSemver(cleanActual)
	if !ok {
		return false, fmt.Sprintf("无法识别本机 Node.js 版本 %q（要求 %s）", actual, constraint)
	}
	branches := strings.Split(constraint, "||")
	for _, branch := range branches {
		if matchSemverAndClause(actualVer, branch) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("本机 Node.js v%s 不满足 Pi 要求（%s）", cleanActual, constraint)
}

func parseExactSemver(raw string) ([3]int, bool) {
	raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(raw, "v"), "V"))
	if raw == "" {
		return [3]int{}, false
	}
	if i := strings.IndexAny(raw, "+-"); i >= 0 {
		raw = raw[:i]
	}
	parts := strings.Split(raw, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, part := range parts {
		if part == "" || strings.EqualFold(part, "x") || part == "*" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func parsePartialSemver(raw string) ([3]int, int, bool) {
	raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(raw, "v"), "V"))
	if raw == "" || raw == "*" || strings.EqualFold(raw, "x") {
		return [3]int{}, 0, true
	}
	if i := strings.IndexAny(raw, "+-"); i >= 0 {
		raw = raw[:i]
	}
	parts := strings.Split(raw, ".")
	if len(parts) > 3 {
		return [3]int{}, 0, false
	}
	var out [3]int
	count := 0
	for i, part := range parts {
		if part == "" || strings.EqualFold(part, "x") || part == "*" {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return [3]int{}, 0, false
		}
		out[i] = n
		count++
	}
	return out, count, count > 0 || raw == "*" || strings.EqualFold(raw, "x")
}

func compareTuple(a, b [3]int) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func matchSemverAndClause(actual [3]int, clause string) bool {
	tokens := tokenizeSemverClause(clause)
	if len(tokens) == 0 {
		return false
	}
	for _, token := range tokens {
		if !matchSemverComparator(actual, token) {
			return false
		}
	}
	return true
}

func tokenizeSemverClause(clause string) []string {
	clause = strings.ReplaceAll(strings.TrimSpace(clause), ",", " ")
	fields := strings.Fields(clause)
	var out []string
	for i := 0; i < len(fields); i++ {
		tok := fields[i]
		if (tok == ">=" || tok == "<=" || tok == ">" || tok == "<" || tok == "=" || tok == "^" || tok == "~") && i+1 < len(fields) {
			out = append(out, tok+fields[i+1])
			i++
			continue
		}
		out = append(out, tok)
	}
	return out
}

func matchSemverComparator(actual [3]int, expr string) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" || expr == "*" || strings.EqualFold(expr, "x") {
		return true
	}
	op := ""
	for _, prefix := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(expr, prefix) {
			op = prefix
			expr = strings.TrimSpace(expr[len(prefix):])
			break
		}
	}
	ver, count, ok := parsePartialSemver(expr)
	if !ok {
		return false
	}
	if count == 0 {
		return true
	}
	switch op {
	case "", "=":
		switch count {
		case 1:
			return actual[0] == ver[0]
		case 2:
			return actual[0] == ver[0] && actual[1] == ver[1]
		default:
			return compareTuple(actual, ver) == 0
		}
	case ">=":
		return compareTuple(actual, ver) >= 0
	case ">":
		if count == 1 {
			return actual[0] > ver[0]
		}
		if count == 2 {
			return actual[0] > ver[0] || (actual[0] == ver[0] && actual[1] > ver[1])
		}
		return compareTuple(actual, ver) > 0
	case "<=":
		if count == 1 {
			return actual[0] <= ver[0]
		}
		if count == 2 {
			return actual[0] < ver[0] || (actual[0] == ver[0] && actual[1] <= ver[1])
		}
		return compareTuple(actual, ver) <= 0
	case "<":
		return compareTuple(actual, ver) < 0
	case "^":
		if compareTuple(actual, ver) < 0 {
			return false
		}
		var upper [3]int
		if ver[0] > 0 || count == 1 {
			upper = [3]int{ver[0] + 1, 0, 0}
		} else if ver[1] > 0 || count == 2 {
			upper = [3]int{0, ver[1] + 1, 0}
		} else {
			upper = [3]int{0, 0, ver[2] + 1}
		}
		return compareTuple(actual, upper) < 0
	case "~":
		if compareTuple(actual, ver) < 0 {
			return false
		}
		var upper [3]int
		if count == 1 {
			upper = [3]int{ver[0] + 1, 0, 0}
		} else {
			upper = [3]int{ver[0], ver[1] + 1, 0}
		}
		return compareTuple(actual, upper) < 0
	}
	return false
}

// VerifyPiContract runs an isolated, zero-LLM contract probe against piExecutable:
// verifies --version matches expectedVersion, starts pi --mode rpc with
// builtin:codemode, builtin:tool-search, and Vantry's embedded vantry_tools.ts +
// approval.ts extensions against an empty temp PI_CODING_AGENT_DIR, confirms
// vantry_approval_ready handshake, and tests get_available_models, get_state, and
// get_available_thinking_levels.
func VerifyPiContract(ctx context.Context, piExecutable, expectedVersion string) error {
	if strings.TrimSpace(piExecutable) == "" {
		return errors.New("缺少 Pi 可执行文件路径")
	}
	probeCtx, cancel := context.WithTimeout(ctx, contractProbeTimeout)
	defer cancel()

	reportedVer, err := Version(probeCtx, piExecutable)
	if err != nil {
		return fmt.Errorf("Pi 版本自检失败：%w", err)
	}
	if strings.TrimSpace(expectedVersion) != "" && CompareSemver(reportedVer, expectedVersion) != 0 {
		return fmt.Errorf("Pi 实际版本 (%s) 与预期 (%s) 不一致", reportedVer, expectedVersion)
	}

	probeRoot, err := os.MkdirTemp("", "vantry-pi-contract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probeRoot)

	agentDir := filepath.Join(probeRoot, "agent")
	jobDir := filepath.Join(probeRoot, "job")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return err
	}
	toolsPath := filepath.Join(jobDir, "vantry_tools.ts")
	approvalPath := filepath.Join(jobDir, "approval.ts")
	if err := os.WriteFile(toolsPath, vantryToolsExtension, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(approvalPath, approvalExtension, 0o600); err != nil {
		return err
	}

	cmd, err := newPiQueryCommand(
		probeCtx,
		piExecutable,
		"--mode", "rpc",
		"--no-session",
		"--no-extensions",
		"--no-mcp",
		"--extension", "builtin:codemode",
		"--extension", "builtin:tool-search",
		"--tools", "+codemode,+tool_search",
		"--extension", toolsPath,
		"--extension", approvalPath,
	)
	if err != nil {
		return err
	}
	defer cmd.Close()
	cmd.Dir = probeRoot
	cmd.Env = buildContractVerifyEnv(piExecutable, agentDir, jobDir)
	cmd.WaitDelay = 2 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法启动 Pi 契约沙箱验证：%w", err)
	}
	waitDone := false
	defer func() {
		_ = stdin.Close()
		if !waitDone {
			cancel()
			_ = cmd.Wait()
		}
	}()

	type lineMsg struct {
		line string
		err  error
		eof  bool
	}
	lines := make(chan lineMsg, 32)
	go func() {
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 64<<10), 4<<20)
		for scan.Scan() {
			lines <- lineMsg{line: scan.Text()}
		}
		if scanErr := scan.Err(); scanErr != nil {
			lines <- lineMsg{err: scanErr}
		} else {
			lines <- lineMsg{eof: true}
		}
		close(lines)
	}()

	readLine := func() (string, error) {
		for {
			select {
			case msg, ok := <-lines:
				if !ok || msg.eof {
					return "", io.EOF
				}
				if msg.err != nil {
					return "", msg.err
				}
				if strings.TrimSpace(msg.line) == "" {
					continue
				}
				return msg.line, nil
			case <-probeCtx.Done():
				return "", probeCtx.Err()
			}
		}
	}

	// Wait for approval.ts extension_ui_request notify "vantry_approval_ready".
	approvalReady := false
	for !approvalReady {
		rawLine, readErr := readLine()
		if readErr != nil {
			return fmt.Errorf("等待 approval.ts 就绪失败：%w（stderr=%s）", readErr, compactStderrTail(stderrBuf.String()))
		}
		var evt struct {
			Type    string `json:"type"`
			Method  string `json:"method"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(rawLine), &evt) == nil &&
			evt.Type == "extension_ui_request" &&
			evt.Method == "notify" &&
			evt.Message == "vantry_approval_ready" {
			approvalReady = true
		}
	}

	checks := []struct {
		id  string
		typ string
	}{
		{"c1", "get_available_models"},
		{"c2", "get_state"},
		{"c3", "get_available_thinking_levels"},
	}
	for _, chk := range checks {
		if err := json.NewEncoder(stdin).Encode(map[string]string{"id": chk.id, "type": chk.typ}); err != nil {
			return fmt.Errorf("发送 RPC %s 失败：%w", chk.typ, err)
		}
		got := false
		for !got {
			rawLine, readErr := readLine()
			if readErr != nil {
				return fmt.Errorf("读取 RPC %s 响应失败：%w（stderr=%s）", chk.typ, readErr, compactStderrTail(stderrBuf.String()))
			}
			var resp struct {
				ID      string `json:"id"`
				Type    string `json:"type"`
				Success bool   `json:"success"`
				Error   string `json:"error"`
			}
			if json.Unmarshal([]byte(rawLine), &resp) != nil {
				continue
			}
			if resp.Type == "response" && resp.ID == chk.id {
				if !resp.Success {
					return fmt.Errorf("Pi RPC %s 返回失败：%s", chk.typ, firstNonEmpty(resp.Error, "success=false"))
				}
				got = true
			}
		}
	}

	_ = stdin.Close()
	waitErr := cmd.Wait()
	waitDone = true
	if waitErr != nil && probeCtx.Err() == nil {
		return fmt.Errorf("Pi 契约验证退出异常：%w（stderr=%s）", waitErr, compactStderrTail(stderrBuf.String()))
	}
	if probeCtx.Err() != nil {
		return fmt.Errorf("Pi 契约验证超时：%w", probeCtx.Err())
	}
	return nil
}

func buildContractVerifyEnv(piExecutable, agentDir, jobDir string) []string {
	base := piEnvironment(piExecutable)
	filtered := make([]string, 0, len(base)+8)
	for _, entry := range base {
		k, _, _ := strings.Cut(entry, "=")
		if k == "PI_CODING_AGENT_DIR" || strings.HasPrefix(k, "VANTRY_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(
		filtered,
		"PI_CODING_AGENT_DIR="+agentDir,
		EnvApprovalPolicy+"="+ApprovalPolicyAsk,
		EnvInteractionMode+"="+InteractionModePlan,
		EnvJobDir+"="+jobDir,
		EnvDataDir+"="+jobDir,
		EnvRunID+"=preflight",
		EnvAttemptID+"=preflight",
		EnvThreadID+"=preflight",
	)
}

func compactStderrTail(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > 300 {
		trimmed = trimmed[len(trimmed)-300:]
	}
	return trimmed
}

func sandboxPiExecutable(prefixDir string) string {
	var candidates []string
	if runtime.GOOS == "windows" {
		candidates = append(candidates,
			filepath.Join(prefixDir, "node_modules", ".bin", "pi.cmd"),
			filepath.Join(prefixDir, "node_modules", ".bin", "pi.exe"),
		)
	} else {
		candidates = append(candidates,
			filepath.Join(prefixDir, "node_modules", ".bin", "pi"),
		)
	}
	candidates = append(candidates, filepath.Join(prefixDir, "node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "cli.js"))
	for _, cand := range candidates {
		resolved := cand
		if link, err := filepath.EvalSymlinks(cand); err == nil {
			resolved = link
		}
		if isExecutableFile(resolved) {
			return resolved
		}
		if isExecutableFile(cand) {
			return cand
		}
	}
	return ""
}

func preflightCachePath(dataDir string) string {
	return filepath.Join(dataDir, "pi-preflight-cache.json")
}

func loadPreflightCacheEntry(dataDir, cacheKey string) (preflightCacheEntry, bool) {
	if strings.TrimSpace(dataDir) == "" {
		return preflightCacheEntry{}, false
	}
	raw, err := os.ReadFile(preflightCachePath(dataDir))
	if err != nil {
		return preflightCacheEntry{}, false
	}
	var file preflightCacheFile
	if json.Unmarshal(raw, &file) != nil || len(file.Entries) == 0 {
		return preflightCacheEntry{}, false
	}
	entry, ok := file.Entries[cacheKey]
	if !ok || time.Since(entry.CheckedAt) > preflightCacheTTL {
		return preflightCacheEntry{}, false
	}
	return entry, true
}

func savePreflightCacheEntry(dataDir, cacheKey string, entry preflightCacheEntry) {
	if strings.TrimSpace(dataDir) == "" {
		return
	}
	_ = makePrivateDir(dataDir)
	file := preflightCacheFile{Entries: map[string]preflightCacheEntry{}}
	if raw, err := os.ReadFile(preflightCachePath(dataDir)); err == nil {
		var existing preflightCacheFile
		if json.Unmarshal(raw, &existing) == nil && existing.Entries != nil {
			file = existing
		}
	}
	now := time.Now().UTC()
	for k, v := range file.Entries {
		if now.Sub(v.CheckedAt) > preflightCacheTTL {
			delete(file.Entries, k)
		}
	}
	file.Entries[cacheKey] = entry
	_ = writeJSONFile(preflightCachePath(dataDir), file)
}

// PreflightExternalPi evaluates all 4 safety gates before Vantry surfaces or
// executes an external Pi upgrade/install:
//
//	Gate 1: SemVer window & blocklist policy
//	Gate 2: Local Node.js constraint, npm availability & npm-managed install check
//	Gate 3: Zero active Vantry task runs
//	Gate 4: Isolated temporary sandbox npm install + real Vantry RPC/extension contract probe
func PreflightExternalPi(
	ctx context.Context,
	dataDir string,
	activePiPath string,
	activeIsNPM bool,
	candidateVer string,
	requiredNode string,
	npmRegistry string,
	nodeVersion string,
	npmExe string,
	activeRuns int,
	deps upgradeDeps,
) (verified bool, reason string) {
	// Gate 1: SemVer policy
	if err := CheckPiVersionPolicy(candidateVer); err != nil {
		return false, err.Error()
	}

	// Gate 2: Local Node & npm eligibility
	if activePiPath != "" && !activeIsNPM {
		return false, "当前外置 Pi 非 npm 全局安装，无法通过 npm 自动升级"
	}
	if strings.TrimSpace(npmExe) == "" {
		return false, "未检测到 npm 命令，请先安装 Node.js / npm 后再操作"
	}
	if ok, msg := CheckNodeVersionSatisfies(nodeVersion, requiredNode); !ok {
		return false, msg
	}

	// Gate 3: Zero active runs
	if activeRuns > 0 {
		return false, fmt.Sprintf("当前有 %d 个任务正在运行，请等待结束后再升级 Pi", activeRuns)
	}

	// Gate 4: Isolated sandbox install + contract verification (with 24h cache)
	contractHash := vantryContractHash()
	cacheKey := candidateVer + "|" + nodeVersion + "|" + contractHash
	if cached, ok := loadPreflightCacheEntry(dataDir, cacheKey); ok {
		return cached.Verified, cached.Reason
	}

	sandboxRoot, err := os.MkdirTemp("", "vantry-pi-preflight-*")
	if err != nil {
		return false, fmt.Sprintf("创建兼容验证沙箱失败：%v", err)
	}
	defer os.RemoveAll(sandboxRoot)

	pkgSpec := PiNPMPackage + "@" + strings.TrimPrefix(strings.TrimSpace(candidateVer), "v")
	args := []string{"install", "--prefix", sandboxRoot, "--no-audit", "--no-fund", pkgSpec}
	if strings.TrimSpace(npmRegistry) != "" {
		args = append(args, "--registry="+npmRegistry)
	}
	env := buildNPMCommandEnv(firstNonEmpty(activePiPath, npmExe), sandboxRoot)

	installCtx, cancelInstall := context.WithTimeout(ctx, npmSandboxTimeout)
	defer cancelInstall()

	runner := deps.NPMRunner
	if runner == nil {
		runner = defaultNPMRunner
	}
	if out, runErr := runner(installCtx, npmExe, args, env); runErr != nil {
		msg := compactStderrTail(string(out))
		reason = fmt.Sprintf("沙箱预装 Pi v%s 失败：%v", candidateVer, runErr)
		if msg != "" {
			reason += " — " + msg
		}
		return false, reason
	}

	verifier := deps.ContractVerifier
	if verifier == nil {
		verifier = VerifyPiContract
	}

	candidateExe := sandboxPiExecutable(sandboxRoot)
	if candidateExe == "" && deps.ContractVerifier == nil {
		return false, fmt.Sprintf("沙箱预装 Pi v%s 后未找到可执行入口", candidateVer)
	}
	if verifyErr := verifier(ctx, candidateExe, candidateVer); verifyErr != nil {
		reason = fmt.Sprintf("兼容验证未通过（已拦截 Pi v%s 升级，不影响现有工作区）：%v", candidateVer, verifyErr)
		savePreflightCacheEntry(dataDir, cacheKey, preflightCacheEntry{
			CandidateVersion: candidateVer,
			NodeVersion:      nodeVersion,
			ContractHash:     contractHash,
			Verified:         false,
			Reason:           reason,
			CheckedAt:        time.Now().UTC(),
		})
		return false, reason
	}

	savePreflightCacheEntry(dataDir, cacheKey, preflightCacheEntry{
		CandidateVersion: candidateVer,
		NodeVersion:      nodeVersion,
		ContractHash:     contractHash,
		Verified:         true,
		CheckedAt:        time.Now().UTC(),
	})
	return true, ""
}

func buildNPMCommandEnv(toolHint, cacheRoot string) []string {
	env := piEnvironment(toolHint)
	if nodeDir := filepath.Dir(firstNonEmpty(findSiblingNode(toolHint), toolHint)); nodeDir != "" && nodeDir != "." {
		env = append(env, "PATH="+nodeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	filtered := make([]string, 0, len(env)+4)
	for _, e := range env {
		if strings.HasPrefix(e, "npm_config_cache=") {
			continue
		}
		filtered = append(filtered, e)
	}
	filtered = append(filtered,
		"NO_UPDATE_NOTIFIER=1",
		"npm_config_update_notifier=false",
	)
	if cacheRoot != "" {
		filtered = append(filtered, "npm_config_cache="+filepath.Join(cacheRoot, ".npm-cache"))
	}
	return filtered
}
