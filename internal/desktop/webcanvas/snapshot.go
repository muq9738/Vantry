package webcanvas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

const (
	snapshotWinW = 760
	snapshotWinH = 420
	snapshotLRU  = 64
)

// SnapshotRequest asks the shared hidden snapshot worker to rasterize one
// Mermaid/SVG/HTML/Math block into a tight Retina PNG for native timeline display.
type SnapshotRequest struct {
	Key    string
	Kind   string
	Source string
	Dark   bool
}

// SnapshotResult caches the outcome of a rasterized snapshot.
type SnapshotResult struct {
	Key    string
	PNG    []byte
	Width  int
	Height int
	Err    error
}

// Snapshotter owns a single hidden offscreen WebView window and a serial
// queue so the native chat timeline stays 100% WebView-free at scroll time.
type Snapshotter struct {
	mu       sync.Mutex
	live     bool
	closed   bool
	onReady  func(key string)
	cache    map[string]SnapshotResult
	order    []string
	inFlight map[string]bool
	queue    chan SnapshotRequest
	quit     chan struct{}

	win     *mygo.Window
	id      string
	token   string
	loaded  chan struct{}
	snapEvt chan BridgeEvent
}

// NewSnapshotter constructs an offscreen rasterizer. Call EnableLive once
// mygo.App is ready so headless unit tests never instantiate hidden windows.
func NewSnapshotter(onReady func(key string)) *Snapshotter {
	return &Snapshotter{
		id:       "snap-" + randomHex(6),
		token:    randomHex(16),
		onReady:  onReady,
		cache:    make(map[string]SnapshotResult, snapshotLRU),
		inFlight: make(map[string]bool),
		queue:    make(chan SnapshotRequest, 48),
		quit:     make(chan struct{}),
		loaded:   make(chan struct{}),
		snapEvt:  make(chan BridgeEvent, 8),
	}
}

// EnableLive starts the serial background rasterizer worker when WKWebView is
// supported and mygo.App is active.
func (s *Snapshotter) EnableLive() {
	if s == nil || !mygo.WebViewAvailable() {
		return
	}
	s.mu.Lock()
	if s.closed || s.live {
		s.mu.Unlock()
		return
	}
	s.live = true
	s.mu.Unlock()
	go s.run()
}

