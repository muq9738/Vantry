package pi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestCheckPiVersionPolicy(t *testing.T) {
	if err := CheckPiVersionPolicy("1.6.1"); err != nil {
		t.Fatalf("expected 1.6.1 allowed, got %v", err)
	}
	for _, blocked := range []string{"", "0.9.0", "1.7.0-rc.1", "2.0.0"} {
		if err := CheckPiVersionPolicy(blocked); err == nil {
			t.Errorf("expected version %q to be blocked", blocked)
		}
	}
}

func TestCheckNodeVersionSatisfies(t *testing.T) {
	cases := []struct {
		actual     string
		constraint string
		want       bool
	}{
		{"22.21.0", ">=22.19.0", true},
		{"v24.0.0", ">=22.19.0", true},
		{"22.18.9", ">=22.19.0", false},
		{"20.11.0", ">=22.19.0", false},
		{"22.5.0", "^22.0.0", true},
		{"23.0.0", "^22.0.0", false},
		{"20.19.0", "^20.19.0 || >=22.19.0", true},
		{"21.0.0", "^20.19.0 || >=22.19.0", false},
		{"", ">=22.19.0", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got, reason := CheckNodeVersionSatisfies(tc.actual, tc.constraint)
		if got != tc.want {
			t.Errorf("CheckNodeVersionSatisfies(%q, %q) = %v (reason=%q), want %v", tc.actual, tc.constraint, got, reason, tc.want)
		}
	}
}

func TestVerifyPiContractSuccessAndFailureCases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("python3 executable script stub")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}

	writeContractStub := func(ver string, mode string) string {
		path := filepath.Join(t.TempDir(), "pi")
		script := `#!` + python + `
import json, os, sys
if "--version" in sys.argv:
    print("` + ver + `")
    sys.exit(0)
assert os.environ.get("PI_CODING_AGENT_DIR") != os.path.expanduser("~/.pi/agent")
assert "builtin:codemode" in sys.argv
assert "builtin:tool-search" in sys.argv
if "` + mode + `" != "no_ready":
    print(json.dumps({"type":"extension_ui_request","method":"notify","message":"vantry_approval_ready"}), flush=True)
for line in sys.stdin:
    cmd = json.loads(line)
    kind = cmd["type"]
    if "` + mode + `" == "fail_state" and kind == "get_state":
        print(json.dumps({"id": cmd["id"], "type": "response", "success": False, "error": "boom"}), flush=True)
        continue
    print(json.dumps({"id": cmd["id"], "type": "response", "success": True, "data": {}}), flush=True)
`
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}

	okPi := writeContractStub("1.6.1", "ok")
	if err := VerifyPiContract(context.Background(), okPi, "1.6.1"); err != nil {
		t.Fatalf("VerifyPiContract passed stub failed: %v", err)
	}
	if err := VerifyPiContract(context.Background(), okPi, "1.6.2"); err == nil {
		t.Fatal("expected version mismatch to fail VerifyPiContract")
	}
	noReadyPi := writeContractStub("1.6.1", "no_ready")
	if err := VerifyPiContract(context.Background(), noReadyPi, "1.6.1"); err == nil {
		t.Fatal("expected missing vantry_approval_ready to fail VerifyPiContract")
	}
	failStatePi := writeContractStub("1.6.1", "fail_state")
	if err := VerifyPiContract(context.Background(), failStatePi, "1.6.1"); err == nil {
		t.Fatal("expected failed get_state to fail VerifyPiContract")
	}
}

