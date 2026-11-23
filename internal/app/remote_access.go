package app

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/muq9738/Vantry/internal/plugins"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/internal/t3compat"
	"github.com/muq9738/Vantry/internal/updater"
)

func (s *workbenchService) accessManager() (*remoteaccess.Manager, error) {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if s.access != nil {
		return s.access, nil
	}
	root := s.root
	if root == nil {
		root = context.Background()
	}
	info, err := s.environmentInfo()
	if err != nil {
		return nil, err
	}
	bridge := s.rdBridge()
	adapter, err := t3compat.New(s, t3compat.Options{DataDir: s.config.DataDir, EnvironmentID: info.ID, Name: info.Name, Version: updater.CurrentVersion, Provider: info.ProviderID, Model: info.DefaultModel, Workspace: info.Workspace, Desktop: bridge.mobileDesktopTarget, ConnectionRoutes: func() t3compat.Object {
		s.accessMu.Lock()
		manager := s.access
		s.accessMu.Unlock()
		if manager == nil {
			return nil
		}
		return t3compat.Object(manager.MobileRoutes())
	}})
	if err != nil {
		return nil, err
	}
	manager, err := remoteaccess.New(root, remoteaccess.Options{DataDir: s.config.DataDir, SocketPath: s.config.SocketPath(), T3Handler: adapter, T3Pairing: adapter.PairingLink, T3Revoke: adapter.RevokeAll, T3Disconnect: adapter.Disconnect, T3CancelPairings: adapter.RevokePairings, T3RevokeClient: adapter.Revoke, T3Clients: func() []remoteaccess.AccessClient {
		clients := []remoteaccess.AccessClient{}
		for _, client := range adapter.Clients() {
			clients = append(clients, remoteaccess.AccessClient{ID: "t3:" + client.ID, Name: "T3 · " + client.Name, CreatedAt: client.CreatedAt})
		}
		return clients
	}})
	if err != nil {
		return nil, err
	}
	s.access = manager
	return manager, nil
}

// sharedDesktopRelay is the relay desktop sharing falls back to across
// networks: the relay of this computer's remote environments, so desktop
// sharing needs no address of its own. A relay saved in the desktop plugin
// by earlier versions is still honored when no environment relay is set.
func (s *workbenchService) sharedDesktopRelay() string {
	if manager, err := s.accessManager(); err == nil {
		if relay := strings.TrimSpace(manager.Status().Relay); relay != "" {
			return relay
		}
	}
	legacy, _ := (plugins.Host{Directory: filepath.Join(s.config.DataDir, "plugins"), DataDir: s.config.DataDir}).GetSetting("vantry-remote-desktop", "relayURL")
	return strings.TrimSpace(legacy)
}
