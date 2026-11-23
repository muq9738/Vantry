package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workbench"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	// statusProbeTimeout bounds one capability probe so a wedged socket cannot
	// hold client startup.
	statusProbeTimeout = 2 * time.Second
	// defaultDrainTimeout bounds the wait for a retired Core's socket.
	defaultDrainTimeout = 5 * time.Second
	// defaultDrainPollInterval is how often rotation re-probes the old Core.
	defaultDrainPollInterval = 20 * time.Millisecond
)

var (
	// ErrLegacyCore marks a Core that cannot report capabilities at all. It is a
	// different condition from an unreachable Core: the socket answers, but the
	// build predates the capability contract.
	ErrLegacyCore = errors.New("已连接的 Core 不支持能力报告")
	// ErrCoreCapabilitiesMissing marks a reachable Core that reports capabilities
	// but lacks at least one the client needs.
	ErrCoreCapabilitiesMissing = errors.New("已连接的 Core 缺少必需能力")
	// ErrCoreBusy marks a Core that must not be stopped because it still has
	// active runs, or because it could not prove that it is idle.
	ErrCoreBusy = errors.New("Core 仍有活动任务")
	// ErrCoreRotationTimeout marks a Core that accepted the drain request but did
	// not release its socket in time.
	ErrCoreRotationTimeout = errors.New("旧 Core 未能及时释放 socket")
)

// CapabilityClient is the transport subset the handshake and the idle rotation
// need. It lets the logic be verified against a real socket without depending on
// the concrete client type.
type CapabilityClient interface {
	api.StatusReporter
	api.AdminController
	Health(context.Context) (api.Health, error)
}

// RequiredCoreCapabilities are the capabilities this client build depends on.
// They are business features, so a Core either serves them or does not.
func RequiredCoreCapabilities() []string {
	return []string{
		api.CapabilityStatus,
		api.CapabilityThreadSummaries,
		api.CapabilityThreadDetail,
	}
}

// RequiredRemoteDesktopCapabilities names the interfaces a remote Core must
// support when attached as a peer workbench. Local-only controls such as
// external Pi runtime management (CapabilityPiRuntime) are excluded so remote
// peers running slightly older builds can connect seamlessly.
func RequiredRemoteDesktopCapabilities() []string {
	return append(RequiredCoreCapabilities(), api.CapabilityInterThreadOrchestration,
		api.CapabilityWorkbenchV3, api.CapabilityNativePluginPages, api.CapabilityRemoteAccess, api.CapabilityNativeConnectivity, api.CapabilityDirectDiscovery, api.CapabilityEnvironmentSessions, api.CapabilityEnvironmentOrchestration, api.CapabilitySkills, api.CapabilityPiScriptedTools, api.CapabilityRemoteDesktopPermissions)
}

// RequiredDesktopCapabilities names the interfaces consumed by this local desktop.
func RequiredDesktopCapabilities() []string {
	return append(RequiredRemoteDesktopCapabilities(), api.CapabilityPiRuntime)
}

// HandshakeResult records what verification observed and what rotation did.
type HandshakeResult struct {
	Status     api.ServerStatus
	Missing    []string
	ActiveRuns int
	Legacy     bool
	Reused     bool
	Rotated    bool
}

// HandshakeOptions bounds capability verification and idle rotation.
type HandshakeOptions struct {
	// RequiredCaps defaults to RequiredCoreCapabilities.
	RequiredCaps []string
	// AllowRotate permits retiring an idle incompatible Core. It never permits
	// stopping a Core that is running tasks.
	AllowRotate bool
	// DrainTimeout bounds the wait for the old socket; default 5s.
	DrainTimeout time.Duration
	// PollInterval is the re-probe interval while waiting; default 20ms.
	PollInterval time.Duration
}

func (o HandshakeOptions) requiredCapabilities() []string {
	if len(o.RequiredCaps) > 0 {
		return o.RequiredCaps
	}
	return RequiredCoreCapabilities()
}

func (o HandshakeOptions) drainTimeout() time.Duration {
	if o.DrainTimeout > 0 {
		return o.DrainTimeout
	}
	return defaultDrainTimeout
}

func (o HandshakeOptions) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return defaultDrainPollInterval
}

