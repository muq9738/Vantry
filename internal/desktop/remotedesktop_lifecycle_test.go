package desktop

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

type ownedFakeViewer struct {
	fakeRemoteDesktop
	closed    atomic.Int32
	hostCalls atomic.Int32
}

func (v *ownedFakeViewer) Close() error { v.closed.Add(1); return nil }
func (v *ownedFakeViewer) HostStart(context.Context, string) (RemoteDesktopHostStatus, error) {
	v.hostCalls.Add(1)
	return RemoteDesktopHostStatus{Hosting: true}, nil
}
func (v *ownedFakeViewer) HostStop(context.Context) (RemoteDesktopHostStatus, error) {
	v.hostCalls.Add(1)
	return RemoteDesktopHostStatus{}, nil
}
func (v *ownedFakeViewer) HostStatus(context.Context) (RemoteDesktopHostStatus, error) {
	v.hostCalls.Add(1)
	return RemoteDesktopHostStatus{}, nil
}

func TestEachEnvironmentOwnsAndReleasesItsViewer(t *testing.T) {
	root := newWorkbench(&fakeService{})
	var viewers []*ownedFakeViewer
	root.defaults.RemoteDesktopFactory = func(context.Context) RemoteDesktopDriver {
		v := &ownedFakeViewer{}
		viewers = append(viewers, v)
		return v
	}
	h := newHost(context.Background(), root)
	defer h.cancel()
	if len(viewers) != 1 || root.defaults.RemoteDesktop == nil {
		t.Fatal("production host did not instantiate its configured viewer")
	}
	root.defaults.ConnectEnvironment = func(context.Context, string) (*EnvironmentSession, error) {
		return &EnvironmentSession{Service: &fakeService{}, Info: api.EnvironmentInfo{ID: "remote", ProviderID: "controlled"}}, nil
	}
	h.environments.records = append(h.environments.records, environmentRecord{key: "remote", connectionID: "peer-remote", label: "Remote"})
	h.environments.choose(root, 1, false)
	if len(viewers) != 2 || h.environments.current.defaults.RemoteDesktop == root.defaults.RemoteDesktop {
		t.Fatal("environment views shared a viewer session")
	}
	h.environments.close()
	h.environments.close()
	for _, viewer := range viewers {
		if viewer.closed.Load() != 1 {
			t.Fatal("view close did not release exactly its own driver")
		}
	}
}

func TestRemoteHostingNeverCallsLocalViewerHoster(t *testing.T) {
	root, local := v3Workbench(t, nil)
	remote, service := v3Workbench(t, func(request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		return api.WorkbenchResponse{Output: `{"hosting":false}`}, nil
	})
	remote.defaults.RemotePeer = true
	remote.defaults.RemoteName = "Remote machine"
	viewer := &ownedFakeViewer{}
	remote.defaults.RemoteDesktop = viewer
	for _, action := range []string{actionRDStatus, actionRDHostStart, actionRDHostStop} {
		called := false
		remote.hostCall(action, func(_ *RemoteDesktopHostStatus, err error) {
			if err != nil {
				t.Fatal(err)
			}
			called = true
		})
		if !called {
			t.Fatal("hosting response did not return")
		}
		if _, ok := service.find(action); !ok {
			t.Fatal("hosting action did not reach the selected Core")
		}
	}
	if viewer.hostCalls.Load() != 0 || len(local.requests) != 0 {
		t.Fatal("a remote view operated the local viewer's capture host")
	}
	_ = root
}

func TestRemoteTicketIsAbsentFromStageLabels(t *testing.T) {
	w := newWorkbench(&fakeService{})
	w.rd.target = "vantry-rtc://qmt/test-session?token=private-test-only-token"
	if got := w.remoteDesktopPeerLabel(); got != "qmt" {
		t.Fatal("display label included a private ticket")
	}
}
