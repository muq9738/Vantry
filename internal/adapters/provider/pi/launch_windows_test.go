//go:build windows

package pi

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWindowsNpmPiShimUsesNodeAndLiteralArguments(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "npm & user")
	if err := os.MkdirAll(filepath.Join(folder, "node_modules", "@earendil-works", "pi-coding-agent", "dist"), 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(folder, "node_modules", "@earendil-works", "pi-coding-agent", "dist", "cli.js")
	if err := os.WriteFile(script, []byte("// fixture only"), 0600); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(folder, "node.exe")
	if err := os.WriteFile(node, []byte("fixture not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(folder, "pi.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\nSET dp0=%~dp0\r\n\"%dp0%\\node.exe\" \"%dp0%\\node_modules\\@earendil-works\\pi-coding-agent\\dist\\cli.js\" %*\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--mode", "rpc", "literal & not a shell command", "a%PATH%b"}
	executable, got, err := piLaunchArguments(shim, args)
	if err != nil {
		t.Fatal(err)
	}
	if executable != node || !reflect.DeepEqual(got, append([]string{script}, args...)) {
		t.Fatalf("npm launch was not direct Node: %q %q", executable, got)
	}
}
func TestWindowsPiLaunchRejectsUnknownBatchWrapper(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "pi.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\necho arbitrary wrapper\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := piLaunchArguments(shim, nil); err == nil {
		t.Fatal("unknown batch wrapper was handed to CreateProcess")
	}
}
func TestWindowsPiExecutableDoesNotAcquireShellSemantics(t *testing.T) {
	args := []string{"--mode", "rpc"}
	binary := `C:\Program Files\Pi\pi.exe`
	got, actual, err := piLaunchArguments(binary, args)
	if err != nil || got != binary || !reflect.DeepEqual(actual, args) {
		t.Fatal("native executable was rewritten")
	}
}
