package pi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestUpgradeExternalNPMPiUsesDetectedMirrorAndVerifies(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	systemDir := t.TempDir()
	currentVer := "1.5.2"
	nextVer := "1.6.0"

	piRel := filepath.Join("node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "cli.js")
	fakeSystem := writeFakeExecutableWithVersion(t, systemDir, piRel, "")
	pkgJSONPath := filepath.Join(systemDir, "node_modules", "@earendil-works", "pi-coding-agent", "package.json")
	if err := os.WriteFile(pkgJSONPath, []byte(`{"name":"@earendil-works/pi-coding-agent","version":"`+currentVer+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	npmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"` + nextVer + `","engines":{"node":">=22.19.0"}}`))
	}))
	defer npmSrv.Close()

	var recordedNPMCalls [][]string
	var verifiedCalls int
	cnHint := true
	deps := upgradeDeps{
		Client: &http.Client{},
		Mirror: MirrorOptions{
			Mode:        MirrorModeCN,
			DirectNPM:   npmSrv.URL,
			ChinaNPM:    npmSrv.URL,
			ChinaHint:   &cnHint,
			HTTPClient:  &http.Client{},
			BypassCache: true,
		},
		SystemProbe:      func() (string, bool) { return fakeSystem, true },
		NPMProbe:         func(string) string { return "/usr/local/bin/npm" },
		NodeVersionProbe: func(context.Context, string) string { return "22.21.0" },
		NPMRunner: func(_ context.Context, _ string, args []string, _ []string) ([]byte, error) {
			recordedNPMCalls = append(recordedNPMCalls, append([]string(nil), args...))
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "-g") && strings.Contains(joined, "@"+nextVer) {
				_ = os.WriteFile(pkgJSONPath, []byte(`{"name":"@earendil-works/pi-coding-agent","version":"`+nextVer+`"}`), 0o600)
			}
			return []byte("added 1 package"), nil
		},
		ContractVerifier: func(_ context.Context, _ string, expectedVer string) error {
			verifiedCalls++
			if expectedVer != nextVer {
				t.Fatalf("expected verifier version %q, got %q", nextVer, expectedVer)
			}
			return nil
		},
	}

	// First inspectRemote: runs sandbox preflight and unlocks UpdateAvailable + CompatVerified + CanUpgrade.
	st := inspectRuntimeWith(context.Background(), "pi", dataDir, true, 0, deps)
	if !st.UpdateAvailable || !st.CompatVerified || !st.CanUpgrade || st.CompatBlocked {
		t.Fatalf("expected verified upgrade status, got %+v", st)
	}
	if len(recordedNPMCalls) != 1 || !strings.Contains(strings.Join(recordedNPMCalls[0], " "), "--prefix") {
		t.Fatalf("expected isolated --prefix preflight install, calls=%v", recordedNPMCalls)
	}

	// Second call: UpgradePi uses cached sandbox result, runs npm install -g with mirror, and runs live contract check.
	updated, summary, err := upgradePiWith(context.Background(), "pi", dataDir, api.PiUpgradeInput{}, 0, deps)
	if err != nil {
		t.Fatalf("upgradePiWith failed: %v", err)
	}
	if updated.ActiveSource != RuntimeSourceSystem || updated.ActiveVersion != nextVer || !strings.Contains(summary, nextVer) {
		t.Fatalf("unexpected post-upgrade status=%+v summary=%q", updated, summary)
	}
	if len(recordedNPMCalls) != 2 || !strings.Contains(strings.Join(recordedNPMCalls[1], " "), "-g") || !strings.Contains(strings.Join(recordedNPMCalls[1], " "), "--registry="+npmSrv.URL) {
		t.Fatalf("expected global npm install call with mirror, calls=%v", recordedNPMCalls)
	}
	if verifiedCalls != 2 {
		t.Fatalf("expected 2 contract verifications (sandbox + live post-upgrade), got %d", verifiedCalls)
	}
}

func TestUpgradeExternalNPMPiRollsBackWhenPostUpgradeCheckFails(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	systemDir := t.TempDir()
	oldVer := "1.5.2"
	newVer := "1.6.0"

	piRel := filepath.Join("node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "cli.js")
	fakeSystem := writeFakeExecutableWithVersion(t, systemDir, piRel, "")
	pkgJSONPath := filepath.Join(systemDir, "node_modules", "@earendil-works", "pi-coding-agent", "package.json")
	if err := os.WriteFile(pkgJSONPath, []byte(`{"name":"@earendil-works/pi-coding-agent","version":"`+oldVer+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	npmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"` + newVer + `","engines":{"node":">=22.19.0"}}`))
	}))
	defer npmSrv.Close()

	var globalSpecs []string
	callCount := 0
	deps := upgradeDeps{
		Client: &http.Client{},
		Mirror: MirrorOptions{
			Mode:        MirrorModeDirect,
			DirectNPM:   npmSrv.URL,
			BypassCache: true,
		},
		SystemProbe:      func() (string, bool) { return fakeSystem, true },
		NPMProbe:         func(string) string { return "/usr/local/bin/npm" },
		NodeVersionProbe: func(context.Context, string) string { return "22.21.0" },
		NPMRunner: func(_ context.Context, _ string, args []string, _ []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, " -g ") {
				globalSpecs = append(globalSpecs, joined)
				if strings.Contains(joined, "@"+newVer) {
					_ = os.WriteFile(pkgJSONPath, []byte(`{"name":"@earendil-works/pi-coding-agent","version":"`+newVer+`"}`), 0o600)
				} else if strings.Contains(joined, "@"+oldVer) {
					_ = os.WriteFile(pkgJSONPath, []byte(`{"name":"@earendil-works/pi-coding-agent","version":"`+oldVer+`"}`), 0o600)
				}
			}
			return []byte("ok"), nil
		},
		ContractVerifier: func(_ context.Context, piExe, _ string) error {
			callCount++
			if callCount == 1 {
				// Sandbox passes cleanly
				return nil
			}
			// Post-upgrade live check fails -> must roll back!
			return os.ErrInvalid
		},
	}

	_, _, err := upgradePiWith(context.Background(), "pi", dataDir, api.PiUpgradeInput{}, 0, deps)
	if err == nil {
		t.Fatal("expected post-upgrade live verification failure to error and roll back")
	}
	if !strings.Contains(err.Error(), "回滚") || !strings.Contains(err.Error(), oldVer) {
		t.Fatalf("expected rollback message in error, got: %v", err)
	}
	if len(globalSpecs) != 2 || !strings.Contains(globalSpecs[1], "@"+oldVer) {
		t.Fatalf("expected second global install rolling back to %s, got %v", oldVer, globalSpecs)
	}
	if readPiPackageVersion(fakeSystem) != oldVer {
		t.Fatalf("expected package.json rolled back to %s, got %s", oldVer, readPiPackageVersion(fakeSystem))
	}
}

func TestFindSiblingToolsLocateNodeAndNPM(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	npmName, nodeName := "npm", "node"
	if runtime.GOOS == "windows" {
		npmName, nodeName = "npm.cmd", "node.exe"
	}
	for _, n := range []string{npmName, nodeName} {
		if err := os.WriteFile(filepath.Join(binDir, n), []byte("#!/bin/sh\necho v22.21.0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	piStub := filepath.Join(binDir, "pi")
	_ = os.WriteFile(piStub, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "")

	if got := findSiblingNPM(piStub); got != filepath.Join(binDir, npmName) {
		t.Fatalf("findSiblingNPM = %q want %q", got, filepath.Join(binDir, npmName))
	}
	if got := findSiblingNode(piStub); got != filepath.Join(binDir, nodeName) {
		t.Fatalf("findSiblingNode = %q want %q", got, filepath.Join(binDir, nodeName))
	}
}
