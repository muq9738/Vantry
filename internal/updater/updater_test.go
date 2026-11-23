package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRepoPath = "/repos/" + DefaultRepo + "/releases/latest"

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入文件: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(data)
}

func newTestManager(t *testing.T, apiBase string, opts Options) *Manager {
	t.Helper()
	opts.APIBase = apiBase
	opts.GOOS = "darwin"
	opts.GOARCH = "arm64"
	opts.CurrentVersion = "0.1.3"
	opts.Slug = "vantry"
	if opts.Token == "" && opts.TokenFinder == nil {
		opts.TokenFinder = func(context.Context) (string, error) { return "", nil }
	}
	return NewManager(opts)
}

func writeRelease(t *testing.T, w http.ResponseWriter, release githubRelease) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(release); err != nil {
		t.Errorf("写入发布 JSON: %v", err)
	}
}

type requestRecord struct {
	Path          string
	Authorization string
	Accept        string
}

type requestLog struct {
	mu       sync.Mutex
	requests []requestRecord
}

func (l *requestLog) add(request *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, requestRecord{
		Path:          request.URL.Path,
		Authorization: request.Header.Get("Authorization"),
		Accept:        request.Header.Get("Accept"),
	})
}

func (l *requestLog) all() []requestRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]requestRecord(nil), l.requests...)
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.4", "0.1.3", 1},
		{"v0.1.4", "0.1.3", 1},
		{"0.1.3", "0.1.3", 0},
		{"v0.1.3", "0.1.3", 0},
		{"0.1.3", "0.1.4", -1},
		{"1.10.0", "1.9.9", 1},
		{"0.1.3-beta.1", "0.1.3", -1},
		{"0.1.4-beta.1", "0.1.3", 1},
		{"0.1.4-beta.10", "0.1.4-beta.2", 1},
		{"0.1.4-beta.1", "0.1.4-beta.1", 0},
		{"0.1.4+build.7", "0.1.4", 0},
		{"0.1.4-rc.1", "0.1.4-beta.3", 1},
		{"2.0", "1.9.9", 1},
		{"dev", "0.0.1", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Fatalf("CompareVersions(%q, %q) = %d，期望 %d", c.a, c.b, got, c.want)
		}
		if got := CompareVersions(c.b, c.a); got != -c.want {
			t.Fatalf("CompareVersions(%q, %q) = %d，期望 %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestResolveCurrentAppBundle(t *testing.T) {
	cases := []struct {
		path   string
		bundle string
		ok     bool
	}{
		{"/Applications/Vantry.app/Contents/MacOS/vantry", "/Applications/Vantry.app", true},
		{"Vantry.app/Contents/MacOS/vantry", "Vantry.app", true},
		{"/Applications/Vantry.app/Contents/MacOS/", "", false},
		{"/Applications/Vantry.app/Contents/MacOS/sub/vantry", "", false},
		{"/Applications/Vantry.app/Contents/Resources/tool", "", false},
		{"/usr/local/bin/vantry", "", false},
		{"/Applications/Vantry.app", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		bundle, ok := ResolveCurrentAppBundle(c.path)
		if bundle != c.bundle || ok != c.ok {
			t.Fatalf("ResolveCurrentAppBundle(%q) = (%q, %v)，期望 (%q, %v)", c.path, bundle, ok, c.bundle, c.ok)
		}
	}
}

func TestCheckFindsReleaseDeltaAndArchive(t *testing.T) {
	log := &requestLog{}
	release := githubRelease{
		TagName:     "v0.1.4",
		Body:        "## 更新\n- 修复远程会话",
		HTMLURL:     "https://github.com/" + DefaultRepo + "/releases/tag/v0.1.4",
		PublishedAt: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
		Assets: []githubAsset{
			{Name: "vantry-0.1.3-to-0.1.4-darwin-arm64.delta", Size: 2048},
			{Name: "update-darwin-arm64.json", Size: 512},
			{Name: "vantry-0.1.4-darwin-arm64.tar.gz", Size: 4096},
			{Name: "vantry-0.1.4-linux-amd64.tar.gz", Size: 4096},
			{Name: "vantry-0.1.4-arm64.dmg", Size: 8192},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		if r.URL.Path != testRepoPath {
			http.NotFound(w, r)
			return
		}
		writeRelease(t, w, release)
	}))
	defer server.Close()

	target := newTargetApp(t)
	manager := newTestManager(t, server.URL, Options{Token: "unit-token-1234", TargetAppPath: target})
	info, err := manager.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.CurrentVersion != "0.1.3" || info.LatestVersion != "0.1.4" || !info.Available {
		t.Fatalf("info = %+v", info)
	}
	if !info.DeltaAvailable || info.DeltaSize != 2048 {
		t.Fatalf("expected delta available with size 2048 when target app exists: %+v", info)
	}
	if info.AssetName != "vantry-0.1.4-darwin-arm64.tar.gz" || info.AssetSize != 4096 {
		t.Fatalf("资产 = %q (%d)", info.AssetName, info.AssetSize)
	}
	if info.Notes != release.Body || !info.PublishedAt.Equal(release.PublishedAt) || info.ReleaseURL != release.HTMLURL {
		t.Fatalf("info = %+v", info)
	}
	requests := log.all()
	if len(requests) != 1 {
		t.Fatalf("请求数 = %d：%+v", len(requests), requests)
	}
	if requests[0].Authorization != "Bearer unit-token-1234" {
		t.Fatalf("Authorization = %q", requests[0].Authorization)
	}
	if !strings.Contains(requests[0].Accept, "vnd.github+json") {
		t.Fatalf("Accept = %q", requests[0].Accept)
	}
}

func TestDeltaRequiresExistingTargetApp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeRelease(t, w, githubRelease{
			TagName: "v0.1.4",
			Assets: []githubAsset{
				{Name: "vantry-0.1.3-to-0.1.4-darwin-arm64.delta", Size: 20},
				{Name: "vantry-0.1.4-darwin-arm64.tar.gz", Size: 100},
			},
		})
	}))
	defer server.Close()

	// Without an existing target bundle on disk, delta cannot be applied.
	missingTargetMgr := newTestManager(t, server.URL, Options{TargetAppPath: filepath.Join(t.TempDir(), "Missing.app")})
	info, err := missingTargetMgr.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.DeltaAvailable {
		t.Fatalf("目标应用不存在时不应标记增量可用: %+v", info)
	}
	if info.AssetName != "vantry-0.1.4-darwin-arm64.tar.gz" {
		t.Fatalf("AssetName = %q", info.AssetName)
	}

	// Self-verifying .delta requires an existing base bundle, not a separate manifest JSON.
	presentTargetMgr := newTestManager(t, server.URL, Options{TargetAppPath: newTargetApp(t)})
	info, err = presentTargetMgr.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.DeltaAvailable || info.DeltaSize != 20 {
		t.Fatalf("存在本地目标应用且存在 .delta 资产时应标记增量可用: %+v", info)
	}
}

