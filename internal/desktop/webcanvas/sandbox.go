package webcanvas

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/egoist/mygo"
)

// OfflineRulesJSON blocks all network HTTP(S) and WebSocket egress inside L2
// WKWebView contexts using WebKit's native content blocker engine.
const OfflineRulesJSON = `[{"trigger":{"url-filter":"^https?://.*"},"action":{"type":"block"}},{"trigger":{"url-filter":"^wss?://.*"},"action":{"type":"block"}}]`

// HTMLArtifactCSP is injected into sandboxed HTML artifact documents rendered
// inside `<iframe sandbox="allow-scripts">` (no allow-same-origin).
const HTMLArtifactCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'none';"

var (
	offlineRulesOnce sync.Once
	offlineRules     *mygo.ContentRules
)

// CompiledOfflineRules compiles OfflineRulesJSON once for attachment to
// PageOptions.ContentRules. It returns nil safely when unsupported.
func CompiledOfflineRules() *mygo.ContentRules {
	offlineRulesOnce.Do(func() {
		rules, err := mygo.CompileContentRules("vantry-webcanvas-offline", OfflineRulesJSON)
		if err == nil {
			offlineRules = rules
		}
	})
	return offlineRules
}

var (
	reDangerousSVGBlock  = regexp.MustCompile(`(?is)<\s*(script|foreignobject|iframe|object|embed|meta)\b[^>]*>.*?<\s*/\s*(?:script|foreignobject|iframe|object|embed|meta)\s*>`)
	reDangerousSVGInline = regexp.MustCompile(`(?is)<\s*(script|foreignobject|iframe|object|embed|meta)\b[^>]*/?\s*>`)
	reEventHandlerAttr   = regexp.MustCompile(`(?is)\s+on[a-z0-9_:-]+\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)
	reDangerousScheme    = regexp.MustCompile(`(?is)\b(?:xlink:)?(?:href|src)\s*=\s*(?:"\s*(?:javascript:|vbscript:|data\s*:\s*text/html|https?://|//)[^"]*"|'\s*(?:javascript:|vbscript:|data\s*:\s*text/html|https?://|//)[^']*')`)
	reExternalCSSURL     = regexp.MustCompile(`(?is)url\(\s*['"]?\s*(?:https?:|//|javascript:)[^)]*\)`)
)

// SanitizeSVG removes active scripting constructs, foreignObject, inline event
// handlers, and external http(s)/javascript URLs before L2 DOM insertion.
func SanitizeSVG(raw string) string {
	clean := reDangerousSVGBlock.ReplaceAllString(raw, "")
	clean = reDangerousSVGInline.ReplaceAllString(clean, "")
	clean = reEventHandlerAttr.ReplaceAllString(clean, "")
	clean = reDangerousScheme.ReplaceAllString(clean, "")
	clean = reExternalCSSURL.ReplaceAllString(clean, "none")
	return strings.TrimSpace(clean)
}

const sandboxSelectionScript = `<script>
(function(){
  if (window.__vantryHooked) return;
  window.__vantryHooked = true;
  document.addEventListener('click', function(ev) {
    try {
      var el = ev.target;
      if (!el || !el.tagName) return;
      var sel = el.tagName.toLowerCase();
      if (el.id) sel += '#' + el.id;
      else if (el.className && typeof el.className === 'string' && el.className.trim()) {
        sel += '.' + el.className.trim().split(/\s+/).slice(0,2).join('.');
      }
      var txt = (el.innerText || el.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 140);
      window.parent.postMessage({type: 'vantry-select', selector: sel, text: txt}, '*');
    } catch (_) {}
  }, true);
  var lastH = 0;
  function reportHeight() {
    try {
      var root = document.documentElement;
      var body = document.body;
      var h = Math.max(root ? root.scrollHeight : 0, body ? body.scrollHeight : 0);
      if (h > 0 && h !== lastH) {
        lastH = h;
        window.parent.postMessage({type: 'vantry-height', height: Math.ceil(h)}, '*');
      }
    } catch (_) {}
  }
  if (typeof ResizeObserver === 'function' && document.documentElement) {
    try {
      new ResizeObserver(reportHeight).observe(document.documentElement);
    } catch (_) {}
  }
  if (document.readyState === 'complete') reportHeight();
  else {
    window.addEventListener('DOMContentLoaded', reportHeight);
    window.addEventListener('load', reportHeight);
  }
})();
</script>`

// WrapSandboxedHTML wraps an HTML artifact or fragment with an offline CSP meta
// header, Studio CSS theme variables, micro-interaction transitions, and safe
// postMessage click/resize bridges for Canvas element selection & sizing.
func WrapSandboxedHTML(raw string, dark bool) string {
	trimmed := strings.TrimSpace(raw)
	themeAttr := "light"
	tokens := `--v-bg:#ffffff;--v-surface:#f7f7f9;--v-surface-2:#efeff3;--v-ink:#18181b;--v-muted:#71717a;--v-line:rgba(24,24,27,0.10);--v-accent:#3B6CE6;--v-good:#16A34A;--v-warn:#D97706;--v-bad:#E5482E;`
	if dark {
		themeAttr = "dark"
		tokens = `--v-bg:#1b1b1f;--v-surface:#232329;--v-surface-2:#2b2b33;--v-ink:#f4f4f5;--v-muted:#a1a1aa;--v-line:rgba(255,255,255,0.10);--v-accent:#60A5FA;--v-good:#34D399;--v-warn:#FBBF24;--v-bad:#F26D52;`
	}
	metaAndStyle := fmt.Sprintf(
		`<meta http-equiv="Content-Security-Policy" content="%s"><style>:root{color-scheme:%s;%s--v-font:-apple-system,BlinkMacSystemFont,"SF Pro Text","PingFang SC","Segoe UI",sans-serif;--v-mono:ui-monospace,SFMono-Regular,Menlo,monospace;}*{box-sizing:border-box;}html,body{margin:0;padding:16px;background:var(--v-bg);color:var(--v-ink);font-family:var(--v-font);-webkit-font-smoothing:antialiased;}.tnum{font-variant-numeric:tabular-nums;}button,[role="button"],.v-interactive{transition:transform 160ms cubic-bezier(0.23,1,0.32,1),opacity 160ms cubic-bezier(0.23,1,0.32,1),background-color 160ms cubic-bezier(0.23,1,0.32,1),border-color 160ms cubic-bezier(0.23,1,0.32,1);}button:active,[role="button"]:active,.v-interactive:active{transform:scale(0.97);}::-webkit-scrollbar{width:6px;height:6px;}::-webkit-scrollbar-thumb{background:var(--v-line);border-radius:999px;}@media (prefers-reduced-motion:reduce){*,*::before,*::after{transition-duration:0.01ms!important;animation-duration:0.01ms!important;}}</style>`,
		HTMLArtifactCSP, themeAttr, tokens,
	)

	lower := strings.ToLower(trimmed)
	if !strings.Contains(lower, "<html") {
		return "<!doctype html><html data-theme=\"" + themeAttr + "\"><head><meta charset=\"utf-8\">" +
			metaAndStyle + "</head><body>" + trimmed + sandboxSelectionScript + "</body></html>"
	}

	out := trimmed
	if idx := strings.Index(lower, "<head>"); idx >= 0 {
		pos := idx + len("<head>")
		out = out[:pos] + metaAndStyle + out[pos:]
	} else if idx := strings.Index(lower, "<head "); idx >= 0 {
		if closeIdx := strings.IndexByte(lower[idx:], '>'); closeIdx >= 0 {
			pos := idx + closeIdx + 1
			out = out[:pos] + metaAndStyle + out[pos:]
		}
	} else {
		out = metaAndStyle + out
	}

	lowerOut := strings.ToLower(out)
	if idx := strings.LastIndex(lowerOut, "</body>"); idx >= 0 {
		out = out[:idx] + sandboxSelectionScript + out[idx:]
	} else {
		out += sandboxSelectionScript
	}
	return out
}
