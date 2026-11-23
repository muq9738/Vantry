//go:build darwin

package hostprocess

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const seatbeltExecutable = "/usr/bin/sandbox-exec"

func SandboxSupported() bool {
	info, err := os.Stat(seatbeltExecutable)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func BuildSeatbeltProfile(cfg SandboxConfig) (string, error) {
	if strings.TrimSpace(cfg.WorkspaceDir) == "" {
		return "", errors.New("sandbox workspace directory is required")
	}
	allowPaths := make([]string, 0, len(cfg.ExtraWritableDirs)+6)
	seen := make(map[string]bool)
	addPath := func(raw string) error {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil
		}
		if strings.ContainsAny(trimmed, "\x00\r\n") {
			return fmt.Errorf("invalid control character in sandbox path %q", raw)
		}
		abs, err := filepath.Abs(trimmed)
		if err != nil {
			abs = filepath.Clean(trimmed)
		}
		if !seen[abs] {
			seen[abs] = true
			allowPaths = append(allowPaths, abs)
		}
		if resolved := resolvePathWithParentSymlinks(abs); resolved != "" && !seen[resolved] {
			seen[resolved] = true
			allowPaths = append(allowPaths, resolved)
		}
		return nil
	}

	if err := addPath(cfg.WorkspaceDir); err != nil {
		return "", err
	}
	for _, extra := range cfg.ExtraWritableDirs {
		if err := addPath(extra); err != nil {
			return "", err
		}
	}
	if cfg.AllowTempDirs {
		_ = addPath("/tmp")
		_ = addPath("/private/tmp")
		if tmp := os.TempDir(); tmp != "" {
			_ = addPath(tmp)
		}
	}

	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")
	b.WriteString("(deny file-write*)\n")
	b.WriteString("(allow file-write*\n")
	b.WriteString(`  (literal "/dev/null")` + "\n")
	b.WriteString(`  (literal "/dev/zero")` + "\n")
	b.WriteString(`  (literal "/dev/tty")` + "\n")
	b.WriteString(`  (literal "/dev/dtracehelper")` + "\n")
	b.WriteString(`  (regex #"^/dev/fd/")` + "\n")
	b.WriteString(`  (regex #"^/dev/tty")` + "\n")
	for _, p := range allowPaths {
		b.WriteString(`  (subpath "` + escapeSBPLString(p) + `")` + "\n")
	}
	b.WriteString(")\n")

	if cfg.ProtectGitMeta {
		wsVariants := pathVariants(cfg.WorkspaceDir)
		var denyRules []string
		denySeen := make(map[string]bool)
		addSubpath := func(p string) {
			rule := `  (subpath "` + escapeSBPLString(p) + `")`
			if !denySeen[rule] {
				denySeen[rule] = true
				denyRules = append(denyRules, rule)
			}
		}
		addLiteral := func(p string) {
			rule := `  (literal "` + escapeSBPLString(p) + `")`
			if !denySeen[rule] {
				denySeen[rule] = true
				denyRules = append(denyRules, rule)
			}
		}
		for _, ws := range wsVariants {
			addSubpath(filepath.Join(ws, ".git", "hooks"))
			addLiteral(filepath.Join(ws, ".git", "config"))
			if info, err := os.Lstat(filepath.Join(ws, ".git")); err == nil && info.Mode().IsRegular() {
				addLiteral(filepath.Join(ws, ".git"))
			}
		}
		if len(denyRules) > 0 {
			b.WriteString("(deny file-write*\n")
			for _, rule := range denyRules {
				b.WriteString(rule + "\n")
			}
			b.WriteString(")\n")
		}
	}

	if !cfg.AllowNetwork {
		b.WriteString("(deny network-outbound (remote ip))\n")
	}
	return b.String(), nil
}

func WrapSandboxArgs(cfg SandboxConfig, executable string, args []string) (string, []string, error) {
	if cfg.Mode == "" || cfg.Mode == SandboxOff {
		return executable, args, nil
	}
	if cfg.Mode != SandboxWorkspaceWrite {
		return "", nil, fmt.Errorf("unsupported sandbox mode %q", cfg.Mode)
	}
	if !SandboxSupported() {
		return "", nil, errors.New("macOS Seatbelt sandbox-exec is unavailable")
	}
	profile, err := BuildSeatbeltProfile(cfg)
	if err != nil {
		return "", nil, err
	}
	wrapped := make([]string, 0, len(args)+3)
	wrapped = append(wrapped, "-p", profile, executable)
	wrapped = append(wrapped, args...)
	return seatbeltExecutable, wrapped, nil
}

func pathVariants(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		abs = filepath.Clean(trimmed)
	}
	out := []string{abs}
	if resolved := resolvePathWithParentSymlinks(abs); resolved != "" && resolved != abs {
		out = append(out, resolved)
	}
	return out
}

func resolvePathWithParentSymlinks(abs string) string {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	dir := filepath.Dir(abs)
	base := filepath.Base(abs)
	if resolvedDir, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(resolvedDir, base)
	}
	return ""
}

func escapeSBPLString(value string) string {
	replaced := strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(replaced, `"`, `\"`)
}