func TestPreflightFourSafetyGatesBlockUnsafeUpgrades(t *testing.T) {
	systemDir := t.TempDir()
	fakeSystem := writeFakeExecutableWithVersion(t, systemDir, filepath.Join("node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "cli.js"), "")
	_ = os.WriteFile(filepath.Join(systemDir, "node_modules", "@earendil-works", "pi-coding-agent", "package.json"), []byte(`{"name":"@earendil-works/pi-coding-agent","version":"1.5.2"}`), 0o600)

	serveMeta := func(ver, enginesNode string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"version":"` + ver + `","engines":{"node":"` + enginesNode + `"}}`))
		}))
	}

	baseDeps := func(srvURL string) upgradeDeps {
		return upgradeDeps{
			Client: &http.Client{},
			Mirror: MirrorOptions{
				Mode:        MirrorModeDirect,
				DirectNPM:   srvURL,
				BypassCache: true,
			},
			SystemProbe:      func() (string, bool) { return fakeSystem, true },
			NPMProbe:         func(string) string { return "/usr/local/bin/npm" },
			NodeVersionProbe: func(context.Context, string) string { return "22.21.0" },
			NPMRunner: func(_ context.Context, _ string, _ []string, _ []string) ([]byte, error) {
				return []byte("installed"), nil
			},
			ContractVerifier: func(context.Context, string, string) error { return nil },
		}
	}

	// Gate 1: Major version bump (2.0.0) must NOT advertise UpdateAvailable or CanUpgrade.
	srvMajor := serveMeta("2.0.0", ">=22.19.0")
	defer srvMajor.Close()
	st := inspectRuntimeWith(context.Background(), "pi", filepath.Join(t.TempDir(), "data"), true, 0, baseDeps(srvMajor.URL))
	if st.UpdateAvailable || st.CanUpgrade || !st.CompatBlocked || !strings.Contains(st.CompatReason, "跨主版本") {
		t.Fatalf("expected Gate 1 to block major update, got %+v", st)
	}

	// Gate 2: Node.js mismatch must NOT advertise UpdateAvailable or CanUpgrade.
	srvNode := serveMeta("1.6.1", ">=24.0.0")
	defer srvNode.Close()
	st = inspectRuntimeWith(context.Background(), "pi", filepath.Join(t.TempDir(), "data"), true, 0, baseDeps(srvNode.URL))
	if st.UpdateAvailable || st.CanUpgrade || !st.CompatBlocked || !strings.Contains(st.CompatReason, "Node.js") {
		t.Fatalf("expected Gate 2 to block node mismatch, got %+v", st)
	}

	// Gate 3: Active runs > 0 must block UpdateAvailable, CanUpgrade, and UpgradePiWithOptions.
	srvOK := serveMeta("1.6.1", ">=22.19.0")
	defer srvOK.Close()
	dataDir := filepath.Join(t.TempDir(), "data")
	st = inspectRuntimeWith(context.Background(), "pi", dataDir, true, 2, baseDeps(srvOK.URL))
	if st.UpdateAvailable || st.CanUpgrade || !st.CompatBlocked || st.ActiveRuns != 2 {
		t.Fatalf("expected Gate 3 to block when activeRuns=2, got %+v", st)
	}
	if _, _, err := upgradePiWith(context.Background(), "pi", dataDir, api.PiUpgradeInput{}, 2, baseDeps(srvOK.URL)); err == nil {
		t.Fatal("expected upgradePiWith to fail when activeRuns=2")
	}

	// Gate 4: Sandbox contract failure blocks UpdateAvailable and caches the block.
	failDeps := baseDeps(srvOK.URL)
	sandboxRuns := 0
	failDeps.ContractVerifier = func(context.Context, string, string) error {
		sandboxRuns++
		return errors.New("broken codemode")
	}
	failDataDir := filepath.Join(t.TempDir(), "data")
	st = inspectRuntimeWith(context.Background(), "pi", failDataDir, true, 0, failDeps)
	if st.UpdateAvailable || st.CanUpgrade || !st.CompatBlocked || !strings.Contains(st.CompatReason, "broken codemode") {
		t.Fatalf("expected Gate 4 to block broken sandbox candidate, got %+v", st)
	}
	// Second remote inspect reads persistent disk cache without rerunning sandbox npm install.
	st2 := inspectRuntimeWith(context.Background(), "pi", failDataDir, true, 0, failDeps)
	if sandboxRuns != 1 || st2.UpdateAvailable || !st2.CompatBlocked {
		t.Fatalf("expected disk preflight cache hit on second check, sandboxRuns=%d st2=%+v", sandboxRuns, st2)
	}
}

func TestVerifyPiContractLiveSystem(t *testing.T) {
	sysPi, _ := findSystemPiExecutable()
	if sysPi == "" {
		t.Skip("system pi CLI not installed")
	}
	ver, err := Version(context.Background(), sysPi)
	if err != nil {
		t.Skipf("system pi version check failed (%v)", err)
	}
	if err := VerifyPiContract(context.Background(), sysPi, ver); err != nil {
		t.Fatalf("live VerifyPiContract failed against system pi %q (%s): %v", sysPi, ver, err)
	}
}
