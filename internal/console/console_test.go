package console

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var build = fstest.MapFS{
	"index.html":          {Data: []byte("<!doctype html><title>fleetbox</title>")},
	"assets/index-a1.js":  {Data: []byte("console.log(1)")},
	"favicon.svg":         {Data: []byte("<svg/>")},
	".gitkeep":            {},
	".vite/manifest.json": {Data: []byte("{}")},
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestHandler(t *testing.T) {
	h := Handler(build)
	for _, p := range []string{"/", "/index.html", "/services/prod/web/api", "/.gitkeep", "/.vite/manifest.json", "/../etc/passwd", "/assets"} {
		rec := get(t, h, "GET", p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>fleetbox</title>") ||
			rec.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s = %d %q %v", p, rec.Code, rec.Body, rec.Header())
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: security headers missing: %v", p, rec.Header())
		}
	}

	rec := get(t, h, "GET", "/assets/index-a1.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") ||
		!strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("asset = %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	if rec := get(t, h, "GET", "/favicon.svg"); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("root file = %d %v", rec.Code, rec.Header())
	}
	if rec := get(t, h, "GET", "/assets/index-old.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d", rec.Code)
	}
	if rec := get(t, h, "HEAD", "/services"); rec.Code != http.StatusOK || rec.Body.Len() != 0 ||
		rec.Header().Get("Content-Length") != fmt.Sprint(len(build["index.html"].Data)) {
		t.Fatalf("HEAD = %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	if rec := get(t, h, "POST", "/"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST = %d", rec.Code)
	}
}

func TestHandlerNotBuilt(t *testing.T) {
	rec := get(t, Handler(fstest.MapFS{".gitkeep": {}}), "GET", "/")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "make web") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("not built = %d %q", rec.Code, rec.Body)
	}
}
