package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

const (
	defaultGitHubAPIBase      = "https://api.github.com"
	maxArchiveDownloadBytes   = 32 << 20
	maxArchiveFiles           = 512
	maxArchiveTotalBytes      = 64 << 20
	maxArchiveSingleFileBytes = 16 << 20
	githubAPIVersion          = "2022-11-28"
)

var stagingSeq atomic.Uint64

type githubEndpointKey struct{}

type githubEndpointConfig struct {
	apiBase    string
	httpClient *http.Client
	ghTokenFn  func(context.Context) string
}

func withGitHubEndpoint(ctx context.Context, apiBase string, client *http.Client) context.Context {
	return context.WithValue(ctx, githubEndpointKey{}, githubEndpointConfig{
		apiBase:    strings.TrimRight(strings.TrimSpace(apiBase), "/"),
		httpClient: client,
	})
}

func resolveGitHubConfig(ctx context.Context, host *Host) githubEndpointConfig {
	var cfg githubEndpointConfig
	if v, ok := ctx.Value(githubEndpointKey{}).(githubEndpointConfig); ok {
		cfg = v
	}
	if cfg.apiBase == "" && host != nil && strings.TrimSpace(host.GitHubAPIBase) != "" {
		cfg.apiBase = strings.TrimRight(strings.TrimSpace(host.GitHubAPIBase), "/")
	}
	if cfg.apiBase == "" {
		if envURL := strings.TrimSpace(os.Getenv("VANTRY_GITHUB_API_URL")); envURL != "" {
			cfg.apiBase = strings.TrimRight(envURL, "/")
		}
	}
	if cfg.apiBase == "" {
		cfg.apiBase = defaultGitHubAPIBase
	}
	if cfg.httpClient == nil && host != nil && host.HTTPClient != nil {
		cfg.httpClient = host.HTTPClient
	}
	if cfg.httpClient == nil {
		cfg.httpClient = defaultPluginHTTPClient
	}
	return cfg
}

// ParseRepo normalizes a GitHub repository reference into "owner/repo".
// Supported forms:
//   - "owner/repo"
//   - "https://github.com/owner/repo"
//   - "https://github.com/owner/repo/tree/main"
//   - "git@github.com:owner/repo.git"
func ParseRepo(in string) (string, error) {
	s := strings.TrimSpace(in)
	if s == "" {
		return "", errors.New("GitHub 仓库地址不能为空")
	}

	// SSH SCP-like syntax: git@github.com:owner/repo.git
	if strings.HasPrefix(s, "git@") {
		rest := strings.TrimPrefix(s, "git@")
		colon := strings.IndexByte(rest, ':')
		if colon <= 0 {
			return "", fmt.Errorf("无效的 GitHub SSH 地址：%q", in)
		}
		host := strings.ToLower(rest[:colon])
		if host != "github.com" {
			return "", fmt.Errorf("仅支持 github.com 仓库，不支持 %q", host)
		}
		repoPath := strings.Trim(rest[colon+1:], "/")
		segs := splitCleanSegments(repoPath)
		if len(segs) != 2 {
			return "", fmt.Errorf("无效的 GitHub 仓库路径：%q", in)
		}
		return validateOwnerRepo(segs[0], strings.TrimSuffix(segs[1], ".git"), in)
	}

	// URL with scheme or leading github.com/
	if strings.Contains(s, "://") || strings.HasPrefix(strings.ToLower(s), "github.com/") || strings.HasPrefix(strings.ToLower(s), "www.github.com/") {
		rawURL := s
		if !strings.Contains(rawURL, "://") {
			rawURL = "https://" + rawURL
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return "", fmt.Errorf("解析 GitHub URL 失败：%w", err)
		}
		host := strings.ToLower(u.Hostname())
		if host != "github.com" && host != "www.github.com" {
			return "", fmt.Errorf("仅支持 github.com 仓库，不支持 %q", host)
		}
		segs := splitCleanSegments(u.Path)
		if len(segs) < 2 {
			return "", fmt.Errorf("GitHub URL 缺少 owner/repo：%q", in)
		}
		owner := segs[0]
		repo := segs[1]
		if len(segs) == 2 {
			repo = strings.TrimSuffix(repo, ".git")
		}
		return validateOwnerRepo(owner, repo, in)
	}

	// Shorthand owner/repo
	segs := splitCleanSegments(s)
	if len(segs) != 2 || strings.HasPrefix(s, "/") {
		return "", fmt.Errorf("无效的 GitHub 仓库标识 %q（应为 owner/repo）", in)
	}
	return validateOwnerRepo(segs[0], strings.TrimSuffix(segs[1], ".git"), in)
}

