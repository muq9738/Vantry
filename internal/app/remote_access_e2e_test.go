package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/connectivity"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestRelayRemoteWorkspaceEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	key := connectivity.Token()
	relay, err := connectivity.NewRelay(connectivity.RelayConfig{RegistrationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	gateway := httptest.NewServer(relay)
	defer gateway.Close()
	data, err := os.MkdirTemp("/tmp", "vt-native-access-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	workspace := t.TempDir()
	if err = os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("native remote workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	coreCtx, stopCore := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- RunCore(coreCtx, Config{DataDir: data, ProviderID: "controlled", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: workspace}})
	}()
	defer func() {
		stopCore()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	probe := local.Dial(filepath.Join(data, "core.sock"))
	defer probe.Close()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err = probe.Health(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	host, err := remoteaccess.New(ctx, remoteaccess.Options{DataDir: data, SocketPath: filepath.Join(data, "core.sock"), Node: "/no-node-installed", DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if _, err = host.Start(ctx, "Host Mac", gateway.URL, key); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !host.Status().RelayConnected; {
		if time.Now().After(deadline) {
			t.Fatal(host.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, err := host.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	controllerData := t.TempDir()
	controllerOpts := remoteaccess.Options{DataDir: controllerData, Node: "/no-node-installed", DisableDiscovery: true}
	controller, err := remoteaccess.New(ctx, controllerOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	paired, err := controller.PairWithContext(ctx, status.PairingCode, gateway.URL)
	if err != nil || len(paired.Peers) != 1 {
		t.Fatal(paired, err)
	}
	if _, err = controller.PairWithContext(ctx, status.PairingCode, gateway.URL); err == nil {
		t.Fatal("single-use pairing was reused")
	}
	client, err := remoteaccess.ConnectWithMode(ctx, controllerOpts, paired.Peers[0].ID, "relay")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.Mode() != "relay" {
		t.Fatal("relay was bypassed")
	}
	for _, mode := range []string{"direct", "auto"} {
		connection, err := remoteaccess.ConnectWithMode(ctx, controllerOpts, paired.Peers[0].ID, mode)
		if err != nil {
			t.Fatal(mode, err)
		}
		if connection.Mode() != "direct" {
			t.Fatal("direct route was not used")
		}
		_ = connection.Close()
	}
	read, err := client.Workbench(ctx, api.WorkbenchRequest{Action: "files.read", Workspace: workspace, Paths: []string{"sample.txt"}})
	if err != nil || read.Output == "" {
		t.Fatal("remote read", read, err)
	}
	if _, err = client.Workbench(ctx, api.WorkbenchRequest{Action: "rd.host_start"}); err == nil {
		t.Fatal("session credential granted desktop sharing")
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := client.Subscribe(ctx, snapshot.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	created, err := client.Dispatch(ctx, api.Command{Version: 1, CommandID: "native-create", Kind: api.CreateThread, Title: "Native relay", ProviderID: "controlled", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: workspace}})
	if err != nil || created.ThreadID == "" {
		t.Fatal(created, err)
	}
	select {
	case <-sub.Events:
	case <-time.After(3 * time.Second):
		t.Fatal("event stream did not deliver")
	}
	// Regenerating a code must not revoke devices that already redeemed one.
	if _, err = host.GenerateCode(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Health(ctx); err != nil {
		t.Fatal("code regeneration revoked an established device", err)
	}
	configData, err := os.ReadFile(filepath.Join(controllerData, "remote-access.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Peers []remoteaccess.Peer `json:"peers"`
	}
	if json.Unmarshal(configData, &saved) != nil {
		t.Fatal("saved pairing unreadable")
	}
	pair := saved.Peers[0].Pair
	wrong := pair
	wrong.Secret = connectivity.Token()
	if _, err = controller.Pair(wrong.Link()); err != nil {
		t.Fatal(err)
	}
	if unauthorized, err := remoteaccess.ConnectWithMode(ctx, controllerOpts, paired.Peers[0].ID, "relay"); err == nil {
		_ = unauthorized.Close()
		t.Fatal("wrong credential accepted")
	}
	if _, err = controller.Pair(pair.Link()); err != nil {
		t.Fatal(err)
	}
	if _, err = host.RevokeClient(host.Status().Clients[0].ID); err != nil {
		t.Fatal(err)
	}
	if unauthorized, err := remoteaccess.ConnectWithMode(ctx, controllerOpts, paired.Peers[0].ID, "direct"); err == nil {
		_ = unauthorized.Close()
		t.Fatal("revoked device accepted")
	}
	// A private LAN link exchanges a code over authenticated TLS too.
	directHost, err := remoteaccess.New(ctx, remoteaccess.Options{DataDir: t.TempDir(), SocketPath: filepath.Join(data, "core.sock"), DisableDiscovery: true, ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer directHost.Close()
	if _, err = directHost.Start(ctx, "LAN", "", ""); err != nil {
		t.Fatal(err)
	}
	directStatus, err := directHost.GenerateCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lanPaired, err := controller.Pair(directStatus.PairingLink)
	if err != nil {
		t.Fatal(err)
	}
	lanID := lanPaired.Peers[len(lanPaired.Peers)-1].ID
	lan, err := remoteaccess.Connect(ctx, controllerOpts, lanID)
	if err != nil {
		t.Fatal(err)
	}
	_ = lan.Close()
	if _, err = probe.Health(ctx); err != nil {
		t.Fatal("closing clients stopped Core", err)
	}
	resp, err := http.Get(gateway.URL + "/v1/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("relay exposed unauthenticated Core")
	}
}
