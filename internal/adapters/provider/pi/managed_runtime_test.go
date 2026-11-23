package pi

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFakeExecutableWithVersion(t *testing.T, dir, relPath, version string) string {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "#!/bin/sh\necho '" + version + "'\n"
	perm := os.FileMode(0o700)
	if runtime.GOOS == "windows" && filepath.Ext(full) == "" {
		full += ".exe"
	}
	if err := os.WriteFile(full, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if version != "" {
		pkgPath := filepath.Join(filepath.Dir(full), "package.json")
		if err := os.WriteFile(pkgPath, []byte(`{"name":"@earendil-works/pi-coding-agent","version":"`+version+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return full
}

func TestResolveExternalPiRuntimePrecedence(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	systemDir := t.TempDir()

	systemBin := writeFakeExecutableWithVersion(t, systemDir, filepath.Join("node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "cli.js"), "1.5.2")
	if err := os.WriteFile(filepath.Join(systemDir, "node_modules", "@earendil-works", "pi-coding-agent", "package.json"), []byte(`{"name":"@earendil-works/pi-coding-agent","version":"1.5.2"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	probeWithSystem := func() (string, bool) { return systemBin, true }
	probeNoSystem := func() (string, bool) { return "", false }

	// 1. Missing when no system Pi and no custom path.
	res := resolvePiRuntimeWith(runtimeResolveOptions{
		Requested:   "pi",
		DataDir:     dataDir,
		SystemProbe: probeNoSystem,
	})
	if res.ActiveSource != RuntimeSourceMissing || res.Path != "pi" {
		t.Fatalf("missing resolution = %+v", res)
	}

	// 2. Discovers system external npm Pi cleanly.
	res = resolvePiRuntimeWith(runtimeResolveOptions{
		Requested:   "pi",
		DataDir:     dataDir,
		SystemProbe: probeWithSystem,
	})
	if res.ActiveSource != RuntimeSourceSystem || res.Path != systemBin || !res.SystemIsNPM || res.SystemVersion != "1.5.2" {
		t.Fatalf("system resolution = %+v", res)
	}

	// 3. Saved CustomPath overrides system Pi discovery.
	savedCustom := writeFakeExecutableWithVersion(t, t.TempDir(), "my-pi", "1.6.0")
	if _, err := SaveRuntimeSettings(dataDir, RuntimeSettings{
		CustomPath: savedCustom,
		MirrorMode: MirrorModeCN,
	}); err != nil {
		t.Fatal(err)
	}
	res = resolvePiRuntimeWith(runtimeResolveOptions{
		Requested:   "pi",
		DataDir:     dataDir,
		SystemProbe: probeWithSystem,
	})
	wantSavedCustom, _ := filepath.EvalSymlinks(savedCustom)
	if res.ActiveSource != RuntimeSourceCustom || res.Path != wantSavedCustom || res.ActiveVersion != "1.6.0" {
		t.Fatalf("saved custom resolution = %+v want %q", res, wantSavedCustom)
	}

	// 4. Explicit CLI flag overrides saved CustomPath.
	cliCustom := writeFakeExecutableWithVersion(t, t.TempDir(), "cli-pi", "1.6.2")
	res = resolvePiRuntimeWith(runtimeResolveOptions{
		Requested:   cliCustom,
		DataDir:     dataDir,
		SystemProbe: probeWithSystem,
	})
	wantCLICustom, _ := filepath.EvalSymlinks(cliCustom)
	if res.ActiveSource != RuntimeSourceCustom || res.Path != wantCLICustom || res.ActiveVersion != "1.6.2" {
		t.Fatalf("explicit override resolution = %+v want %q", res, wantCLICustom)
	}
}

func TestRuntimeSettingsValidationLegacyCompatAndRoundtrip(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")

	// Reject nonexistent custom executable path.
	if _, err := SaveRuntimeSettings(dataDir, RuntimeSettings{
		CustomPath: filepath.Join(t.TempDir(), "does-not-exist"),
		MirrorMode: MirrorModeAuto,
	}); err == nil {
		t.Fatal("expected nonexistent customPath error")
	}

	// Reject invalid registry URL scheme.
	if _, err := SaveRuntimeSettings(dataDir, RuntimeSettings{
		MirrorMode:  MirrorModeCN,
		NPMRegistry: "ftp://invalid.local",
	}); err == nil {
		t.Fatal("expected invalid NPMRegistry scheme error")
	}

	customPi := writeFakeExecutableWithVersion(t, t.TempDir(), "pi", "1.6.0")
	saved, err := SaveRuntimeSettings(dataDir, RuntimeSettings{
		CustomPath:  customPi,
		MirrorMode:  MirrorModeCN,
		NPMRegistry: "https://registry.npmmirror.com/",
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded := LoadRuntimeSettings(dataDir)
	if loaded != saved || loaded.NPMRegistry != "https://registry.npmmirror.com" {
		t.Fatalf("loaded settings mismatch: %+v vs %+v", loaded, saved)
	}

	// Ignore legacy v0.1.32 keys cleanly when loading pi-runtime.json.
	legacyJSON := `{"mode":"managed","mirrorMode":"direct","githubProxy":"https://gh-proxy.org/","npmRegistry":"https://registry.npmjs.org"}`
	if err := os.WriteFile(runtimeSettingsPath(dataDir), []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyLoaded := LoadRuntimeSettings(dataDir)
	if legacyLoaded.MirrorMode != MirrorModeDirect || legacyLoaded.NPMRegistry != "https://registry.npmjs.org" {
		t.Fatalf("legacy settings parse failed: %+v", legacyLoaded)
	}
}
