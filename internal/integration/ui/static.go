package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/http"
	"path"
	"sync"
)

// Static assets (static/*) are embedded in the binary and served under
// /static/. Templates reference them through the asset func, which
// appends a content hash — so a browser may cache them forever, and a new
// build is picked up the moment the page references the new hash.

var (
	assetHashOnce sync.Once
	assetHashes   map[string]string
)

func loadAssetHashes() {
	assetHashes = map[string]string{}
	entries, _ := staticFS.ReadDir("static")
	for _, e := range entries {
		b, err := staticFS.ReadFile("static/" + e.Name())
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		assetHashes[e.Name()] = hex.EncodeToString(sum[:])[:10]
	}
}

// staticTypes names the types a slim container's mime table may not know
// (alpine ships no /etc/mime.types): a font served as text is refused.
var staticTypes = map[string]string{
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".woff2":       "font/woff2",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain; charset=utf-8",
}

// assetURL is the template func: {{asset "app.css"}}.
func assetURL(name string) string {
	assetHashOnce.Do(loadAssetHashes)
	return "/static/" + name + "?v=" + assetHashes[name]
}

func handleStatic(w http.ResponseWriter, r *http.Request) {
	assetHashOnce.Do(loadAssetHashes)
	name := path.Base(r.PathValue("file"))
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := staticTypes[path.Ext(name)]; ct != "" {
		w.Header().Set("Content-Type", ct)
	} else if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if v := r.URL.Query().Get("v"); v != "" && v == assetHashes[name] {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(b)
}
