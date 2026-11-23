package relaycmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/internal/connectivity"
)

func TestInitCreatesSeparatePrivateCredentialsAndNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"init", "--config", path, "--desktop"}, &output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if json.Unmarshal(data, &cfg) != nil {
		t.Fatal("invalid config")
	}
	if cfg.Sessions == nil || cfg.Desktop == nil || len(cfg.Sessions.RegistrationKey) < 32 || len(cfg.Desktop.RegistrationKey) < 32 || cfg.Sessions.RegistrationKey == cfg.Desktop.RegistrationKey {
		t.Fatal("registration credentials not independent")
	}
	if strings.Contains(output.String(), cfg.Sessions.RegistrationKey) || strings.Contains(output.String(), cfg.Desktop.RegistrationKey) {
		t.Fatal("credentials printed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private mode", info, err)
	}
	if err = Run(context.Background(), []string{"init", "--config", path}, &output); !os.IsExist(err) {
		t.Fatal("existing config overwritten", err)
	}
	var retained Config
	data, _ = os.ReadFile(path)
	_ = json.Unmarshal(data, &retained)
	if retained.Sessions.RegistrationKey != cfg.Sessions.RegistrationKey {
		t.Fatal("existing credentials changed")
	}
}
func TestPublicListenerRequiresNativeTLS(t *testing.T) {
	err := Serve(context.Background(), Config{Listen: "0.0.0.0:0", Sessions: &connectivity.RelayConfig{RegistrationKey: connectivity.Token()}}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatal("public plaintext listener started", err)
	}
}
