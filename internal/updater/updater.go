// Package updater checks the GitHub releases of Vantry and stages a new
// version next to the running app. It never computes or stores content
// signatures: verification is the code signature of the staged bundle, the
// size the release declares, and the bytes the HTTP response delivers.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/api"
)

const (
	// DefaultRepo is the repository whose releases Vantry installs from.
	DefaultRepo = "muq9738/Vantry"
	// CurrentVersion is the version this build reports when the caller does
	// not pass one.
	CurrentVersion = "0.1.40"
	// DefaultAPIBase is the GitHub REST API.
	DefaultAPIBase = "https://api.github.com"

	maxAssetSize    = 1 << 30 // 1 GiB, the archive bound mygo publishes under
	maxExtractSize  = 2 << 30
	maxSingleFile   = 1 << 30
	maxReleaseBody  = 64 << 10
	maxAPIResponse  = 1 << 20
	apiRequestLimit = 30 * time.Second
)

// Options configures a Manager. The zero value targets DefaultRepo,
// CurrentVersion and the current platform, preferring the local dedicated
// credential over the read-only credential shipped in the app's resources.
type Options struct {
	Repo           string
	CurrentVersion string
	APIBase        string
	Slug           string
	GOOS           string
	GOARCH         string
	HTTPClient     *http.Client
	Token          string
	TokenFinder    func(context.Context) (string, error)
	TokenFile      string
	// BundledTokenFile overrides the credential path in packaged resources.
	BundledTokenFile string
	DataDir        string
	TargetAppPath  string
	// Runner runs codesign and hdiutil; tests replace it.
	Runner func(context.Context, string, ...string) error
	// LaunchRunner spawns the detached apply-update worker; tests replace it.
	LaunchRunner func(context.Context, string, ...string) error
}

// StagedUpdate is a new version ready to run: either installed in place or
// staged at StagedPath for the caller to move.
type StagedUpdate struct {
	Version        string
	StagedPath     string
	InstallerPath  string
	TargetPath     string
	Installed      bool
	Native         bool
	UsedDelta      bool
	CanAutoRestart bool
	AssetName      string
	Size           int64
}

// githubRelease is the part of the GitHub release JSON Vantry consumes.
type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	HTMLURL     string        `json:"html_url"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	PublishedAt time.Time     `json:"published_at"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Manager checks releases and stages updates. It is safe for concurrent use.
type Manager struct {
	mu            sync.Mutex
	opts          Options
	client        *http.Client
	token         string
	tokenResolved bool
	checked       bool
	tokenErr      error
	info          api.AppUpdateInfo
	asset         githubAsset
	deltaAsset    githubAsset
	staged        *StagedUpdate
	stagedRoot    string
}

// NewManager returns a Manager for opts.
func NewManager(opts Options) *Manager {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &Manager{opts: opts, client: client}
}

// CurrentInfo returns the latest checked update status enriched with any staged
// bundle state on disk.
func (m *Manager) CurrentInfo() api.AppUpdateInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	info := m.info
	m.mergeStagedLocked(&info)
	return info
}

// Check reads release metadata. Only the offline installer replaces an app.
func (m *Manager) Check(ctx context.Context) (api.AppUpdateInfo, error) {
	// A user can add or rotate the dedicated credential while Core stays
	// alive. Retry file errors too; an absent token must not require restart.
	if m.opts.Token == "" && m.opts.TokenFinder == nil {
		m.mu.Lock()
		m.tokenResolved, m.tokenErr = false, nil
		m.mu.Unlock()
	}
	return m.checkGitHub(contextOrBackground(ctx))
}

// DownloadAndStage downloads and verifies a separate bundle without changing
// the running app. When a valid bundle for the latest release is already
// staged on disk, it reuses that bundle without re-downloading.
func (m *Manager) DownloadAndStage(ctx context.Context, progress func(int64, int64)) (StagedUpdate, error) {
	ctx = contextOrBackground(ctx)
	if progress == nil {
		progress = func(int64, int64) {}
	}
	m.mu.Lock()
	checked := m.checked
	m.mu.Unlock()
	if !checked {
		if _, err := m.Check(ctx); err != nil {
			return StagedUpdate{}, err
		}
	}
	m.mu.Lock()
	info, asset, deltaAsset := m.info, m.asset, m.deltaAsset
	var existing *StagedUpdate
	if m.staged != nil {
		copyStaged := *m.staged
		existing = &copyStaged
	}
	m.mu.Unlock()
	if !info.Available {
		return StagedUpdate{}, errors.New("当前已是最新版本")
	}
	if existing != nil && normalizeVersion(existing.Version) == normalizeVersion(info.LatestVersion) && pathExists(existing.StagedPath) {
		if m.goos() != "darwin" || m.verifyBundle(ctx, existing.StagedPath) == nil {
			existing.CanAutoRestart = m.canAutoRestart(*existing)
			if existing.Size > 0 {
				progress(existing.Size, existing.Size)
			}
			return *existing, nil
		}
	}
	if asset.Name == "" && (!info.DeltaAvailable || deltaAsset.Name == "") {
		return StagedUpdate{}, fmt.Errorf("发布 %s 没有适用于 %s-%s 的安装包", info.LatestVersion, m.goos(), m.goarch())
	}
	return m.stageAsset(ctx, asset, deltaAsset, info, progress)
}