func TestCheckUpToDate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeRelease(t, w, githubRelease{
			TagName: "v0.1.3",
			Assets: []githubAsset{
				{Name: "vantry-0.1.3-darwin-arm64.tar.gz", Size: 10},
				{Name: "vantry-0.1.2-to-0.1.3-darwin-arm64.delta", Size: 2},
				{Name: "update-darwin-arm64.json", Size: 1},
			},
		})
	}))
	defer server.Close()

	manager := newTestManager(t, server.URL, Options{})
	info, err := manager.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Available || info.LatestVersion != "0.1.3" {
		t.Fatalf("info = %+v", info)
	}
	if info.AssetName != "" || info.DeltaAvailable {
		t.Fatalf("已是最新时不应选择资产: %+v", info)
	}
}

func TestCheckTokenRejectedFallsBackToUnauthenticated(t *testing.T) {
	log := &requestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeRelease(t, w, githubRelease{
			TagName: "v0.1.4",
			Assets:  []githubAsset{{Name: "vantry-0.1.4-darwin-arm64.tar.gz", Size: 10}},
		})
	}))
	defer server.Close()

	manager := newTestManager(t, server.URL, Options{Token: "stale-token-1234"})
	info, err := manager.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.Available {
		t.Fatalf("info = %+v", info)
	}
	requests := log.all()
	if len(requests) != 2 {
		t.Fatalf("请求数 = %d，期望 2：%+v", len(requests), requests)
	}
	if requests[0].Authorization != "Bearer stale-token-1234" || requests[1].Authorization != "" {
		t.Fatalf("请求 = %+v", requests)
	}

	// The rejected token is not retried on the next check.
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatalf("第二次 Check: %v", err)
	}
	if got := len(log.all()); got != 3 {
		t.Fatalf("请求数 = %d，期望 3", got)
	}
}

