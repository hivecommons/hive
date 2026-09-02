package dashboard

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestInjectBrandingPlacesLinkBeforeHeadClose(t *testing.T) {
	got := injectBranding([]byte("<html><head><title>x</title></head><body>b</body></html>"))
	if !bytes.Contains(got, []byte(brandingLinkTag)) {
		t.Fatalf("override link not injected: %s", got)
	}
	// It must be LAST in head, or the embedded document's own styles win.
	if bytes.Index(got, []byte(brandingLinkTag)) > bytes.Index(got, []byte("</head>")) {
		t.Error("override link injected after </head>; it would not win the cascade")
	}
}

// A document with no </head> must be returned untouched rather than corrupted.
func TestInjectBrandingNoHeadIsInert(t *testing.T) {
	in := []byte("not really html")
	if got := injectBranding(in); !bytes.Equal(got, in) {
		t.Errorf("document without </head> was modified: %s", got)
	}
}

func TestBrandingCSSServedWhenPresentAnd404WhenNot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_BRANDING_CSS", filepath.Join(dir, "custom.css"))
	s := &Server{}

	// Absent: the index links to this path unconditionally, so a missing
	// override is the NORMAL case and must 404 quietly, not error.
	w := httptest.NewRecorder()
	s.handleBrandingCSS(w, httptest.NewRequest(http.MethodGet, "/branding/custom.css", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("missing override: got %d, want 404", w.Code)
	}

	// Present: served as CSS.
	css := ":root{--accent:#0aa}"
	if err := os.WriteFile(filepath.Join(dir, "custom.css"), []byte(css), 0o644); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleBrandingCSS(w, httptest.NewRequest(http.MethodGet, "/branding/custom.css", nil))
	if w.Code != http.StatusOK || w.Body.String() != css {
		t.Errorf("got %d %q, want 200 %q", w.Code, w.Body.String(), css)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// The one that matters: the SERVED document must carry the override link.
// Testing injectBranding() alone passes even if newIndexDocument never calls
// it, so this goes through the real document builder and the real handler.
func TestServedIndexCarriesBrandingLink(t *testing.T) {
	doc := newIndexDocument([]byte("<html><head><title>hive</title></head><body></body></html>"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "identity") // want the plain body to assert on
	w := httptest.NewRecorder()
	doc.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(brandingLinkTag)) {
		t.Errorf("served index is missing the branding link — newIndexDocument is not injecting it:\n%s", w.Body.String())
	}
}
