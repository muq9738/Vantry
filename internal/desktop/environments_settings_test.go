package desktop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestSettingsEnvironmentsShowsAndEditsDisconnectedPeers(t *testing.T) {
	peers := []remoteaccess.PeerInfo{
		{
			ID:        "peer-mini",
			DeviceID:  "dev-mac-mini",
			Name:      "Mac mini",
			Relay:     "https://relay.example.com/vantry-relay",
			Relays:    []string{"https://relay.example.com/vantry-relay", "https://backup-relay.example.com:9443"},
			Direct:    []string{"192.168.1.20:52041"},
			LastError: "relay dial timeout",
		},
		{
			ID:       "peer-laptop",
			DeviceID: "dev-laptop-mac",
			Name:     "Dev Laptop",
			Relay:    "https://relay.example.com",
			Relays:   []string{"https://relay.example.com"},
			Direct:   []string{"10.0.0.20:61883"},
		},
	}

	makeStatus := func() string {
		b, _ := json.Marshal(remoteaccess.Status{
			Enabled: true,
			State:   "online",
			Name:    "Mac Studio",
			Relay:   "https://backup-relay.example.com:9443",
			Peers:   peers,
		})
		return string(b)
	}

	w, rec := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		switch r.Action {
		case "access.status", "access.discover":
			return api.WorkbenchResponse{Output: makeStatus()}, nil
		case "access.peer.update":
			var update remoteaccess.PeerUpdate
			if err := json.Unmarshal(r.Input, &update); err != nil {
				t.Fatal(err)
			}
			for i := range peers {
				if peers[i].ID == update.ID {
					peers[i].Name = update.Name
					peers[i].Relay = update.Relay
					relays := []string{}
					if update.Relay != "" {
						relays = append(relays, update.Relay)
					}
					relays = append(relays, update.Relays...)
					peers[i].Relays = relays
					peers[i].Direct = append([]string(nil), update.Direct...)
					peers[i].LastError = ""
				}
			}
			return api.WorkbenchResponse{Output: makeStatus()}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	w.widgets.settingsOpen = true
	w.widgets.settingsPage = "environments"

	tt := ui.NewTester(w.view, 1180, 860)
	tt.Frame()

	for _, want := range []string{"远程环境", "Mac mini", "Dev Laptop", "已断开"} {
		if !tt.HasText(want) {
			t.Fatalf("Settings -> Environments missing %q; texts=%v", want, tt.Texts())
		}
	}

	// Open settings editor for disconnected Mac mini.
	mustClick(t, tt, "设置 Mac mini")
	tt.Frame()
	if w.widgets.envSettings.editingPeerID != "peer-mini" || !tt.HasText("环境连接设置") {
		t.Fatalf("inline peer editor did not open for Mac mini: editing=%q texts=%v", w.widgets.envSettings.editingPeerID, tt.Texts())
	}

	w.widgets.envSettings.editName = "Mac mini Office"
	w.widgets.envSettings.editRelay = "https://new-relay.example.com"
	w.widgets.envSettings.editDirect = "192.168.31.200:52041"
	mustClick(t, tt, "保存设置")
	tt.Frame()

	req, ok := rec.find("access.peer.update")
	if !ok {
		t.Fatal("access.peer.update action was not dispatched")
	}
	var sent remoteaccess.PeerUpdate
	if err := json.Unmarshal(req.Input, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ID != "peer-mini" || sent.Name != "Mac mini Office" || sent.Relay != "https://new-relay.example.com" || len(sent.Direct) != 1 || sent.Direct[0] != "192.168.31.200:52041" {
		t.Fatalf("unexpected access.peer.update payload: %+v", sent)
	}
	if !tt.HasText("Mac mini Office") || !strings.Contains(strings.Join(tt.Texts(), "\n"), "https://new-relay.example.com") {
		t.Fatalf("updated peer not reflected in Settings -> Environments: %v", tt.Texts())
	}
}

func TestSidebarDisconnectedMachineOpensSettingsEnvironments(t *testing.T) {
	statusJSON, _ := json.Marshal(remoteaccess.Status{
		Enabled: true,
		Name:    "Mac Studio",
		Peers: []remoteaccess.PeerInfo{
			{ID: "peer-laptop", DeviceID: "dev-laptop", Name: "Dev Laptop", Relay: "https://relay.example.com"},
		},
	})
	root, _ := v3Workbench(t, func(r api.WorkbenchRequest) (api.WorkbenchResponse, error) {
		if r.Action == "access.status" || r.Action == "access.discover" {
			return api.WorkbenchResponse{Output: string(statusJSON)}, nil
		}
		return api.WorkbenchResponse{}, nil
	})
	hub := newEnvironmentHub(root, root.ctx)
	hub.syncStatus(remoteaccess.Status{
		Enabled: true,
		Name:    "Mac Studio",
		Peers: []remoteaccess.PeerInfo{
			{ID: "peer-laptop", DeviceID: "dev-laptop", Name: "Dev Laptop", Relay: "https://relay.example.com"},
		},
	})
	// Keep peer-laptop disconnected (view == nil) with a failure notice.
	hub.records[1].failed = "连接 Dev Laptop 失败：timeout"

	tt := ui.NewTester(root.view, 1280, 820)
	tt.Frame()
	if tt.HasText("连接 Dev Laptop 失败") {
		t.Fatalf("unconnected peer should stay hidden in unfiltered sidebar: %v", tt.Texts())
	}
	// Filtering the sidebar to Dev Laptop surfaces its status row and settings gear.
	hub.setFilter("peer-laptop")
	tt.Frame()

	mustClick(t, tt, "环境设置 Dev Laptop")
	tt.Frame()

	if !root.widgets.settingsOpen || root.widgets.settingsPage != "environments" || root.widgets.envSettings.editingPeerID != "peer-laptop" {
		t.Fatalf("sidebar gear did not open Settings -> Environments for Dev Laptop: open=%v page=%q editing=%q", root.widgets.settingsOpen, root.widgets.settingsPage, root.widgets.envSettings.editingPeerID)
	}
	if !tt.HasText("环境连接设置") || !tt.HasText("Dev Laptop") {
		t.Fatalf("Settings -> Environments did not open Dev Laptop editor: %v", tt.Texts())
	}
}
