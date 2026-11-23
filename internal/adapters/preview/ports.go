package preview

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MinDevPort and MaxDevPort gate every probe. Privileged ports are refused
	// so a probe can never reach a system service by accident.
	MinDevPort = 1024
	MaxDevPort = 65535
	// ProbeTimeout bounds one port probe, including the HTTP metadata request.
	ProbeTimeout = 1500 * time.Millisecond
	// MaxProbeBody bounds how much of the response is read for the title.
	MaxProbeBody = 32 << 10

	maxPortStatusMessage = 200
	maxTitleRunes        = 200
	maxProbePathLength   = 512
	maxScanWorkers       = 8
)

// DefaultDevPorts are the common Vite, Next, Astro, Rails and Go development
// ports probed by ScanActiveDevPorts when the caller passes no list.
var DefaultDevPorts = []int{3000, 3001, 4173, 4321, 5173, 5174, 5175, 8000, 8080, 8081, 9000}

// PortStatus is the outcome of one probe. Listening reports a TCP listener;
// HTTP, StatusCode and Title are filled only when the service answered HTTP.
type PortStatus struct {
	Port       int    `json:"port"`
	Address    string `json:"address"`
	URL        string `json:"url"`
	Listening  bool   `json:"listening"`
	HTTP       bool   `json:"http"`
	StatusCode int    `json:"statusCode,omitempty"`
	Title      string `json:"title,omitempty"`
	LatencyMS  int64  `json:"latencyMs"`
	Error      string `json:"error,omitempty"`
}

// ValidDevPort reports whether a port may be probed.
func ValidDevPort(port int) bool {
	return port >= MinDevPort && port <= MaxDevPort
}

// ProbeLocalPort checks one loopback development port. A closed port is a
// normal result, not an error; only an invalid port is rejected.
func ProbeLocalPort(ctx context.Context, port int) (PortStatus, error) {
	if !ValidDevPort(port) {
		return PortStatus{}, fmt.Errorf("端口 %d 不在 %d..%d 的本地开发端口范围内", port, MinDevPort, MaxDevPort)
	}
	return probePort(ctx, port, "/")
}

// ProbeLoopbackURL probes an Agent-supplied preview URL. Only loopback hosts on
// development ports are accepted, so a URL can never turn into a scan of the
// local network or a metadata service.
func ProbeLoopbackURL(ctx context.Context, rawURL string) (PortStatus, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return PortStatus{}, errors.New("预览地址不能为空")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return PortStatus{}, errors.New("预览地址无效")
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return PortStatus{}, fmt.Errorf("仅支持探测 http 本地开发地址：%s", trimmed)
	}
	host := parsed.Hostname()
	if !loopbackHost(host) {
		return PortStatus{}, fmt.Errorf("只允许探测本机环回地址，拒绝 %q", host)
	}
	port := 80
	if raw := parsed.Port(); raw != "" {
		parsedPort, err := strconv.Atoi(raw)
		if err != nil {
			return PortStatus{}, fmt.Errorf("预览端口无效：%q", raw)
		}
		port = parsedPort
	}
	if !ValidDevPort(port) {
		return PortStatus{}, fmt.Errorf("端口 %d 不在 %d..%d 的本地开发端口范围内", port, MinDevPort, MaxDevPort)
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if len(path) > maxProbePathLength {
		return PortStatus{}, errors.New("预览路径过长")
	}
	return probePort(ctx, port, path)
}

