// Package web embeds the built console and serves it as a single-page app.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var FS embed.FS

// DistFS provides access to the sub-filesystem rooted at dist.
var DistFS fs.FS

func init() {
	if sub, err := fs.Sub(FS, "dist"); err == nil {
		DistFS = sub
	} else {
		DistFS = FS
	}
}

// Handler serves the embedded console SPA. Unknown paths fall back to
// dist/index.html so client-side routing works on refresh and deep links.
func Handler() http.Handler {
	sub, err := fs.Sub(FS, "dist")
	if err != nil {
		panic("web: embedded dist missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean("/" + r.URL.Path)
		if p == "/" {
			p = "/index.html"
		}
		if info, err := fs.Stat(sub, strings.TrimPrefix(p, "/")); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback: serve index.html for unknown, non-asset paths.
		if !strings.Contains(path.Ext(p), ".") || p == "/index.html" {
			index, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.Error(w, "console not built", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(index)
			}
			return
		}
		http.NotFound(w, r)
	})
}
