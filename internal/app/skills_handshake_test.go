package app

import (
	"context"
	"errors"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestDesktopRejectsCoreWithoutSkillCatalog(t *testing.T) {
	for _, active := range []int{0, 1} {
		status := compatibleStatus()
		status.Capabilities = nil
		for _, capability := range RequiredDesktopCapabilities() {
			if capability != api.CapabilitySkills {
				status.Capabilities = append(status.Capabilities, capability)
			}
		}
		status.ActiveRuns = active
		service := &statusAdminService{
			statusService: &statusService{baseService: &baseService{health: readyHealth()}, status: status},
			drainRecorder: &drainRecorder{},
		}
		_, client := startCapabilityServer(t, service)
		result, err := EnsureCompatibleCore(context.Background(), client, HandshakeOptions{
			AllowRotate: active > 0, RequiredCaps: RequiredDesktopCapabilities(),
		})
		want := ErrCoreCapabilitiesMissing
		if active > 0 {
			want = ErrCoreBusy
		}
		if !errors.Is(err, want) || result.Reused || len(result.Missing) != 1 || result.Missing[0] != api.CapabilitySkills {
			t.Fatalf("desktop reused a Core without its skill menu endpoint (active=%d): %+v %v", active, result, err)
		}
		if service.calls.Load() != 0 {
			t.Fatal("checking the skill capability stopped an existing Core")
		}
	}
}

func TestWorkbenchAdvertisesSkillCatalog(t *testing.T) {
	for _, service := range []api.Service{
		&baseService{health: readyHealth()},
		&statusService{baseService: &baseService{health: readyHealth()}, status: compatibleStatus()},
	} {
		status, err := (&workbenchService{Service: service}).ServerStatus(context.Background())
		if err != nil || !status.HasCapability(api.CapabilitySkills) {
			t.Fatalf("running workbench did not advertise skills.list: %+v %v", status, err)
		}
	}
}
