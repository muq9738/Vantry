package pi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

const latestCheckCacheTTL = 10 * time.Minute

var (
	latestMetaMu      sync.Mutex
	latestMetaCached  PiPackageMetadata
	latestMetaExpires time.Time
)

type upgradeDeps struct {
	Mirror           MirrorOptions
	Client           *http.Client
	NPMRunner        func(ctx context.Context, npmExe string, args []string, env []string) ([]byte, error)
	ContractVerifier func(ctx context.Context, piExecutable, expectedVersion string) error
	NodeVersionProbe func(ctx context.Context, piPath string) string
	NPMProbe         func(piPath string) string
	SystemProbe      func() (string, bool)
}

func defaultNPMRunner(runCtx context.Context, exe string, runArgs []string, runEnv []string) ([]byte, error) {
	cmd := exec.CommandContext(runCtx, exe, runArgs...)
	cmd.Env = runEnv
	return cmd.CombinedOutput()
}

// InspectRuntime returns full status of the active external Pi executable,
// local Node/npm toolchain, mirror routing, compatibility state, and ~/.pi/agent.
func InspectRuntime(ctx context.Context, requested, dataDir string, checkRemote bool) api.PiRuntimeStatus {
	return InspectRuntimeWithOptions(ctx, requested, dataDir, checkRemote, 0)
}

// InspectRuntimeWithOptions inspects external Pi runtime and evaluates the
// 4-gate compatibility status considering Core's live active run count.
func InspectRuntimeWithOptions(ctx context.Context, requested, dataDir string, checkRemote bool, activeRuns int) api.PiRuntimeStatus {
	return inspectRuntimeWith(ctx, requested, dataDir, checkRemote, activeRuns, upgradeDeps{})
}