func splitCleanSegments(p string) []string {
	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg == "" {
			continue
		}
		out = append(out, seg)
	}
	return out
}

func validateOwnerRepo(owner, repo, original string) (string, error) {
	if !isValidGitHubIdent(owner) || !isValidGitHubIdent(repo) {
		return "", fmt.Errorf("无效的 GitHub 仓库标识：%q", original)
	}
	return owner + "/" + repo, nil
}

func isValidGitHubIdent(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func resolveGitHubToken(ctx context.Context, cfg githubEndpointConfig) string {
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		return tok
	}
	if tok := strings.TrimSpace(os.Getenv("GH_TOKEN")); tok != "" {
		return tok
	}
	if cfg.ghTokenFn != nil {
		return strings.TrimSpace(cfg.ghTokenFn(ctx))
	}
	if cfg.apiBase != defaultGitHubAPIBase {
		return ""
	}
	return queryGHCLIToken(ctx)
}

func queryGHCLIToken(ctx context.Context) string {
	ghBin, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ghCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ghCtx, ghBin, "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func doGitHubRequest(ctx context.Context, cfg githubEndpointConfig, targetURL, accept, token string) (*http.Response, error) {
	client := redirectSafeClient(cfg.httpClient)

	sendOnce := func(authToken string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		} else {
			req.Header.Set("Accept", "application/vnd.github+json")
		}
		req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
		req.Header.Set("User-Agent", "Vantry-Plugin-Host")
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}
		return client.Do(req)
	}

	resp, err := sendOnce(token)
	if err != nil {
		return nil, err
	}
	if token != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		return sendOnce("")
	}
	return resp, nil
}

func redirectSafeClient(base *http.Client) *http.Client {
	if base == nil {
		base = defaultPluginHTTPClient
	}
	clone := *base
	prevRedirect := base.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("GitHub 重定向次数过多")
		}
		if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
			req.Header.Del("Authorization")
		}
		if prevRedirect != nil {
			return prevRedirect(req, via)
		}
		return nil
	}
	return &clone
}

type githubReleaseAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
	ContentType        string `json:"content_type"`
}

type githubReleaseInfo struct {
	TagName    string               `json:"tag_name"`
	ZipballURL string               `json:"zipball_url"`
	Assets     []githubReleaseAsset `json:"assets"`
}

type githubRepoMeta struct {
	DefaultBranch string `json:"default_branch"`
	PushedAt      string `json:"pushed_at"`
}