// ScanActiveDevPorts probes candidate ports concurrently and returns the
// listening services sorted by port. Invalid and duplicate candidates are
// skipped; the result is never nil so JSON encoding stays an array.
func ScanActiveDevPorts(ctx context.Context, candidatePorts []int) []PortStatus {
	if len(candidatePorts) == 0 {
		candidatePorts = DefaultDevPorts
	}
	seen := make(map[int]bool, len(candidatePorts))
	ports := make([]int, 0, len(candidatePorts))
	for _, port := range candidatePorts {
		if !ValidDevPort(port) || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	statuses := make([]PortStatus, len(ports))
	active := make([]bool, len(ports))
	if len(ports) == 0 {
		return []PortStatus{}
	}
	sort.Ints(ports)
	limit := make(chan struct{}, min(len(ports), maxScanWorkers))
	var group sync.WaitGroup
	for index, port := range ports {
		group.Add(1)
		go func(index, port int) {
			defer group.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			if ctx.Err() != nil {
				return
			}
			status, err := ProbeLocalPort(ctx, port)
			if err != nil || !status.Listening {
				return
			}
			statuses[index] = status
			active[index] = true
		}(index, port)
	}
	group.Wait()
	result := make([]PortStatus, 0, len(ports))
	for index := range statuses {
		if active[index] {
			result = append(result, statuses[index])
		}
	}
	return result
}

// probePort tries the IPv4 loopback first and falls back to ::1, sharing one
// timeout budget so a probe stays fast even when both addresses hang.
func probePort(ctx context.Context, port int, path string) (PortStatus, error) {
	deadline := time.Now().Add(ProbeTimeout)
	var primary PortStatus
	for index, host := range []string{"127.0.0.1", "::1"} {
		remaining := time.Until(deadline)
		if remaining <= 0 || ctx.Err() != nil {
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, remaining)
		status := probeHost(attemptCtx, host, port, path)
		cancel()
		if index == 0 {
			primary = status
		}
		if status.Listening {
			return status, nil
		}
	}
	return primary, nil
}

func probeHost(ctx context.Context, host string, port int, path string) PortStatus {
	started := time.Now()
	address := net.JoinHostPort(host, strconv.Itoa(port))
	status := PortStatus{
		Port:    port,
		Address: address,
		URL:     "http://" + address + path,
	}
	dialer := net.Dialer{Timeout: ProbeTimeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		status.LatencyMS = elapsedMilliseconds(started)
		status.Error = shortProbeError(err)
		return status
	}
	connection.Close()
	status.Listening = true
	code, title, err := fetchDocumentMetadata(ctx, address, path)
	status.LatencyMS = elapsedMilliseconds(started)
	if err != nil {
		status.Error = shortProbeError(err)
		return status
	}
	status.HTTP = true
	status.StatusCode = code
	status.Title = title
	return status
}

// fetchDocumentMetadata issues one bounded GET / and extracts the page title.
// The client is pinned to the probed address, ignores proxies, never follows a
// redirect and never reads more than MaxProbeBody.
func fetchDocumentMetadata(ctx context.Context, address, path string) (int, string, error) {
	dialer := &net.Dialer{Timeout: ProbeTimeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   false,
		TLSHandshakeTimeout: ProbeTimeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1")
	request.Header.Set("User-Agent", "Vantry-Preview-Probe/1.0")
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxProbeBody))
	if err != nil && len(body) == 0 {
		return response.StatusCode, "", err
	}
	title := ""
	if isHTMLContentType(response.Header.Get("Content-Type")) {
		title = documentTitle(body)
	}
	return response.StatusCode, title, nil
}

func isHTMLContentType(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return false
	}
	return mediaType == "text/html" || mediaType == "application/xhtml+xml"
}

// documentTitle extracts the first <title> text. It works on the bounded body,
// so a truncated document yields either an empty title or its prefix.
func documentTitle(body []byte) string {
	text := string(body)
	lower := strings.ToLower(text)
	opening := strings.Index(lower, "<title")
	if opening < 0 {
		return ""
	}
	afterName := opening + len("<title")
	closing := strings.IndexByte(text[afterName:], '>')
	if closing < 0 {
		return ""
	}
	content := text[afterName+closing+1:]
	lowerContent := strings.ToLower(content)
	if end := strings.Index(lowerContent, "</title"); end >= 0 {
		content = content[:end]
	} else if markup := strings.IndexByte(content, '<'); markup >= 0 {
		content = content[:markup]
	}
	title := html.UnescapeString(content)
	return truncateText(strings.Join(strings.Fields(title), " "), maxTitleRunes)
}

func loopbackHost(host string) bool {
	trimmed := strings.Trim(host, "[]")
	if strings.EqualFold(trimmed, "localhost") {
		return true
	}
	address := net.ParseIP(trimmed)
	return address != nil && address.IsLoopback()
}

func elapsedMilliseconds(started time.Time) int64 {
	return time.Since(started).Milliseconds()
}

func shortProbeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "探测超时"
	}
	if errors.Is(err, context.Canceled) {
		return "探测已取消"
	}
	message := err.Error()
	var operationError *net.OpError
	if errors.As(err, &operationError) && operationError.Err != nil {
		message = operationError.Err.Error()
	}
	return truncateText(message, maxPortStatusMessage)
}

func truncateText(value string, limit int) string {
	cleaned := strings.ToValidUTF8(value, "")
	if utf8.RuneCountInString(cleaned) <= limit {
		return cleaned
	}
	return string([]rune(cleaned)[:limit]) + "…"
}
