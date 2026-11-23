// Package preview turns Agent-authored HTML into self-contained workspace
// visuals and probes local development servers. Vantry keeps its native MyGo
// timeline and existing reply image path instead of embedding a WebView:
// documents are rewritten into a single safe file, snapshotted by an optional
// local Chromium kernel, and handed back as workspace paths.
package preview

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxHTMLSize bounds one prepared visual. Larger documents are rejected
	// instead of being truncated, because a partial document renders wrong.
	MaxHTMLSize = 2 << 20
	// MaxInlineImageSize bounds a single inlined workspace image.
	MaxInlineImageSize = 4 << 20
	// PreviewDirectory is the workspace-relative output directory. It matches
	// the paths already used by reply.image, so the timeline can show the PNG.
	PreviewDirectory = ".agent/previews"

	// DefaultViewportWidth and DefaultViewportHeight describe a desktop card.
	DefaultViewportWidth  = 1280
	DefaultViewportHeight = 800
	// DefaultRenderTimeout bounds one headless snapshot.
	DefaultRenderTimeout = 30 * time.Second
	// MaxPreviewNameLength bounds the sanitized file name.
	MaxPreviewNameLength = 64

	// screenshotScale captures at 2x for high DPI timelines.
	screenshotScale = 2
)

// sandboxPolicy blocks every network request from a snapshot while keeping
// inline styles, inline scripts and data: images working. A prepared visual is
// a static card; it must not phone home.
const sandboxPolicy = "default-src 'none'; img-src data: blob:; media-src data: blob:; " +
	"style-src 'unsafe-inline' data:; font-src data:; script-src 'unsafe-inline'; " +
	"connect-src 'none'; frame-src 'none'; form-action 'none'; base-uri 'none'"

// imageMediaTypes is the only set of workspace images that may be inlined.
var imageMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".svg":  "image/svg+xml",
	".webp": "image/webp",
}

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// RenderOptions configures one visual snapshot.
type RenderOptions struct {
	// ViewportWidth and ViewportHeight are CSS pixels. Zero uses the defaults;
	// values outside 320..4096 and 200..4096 are clamped.
	ViewportWidth  int
	ViewportHeight int
	// AllowRemoteResources keeps remote http(s) images and skips the network
	// blocking policy. The default (false) means a prepared visual never
	// touches the network.
	AllowRemoteResources bool
}

func (o RenderOptions) viewport() (int, int) {
	width, height := o.ViewportWidth, o.ViewportHeight
	if width <= 0 {
		width = DefaultViewportWidth
	}
	if height <= 0 {
		height = DefaultViewportHeight
	}
	return min(max(width, 320), 4096), min(max(height, 200), 4096)
}

// RenderedVisual describes the files written for one preview.
type RenderedVisual struct {
	// HTMLPath is the workspace-relative, forward-slashed HTML snapshot.
	HTMLPath string
	// HTMLFile is the absolute HTML path for OpenInBrowser.
	HTMLFile string
	// PNGPath is the workspace-relative, forward-slashed PNG snapshot, which a
	// Markdown message can embed directly as ![](PNGPath). Empty when no local
	// headless browser is available.
	PNGPath string
	// Markdown is a ready-to-send image link when PNGPath exists.
	Markdown string
	// Warnings explain skipped resources and degraded snapshots.
	Warnings []string
	// Browser names the headless kernel used, empty when injected or absent.
	Browser string
}

// PrepareStandaloneHTML rewrites an Agent-authored document into one
// self-contained file: workspace images become data URIs and, unless the caller
// allows remote resources, a Content-Security-Policy blocks the network.
//
// A local image is inlined only when its real path stays inside workspaceRoot,
// is a regular file and is a supported image under the size limit. Parent
// traversal, symlinks in relative paths and anything resolved outside the
// project are skipped with a warning, so a model cannot use
// <img src="/etc/passwd"> or a symlink to read files outside the workspace.
func PrepareStandaloneHTML(workspaceRoot string, rawHTML string, opts RenderOptions) (string, []string, error) {
	root, err := canonicalRoot(workspaceRoot)
	if err != nil {
		return "", nil, err
	}
	return prepareStandaloneHTML(root, rawHTML, opts)
}