func TestCheckPrivateRepoUsesBearerToken(t *testing.T) {
	log := &requestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		if r.Header.Get("Authorization") != "Bearer private-token-9" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRelease(t, w, githubRelease{
			TagName: "v0.2.0",
			Assets:  []githubAsset{{Name: "vantry-0.2.0-darwin-arm64.tar.gz", Size: 10}},
		})
	}))
	defer server.Close()

	manager := newTestManager(t, server.URL, Options{
		TokenFinder: func(context.Context) (string, error) { return "private-token-9", nil },
	})
	info, err := manager.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.Available || info.LatestVersion != "0.2.0" {
		t.Fatalf("info = %+v", info)
	}
	requests := log.all()
	if len(requests) != 1 || requests[0].Authorization != "Bearer private-token-9" {
		t.Fatalf("请求 = %+v", requests)
	}
}

func TestCheckNoPlatformAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeRelease(t, w, githubRelease{
			TagName: "v0.1.4",
			Assets:  []githubAsset{{Name: "vantry-0.1.4-linux-amd64.tar.gz", Size: 10}},
		})
	}))
	defer server.Close()

	manager := newTestManager(t, server.URL, Options{})
	info, err := manager.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.Available || info.AssetName != "" || info.DeltaAvailable {
		t.Fatalf("info = %+v", info)
	}
	if _, err := manager.DownloadAndStage(context.Background(), nil); err == nil {
		t.Fatal("没有匹配资产时 DownloadAndStage 应失败")
	}
}

func TestSelectAssetPreferences(t *testing.T) {
	assets := []githubAsset{
		{Name: "vantry-0.1.4-arm64.dmg"},
		{Name: "vantry-0.1.4-darwin-arm64.zip"},
		{Name: "vantry-0.1.2-to-0.1.4-darwin-arm64.delta"},
		{Name: "update-darwin-arm64.json"},
	}
	asset, ok := selectAsset(assets, "vantry", "0.1.4", "darwin-arm64", "arm64")
	if !ok || asset.Name != "vantry-0.1.4-darwin-arm64.zip" {
		t.Fatalf("asset = %+v, ok = %v", asset, ok)
	}
	asset, ok = selectAsset(assets[0:1], "vantry", "0.1.4", "darwin-arm64", "arm64")
	if !ok || asset.Name != "vantry-0.1.4-arm64.dmg" {
		t.Fatalf("asset = %+v, ok = %v", asset, ok)
	}
	if _, ok := selectAsset([]githubAsset{{Name: "other-0.1.4-darwin-arm64.tar.gz"}}, "vantry", "0.1.4", "darwin-arm64", "arm64"); ok {
		t.Fatal("不同产品的资产不应被选中")
	}
}

func tarGzBytes(t *testing.T, files map[string]string, modes map[string]int64) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(writer)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		mode := int64(0o644)
		if modes != nil {
			if custom, ok := modes[name]; ok {
				mode = custom
			}
		}
		header := &tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatalf("写入 tar 头: %v", err)
		}
		if _, err := archive.Write([]byte(content)); err != nil {
			t.Fatalf("写入 tar 内容: %v", err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("关闭 tar: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 gzip: %v", err)
	}
	return buffer.Bytes()
}

