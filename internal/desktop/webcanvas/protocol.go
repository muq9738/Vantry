package webcanvas

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Scheme is the custom URL scheme registered with MyGo before opening any
// window so untrusted WKWebView canvases stay 100% offline.
const Scheme = "vantry-canvas"

var (
	protocolOnce sync.Once
	protocolErr  error

	bridgeMu    sync.RWMutex
	bridgeSinks = map[string]bridgeEntry{}
)

type bridgeEntry struct {
	token string
	fn    func(BridgeEvent)
}

// BridgeEvent is emitted by shell.js / snapshot.html over POST /bridge.
type BridgeEvent struct {
	Type     string  `json:"type"`
	Selector string  `json:"selector,omitempty"`
	Text     string  `json:"text,omitempty"`
	Message  string  `json:"message,omitempty"`
	Width    float64 `json:"width,omitempty"`
	Height   float64 `json:"height,omitempty"`
}

// RegisterProtocol registers vantry-canvas:// with mygo.Protocol idempotently.
// Must be called before mygo.App creates any window.
func RegisterProtocol() error {
	protocolOnce.Do(func() {
		// Import mygo via protocolModule.
		protocolErr = registerWithMyGo()
	})
	return protocolErr
}

func registerBridge(id, token string, fn func(BridgeEvent)) {
	bridgeMu.Lock()
	bridgeSinks[id] = bridgeEntry{token: token, fn: fn}
	bridgeMu.Unlock()
}

func unregisterBridge(id string) {
	bridgeMu.Lock()
	delete(bridgeSinks, id)
	bridgeMu.Unlock()
}

func handleProtocol(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "bridge" || strings.HasPrefix(p, "bridge/") {
		handleBridgeRequest(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(p, "assets/")
	if p == "shell.html" || p == "snapshot.html" || strings.HasPrefix(p, "assets/") {
		data, ctype, ok := loadAsset(rel)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
		return
	}
	http.NotFound(w, r)
}

func handleBridgeRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	id := q.Get("id")
	tok := q.Get("token")

	bridgeMu.RLock()
	entry, ok := bridgeSinks[id]
	bridgeMu.RUnlock()
	if !ok || subtle.ConstantTimeCompare([]byte(entry.token), []byte(tok)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var evt BridgeEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if entry.fn != nil {
		go entry.fn(evt)
	}
	w.WriteHeader(http.StatusNoContent)
}