// VerifyCoreCapabilities asks the connected Core what it can do and reports every
// required capability it lacks. A Core without a status endpoint is reported as
// ErrLegacyCore: attaching to it would silently run a client against a build that
// cannot serve the features it depends on.
func VerifyCoreCapabilities(ctx context.Context, client CapabilityClient, requiredCaps []string) (HandshakeResult, error) {
	if client == nil {
		return HandshakeResult{}, errors.New("能力校验需要一个可用的 Core 客户端")
	}
	probeCtx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
	defer cancel()
	status, err := client.ServerStatus(probeCtx)
	if err != nil {
		if api.IsMissingCapabilityReport(err) {
			return HandshakeResult{Legacy: true}, fmt.Errorf("%w：%w", ErrLegacyCore, err)
		}
		return HandshakeResult{}, fmt.Errorf("读取 Core 能力状态失败：%w", err)
	}
	result := HandshakeResult{
		Status:     status,
		ActiveRuns: status.ActiveRuns,
		Missing:    status.MissingCapabilities(requiredCaps),
	}
	if status.ProtocolVersion != api.ProtocolVersion {
		return result, &api.Error{
			Code:    api.VersionMismatch,
			Message: fmt.Sprintf("Core 协议版本 %d 与客户端 %d 不一致", status.ProtocolVersion, api.ProtocolVersion),
		}
	}
	if len(result.Missing) > 0 {
		return result, fmt.Errorf("%w：%s", ErrCoreCapabilitiesMissing, missingDiagnosis(result))
	}
	result.Reused = true
	return result, nil
}

// RotateIdleCore verifies the connected Core and, when it does not satisfy the
// required capabilities, retires it. Rotation happens only with proof that the
// Core is idle:
//   - a status report with ActiveRuns > 0 is refused without touching the Core,
//     so a version change never interrupts running work;
//   - a legacy Core cannot prove idleness, so its own idle-shutdown decision is
//     the only authority; if it has no such endpoint, rotation reports the fact
//     instead of killing the process.
func RotateIdleCore(ctx context.Context, client CapabilityClient, opts HandshakeOptions) (HandshakeResult, error) {
	result, verifyErr := VerifyCoreCapabilities(ctx, client, opts.requiredCapabilities())
	if verifyErr == nil {
		result.Reused = true
		return result, nil
	}
	if result.Legacy {
		return rotateLegacyCore(ctx, client, opts, verifyErr)
	}
	if result.ActiveRuns > 0 {
		return result, fmt.Errorf("%w：%s", ErrCoreBusy, busyDiagnosis(result, nil))
	}
	if result.Status.State == "draining" || result.Status.State == "stopped" {
		// The Core already stopped accepting work; only its socket is left.
		if err := waitForCoreRelease(ctx, client, opts.drainTimeout(), opts.pollInterval()); err != nil {
			return result, err
		}
		result.Rotated = true
		return result, nil
	}
	if !result.Status.HasCapability(api.CapabilityDrainShutdown) {
		return result, fmt.Errorf("%w：%s；该 Core 没有空闲停机接口，无法自动轮换，请手动重启本地 Core",
			ErrCoreCapabilitiesMissing, missingDiagnosis(result))
	}
	return drainAndWait(ctx, client, opts, result)
}

// EnsureCompatibleCore is the client-startup entry point: reuse a matching Core,
// retire an idle incompatible one, or fail with a readable diagnosis instead of
// attaching to a Core that cannot serve this build. A startup path that only
// compares ProtocolVersion and the ready state attaches to an upgraded client's
// old Core; this handshake is what replaces that check.
func EnsureCompatibleCore(ctx context.Context, client CapabilityClient, opts HandshakeOptions) (HandshakeResult, error) {
	result, err := VerifyCoreCapabilities(ctx, client, opts.requiredCapabilities())
	if err == nil {
		result.Reused = true
		return result, nil
	}
	if !opts.AllowRotate {
		return result, fmt.Errorf("%w；请等待活动任务结束后重试，或手动重启本地 Core 以启用新能力", err)
	}
	return RotateIdleCore(ctx, client, opts)
}