func zipBytes(t *testing.T, files map[string]string, modes map[string]os.FileMode) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mode := os.FileMode(0o644)
		if modes != nil {
			if custom, ok := modes[name]; ok {
				mode = custom
			}
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(mode)
		entry, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatalf("写入 zip 头: %v", err)
		}
		if _, err := entry.Write([]byte(files[name])); err != nil {
			t.Fatalf("写入 zip 内容: %v", err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("关闭 zip: %v", err)
	}
	return buffer.Bytes()
}

func newTargetApp(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "Vantry.app")
	writeFile(t, filepath.Join(target, "Contents", "Resources", "old.txt"), "old")
	return target
}

type runnerLog struct {
	mu    sync.Mutex
	calls []string
}

func (r *runnerLog) run(_ context.Context, name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return nil
}

func (r *runnerLog) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func TestDownloadAndStageTarGz(t *testing.T) {
	archive := tarGzBytes(t, map[string]string{
		"Vantry.app/Contents/Info.plist":        "new",
		"Vantry.app/Contents/MacOS/vantry":      "binary",
		"Vantry.app/Contents/Resources/icons/a": "icon",
	}, map[string]int64{"Vantry.app/Contents/MacOS/vantry": 0o755})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testRepoPath:
			writeRelease(t, w, githubRelease{
				TagName: "v0.1.4",
				Assets: []githubAsset{{
					Name:               "vantry-0.1.4-darwin-arm64.tar.gz",
					Size:               int64(len(archive)),
					BrowserDownloadURL: "http://" + r.Host + "/download/app.tar.gz",
				}},
			})
		case "/download/app.tar.gz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := newTargetApp(t)
	runner := &runnerLog{}
	manager := newTestManager(t, server.URL, Options{TargetAppPath: target, Runner: runner.run})

	var downloads []int64
	var totals []int64
	staged, err := manager.DownloadAndStage(context.Background(), func(downloaded, total int64) {
		downloads = append(downloads, downloaded)
		totals = append(totals, total)
	})
	if err != nil {
		t.Fatalf("DownloadAndStage: %v", err)
	}
	if staged.Installed || staged.StagedPath == target || staged.InstallerPath == "" || staged.Version != "0.1.4" {
		t.Fatalf("staged = %+v", staged)
	}
	if got := readFile(t, filepath.Join(staged.StagedPath, "Contents", "MacOS", "vantry")); got != "binary" {
		t.Fatalf("新应用内容 = %q", got)
	}
	if got := readFile(t, filepath.Join(target, "Contents", "Resources", "old.txt")); got != "old" {
		t.Fatalf("running app was changed: %q", got)
	}
	if len(downloads) == 0 || downloads[len(downloads)-1] != int64(len(archive)) {
		t.Fatalf("进度 = %v", downloads)
	}
	if totals[len(totals)-1] != int64(len(archive)) {
		t.Fatalf("总大小 = %v", totals)
	}
	calls := runner.all()
	if len(calls) < 1 || !strings.HasPrefix(calls[0], "codesign --verify --deep --strict ") || !strings.Contains(calls[0], "Vantry.app") {
		t.Fatalf("签名校验调用 = %+v", calls)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatalf("读取应用目录: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected untouched app and staged download: %+v", entries)
	}
}

