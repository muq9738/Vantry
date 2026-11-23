package preview

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// portOf returns the listening port of a test server.
func portOf(t *testing.T, server *httptest.Server) int {
	t.Helper()
	address, ok := server.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener %T", server.Listener.Addr())
	}
	return address.Port
}

func TestProbeLocalPortReportsHTTPTitle(t *testing.T) {
	var mu sync.Mutex
	var method, path, host string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		method, path, host = request.Method, request.URL.Path, request.Host
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`<!doctype html><html><head><title>  My Dev
			App — 预览 </title></head><body></body></html>`))
	}))
	defer server.Close()
	port := portOf(t, server)

	status, err := ProbeLocalPort(context.Background(), port)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Listening || !status.HTTP {
		t.Fatalf("status = %+v, want a listening HTTP service", status)
	}
	if status.Port != port {
		t.Fatalf("port = %d, want %d", status.Port, port)
	}
	if status.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d", status.StatusCode)
	}
	if status.Title != "My Dev App — 预览" {
		t.Fatalf("title = %q", status.Title)
	}
	if status.Error != "" {
		t.Fatalf("unexpected error text: %q", status.Error)
	}
	if status.Address != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) {
		t.Fatalf("address = %q", status.Address)
	}
	if status.URL != "http://"+status.Address+"/" {
		t.Fatalf("url = %q", status.URL)
	}
	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodGet || path != "/" || host != status.Address {
		t.Fatalf("request = %s %s host %s, want GET / host %s", method, path, host, status.Address)
	}
}

func TestProbeLocalPortHandlesNonHTMLResponses(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write([]byte(`<title>not html</title>`))
	}))
	defer plain.Close()
	status, err := ProbeLocalPort(context.Background(), portOf(t, plain))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Listening || !status.HTTP || status.StatusCode != http.StatusOK {
		t.Fatalf("status = %+v", status)
	}
	if status.Title != "" {
		t.Fatalf("title taken from a non-html response: %q", status.Title)
	}

	missing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`<html><head><title> Not Found </title></head></html>`))
	}))
	defer missing.Close()
	status, err = ProbeLocalPort(context.Background(), portOf(t, missing))
	if err != nil {
		t.Fatal(err)
	}
	if status.StatusCode != http.StatusNotFound || status.Title != "Not Found" {
		t.Fatalf("status = %+v, want the error page metadata", status)
	}
}

func TestProbeLocalPortReportsNonHTTPService(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connection.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port

	status, err := ProbeLocalPort(context.Background(), port)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Listening {
		t.Fatalf("raw listener not detected: %+v", status)
	}
	if status.HTTP {
		t.Fatalf("raw listener reported as HTTP: %+v", status)
	}
	if status.Error == "" {
		t.Fatalf("expected a diagnostic for a non-HTTP service: %+v", status)
	}
	listener.Close()
	<-done
}

func TestProbeLocalPortReportsClosedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	status, err := ProbeLocalPort(context.Background(), port)
	if err != nil {
		t.Fatalf("a closed port is a normal result: %v", err)
	}
	if status.Listening || status.HTTP || status.StatusCode != 0 {
		t.Fatalf("status = %+v, want a closed port", status)
	}
	if status.Port != port || status.Address != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) {
		t.Fatalf("address = %+v", status)
	}
}

func TestProbeLocalPortRejectsInvalidPorts(t *testing.T) {
	for _, port := range []int{-1, 0, 80, 1023, MaxDevPort + 1, 70000} {
		if _, err := ProbeLocalPort(context.Background(), port); err == nil {
			t.Fatalf("port %d accepted", port)
		}
	}
	for _, port := range []int{MinDevPort, 3000, MaxDevPort} {
		if !ValidDevPort(port) {
			t.Fatalf("port %d rejected", port)
		}
	}
}

func TestScanActiveDevPortsFindsListeningServices(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("<html><head><title>first</title></head></html>"))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("<html><head><title>second</title></head></html>"))
	}))
	defer second.Close()
	ports := []int{portOf(t, second), portOf(t, first), portOf(t, first), 0, 80, 70000}

	statuses := ScanActiveDevPorts(context.Background(), ports)
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want two services", statuses)
	}
	if statuses[0].Port >= statuses[1].Port {
		t.Fatalf("statuses are not sorted by port: %+v", statuses)
	}
	titles := map[string]string{}
	for _, status := range statuses {
		if !status.Listening || !status.HTTP {
			t.Fatalf("status = %+v", status)
		}
		titles[strconv.Itoa(status.Port)] = status.Title
	}
	if titles[strconv.Itoa(portOf(t, first))] != "first" || titles[strconv.Itoa(portOf(t, second))] != "second" {
		t.Fatalf("titles = %v", titles)
	}
}