func inspectRuntimeWith(ctx context.Context, requested, dataDir string, checkRemote bool, activeRuns int, deps upgradeDeps) api.PiRuntimeStatus {
	res := resolvePiRuntimeWith(runtimeResolveOptions{
		Requested:   requested,
		DataDir:     dataDir,
		SystemProbe: deps.SystemProbe,
	})

	npmProbe := deps.NPMProbe
	if npmProbe == nil {
		npmProbe = findSiblingNPM
	}
	nodeProbe := deps.NodeVersionProbe
	if nodeProbe == nil {
		nodeProbe = detectNodeVersion
	}

	npmExe := npmProbe(firstNonEmpty(res.Path, res.SystemPath))
	nodeVer := nodeProbe(ctx, firstNonEmpty(res.Path, res.SystemPath, npmExe))

	status := api.PiRuntimeStatus{
		ActiveSource:  res.ActiveSource,
		CustomPath:    res.CustomPath,
		SystemPath:    res.SystemPath,
		SystemVersion: res.SystemVersion,
		SystemIsNPM:   res.SystemIsNPM || (res.ActiveSource == RuntimeSourceCustom && res.Path == res.SystemPath && res.ActiveIsNPM),
		NodeVersion:   nodeVer,
		NPMAvailable:  npmExe != "",
		ActiveRuns:    activeRuns,
		MirrorMode:    res.Settings.MirrorMode,
		NPMRegistry:   res.Settings.NPMRegistry,
		AgentDir:      piAgentDirectory(),
		DefaultModel:  ConfiguredModel(),
	}
	if res.ActiveSource != RuntimeSourceMissing {
		status.ActivePath = res.Path
		status.ActiveVersion = res.ActiveVersion
	}

	probeCtx, cancelProbe := context.WithTimeout(ctx, 4*time.Second)
	if status.ActivePath != "" && deps.SystemProbe == nil {
		if ver, err := Version(probeCtx, status.ActivePath); err == nil && ver != "" {
			status.ActiveVersion = ver
		} else if err != nil && status.ActiveVersion == "" {
			status.Error = err.Error()
		}
	}
	if status.SystemPath != "" && status.SystemVersion == "" && deps.SystemProbe == nil {
		if ver, err := Version(probeCtx, status.SystemPath); err == nil {
			status.SystemVersion = ver
		}
	}
	cancelProbe()

	if status.ActiveSource == RuntimeSourceSystem && status.SystemVersion == "" {
		status.SystemVersion = status.ActiveVersion
	}

	if status.ActivePath != "" && deps.SystemProbe == nil {
		catCtx, cancelCat := context.WithTimeout(ctx, 6*time.Second)
		catalog, err := Catalog(catCtx, status.ActivePath, "", "")
		cancelCat()
		if err == nil {
			status.ModelCount = len(catalog.Models)
			if status.DefaultModel == "" {
				status.DefaultModel = firstNonEmpty(catalog.ConfiguredDefault, catalog.Default)
			}
			if len(catalog.Models) > 0 {
				status.Configured = true
			}
		}
	}
	if !status.Configured {
		status.Configured = HasPiConfiguration(status.AgentDir)
	}

	mirrorOpts := deps.Mirror
	if mirrorOpts.Mode == "" {
		mirrorOpts.Mode = res.Settings.MirrorMode
	}
	if mirrorOpts.NPMRegistry == "" {
		mirrorOpts.NPMRegistry = res.Settings.NPMRegistry
	}
	if mirrorOpts.HTTPClient == nil {
		mirrorOpts.HTTPClient = deps.Client
	}
	if checkRemote {
		mirrorOpts.BypassCache = true
	}

	route := DetectNetworkRoute(ctx, mirrorOpts)
	status.ResolvedRoute = route.Summary

	var meta PiPackageMetadata
	if checkRemote {
		fetched, err := FetchLatestPiMetadata(ctx, route, mirrorOpts)
		if err != nil {
			if status.Error == "" {
				status.Error = err.Error()
			}
		} else if fetched.Version != "" {
			meta = fetched
			if deps.Mirror.DirectNPM == "" && deps.Mirror.ChinaNPM == "" && deps.Client == nil {
				latestMetaMu.Lock()
				latestMetaCached = fetched
				latestMetaExpires = time.Now().Add(latestCheckCacheTTL)
				latestMetaMu.Unlock()
			}
		}
	} else if deps.Mirror.DirectNPM == "" && deps.Mirror.ChinaNPM == "" && deps.Client == nil {
		latestMetaMu.Lock()
		if latestMetaCached.Version != "" && time.Now().Before(latestMetaExpires) {
			meta = latestMetaCached
		}
		latestMetaMu.Unlock()
	}

	status.LatestVersion = meta.Version
	status.RequiredNode = meta.EnginesNode

	hasNewer := status.LatestVersion != "" && status.ActiveVersion != "" && CompareSemver(status.LatestVersion, status.ActiveVersion) > 0
	isMissingCandidate := status.LatestVersion != "" && status.ActiveSource == RuntimeSourceMissing

	if !hasNewer && !isMissingCandidate {
		return status
	}

	// Evaluate the 4 zero-impact safety gates before surfacing any upgrade/install action.
	eligibleForNPM, preReason := checkNPMUpgradeEligibility(res, status.SystemPath, status.SystemIsNPM)
	if !eligibleForNPM {
		status.CompatBlocked = true
		status.CompatReason = preReason
		return status
	}

	if checkRemote {
		verified, reason := PreflightExternalPi(
			ctx,
			dataDir,
			status.ActivePath,
			true,
			status.LatestVersion,
			status.RequiredNode,
			route.NPMRegistry,
			nodeVer,
			npmExe,
			activeRuns,
			deps,
		)
		applyGateOutcome(&status, verified, reason, isMissingCandidate)
		return status
	}

	// Non-check status call: evaluate cheap synchronous Gates 1..3 plus cached Gate 4.
	if err := CheckPiVersionPolicy(status.LatestVersion); err != nil {
		applyGateOutcome(&status, false, err.Error(), isMissingCandidate)
		return status
	}
	if npmExe == "" {
		applyGateOutcome(&status, false, "未检测到 npm 命令，请先安装 Node.js / npm 后再操作", isMissingCandidate)
		return status
	}
	if ok, msg := CheckNodeVersionSatisfies(nodeVer, status.RequiredNode); !ok {
		applyGateOutcome(&status, false, msg, isMissingCandidate)
		return status
	}
	if activeRuns > 0 {
		applyGateOutcome(&status, false, fmt.Sprintf("当前有 %d 个任务正在运行，请等待结束后再升级 Pi", activeRuns), isMissingCandidate)
		return status
	}
	cacheKey := status.LatestVersion + "|" + nodeVer + "|" + vantryContractHash()
	if cached, ok := loadPreflightCacheEntry(dataDir, cacheKey); ok {
		applyGateOutcome(&status, cached.Verified, cached.Reason, isMissingCandidate)
	}
	return status
}