func TestDownloadAndStageZip(t *testing.T) {
	archive := zipBytes(t, map[string]string{
		"Vantry.app/Contents/Info.plist":   "zip-new",
		"Vantry.app/Contents/MacOS/vantry": "zip-binary",
	}, map[string]os.FileMode{"Vantry.app/Contents/MacOS/vantry": 0o755})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testRepoPath:
			writeRelease(t, w, githubRelease{
				TagName: "v0.1.5",
				Assets: []githubAsset{{
					Name:               "vantry-0.1.5-darwin-arm64.zip",
					Size:               int64(len(archive)),
					BrowserDownloadURL: "http://" + r.Host + "/download/app.zip",
				}},
			})
		case "/download/app.zip":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := newTargetApp(t)
	runner := &runnerLog{}
	manager := newTestManager(t, server.URL, Options{TargetAppPath: target, Runner: runner.run})
	staged, err := manager.DownloadAndStage(context.Background(), nil)
	if err != nil {
		t.Fatalf("DownloadAndStage: %v", err)
	}
	if staged.Installed || staged.AssetName != "vantry-0.1.5-darwin-arm64.zip" {
		t.Fatalf("staged = %+v", staged)
	}
	if got := readFile(t, filepath.Join(staged.StagedPath, "Contents", "Info.plist")); got != "zip-new" {
		t.Fatalf("新应用内容 = %q", got)
	}
	info, err := os.Stat(filepath.Join(staged.StagedPath, "Contents", "MacOS", "vantry"))
	if err != nil {
		t.Fatalf("可执行文件不存在: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("可执行位丢失: %v", info.Mode())
	}
}

func TestDownloadStagesWithoutWritingUnreadableTarget(t *testing.T) {
	archive := tarGzBytes(t, map[string]string{"Vantry.app/Contents/Info.plist": "new"}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testRepoPath:
			writeRelease(t, w, githubRelease{
				TagName: "v0.1.6",
				Assets: []githubAsset{{
					Name:               "vantry-0.1.6-darwin-arm64.tar.gz",
					Size:               int64(len(archive)),
					BrowserDownloadURL: "http://" + r.Host + "/download/app.tar.gz",
				}},
			})
		case "/download/app.tar.gz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	parent := filepath.Join(t.TempDir(), "Applications")
	target := filepath.Join(parent, "Vantry.app")
	writeFile(t, filepath.Join(target, "Contents", "Resources", "old.txt"), "old")
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatalf("设置只读目录: %v", err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })

	manager := newTestManager(t, server.URL, Options{TargetAppPath: target, Runner: (&runnerLog{}).run})
	staged, err := manager.DownloadAndStage(context.Background(), nil)
	if err != nil {
		t.Fatalf("DownloadAndStage: %v", err)
	}
	if staged.Installed || staged.StagedPath == target {
		t.Fatalf("只读目标不应被替换: %+v", staged)
	}
	if got := readFile(t, filepath.Join(staged.StagedPath, "Contents", "Info.plist")); got != "new" {
		t.Fatalf("暂存内容 = %q", got)
	}
	if got := readFile(t, filepath.Join(target, "Contents", "Resources", "old.txt")); got != "old" {
		t.Fatalf("目标应用被修改: %q", got)
	}
	os.RemoveAll(filepath.Dir(filepath.Dir(staged.StagedPath)))
}

func TestDownloadRejectsZipSlip(t *testing.T) {
	archive := zipBytes(t, map[string]string{
		"Vantry.app/Contents/Info.plist": "ok",
		"../escape.txt":                  "escaped",
	}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testRepoPath:
			writeRelease(t, w, githubRelease{
				TagName: "v0.1.6",
				Assets: []githubAsset{{
					Name:               "vantry-0.1.6-darwin-arm64.zip",
					Size:               int64(len(archive)),
					BrowserDownloadURL: "http://" + r.Host + "/download/app.zip",
				}},
			})
		case "/download/app.zip":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	base := t.TempDir()
	target := filepath.Join(base, "Vantry.app")
	manager := newTestManager(t, server.URL, Options{TargetAppPath: target, Runner: (&runnerLog{}).run})
	if _, err := manager.DownloadAndStage(context.Background(), nil); err == nil {
		t.Fatal("zip-slip 应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(base, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("越界文件被写出: %v", err)
	}
}

func TestDownloadRejectsTarTraversal(t *testing.T) {
	archive := tarGzBytes(t, map[string]string{
		"Vantry.app/Contents/Info.plist": "ok",
		"../escape.txt":                  "escaped",
	}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testRepoPath:
			writeRelease(t, w, githubRelease{
				TagName: "v0.1.7",
				Assets: []githubAsset{{
					Name:               "vantry-0.1.7-darwin-arm64.tar.gz",
					Size:               int64(len(archive)),
					BrowserDownloadURL: "http://" + r.Host + "/download/app.tar.gz",
				}},
			})
		case "/download/app.tar.gz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	base := t.TempDir()
	target := filepath.Join(base, "Vantry.app")
	manager := newTestManager(t, server.URL, Options{TargetAppPath: target, Runner: (&runnerLog{}).run})
	if _, err := manager.DownloadAndStage(context.Background(), nil); err == nil {
		t.Fatal("tar 路径越界应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(base, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("越界文件被写出: %v", err)
	}
}

func TestDownloadRejectsSizeMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testRepoPath:
			writeRelease(t, w, githubRelease{
				TagName: "v0.1.8",
				Assets: []githubAsset{{
					Name:               "vantry-0.1.8-darwin-arm64.tar.gz",
					Size:               999,
					BrowserDownloadURL: "http://" + r.Host + "/download/app.tar.gz",
				}},
			})
		case "/download/app.tar.gz":
			w.Write([]byte("short"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := newTargetApp(t)
	manager := newTestManager(t, server.URL, Options{TargetAppPath: target, Runner: (&runnerLog{}).run})
	_, err := manager.DownloadAndStage(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "大小不一致") {
		t.Fatalf("err = %v，期望大小不一致", err)
	}
	if got := readFile(t, filepath.Join(target, "Contents", "Resources", "old.txt")); got != "old" {
		t.Fatalf("失败时不应替换应用: %q", got)
	}
}

func TestDownloadAndStageDMG(t *testing.T) {
	payload := []byte("disk-image")
	runner := &runnerLog{}
	target := newTargetApp(t)
	manager := newTestManager(t, "", Options{TargetAppPath: target, Runner: func(ctx context.Context, name string, args ...string) error {
		if err := runner.run(ctx, name, args...); err != nil {
			return err
		}
		if name != "hdiutil" || len(args) == 0 {
			return nil
		}
		switch args[0] {
		case "attach":
			mount := args[len(args)-2]
			return writeMountApp(mount)
		case "detach":
			return nil
		}
		return nil
	}})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testRepoPath:
			writeRelease(t, w, githubRelease{
				TagName: "v0.1.9",
				Assets: []githubAsset{{
					Name:               "vantry-0.1.9-arm64.dmg",
					Size:               int64(len(payload)),
					BrowserDownloadURL: "http://" + r.Host + "/download/app.dmg",
				}},
			})
		case "/download/app.dmg":
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager.opts.APIBase = server.URL

	staged, err := manager.DownloadAndStage(context.Background(), nil)
	if err != nil {
		t.Fatalf("DownloadAndStage: %v", err)
	}
	if staged.Installed || staged.Version != "0.1.9" {
		t.Fatalf("staged = %+v", staged)
	}
	if got := readFile(t, filepath.Join(staged.StagedPath, "Contents", "Info.plist")); got != "dmg-new" {
		t.Fatalf("新应用内容 = %q", got)
	}
	var attach, detach, codesign bool
	for _, call := range runner.all() {
		attach = attach || strings.HasPrefix(call, "hdiutil attach -nobrowse -readonly -mountpoint ")
		detach = detach || strings.HasPrefix(call, "hdiutil detach ")
		codesign = codesign || strings.HasPrefix(call, "codesign --verify --deep --strict ")
	}
	if !attach || !detach || !codesign {
		t.Fatalf("调用 = %+v", runner.all())
	}
}

func writeMountApp(mount string) error {
	app := filepath.Join(mount, "Vantry.app", "Contents")
	if err := os.MkdirAll(app, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(app, "Info.plist"), []byte("dmg-new"), 0o644)
}

func TestDownloadFallsBackWhenTokenRejected(t *testing.T) {
	archive := tarGzBytes(t, map[string]string{"Vantry.app/Contents/Info.plist": "new"}, nil)
	log := &requestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		switch r.URL.Path {
		case testRepoPath:
			writeRelease(t, w, githubRelease{
				TagName: "v0.1.4",
				Assets: []githubAsset{{
					Name:               "vantry-0.1.4-darwin-arm64.tar.gz",
					Size:               int64(len(archive)),
					URL:                "http://" + r.Host + "/api/asset/app.tar.gz",
					BrowserDownloadURL: "http://" + r.Host + "/download/app.tar.gz",
				}},
			})
		case "/api/asset/app.tar.gz":
			if r.Header.Get("Authorization") != "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Write(archive)
		case "/download/app.tar.gz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := newTargetApp(t)
	manager := newTestManager(t, server.URL, Options{TargetAppPath: target, Token: "stale-download-token", Runner: (&runnerLog{}).run})
	staged, err := manager.DownloadAndStage(context.Background(), nil)
	if err != nil {
		t.Fatalf("DownloadAndStage: %v", err)
	}
	if staged.Installed || staged.StagedPath == target {
		t.Fatalf("download changed target: %+v", staged)
	}
	var apiHit, downloadHit bool
	for _, request := range log.all() {
		switch request.Path {
		case "/api/asset/app.tar.gz":
			apiHit = true
			if !strings.Contains(request.Accept, "application/octet-stream") {
				t.Fatalf("私有仓库下载应请求 octet-stream: %+v", request)
			}
		case "/download/app.tar.gz":
			downloadHit = true
			if request.Authorization != "" {
				t.Fatalf("回退下载不应携带 Authorization: %+v", request)
			}
		}
	}
	if !apiHit || !downloadHit {
		t.Fatalf("请求 = %+v", log.all())
	}
}

func TestUpdateTokenDoesNotUseDevelopmentCredentials(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "gh-was-called")
	writeFile(t, filepath.Join(dir, "gh"), "#!/bin/sh\ntouch '"+marker+"'\nprintf 'broad-token-123'\n")
	_ = os.Chmod(filepath.Join(dir, "gh"), 0o755)
	t.Setenv("PATH", dir)
	t.Setenv("GITHUB_TOKEN", "broad-token-1234")
	t.Setenv("GH_TOKEN", "broad-token-5678")
	path := filepath.Join(dir, "update-token")
	t.Setenv("VANTRY_UPDATE_TOKEN_FILE", path)
	if got := FindGitHubToken(context.Background()); got != "" {
		t.Fatal("development credential was reused")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("gh was invoked")
	}
	if err := os.WriteFile(path, []byte("dedicated-token-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := FindGitHubToken(context.Background()); got != "dedicated-token-123" {
		t.Fatal("dedicated credential unavailable")
	}
	_ = os.Chmod(path, 0o644)
	if _, err := readUpdateToken(path); err == nil {
		t.Fatal("shared-readable credential was accepted")
	}
}

func TestCleanToken(t *testing.T) {
	if got := cleanToken("  ghx_1234567890abcdef  "); got != "ghx_1234567890abcdef" {
		t.Fatalf("cleanToken = %q", got)
	}
	for _, value := range []string{"", "short", "two words here", "line\nbreak", "token!"} {
		if got := cleanToken(value); got != "" {
			t.Fatalf("cleanToken(%q) = %q，期望空", value, got)
		}
	}
}

func TestFindGitHubTokenNeverLeaksRejectedOutput(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'two words here\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("写入假 gh: %v", err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", dir)
	t.Setenv("VANTRY_UPDATE_TOKEN_FILE", filepath.Join(dir, "missing-token"))
	if got := FindGitHubToken(context.Background()); got != "" {
		t.Fatalf("got = %q", got)
	}
}
