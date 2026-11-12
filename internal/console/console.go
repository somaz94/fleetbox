// Package console serves the embedded React build: its files as they are, and
// index.html for every other path, so a reload on a client-side route works.
package console

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// csp allows only the build's own files: Vite emits no inline script or style,
// and log lines, which are untrusted, are rendered as text.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Handler serves fsys, a Vite build with index.html at its root.
func Handler(fsys fs.FS) http.Handler {
	index, indexErr := fs.ReadFile(fsys, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if indexErr != nil {
			h.Set("Cache-Control", "no-store")
			http.Error(w, "the console is not built into this binary: run make web, then rebuild", http.StatusServiceUnavailable)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" && name != "index.html" && !strings.HasPrefix(name, ".") && !strings.Contains(name, "/.") {
			if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
				// Vite puts a content hash in every name under assets/.
				if strings.HasPrefix(name, "assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					h.Set("Cache-Control", "no-cache")
				}
				http.ServeFileFS(w, r, fsys, name)
				return
			}
			// A missing asset is a 404, not the page: HTML served as a script
			// would hide a broken build behind a blank screen.
			if strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)
				return
			}
		}
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	})
}