func rotateLegacyCore(ctx context.Context, client CapabilityClient, opts HandshakeOptions, cause error) (HandshakeResult, error) {
	result := HandshakeResult{Legacy: true}
	err := client.RequestDrainShutdown(ctx)
	switch {
	case err == nil:
	case api.CodeOf(err) == api.Conflict:
		// The legacy Core refused on its own authority. Never escalate to a kill.
		return result, fmt.Errorf("%w：旧版 Core 没有能力报告，无法确认它是否空闲；它的空闲停机接口拒绝了请求，请先结束其任务并手动重启本地 Core：%w", ErrLegacyCore, err)
	case api.CodeOf(err) == api.NotFound:
		return result, fmt.Errorf("%w；旧版 Core 也没有空闲停机接口，无法安全自动轮换：请结束其任务后手动重启本地 Core", cause)
	default:
		return result, fmt.Errorf("请求旧版 Core 空闲停机失败：%w", err)
	}
	if err := waitForCoreRelease(ctx, client, opts.drainTimeout(), opts.pollInterval()); err != nil {
		return result, err
	}
	result.Rotated = true
	return result, nil
}

func drainAndWait(ctx context.Context, client CapabilityClient, opts HandshakeOptions, result HandshakeResult) (HandshakeResult, error) {
	if err := client.RequestDrainShutdown(ctx); err != nil {
		if api.CodeOf(err) == api.Conflict {
			return result, fmt.Errorf("%w：%s", ErrCoreBusy, busyDiagnosis(result, err))
		}
		return result, fmt.Errorf("请求 Core 空闲停机失败：%w", err)
	}
	if err := waitForCoreRelease(ctx, client, opts.drainTimeout(), opts.pollInterval()); err != nil {
		return result, err
	}
	result.Rotated = true
	return result, nil
}

// waitForCoreRelease waits until the retired Core stops answering on its socket,
// which is when a matching Core can listen on the same path.
func waitForCoreRelease(ctx context.Context, client CapabilityClient, timeout, interval time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("%w（等待 %s）", ErrCoreRotationTimeout, timeout)
		case <-timer.C:
		}
		probeCtx, stop := context.WithTimeout(waitCtx, statusProbeTimeout)
		_, err := client.Health(probeCtx)
		stop()
		if err != nil {
			return nil
		}
		timer.Reset(interval)
	}
}

func missingDiagnosis(result HandshakeResult) string {
	missing := "无"
	if len(result.Missing) > 0 {
		missing = strings.Join(result.Missing, "、")
	}
	return fmt.Sprintf("缺少能力：%s；当前活动任务 %d 个（pid=%d，状态 %s）",
		missing, result.ActiveRuns, result.Status.PID, result.Status.State)
}

func busyDiagnosis(result HandshakeResult, cause error) string {
	diagnosis := fmt.Sprintf("%s。为避免中断正在执行的任务，客户端不会强制停止该 Core；请等待任务结束后重试，或确认其空闲后手动重启本地 Core",
		missingDiagnosis(result))
	if cause != nil {
		diagnosis += "：" + cause.Error()
	}
	return diagnosis
}