type githubCommitMeta struct {
	Ref     string `json:"ref"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Committer struct {
			Date string `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

type resolvedRemoteSource struct {
	repo        string
	version     string
	downloadURL string
	accept      string
}

func queryLatestGitHubSource(ctx context.Context, cfg githubEndpointConfig, repo string) (resolvedRemoteSource, error) {
	token := resolveGitHubToken(ctx, cfg)
	releaseEndpoint := fmt.Sprintf("%s/repos/%s/releases/latest", cfg.apiBase, repo)

	resp, err := doGitHubRequest(ctx, cfg, releaseEndpoint, "application/vnd.github+json", token)
	if err != nil {
		return resolvedRemoteSource{}, fmt.Errorf("请求 GitHub Releases 失败：%w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		return resolvedRemoteSource{}, fmt.Errorf("读取 GitHub Releases 响应失败：%w", readErr)
	}

	if resp.StatusCode == http.StatusOK {
		var rel githubReleaseInfo
		if err := json.Unmarshal(body, &rel); err != nil {
			return resolvedRemoteSource{}, fmt.Errorf("解析 GitHub Release 元数据失败：%w", err)
		}
		version := strings.TrimSpace(rel.TagName)
		if version == "" {
			return resolvedRemoteSource{}, errors.New("GitHub Release 缺少 tag_name")
		}
		for _, a := range rel.Assets {
			lowerName := strings.ToLower(strings.TrimSpace(a.Name))
			if strings.HasSuffix(lowerName, ".zip") || strings.EqualFold(a.ContentType, "application/zip") {
				if strings.TrimSpace(a.URL) != "" {
					return resolvedRemoteSource{
						repo:        repo,
						version:     version,
						downloadURL: a.URL,
						accept:      "application/octet-stream",
					}, nil
				}
				if strings.TrimSpace(a.BrowserDownloadURL) != "" {
					return resolvedRemoteSource{
						repo:        repo,
						version:     version,
						downloadURL: a.BrowserDownloadURL,
						accept:      "application/octet-stream",
					}, nil
				}
			}
		}
		dlURL := strings.TrimSpace(rel.ZipballURL)
		if dlURL == "" {
			dlURL = fmt.Sprintf("%s/repos/%s/zipball/%s", cfg.apiBase, repo, url.PathEscape(version))
		}
		return resolvedRemoteSource{
			repo:        repo,
			version:     version,
			downloadURL: dlURL,
			accept:      "application/vnd.github+json",
		}, nil
	}

	if resp.StatusCode != http.StatusNotFound {
		return resolvedRemoteSource{}, fmt.Errorf("GitHub API 返回状态码 %d（%s）", resp.StatusCode, repo)
	}

	// Repo has no releases: fall back to default branch zipball and head commit metadata.
	defaultBranch := ""
	version := ""

	repoEndpoint := fmt.Sprintf("%s/repos/%s", cfg.apiBase, repo)
	repoResp, err := doGitHubRequest(ctx, cfg, repoEndpoint, "application/vnd.github+json", token)
	if err != nil {
		return resolvedRemoteSource{}, fmt.Errorf("请求 GitHub 仓库信息失败：%w", err)
	}
	repoBody, _ := io.ReadAll(io.LimitReader(repoResp.Body, 1<<20))
	_ = repoResp.Body.Close()
	if repoResp.StatusCode == http.StatusOK {
		var meta githubRepoMeta
		if json.Unmarshal(repoBody, &meta) == nil {
			defaultBranch = strings.TrimSpace(meta.DefaultBranch)
			version = strings.TrimSpace(meta.PushedAt)
		}
	} else if repoResp.StatusCode != http.StatusNotFound {
		return resolvedRemoteSource{}, fmt.Errorf("GitHub 仓库查询返回状态码 %d（%s）", repoResp.StatusCode, repo)
	}

	commitTarget := defaultBranch
	if commitTarget == "" {
		commitTarget = "HEAD"
	}
	commitEndpoint := fmt.Sprintf("%s/repos/%s/commits/%s", cfg.apiBase, repo, url.PathEscape(commitTarget))
	if commitResp, cerr := doGitHubRequest(ctx, cfg, commitEndpoint, "application/vnd.github+json", token); cerr == nil {
		commitBody, _ := io.ReadAll(io.LimitReader(commitResp.Body, 1<<20))
		_ = commitResp.Body.Close()
		if commitResp.StatusCode == http.StatusOK {
			var cm githubCommitMeta
			if json.Unmarshal(commitBody, &cm) == nil {
				if ref := extractCommitVersionTag(cm); ref != "" {
					version = ref
				}
			}
		}
	}

	zipURL := fmt.Sprintf("%s/repos/%s/zipball", cfg.apiBase, repo)
	if defaultBranch != "" {
		zipURL = fmt.Sprintf("%s/repos/%s/zipball/%s", cfg.apiBase, repo, url.PathEscape(defaultBranch))
	}
	return resolvedRemoteSource{
		repo:        repo,
		version:     version,
		downloadURL: zipURL,
		accept:      "application/vnd.github+json",
	}, nil
}

func extractCommitVersionTag(cm githubCommitMeta) string {
	if r := strings.TrimSpace(cm.Ref); r != "" {
		return r
	}
	if u := strings.TrimSpace(cm.HTMLURL); u != "" {
		if base := path.Base(strings.TrimRight(u, "/")); base != "" && base != "." && base != "/" {
			if len(base) > 12 {
				base = base[:12]
			}
			return base
		}
	}
	if d := strings.TrimSpace(cm.Commit.Committer.Date); d != "" {
		return d
	}
	return ""
}

func downloadGitHubArchive(ctx context.Context, cfg githubEndpointConfig, src resolvedRemoteSource, repo string) ([]byte, error) {
	token := resolveGitHubToken(ctx, cfg)
	resp, err := doGitHubRequest(ctx, cfg, src.downloadURL, src.accept, token)
	if err != nil {
		return nil, fmt.Errorf("下载插件压缩包失败：%w", err)
	}
	if resp.StatusCode == http.StatusNotFound && strings.Contains(src.downloadURL, "/zipball/") {
		_ = resp.Body.Close()
		fallbackURL := fmt.Sprintf("%s/repos/%s/zipball", cfg.apiBase, repo)
		resp, err = doGitHubRequest(ctx, cfg, fallbackURL, src.accept, token)
		if err != nil {
			return nil, fmt.Errorf("下载默认分支压缩包失败：%w", err)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载插件包失败：HTTP %d（%s）", resp.StatusCode, repo)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取插件压缩包失败：%w", err)
	}
	if int64(len(data)) > maxArchiveDownloadBytes {
		return nil, fmt.Errorf("插件压缩包超过 %d MiB 上限", maxArchiveDownloadBytes>>20)
	}
	return data, nil
}

// InstallFromGitHub downloads, safely unpacks, validates, compiles, and atomically installs
// a plugin from a GitHub repository (latest release zip asset or default-branch zipball).
func InstallFromGitHub(ctx context.Context, pluginsDir, repoSpec string) (api.PluginInfo, error) {
	return (Host{Directory: pluginsDir}).InstallFromGitHub(ctx, repoSpec)
}

// InstallFromGitHub installs a GitHub plugin into h.Directory using h's HTTP/Locale configuration.
func (h Host) InstallFromGitHub(ctx context.Context, repoSpec string) (api.PluginInfo, error) {
	if ctx == nil {
		return api.PluginInfo{}, errors.New("plugins: nil context")
	}
	if err := ctx.Err(); err != nil {
		return api.PluginInfo{}, err
	}
	if strings.TrimSpace(h.Directory) == "" {
		return api.PluginInfo{}, errors.New("未配置插件安装目录")
	}
	repo, err := ParseRepo(repoSpec)
	if err != nil {
		return api.PluginInfo{}, err
	}
	cfg := resolveGitHubConfig(ctx, &h)
	remoteSrc, err := queryLatestGitHubSource(ctx, cfg, repo)
	if err != nil {
		return api.PluginInfo{}, err
	}
	archiveBytes, err := downloadGitHubArchive(ctx, cfg, remoteSrc, repo)
	if err != nil {
		return api.PluginInfo{}, err
	}

	tempWorkDir, err := os.MkdirTemp("", "vantry-plugin-gh-*")
	if err != nil {
		return api.PluginInfo{}, err
	}
	defer os.RemoveAll(tempWorkDir)

	unpackDir := filepath.Join(tempWorkDir, "unpacked")
	if err := unpackZipBytesSafely(archiveBytes, unpackDir); err != nil {
		return api.PluginInfo{}, err
	}
	pluginRootDir, err := locatePluginManifestRoot(unpackDir)
	if err != nil {
		return api.PluginInfo{}, err
	}

	repoParts := strings.Split(repo, "/")
	fallbackID := repoParts[len(repoParts)-1]
	origin := &Origin{
		Repo:    repo,
		Version: remoteSrc.version,
	}
	return h.stageValidateAndInstall(ctx, pluginRootDir, fallbackID, origin)
}

// InstallFromLocal validates, compiles, and atomically installs a plugin from a local folder or .zip file.
func InstallFromLocal(ctx context.Context, pluginsDir, srcPath string) (api.PluginInfo, error) {
	return (Host{Directory: pluginsDir}).InstallFromLocal(ctx, srcPath)
}

// InstallFromLocal installs a local folder or .zip plugin into h.Directory.
func (h Host) InstallFromLocal(ctx context.Context, srcPath string) (api.PluginInfo, error) {
	if ctx == nil {
		return api.PluginInfo{}, errors.New("plugins: nil context")
	}
	if err := ctx.Err(); err != nil {
		return api.PluginInfo{}, err
	}
	if strings.TrimSpace(h.Directory) == "" {
		return api.PluginInfo{}, errors.New("未配置插件安装目录")
	}
	cleanSrc := filepath.Clean(strings.TrimSpace(srcPath))
	info, err := os.Lstat(cleanSrc)
	if err != nil {
		return api.PluginInfo{}, fmt.Errorf("读取本地插件路径：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return api.PluginInfo{}, errors.New("本地插件路径不能是符号链接")
	}

	tempWorkDir, err := os.MkdirTemp("", "vantry-plugin-local-*")
	if err != nil {
		return api.PluginInfo{}, err
	}
	defer os.RemoveAll(tempWorkDir)

	var pluginRootDir string
	fallbackID := strings.TrimSuffix(filepath.Base(cleanSrc), filepath.Ext(cleanSrc))

	if info.IsDir() {
		copyDest := filepath.Join(tempWorkDir, "copied")
		if err := copyPluginDirSafely(cleanSrc, copyDest); err != nil {
			return api.PluginInfo{}, err
		}
		pluginRootDir, err = locatePluginManifestRoot(copyDest)
		if err != nil {
			return api.PluginInfo{}, err
		}
	} else if info.Mode().IsRegular() {
		if info.Size() > maxArchiveDownloadBytes {
			return api.PluginInfo{}, fmt.Errorf("插件压缩包超过 %d MiB 上限", maxArchiveDownloadBytes>>20)
		}
		rawZip, err := os.ReadFile(cleanSrc)
		if err != nil {
			return api.PluginInfo{}, fmt.Errorf("读取本地压缩包：%w", err)
		}
		unpackDir := filepath.Join(tempWorkDir, "unpacked")
		if err := unpackZipBytesSafely(rawZip, unpackDir); err != nil {
			return api.PluginInfo{}, err
		}
		pluginRootDir, err = locatePluginManifestRoot(unpackDir)
		if err != nil {
			return api.PluginInfo{}, err
		}
	} else {
		return api.PluginInfo{}, errors.New("本地插件源必须是文件夹或 .zip 压缩包")
	}

	return h.stageValidateAndInstall(ctx, pluginRootDir, fallbackID, nil)
}

// CheckPluginUpdates inspects all installed plugins in pluginsDir, checking .origin.json against
// GitHub's latest release tag or default-branch head ref, and marks HasUpdate & LatestVersion.
func CheckPluginUpdates(ctx context.Context, pluginsDir string) ([]api.PluginInfo, error) {
	return (Host{Directory: pluginsDir}).CheckUpdates(ctx)
}

// CheckUpdates checks GitHub updates for all GitHub-installed plugins in h.Directory.
func (h Host) CheckUpdates(ctx context.Context) ([]api.PluginInfo, error) {
	list, err := h.ListContext(ctx)
	if err != nil {
		return nil, err
	}
	cfg := resolveGitHubConfig(ctx, &h)
	for i := range list {
		if list[i].Builtin {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pluginDir := filepath.Join(h.Directory, list[i].ID)
		orig, ok := readOrigin(pluginDir)
		if !ok || strings.TrimSpace(orig.Repo) == "" {
			continue
		}
		remoteSrc, qErr := queryLatestGitHubSource(ctx, cfg, orig.Repo)
		if qErr != nil || remoteSrc.version == "" {
			continue
		}
		latest := remoteSrc.version
		current := orig.Version
		if current == "" {
			current = list[i].Version
		}
		orig.LatestVersion = latest
		_ = writeOrigin(pluginDir, orig)

		list[i].Repo = orig.Repo
		list[i].LatestVersion = latest
		list[i].HasUpdate = current != "" && latest != current
	}
	return list, nil
}

// UpdatePlugin hot-updates pluginID in place from its recorded GitHub origin without restarting Vantry.
func UpdatePlugin(ctx context.Context, pluginsDir, pluginID string) (api.PluginInfo, error) {
	return (Host{Directory: pluginsDir}).UpdatePlugin(ctx, pluginID)
}

// UpdatePlugin hot-updates pluginID in h.Directory from its .origin.json repository.
func (h Host) UpdatePlugin(ctx context.Context, pluginID string) (api.PluginInfo, error) {
	if ctx == nil {
		return api.PluginInfo{}, errors.New("plugins: nil context")
	}
	if err := ctx.Err(); err != nil {
		return api.PluginInfo{}, err
	}
	if !isValidPluginID(pluginID) {
		return api.PluginInfo{}, fmt.Errorf("非法插件 ID：%q", pluginID)
	}
	pluginDir := filepath.Join(h.Directory, pluginID)
	if _, err := os.Stat(pluginDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return api.PluginInfo{}, fmt.Errorf("插件 %q 不存在", pluginID)
		}
		return api.PluginInfo{}, err
	}
	orig, ok := readOrigin(pluginDir)
	if !ok || strings.TrimSpace(orig.Repo) == "" {
		return api.PluginInfo{}, fmt.Errorf("插件 %q 未记录 GitHub 来源，无法在线更新", pluginID)
	}
	return h.InstallFromGitHub(ctx, orig.Repo)
}

// RemovePlugin removes an installed plugin and its .plugin-data folder.
func RemovePlugin(pluginsDir, pluginID string) error {
	return (Host{Directory: pluginsDir}).RemovePlugin(pluginID)
}

// RemovePlugin removes pluginID from h.Directory and cleans up its per-plugin data folder.
func (h Host) RemovePlugin(pluginID string) error {
	if !isValidPluginID(pluginID) {
		return fmt.Errorf("非法插件 ID：%q", pluginID)
	}
	if strings.TrimSpace(h.Directory) == "" {
		return errors.New("未配置插件目录")
	}
	targetDir := filepath.Join(h.Directory, pluginID)
	if _, err := os.Lstat(targetDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("插件 %q 不存在：%w", pluginID, err)
		}
		return err
	}
	if err := os.RemoveAll(targetDir); err != nil {
		return fmt.Errorf("删除插件目录失败：%w", err)
	}
	if dataDir, err := h.pluginDataDir(pluginID); err == nil {
		pluginStateMu.Lock()
		_ = os.RemoveAll(dataDir)
		pluginStateMu.Unlock()
	}
	return nil
}

func (h Host) stageValidateAndInstall(
	ctx context.Context,
	extractedPluginDir string,
	fallbackID string,
	origin *Origin,
) (api.PluginInfo, error) {
	if err := os.MkdirAll(h.Directory, 0o700); err != nil {
		return api.PluginInfo{}, fmt.Errorf("创建插件目录：%w", err)
	}

	rawManifestBytes, err := readPluginFile(extractedPluginDir, "plugin.json", maxManifestBytes)
	if err != nil {
		return api.PluginInfo{}, fmt.Errorf("读取安装包 plugin.json：%w", err)
	}
	var parsed rawManifest
	if err := json.Unmarshal(rawManifestBytes, &parsed); err != nil {
		return api.PluginInfo{}, fmt.Errorf("解析安装包 plugin.json：%w", err)
	}
	pluginID := strings.TrimSpace(parsed.ID)
	if pluginID == "" && parsed.Main != "" && isValidPluginID(fallbackID) {
		pluginID = fallbackID
	}
	if !isValidPluginID(pluginID) {
		return api.PluginInfo{}, fmt.Errorf("安装包 plugin.json 的 id %q 非法", parsed.ID)
	}

	seq := stagingSeq.Add(1)
	stageContainer := filepath.Join(h.Directory, fmt.Sprintf(".staging-%s-%d", pluginID, seq))
	if err := os.MkdirAll(stageContainer, 0o700); err != nil {
		return api.PluginInfo{}, fmt.Errorf("创建插件暂存目录：%w", err)
	}
	defer os.RemoveAll(stageContainer)

	stagedPluginDir := filepath.Join(stageContainer, pluginID)
	if err := copyPluginDirSafely(extractedPluginDir, stagedPluginDir); err != nil {
		return api.PluginInfo{}, err
	}

	if origin != nil {
		ver := strings.TrimSpace(origin.Version)
		if ver == "" {
			ver = strings.TrimSpace(parsed.Version)
		}
		if ver == "" {
			ver = "main"
		}
		if err := writeOrigin(stagedPluginDir, Origin{
			Repo:    origin.Repo,
			Version: ver,
		}); err != nil {
			return api.PluginInfo{}, fmt.Errorf("写入插件来源元数据：%w", err)
		}
	}

	// Validate declarative panels (or resolve process entry) before replacement.
	validatorHost := h
	validatorHost.Directory = stageContainer
	validatorHost.IncludeBuiltins = false
	stagedDef := validatorHost.definitionFromDir(pluginID, stagedPluginDir)
	stagedInfo := validatorHost.buildPluginInfo(ctx, stagedDef)
	if stagedInfo.Error != "" {
		return api.PluginInfo{}, fmt.Errorf("验证插件 %q 失败：%s", pluginID, stagedInfo.Error)
	}

	finalDir := filepath.Join(h.Directory, pluginID)
	if err := atomicReplaceDir(stagedPluginDir, finalDir, h.Directory, pluginID); err != nil {
		return api.PluginInfo{}, err
	}

	liveDef := h.definitionFromDir(pluginID, finalDir)
	return h.buildPluginInfo(ctx, liveDef), nil
}

func atomicReplaceDir(stagedDir, finalDir, parentDir, pluginID string) error {
	if _, err := os.Lstat(finalDir); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(stagedDir, finalDir); err != nil {
			return fmt.Errorf("启用插件目录失败：%w", err)
		}
		return nil
	} else if err != nil {
		return err
	}

	backupDir := filepath.Join(parentDir, fmt.Sprintf(".backup-%s-%d", pluginID, stagingSeq.Add(1)))
	if err := os.Rename(finalDir, backupDir); err != nil {
		return fmt.Errorf("备份旧版插件目录失败：%w", err)
	}
	if err := os.Rename(stagedDir, finalDir); err != nil {
		_ = os.Rename(backupDir, finalDir)
		return fmt.Errorf("替换新版插件目录失败：%w", err)
	}
	_ = os.RemoveAll(backupDir)
	return nil
}

func unpackZipBytesSafely(archiveBytes []byte, destDir string) error {
	zr, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
	if err != nil {
		return fmt.Errorf("无效的 zip 压缩包：%w", err)
	}
	if len(zr.File) > maxArchiveFiles {
		return fmt.Errorf("压缩包条目数 %d 超过上限 %d", len(zr.File), maxArchiveFiles)
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	var totalExtracted int64
	fileCount := 0

	for _, zf := range zr.File {
		rawName := zf.Name
		if rawName == "" || strings.ContainsRune(rawName, 0) || strings.ContainsRune(rawName, '\\') {
			return fmt.Errorf("压缩包包含非法路径 %q（zip-slip 防护拦截）", rawName)
		}
		if strings.HasPrefix(rawName, "/") || filepath.IsAbs(rawName) {
			return fmt.Errorf("压缩包包含绝对路径 %q（zip-slip 防护拦截）", rawName)
		}
		if len(rawName) >= 2 && rawName[1] == ':' {
			return fmt.Errorf("压缩包包含盘符路径 %q（zip-slip 防护拦截）", rawName)
		}
		segs := strings.Split(rawName, "/")
		if slices.Contains(segs, "..") {
			return fmt.Errorf("压缩包包含越界路径 %q（zip-slip 防护拦截）", rawName)
		}
		cleanRel := path.Clean(strings.TrimSuffix(rawName, "/"))
		if cleanRel == "." || cleanRel == "" {
			continue
		}
		if cleanRel == ".." || strings.HasPrefix(cleanRel, "../") {
			return fmt.Errorf("压缩包路径越界 %q", rawName)
		}
		if cleanRel == "__MACOSX" || strings.HasPrefix(cleanRel, "__MACOSX/") {
			continue
		}

		targetPath := filepath.Join(absDest, filepath.FromSlash(cleanRel))
		relCheck, err := filepath.Rel(absDest, targetPath)
		if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
			return fmt.Errorf("压缩包路径越界 %q（zip-slip 防护拦截）", rawName)
		}

		mode := zf.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("压缩包不允许包含符号链接：%q", rawName)
		}
		if zf.FileInfo().IsDir() || strings.HasSuffix(rawName, "/") {
			if err := os.MkdirAll(targetPath, 0o700); err != nil {
				return err
			}
			continue
		}
		if !mode.IsRegular() {
			return fmt.Errorf("压缩包不允许包含特殊文件：%q", rawName)
		}

		fileCount++
		if fileCount > maxArchiveFiles {
			return fmt.Errorf("压缩包文件数量超过上限 %d", maxArchiveFiles)
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
			return err
		}

		rc, err := zf.Open()
		if err != nil {
			return fmt.Errorf("解压文件 %q：%w", rawName, err)
		}
		remainingArchive := int64(maxArchiveTotalBytes) - totalExtracted
		maxThisFile := int64(maxArchiveSingleFileBytes)
		if remainingArchive < maxThisFile {
			maxThisFile = remainingArchive
		}
		if maxThisFile < 0 {
			_ = rc.Close()
			return fmt.Errorf("解压总体积超过 %d MiB 上限", maxArchiveTotalBytes>>20)
		}

		perm := os.FileMode(0o600)
		if mode.Perm()&0o111 != 0 {
			perm = 0o700
		}
		outFile, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
		if err != nil {
			_ = rc.Close()
			return err
		}
		written, copyErr := io.Copy(outFile, io.LimitReader(rc, maxThisFile+1))
		closeErr := outFile.Close()
		_ = rc.Close()
		if copyErr != nil {
			return fmt.Errorf("写入解压文件 %q：%w", rawName, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if written > maxThisFile {
			return fmt.Errorf("解压文件 %q 超过体积上限", rawName)
		}
		totalExtracted += written
	}
	return nil
}

func locatePluginManifestRoot(unpackDir string) (string, error) {
	if st, err := os.Lstat(filepath.Join(unpackDir, "plugin.json")); err == nil && st.Mode().IsRegular() {
		return unpackDir, nil
	}
	entries, err := os.ReadDir(unpackDir)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		name := e.Name()
		if name == ".DS_Store" || name == "__MACOSX" {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			return "", errors.New("安装包内不能包含符号链接")
		}
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(unpackDir, name))
		}
	}
	if len(dirs) == 1 {
		if st, err := os.Lstat(filepath.Join(dirs[0], "plugin.json")); err == nil && st.Mode().IsRegular() {
			return dirs[0], nil
		}
	}
	return "", errors.New("未在安装包中找到 plugin.json")
}

func copyPluginDirSafely(srcDir, dstDir string) error {
	var totalFiles int
	var totalBytes int64

	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(srcDir, func(curPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, curPath)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		base := d.Name()
		if base == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("插件目录不允许包含符号链接：%q", rel)
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("插件目录包含非常规文件：%q", rel)
		}
		totalFiles++
		if totalFiles > maxArchiveFiles {
			return fmt.Errorf("插件文件数量超过上限 %d", maxArchiveFiles)
		}
		if info.Size() > maxArchiveSingleFileBytes {
			return fmt.Errorf("插件文件 %q 超过体积上限", rel)
		}
		totalBytes += info.Size()
		if totalBytes > maxArchiveTotalBytes {
			return fmt.Errorf("插件总体积超过 %d MiB 上限", maxArchiveTotalBytes>>20)
		}
		data, err := os.ReadFile(curPath)
		if err != nil {
			return err
		}
		perm := os.FileMode(0o600)
		if info.Mode().Perm()&0o111 != 0 {
			perm = 0o700
		}
		return writeAtomicFile(target, data, perm)
	})
}