func (m *Manager) checkGitHub(ctx context.Context) (api.AppUpdateInfo, error) {
	release, _, err := m.fetchLatestRelease(ctx)
	if err != nil {
		return api.AppUpdateInfo{}, err
	}
	current := m.currentVersion()
	latest := normalizeVersion(release.TagName)
	if latest == "" {
		latest = normalizeVersion(release.Name)
	}
	if latest == "" {
		return api.AppUpdateInfo{}, errors.New("GitHub 发布缺少版本号")
	}
	info := api.AppUpdateInfo{
		CurrentVersion: current,
		LatestVersion:  latest,
		Notes:          truncateText(release.Body, maxReleaseBody),
		PublishedAt:    release.PublishedAt,
		ReleaseURL:     release.HTMLURL,
	}
	if info.ReleaseURL == "" {
		info.ReleaseURL = "https://github.com/" + m.repo() + "/releases/tag/" + release.TagName
	}
	asset, delta := m.pickAsset(release.Assets, latest)
	if CompareVersions(latest, current) <= 0 {
		return m.record(info, githubAsset{}, githubAsset{}), nil
	}
	target := m.targetAppPath()
	deltaReady := delta.Name != "" && isExistingDir(target)
	info.Available = true
	info.DeltaAvailable = deltaReady
	if deltaReady {
		info.DeltaSize = delta.Size
	} else {
		delta = githubAsset{}
	}
	info.AssetName = asset.Name
	info.AssetSize = asset.Size
	return m.record(info, asset, delta), nil
}

func (m *Manager) record(info api.AppUpdateInfo, asset, deltaAsset githubAsset) api.AppUpdateInfo {
	m.mu.Lock()
	m.checked = true
	m.asset = asset
	m.deltaAsset = deltaAsset
	m.mergeStagedLocked(&info)
	m.info = info
	m.mu.Unlock()
	return info
}

func (m *Manager) mergeStagedLocked(info *api.AppUpdateInfo) {
	if info == nil || m.staged == nil {
		return
	}
	if !pathExists(m.staged.StagedPath) {
		return
	}
	if info.LatestVersion != "" && normalizeVersion(m.staged.Version) != normalizeVersion(info.LatestVersion) {
		return
	}
	info.Staged = true
	info.StagedVersion = m.staged.Version
	info.StagedPath = m.staged.StagedPath
	info.TargetPath = m.staged.TargetPath
	info.InstallerPath = m.staged.InstallerPath
	info.UsedDelta = m.staged.UsedDelta
	info.CanAutoRestart = m.canAutoRestart(*m.staged)
}

// pickAsset selects the platform full archive and any matching self-verifying
// binary delta from the running version.
func (m *Manager) pickAsset(assets []githubAsset, latest string) (githubAsset, githubAsset) {
	target := m.goos() + "-" + m.goarch()
	deltaName := m.slug() + "-" + fileVersion(m.currentVersion()) + "-to-" + fileVersion(latest) + "-" + target + ".delta"
	delta, _ := findAssetByName(assets, deltaName)
	asset, _ := selectAsset(assets, m.slug(), fileVersion(latest), target, m.goarch())
	return asset, delta
}

func findAssetByName(assets []githubAsset, name string) (githubAsset, bool) {
	for _, asset := range assets {
		if strings.EqualFold(asset.Name, name) {
			return asset, true
		}
	}
	return githubAsset{}, false
}