// Get returns a cached snapshot PNG if already rendered.
func (s *Snapshotter) Get(key string) (SnapshotResult, bool) {
	if s == nil {
		return SnapshotResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.cache[key]
	return res, ok
}

// Request enqueues key for offscreen rasterization when not already cached or
// in flight. In headless test environments (!live), it is a safe no-op.
func (s *Snapshotter) Request(req SnapshotRequest) {
	if s == nil || req.Key == "" || strings.TrimSpace(req.Source) == "" {
		return
	}
	s.mu.Lock()
	if s.closed || !s.live {
		s.mu.Unlock()
		return
	}
	if _, cached := s.cache[req.Key]; cached || s.inFlight[req.Key] {
		s.mu.Unlock()
		return
	}
	s.inFlight[req.Key] = true
	s.mu.Unlock()

	select {
	case s.queue <- req:
	default:
		s.mu.Lock()
		delete(s.inFlight, req.Key)
		s.mu.Unlock()
	}
}

func (s *Snapshotter) run() {
	for {
		select {
		case <-s.quit:
			return
		case req := <-s.queue:
			res := s.renderOne(req)
			s.mu.Lock()
			delete(s.inFlight, req.Key)
			if len(res.PNG) > 0 || res.Err != nil {
				if _, exists := s.cache[req.Key]; !exists && len(s.order) >= snapshotLRU {
					delete(s.cache, s.order[0])
					s.order = s.order[1:]
				}
				if _, exists := s.cache[req.Key]; !exists {
					s.order = append(s.order, req.Key)
				}
				s.cache[req.Key] = res
			}
			cb := s.onReady
			closed := s.closed
			s.mu.Unlock()
			if !closed && cb != nil {
				cb(req.Key)
			}
		}
	}
}

func (s *Snapshotter) ensureWindow() (*mygo.Window, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	if s.win != nil && !s.win.IsDestroyed() {
		w := s.win
		s.mu.Unlock()
		return w, nil
	}
	s.mu.Unlock()

	if err := RegisterProtocol(); err != nil {
		return nil, err
	}

	var loadOnce sync.Once
	registerBridge(s.id, s.token, func(evt BridgeEvent) {
		if evt.Type == "loaded" {
			loadOnce.Do(func() { close(s.loaded) })
			return
		}
		select {
		case s.snapEvt <- evt:
		default:
		}
	})

	var rules []*mygo.ContentRules
	if r := CompiledOfflineRules(); r != nil {
		rules = []*mygo.ContentRules{r}
	}
	snapURL := fmt.Sprintf("%s://app/snapshot.html?id=%s&token=%s",
		Scheme, url.QueryEscape(s.id), url.QueryEscape(s.token))

	win := mygo.NewWindow(mygo.WindowOptions{
		Hidden:         true,
		Frameless:      true,
		UseContentSize: true,
		Width:          snapshotWinW,
		Height:         snapshotWinH,
		URL:            snapURL,
		Page: mygo.PageOptions{
			Untrusted:            true,
			BackgroundThrottling: mygo.ThrottlingOff,
			ContentRules:         rules,
		},
	})
	if win == nil {
		unregisterBridge(s.id)
		return nil, ErrUnavailable
	}
	if page := win.Page(); page != nil {
		page.OnWillNavigate(func(e *mygo.NavigateEvent) {
			if !strings.HasPrefix(e.URL, Scheme+"://") {
				e.PreventDefault()
			}
		})
		page.SetWindowOpenHandler(func(_ mygo.WindowOpenRequest) *mygo.WindowOptions { return nil })
		page.SetPermissionHandler(func(_ mygo.PermissionRequest) bool { return false })
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		unregisterBridge(s.id)
		win.Close()
		return nil, ErrUnavailable
	}
	s.win = win
	s.mu.Unlock()

	select {
	case <-s.loaded:
	case <-s.quit:
		return nil, ErrUnavailable
	case <-time.After(4 * time.Second):
		// proceed cleanly; EvalContext below checks readiness
	}
	return win, nil
}

func (s *Snapshotter) renderOne(req SnapshotRequest) SnapshotResult {
	res := SnapshotResult{Key: req.Key}
	win, err := s.ensureWindow()
	if err != nil {
		res.Err = err
		return res
	}
	page := win.Page()
	if page == nil {
		res.Err = ErrUnavailable
		return res
	}
	// Drain stale bridge notifications before running this job.
	for len(s.snapEvt) > 0 {
		<-s.snapEvt
	}

	prepared := preparePayload(Payload{
		Kind:   req.Kind,
		Source: req.Source,
		Dark:   req.Dark,
	})
	rawJSON, err := json.Marshal(prepared)
	if err != nil {
		res.Err = err
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := page.EvalContext(ctx, "window.__vantrySnapshot("+string(rawJSON)+")"); err != nil {
		res.Err = err
		return res
	}

	var readyEvt BridgeEvent
	select {
	case readyEvt = <-s.snapEvt:
		if readyEvt.Type == "error" {
			res.Err = fmt.Errorf("snapshot render error: %s", readyEvt.Message)
			return res
		}
	case <-s.quit:
		res.Err = ErrUnavailable
		return res
	case <-time.After(5 * time.Second):
		res.Err = fmt.Errorf("snapshot timed out")
		return res
	}

	pngBytes, err := win.CapturePage()
	if err != nil {
		res.Err = err
		return res
	}
	cropped, w, h := cropSnapshotPNG(pngBytes, readyEvt.Width, readyEvt.Height)
	res.PNG, res.Width, res.Height = cropped, w, h
	return res
}

func cropSnapshotPNG(raw []byte, dipW, dipH float64) ([]byte, int, int) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw, snapshotWinW, snapshotWinH
	}
	b := img.Bounds()
	scale := float64(b.Dx()) / float64(snapshotWinW)
	if scale < 1 {
		scale = 1
	}
	cw := int(math.Ceil(dipW * scale))
	ch := int(math.Ceil(dipH * scale))
	if cw <= 0 || ch <= 0 || cw >= b.Dx() && ch >= b.Dy() {
		return raw, b.Dx(), b.Dy()
	}
	cw = min(cw, b.Dx())
	ch = min(ch, b.Dy())
	sub, ok := img.(interface {
		SubImage(r image.Rectangle) image.Image
	})
	if !ok {
		return raw, b.Dx(), b.Dy()
	}
	var out bytes.Buffer
	if err := png.Encode(&out, sub.SubImage(image.Rect(b.Min.X, b.Min.Y, b.Min.X+cw, b.Min.Y+ch))); err != nil {
		return raw, b.Dx(), b.Dy()
	}
	return out.Bytes(), cw, ch
}

// Close shuts down the background rasterizer worker and closes its hidden window.
func (s *Snapshotter) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	win := s.win
	s.win = nil
	id := s.id
	close(s.quit)
	s.mu.Unlock()

	unregisterBridge(id)
	if win != nil && !win.IsDestroyed() {
		win.Close()
	}
}
