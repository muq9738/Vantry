package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/adapters/workbench"
	"github.com/muq9738/Vantry/pkg/api"
)

// baseService is an api.Service that cannot report capabilities, which is how a
// Core that predates the capability contract behaves.
type baseService struct {
	health   api.Health
	snapshot api.Snapshot
}

func (s *baseService) Health(context.Context) (api.Health, error) { return s.health, nil }

func (s *baseService) Dispatch(context.Context, api.Command) (api.Receipt, error) {
	return api.Receipt{}, errors.New("unexpected Dispatch call")
}

func (s *baseService) Snapshot(context.Context) (api.Snapshot, error) { return s.snapshot, nil }

func (s *baseService) Subscribe(context.Context, int64) (*api.Subscription, error) {
	return nil, errors.New("unexpected Subscribe call")
}

// statusService reports structured capabilities.
type statusService struct {
	*baseService
	status api.ServerStatus
}

func (s *statusService) ServerStatus(context.Context) (api.ServerStatus, error) {
	return s.status, nil
}

// drainRecorder is the idle-shutdown endpoint some Cores expose. It is embedded
// so a legacy Core can have the endpoint without gaining capability reporting.
type drainRecorder struct {
	calls atomic.Int32
	err   error
}

func (r *drainRecorder) RequestDrainShutdown(context.Context) error {
	r.calls.Add(1)
	return r.err
}

// adminService is a legacy Core that can still be asked to stop.
type adminService struct {
	*baseService
	*drainRecorder
}

// statusAdminService is a Core that reports capabilities and accepts the idle
// shutdown request.
type statusAdminService struct {
	*statusService
	*drainRecorder
}

// stoppableCore is a legacy Core that can close itself, used to verify the
// app-level status and idle-shutdown guards without a live socket.
type stoppableCore struct {
	*baseService
	closeCalls atomic.Int32
}

func (s *stoppableCore) Close() error {
	s.closeCalls.Add(1)
	return nil
}

func readyHealth() api.Health { return api.Health{Version: api.ProtocolVersion, State: "ready"} }

func compatibleStatus() api.ServerStatus {
	return api.ServerStatus{
		ProtocolVersion: api.ProtocolVersion,
		State:           "ready",
		SchemaVersion:   api.StatusSchemaVersion,
		PID:             4242,
		ActiveRuns:      0,
		Capabilities:    RequiredCoreCapabilities(),
	}
}

func startCapabilityServer(t *testing.T, service api.Service) (string, *local.Client) {
	t.Helper()
	// Keep the socket path below Darwin's sun_path limit.
	directory, err := os.MkdirTemp("/tmp", "vantry-handshake-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "core.sock")
	server, err := local.Listen(socket, service)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = server.Close(context.Background())
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve() did not stop")
		}
	})
	client := local.Dial(socket)
	t.Cleanup(func() { _ = client.Close() })
	return socket, client
}

func rotationOptions() HandshakeOptions {
	return HandshakeOptions{
		AllowRotate:  true,
		DrainTimeout: 2 * time.Second,
		PollInterval: 10 * time.Millisecond,
	}
}

func TestVerifyCoreCapabilitiesReusesMatchingCore(t *testing.T) {
	service := &statusService{baseService: &baseService{health: readyHealth()}, status: compatibleStatus()}
	_, client := startCapabilityServer(t, service)

	result, err := VerifyCoreCapabilities(context.Background(), client, RequiredCoreCapabilities())
	if err != nil {
		t.Fatalf("VerifyCoreCapabilities() error = %v", err)
	}
	if !result.Reused || result.Legacy || result.Rotated {
		t.Fatalf("unexpected handshake result: %+v", result)
	}
	if len(result.Missing) != 0 {
		t.Fatalf("missing capabilities = %v", result.Missing)
	}
	if !result.Status.HasCapability(api.CapabilityThreadSummaries) {
		t.Fatalf("status = %+v", result.Status)
	}
}

func TestVerifyCoreCapabilitiesRecognisesLegacyCore(t *testing.T) {
	service := &baseService{health: readyHealth()}
	_, client := startCapabilityServer(t, service)

	result, err := VerifyCoreCapabilities(context.Background(), client, RequiredCoreCapabilities())
	if !errors.Is(err, ErrLegacyCore) {
		t.Fatalf("error = %v, want ErrLegacyCore", err)
	}
	if !result.Legacy {
		t.Fatalf("legacy flag not set: %+v", result)
	}
	// A legacy Core is a reachable socket whose status endpoint is absent, which
	// the transport reports as a typed NotFound rather than a connection error.
	if !api.IsMissingCapabilityReport(err) {
		t.Fatalf("error does not carry the missing-endpoint signal: %v", err)
	}
	if !strings.Contains(err.Error(), "旧版 Core") {
		t.Fatalf("diagnosis is not readable: %v", err)
	}
}

