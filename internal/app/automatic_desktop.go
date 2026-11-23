package app

import (
	"context"
	"sync"
)

// The local host is ready even when the optional relay is offline. A relay
// host is created only after a viewer cannot establish its direct session.
type automaticDesktopHost struct {
	direct   rtcHost
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	fallback rtcHost
	create   func(context.Context) (rtcHost, error)
}

func (h *automaticDesktopHost) relay(ctx context.Context) (rtcHost, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := h.ctx.Err(); err != nil {
		return nil, err
	}
	if h.fallback != nil {
		if h.fallback.Err() == nil {
			return h.fallback, nil
		}
		_ = h.fallback.Close()
		h.fallback = nil
	}
	life, cancel := context.WithCancel(h.ctx)
	stop := context.AfterFunc(ctx, cancel)
	host, err := h.create(life)
	stop()
	if err != nil || ctx.Err() != nil || h.ctx.Err() != nil {
		cancel()
		if host != nil {
			_ = host.Close()
		}
		if err == nil {
			err = context.Canceled
		}
		return nil, err
	}
	h.fallback = host
	return host, nil
}
func (h *automaticDesktopHost) currentRelay() rtcHost {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fallback
}
func (h *automaticDesktopHost) Ticket() string {
	if r := h.currentRelay(); r != nil {
		return r.Ticket()
	}
	return h.direct.Ticket()
}
func (h *automaticDesktopHost) SessionID() string { return h.direct.SessionID() }
func (h *automaticDesktopHost) Connected() bool {
	r := h.currentRelay()
	return h.direct.Connected() || (r != nil && r.Connected())
}
func (h *automaticDesktopHost) Err() error { return desktopHostError(h.direct) }
func (h *automaticDesktopHost) Close() error {
	h.cancel()
	h.mu.Lock()
	r := h.fallback
	h.fallback = nil
	h.mu.Unlock()
	if r != nil {
		_ = r.Close()
	}
	return h.direct.Close()
}
func (h *automaticDesktopHost) DirectAddrs() []string {
	if r, ok := h.direct.(interface{ DirectAddrs() []string }); ok {
		return r.DirectAddrs()
	}
	return nil
}
func (h *automaticDesktopHost) ListenAddr() string {
	if r, ok := h.direct.(interface{ ListenAddr() string }); ok {
		return r.ListenAddr()
	}
	return ""
}