func checkNPMUpgradeEligibility(res PiRuntimeResolution, systemPath string, activeOrSystemNPM bool) (bool, string) {
	if res.ActiveSource == RuntimeSourceMissing {
		return true, ""
	}
	if res.ActiveSource == RuntimeSourceCustom {
		if systemPath == "" || res.Path != systemPath || !res.SystemIsNPM {
			return false, "当前使用自定义路径 Pi，全局 npm 升级不会替换该可执行文件"
		}
		return true, ""
	}
	if !activeOrSystemNPM {
		return false, "当前外置 Pi 非 npm 全局安装，请手动更新或改用 npm install -g @earendil-works/pi-coding-agent"
	}
	return true, ""
}

func applyGateOutcome(status *api.PiRuntimeStatus, verified bool, reason string, isMissingCandidate bool) {
	if verified {
		status.CompatVerified = true
		status.CompatBlocked = false
		status.CompatReason = ""
		if isMissingCandidate {
			status.CanInstall = true
			status.UpdateAvailable = false
		} else {
			status.UpdateAvailable = true
			status.CanUpgrade = true
		}
		return
	}
	status.CompatVerified = false
	status.UpdateAvailable = false
	status.CanUpgrade = false
	status.CanInstall = false
	if strings.TrimSpace(reason) != "" {
		status.CompatBlocked = true
		status.CompatReason = reason
	}
}

// UpgradePi upgrades (or installs if missing) the external npm-managed Pi
// runtime after all 4 safety gates pass, with live verification and rollback.
func UpgradePi(ctx context.Context, requested, dataDir string, input api.PiUpgradeInput) (api.PiRuntimeStatus, string, error) {
	return UpgradePiWithOptions(ctx, requested, dataDir, input, 0)
}

// UpgradePiWithOptions performs a guarded external npm upgrade/install respecting activeRuns.
func UpgradePiWithOptions(ctx context.Context, requested, dataDir string, input api.PiUpgradeInput, activeRuns int) (api.PiRuntimeStatus, string, error) {
	return upgradePiWith(ctx, requested, dataDir, input, activeRuns, upgradeDeps{})
}

