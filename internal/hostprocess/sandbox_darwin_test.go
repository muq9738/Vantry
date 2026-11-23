//go:build darwin

package hostprocess

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeatbeltProfileBuildAndTempDirs(t *testing.T) {
	profile, err := BuildSeatbeltProfile(SandboxConfig{
		Mode:              SandboxWorkspaceWrite,
		WorkspaceDir:      `/tmp/ws "quoted"`,
		ExtraWritableDirs: []string{"/var/tmp/extra"},
		ProtectGitMeta:    true,
		AllowTempDirs:     true,
		AllowNetwork:      true,
	})
	if err != nil {
		t.Fatalf("BuildSeatbeltProfile: %v", err)
	}
	for _, want := range []string{
		`(subpath "/tmp")`,
		`(subpath "/private/tmp")`,
		`\"quoted\"`,
		`.git/hooks")`,
		`.git/config")`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile missing %q:\n%s", want, profile)
		}
	}
	if _, err := BuildSeatbeltProfile(SandboxConfig{Mode: SandboxWorkspaceWrite}); err == nil {
		t.Fatal("expected error for missing workspace directory")
	}
}

func TestSeatbeltKernelEnforcement(t *testing.T) {
	if !SandboxSupported() {
		t.Skip("/usr/bin/sandbox-exec unavailable")
	}
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	hooks := filepath.Join(ws, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "readable.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := SandboxConfig{
		Mode:           SandboxWorkspaceWrite,
		WorkspaceDir:   ws,
		ProtectGitMeta: true,
		AllowTempDirs:  false,
		AllowNetwork:   true,
	}

	runSandboxed := func(shCmd string) error {
		exe, args, err := WrapSandboxArgs(cfg, "/bin/sh", []string{"-c", shCmd})
		if err != nil {
			t.Fatalf("WrapSandboxArgs: %v", err)
		}
		cmd := exec.Command(exe, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("cmd %q -> %v: %s", shCmd, err, string(out))
		}
		return err
	}

	// Writing inside workspace succeeds.
	if err := runSandboxed(`echo ok > "` + filepath.Join(ws, "hello.txt") + `"`); err != nil {
		t.Fatalf("write inside workspace failed unexpectedly: %v", err)
	}
	// Reading outside workspace succeeds.
	if err := runSandboxed(`cat "` + filepath.Join(outside, "readable.txt") + `" >/dev/null`); err != nil {
		t.Fatalf("read outside workspace failed unexpectedly: %v", err)
	}
	// Writing outside workspace is denied by Seatbelt.
	if err := runSandboxed(`echo no > "` + filepath.Join(outside, "evil.txt") + `"`); err == nil {
		t.Fatal("expected write outside workspace to fail under Seatbelt")
	}
	// Writing to .git/hooks and .git/config is denied by Seatbelt last-rule precedence.
	if err := runSandboxed(`echo no > "` + filepath.Join(hooks, "pre-commit") + `"`); err == nil {
		t.Fatal("expected write to .git/hooks to fail under Seatbelt")
	}
	if err := runSandboxed(`echo no > "` + filepath.Join(ws, ".git", "config") + `"`); err == nil {
		t.Fatal("expected write to .git/config to fail under Seatbelt")
	}
}