func prepareStandaloneHTML(root, rawHTML string, opts RenderOptions) (string, []string, error) {
	if strings.TrimSpace(rawHTML) == "" {
		return "", nil, errors.New("HTML 预览内容不能为空")
	}
	if len(rawHTML) > MaxHTMLSize {
		return "", nil, fmt.Errorf("HTML 预览超过 %d KiB 上限", MaxHTMLSize>>10)
	}
	if !utf8.ValidString(rawHTML) {
		return "", nil, errors.New("HTML 预览必须是合法的 UTF-8 文本")
	}
	document, warnings := inlineWorkspaceImages(root, rawHTML, opts.AllowRemoteResources)
	if !opts.AllowRemoteResources {
		if hasContentSecurityPolicy(document) {
			warnings = append(warnings, "HTML 已声明 Content-Security-Policy，保留原策略")
		} else {
			document = injectContentSecurityPolicy(document, sandboxPolicy)
		}
	}
	return document, warnings, nil
}

// inlineWorkspaceImages rewrites every <img src> whose value resolves to a
// workspace image file. Non-image sources are left untouched.
func inlineWorkspaceImages(root, document string, allowRemote bool) (string, []string) {
	var out strings.Builder
	out.Grow(len(document) + 1024)
	var warnings []string
	rest := document
	for {
		start := indexFold(rest, "<img")
		if start < 0 {
			out.WriteString(rest)
			break
		}
		after := start + len("<img")
		if after < len(rest) && !isHTMLSpace(rest[after]) && rest[after] != '/' && rest[after] != '>' {
			out.WriteString(rest[:after])
			rest = rest[after:]
			continue
		}
		end := tagEnd(rest, after)
		if end < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:start])
		out.WriteString(rewriteImageTag(rest[start:end], root, allowRemote, &warnings))
		rest = rest[end:]
	}
	return out.String(), warnings
}

func rewriteImageTag(tag, root string, allowRemote bool, warnings *[]string) string {
	valueStart, valueEnd, ok := findAttribute(tag, "src")
	if !ok {
		return tag
	}
	replacement, inlined, warning := inlineImageSource(root, tag[valueStart:valueEnd], allowRemote)
	if warning != "" {
		*warnings = append(*warnings, warning)
	}
	if !inlined {
		return tag
	}
	return tag[:valueStart] + replacement + tag[valueEnd:]
}

// inlineImageSource reports the data URI for a workspace image. Sources that
// must not be inlined return inlined=false with an optional warning.
func inlineImageSource(root, source string, allowRemote bool) (replacement string, inlined bool, warning string) {
	raw := strings.TrimSpace(html.UnescapeString(source))
	if raw == "" {
		return "", false, ""
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "data:") {
		return "", false, ""
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(raw, "//") {
		if allowRemote {
			return "", false, ""
		}
		return "", false, fmt.Sprintf("跳过远程图片 %q：自包含快照默认不访问网络", source)
	}
	if strings.HasPrefix(raw, "#") || strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "about:") {
		return "", false, fmt.Sprintf("跳过不受支持的图片地址 %q", source)
	}
	local, ok := imageFilePath(raw)
	if !ok {
		return "", false, fmt.Sprintf("跳过不受支持的图片地址 %q", source)
	}
	file, reason := resolveWorkspaceFile(root, local)
	if reason != "" {
		return "", false, fmt.Sprintf("跳过图片 %q：%s", source, reason)
	}
	mediaType, ok := imageMediaTypes[strings.ToLower(filepath.Ext(file))]
	if !ok {
		return "", false, fmt.Sprintf("跳过图片 %q：不是支持的图片类型", source)
	}
	data, err := readBoundedFile(file, MaxInlineImageSize)
	if err != nil {
		return "", false, fmt.Sprintf("跳过图片 %q：%v", source, err)
	}
	if len(data) == 0 {
		return "", false, fmt.Sprintf("跳过图片 %q：文件为空", source)
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), true, ""
}

