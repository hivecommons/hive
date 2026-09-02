package dashboard

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	doc := newIndexDocument([]byte("<html><head><title>hive</title></head><body></body></html>"), Branding{})

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

// A miniature of the shipped document: every anchor applyBranding targets,
// plus a bare "HIVE" inside a <script> that must NOT be touched.
const sampleIndex = `<html><head><title>Hive Dashboard</title>` +
	`<link rel="icon" href="data:image/svg+xml,<svg><text y='.9em' font-size='90'>🐝</text></svg>">` +
	`</head><body><span class="oc-logo-icon">🐝</span>` +
	`<div class="oc-logo-title">HIVE</div>` +
	`<div class="oc-logo-sub">GATEWAY DASHBOARD</div>` +
	`<h1><span class="bee">🐝</span> KubeStellar Hive Dashboard</h1>` +
	`<span class="wb-bee">&#x1F41D;</span>` +
	`<script>const s="HIVE appears in script too";</script></body></html>`

func TestApplyBrandingReplacesEveryDefault(t *testing.T) {
	out := string(applyBranding([]byte(sampleIndex), Branding{
		ProductName: "REEF", Tagline: "APPLICATIONS", Mark: "🪸", Title: "Reef",
	}))
	for _, want := range []string{
		`<span class="oc-logo-icon">🪸</span>`,
		`<div class="oc-logo-title">REEF</div>`,
		`<div class="oc-logo-sub">APPLICATIONS</div>`,
		`<span class="bee">🪸</span>`,
		`<span class="wb-bee">🪸</span>`,          // entity form, not the glyph
		`<text y='.9em' font-size='90'>🪸</text>`, // favicon
		`<title>Reef</title>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Substitution is anchored to markup; a loose replace would corrupt this.
	if !strings.Contains(out, `"HIVE appears in script too"`) {
		t.Error("unanchored replacement corrupted script content")
	}
}

// An empty field leaves the shipped default alone, so a partial branding.json
// is valid and a missing file changes nothing at all.
func TestApplyBrandingPartialAndEmpty(t *testing.T) {
	if got := string(applyBranding([]byte(sampleIndex), Branding{})); got != sampleIndex {
		t.Error("empty Branding modified the document")
	}
	out := string(applyBranding([]byte(sampleIndex), Branding{ProductName: "SCHOOL"}))
	if !strings.Contains(out, `<div class="oc-logo-title">SCHOOL</div>`) {
		t.Error("product name not applied")
	}
	if !strings.Contains(out, `<div class="oc-logo-sub">GATEWAY DASHBOARD</div>`) {
		t.Error("unset tagline should have been left alone")
	}
}

// Through the real document builder and handler, so this fails if the wiring
// is removed even though applyBranding itself still works.
func TestServedIndexCarriesBrandingStrings(t *testing.T) {
	doc := newIndexDocument([]byte(sampleIndex), Branding{ProductName: "SCHOOL"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "identity")
	w := httptest.NewRecorder()
	doc.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `<div class="oc-logo-title">SCHOOL</div>`) {
		t.Error("served index does not carry the overridden wordmark")
	}
}
