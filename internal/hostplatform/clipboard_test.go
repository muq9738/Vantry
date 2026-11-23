package hostplatform

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pngTestData is a PNG signature followed by opaque bytes: the reader validates
// the container, not the whole image.
func pngTestData() []byte {
	data := make([]byte, 0, len(pngSignature)+4)
	data = append(data, pngSignature...)
	return append(data, 0x00, 0x00, 0x00, 0x0d)
}

func clipboardTestDeps(t *testing.T, goos string, run commandRunner, lookPath func(string) (string, error)) (clipboardDeps, string) {
	t.Helper()
	root := t.TempDir()
	if run == nil {
		run = func(context.Context, CommandSpec) (commandOutput, error) {
			t.Fatal("no native command expected on this path")
			return commandOutput{}, nil
		}
	}
	if lookPath == nil {
		lookPath = func(name string) (string, error) {
			t.Fatalf("unexpected tool lookup %q", name)
			return "", exec.ErrNotFound
		}
	}
	return clipboardDeps{
		goos:        goos,
		lookPath:    lookPath,
		run:         run,
		makeTempDir: func() (string, error) { return os.MkdirTemp(root, "clipboard-") },
	}, root
}

func assertEmptyDirectory(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("clipboard temporary files survived the read: %v", entries)
	}
}

func TestReadClipboardImageDarwinWritesManagedTempFile(t *testing.T) {
	var (
		seen   CommandSpec
		target string
	)
	run := func(_ context.Context, spec CommandSpec) (commandOutput, error) {
		seen = spec
		target = spec.Args[1]
		directory := filepath.Dir(target)
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if permissions := info.Mode().Perm(); permissions != 0o700 {
			t.Fatalf("clipboard temporary directory is %o, want 0700", permissions)
		}
		if err := os.WriteFile(target, pngTestData(), 0o600); err != nil {
			t.Fatal(err)
		}
		return commandOutput{stdout: []byte("written\n")}, nil
	}
	deps, root := clipboardTestDeps(t, "darwin", run, nil)

	data, mimeType, fileName, found, err := readClipboardImage(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if !found || mimeType != "image/png" || fileName != "clipboard.png" {
		t.Fatalf("found=%v mime=%q name=%q", found, mimeType, fileName)
	}
	if !slices.Equal(data, pngTestData()) {
		t.Fatalf("data=%v", data)
	}
	if seen.Name != "/usr/bin/osascript" {
		t.Fatalf("name=%q", seen.Name)
	}
	if !slices.Equal(seen.Args, []string{"-", target}) {
		t.Fatalf("the clipboard path must travel as argv: %v", seen.Args)
	}
	if !strings.Contains(seen.Stdin, "on run argv") || !strings.Contains(seen.Stdin, "PNGf") {
		t.Fatalf("script=%q", seen.Stdin)
	}
	if strings.Contains(seen.Stdin, target) {
		t.Fatalf("clipboard path reached the script source: %q", seen.Stdin)
	}
	if !strings.HasPrefix(target, root) {
		t.Fatalf("clipboard file %q escaped the managed directory %q", target, root)
	}
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("clipboard file was not removed after reading: %v", err)
	}
	assertEmptyDirectory(t, root)
}

func TestReadClipboardImageDarwinReportsTextClipboardAsNotFound(t *testing.T) {
	run := func(context.Context, CommandSpec) (commandOutput, error) {
		return commandOutput{stdout: []byte("none\n")}, nil
	}
	deps, root := clipboardTestDeps(t, "darwin", run, nil)

	data, mimeType, fileName, found, err := readClipboardImage(context.Background(), deps)
	if err != nil {
		t.Fatalf("a text clipboard is not an error: %v", err)
	}
	if found || data != nil || mimeType != "" || fileName != "" {
		t.Fatalf("found=%v data=%v mime=%q name=%q", found, data, mimeType, fileName)
	}
	assertEmptyDirectory(t, root)
}

func TestReadClipboardImageDarwinRejectsInvalidPNGMagic(t *testing.T) {
	run := func(_ context.Context, spec CommandSpec) (commandOutput, error) {
		if err := os.WriteFile(spec.Args[1], []byte("plain text, not a png"), 0o600); err != nil {
			t.Fatal(err)
		}
		return commandOutput{stdout: []byte("written")}, nil
	}
	deps, root := clipboardTestDeps(t, "darwin", run, nil)

	if _, _, _, found, err := readClipboardImage(context.Background(), deps); !errors.Is(err, errClipboardImageNotPNG) {
		t.Fatalf("found=%v err=%v", found, err)
	}
	assertEmptyDirectory(t, root)
}

func TestReadClipboardImageDarwinRejectsOversizedImage(t *testing.T) {
	run := func(_ context.Context, spec CommandSpec) (commandOutput, error) {
		file, err := os.OpenFile(spec.Args[1], os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		// A sparse file keeps the test fast: the reader must reject it from the
		// reported size, before any byte is read.
		if _, err := file.WriteAt(pngTestData(), 0); err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxClipboardImageSize + 1); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return commandOutput{stdout: []byte("written")}, nil
	}
	deps, root := clipboardTestDeps(t, "darwin", run, nil)

	if _, _, _, found, err := readClipboardImage(context.Background(), deps); !errors.Is(err, errClipboardImageTooLarge) {
		t.Fatalf("found=%v err=%v", found, err)
	}
	assertEmptyDirectory(t, root)
}