// imageFilePath extracts the file system path from an image source.
func imageFilePath(raw string) (string, bool) {
	value := raw
	if strings.HasPrefix(strings.ToLower(value), "file:") {
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Host != "" && parsed.Host != "localhost") {
			return "", false
		}
		value = parsed.Path
	} else if parsed, err := url.Parse(value); err == nil {
		if parsed.Scheme != "" {
			return "", false
		}
		value = parsed.Path
	} else if index := strings.IndexAny(value, "?#"); index >= 0 {
		value = value[:index]
	}
	if unescaped, err := url.PathUnescape(value); err == nil {
		value = unescaped
	}
	if value == "" {
		return "", false
	}
	return value, true
}

// resolveWorkspaceFile maps a document path onto a real file inside root. It
// rejects parent traversal, absolute paths outside root and any symlink in the
// resolved chain.
func resolveWorkspaceFile(root, value string) (string, string) {
	var candidate string
	if filepath.IsAbs(filepath.FromSlash(value)) {
		// Absolute sources are resolved first: the workspace itself may sit
		// behind an OS level symlink such as /var -> /private/var, and the
		// containment check must run on the real path.
		absolute, err := filepath.EvalSymlinks(filepath.Clean(filepath.FromSlash(value)))
		if err != nil {
			return "", "图片不存在或无法访问"
		}
		relative, err := filepath.Rel(root, absolute)
		if err != nil || !filepath.IsLocal(relative) {
			return "", "图片不在当前工作区内"
		}
		candidate = filepath.Join(root, relative)
	} else {
		relative := filepath.FromSlash(value)
		if !filepath.IsLocal(relative) {
			return "", "图片路径越出当前工作区"
		}
		candidate = filepath.Join(root, relative)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", "图片不存在或无法访问"
	}
	if resolved != candidate {
		return "", "拒绝内联符号链接图片"
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", "图片不存在或无法访问"
	}
	if !info.Mode().IsRegular() {
		return "", "图片必须是普通文件"
	}
	if info.Size() > MaxInlineImageSize {
		return "", fmt.Sprintf("图片超过 %d KiB 上限", MaxInlineImageSize>>10)
	}
	return resolved, ""
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("无法读取")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, errors.New("无法读取")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("超过 %d KiB 上限", limit>>10)
	}
	return data, nil
}

func hasContentSecurityPolicy(document string) bool {
	return indexFold(document, "content-security-policy") >= 0
}

// injectContentSecurityPolicy places the policy before any rendered content so
// it applies to the whole document.
func injectContentSecurityPolicy(document, policy string) string {
	meta := `<meta http-equiv="Content-Security-Policy" content="` + policy + `">`
	for _, opening := range []string{"<head", "<html"} {
		if start := indexFold(document, opening); start >= 0 {
			if end := tagEnd(document, start+len(opening)); end >= 0 {
				return document[:end] + meta + document[end:]
			}
		}
	}
	if start := indexFold(document, "<!doctype"); start >= 0 {
		if end := tagEnd(document, start+len("<!doctype")); end >= 0 {
			return document[:end] + meta + document[end:]
		}
	}
	return meta + document
}

