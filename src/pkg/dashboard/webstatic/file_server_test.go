package webstatic

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/fstest"
)

func serveFile(h http.Handler, method, url, validator string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, nil)
	if validator != "" {
		r.Header.Set("If-None-Match", validator)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestFileServerRevalidation(t *testing.T) {
	oldContent := []byte("old content")
	newContent := []byte("new content")
	for _, name := range []string{"design-system.html", "tokens.css", "app.js", "static/logo.svg", "docs/index.html"} {
		t.Run(name, func(t *testing.T) {
			handler := FileServer(fstest.MapFS{name: &fstest.MapFile{Data: oldContent}})
			url := "/" + name
			if name == "docs/index.html" {
				url = "/docs/"
			}
			first := serveFile(handler, http.MethodGet, url, "")
			etag := first.Header().Get("ETag")
			// The validator must be the shared ETagFor format, identical to
			// the one the hub and spoke HTML handlers emit.
			if want := ETagFor(oldContent); etag != want {
				t.Fatalf("ETag = %q, want shared ETagFor value %q", etag, want)
			}
			if first.Code != http.StatusOK || first.Body.String() != string(oldContent) || first.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("initial response: status=%d headers=%v body=%q", first.Code, first.Header(), first.Body.String())
			}
			for _, validator := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
				cached := serveFile(handler, http.MethodGet, url, validator)
				if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 || cached.Header().Get("ETag") != etag || cached.Header().Get("Cache-Control") != "no-cache" {
					t.Fatalf("revalidation %q: status=%d headers=%v body=%q", validator, cached.Code, cached.Header(), cached.Body.String())
				}
			}
			stale := serveFile(handler, http.MethodGet, url, `"stale"`)
			if stale.Code != http.StatusOK || stale.Body.String() != string(oldContent) {
				t.Fatalf("non-matching validator: status=%d body=%q", stale.Code, stale.Body.String())
			}
			head := serveFile(handler, http.MethodHead, url, "")
			if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("ETag") != etag {
				t.Fatalf("HEAD: status=%d headers=%v body=%q", head.Code, head.Header(), head.Body.String())
			}
			// A new binary constructs a new handler over its new embedded bytes.
			upgraded := FileServer(fstest.MapFS{name: &fstest.MapFile{Data: newContent}})
			fresh := serveFile(upgraded, http.MethodGet, url, etag)
			if fresh.Code != http.StatusOK || fresh.Body.String() != string(newContent) || fresh.Header().Get("ETag") != ETagFor(newContent) {
				t.Fatalf("upgrade: status=%d headers=%v body=%q", fresh.Code, fresh.Header(), fresh.Body.String())
			}
		})
	}
}

func TestFileServerMissingFile(t *testing.T) {
	w := serveFile(FileServer(fstest.MapFS{}), http.MethodGet, "/missing.css", "*")
	if w.Code != http.StatusNotFound || w.Header().Get("ETag") != "" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("missing file: status=%d headers=%v", w.Code, w.Header())
	}
	// A directory with no index.html falls through to net/http's listing:
	// there is no single file to validate, so no ETag and never a 304.
	listing := serveFile(FileServer(fstest.MapFS{"assets/a.css": &fstest.MapFile{Data: []byte("a")}}), http.MethodGet, "/assets/", "*")
	if listing.Code != http.StatusOK || listing.Header().Get("ETag") != "" || listing.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("directory listing: status=%d headers=%v", listing.Code, listing.Header())
	}
}

// A matching validator must never turn one of net/http's canonicalizing
// redirects into a 304, or the browser would keep a stale URL.
func TestFileServerRedirectsAreNotRevalidated(t *testing.T) {
	files := fstest.MapFS{
		"docs/index.html": &fstest.MapFile{Data: []byte("docs")},
		"app.js":          &fstest.MapFile{Data: []byte("js")},
	}
	handler := FileServer(files)
	for _, url := range []string{"/docs", "/docs/index.html", "/app.js/"} {
		t.Run(url, func(t *testing.T) {
			w := serveFile(handler, http.MethodGet, url, "*")
			if w.Code != http.StatusMovedPermanently || w.Header().Get("ETag") != "" {
				t.Fatalf("status=%d headers=%v, want 301 without ETag", w.Code, w.Header())
			}
		})
	}
}

func TestServedFileName(t *testing.T) {
	files := fstest.MapFS{
		"index.html":      &fstest.MapFile{Data: []byte("root")},
		"docs/index.html": &fstest.MapFile{Data: []byte("docs")},
		"app.js":          &fstest.MapFile{Data: []byte("js")},
	}
	cases := []struct {
		url    string
		want   string
		wantOK bool
	}{
		{"/", indexPage, true},
		{"/docs/", "docs/" + indexPage, true},
		{"/app.js", "app.js", true},
		{"app.js", "app.js", true},
		{"/docs", "", false},
		{"/docs/index.html", "", false},
		{"/app.js/", "", false},
		{"/missing.css", "", false},
	}
	for _, tc := range cases {
		got, ok := servedFileName(files, tc.url)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("servedFileName(%q) = (%q, %v), want (%q, %v)", tc.url, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestCachedETagMemoizes(t *testing.T) {
	files := fstest.MapFS{"app.js": &fstest.MapFile{Data: []byte("js")}}
	var etags sync.Map
	first, ok := cachedETag(&etags, files, "app.js")
	if !ok || first != ETagFor([]byte("js")) {
		t.Fatalf("cachedETag = (%q, %v), want (%q, true)", first, ok, ETagFor([]byte("js")))
	}
	// Changing the underlying bytes must not change the memoized validator:
	// the handler serves immutable embedded files for its whole life.
	files["app.js"] = &fstest.MapFile{Data: []byte("changed")}
	if again, ok := cachedETag(&etags, files, "app.js"); !ok || again != first {
		t.Fatalf("cachedETag after change = (%q, %v), want memoized %q", again, ok, first)
	}
	if _, ok := cachedETag(&etags, files, "missing.js"); ok {
		t.Fatal("cachedETag reported ok for a missing file")
	}
}
