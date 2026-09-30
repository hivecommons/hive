package hub

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
)

func TestStaticAssetCacheRevalidation(t *testing.T) {
	s := NewHubServer(0, slog.Default(), "test", "v5")
	for _, url := range []string{"/static/og-card.png", "/static/tokens.css", "/static/learn.html"} {
		t.Run(url, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
			etag := w.Header().Get("ETag")
			data, err := fs.ReadFile(staticFS, strings.TrimPrefix(url, "/"))
			if err != nil {
				t.Fatalf("read embedded %s: %v", url, err)
			}
			// Same validator format as the hub's HTML handlers (#9676).
			if want := webstatic.ETagFor(data); etag != want {
				t.Fatalf("ETag = %q, want shared ETagFor value %q", etag, want)
			}
			if w.Code != http.StatusOK || etag == "" || w.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("initial response: status=%d headers=%v", w.Code, w.Header())
			}
			r := httptest.NewRequest(http.MethodGet, url, nil)
			r.Header.Set("If-None-Match", etag)
			w = httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("revalidation: status=%d headers=%v body length=%d", w.Code, w.Header(), w.Body.Len())
			}
		})
	}
}
