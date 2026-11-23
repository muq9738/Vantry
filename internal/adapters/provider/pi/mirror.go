package pi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	MirrorModeAuto   = "auto"
	MirrorModeCN     = "cn"
	MirrorModeDirect = "direct"

	DefaultNPMRegistry = "https://registry.npmjs.org"
	ChinaNPMRegistry   = "https://registry.npmmirror.com"
	PiNPMPackage       = "@earendil-works/pi-coding-agent"

	mirrorProbeTimeout = 2500 * time.Millisecond
	mirrorCacheTTL     = 10 * time.Minute
)

// NetworkRoute describes the selected npm registry route for public external
// Pi package checks, preflight verification, and upgrades. Never send private
// Vantry repository tokens through any third-party mirror.
type NetworkRoute struct {
	Mode        string    `json:"mode"`
	Region      string    `json:"region"` // "cn" | "direct"
	NPMRegistry string    `json:"npmRegistry"`
	Summary     string    `json:"summary"`
	CheckedAt   time.Time `json:"checkedAt"`
}

// MirrorOptions customizes npm mirror detection; zero values probe the official
// registry and China Taobao mirror in parallel with caching enabled.
type MirrorOptions struct {
	Mode        string
	NPMRegistry string
	HTTPClient  *http.Client
	DirectNPM   string
	ChinaNPM    string
	ChinaHint   *bool
	BypassCache bool
}

var (
	mirrorCacheMu      sync.Mutex
	mirrorCacheKey     string
	mirrorCacheRoute   NetworkRoute
	mirrorCacheExpires time.Time
)

// InvalidateMirrorRouteCache clears the cached network route probe result so
// subsequent checks re-evaluate mirrors immediately.
func InvalidateMirrorRouteCache() {
	mirrorCacheMu.Lock()
	mirrorCacheKey = ""
	mirrorCacheRoute = NetworkRoute{}
	mirrorCacheExpires = time.Time{}
	mirrorCacheMu.Unlock()
}

func normalizeMirrorMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case MirrorModeCN:
		return MirrorModeCN
	case MirrorModeDirect:
		return MirrorModeDirect
	default:
		return MirrorModeAuto
	}
}

func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || strings.TrimSpace(u.Host) == "" {
		return "", fmt.Errorf("镜像地址无效（须为 http 或 https URL）：%s", raw)
	}
	return strings.TrimRight(trimmed, "/"), nil
}

// DetectNetworkRoute selects the best npm registry for external Pi checks and upgrades.
func DetectNetworkRoute(ctx context.Context, opts MirrorOptions) NetworkRoute {
	opts = resolveMirrorOptions(opts)
	useCache := opts.HTTPClient == nil &&
		!opts.BypassCache &&
		opts.ChinaHint == nil &&
		opts.DirectNPM == DefaultNPMRegistry &&
		opts.ChinaNPM == ChinaNPMRegistry

	cacheKey := opts.Mode + "\x00" + opts.NPMRegistry
	if useCache {
		mirrorCacheMu.Lock()
		if mirrorCacheKey == cacheKey && time.Now().Before(mirrorCacheExpires) {
			cached := mirrorCacheRoute
			mirrorCacheMu.Unlock()
			return cached
		}
		mirrorCacheMu.Unlock()
	}

	route := computeNetworkRoute(ctx, opts)
	if useCache {
		mirrorCacheMu.Lock()
		mirrorCacheKey = cacheKey
		mirrorCacheRoute = route
		mirrorCacheExpires = time.Now().Add(mirrorCacheTTL)
		mirrorCacheMu.Unlock()
	}
	return route
}

func resolveMirrorOptions(opts MirrorOptions) MirrorOptions {
	if strings.TrimSpace(opts.Mode) == "" {
		opts.Mode = os.Getenv("VANTRY_MIRROR_MODE")
	}
	opts.Mode = normalizeMirrorMode(opts.Mode)
	if strings.TrimSpace(opts.NPMRegistry) == "" {
		opts.NPMRegistry = os.Getenv("VANTRY_NPM_REGISTRY")
	}
	if normalized, err := normalizeBaseURL(opts.NPMRegistry); err == nil {
		opts.NPMRegistry = normalized
	} else {
		opts.NPMRegistry = ""
	}
	if strings.TrimSpace(opts.DirectNPM) == "" {
		opts.DirectNPM = DefaultNPMRegistry
	} else {
		opts.DirectNPM = strings.TrimRight(strings.TrimSpace(opts.DirectNPM), "/")
	}
	if strings.TrimSpace(opts.ChinaNPM) == "" {
		opts.ChinaNPM = ChinaNPMRegistry
	} else {
		opts.ChinaNPM = strings.TrimRight(strings.TrimSpace(opts.ChinaNPM), "/")
	}
	return opts
}

type mirrorProbeResult struct {
	target  string
	ok      bool
	latency time.Duration
}

