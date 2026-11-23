package app

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

const DefaultDelay = 80 * time.Millisecond

var ErrAlreadyRunning = errors.New("Vantry Core 已在运行")

type Config struct {
	DataDir            string
	Delay              time.Duration
	ProviderID         string
	Model              string
	PiExecutable       string
	Workspace          *api.WorkspaceSpec
	MagpieSettingsPath string
}

func DefaultDataDir() (string, error) {
	if configured := os.Getenv("VANTRY_DATA_DIR"); configured != "" {
		return filepath.Abs(configured)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Vantry"), nil
}

func (c Config) SocketPath() string { return filepath.Join(c.DataDir, "core.sock") }
