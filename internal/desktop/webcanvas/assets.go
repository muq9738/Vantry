package webcanvas

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io"
	"io/fs"
	"mime"
	"path"
	"strings"
	"sync"
)

//go:embed all:assets
var embeddedAssets embed.FS

type decodedAsset struct {
	data        []byte
	contentType string
}

var (
	assetCacheMu sync.RWMutex
	assetCache   = map[string]decodedAsset{}
)

// loadAsset serves an embedded shell or vendor asset, transparently gunzipping
// *.gz files once in memory and returning uncompressed bytes + exact MIME type
// (never Content-Encoding: gzip, so WKURLSchemeHandler stays deterministic).
func loadAsset(reqPath string) ([]byte, string, bool) {
	clean := path.Clean("/" + reqPath)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		clean = "shell.html"
	}

	assetCacheMu.RLock()
	if hit, ok := assetCache[clean]; ok {
		assetCacheMu.RUnlock()
		return hit.data, hit.contentType, true
	}
	assetCacheMu.RUnlock()

	fsPath := "assets/" + clean
	data, err := fs.ReadFile(embeddedAssets, fsPath)
	gzipped := false
	if err != nil {
		gzData, gzErr := fs.ReadFile(embeddedAssets, fsPath+".gz")
		if gzErr != nil {
			return nil, "", false
		}
		data = gzData
		gzipped = true
	}
	if gzipped {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, "", false
		}
		decoded, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return nil, "", false
		}
		data = decoded
	}

	ctype := assetContentType(clean)
	assetCacheMu.Lock()
	assetCache[clean] = decodedAsset{data: data, contentType: ctype}
	assetCacheMu.Unlock()
	return data, ctype, true
}

func assetContentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// EmbeddedCompressedSize reports the total byte size of embedded .gz vendor
// assets (Mermaid, KaTeX, fonts) so tests can enforce lightweight footprint.
func EmbeddedCompressedSize() (int64, error) {
	var total int64
	err := fs.WalkDir(embeddedAssets, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".gz") {
			return err
		}
		info, iErr := d.Info()
		if iErr != nil {
			return iErr
		}
		total += info.Size()
		return nil
	})
	return total, err
}