// findAttribute returns the value span of an attribute inside a start tag.
func findAttribute(tag, want string) (int, int, bool) {
	index := 0
	for index < len(tag) {
		for index < len(tag) && (isHTMLSpace(tag[index]) || tag[index] == '/' || tag[index] == '<') {
			index++
		}
		if index >= len(tag) || tag[index] == '>' {
			break
		}
		nameStart := index
		for index < len(tag) && !isHTMLSpace(tag[index]) && tag[index] != '=' && tag[index] != '>' && tag[index] != '/' {
			index++
		}
		name := tag[nameStart:index]
		for index < len(tag) && isHTMLSpace(tag[index]) {
			index++
		}
		if index >= len(tag) || tag[index] != '=' {
			if strings.EqualFold(name, want) {
				return 0, 0, false
			}
			continue
		}
		index++
		for index < len(tag) && isHTMLSpace(tag[index]) {
			index++
		}
		valueStart := index
		valueEnd := index
		if index < len(tag) && (tag[index] == '"' || tag[index] == '\'') {
			quote := tag[index]
			index++
			valueStart = index
			for index < len(tag) && tag[index] != quote {
				index++
			}
			valueEnd = index
			if index < len(tag) {
				index++
			}
		} else {
			for index < len(tag) && !isHTMLSpace(tag[index]) && tag[index] != '>' {
				index++
			}
			valueEnd = index
		}
		if strings.EqualFold(name, want) {
			return valueStart, valueEnd, true
		}
	}
	return 0, 0, false
}

func isHTMLSpace(character byte) bool {
	switch character {
	case ' ', '\t', '\n', '\r', '\f':
		return true
	}
	return false
}

// indexFold finds substr case-insensitively. The needles used here are short
// ASCII tags and attribute names.
func indexFold(haystack, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	head := needle[0]
	lowerHead, upperHead := asciiLower(head), asciiUpper(head)
	for offset := 0; offset+len(needle) <= len(haystack); offset++ {
		character := haystack[offset]
		if character != lowerHead && character != upperHead {
			continue
		}
		if strings.EqualFold(haystack[offset:offset+len(needle)], needle) {
			return offset
		}
	}
	return -1
}

func asciiLower(character byte) byte {
	if character >= 'A' && character <= 'Z' {
		return character + 'a' - 'A'
	}
	return character
}

func asciiUpper(character byte) byte {
	if character >= 'a' && character <= 'z' {
		return character - ('a' - 'A')
	}
	return character
}

// tagEnd returns the index just past the closing '>' of the tag starting at
// from, honouring quoted attribute values.
func tagEnd(document string, from int) int {
	var quote byte
	for index := from; index < len(document); index++ {
		character := document[index]
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '"', '\'':
			quote = character
		case '>':
			return index + 1
		}
	}
	return -1
}

// canonicalRoot verifies the workspace exists and resolves its real path.
func canonicalRoot(workspaceRoot string) (string, error) {
	if strings.TrimSpace(workspaceRoot) == "" {
		return "", errors.New("工作区路径不能为空")
	}
	absolute, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", errors.New("工作区路径无效")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("工作区不可访问")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("工作区必须是已存在的目录")
	}
	return resolved, nil
}

// previewDirectory creates .agent/previews inside the workspace and refuses to
// follow a symlinked component out of the project.
func previewDirectory(root string) (string, error) {
	handle, err := os.OpenRoot(root)
	if err != nil {
		return "", errors.New("无法打开工作区")
	}
	defer handle.Close()
	for _, component := range []string{".agent", filepath.FromSlash(PreviewDirectory)} {
		info, err := handle.Lstat(component)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("预览目录不能是符号链接")
		}
	}
	if err := handle.MkdirAll(filepath.FromSlash(PreviewDirectory), 0o700); err != nil {
		return "", fmt.Errorf("创建预览目录失败：%w", err)
	}
	directory := filepath.Join(root, filepath.FromSlash(PreviewDirectory))
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", errors.New("预览目录不可访问")
	}
	if resolved != directory {
		return "", errors.New("预览目录不能是符号链接")
	}
	return directory, nil
}

