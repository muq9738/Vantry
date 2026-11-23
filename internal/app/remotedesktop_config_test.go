package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/muq9738/Vantry/internal/plugins"
)

func configureTestSSH(t *testing.T, data string) string {
	t.Helper()
	dir := filepath.Join(data, "plugins", "vantry-remote-desktop")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"id": "vantry-remote-desktop", "name": "RTC test", "main": "index.js", "settings": []map[string]any{{"key": "sshConfigFile", "type": "string", "default": ""}}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("export function run(){return 'ok';}"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "ssh_config")
	if err := os.WriteFile(path, []byte("Host qmt\n  HostName 127.0.0.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (plugins.Host{Directory: filepath.Join(data, "plugins")}).SetSetting(context.Background(), "vantry-remote-desktop", "sshConfigFile", path); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestRTCSSHConfigComesFromOwningPhysicalDataDirectory(t *testing.T) {
	t.Setenv("VANTRY_REMOTE_SSH_CONFIG", "")
	local, remote := t.TempDir(), t.TempDir()
	localPath, remotePath := configureTestSSH(t, local), configureTestSSH(t, remote)
	viewer := NewRemoteDesktopDriver(context.Background(), RemoteDesktopOptions{DataDir: local})
	defer viewer.Close()
	host := NewRemoteDesktopDriver(context.Background(), RemoteDesktopOptions{DataDir: remote})
	defer host.Close()
	a, err := viewer.localSSHConfigFile()
	if err != nil || a != localPath {
		t.Fatal("viewer did not use its physical machine's setting")
	}
	b, err := host.localSSHConfigFile()
	if err != nil || b != remotePath || a == b {
		t.Fatal("host and viewer borrowed another machine's SSH path")
	}
}
func TestEmptyRTCSSHSettingPreservesOpenSSHDefaults(t *testing.T) {
	t.Setenv("VANTRY_REMOTE_SSH_CONFIG", "")
	b := NewRemoteDesktopDriver(context.Background(), RemoteDesktopOptions{DataDir: t.TempDir()})
	defer b.Close()
	path, err := b.localSSHConfigFile()
	if err != nil || path != "" {
		t.Fatal("empty setting replaced existing OpenSSH configuration")
	}
}