func upgradePiWith(ctx context.Context, requested, dataDir string, input api.PiUpgradeInput, activeRuns int, deps upgradeDeps) (api.PiRuntimeStatus, string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return api.PiRuntimeStatus{}, "", errors.New("需要有效的数据目录以保存 Pi 升级状态")
	}
	if activeRuns > 0 {
		status := inspectRuntimeWith(ctx, requested, dataDir, false, activeRuns, deps)
		return status, "", fmt.Errorf("当前有 %d 个任务正在运行，请等待结束后再升级 Pi", activeRuns)
	}

	settings := LoadRuntimeSettings(dataDir)
	mirrorOpts := deps.Mirror
	if mirrorOpts.Mode == "" {
		mirrorOpts.Mode = settings.MirrorMode
	}
	if mirrorOpts.NPMRegistry == "" {
		mirrorOpts.NPMRegistry = settings.NPMRegistry
	}
	if mirrorOpts.HTTPClient == nil {
		mirrorOpts.HTTPClient = deps.Client
	}
	route := DetectNetworkRoute(ctx, mirrorOpts)

	var meta PiPackageMetadata
	targetVer := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(input.Version), "v"), "V")
	if targetVer == "" {
		fetched, err := FetchLatestPiMetadata(ctx, route, mirrorOpts)
		if err != nil {
			return inspectRuntimeWith(ctx, requested, dataDir, false, activeRuns, deps), "", err
		}
		meta = fetched
		targetVer = fetched.Version
	}

	res := resolvePiRuntimeWith(runtimeResolveOptions{
		Requested:   requested,
		DataDir:     dataDir,
		SystemProbe: deps.SystemProbe,
	})
	if res.ActiveSource == RuntimeSourceMissing && !input.Install {
		return inspectRuntimeWith(ctx, requested, dataDir, false, activeRuns, deps), "", errors.New("未检测到外置 Pi；请点击「安装外置 Pi」通过 npm 安装")
	}
	eligibleForNPM, preReason := checkNPMUpgradeEligibility(res, res.SystemPath, res.SystemIsNPM)
	if !eligibleForNPM {
		return inspectRuntimeWith(ctx, requested, dataDir, false, activeRuns, deps), "", errors.New(preReason)
	}

	previousVersion := res.ActiveVersion
	if res.ActiveSource != RuntimeSourceMissing && previousVersion == "" && deps.SystemProbe == nil {
		probeCtx, cancelProbe := context.WithTimeout(ctx, 4*time.Second)
		previousVersion, _ = Version(probeCtx, res.Path)
		cancelProbe()
	}
	if res.ActiveSource != RuntimeSourceMissing && strings.TrimSpace(input.Version) == "" && previousVersion != "" && targetVer != "" && CompareSemver(targetVer, previousVersion) <= 0 {
		current := inspectRuntimeWith(ctx, requested, dataDir, false, activeRuns, deps)
		current.LatestVersion = targetVer
		current.RequiredNode = meta.EnginesNode
		current.UpdateAvailable = false
		current.CanUpgrade = false
		current.CanInstall = false
		return current, fmt.Sprintf("外置 Pi 已是最新版本 v%s，无需升级", previousVersion), nil
	}

	npmProbe := deps.NPMProbe
	if npmProbe == nil {
		npmProbe = findSiblingNPM
	}
	nodeProbe := deps.NodeVersionProbe
	if nodeProbe == nil {
		nodeProbe = detectNodeVersion
	}
	npmExe := npmProbe(firstNonEmpty(res.Path, res.SystemPath))
	nodeVer := nodeProbe(ctx, firstNonEmpty(res.Path, res.SystemPath, npmExe))

	verified, reason := PreflightExternalPi(
		ctx,
		dataDir,
		res.PathIfInstalled(),
		true,
		targetVer,
		meta.EnginesNode,
		route.NPMRegistry,
		nodeVer,
		npmExe,
		activeRuns,
		deps,
	)
	if !verified {
		status := inspectRuntimeWith(ctx, requested, dataDir, false, activeRuns, deps)
		status.LatestVersion = targetVer
		status.RequiredNode = meta.EnginesNode
		applyGateOutcome(&status, false, reason, res.ActiveSource == RuntimeSourceMissing)
		return status, "", errors.New(reason)
	}

	if err := runGlobalNPMInstall(ctx, npmExe, res.PathIfInstalled(), targetVer, route.NPMRegistry, deps); err != nil {
		return inspectRuntimeWith(ctx, requested, dataDir, false, activeRuns, deps), "", err
	}

	// Post-upgrade live contract check on the newly installed external Pi.
	afterRes := resolvePiRuntimeWith(runtimeResolveOptions{
		Requested:   requested,
		DataDir:     dataDir,
		SystemProbe: deps.SystemProbe,
	})
	verifier := deps.ContractVerifier
	if verifier == nil {
		verifier = VerifyPiContract
	}
	if err := verifier(ctx, afterRes.PathIfInstalled(), targetVer); err != nil {
		cacheKey := targetVer + "|" + nodeVer + "|" + vantryContractHash()
		failReason := fmt.Sprintf("升级后实时契约校验失败：%v", err)
		savePreflightCacheEntry(dataDir, cacheKey, preflightCacheEntry{
			CandidateVersion: targetVer,
			NodeVersion:      nodeVer,
			ContractHash:     vantryContractHash(),
			Verified:         false,
			Reason:           failReason,
			CheckedAt:        time.Now().UTC(),
		})
		rollbackNote := "未发现旧版本可供回滚"
		if previousVersion != "" {
			if rbErr := runGlobalNPMInstall(ctx, npmExe, res.PathIfInstalled(), previousVersion, route.NPMRegistry, deps); rbErr != nil {
				rollbackNote = fmt.Sprintf("自动回滚到 v%s 失败：%v", previousVersion, rbErr)
			} else {
				rollbackNote = fmt.Sprintf("已自动回滚到 v%s，现有工作区不受影响", previousVersion)
			}
		}
		status := inspectRuntimeWith(ctx, requested, dataDir, false, activeRuns, deps)
		status.LatestVersion = targetVer
		status.RequiredNode = meta.EnginesNode
		applyGateOutcome(&status, false, failReason+"（"+rollbackNote+"）", res.ActiveSource == RuntimeSourceMissing)
		return status, "", fmt.Errorf("升级后的外置 Pi 未通过 Vantry 兼容检查（%v）；%s", err, rollbackNote)
	}

	if deps.Mirror.DirectNPM == "" && deps.Mirror.ChinaNPM == "" && deps.Client == nil {
		latestMetaMu.Lock()
		latestMetaCached = PiPackageMetadata{
			Version:     targetVer,
			EnginesNode: meta.EnginesNode,
			Registry:    route.NPMRegistry,
		}
		latestMetaExpires = time.Now().Add(latestCheckCacheTTL)
		latestMetaMu.Unlock()
	}

	updated := inspectRuntimeWith(ctx, requested, dataDir, false, activeRuns, deps)
	updated.LatestVersion = targetVer
	updated.RequiredNode = meta.EnginesNode
	updated.UpdateAvailable = false
	updated.CanUpgrade = false
	updated.CanInstall = false
	updated.CompatVerified = true

	verb := "升级"
	if input.Install || res.ActiveSource == RuntimeSourceMissing {
		verb = "安装"
	}
	regHost := firstNonEmpty(hostFromURL(route.NPMRegistry), route.NPMRegistry)
	summary := fmt.Sprintf("已通过 %s 将外置 Pi %s到 v%s，并完成 Vantry 兼容验证", regHost, verb, targetVer)
	return updated, summary, nil
}

