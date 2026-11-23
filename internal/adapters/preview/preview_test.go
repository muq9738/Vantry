package preview

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngBytes builds a real PNG so inlining and screenshot checks use valid data.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 1, 1))
	picture.Set(0, 0, color.RGBA{R: 0x2a, G: 0x6f, B: 0xdb, A: 0xff})
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// previewWorkspace creates a workspace with one image asset.
func previewWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "logo.png"), pngBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// resolved returns the workspace root with symlinks removed, matching what the
// package reports for preview paths on macOS temp directories.
func resolved(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func TestPrepareStandaloneHTMLInlinesWorkspaceImages(t *testing.T) {
	root := previewWorkspace(t)
	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"><rect width="8" height="8"/></svg>`
	if err := os.WriteFile(filepath.Join(root, "assets", "mark.svg"), []byte(svg), 0o600); err != nil {
		t.Fatal(err)
	}
	document := `<!doctype html><html><head><title>卡片</title></head><body>` +
		`<img src="assets/logo.png" alt="logo">` +
		`<img src='./assets/logo.png'>` +
		`<img src="assets/logo.png?v=2#top">` +
		`<IMG SRC=assets/logo.png>` +
		`<img src="assets/mark.svg">` +
		`</body></html>`

	prepared, warnings, err := PrepareStandaloneHTML(root, document, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	pngURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes(t))
	if got := strings.Count(prepared, pngURI); got != 4 {
		t.Fatalf("inlined png count = %d, want 4: %s", got, prepared)
	}
	if !strings.Contains(prepared, "data:image/svg+xml;base64,") {
		t.Fatalf("svg not inlined: %s", prepared)
	}
	if !strings.Contains(prepared, "Content-Security-Policy") || !strings.Contains(prepared, sandboxPolicy) {
		t.Fatalf("policy missing: %s", prepared)
	}
	if !strings.HasPrefix(prepared, "<!doctype html><html><head><meta http-equiv=") {
		t.Fatalf("policy must be the first head element: %s", prepared)
	}
	if strings.Contains(prepared, "src=\"assets/logo.png\"") {
		t.Fatalf("workspace source left in place: %s", prepared)
	}
}

func TestPrepareStandaloneHTMLAllowsAbsoluteWorkspaceImage(t *testing.T) {
	root := previewWorkspace(t)
	absolute := filepath.Join(root, "assets", "logo.png")
	prepared, warnings, err := PrepareStandaloneHTML(root, `<img src="`+absolute+`">`, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if !strings.Contains(prepared, "data:image/png;base64,") {
		t.Fatalf("absolute workspace image not inlined: %s", prepared)
	}
}

func TestPrepareStandaloneHTMLRejectsImagesOutsideWorkspace(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.png")
	if err := os.WriteFile(secret, pngBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	root := previewWorkspace(t)
	if err := os.Symlink(secret, filepath.Join(root, "escape.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "assets", "logo.png"), filepath.Join(root, "alias.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		src   string
		match string
	}{
		{name: "parent traversal", src: "../outside/secret.png", match: "越出当前工作区"},
		{name: "absolute outside", src: secret, match: "不在当前工作区内"},
		{name: "file url outside", src: "file://" + secret, match: "不在当前工作区内"},
		{name: "symlink file", src: "escape.png", match: "符号链接"},
		{name: "symlink directory", src: "escape-dir/secret.png", match: "符号链接"},
		{name: "symlink inside workspace", src: "alias.png", match: "符号链接"},
		{name: "unsupported type", src: "note.txt", match: "不是支持的图片类型"},
		{name: "directory", src: "folder", match: "普通文件"},
		{name: "missing file", src: "assets/missing.png", match: "不存在"},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := `<img src="` + test.src + `">`
			prepared, warnings, err := PrepareStandaloneHTML(root, document, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(prepared, "base64") {
				t.Fatalf("leaked image content: %s", prepared)
			}
			if !strings.Contains(prepared, `<img src="`+test.src+`">`) {
				t.Fatalf("source rewritten unexpectedly: %s", prepared)
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want 1", warnings)
			}
			if !strings.Contains(warnings[0], "跳过图片") || !strings.Contains(warnings[0], test.match) {
				t.Fatalf("warning %q does not mention %q", warnings[0], test.match)
			}
		})
	}
}

func TestPrepareStandaloneHTMLRejectsInvalidDocuments(t *testing.T) {
	root := previewWorkspace(t)
	for _, test := range []struct {
		name     string
		document string
	}{
		{name: "empty", document: "   \n\t "},
		{name: "oversized", document: strings.Repeat("a", MaxHTMLSize+1)},
		{name: "invalid utf8", document: "<p>\xff\xfe</p>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := PrepareStandaloneHTML(root, test.document, RenderOptions{}); err == nil {
				t.Fatal("invalid document accepted")
			}
		})
	}
	if _, _, err := PrepareStandaloneHTML(filepath.Join(root, "missing"), "<p>ok</p>", RenderOptions{}); err == nil {
		t.Fatal("missing workspace accepted")
	}
	file := filepath.Join(root, "note.txt")
	if err := os.WriteFile(file, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PrepareStandaloneHTML(file, "<p>ok</p>", RenderOptions{}); err == nil {
		t.Fatal("file workspace accepted")
	}
}

func TestPrepareStandaloneHTMLBlocksRemoteResources(t *testing.T) {
	root := previewWorkspace(t)
	document := `<html><head></head><body><img src="https://example.com/tracker.png"></body></html>`
	prepared, warnings, err := PrepareStandaloneHTML(root, document, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "远程图片") {
		t.Fatalf("warnings = %v", warnings)
	}
	if !strings.Contains(prepared, "https://example.com/tracker.png") {
		t.Fatal("remote source should stay untouched")
	}
	if !strings.Contains(prepared, "default-src 'none'") {
		t.Fatalf("network policy missing: %s", prepared)
	}

	allowed, warnings, err := PrepareStandaloneHTML(root, document, RenderOptions{AllowRemoteResources: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if strings.Contains(allowed, "Content-Security-Policy") {
		t.Fatalf("policy should be skipped when remote resources are allowed: %s", allowed)
	}
}

func TestPrepareStandaloneHTMLKeepsAuthorPolicy(t *testing.T) {
	root := previewWorkspace(t)
	document := `<html><head><meta http-equiv="Content-Security-Policy" content="default-src 'self'"></head><body></body></html>`
	prepared, warnings, err := PrepareStandaloneHTML(root, document, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.ToLower(prepared), "content-security-policy"); got != 1 {
		t.Fatalf("policy count = %d, want 1: %s", got, prepared)
	}
	found := false
	for _, warning := range warnings {
		if strings.Contains(warning, "保留原策略") {
			found = true
		}
	}
	if !found {
		t.Fatalf("author policy warning missing: %v", warnings)
	}
}

// fakeScreenshotter stands in for a local Chromium kernel.
type fakeScreenshotter struct {
	calls             int
	htmlPath, pngPath string
	width, height     int
	err               error
	payload           []byte
	skipArtifact      bool
}

func (f *fakeScreenshotter) Screenshot(_ context.Context, htmlPath, pngPath string, width, height int) error {
	f.calls++
	f.htmlPath, f.pngPath = htmlPath, pngPath
	f.width, f.height = width, height
	if f.err != nil {
		return f.err
	}
	if f.skipArtifact {
		return nil
	}
	return os.WriteFile(pngPath, f.payload, 0o600)
}

func TestRenderHTMLWritesSnapshotWithInjectedRenderer(t *testing.T) {
	root := previewWorkspace(t)
	fake := &fakeScreenshotter{payload: pngBytes(t)}
	renderer := Renderer{Screenshotter: fake}
	document := `<html><head><title>日报</title></head><body><img src="assets/logo.png"></body></html>`

	visual, err := renderer.RenderHTML(context.Background(), root, "daily report", document,
		RenderOptions{ViewportWidth: 1440, ViewportHeight: 900})
	if err != nil {
		t.Fatal(err)
	}
	if visual.HTMLPath != ".agent/previews/daily-report.html" {
		t.Fatalf("HTMLPath = %q", visual.HTMLPath)
	}
	if visual.PNGPath != ".agent/previews/daily-report.png" {
		t.Fatalf("PNGPath = %q", visual.PNGPath)
	}
	if visual.Markdown != "![preview](.agent/previews/daily-report.png)" {
		t.Fatalf("Markdown = %q", visual.Markdown)
	}
	if visual.HTMLFile != filepath.Join(resolved(t, root), ".agent", "previews", "daily-report.html") {
		t.Fatalf("HTMLFile = %q", visual.HTMLFile)
	}
	if fake.calls != 1 {
		t.Fatalf("screenshot calls = %d", fake.calls)
	}
	if fake.width != 1440 || fake.height != 900 {
		t.Fatalf("viewport = %dx%d", fake.width, fake.height)
	}
	if fake.htmlPath != visual.HTMLFile {
		t.Fatalf("renderer html path = %q", fake.htmlPath)
	}
	if fake.pngPath != filepath.Join(filepath.Dir(fake.htmlPath), "daily-report.png") {
		t.Fatalf("renderer png path = %q", fake.pngPath)
	}
	renamed := false
	for _, warning := range visual.Warnings {
		if strings.Contains(warning, "规范化为") {
			renamed = true
		}
	}
	if !renamed {
		t.Fatalf("rename warning missing: %v", visual.Warnings)
	}

	saved, err := os.ReadFile(visual.HTMLFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "data:image/png;base64,") {
		t.Fatalf("saved document is not self-contained: %s", saved)
	}
	if !strings.Contains(string(saved), "Content-Security-Policy") {
		t.Fatalf("saved document lost the policy: %s", saved)
	}
	info, err := os.Stat(visual.HTMLFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("snapshot permissions = %v", info.Mode().Perm())
	}
	directory, err := os.Stat(filepath.Dir(visual.HTMLFile))
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0o700 {
		t.Fatalf("preview directory permissions = %v", directory.Mode().Perm())
	}
	captured, err := os.ReadFile(filepath.Join(resolved(t, root), ".agent", "previews", "daily-report.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(captured, pngSignature) {
		t.Fatal("captured file is not a PNG")
	}
}

func TestRenderHTMLToPNGUsesDefaultRenderer(t *testing.T) {
	root := previewWorkspace(t)
	fake := &fakeScreenshotter{payload: pngBytes(t)}
	original := DefaultRenderer
	DefaultRenderer = Renderer{Screenshotter: fake}
	t.Cleanup(func() { DefaultRenderer = original })

	visual, err := RenderHTMLToPNG(context.Background(), root, "card", "<p>hi</p>", 640, 480)
	if err != nil {
		t.Fatal(err)
	}
	if visual.PNGPath != ".agent/previews/card.png" {
		t.Fatalf("PNGPath = %q", visual.PNGPath)
	}
	if fake.width != 640 || fake.height != 480 {
		t.Fatalf("viewport = %dx%d", fake.width, fake.height)
	}
}

func TestRenderHTMLDegradesWithoutHeadlessBrowser(t *testing.T) {
	root := previewWorkspace(t)
	original := discoverBrowser
	discoverBrowser = func() (Browser, bool) { return Browser{}, false }
	t.Cleanup(func() { discoverBrowser = original })

	visual, err := Renderer{}.RenderHTML(context.Background(), root, "card", "<p>hi</p>", RenderOptions{})
	if err != nil {
		t.Fatalf("missing browser must degrade, not fail: %v", err)
	}
	if visual.PNGPath != "" || visual.Markdown != "" {
		t.Fatalf("unexpected snapshot: %+v", visual)
	}
	if visual.HTMLPath != ".agent/previews/card.html" {
		t.Fatalf("HTMLPath = %q", visual.HTMLPath)
	}
	if _, err := os.Stat(visual.HTMLFile); err != nil {
		t.Fatalf("html snapshot missing: %v", err)
	}
	warned := false
	for _, warning := range visual.Warnings {
		if strings.Contains(warning, "无头浏览器") && strings.Contains(warning, "OpenInBrowser") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("degradation warning missing: %v", visual.Warnings)
	}
}

func TestRenderHTMLReportsSnapshotFailures(t *testing.T) {
	root := previewWorkspace(t)
	for _, test := range []struct {
		name      string
		renderer  Screenshotter
		substring string
	}{
		{name: "renderer error", renderer: &fakeScreenshotter{err: errors.New("boom")}, substring: "boom"},
		{name: "missing artifact", renderer: &fakeScreenshotter{skipArtifact: true}, substring: "预览"},
	} {
		t.Run(test.name, func(t *testing.T) {
			visual, err := Renderer{Screenshotter: test.renderer}.RenderHTML(context.Background(), root, test.name, "<p>hi</p>", RenderOptions{})
			if err == nil {
				t.Fatal("snapshot failure was swallowed")
			}
			if !strings.Contains(err.Error(), test.substring) {
				t.Fatalf("error %q does not mention %q", err, test.substring)
			}
			if visual == nil || visual.HTMLPath == "" || visual.PNGPath != "" {
				t.Fatalf("visual = %+v, want saved html without png", visual)
			}
			if _, statErr := os.Stat(visual.HTMLFile); statErr != nil {
				t.Fatalf("html snapshot missing: %v", statErr)
			}
		})
	}
}

func TestRenderHTMLDoesNotFollowExistingSymlinks(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := previewWorkspace(t)
	previewDir := filepath.Join(root, ".agent", "previews")
	if err := os.MkdirAll(previewDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(previewDir, "card.html")); err != nil {
		t.Fatal(err)
	}
	pwned := filepath.Join(outside, "pwned.png")
	if err := os.Symlink(pwned, filepath.Join(previewDir, "card.png")); err != nil {
		t.Fatal(err)
	}

	visual, err := Renderer{Screenshotter: &fakeScreenshotter{payload: pngBytes(t)}}.
		RenderHTML(context.Background(), root, "card", "<p>hi</p>", RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Fatalf("symlinked victim overwritten: %q", content)
	}
	if _, err := os.Stat(pwned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("screenshot escaped the workspace: %v", err)
	}
	info, err := os.Lstat(visual.HTMLFile)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("snapshot is not a regular file: %v", info.Mode())
	}
}

func TestRenderHTMLRejectsSymlinkedPreviewDirectory(t *testing.T) {
	root := previewWorkspace(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".agent")); err != nil {
		t.Fatal(err)
	}
	_, err := Renderer{Screenshotter: &fakeScreenshotter{payload: pngBytes(t)}}.
		RenderHTML(context.Background(), root, "card", "<p>hi</p>", RenderOptions{})
	if err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("symlinked preview directory accepted: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("files written outside the workspace: %v", entries)
	}
}

func TestRenderHTMLRequiresUsableWorkspace(t *testing.T) {
	root := previewWorkspace(t)
	renderer := Renderer{Screenshotter: &fakeScreenshotter{payload: pngBytes(t)}}
	if _, err := renderer.RenderHTML(context.Background(), "", "card", "<p>hi</p>", RenderOptions{}); err == nil {
		t.Fatal("empty workspace accepted")
	}
	if _, err := renderer.RenderHTML(context.Background(), filepath.Join(root, "missing"), "card", "<p>hi</p>", RenderOptions{}); err == nil {
		t.Fatal("missing workspace accepted")
	}
	if _, err := renderer.RenderHTML(context.Background(), root, "card", "", RenderOptions{}); err == nil {
		t.Fatal("empty document accepted")
	}
}

func TestSanitizePreviewNameKeepsFilesInsidePreviewDirectory(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain", input: "card", want: "card"},
		{name: "html suffix", input: "card.html", want: "card"},
		{name: "spaces", input: "daily report", want: "daily-report"},
		{name: "traversal", input: "../../etc/passwd", want: "etc-passwd"},
		{name: "separators", input: "a/b\\c", want: "a-b-c"},
		{name: "empty", input: "  ", want: "preview"},
		{name: "glyphs", input: "报告", want: "preview"},
		{name: "long", input: strings.Repeat("x", MaxPreviewNameLength+20), want: strings.Repeat("x", MaxPreviewNameLength)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _ := sanitizePreviewName(test.input)
			if got != test.want {
				t.Fatalf("sanitizePreviewName(%q) = %q, want %q", test.input, got, test.want)
			}
			if filepath.Base(got) != got || strings.ContainsAny(got, `/\`) {
				t.Fatalf("unsafe file name %q", got)
			}
		})
	}
}

func TestDiscoverBrowserHelpers(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "fake-browser")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(directory, "plain.txt")
	if err := os.WriteFile(plain, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, ok := usableExecutable(executable); !ok || path != executable {
		t.Fatalf("executable rejected: %q %v", path, ok)
	}
	if _, ok := usableExecutable(plain); ok {
		t.Fatal("non-executable accepted")
	}
	if _, ok := usableExecutable(directory); ok {
		t.Fatal("directory accepted")
	}
	if _, ok := usableExecutable(filepath.Join(directory, "missing")); ok {
		t.Fatal("missing path accepted")
	}
	if len(browserCandidates()) == 0 {
		t.Fatal("browser candidates must not be empty")
	}
	if err := (Browser{}).Screenshot(context.Background(), "a.html", "a.png", 800, 600); err == nil {
		t.Fatal("empty browser accepted")
	}

	t.Setenv("VANTRY_BROWSER_BIN", executable)
	browser, ok := DiscoverBrowser()
	if !ok || browser.Path != executable {
		t.Fatalf("environment override ignored: %+v %v", browser, ok)
	}
}

func TestOpenInBrowserRejectsNonSnapshots(t *testing.T) {
	root := previewWorkspace(t)
	notes := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(notes, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", filepath.Join(root, "missing.html"), notes, root} {
		if err := OpenInBrowser(context.Background(), path); err == nil {
			t.Fatalf("OpenInBrowser(%q) accepted", path)
		}
	}
}