func TestReadClipboardImageDarwinReportsWriteFailure(t *testing.T) {
	run := func(context.Context, CommandSpec) (commandOutput, error) {
		return commandOutput{stdout: []byte("failed")}, nil
	}
	deps, _ := clipboardTestDeps(t, "darwin", run, nil)
	if _, _, _, found, err := readClipboardImage(context.Background(), deps); err == nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestReadClipboardImageLinuxReadsWaylandClipboard(t *testing.T) {
	run := func(_ context.Context, spec CommandSpec) (commandOutput, error) {
		if spec.Name != "/fake/wl-paste" {
			t.Fatalf("unexpected command %q", spec.Name)
		}
		if !slices.Equal(spec.Args, []string{"--type", "image/png", "--no-newline"}) {
			t.Fatalf("args=%v", spec.Args)
		}
		return commandOutput{stdout: pngTestData()}, nil
	}
	lookPath := func(name string) (string, error) { return "/fake/" + name, nil }
	deps, _ := clipboardTestDeps(t, "linux", run, lookPath)

	data, mimeType, fileName, found, err := readClipboardImage(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !slices.Equal(data, pngTestData()) || mimeType != "image/png" || fileName != "clipboard.png" {
		t.Fatalf("found=%v mime=%q name=%q data=%v", found, mimeType, fileName, data)
	}
}

func TestReadClipboardImageLinuxFallsBackToXclip(t *testing.T) {
	run := func(_ context.Context, spec CommandSpec) (commandOutput, error) {
		switch spec.Name {
		case "/fake/wl-paste":
			return commandOutput{}, errors.New("exit status 1")
		case "/fake/xclip":
			if !slices.Equal(spec.Args, []string{"-selection", "clipboard", "-target", "image/png", "-out"}) {
				t.Fatalf("args=%v", spec.Args)
			}
			return commandOutput{stdout: pngTestData()}, nil
		default:
			t.Fatalf("unexpected command %q", spec.Name)
			return commandOutput{}, nil
		}
	}
	lookPath := func(name string) (string, error) { return "/fake/" + name, nil }
	deps, _ := clipboardTestDeps(t, "linux", run, lookPath)

	if _, _, _, found, err := readClipboardImage(context.Background(), deps); err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestReadClipboardImageLinuxDegradesWithoutTools(t *testing.T) {
	deps, _ := clipboardTestDeps(t, "linux", nil, func(string) (string, error) {
		return "", exec.ErrNotFound
	})
	data, mimeType, fileName, found, err := readClipboardImage(context.Background(), deps)
	if err != nil {
		t.Fatalf("a host without clipboard tools is not an error: %v", err)
	}
	if found || data != nil || mimeType != "" || fileName != "" {
		t.Fatalf("found=%v data=%v mime=%q name=%q", found, data, mimeType, fileName)
	}
}

func TestReadClipboardImageLinuxRejectsOversizedOutput(t *testing.T) {
	run := func(context.Context, CommandSpec) (commandOutput, error) {
		return commandOutput{stdout: pngTestData(), excess: true}, nil
	}
	lookPath := func(name string) (string, error) { return "/fake/" + name, nil }
	deps, _ := clipboardTestDeps(t, "linux", run, lookPath)

	if _, _, _, found, err := readClipboardImage(context.Background(), deps); !errors.Is(err, errClipboardImageTooLarge) {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestReadClipboardImageLinuxRejectsNonPNGOutput(t *testing.T) {
	run := func(context.Context, CommandSpec) (commandOutput, error) {
		return commandOutput{stdout: []byte("clipboard text")}, nil
	}
	lookPath := func(name string) (string, error) { return "/fake/" + name, nil }
	deps, _ := clipboardTestDeps(t, "linux", run, lookPath)

	if _, _, _, found, err := readClipboardImage(context.Background(), deps); !errors.Is(err, errClipboardImageNotPNG) {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestReadClipboardImageUnsupportedPlatform(t *testing.T) {
	deps, _ := clipboardTestDeps(t, "windows", nil, nil)
	if _, _, _, found, err := readClipboardImage(context.Background(), deps); found || err != nil {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestReadClipboardImageRespectsCancelledContext(t *testing.T) {
	deps, _ := clipboardTestDeps(t, "darwin", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, found, err := readClipboardImage(ctx, deps); !errors.Is(err, context.Canceled) || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestValidateClipboardImage(t *testing.T) {
	valid := pngTestData()
	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{name: "signature-ok", data: valid},
		{name: "empty", data: nil, wantErr: errClipboardImageNotPNG},
		{name: "short", data: pngSignature[:4], wantErr: errClipboardImageNotPNG},
		{name: "wrong-signature", data: []byte("\x89PNG\r\n\x1a\x0b"), wantErr: errClipboardImageNotPNG},
		{name: "text", data: []byte("clipboard"), wantErr: errClipboardImageNotPNG},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateClipboardImage(test.data); !errors.Is(err, test.wantErr) {
				t.Fatalf("err=%v want %v", err, test.wantErr)
			}
		})
	}
}

func TestValidateClipboardImageSize(t *testing.T) {
	tests := []struct {
		size    int64
		wantErr error
	}{
		{size: 0},
		{size: 1},
		{size: maxClipboardImageSize},
		{size: maxClipboardImageSize + 1, wantErr: errClipboardImageTooLarge},
	}
	for _, test := range tests {
		if err := validateClipboardImageSize(test.size); !errors.Is(err, test.wantErr) {
			t.Fatalf("size=%d err=%v want %v", test.size, err, test.wantErr)
		}
	}
}
