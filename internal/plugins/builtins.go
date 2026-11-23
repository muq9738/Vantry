package plugins

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func nativeBuiltins() []definition {
	return nil
}

// EnsureBuiltins prepares pluginsDir, retires deprecated bundled/script plugins,
// and clears any legacy .builtin.json marker on user-installed plugins.
func EnsureBuiltins(pluginsDir string) error {
	if strings.TrimSpace(pluginsDir) == "" {
		return errors.New("未配置内置插件目录")
	}
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		return fmt.Errorf("创建内置插件目录：%w", err)
	}

	for _, id := range []string{"workspace-insights", "herdr-bridge", "draft-cleanup", "vantry-plugin-draft-cleanup", "remote-access", "vantry-remote-access"} {
		if err := retireBundledExample(pluginsDir, id); err != nil {
			return err
		}
	}
	if err := retireDeprecatedScriptPlugins(pluginsDir); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(filepath.Dir(pluginsDir), "retired-plugins"))
	// Migrate previously force-seeded vantry-remote-desktop into a normal user-installed plugin
	// by clearing any legacy .builtin.json marker while keeping the user's installation intact.
	_ = os.Remove(filepath.Join(pluginsDir, "vantry-remote-desktop", builtinFileName))
	return nil
}

// EnsureBuiltins prepares h.Directory.
func (h Host) EnsureBuiltins() error {
	return EnsureBuiltins(h.Directory)
}

// retireBundledExample removes previously bundled or official example plugins directly.
func retireBundledExample(pluginsDir, id string) error {
	legacy := filepath.Join(pluginsDir, id)
	st, err := os.Lstat(legacy)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	_, owned := readBuiltinMarker(legacy)
	if !owned {
		if orig, hasOrig := readOrigin(legacy); hasOrig && strings.HasPrefix(strings.ToLower(orig.Repo), "muq9738/") {
			owned = true
		}
	}
	if !owned {
		return nil
	}
	return os.RemoveAll(legacy)
}

// retireDeprecatedScriptPlugins deletes inactive legacy JS/TS plugins (except
// vantry-remote-desktop, which upgrades in place) directly without backup clutter.
func retireDeprecatedScriptPlugins(pluginsDir string) error {
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	host := Host{Directory: pluginsDir}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "vantry-remote-desktop" || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		d := host.definitionFromDir(name, filepath.Join(pluginsDir, name))
		rt := d.effectiveRuntime()
		if rt != runtimeNative && rt != runtimeProcess {
			if err := os.RemoveAll(filepath.Join(pluginsDir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}