// writePreviewFile writes inside the workspace without following a stale
// symlink left by an earlier run.
func writePreviewFile(root, relative string, data []byte, perm os.FileMode) error {
	handle, err := os.OpenRoot(root)
	if err != nil {
		return errors.New("无法打开工作区")
	}
	defer handle.Close()
	path := filepath.FromSlash(relative)
	if info, err := handle.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := handle.Remove(path); err != nil {
			return err
		}
	}
	return handle.WriteFile(path, data, perm)
}

// clearStalePreview drops a pre-existing symlink or file at relative so an
// external tool cannot be redirected outside the workspace.
func clearStalePreview(root, relative string) error {
	handle, err := os.OpenRoot(root)
	if err != nil {
		return errors.New("无法打开工作区")
	}
	defer handle.Close()
	path := filepath.FromSlash(relative)
	if _, err := handle.Lstat(path); err != nil {
		return nil
	}
	return handle.Remove(path)
}

// sanitizePreviewName maps an Agent-supplied name onto one safe file name. The
// second result reports whether the name changed.
func sanitizePreviewName(name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	lower := strings.ToLower(trimmed)
	for _, suffix := range []string{".html", ".htm", ".png"} {
		if strings.HasSuffix(lower, suffix) {
			trimmed = trimmed[:len(trimmed)-len(suffix)]
			break
		}
	}
	var builder strings.Builder
	previousDash := false
	for _, character := range trimmed {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '.', character == '_':
			builder.WriteRune(character)
			previousDash = false
		default:
			if !previousDash {
				builder.WriteByte('-')
				previousDash = true
			}
		}
	}
	cleaned := strings.Trim(builder.String(), "-._")
	if len(cleaned) > MaxPreviewNameLength {
		cleaned = strings.Trim(cleaned[:MaxPreviewNameLength], "-._")
	}
	if cleaned == "" {
		cleaned = "preview"
	}
	return cleaned, cleaned != trimmed
}

// Screenshotter captures a prepared HTML file into a PNG file. Tests inject a
// fake implementation; production uses a discovered Chromium kernel.
type Screenshotter interface {
	Screenshot(ctx context.Context, htmlPath, pngPath string, width, height int) error
}

// Browser is a local Chromium kernel that supports --headless snapshots.
type Browser struct {
	Path string
	Name string
}

// Screenshot runs the browser with an isolated profile and writes one PNG.
func (b Browser) Screenshot(ctx context.Context, htmlPath, pngPath string, width, height int) error {
	if b.Path == "" {
		return errors.New("未找到可用的无头浏览器")
	}
	profile, err := os.MkdirTemp("", "vantry-preview-profile-")
	if err != nil {
		return errors.New("创建隔离浏览器目录失败")
	}
	defer os.RemoveAll(profile)
	arguments := []string{
		"--headless",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-sync",
		"--disable-background-networking",
		"--disable-default-apps",
		"--disable-component-update",
		"--metrics-recording-only",
		"--mute-audio",
		"--run-all-compositor-stages-before-draw",
		"--user-data-dir=" + profile,
		"--window-size=" + strconv.Itoa(width) + "," + strconv.Itoa(height),
		"--force-device-scale-factor=" + strconv.Itoa(screenshotScale),
		"--screenshot=" + pngPath,
		fileURL(htmlPath),
	}
	command := exec.CommandContext(ctx, b.Path, arguments...)
	command.WaitDelay = 5 * time.Second
	output := &cappedBuffer{limit: 8 << 10}
	command.Stderr = output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("无头浏览器超时：%w", ctx.Err())
		}
		message := strings.TrimSpace(output.String())
		if message == "" {
			return fmt.Errorf("无头浏览器执行失败：%w", err)
		}
		return fmt.Errorf("无头浏览器执行失败：%w（%s）", err, message)
	}
	return nil
}

func fileURL(path string) string {
	return fileURLFromSlashPath(filepath.ToSlash(path))
}

func fileURLFromSlashPath(slashPath string) string {
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	return (&url.URL{Scheme: "file", Path: slashPath}).String()
}

