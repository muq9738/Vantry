package app

import (
	"context"
	"errors"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestDesktopDoesNotReuseBusyCoreWithoutPermissionAPI(t *testing.T) {
	status := compatibleStatus()
	status.Capabilities = nil
	for _, capability := range RequiredDesktopCapabilities() {
		if capability != api.CapabilityRemoteDesktopPermissions {
			status.Capabilities = append(status.Capabilities, capability)
		}
	}
	status.ActiveRuns = 1
	service := &statusAdminService{statusService: &statusService{baseService: &baseService{health: readyHealth()}, status: status}, drainRecorder: &drainRecorder{}}
	_, client := startCapabilityServer(t, service)
	result, err := EnsureCompatibleCore(context.Background(), client, HandshakeOptions{AllowRotate: true, RequiredCaps: RequiredDesktopCapabilities()})
	if !errors.Is(err, ErrCoreBusy) || result.Reused || len(result.Missing) != 1 || result.Missing[0] != api.CapabilityRemoteDesktopPermissions {
		t.Fatal("permission UI reused an incompatible Core or masked its missing endpoint")
	}
	if service.calls.Load() != 0 {
		t.Fatal("permission upgrade stopped a Core with active work")
	}
}
func TestWorkbenchReportsPermissionAPIWithoutPromisingCaptureHardware(t *testing.T) {
	status, err := (&workbenchService{Service: &baseService{health: readyHealth()}}).ServerStatus(context.Background())
	if err != nil || !status.HasCapability(api.CapabilityRemoteDesktopPermissions) {
		t.Fatal("running Core did not advertise its explicit permission contract")
	}
}
