package desktop

import (
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

type permissionFixture struct {
	status                                    RemotePermissions
	grant                                     bool
	probes, requests, settings, starts, stops int
	startRequestID, stopRequestID             string
}

func permissionWorkbench(t *testing.T, platform string) (*workbench, *permissionFixture) {
	t.Helper()
	f := &permissionFixture{status: RemotePermissions{Platform: platform, ScreenCapture: RemotePermissionAccess{State: "denied", CanRequest: true, CanOpenSettings: true}, InputControl: RemotePermissionAccess{State: "denied", CanRequest: true, CanOpenSettings: true}}}
	w, _ := v3Workbench(t, func(request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch request.Action {
		case actionRDStatus:
			return api.WorkbenchResponse{Output: `{"hosting":false}`}, nil
		case actionRDPermissions:
			f.probes++
		case actionRDPermissionRequest:
			f.requests++
			if f.grant {
				if request.Name == "screen-capture" {
					f.status.ScreenCapture.State = "granted"
				} else {
					f.status.InputControl.State = "granted"
				}
				f.status.CanHost = permissionReady(f.status.ScreenCapture) && permissionReady(f.status.InputControl)
			}
		case actionRDPermissionSettings:
			f.settings++
		case actionRDHostStart:
			f.starts++
			f.startRequestID = request.Message
			raw, _ := json.Marshal(RemoteDesktopHostStatus{Hosting: true, RelaySSHHost: request.Name})
			return api.WorkbenchResponse{Output: string(raw)}, nil
		case actionRDHostStop:
			f.stops++
			f.stopRequestID = request.Message
			return api.WorkbenchResponse{Output: `{"hosting":false}`}, nil
		default:
			return api.WorkbenchResponse{}, nil
		}
		raw, _ := json.Marshal(f.status)
		return api.WorkbenchResponse{Output: string(raw)}, nil
	})
	remoteDesktopPluginFixture(t, w)
	return w, f
}
func TestPermissionPanelRenders(t *testing.T) {
	output := os.Getenv("VANTRY_PERMISSION_RENDER_DIR")
	if output == "" {
		t.Skip("set VANTRY_PERMISSION_RENDER_DIR for permission review images")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, platform string
		dark           bool
	}{{"mac-dark", "darwin", true}, {"mac-light", "darwin", false}, {"windows-unavailable", "windows", false}} {
		t.Run(test.name, func(t *testing.T) {
			w, f := permissionWorkbench(t, test.platform)
			w.widgets.themeSet, w.widgets.dark = true, test.dark
			if test.platform == "windows" {
				f.status.ScreenCapture = RemotePermissionAccess{State: "not-required"}
				f.status.InputControl = RemotePermissionAccess{State: "not-required"}
				f.status.UnavailableReason = "Windows 原生捕获组件尚不可用"
			}
			tt := ui.NewTesterWith(w.view, 1440, 900, ui.TesterOptions{Dark: test.dark})
			mustClick(t, tt, "远程桌面")
			mustClick(t, tt, "本机被控")
			file, err := os.Create(filepath.Join(output, test.name+".png"))
			if err != nil {
				t.Fatal(err)
			}
			if err = png.Encode(file, tt.Image()); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPermissionPageOnlyProbesUntilExplicitEnable(t *testing.T) {
	w, f := permissionWorkbench(t, "darwin")
	tt := ui.NewTester(w.view, 1440, 900)
	if f.requests != 0 || f.starts != 0 || f.probes != 0 {
		t.Fatal("install or startup requested permissions")
	}
	mustClick(t, tt, "远程桌面")
	if f.probes != 1 || f.requests != 0 || f.starts != 0 || w.rd.permission.intent {
		t.Fatal("opening the plugin requested permission or sharing")
	}
	mustClick(t, tt, "本机被控")
	if !w.rd.permission.intent || f.requests != 0 || f.starts != 0 || w.rd.host.Hosting {
		t.Fatal("master switch started sharing before authorization")
	}
	if !tt.HasText("需要允许") {
		t.Fatal("missing compact permission states")
	}
}
func TestPermissionRequestsAreExplicitAndSharingReflectsCoreSuccess(t *testing.T) {
	w, f := permissionWorkbench(t, "darwin")
	f.grant = true
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "远程桌面")
	mustClick(t, tt, "本机被控")
	mustClick(t, tt, "允许 屏幕录制")
	if f.requests != 1 || f.starts != 0 {
		t.Fatal("screen permission alone started input sharing")
	}
	mustClick(t, tt, "允许 辅助功能")
	if f.requests != 2 || f.starts != 1 || w.rd.host == nil || !w.rd.host.Hosting || w.rd.permission.intent {
		t.Fatal("switch did not wait for all permissions and Core host acknowledgement")
	}
	mustClick(t, tt, "本机被控")
	if f.stops != 1 || w.rd.host.Hosting {
		t.Fatal("off did not stop the owning Core")
	}
}
func TestPermissionDenialNeverRepeatsPromptAndPollingIsBounded(t *testing.T) {
	w, f := permissionWorkbench(t, "darwin")
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "远程桌面")
	mustClick(t, tt, "本机被控")
	mustClick(t, tt, "允许 屏幕录制")
	for i := 0; i < 30; i++ {
		tt.Advance(2 * time.Second)
	}
	if f.requests != 1 || f.starts != 0 || f.probes > 17 || w.rd.permission.polls != 0 {
		t.Fatalf("permission retry escaped its explicit/bounded flow: prompts=%d probes=%d starts=%d", f.requests, f.probes, f.starts)
	}
	w.stage = ""
	tt.Frame()
	before := f.probes
	w.remotePermissionFocusReturned()
	if w.rd.permission.intent || f.probes != before {
		t.Fatal("leaving the plugin kept an authorization intent")
	}
}
func TestLatePermissionProbeCannotStartAfterPageExit(t *testing.T) {
	w, f := permissionWorkbench(t, "darwin")
	f.status.ScreenCapture.State = "granted"
	f.status.InputControl.State = "granted"
	f.status.CanHost = true
	w.openRemoteDesktop("")
	var posts []func()
	w.post = func(fn func()) { posts = append(posts, fn) }
	w.beginRemoteHostingIntent()
	if len(posts) != 1 {
		t.Fatal("probe was not scheduled")
	}
	w.stage = ""
	w.cancelRemoteHostingIntent()
	posts[0]()
	if f.starts != 0 || w.rd.permission.intent {
		t.Fatal("late permission response started sharing on a closed page")
	}
}
func TestCancelledLateHostStartIsStoppedOnItsOriginalCore(t *testing.T) {
	w, f := permissionWorkbench(t, "darwin")
	f.status.ScreenCapture.State = "granted"
	f.status.InputControl.State = "granted"
	f.status.CanHost = true
	w.openRemoteDesktop("")
	var posts []func()
	w.post = func(fn func()) { posts = append(posts, fn) }
	w.beginRemoteHostingIntent()
	posts[0]()
	if f.starts != 1 || len(posts) != 2 {
		t.Fatal("authorized start not issued")
	}
	w.stage = ""
	w.cancelRemoteHostingIntent()
	posts[1]()
	if f.stops != 1 || f.stopRequestID == "" || f.stopRequestID != f.startRequestID || w.rd.host != nil && w.rd.host.Hosting {
		t.Fatal("cancelled start leaked a sharing session")
	}
}
func TestPermissionFocusReturnOnlyChecksAnActiveIntent(t *testing.T) {
	w, f := permissionWorkbench(t, "darwin")
	w.openRemoteDesktop("")
	w.remotePermissionFocusReturned()
	if f.probes != 0 {
		t.Fatal("focus caused an unsolicited permission flow")
	}
	w.beginRemoteHostingIntent()
	before := f.probes
	w.remotePermissionFocusReturned()
	if f.probes != before+1 || f.requests != 0 {
		t.Fatal("focus return did not perform a read-only status check")
	}
	w.cancelRemoteHostingIntent()
	before = f.probes
	w.remotePermissionFocusReturned()
	if f.probes != before {
		t.Fatal("cancelled flow checked on focus")
	}
}
func TestWindowsMissingCaptureDoesNotShowMacSettingsOrEnable(t *testing.T) {
	w, f := permissionWorkbench(t, "windows")
	f.status.ScreenCapture = RemotePermissionAccess{State: "unavailable"}
	f.status.InputControl = RemotePermissionAccess{State: "not-required"}
	f.status.UnavailableReason = "Windows 原生捕获组件尚未安装"
	tt := ui.NewTester(w.view, 1440, 900)
	mustClick(t, tt, "远程桌面")
	mustClick(t, tt, "本机被控")
	text := strings.Join(tt.Texts(), " ")
	if !strings.Contains(text, "Windows") || strings.Contains(text, "macOS") || strings.Contains(text, "隐私与安全性") || f.starts != 0 || f.requests != 0 {
		t.Fatal("Windows component state was presented as a Mac permission or working host")
	}
	if _, ok := tt.Access("允许 屏幕共享"); ok {
		t.Fatal("unavailable component offered a fake grant button")
	}
}