// cappedBuffer keeps the last browser diagnostics bounded.
type cappedBuffer struct {
	limit int
	data  []byte
}

func (b *cappedBuffer) Write(chunk []byte) (int, error) {
	if room := b.limit - len(b.data); room > 0 {
		b.data = append(b.data, chunk[:min(room, len(chunk))]...)
	}
	return len(chunk), nil
}

func (b *cappedBuffer) String() string {
	return strings.ToValidUTF8(string(b.data), "")
}

// discoverBrowser is a variable so tests can simulate a machine without any
// Chromium kernel.
var discoverBrowser = DiscoverBrowser

// DiscoverBrowser finds a usable Chromium kernel. VANTRY_BROWSER_BIN wins when
// it points at an executable; otherwise macOS application bundles, Linux
// package paths and PATH names are tried in order.
func DiscoverBrowser() (Browser, bool) {
	if configured := strings.TrimSpace(os.Getenv("VANTRY_BROWSER_BIN")); configured != "" {
		if path, ok := usableExecutable(configured); ok {
			return Browser{Path: path, Name: filepath.Base(path)}, true
		}
	}
	for _, candidate := range browserCandidates() {
		if path, ok := usableExecutable(candidate); ok {
			return Browser{Path: path, Name: filepath.Base(path)}, true
		}
	}
	return Browser{}, false
}

func usableExecutable(candidate string) (string, bool) {
	path := candidate
	if !strings.ContainsRune(path, os.PathSeparator) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return "", false
		}
		path = resolved
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return path, true
}

func browserCandidates() []string {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
			filepath.Join(home, "Applications/Chromium.app/Contents/MacOS/Chromium"),
			filepath.Join(home, "Applications/Brave Browser.app/Contents/MacOS/Brave Browser"),
		)
	}
	candidates = append(candidates,
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/usr/bin/microsoft-edge",
		"/snap/bin/chromium",
		"/opt/google/chrome/chrome",
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"brave-browser",
		"microsoft-edge",
		"chrome",
	)
	return candidates
}

// Renderer renders HTML visuals. The zero value discovers a local browser and
// is used by RenderHTMLToPNG.
type Renderer struct {
	// Screenshotter overrides browser discovery; tests inject a fake here.
	Screenshotter Screenshotter
	// Timeout bounds one snapshot; zero uses DefaultRenderTimeout.
	Timeout time.Duration
}

// DefaultRenderer serves the package-level RenderHTMLToPNG entry point.
var DefaultRenderer Renderer

// RenderHTMLToPNG prepares, stores and snapshots one HTML visual. The returned
// paths are workspace-relative so Markdown can embed the PNG directly. When no
// local headless browser exists the HTML is still saved, PNGPath stays empty
// and the call succeeds with a warning.
func RenderHTMLToPNG(ctx context.Context, workspaceRoot, name, rawHTML string, viewportWidth, viewportHeight int) (*RenderedVisual, error) {
	return DefaultRenderer.RenderHTML(ctx, workspaceRoot, name, rawHTML, RenderOptions{
		ViewportWidth:  viewportWidth,
		ViewportHeight: viewportHeight,
	})
}

