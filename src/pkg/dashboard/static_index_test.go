package dashboard

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// testIndexBody is intentionally repetitive so the gzipped form is measurably
// smaller than the raw form — a positive control that compression actually
// happened rather than the raw bytes being relabeled.
var testIndexBody = []byte("<!DOCTYPE html><html>" + strings.Repeat("<div>hive dashboard</div>", 200) + "</html>")

func newTestIndex(t *testing.T) *indexDocument {
	t.Helper()
	d := newIndexDocument(testIndexBody, Branding{})
	if d.gzipped == nil {
		t.Fatal("gzip precompression failed for test body")
	}
	if len(d.gzipped) >= len(d.raw) {
		t.Fatalf("gzipped form (%d bytes) not smaller than raw (%d bytes)", len(d.gzipped), len(d.raw))
	}
	return d
}

func TestIndexDocumentServesGzipWhenAccepted(t *testing.T) {
	d := newTestIndex(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompressing body: %v", err)
	}
	if string(decoded) != string(testIndexBody) {
		t.Fatal("decompressed body does not round-trip to the raw document")
	}
}

func TestIndexDocumentServesIdentityWithoutAcceptEncoding(t *testing.T) {
	d := newTestIndex(t)
	for _, ae := range []string{"", "identity", "gzip;q=0", "br"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if ae != "" {
			req.Header.Set("Accept-Encoding", ae)
		}
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Accept-Encoding=%q: status = %d, want 200", ae, rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("Accept-Encoding=%q: unexpected Content-Encoding %q", ae, got)
		}
		if rec.Body.String() != string(testIndexBody) {
			t.Fatalf("Accept-Encoding=%q: body is not the raw document", ae)
		}
	}
}

func TestIndexDocumentRevalidation(t *testing.T) {
	d := newTestIndex(t)

	// First load discovers the ETag.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	etag := rec.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("ETag = %q, want a quoted validator", etag)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}

	// Revalidation with the same ETag — including a weak-prefixed or listed
	// form — must produce an empty-body 304.
	for _, inm := range []string{etag, "W/" + etag, `"stale-etag", ` + etag, "*"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("If-None-Match", inm)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match=%q: status = %d, want 304", inm, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("If-None-Match=%q: 304 carried a %d-byte body", inm, rec.Body.Len())
		}
	}

	// A mismatched validator serves the full document.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", `"something-else"`)
	rec = httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("mismatched If-None-Match: status = %d, body = %d bytes; want 200 with body", rec.Code, rec.Body.Len())
	}
}

func TestIndexDocumentHeadHasNoBody(t *testing.T) {
	d := newTestIndex(t)
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD carried a %d-byte body", rec.Body.Len())
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"gzip, deflate, br", true},
		{"br;q=1.0, gzip;q=0.8", true},
		{"*", true},
		{"GZIP", true},
		{"", false},
		{"identity", false},
		{"gzip;q=0", false},
		{"gzip;q=0.0", false},
		{"br, deflate", false},
	}
	for _, c := range cases {
		if got := acceptsGzip(c.header); got != c.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

func TestStaticTerminalLinksRenewAssertionBeforeOpening(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const TERMINAL_ASSERTION_RENEW_PATH = '/api/terminal/assertion/renew';",
		"const TERMINAL_HANDOFF_PATH = '/api/terminal/handoff';",
		"async function createTerminalHandoff()",
		"async function dashboardTokenConfigured()",
		"async function openAuthenticatedLink",
		"case 'openAuthenticatedLink': e.preventDefault();",
		"async function renewTerminalAssertion()",
		"credentials: 'same-origin'",
		"case 'openTerminal': e.preventDefault(); openTerminal(agent, el.href); break;",
		"data-action=\"openTerminal\"",
		"openTerminal(name);",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard terminal renewal wiring missing %q", want)
		}
	}
	if strings.Contains(html, "window.open(terminalUrl(name), '_blank', 'noopener');") {
		t.Fatal("welcome terminal action still opens /terminal directly without renewing the assertion")
	}
	if strings.Contains(html, "token=${encodeURIComponent(t)}") || strings.Contains(html, "tokenParam") {
		t.Fatal("static dashboard still appends the shared token to URLs")
	}
}
