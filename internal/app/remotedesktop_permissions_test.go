package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

func TestPermissionCoreAPISeparatesProbeExplicitRequestAndSettings(t *testing.T) {
	probes, requests, settings := 0, 0, 0
	status := rtc.PermissionStatus{Platform: "darwin", ScreenCapture: rtc.PermissionAccess{State: "denied", CanRequest: true, CanOpenSettings: true}, InputControl: rtc.PermissionAccess{State: "denied", CanRequest: true, CanOpenSettings: true}}
	service := &workbenchService{permissions: &remotePermissionBackend{
		helper: func() (string, error) { return "/target/Core/Resources/remote-desktop/vantry-remote-helper", nil },
		probe: func(_ context.Context, path string) (rtc.PermissionStatus, error) {
			probes++
			if path != "/target/Core/Resources/remote-desktop/vantry-remote-helper" {
				t.Fatal("probe borrowed another Core's helper")
			}
			return status, nil
		},
		request: func(_ context.Context, path, kind string) (rtc.PermissionStatus, error) {
			requests++
			if path != "/target/Core/Resources/remote-desktop/vantry-remote-helper" || kind != "input-control" {
				t.Fatal("explicit request used a foreign responsibility or kind")
			}
			return status, nil
		},
		settings: func(_ context.Context, kind string) error {
			settings++
			if kind != "screen-capture" {
				t.Fatal("wrong settings kind")
			}
			return nil
		},
	}}
	response, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "rd.permissions"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Platform      string `json:"platform"`
		ScreenCapture struct {
			State string `json:"state"`
		} `json:"screenCapture"`
	}
	if json.Unmarshal([]byte(response.Output), &decoded) != nil || decoded.Platform != "darwin" || decoded.ScreenCapture.State != "denied" {
		t.Fatal("permission wire contract changed")
	}
	if probes != 1 || requests != 0 || settings != 0 {
		t.Fatal("read-only probe requested consent or opened settings")
	}
	_, err = service.Workbench(context.Background(), api.WorkbenchRequest{Action: "rd.permission.request", Name: "input-control"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Workbench(context.Background(), api.WorkbenchRequest{Action: "rd.permission.settings", Name: "screen-capture"})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || settings != 1 || probes != 2 {
		t.Fatal("settings/request operations crossed their explicit boundary")
	}
	_, err = service.Workbench(context.Background(), api.WorkbenchRequest{Action: "rd.permission.request", Name: "arbitrary-system-command"})
	if err == nil || requests != 1 {
		t.Fatal("unrecognized permission kind reached the OS backend")
	}
}
func TestMissingPermissionHelperIsStructuredUnavailable(t *testing.T) {
	service := &workbenchService{permissions: &remotePermissionBackend{
		helper: func() (string, error) { return "", errors.New("not packaged") },
		probe: func(_ context.Context, path string) (rtc.PermissionStatus, error) {
			if path != "" {
				t.Fatal("missing component silently selected another helper")
			}
			return rtc.PermissionStatus{Platform: "windows", CanHost: false, ScreenCapture: rtc.PermissionAccess{State: "not-required"}, InputControl: rtc.PermissionAccess{State: "not-required"}, UnavailableReason: "Windows native component unavailable"}, nil
		},
	}}
	response, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "rd.permissions"})
	if err != nil {
		t.Fatal(err)
	}
	var status rtc.PermissionStatus
	if json.Unmarshal([]byte(response.Output), &status) != nil || status.Platform != "windows" || status.CanHost || status.UnavailableReason == "" {
		t.Fatal("missing Windows component was treated as a working host")
	}
}

func TestAccessibilityRegistrationDoesNotOverrideInputAccess(t *testing.T) {
	registrations, requests, settings, probes := 0, 0, 0, 0
	status := rtc.PermissionStatus{
		Platform:      "darwin",
		ScreenCapture: rtc.PermissionAccess{State: "granted"},
		InputControl:  rtc.PermissionAccess{State: "denied", CanRequest: true, CanOpenSettings: true},
	}
	service := &workbenchService{permissions: &remotePermissionBackend{
		registerInput: func() (bool, error) { registrations++; return true, nil },
		helper:        func() (string, error) { return "", nil },
		probe:         func(context.Context, string) (rtc.PermissionStatus, error) { probes++; return status, nil },
		request:       func(context.Context, string, string) (rtc.PermissionStatus, error) { requests++; return status, nil },
		settings:      func(context.Context, string) error { settings++; return nil },
	}}
	check := func(action, kind, state string, canHost bool) {
		t.Helper()
		response, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: action, Name: kind})
		if err != nil {
			t.Fatal(err)
		}
		var got rtc.PermissionStatus
		if err = json.Unmarshal([]byte(response.Output), &got); err != nil {
			t.Fatal(err)
		}
		if got.InputControl.State != state || got.CanHost != canHost {
			t.Fatalf("reported AX registration instead of actual input access: %+v", got)
		}
	}
	check("rd.permissions", "", "denied", false)
	check("rd.permission.settings", rtc.PermissionInputControl, "denied", false)
	if registrations != 0 || requests != 0 || settings != 1 || probes != 2 {
		t.Fatal("probe or Settings requested consent")
	}
	check("rd.permission.request", rtc.PermissionInputControl, "denied", false)
	check("rd.permission.request", rtc.PermissionInputControl, "denied", false)
	if registrations != 2 || requests != 2 {
		t.Fatal("explicit input requests did not reach both system APIs")
	}
	check("rd.permission.request", rtc.PermissionScreenCapture, "denied", false)
	if registrations != 2 || requests != 3 {
		t.Fatal("screen request registered input consent")
	}
	status.InputControl.State, status.CanHost = "granted", true
	check("rd.permissions", "", "granted", true)
	if registrations != 2 || requests != 3 {
		t.Fatal("rechecking repeated a consent request")
	}
}

func TestAccessibilityPromptDoesNotRequestASecondPrompt(t *testing.T) {
	for _, registrationError := range []error{nil, errors.New("AX unavailable")} {
		probes, requests := 0, 0
		denied := rtc.PermissionStatus{Platform: "darwin", InputControl: rtc.PermissionAccess{State: "denied"}}
		service := &workbenchService{permissions: &remotePermissionBackend{
			registerInput: func() (bool, error) { return false, registrationError },
			helper:        func() (string, error) { return "", nil },
			probe:         func(context.Context, string) (rtc.PermissionStatus, error) { probes++; return denied, nil },
			request:       func(context.Context, string, string) (rtc.PermissionStatus, error) { requests++; return denied, nil },
		}}
		response, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "rd.permission.request", Name: rtc.PermissionInputControl})
		if err != nil {
			t.Fatal(err)
		}
		var got rtc.PermissionStatus
		if err = json.Unmarshal([]byte(response.Output), &got); err != nil {
			t.Fatal(err)
		}
		if got.InputControl.State != "denied" || got.CanHost {
			t.Fatal("pending consent reported granted")
		}
		if registrationError == nil && (probes != 1 || requests != 0) {
			t.Fatal("AX prompt followed by a second permission request")
		}
		if registrationError != nil && (probes != 0 || requests != 1) {
			t.Fatal("unavailable AX prevented the independent native input request")
		}
	}
}