// selectAsset prefers the exact mygo archive name for the target, then any
// archive that names the target, and finally a macOS disk image.
func selectAsset(assets []githubAsset, slug, version, target, goarch string) (githubAsset, bool) {
	preferred := []string{
		slug + "-" + version + "-" + target + ".tar.gz",
		slug + "-" + version + "-" + target + ".zip",
		slug + "-" + version + "-" + goarch + ".dmg",
		slug + "-" + version + ".dmg",
	}
	for _, name := range preferred {
		for _, asset := range assets {
			if strings.EqualFold(asset.Name, name) {
				return asset, true
			}
		}
	}
	for _, suffix := range []string{".tar.gz", ".zip", ".dmg"} {
		for _, asset := range assets {
			if isPlatformAsset(asset.Name, slug, suffix, target, goarch) {
				return asset, true
			}
		}
	}
	return githubAsset{}, false
}

func isPlatformAsset(name, slug, suffix, target, goarch string) bool {
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, suffix) || strings.HasSuffix(lower, ".delta") {
		return false
	}
	if strings.HasPrefix(lower, "update-") || strings.Contains(lower, ".json") {
		return false
	}
	if !strings.Contains(lower, target) {
		if suffix != ".dmg" || !strings.Contains(lower, "-"+goarch+".dmg") {
			return false
		}
	}
	if slug != "" && !strings.HasPrefix(lower, strings.ToLower(slug)+"-") {
		return false
	}
	return true
}

func (m *Manager) fetchLatestRelease(ctx context.Context) (*githubRelease, string, error) {
	token := m.githubToken(ctx)
	m.mu.Lock()
	tokenErr := m.tokenErr
	m.mu.Unlock()
	if tokenErr != nil {
		return nil, "", tokenErr
	}
	release, status, err := m.getRelease(ctx, token)
	if err == nil {
		return release, token, nil
	}
	noRelease := errors.New("仓库尚未发布正式 Release，暂时没有可下载的更新")
	if status == http.StatusNotFound && m.repositoryVisible(ctx, token) {
		return nil, token, noRelease
	}
	if token != "" && isAuthStatus(status) {
		release, status, err = m.getRelease(ctx, "")
		if err == nil {
			m.forgetToken()
			return release, "", nil
		}
		if status == http.StatusNotFound && m.repositoryVisible(ctx, "") {
			return nil, "", noRelease
		}
	}
	if isAuthStatus(status) {
		return nil, token, fmt.Errorf("GitHub 仓库 %s 不可见；私有更新请配置仅该仓库 Contents 只读的专用凭据", m.repo())
	}
	return nil, token, err
}

// A visible repository without releases is not an authentication failure.
func (m *Manager) repositoryVisible(ctx context.Context, token string) bool {
	ctx, cancel := context.WithTimeout(ctx, apiRequestLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.apiBase()+"/repos/"+m.repo(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", m.userAgent())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode == http.StatusOK
}

func (m *Manager) getRelease(ctx context.Context, token string) (*githubRelease, int, error) {
	requestCtx, cancel := context.WithTimeout(ctx, apiRequestLimit)
	defer cancel()
	endpoint := m.apiBase() + "/repos/" + m.repo() + "/releases/latest"
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("构造 GitHub 请求失败: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", m.userAgent())
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("查询 GitHub 发布失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return nil, response.StatusCode, fmt.Errorf("GitHub 返回 %s", response.Status)
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponse)).Decode(&release); err != nil {
		return nil, response.StatusCode, fmt.Errorf("解析 GitHub 发布失败: %w", err)
	}
	return &release, response.StatusCode, nil
}

func (m *Manager) githubToken(ctx context.Context) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokenResolved {
		return m.token
	}
	token := strings.TrimSpace(m.opts.Token)
	if token == "" {
		finder := m.opts.TokenFinder
		if finder == nil {
			finder = func(context.Context) (string, error) {
				path := m.opts.TokenFile
				if path == "" {
					path = UpdateTokenFile()
				}
				bundled := m.opts.BundledTokenFile
				if bundled == "" {
					bundled = bundledUpdateTokenFile()
				}
				return readUpdateCredentials(path, bundled)
			}
		}
		var err error
		token, err = finder(ctx)
		m.tokenErr = err
	}
	m.token = strings.TrimSpace(token)
	m.tokenResolved = true
	return m.token
}

// forgetToken drops a rejected token and stops rediscovering it for this
// Manager, so a failed check does not shell out again.
func (m *Manager) forgetToken() {
	m.mu.Lock()
	m.token = ""
	m.tokenResolved = true
	m.mu.Unlock()
}

func (m *Manager) repo() string {
	if repo := strings.TrimSpace(m.opts.Repo); repo != "" {
		return strings.TrimSuffix(repo, "/")
	}
	return DefaultRepo
}

func (m *Manager) currentVersion() string {
	if version := strings.TrimSpace(m.opts.CurrentVersion); version != "" {
		return version
	}
	return CurrentVersion
}