func TestVerifyCoreCapabilitiesListsMissingCapabilities(t *testing.T) {
	status := compatibleStatus()
	status.ActiveRuns = 3
	status.Capabilities = []string{api.CapabilityStatus}
	service := &statusService{baseService: &baseService{health: readyHealth()}, status: status}
	_, client := startCapabilityServer(t, service)

	result, err := VerifyCoreCapabilities(context.Background(), client, RequiredCoreCapabilities())
	if !errors.Is(err, ErrCoreCapabilitiesMissing) {
		t.Fatalf("error = %v, want ErrCoreCapabilitiesMissing", err)
	}
	if len(result.Missing) != 2 {
		t.Fatalf("missing = %v", result.Missing)
	}
	for _, message := range []string{api.CapabilityThreadSummaries, api.CapabilityThreadDetail, "3", "4242"} {
		if !strings.Contains(err.Error(), message) {
			t.Fatalf("diagnosis %q does not mention %q", err.Error(), message)
		}
	}
}

func TestEnsureCompatibleCoreRotatesIdleIncompatibleCore(t *testing.T) {
	status := compatibleStatus()
	status.Capabilities = []string{api.CapabilityStatus, api.CapabilityDrainShutdown}
	service := &statusAdminService{
		statusService: &statusService{baseService: &baseService{health: readyHealth()}, status: status},
		drainRecorder: &drainRecorder{},
	}
	socket, client := startCapabilityServer(t, service)

	result, err := EnsureCompatibleCore(context.Background(), client, rotationOptions())
	if err != nil {
		t.Fatalf("EnsureCompatibleCore() error = %v", err)
	}
	if !result.Rotated || result.Reused {
		t.Fatalf("unexpected handshake result: %+v", result)
	}
	if got := service.drainRecorder.calls.Load(); got != 1 {
		t.Fatalf("drain requests = %d, want 1", got)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old socket still present after rotation: %v", err)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Health(probeCtx); err == nil {
		t.Fatal("retired Core still answers health")
	}
}

func TestEnsureCompatibleCoreRefusesBusyCore(t *testing.T) {
	status := compatibleStatus()
	status.ActiveRuns = 2
	status.Capabilities = []string{api.CapabilityStatus, api.CapabilityDrainShutdown}
	service := &statusAdminService{
		statusService: &statusService{baseService: &baseService{health: readyHealth()}, status: status},
		drainRecorder: &drainRecorder{},
	}
	_, client := startCapabilityServer(t, service)

	result, err := EnsureCompatibleCore(context.Background(), client, rotationOptions())
	if !errors.Is(err, ErrCoreBusy) {
		t.Fatalf("error = %v, want ErrCoreBusy", err)
	}
	if result.Rotated {
		t.Fatalf("busy Core was rotated: %+v", result)
	}
	if got := service.drainRecorder.calls.Load(); got != 0 {
		t.Fatalf("busy Core received %d shutdown requests, want 0", got)
	}
	for _, message := range []string{"2", "活动任务", api.CapabilityThreadSummaries} {
		if !strings.Contains(err.Error(), message) {
			t.Fatalf("diagnosis %q does not mention %q", err.Error(), message)
		}
	}
	// The Core must still be serving: refusing rotation never stops it.
	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Core stopped despite refusal: %v", err)
	}
}

func TestEnsureCompatibleCoreCannotRotateLegacyCoreWithoutAdminEndpoint(t *testing.T) {
	service := &baseService{health: readyHealth()}
	socket, client := startCapabilityServer(t, service)

	result, err := EnsureCompatibleCore(context.Background(), client, rotationOptions())
	if !errors.Is(err, ErrLegacyCore) {
		t.Fatalf("error = %v, want ErrLegacyCore", err)
	}
	if result.Rotated {
		t.Fatalf("legacy Core without an admin endpoint was rotated: %+v", result)
	}
	if !strings.Contains(err.Error(), "无法安全自动轮换") || !strings.Contains(err.Error(), "手动重启") {
		t.Fatalf("diagnosis is not actionable: %v", err)
	}
	if _, statErr := os.Lstat(socket); statErr != nil {
		t.Fatalf("legacy socket disappeared: %v", statErr)
	}
}

