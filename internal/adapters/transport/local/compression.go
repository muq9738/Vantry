package local

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// Compression lives inside the authenticated TLS tunnel. A single gzip writer
// retains its dictionary across event flushes; Unix clients do not request it.
func compressResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/events", "/v1/snapshot", "/v1/workbench":
		default:
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || r.Header.Get("Range") != "" || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &gzipResponse{ResponseWriter: w}
		defer func() {
			if cw.writer != nil {
				_ = cw.writer.Close()
			}
		}()
		next.ServeHTTP(cw, r)
	})
}

func acceptsGzip(value string) bool {
	wildcard := false
	for _, entry := range strings.Split(value, ",") {
		parts := strings.Split(entry, ";")
		coding := strings.ToLower(strings.TrimSpace(parts[0]))
		if coding != "gzip" && coding != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			key, val, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				q, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
				if err != nil || !(q >= 0 && q <= 1) {
					q = 0
				}
				quality = q
			}
		}
		if coding == "gzip" {
			return quality > 0
		}
		wildcard = quality > 0
	}
	return wildcard
}

type gzipResponse struct {
	http.ResponseWriter
	writer  *gzip.Writer
	started bool
}

func (w *gzipResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *gzipResponse) WriteHeader(status int) {
	if w.started {
		return
	}
	if status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.started = true
	contentType := w.Header().Get("Content-Type")
	if status != http.StatusNoContent && status != http.StatusNotModified && w.Header().Get("Content-Encoding") == "" &&
		(strings.HasPrefix(contentType, "application/json") || strings.HasPrefix(contentType, "application/x-ndjson")) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		w.writer, _ = gzip.NewWriterLevel(w.ResponseWriter, gzip.BestSpeed)
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *gzipResponse) Write(p []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if w.writer != nil {
		return w.writer.Write(p)
	}
	return w.ResponseWriter.Write(p)
}
func (w *gzipResponse) FlushError() error {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if w.writer != nil {
		if err := w.writer.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *gzipResponse) Flush() { _ = w.FlushError() }
