package local

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestGzipNegotiationAndPassthrough(t *testing.T) {
	for _, tc := range []struct {
		accept string
		gzip   bool
	}{
		{"", false}, {"br, gzip", true}, {"gzip;q=0, *;q=1", false},
		{"*;q=0.5", true}, {"gzip;q=bad", false}, {"gzip;q=NaN", false},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "13")
				_, _ = io.WriteString(w, `{"text":"中"}`)
			}))
			req := httptest.NewRequest("GET", "/v1/snapshot", nil)
			req.Header.Set("Accept-Encoding", tc.accept)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if (w.Header().Get("Content-Encoding") == "gzip") != tc.gzip {
				t.Fatal(w.Header())
			}
			var reader io.Reader = w.Body
			if tc.gzip {
				if w.Header().Get("Content-Length") != "" {
					t.Fatal("stale length")
				}
				gz, err := gzip.NewReader(reader)
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				reader = gz
			}
			data, err := io.ReadAll(reader)
			if err != nil || string(data) != `{"text":"中"}` {
				t.Fatal(string(data), err)
			}
		})
	}
}

func TestCompressedEventStreamLosslessAndBounded(t *testing.T) {
	const count = 600
	events := make(chan api.Event, count)
	var original bytes.Buffer
	for i := range count {
		event := api.Event{Sequence: int64(i + 1), ID: fmt.Sprint(i), Type: "run.output.delta", ThreadID: "thread", RunID: "run", Timestamp: time.Unix(0, 0).UTC(), Data: json.RawMessage(`{"text":"中文 token delta repeated payload"}`)}
		events <- event
		_ = json.NewEncoder(&original).Encode(streamEnvelope{Event: &event})
	}
	close(events)
	s := &Server{service: &fakeService{subscribeFunc: func(context.Context, int64) (*api.Subscription, error) { return &api.Subscription{Events: events}, nil }}}
	h := compressResponses(http.HandlerFunc(s.handleEvents))
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/events?after=0", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(w, req)
	gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	decoded, err := io.ReadAll(gz)
	if err != nil || !bytes.Equal(decoded, original.Bytes()) {
		t.Fatalf("event stream changed: %v", err)
	}
	if w.Body.Len() >= original.Len()/3 {
		t.Fatalf("poor compression: %d/%d", w.Body.Len(), original.Len())
	}
	t.Logf("600 events: %d -> %d bytes (%.1f%%)", original.Len(), w.Body.Len(), 100*float64(w.Body.Len())/float64(original.Len()))
}

func TestCompressedSparseEventArrivesBeforeStreamCloses(t *testing.T) {
	events := make(chan api.Event, 1)
	closed := make(chan struct{})
	s := &Server{service: &fakeService{subscribeFunc: func(context.Context, int64) (*api.Subscription, error) {
		return &api.Subscription{Events: events, Close: func() { close(closed) }}, nil
	}}}
	server := httptest.NewServer(compressResponses(http.HandlerFunc(s.handleEvents)))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/v1/events?after=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events <- api.Event{Sequence: 1, Data: json.RawMessage(`{"text":"一"}`)}
	line, err := bufio.NewReader(resp.Body).ReadBytes('\n')
	if err != nil || !bytes.Contains(line, []byte(`"sequence":1`)) {
		t.Fatal(string(line), err)
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("subscription leaked after cancellation")
	}
}

func TestNetworkALPNMultiplexingAndHTTP1Fallback(t *testing.T) {
	for _, h2 := range []bool{true, false} {
		t.Run(fmt.Sprint("h2=", h2), func(t *testing.T) {
			protocols := make(chan int, 8)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				protocols <- r.ProtoMajor
				if r.Header.Get("Authorization") != "Bearer test" {
					t.Error("missing authorization")
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasPrefix(r.URL.Path, "/v1/events") {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, `{"version":1,"state":"ready"}`)
			}))
			server.EnableHTTP2 = h2
			server.StartTLS()
			defer server.Close()
			config := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			config.NextProtos = []string{"h2", "http/1.1"}
			var dials atomic.Int32
			client := DialNetwork(func(ctx context.Context, _, _ string) (net.Conn, error) {
				dials.Add(1)
				return (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", server.Listener.Addr().String())
			}, "test")
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sub, err := client.Subscribe(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			for range 3 {
				if _, err := client.Health(ctx); err != nil {
					t.Fatal(err)
				}
			}
			for range 4 {
				protocol := <-protocols
				if (protocol == 2) != h2 {
					t.Fatalf("unexpected HTTP/%d", protocol)
				}
			}
			want := int32(2)
			if h2 {
				want = 1
			}
			if dials.Load() != want {
				t.Fatalf("dials=%d want %d with an open event stream", dials.Load(), want)
			}
		})
	}
}

func TestUnixClientDoesNotRequestCompression(t *testing.T) {
	client := Dial("unused")
	defer client.Close()
	if !client.transport.DisableCompression {
		t.Fatal("Unix compression enabled")
	}
}
