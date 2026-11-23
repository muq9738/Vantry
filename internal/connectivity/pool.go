package connectivity

import (
	"context"
	"net"
	"net/http"
	"time"
)

type tunnelTarget struct{ route, channel string }

// Each configured destination owns its connection pool so sequential T3 HTTP
// requests reuse tunnels without crossing device boundaries.
func (r *Relay) tunnelTransport(route, channel string) *http.Transport {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := tunnelTarget{route, channel}
	if pool := r.transports[key]; pool != nil {
		return pool
	}
	pool := &http.Transport{
		DisableCompression:  true,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return r.open(ctx, route, channel)
		},
	}
	r.transports[key] = pool
	return pool
}
