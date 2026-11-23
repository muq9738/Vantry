package webcanvas

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

// ErrUnavailable is returned when embedded WKWebView/WebKit cannot be created
// (e.g. headless unit tests with nil *mygo.Window).
var ErrUnavailable = errors.New("webview unavailable")

func registerWithMyGo() error {
	return mygo.Protocol.HandleFunc(Scheme, handleProtocol)
}

// Payload describes a single diagram, SVG, sandboxed HTML artifact, math, or
// markdown view to display inside the offline WebView stage.
type Payload struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Title  string `json:"title,omitempty"`
	Dark   bool   `json:"dark"`
}

// Canvas owns one interactive offline WebView instance inside the desktop
// dock, enforcing CSP, offline ContentRules, blocked popups, and a random
// per-instance token on POST /bridge.
type Canvas struct {
	mu      sync.Mutex
	id      string
	token   string
	wv      *mygo.WebView
	ready   bool
	pending *Payload
	onEvent func(BridgeEvent)
}

// NewCanvas allocates a controller for an interactive offline WebView stage.
func NewCanvas(onEvent func(BridgeEvent)) *Canvas {
	return &Canvas{
		id:      randomHex(8),
		token:   randomHex(16),
		onEvent: onEvent,
	}
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Ensure creates (or returns) the backing *mygo.WebView inside win. Returns
// ErrUnavailable cleanly when win == nil or WebViewAvailable() is false.
func (c *Canvas) Ensure(win *mygo.Window, bg string) (*mygo.WebView, error) {
	return c.ensure(win, bg, false)
}

// EnsureInline creates (or returns) the backing *mygo.WebView configured for
// inline embedding inside scrollable chat containers (wheel scrolls forward).
func (c *Canvas) EnsureInline(win *mygo.Window, bg string) (*mygo.WebView, error) {
	return c.ensure(win, bg, true)
}

func (c *Canvas) ensure(win *mygo.Window, bg string, scrollToContainer bool) (*mygo.WebView, error) {
	if c == nil || win == nil || !mygo.WebViewAvailable() {
		return nil, ErrUnavailable
	}
	c.mu.Lock()
	if c.wv != nil && !c.wv.IsDestroyed() {
		wv := c.wv
		c.mu.Unlock()
		return wv, nil
	}
	c.ready = false
	id, token := c.id, c.token
	pending := c.pending
	onEvent := c.onEvent
	c.mu.Unlock()

	if err := RegisterProtocol(); err != nil {
		return nil, err
	}

	registerBridge(id, token, func(evt BridgeEvent) {
		if evt.Type == "loaded" {
			c.mu.Lock()
			c.ready = true
			p := c.pending
			wv := c.wv
			c.mu.Unlock()
			if wv != nil && !wv.IsDestroyed() && p != nil {
				c.evalRender(wv.Page(), *p)
			}
			return
		}
		if onEvent != nil {
			onEvent(evt)
		}
	})

	theme := "light"
	if pending != nil && pending.Dark {
		theme = "dark"
	}
	var rules []*mygo.ContentRules
	if r := CompiledOfflineRules(); r != nil {
		rules = []*mygo.ContentRules{r}
	}
	pageURL := fmt.Sprintf("%s://app/shell.html?id=%s&token=%s&theme=%s",
		Scheme, url.QueryEscape(id), url.QueryEscape(token), theme)

	wv, err := win.NewWebView(mygo.WebViewOptions{
		URL:               pageURL,
		BackgroundColor:   bg,
		ScrollToContainer: scrollToContainer,
		Page: mygo.PageOptions{
			Untrusted:            true,
			PinchZoom:            true,
			BackgroundThrottling: mygo.ThrottlingOff,
			ContentRules:         rules,
		},
	})
	if err != nil {
		unregisterBridge(id)
		return nil, err
	}

	page := wv.Page()
	if page != nil {
		page.OnWillNavigate(func(e *mygo.NavigateEvent) {
			if !strings.HasPrefix(e.URL, Scheme+"://") {
				e.PreventDefault()
			}
		})
		page.SetWindowOpenHandler(func(_ mygo.WindowOpenRequest) *mygo.WindowOptions {
			return nil
		})
		page.SetPermissionHandler(func(_ mygo.PermissionRequest) bool {
			return false
		})
		page.OnDOMReady(func() {
			c.mu.Lock()
			already := c.ready
			c.ready = true
			p := c.pending
			c.mu.Unlock()
			if !already && p != nil {
				c.evalRender(page, *p)
			}
		})
	}

	c.mu.Lock()
	if c.wv != nil && !c.wv.IsDestroyed() {
		c.mu.Unlock()
		wv.Destroy()
		c.mu.Lock()
		existing := c.wv
		c.mu.Unlock()
		return existing, nil
	}
	c.wv = wv
	c.mu.Unlock()
	return wv, nil
}

// Render prepares (sanitizes SVG / wraps sandboxed HTML in Go) and pushes
// payload into the WebView, queuing it until DOM ready if still loading.
func (c *Canvas) Render(p Payload) error {
	if c == nil {
		return ErrUnavailable
	}
	prepared := preparePayload(p)
	c.mu.Lock()
	c.pending = &prepared
	wv := c.wv
	ready := c.ready
	c.mu.Unlock()
	if wv == nil || wv.IsDestroyed() || !ready {
		return nil
	}
	return c.evalRender(wv.Page(), prepared)
}

func preparePayload(p Payload) Payload {
	out := p
	out.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
	switch out.Kind {
	case "svg":
		out.Source = SanitizeSVG(p.Source)
	case "html", "vantry-html", "artifact":
		out.Kind = "html"
		out.Source = WrapSandboxedHTML(p.Source, p.Dark)
	}
	return out
}

func (c *Canvas) evalRender(page *mygo.Page, p Payload) error {
	if page == nil {
		return ErrUnavailable
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = page.Eval("window.__vantryRender(" + string(raw) + ")")
	return err
}

// SetTheme updates shell color scheme dynamically without recreating the WebView.
func (c *Canvas) SetTheme(dark bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.pending != nil {
		cp := *c.pending
		cp.Dark = dark
		c.pending = &cp
	}
	wv, ready := c.wv, c.ready
	c.mu.Unlock()
	if wv == nil || wv.IsDestroyed() || !ready || wv.Page() == nil {
		return
	}
	arg := "false"
	if dark {
		arg = "true"
	}
	_, _ = wv.Page().Eval("window.__vantrySetTheme(" + arg + ")")
}

// Zoom runs "in", "out", "reset", or "fit" on the active stage.
func (c *Canvas) Zoom(mode string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	wv, ready := c.wv, c.ready
	c.mu.Unlock()
	if wv == nil || wv.IsDestroyed() || !ready || wv.Page() == nil {
		return
	}
	raw, _ := json.Marshal(mode)
	_, _ = wv.Page().Eval("window.__vantryZoom(" + string(raw) + ")")
}

// ExportSVG retrieves the rendered SVG markup from the active Mermaid/SVG stage.
func (c *Canvas) ExportSVG() (string, error) {
	if c == nil {
		return "", ErrUnavailable
	}
	c.mu.Lock()
	wv, ready := c.wv, c.ready
	c.mu.Unlock()
	if wv == nil || wv.IsDestroyed() || !ready || wv.Page() == nil {
		return "", ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	val, err := wv.Page().EvalContext(ctx, "window.__vantryExportSVG()")
	if err != nil {
		return "", err
	}
	svg, _ := val.(string)
	if strings.TrimSpace(svg) == "" {
		return "", errors.New("当前画布不包含 SVG 元素")
	}
	return svg, nil
}

// CapturePNG captures the rendered WebView viewport as PNG bytes.
func (c *Canvas) CapturePNG() ([]byte, error) {
	if c == nil {
		return nil, ErrUnavailable
	}
	c.mu.Lock()
	wv := c.wv
	c.mu.Unlock()
	if wv == nil || wv.IsDestroyed() {
		return nil, ErrUnavailable
	}
	return wv.CapturePage()
}

// Destroy releases the underlying WebView and bridge credentials.
func (c *Canvas) Destroy() {
	if c == nil {
		return
	}
	c.mu.Lock()
	wv := c.wv
	id := c.id
	c.wv = nil
	c.ready = false
	c.mu.Unlock()
	unregisterBridge(id)
	if wv != nil && !wv.IsDestroyed() {
		wv.Destroy()
	}
}