func computeNetworkRoute(ctx context.Context, opts MirrorOptions) NetworkRoute {
	now := time.Now()
	if opts.NPMRegistry != "" {
		region := opts.Mode
		if region == MirrorModeAuto {
			region = MirrorModeDirect
			if strings.Contains(opts.NPMRegistry, "npmmirror") || strings.Contains(opts.NPMRegistry, ".cn") {
				region = MirrorModeCN
			}
		}
		return NetworkRoute{
			Mode:        opts.Mode,
			Region:      region,
			NPMRegistry: opts.NPMRegistry,
			Summary:     buildCustomRouteSummary(opts.NPMRegistry),
			CheckedAt:   now,
		}
	}

	if opts.Mode == MirrorModeDirect {
		return NetworkRoute{
			Mode:        MirrorModeDirect,
			Region:      MirrorModeDirect,
			NPMRegistry: opts.DirectNPM,
			Summary:     buildRouteSummary(MirrorModeDirect, opts.DirectNPM),
			CheckedAt:   now,
		}
	}

	if opts.Mode == MirrorModeCN {
		return NetworkRoute{
			Mode:        MirrorModeCN,
			Region:      MirrorModeCN,
			NPMRegistry: opts.ChinaNPM,
			Summary:     buildRouteSummary(MirrorModeCN, opts.ChinaNPM),
			CheckedAt:   now,
		}
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: mirrorProbeTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	var (
		wg        sync.WaitGroup
		npmDirect mirrorProbeResult
		npmChina  mirrorProbeResult
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		npmDirect = probeURL(ctx, client, opts.DirectNPM+"/"+PiNPMPackage+"/latest")
	}()
	go func() {
		defer wg.Done()
		npmChina = probeURL(ctx, client, opts.ChinaNPM+"/"+PiNPMPackage+"/latest")
	}()
	wg.Wait()

	cnHint := isChinaEnvironment()
	if opts.ChinaHint != nil {
		cnHint = *opts.ChinaHint
	}

	npmReg := chooseNPMRegistry(npmDirect, npmChina, opts.DirectNPM, opts.ChinaNPM, cnHint)
	region := MirrorModeDirect
	if npmReg == opts.ChinaNPM {
		region = MirrorModeCN
	}
	return NetworkRoute{
		Mode:        MirrorModeAuto,
		Region:      region,
		NPMRegistry: npmReg,
		Summary:     buildRouteSummary(region, npmReg),
		CheckedAt:   now,
	}
}

func probeURL(ctx context.Context, client *http.Client, rawURL string) mirrorProbeResult {
	probeCtx, cancel := context.WithTimeout(ctx, mirrorProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return mirrorProbeResult{target: rawURL}
	}
	req.Header.Set("User-Agent", "Vantry-Pi-Probe/1.0")
	req.Header.Set("Accept", "application/json")
	started := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(started)
	if err != nil {
		return mirrorProbeResult{target: rawURL, latency: latency}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	return mirrorProbeResult{target: rawURL, ok: ok, latency: latency}
}

func chooseNPMRegistry(direct, china mirrorProbeResult, directURL, chinaURL string, cnHint bool) string {
	switch {
	case china.ok && !direct.ok:
		return chinaURL
	case direct.ok && !china.ok:
		return directURL
	case china.ok && direct.ok:
		if cnHint && china.latency <= direct.latency*12/10 {
			return chinaURL
		}
		if china.latency < direct.latency {
			return chinaURL
		}
		return directURL
	case cnHint:
		return chinaURL
	default:
		return directURL
	}
}

func isChinaEnvironment() bool {
	if strings.TrimSpace(os.Getenv("HTTPS_PROXY")) != "" ||
		strings.TrimSpace(os.Getenv("https_proxy")) != "" ||
		strings.TrimSpace(os.Getenv("ALL_PROXY")) != "" ||
		strings.TrimSpace(os.Getenv("all_proxy")) != "" {
		return false
	}
	tz := strings.ToLower(strings.TrimSpace(os.Getenv("TZ")))
	for _, cnTZ := range []string{"asia/shanghai", "asia/chongqing", "asia/harbin", "asia/urumqi", "prc"} {
		if strings.Contains(tz, cnTZ) {
			return true
		}
	}
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		val := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
		if strings.HasPrefix(val, "zh_cn") || strings.HasPrefix(val, "zh-hans") {
			return true
		}
	}
	name, offset := time.Now().Zone()
	return offset == 8*3600 && strings.EqualFold(name, "CST")
}

func hostFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimSuffix(raw, "/")
}

func buildRouteSummary(region, npmRegistry string) string {
	host := hostFromURL(npmRegistry)
	prefix := "官方直连"
	if region == MirrorModeCN {
		prefix = "国内镜像"
	}
	if host != "" {
		return prefix + "（" + host + "）"
	}
	return prefix
}

func buildCustomRouteSummary(npmRegistry string) string {
	host := hostFromURL(npmRegistry)
	if host == "" {
		return "自定义镜像"
	}
	return "自定义镜像（" + host + "）"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
