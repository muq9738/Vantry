package webcanvas

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedAssetSizeBudgetAndServing(t *testing.T) {
	totalGZ, err := EmbeddedCompressedSize()
	if err != nil {
		t.Fatalf("EmbeddedCompressedSize: %v", err)
	}
	const maxBudget = int64(1536 << 10) // 1.5 MiB hard ceiling
	if totalGZ <= 0 || totalGZ > maxBudget {
		t.Fatalf("embedded .gz asset size = %d bytes, want (0, %d]", totalGZ, maxBudget)
	}

	routes := []struct {
		path string
		want string
		mime string
	}{
		{"/shell.html", "shell.js", "text/html"},
		{"/snapshot.html", "__vantrySnapshot", "text/html"},
		{"/assets/mermaid.min.js", "mermaid", "application/javascript"},
		{"/assets/katex.min.js", "katex", "application/javascript"},
		{"/assets/katex.min.css", "KaTeX", "text/css"},
		{"/assets/fonts/KaTeX_Main-Regular.woff2", "", "font/woff2"},
	}

	for _, rt := range routes {
		req := httptest.NewRequest(http.MethodGet, "vantry-canvas://app"+rt.path, nil)
		rec := httptest.NewRecorder()
		handleProtocol(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", rt.path, rec.Code)
		}
		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Fatalf("%s unexpectedly set Content-Encoding=%q", rt.path, enc)
		}
		if ctype := rec.Header().Get("Content-Type"); !strings.Contains(ctype, rt.mime) {
			t.Fatalf("%s Content-Type = %q, want substring %q", rt.path, ctype, rt.mime)
		}
		if len(rec.Body.Bytes()) == 0 {
			t.Fatalf("%s returned empty body", rt.path)
		}
		if rt.want != "" && !strings.Contains(rec.Body.String(), rt.want) {
			t.Fatalf("%s body missing expected marker %q", rt.path, rt.want)
		}
	}

	missing := httptest.NewRecorder()
	handleProtocol(missing, httptest.NewRequest(http.MethodGet, "vantry-canvas://app/assets/nonexistent.js", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", missing.Code)
	}
}

func TestSanitizeSVGAndWrapSandboxedHTML(t *testing.T) {
	dirtySVG := `<svg viewBox="0 0 100 100" onload="alert(1)"><script>fetch('https://evil.test')</script><foreignObject><div>bad</div></foreignObject><a xlink:href="javascript:alert(2)"><rect x="1" y="1" width="10" height="10" onclick="steal()" style="fill:url(https://evil.test/x.svg)"/><path d="M0 0L10 10"/></a></svg>`
	clean := SanitizeSVG(dirtySVG)
	for _, banned := range []string{"<script", "<foreignObject", "onload=", "onclick=", "javascript:", "https://evil.test"} {
		if strings.Contains(strings.ToLower(clean), strings.ToLower(banned)) {
			t.Fatalf("SanitizeSVG failed to strip %q in %s", banned, clean)
		}
	}
	for _, kept := range []string{"<svg", "<rect", "<path d=\"M0 0L10 10\""} {
		if !strings.Contains(clean, kept) {
			t.Fatalf("SanitizeSVG lost valid SVG element %q in %s", kept, clean)
		}
	}

	frag := WrapSandboxedHTML(`<div id="app">Hello</div>`, true)
	if !strings.Contains(frag, HTMLArtifactCSP) || !strings.Contains(frag, `data-theme="dark"`) || !strings.Contains(frag, `vantry-select`) {
		t.Fatalf("WrapSandboxedHTML fragment malformed:\n%s", frag)
	}
	full := WrapSandboxedHTML(`<!doctype html><html><head><title>Demo</title></head><body><main>Hi</main></body></html>`, false)
	if !strings.Contains(full, HTMLArtifactCSP) || !strings.Contains(full, `<title>Demo</title>`) || !strings.Contains(full, `<main>Hi</main>`) {
		t.Fatalf("WrapSandboxedHTML full doc malformed:\n%s", full)
	}
}

func TestBridgeTokenAuthAndHeadlessSafety(t *testing.T) {
	ch := make(chan BridgeEvent, 1)
	registerBridge("c1", "secret-tok", func(evt BridgeEvent) { ch <- evt })
	defer unregisterBridge("c1")

	body, _ := json.Marshal(BridgeEvent{Type: "select", Selector: "g.node", Text: "Gateway"})

	badRec := httptest.NewRecorder()
	handleProtocol(badRec, httptest.NewRequest(http.MethodPost, "vantry-canvas://app/bridge?id=c1&token=wrong", bytes.NewReader(body)))
	if badRec.Code != http.StatusForbidden {
		t.Fatalf("wrong token status = %d, want 403", badRec.Code)
	}

	okRec := httptest.NewRecorder()
	handleProtocol(okRec, httptest.NewRequest(http.MethodPost, "vantry-canvas://app/bridge?id=c1&token=secret-tok", bytes.NewReader(body)))
	if okRec.Code != http.StatusNoContent {
		t.Fatalf("valid token status = %d, want 204", okRec.Code)
	}

	select {
	case evt := <-ch:
		if evt.Type != "select" || evt.Selector != "g.node" || evt.Text != "Gateway" {
			t.Fatalf("unexpected bridge event: %+v", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bridge callback")
	}

	canvas := NewCanvas(nil)
	if _, err := canvas.Ensure(nil, "#ffffff"); err != ErrUnavailable {
		t.Fatalf("Ensure(nil) err = %v, want ErrUnavailable", err)
	}
	if err := canvas.Render(Payload{Kind: "mermaid", Source: "graph LR\nA-->B"}); err != nil {
		t.Fatalf("Render queued before Ensure failed: %v", err)
	}
	canvas.Zoom("fit")
	canvas.SetTheme(true)
	canvas.Destroy()

	snap := NewSnapshotter(nil)
	snap.Request(SnapshotRequest{Key: "k1", Kind: "mermaid", Source: "graph LR\nA-->B"})
	if _, ok := snap.Get("k1"); ok {
		t.Fatal("headless Snapshotter should not pretend to have rendered when live mode is off")
	}
	snap.Close()
}
