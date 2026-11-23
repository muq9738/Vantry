package app

import (
	"context"
	"errors"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestDesktopDoesNotReuseBusyCoreWithoutScriptedTools(t *testing.T) {
	status := compatibleStatus()
	status.Capabilities = nil
	for _, capability := range RequiredDesktopCapabilities() {
		if capability != api.CapabilityPiScriptedTools {
			status.Capabilities = append(status.Capabilities, capability)
		}
	}
	status.ActiveRuns = 1
	service := &statusAdminService{
		statusService: &statusService{baseService: &baseService{health: readyHealth()}, status: status},
		drainRecorder: &drainRecorder{},
	}
	_, client := startCapabilityServer(t, service)
	result, err := EnsureCompatibleCore(context.Background(), client, HandshakeOptions{
		AllowRotate: true, RequiredCaps: RequiredDesktopCapabilities(),
	})
	if !errors.Is(err, ErrCoreBusy) || result.Reused || len(result.Missing) != 1 || result.Missing[0] != api.CapabilityPiScriptedTools {
		t.Fatalf("desktop reused a Core without scripted Pi tools: %+v %v", result, err)
	}
	if service.calls.Load() != 0 {
		t.Fatal("scripted-tool upgrade tried to stop a busy Core")
	}
}

func TestDesktopDoesNotReuseBusyCoreWithoutEnvironmentOrchestration(t *testing.T) {
	status := compatibleStatus()
	status.Capabilities = nil
	for _, capability := range RequiredDesktopCapabilities() {
		if capability != api.CapabilityEnvironmentOrchestration {
			status.Capabilities = append(status.Capabilities, capability)
		}
	}
	status.ActiveRuns = 1
	service := &statusAdminService{
		statusService: &statusService{baseService: &baseService{health: readyHealth()}, status: status},
		drainRecorder: &drainRecorder{},
	}
	_, client := startCapabilityServer(t, service)
	result, err := EnsureCompatibleCore(context.Background(), client, HandshakeOptions{
		AllowRotate: true, RequiredCaps: RequiredDesktopCapabilities(),
	})
	if !errors.Is(err, ErrCoreBusy) || result.Reused || len(result.Missing) != 1 || result.Missing[0] != api.CapabilityEnvironmentOrchestration {
		t.Fatalf("desktop reused a Core lacking production cross-environment routing: %+v %v", result, err)
	}
	if service.calls.Load() != 0 {
		t.Fatal("capability upgrade tried to stop a busy Core")
	}
}

func TestRequiredRemoteDesktopCapabilitiesExcludesLocalPiRuntime(t *testing.T) {
	status := compatibleStatus()
	status.Capabilities = nil
	for _, capability := range RequiredRemoteDesktopCapabilities() {
		status.Capabilities = append(status.Capabilities, capability)
	}
	// Remote peers running <= 0.1.31 omit local-only CapabilityPiRuntime.
	_, client := startCapabilityServer(t, &statusService{
		baseService: &baseService{health: readyHealth()},
		status:      status,
	})
	result, err := VerifyCoreCapabilities(context.Background(), client, RequiredRemoteDesktopCapabilities())
	if err != nil {
		t.Fatalf("remote handshake rejected peer without local CapabilityPiRuntime: missing=%v err=%v", result.Missing, err)
	}
	// Local desktop startup still enforces CapabilityPiRuntime.
	localRes, localErr := VerifyCoreCapabilities(context.Background(), client, RequiredDesktopCapabilities())
	if localErr == nil || len(localRes.Missing) != 1 || localRes.Missing[0] != api.CapabilityPiRuntime {
		t.Fatalf("local RequiredDesktopCapabilities should still require CapabilityPiRuntime: missing=%v err=%v", localRes.Missing, localErr)
	}
}