func TestScanActiveDevPortsIgnoresInvalidCandidates(t *testing.T) {
	statuses := ScanActiveDevPorts(context.Background(), []int{-3, 0, 22, 80, 70000})
	if statuses == nil || len(statuses) != 0 {
		t.Fatalf("statuses = %v, want an empty array", statuses)
	}
}

func TestScanActiveDevPortsHonorsCancellationAndDefaults(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if statuses := ScanActiveDevPorts(cancelled, nil); len(statuses) != 0 {
		t.Fatalf("cancelled scan returned %+v", statuses)
	}

	ctx, stop := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer stop()
	allowed := make(map[int]bool, len(DefaultDevPorts))
	for _, port := range DefaultDevPorts {
		allowed[port] = true
	}
	for _, status := range ScanActiveDevPorts(ctx, nil) {
		if !allowed[status.Port] {
			t.Fatalf("unexpected default port %d", status.Port)
		}
		if !status.Listening {
			t.Fatalf("inactive port returned: %+v", status)
		}
	}
}

func TestProbeLoopbackURLAcceptsLocalhost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/status" {
			t.Errorf("probed path = %q", request.URL.Path)
		}
		_, _ = writer.Write([]byte("<html><head><title>app</title></head></html>"))
	}))
	defer server.Close()
	port := portOf(t, server)

	for _, raw := range []string{
		"http://127.0.0.1:" + strconv.Itoa(port) + "/status?probe=1",
		"http://localhost:" + strconv.Itoa(port) + "/status",
		"http://[::1]:" + strconv.Itoa(port) + "/status",
	} {
		status, err := ProbeLoopbackURL(context.Background(), raw)
		if err != nil {
			t.Fatalf("ProbeLoopbackURL(%q): %v", raw, err)
		}
		if !status.Listening || !status.HTTP || status.Title != "app" {
			t.Fatalf("ProbeLoopbackURL(%q) = %+v", raw, status)
		}
	}
}

func TestProbeLoopbackURLRejectsNonLoopbackTargets(t *testing.T) {
	for _, raw := range []string{
		"",
		"http://192.168.1.10:5173/",
		"http://10.0.0.5:3000/",
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1@169.254.169.254:8080/",
		"http://example.com:3000/",
		"https://127.0.0.1:5173/",
		"file:///etc/passwd",
		"http://127.0.0.1:80/",
		"http://127.0.0.1:70000/",
		"http://127.0.0.1:not-a-port/",
	} {
		if _, err := ProbeLoopbackURL(context.Background(), raw); err == nil {
			t.Fatalf("ProbeLoopbackURL(%q) accepted", raw)
		}
	}
}

func TestDocumentTitleIsBoundedAndDecoded(t *testing.T) {
	title := documentTitle([]byte("<html><head><TITLE>A &amp; B &#8212; dev</TITLE></head></html>"))
	if title != "A & B — dev" {
		t.Fatalf("title = %q", title)
	}
	if got := documentTitle([]byte("<html><body>no title</body></html>")); got != "" {
		t.Fatalf("title = %q, want empty", got)
	}
	long := documentTitle([]byte("<title>" + strings.Repeat("x", 400) + "</title>"))
	if len([]rune(long)) != maxTitleRunes+1 {
		t.Fatalf("long title length = %d", len([]rune(long)))
	}
	if got := documentTitle([]byte("<title>unterminated prefix")); got != "unterminated prefix" {
		t.Fatalf("truncated title = %q", got)
	}
}

func TestLoopbackHostValidation(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.1.1", "::1", "[::1]", "localhost", "LOCALHOST"} {
		if !loopbackHost(host) {
			t.Fatalf("host %q rejected", host)
		}
	}
	for _, host := range []string{"", "0.0.0.0", "192.168.0.1", "example.com", "169.254.169.254", "::2", "localhost.example.com"} {
		if loopbackHost(host) {
			t.Fatalf("host %q accepted", host)
		}
	}
}