func (r PiRuntimeResolution) PathIfInstalled() string {
	if r.ActiveSource == RuntimeSourceMissing {
		return ""
	}
	return r.Path
}

func runGlobalNPMInstall(ctx context.Context, npmExe, hintPiPath, targetVersion, npmRegistry string, deps upgradeDeps) error {
	if strings.TrimSpace(npmExe) == "" {
		return errors.New("未找到 npm 命令，无法安装或升级外置 Pi")
	}
	pkgSpec := PiNPMPackage
	if cleanVer := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(targetVersion), "v"), "V"); cleanVer != "" {
		pkgSpec += "@" + cleanVer
	}
	args := []string{"install", "-g", pkgSpec}
	if strings.TrimSpace(npmRegistry) != "" {
		args = append(args, "--registry="+npmRegistry)
	}
	env := buildNPMCommandEnv(firstNonEmpty(hintPiPath, npmExe), "")
	runner := deps.NPMRunner
	if runner == nil {
		runner = defaultNPMRunner
	}
	runCtx, cancel := context.WithTimeout(ctx, npmSandboxTimeout)
	defer cancel()
	out, err := runner(runCtx, npmExe, args, env)
	if err != nil {
		msg := compactStderrTail(string(out))
		if msg != "" {
			return fmt.Errorf("npm 安装外置 Pi 失败：%v — %s", err, msg)
		}
		return fmt.Errorf("npm 安装外置 Pi 失败：%w", err)
	}
	return nil
}