// RenderHTML is RenderHTMLToPNG with full options. HTMLPath is always populated
// when the document could be written; a failed snapshot returns that visual
// together with the error.
func (r Renderer) RenderHTML(ctx context.Context, workspaceRoot, name, rawHTML string, opts RenderOptions) (*RenderedVisual, error) {
	root, err := canonicalRoot(workspaceRoot)
	if err != nil {
		return nil, err
	}
	document, warnings, err := prepareStandaloneHTML(root, rawHTML, opts)
	if err != nil {
		return nil, err
	}
	directory, err := previewDirectory(root)
	if err != nil {
		return nil, err
	}
	file, renamed := sanitizePreviewName(name)
	if renamed {
		warnings = append(warnings, fmt.Sprintf("预览文件名已规范化为 %q", file))
	}
	htmlRelative := filepath.Join(PreviewDirectory, file+".html")
	htmlPath := filepath.Join(directory, file+".html")
	if err := writePreviewFile(root, htmlRelative, []byte(document), 0o600); err != nil {
		return nil, fmt.Errorf("保存预览 HTML 失败：%w", err)
	}
	visual := &RenderedVisual{
		HTMLPath: filepath.ToSlash(htmlRelative),
		HTMLFile: htmlPath,
		Warnings: warnings,
	}

	screenshotter := r.Screenshotter
	browserName := ""
	if screenshotter == nil {
		browser, discovered := discoverBrowser()
		if !discovered {
			visual.Warnings = append(visual.Warnings, fmt.Sprintf(
				"未找到可用的无头浏览器，仅保存 HTML 快照；可调用 OpenInBrowser 打开 %s", visual.HTMLPath))
			return visual, nil
		}
		screenshotter = browser
		browserName = browser.Name
	}

	pngRelative := filepath.Join(PreviewDirectory, file+".png")
	pngPath := filepath.Join(directory, file+".png")
	if err := clearStalePreview(root, pngRelative); err != nil {
		return visual, fmt.Errorf("清理旧预览失败：%w", err)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultRenderTimeout
	}
	renderCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	width, height := opts.viewport()
	if err := screenshotter.Screenshot(renderCtx, htmlPath, pngPath, width, height); err != nil {
		return visual, fmt.Errorf("预览快照生成失败：%w", err)
	}
	if err := verifyScreenshot(pngPath); err != nil {
		return visual, err
	}
	visual.PNGPath = filepath.ToSlash(pngRelative)
	visual.Markdown = "![preview](" + visual.PNGPath + ")"
	visual.Browser = browserName
	return visual, nil
}

func verifyScreenshot(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("无头浏览器未生成预览：%w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < int64(len(pngSignature)) {
		return errors.New("无头浏览器生成的预览无效")
	}
	header := make([]byte, len(pngSignature))
	if _, err := io.ReadFull(file, header); err != nil {
		return errors.New("无头浏览器生成的预览无效")
	}
	if !bytes.Equal(header, pngSignature) {
		return errors.New("无头浏览器生成的预览不是 PNG 图片")
	}
	return nil
}

// OpenInBrowser hands an HTML snapshot to the system default browser. Vantry
// never renders the document itself and never starts a local server. Pass
// RenderedVisual.HTMLFile so the snapshot is found regardless of the process
// directory; the caller decides when a user action justifies a new window.
func OpenInBrowser(ctx context.Context, htmlPath string) error {
	if strings.TrimSpace(htmlPath) == "" {
		return errors.New("HTML 路径不能为空")
	}
	absolute, err := filepath.Abs(htmlPath)
	if err != nil {
		return errors.New("HTML 路径无效")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("HTML 快照不存在")
	}
	switch strings.ToLower(filepath.Ext(absolute)) {
	case ".html", ".htm":
	default:
		return errors.New("仅支持打开 HTML 快照")
	}
	var command string
	var arguments []string
	switch runtime.GOOS {
	case "darwin":
		command, arguments = "open", []string{absolute}
	case "linux":
		command, arguments = "xdg-open", []string{absolute}
	case "windows":
		command, arguments = "rundll32", []string{"url.dll,FileProtocolHandler", absolute}
	default:
		return fmt.Errorf("当前平台 %s 不支持自动打开浏览器", runtime.GOOS)
	}
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	opened := exec.CommandContext(openCtx, command, arguments...)
	opened.WaitDelay = time.Second
	if output, err := opened.CombinedOutput(); err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return fmt.Errorf("打开系统浏览器失败：%w", err)
		}
		return fmt.Errorf("打开系统浏览器失败：%w（%s）", err, message)
	}
	return nil
}