func TestEnsureCompatibleCoreRefusesLegacyCoreThatReportsBusy(t *testing.T) {
	service := &adminService{
		baseService:   &baseService{health: readyHealth()},
		drainRecorder: &drainRecorder{err: &api.Error{Code: api.Conflict, Message: "core 仍有活动任务"}},
	}
	_, client := startCapabilityServer(t, service)

	result, err := EnsureCompatibleCore(context.Background(), client, rotationOptions())
	if !errors.Is(err, ErrLegacyCore) {
		t.Fatalf("error = %v, want ErrLegacyCore", err)
	}
	if result.Rotated {
		t.Fatalf("legacy Core was rotated: %+v", result)
	}
	if got := service.drainRecorder.calls.Load(); got != 1 {
		t.Fatalf("drain requests = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "拒绝") || !strings.Contains(err.Error(), "手动重启") {
		t.Fatalf("diagnosis is not actionable: %v", err)
	}
}

func TestEnsureCompatibleCoreWithoutRotationExplainsHowToProceed(t *testing.T) {
	service := &baseService{health: readyHealth()}
	_, client := startCapabilityServer(t, service)

	_, err := EnsureCompatibleCore(context.Background(), client, HandshakeOptions{})
	if !errors.Is(err, ErrLegacyCore) {
		t.Fatalf("error = %v, want ErrLegacyCore", err)
	}
	if !strings.Contains(err.Error(), "手动重启") {
		t.Fatalf("diagnosis is not actionable: %v", err)
	}
}

func TestServerStatusListsWhatTheWrappedServiceCanServe(t *testing.T) {
	service := &stoppableCore{baseService: &baseService{
		health: readyHealth(),
		snapshot: api.Snapshot{Runs: []api.Run{
			{ID: "run-1", Status: api.RunRunning},
			{ID: "run-2", Status: api.RunCompleted},
		}},
	}}
	wrapped := &workbenchService{Service: service}

	status, err := wrapped.ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus() error = %v", err)
	}
	if status.ProtocolVersion != api.ProtocolVersion || status.State != "ready" {
		t.Fatalf("status = %+v", status)
	}
	if status.SchemaVersion != api.StatusSchemaVersion || status.PID != os.Getpid() {
		t.Fatalf("status identity = %+v", status)
	}
	if status.ActiveRuns != 1 {
		t.Fatalf("active runs = %d, want 1", status.ActiveRuns)
	}
	if missing := status.MissingCapabilities(RequiredCoreCapabilities()); len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if !status.HasCapability(api.CapabilityDrainShutdown) {
		t.Fatalf("closeable Core does not advertise idle shutdown: %+v", status)
	}
}

func TestServerStatusWithoutCloseableServiceOmitsShutdownCapability(t *testing.T) {
	wrapped := &workbenchService{Service: &statusService{
		baseService: &baseService{health: readyHealth()},
		status:      compatibleStatus(),
	}}
	status, err := wrapped.ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus() error = %v", err)
	}
	if status.HasCapability(api.CapabilityDrainShutdown) {
		t.Fatalf("non-closeable service advertises idle shutdown: %+v", status)
	}
}

func TestRequestDrainShutdownRefusesActiveRuns(t *testing.T) {
	service := &stoppableCore{baseService: &baseService{
		health:   readyHealth(),
		snapshot: api.Snapshot{Runs: []api.Run{{ID: "run-1", Status: api.RunRunning}}},
	}}
	wrapped := &workbenchService{Service: service}

	err := wrapped.RequestDrainShutdown(context.Background())
	if api.CodeOf(err) != api.Conflict {
		t.Fatalf("error = %v, want api.Conflict", err)
	}
	if !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "活动任务") {
		t.Fatalf("diagnosis is not readable: %v", err)
	}
	if got := service.closeCalls.Load(); got != 0 {
		t.Fatalf("busy Core was closed %d times", got)
	}
}

func TestRequestDrainShutdownClosesIdleCore(t *testing.T) {
	service := &stoppableCore{baseService: &baseService{health: readyHealth()}}
	wrapped := &workbenchService{Service: service}

	if err := wrapped.RequestDrainShutdown(context.Background()); err != nil {
		t.Fatalf("RequestDrainShutdown() error = %v", err)
	}
	if got := service.closeCalls.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
}

func TestWorkbenchWrappedCoreStillAdvertisesAndHonoursIdleShutdown(t *testing.T) {
	service := &stoppableCore{baseService: &baseService{health: readyHealth()}}
	// This is the production composition: RunCore wraps Core in the workbench
	// adapter before it is served over the socket.
	wrapped := &workbenchService{Service: workbench.New(service, nil, "")}

	status, err := wrapped.ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus() error = %v", err)
	}
	if !status.HasCapability(api.CapabilityDrainShutdown) {
		t.Fatalf("wrapped Core does not advertise idle shutdown: %+v", status)
	}
	if err := wrapped.RequestDrainShutdown(context.Background()); err != nil {
		t.Fatalf("RequestDrainShutdown() error = %v", err)
	}
	if got := service.closeCalls.Load(); got != 1 {
		t.Fatalf("underlying Core close calls = %d, want 1", got)
	}
}
