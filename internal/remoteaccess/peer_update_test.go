package remoteaccess

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestUpdatePeerPersistsOfflinePeerAndClearsChecks(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := generateCertPair()
	if err != nil {
		t.Fatal(err)
	}
	initialCfg := configuration{
		Peers: []Peer{
			{
				ID: "peer-mini",
				Pair: Pair{
					Version:     1,
					Name:        "Mac mini",
					Relay:       "https://relay.example.com/vantry-relay",
					Relays:      []string{"https://backup-relay.example.com:9443"},
					Route:       strings.Repeat("r", 32),
					Secret:      strings.Repeat("s", 32),
					DeviceID:    "dev-mac-mini",
					Certificate: certPEM,
					Direct:      []string{"192.168.31.118:52041"},
				},
			},
		},
	}
	if err := saveConfig(dataDir, initialCfg); err != nil {
		t.Fatal(err)
	}

	mgr, err := New(context.Background(), Options{DataDir: dataDir, DisableDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// Seed a stale failed check entry for offline peer.
	unreachable := false
	mgr.mu.Lock()
	mgr.checks["peer-mini"] = PeerInfo{ID: "peer-mini", Reachable: &unreachable, LastError: "relay timeout"}
	mgr.mu.Unlock()

	status, err := mgr.UpdatePeer(PeerUpdate{
		ID:     "peer-mini",
		Name:   "Mac mini Lab",
		Relay:  "https://new-relay.example.com/",
		Relays: []string{"https://backup1.example.com", "https://new-relay.example.com", "https://backup1.example.com"},
		Direct: []string{"192.168.31.120:52041", "10.0.0.5:52041"},
	})
	if err != nil {
		t.Fatalf("UpdatePeer failed: %v", err)
	}
	if len(status.Peers) != 1 {
		t.Fatalf("peers = %+v", status.Peers)
	}
	got := status.Peers[0]
	if got.Name != "Mac mini Lab" || got.Relay != "https://new-relay.example.com" {
		t.Fatalf("updated peer mismatch: %+v", got)
	}
	if !slices.Equal(got.Direct, []string{"192.168.31.120:52041", "10.0.0.5:52041"}) {
		t.Fatalf("direct mismatch: %v", got.Direct)
	}
	if !slices.Equal(got.Relays, []string{"https://new-relay.example.com", "https://backup1.example.com"}) {
		t.Fatalf("relays mismatch: %v", got.Relays)
	}
	if got.Reachable != nil || got.LastError != "" {
		t.Fatalf("expected stale check cleared, got reachable=%v lastError=%q", got.Reachable, got.LastError)
	}

	// Reload from disk and verify secret/route/cert preserved + updates saved.
	raw, err := os.ReadFile(filepath.Join(dataDir, "remote-access.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reloaded configuration
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	savedPair := reloaded.Peers[0].Pair
	if savedPair.Secret != strings.Repeat("s", 32) || savedPair.DeviceID != "dev-mac-mini" || savedPair.Certificate != certPEM {
		t.Fatalf("identity fields mutated by UpdatePeer: %+v", savedPair)
	}

	// Reject invalid direct address and preserve previous config untouched.
	if _, err := mgr.UpdatePeer(PeerUpdate{
		ID:     "peer-mini",
		Name:   "BadDirect",
		Relay:  "https://new-relay.example.com",
		Direct: []string{"not-a-host-port"},
	}); err == nil {
		t.Fatal("expected invalid direct host:port error")
	}
	if _, err := mgr.UpdatePeer(PeerUpdate{
		ID:    "peer-mini",
		Name:  "EmptyRoutes",
		Relay: "",
	}); err == nil {
		t.Fatal("expected empty routes error")
	}
	afterErr := mgr.Status().Peers[0]
	if afterErr.Name != "Mac mini Lab" {
		t.Fatalf("failed UpdatePeer mutated peer name: %+v", afterErr)
	}
}

func TestMDNSUpdatePersistsDirectAddressesForPairedPeer(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := generateCertPair()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(dataDir, configuration{
		Peers: []Peer{{
			ID: "peer-mini",
			Pair: Pair{
				Version:     1,
				Name:        "Mac mini",
				Route:       strings.Repeat("r", 32),
				Secret:      strings.Repeat("s", 32),
				DeviceID:    "dev-mac-mini",
				Certificate: certPEM,
				Direct:      []string{"192.168.31.100:52041"},
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	mgr, err := New(context.Background(), Options{DataDir: dataDir, DisableDiscovery: true})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	mgr.applyNearbyDevice(NearbyDevice{
		ID:        "dev-mac-mini",
		Name:      "Mac mini",
		Addresses: []string{"192.168.31.118:52041"},
		Port:      52041,
	}, map[string]time.Time{})

	reloaded, err := loadConfig(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reloaded.Peers[0].Pair.Direct, []string{"192.168.31.118:52041"}) {
		t.Fatalf("expected persisted mDNS direct address, got %v", reloaded.Peers[0].Pair.Direct)
	}
}
