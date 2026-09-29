package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// dashboardRequest builds a request that will reach the dashboardHTML branch
// of handleDashboard: a real browser UA (not an unfurl bot) carrying a
// non-empty hive_hub_user cookie.
func dashboardRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (test)")
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "test-session"})
	return req
}

func TestHandleDashboardSetsNoCacheAndETag(t *testing.T) {
	s := &HubServer{}
	rec := httptest.NewRecorder()
	s.handleDashboard(rec, dashboardRequest())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want %q", got, "no-cache")
	}
	if got := rec.Header().Get("ETag"); got != dashboardHTMLETag {
		t.Fatalf("ETag = %q, want %q", got, dashboardHTMLETag)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("expected a non-empty dashboard body on a fresh request")
	}
}

func TestHandleDashboardAnswersMatchingIfNoneMatchWith304(t *testing.T) {
	s := &HubServer{}
	req := dashboardRequest()
	req.Header.Set("If-None-Match", dashboardHTMLETag)
	rec := httptest.NewRecorder()
	s.handleDashboard(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body length = %d, want 0 on 304", rec.Body.Len())
	}
}

func TestServeStaticSetsNoCacheAndETag(t *testing.T) {
	s := &HubServer{}
	handler := s.serveStatic("static/robots.txt")
	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want %q", got, "no-cache")
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected a non-empty ETag")
	}

	// A second request presenting that ETag back gets a 304 with an empty body.
	req2 := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	handler(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d", rec2.Code, http.StatusNotModified)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("body length = %d, want 0 on 304", rec2.Body.Len())
	}
}

func TestServeStaticETagDiffersForDifferentFiles(t *testing.T) {
	s := &HubServer{}

	rec1 := httptest.NewRecorder()
	s.serveStatic("static/robots.txt")(rec1, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))

	rec2 := httptest.NewRecorder()
	s.serveStatic("static/tokens.css")(rec2, httptest.NewRequest(http.MethodGet, "/tokens.css", nil))

	etag1 := rec1.Header().Get("ETag")
	etag2 := rec2.Header().Get("ETag")
	if etag1 == "" || etag2 == "" {
		t.Fatalf("expected non-empty ETags, got %q and %q", etag1, etag2)
	}
	if etag1 == etag2 {
		t.Fatalf("ETags for different files matched: %q", etag1)
	}
}
