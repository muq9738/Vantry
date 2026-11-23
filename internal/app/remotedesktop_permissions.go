package app

import (
	"context"
	"errors"
	"runtime"

	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
)

type remotePermissionBackend struct {
	registerInput func() (bool, error)
	helper        func() (string, error)
	probe         func(context.Context, string) (rtc.PermissionStatus, error)
	request       func(context.Context, string, string) (rtc.PermissionStatus, error)
	settings      func(context.Context, string) error
}

func (s *workbenchService) permissionActions() remotePermissionBackend {
	if s.permissions != nil {
		return *s.permissions
	}
	backend := remotePermissionBackend{
		helper:   remoteDesktopHelperPath,
		probe:    rtc.ProbePermissions,
		request:  rtc.RequestPermission,
		settings: rtc.OpenPermissionSettings,
	}
	if runtime.GOOS == "darwin" {
		backend.registerInput = registerAccessibility
	}
	return backend
}

// Permission operations execute in the selected Core. The local viewer is
// deliberately absent from this path, including settings and input consent.
func (s *workbenchService) remotePermissions(ctx context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	var response api.WorkbenchResponse
	backend := s.permissionActions()
	if err := ctx.Err(); err != nil {
		return response, err
	}
	if request.Action != "rd.permissions" && request.Name != rtc.PermissionScreenCapture && request.Name != rtc.PermissionInputControl {
		return response, errors.New("请选择屏幕或键鼠权限")
	}
	helper, _ := backend.helper() // The platform API reports a missing helper as unavailable.
	var status rtc.PermissionStatus
	var err error
	switch request.Action {
	case "rd.permissions":
		status, err = backend.probe(ctx, helper)
	case "rd.permission.request":
		// AX registration makes the owning application visible in Settings.
		// Its asynchronous result is not an input-capability verdict. After
		// showing its prompt, only probe so one click cannot open two prompts.
		// If AX is already trusted (or unavailable), the native input API can
		// request access. Both paths report the input sink's actual CG access.
		if request.Name == rtc.PermissionInputControl && backend.registerInput != nil {
			if trusted, registerErr := backend.registerInput(); !trusted && registerErr == nil {
				status, err = backend.probe(ctx, helper)
				break
			}
		}
		status, err = backend.request(ctx, helper, request.Name)
	case "rd.permission.settings":
		if err = backend.settings(ctx, request.Name); err == nil {
			status, err = backend.probe(ctx, helper)
		}
	default:
		return response, errors.New("未知权限操作")
	}
	if err != nil {
		return response, err
	}
	return environmentOutput(status)
}
