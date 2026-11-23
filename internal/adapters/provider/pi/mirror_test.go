package pi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDetectNetworkRouteAutoSelectsCNMirrorWhenDirectFails(t *testing.T) {
	npmDirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusBadGateway)
	}))
	defer npmDirect.Close()

	npmCN := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":"1.6.0"}`))
	}))
	defer npmCN.Close()

	cnHint := true
	route := DetectNetworkRoute(context.Background(), MirrorOptions{
		Mode:        MirrorModeAuto,
		DirectNPM:   npmDirect.URL,
		ChinaNPM:    npmCN.URL,
		ChinaHint:   &cnHint,
		HTTPClient:  &http.Client{Timeout: 2 * time.Second},
		BypassCache: true,
	})
	if route.Region != MirrorModeCN {
		t.Fatalf("expected CN region, got %+v", route)
	}
	if route.NPMRegistry != npmCN.URL {
		t.Fatalf("expected CN npm registry %s, got %s", npmCN.URL, route.NPMRegistry)
	}
}

func TestDetectNetworkRouteAutoSelectsDirectWhenFast(t *testing.T) {
	npmDirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer npmDirect.Close()

	npmCN := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer npmCN.Close()

	cnHint := false
	route := DetectNetworkRoute(context.Background(), MirrorOptions{
		Mode:        MirrorModeAuto,
		DirectNPM:   npmDirect.URL,
		ChinaNPM:    npmCN.URL,
		ChinaHint:   &cnHint,
		HTTPClient:  &http.Client{Timeout: 2 * time.Second},
		BypassCache: true,
	})
	if route.Region != MirrorModeDirect {
		t.Fatalf("expected direct route, got %+v", route)
	}
	if route.NPMRegistry != npmDirect.URL {
		t.Fatalf("expected direct npm %s, got %s", npmDirect.URL, route.NPMRegistry)
	}
}

func TestDetectNetworkRouteRespectsExplicitDirectAndCustomRegistry(t *testing.T) {
	direct := DetectNetworkRoute(context.Background(), MirrorOptions{
		Mode:        MirrorModeDirect,
		DirectNPM:   "https://registry.npmjs.org",
		BypassCache: true,
	})
	if direct.Region != MirrorModeDirect || direct.NPMRegistry != "https://registry.npmjs.org" {
		t.Fatalf("unexpected explicit direct route: %+v", direct)
	}

	custom := DetectNetworkRoute(context.Background(), MirrorOptions{
		Mode:        MirrorModeCN,
		NPMRegistry: "https://mirror.example.com/npm/",
		BypassCache: true,
	})
	if custom.NPMRegistry != "https://mirror.example.com/npm" || custom.Region != MirrorModeCN {
		t.Fatalf("unexpected custom registry route: %+v", custom)
	}
}