// ServerStatus reports the Core's structured business capabilities. The wrapped
// service may report them directly; otherwise they are derived from what it can
// actually serve. ActiveRuns is read only while the Core is ready to accept work.
func (s *workbenchService) ServerStatus(ctx context.Context) (api.ServerStatus, error) {
	if reporter, ok := unwrapService[api.StatusReporter](s.Service); ok {
		status, err := reporter.ServerStatus(ctx)
		if err != nil {
			return api.ServerStatus{}, err
		}
		if !status.HasCapability(api.CapabilityRemoteDesktopPermissions) {
			status.Capabilities = append(status.Capabilities, api.CapabilityRemoteDesktopPermissions)
		}
		if !status.HasCapability(api.CapabilityPiScriptedTools) {
			status.Capabilities = append(status.Capabilities, api.CapabilityPiScriptedTools)
		}
		if !status.HasCapability(api.CapabilityPiRuntime) {
			status.Capabilities = append(status.Capabilities, api.CapabilityPiRuntime)
		}
		if s.federator != nil && !status.HasCapability(api.CapabilityEnvironmentOrchestration) {
			status.Capabilities = append(status.Capabilities, api.CapabilityEnvironmentOrchestration)
		}
		if !status.HasCapability(api.CapabilitySkills) {
			status.Capabilities = append(status.Capabilities, api.CapabilitySkills)
		}
		if !status.HasCapability(api.CapabilityEnvironmentSessions) {
			status.Capabilities = append(status.Capabilities, api.CapabilityEnvironmentSessions)
		}
		if !status.HasCapability(api.CapabilityDirectDiscovery) {
			status.Capabilities = append(status.Capabilities, api.CapabilityDirectDiscovery)
		}
		if !status.HasCapability(api.CapabilityRemoteAccess) {
			status.Capabilities = append(status.Capabilities, api.CapabilityRemoteAccess)
		}
		for _, capability := range []string{api.CapabilityNativeConnectivity, api.CapabilityT3Sessions} {
			if !status.HasCapability(capability) {
				status.Capabilities = append(status.Capabilities, capability)
			}
		}
		if !status.HasCapability(api.CapabilityNativePluginPages) {
			status.Capabilities = append(status.Capabilities, api.CapabilityNativePluginPages)
		}
		if _, closeable := closeableService(s.Service); closeable && !status.HasCapability(api.CapabilityDrainShutdown) {
			status.Capabilities = append(status.Capabilities, api.CapabilityDrainShutdown)
		}
		return status, nil
	}
	health, err := s.Service.Health(ctx)
	if err != nil {
		return api.ServerStatus{}, err
	}
	status := api.ServerStatus{
		ProtocolVersion: health.Version,
		State:           health.State,
		SchemaVersion:   api.StatusSchemaVersion,
		PID:             os.Getpid(),
		Capabilities:    append(CoreCapabilities(s.Service), api.CapabilityWorkbenchV3, api.CapabilityNativePluginPages, api.CapabilityRemoteAccess, api.CapabilityNativeConnectivity, api.CapabilityT3Sessions, api.CapabilityDirectDiscovery, api.CapabilityEnvironmentSessions, api.CapabilitySkills, api.CapabilityPiScriptedTools, api.CapabilityPiRuntime, api.CapabilityRemoteDesktopPermissions),
	}
	if health.State != "ready" {
		return status, nil
	}
	snapshot, err := s.Service.Snapshot(ctx)
	if err != nil {
		return api.ServerStatus{}, err
	}
	status.ActiveRuns = api.ActiveRunCount(snapshot)
	return status, nil
}

// CoreCapabilities lists what a service wrapped by this app can serve. The
// lightweight thread projections come from the local transport's default
// projection, and idle shutdown only exists when the service can close.
func CoreCapabilities(service api.Service) []string {
	capabilities := []string{
		api.CapabilityStatus,
		api.CapabilityThreadSummaries,
		api.CapabilityThreadDetail,
	}
	if _, ok := unwrapService[api.OrchestrationBroker](service); ok {
		capabilities = append(capabilities, api.CapabilityInterThreadOrchestration)
	}
	if _, ok := closeableService(service); ok {
		capabilities = append(capabilities, api.CapabilityDrainShutdown)
	}
	return capabilities
}

// closeableService finds the layer of the service chain that owns the process
// lifetime. The workbench adapter wraps Core to add workspace queries and does
// not forward Close, so the app unwraps that known layer instead of advertising
// a shutdown it cannot honour.
func closeableService(service api.Service) (interface{ Close() error }, bool) {
	if closer, ok := service.(interface{ Close() error }); ok {
		return closer, true
	}
	if wrapped, ok := service.(*workbench.Service); ok {
		return closeableService(wrapped.Service)
	}
	return nil, false
}

// RequestDrainShutdown retires an idle Core so a matching one can take its
// socket. It refuses while any run is active: an upgrade must never cost work.
func (s *workbenchService) RequestDrainShutdown(ctx context.Context) error {
	health, err := s.Service.Health(ctx)
	if err != nil {
		return err
	}
	switch health.State {
	case "draining", "stopped":
		return nil
	case "ready":
	default:
		return &api.Error{Code: api.NotReady, Message: "Core 当前状态为 " + health.State + "，无法执行空闲停机"}
	}
	snapshot, err := s.Service.Snapshot(ctx)
	if err != nil {
		return err
	}
	if active := api.ActiveRunCount(snapshot); active > 0 {
		return &api.Error{Code: api.Conflict, Message: fmt.Sprintf(
			"Core 仍有 %d 个活动任务（pid=%d），拒绝空闲停机；请等待任务结束后重试", active, os.Getpid())}
	}
	closer, ok := closeableService(s.Service)
	if !ok {
		return &api.Error{Code: api.NotReady, Message: "当前 Core 不支持空闲停机"}
	}
	return closer.Close()
}