func (m *Manager) apiBase() string {
	if base := strings.TrimSpace(m.opts.APIBase); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return DefaultAPIBase
}

func (m *Manager) slug() string {
	if slug := strings.TrimSpace(m.opts.Slug); slug != "" {
		return slug
	}
	repo := m.repo()
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	return strings.ToLower(repo)
}

func (m *Manager) goos() string {
	if value := strings.TrimSpace(m.opts.GOOS); value != "" {
		return value
	}
	return runtime.GOOS
}

func (m *Manager) goarch() string {
	if value := strings.TrimSpace(m.opts.GOARCH); value != "" {
		return value
	}
	return runtime.GOARCH
}

func (m *Manager) userAgent() string {
	return "Vantry-Updater/" + m.currentVersion()
}

func (m *Manager) targetAppPath() string {
	if target := strings.TrimSpace(m.opts.TargetAppPath); target != "" {
		return target
	}
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	bundle, ok := ResolveCurrentAppBundle(executable)
	if !ok {
		return ""
	}
	return bundle
}

// ResolveCurrentAppBundle maps the executable inside a macOS app bundle to
// the bundle, as in /Applications/Vantry.app/Contents/MacOS/vantry.
func ResolveCurrentAppBundle(executablePath string) (string, bool) {
	if strings.TrimSpace(executablePath) == "" {
		return "", false
	}
	clean := filepath.Clean(executablePath)
	macOS := filepath.Dir(clean)
	if filepath.Base(macOS) != "MacOS" {
		return "", false
	}
	contents := filepath.Dir(macOS)
	if filepath.Base(contents) != "Contents" {
		return "", false
	}
	bundle := filepath.Dir(contents)
	if !strings.HasSuffix(strings.ToLower(bundle), ".app") {
		return "", false
	}
	return bundle, true
}

// CompareVersions compares semantic versions such as v0.1.4, 0.1.3 and
// 0.2.0-beta.3. It returns -1, 0 or 1 and ignores build metadata.
func CompareVersions(a, b string) int {
	aCore, aPre := splitVersion(a)
	bCore, bPre := splitVersion(b)
	for i := range aCore {
		if aCore[i] != bCore[i] {
			if aCore[i] < bCore[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	}
	return comparePrerelease(aPre, bPre)
}

func splitVersion(version string) ([3]int, string) {
	version = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(version), "v"), "V")
	if i := strings.IndexByte(version, '+'); i >= 0 {
		version = version[:i]
	}
	pre := ""
	if i := strings.IndexByte(version, '-'); i >= 0 {
		version, pre = version[:i], version[i+1:]
	}
	var core [3]int
	for i, part := range strings.SplitN(version, ".", 3) {
		core[i] = leadingNumber(part)
	}
	return core, pre
}

func leadingNumber(part string) int {
	total := 0
	for _, r := range part {
		if r < '0' || r > '9' {
			break
		}
		total = total*10 + int(r-'0')
		if total > 1<<30 {
			return 1 << 30
		}
	}
	return total
}

func comparePrerelease(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) || i < len(bParts); i++ {
		switch {
		case i >= len(aParts):
			return -1
		case i >= len(bParts):
			return 1
		}
		if diff := compareIdentifier(aParts[i], bParts[i]); diff != 0 {
			return diff
		}
	}
	return 0
}

func compareIdentifier(a, b string) int {
	aNumber, aOK := identifierNumber(a)
	bNumber, bOK := identifierNumber(b)
	switch {
	case aOK && bOK:
		switch {
		case aNumber < bNumber:
			return -1
		case aNumber > bNumber:
			return 1
		}
		return 0
	case aOK:
		return -1
	case bOK:
		return 1
	}
	return strings.Compare(a, b)
}

func identifierNumber(identifier string) (int, bool) {
	if identifier == "" {
		return 0, false
	}
	total := 0
	for _, r := range identifier {
		if r < '0' || r > '9' {
			return 0, false
		}
		total = total*10 + int(r-'0')
		if total > 1<<30 {
			return 1 << 30, true
		}
	}
	return total, true
}

func normalizeVersion(version string) string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(version), "v"), "V")
}

func tagName(version string) string {
	trimmed := strings.TrimSpace(version)
	if strings.HasPrefix(trimmed, "v") || strings.HasPrefix(trimmed, "V") {
		return trimmed
	}
	return "v" + trimmed
}

// fileVersion keeps a version usable in an asset file name, as mygo does
// when it publishes archives.
func fileVersion(version string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '-'
		}
		return r
	}, normalizeVersion(version))
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
