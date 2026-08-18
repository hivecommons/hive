package dashboard

// Tests for the custom-style HTTP handlers (handleStyle, handleLeaderboardStyle)
// and the validation/fetch/cache pipeline they exercise.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- validateCustomStyleSource ---

func TestValidateCustomStyleSource(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string
		want    customStyleSource
	}{
		{"valid simple", "acme/theme/style.css", "", customStyleSource{"acme", "theme", "style.css", "HEAD"}},
		{"valid nested path", "acme/theme/dir/sub.css", "", customStyleSource{"acme", "theme", "dir/sub.css", "HEAD"}},
		{"valid with ref", "acme/theme/style.css@main", "", customStyleSource{"acme", "theme", "style.css", "main"}},
		{"valid tag ref", "acme/theme/s.css@v1.2.3", "", customStyleSource{"acme", "theme", "s.css", "v1.2.3"}},
		{"empty source", "", "style source is required", customStyleSource{}},
		{"full URL rejected", "https://evil.com/style.css", "not a full URL", customStyleSource{}},
		{"protocol-relative rejected", "//evil.com/style.css", "not a full URL", customStyleSource{}},
		{"too few parts", "acme/theme", "must be owner/repo/path.css", customStyleSource{}},
		{"empty ref", "acme/theme/s.css@", "ref is empty", customStyleSource{}},
		{"invalid owner", "-badowner/repo/s.css", "invalid owner", customStyleSource{}},
		{"invalid repo dot-dot", "acme/../s.css", "invalid repo", customStyleSource{}},
		{"non-css path", "acme/repo/style.js", "must end in .css", customStyleSource{}},
		{"path traversal", "acme/repo/../evil.css", "path traversal", customStyleSource{}},
		{"invalid ref", "acme/repo/s.css@../evil", "invalid ref", customStyleSource{}},
		{"ref with dotdot", "acme/repo/s.css@refs/heads/../../etc", "invalid ref", customStyleSource{}},
		{"backslash in path", "acme/repo/dir\\s.css", "invalid path", customStyleSource{}},
		{"path with colon", "acme/repo/http://x.css", "not a full URL", customStyleSource{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateCustomStyleSource(tt.src)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("got err=%v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// --- validateCustomStylePath ---

func TestValidateCustomStylePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{"valid simple", "theme.css", ""},
		{"valid nested", "dir/sub/style.css", ""},
		{"empty path", "", "invalid path"},
		{"leading slash", "/style.css", "invalid path"},
		{"backslash", "dir\\style.css", "invalid path"},
		{"protocol in path", "http://evil.css", "invalid path"},
		{"not css extension", "style.js", "must end in .css"},
		{"dot segment", "dir/./style.css", "path traversal"},
		{"double-dot segment", "dir/../style.css", "path traversal"},
		{"empty segment", "dir//style.css", "path traversal"},
		{"special chars", "dir/st yle.css", "unsupported characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCustomStylePath(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("got err=%v, want containing %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// --- normalizeCustomStyleScope ---

func TestNormalizeCustomStyleScope(t *testing.T) {
	tests := []struct {
		input     string
		wantScope string
		wantRoot  string
		wantErr   bool
	}{
		{"dashboard", "dashboard", "#hive-dashboard-root", false},
		{"leaderboard", "leaderboard", "#tab-leaderboard", false},
		{"", "dashboard", "#hive-dashboard-root", false},
		{"  dashboard  ", "dashboard", "#hive-dashboard-root", false},
		{"invalid", "", "", true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("scope=%q", tt.input), func(t *testing.T) {
			scope, root, err := normalizeCustomStyleScope(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if scope != tt.wantScope || root != tt.wantRoot {
				t.Errorf("got scope=%q root=%q, want %q %q", scope, root, tt.wantScope, tt.wantRoot)
			}
		})
	}
}

// --- customStyleRawURL ---

func TestCustomStyleRawURL_Segments(t *testing.T) {
	src := customStyleSource{Owner: "acme", Repo: "theme", Path: "css/style.css", Ref: "main"}
	got := customStyleRawURL(src)
	want := "https://raw.githubusercontent.com/acme/theme/main/css/style.css"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- handleStyle handler integration ---

func TestHandleStyle_FetchAndServeCSS(t *testing.T) {
	// Mock GitHub raw content server.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Write([]byte(".card { color: red; }"))
	}))
	defer backend.Close()
	// Override the raw base URL for this test.
	orig := customStyleRawBaseURL
	customStyleRawBaseURL = backend.URL
	defer func() { customStyleRawBaseURL = orig }()
	// Clear cache.
	customStyleCacheMu.Lock()
	customStyleCache = map[string]customStyleCacheEntry{}
	customStyleCacheMu.Unlock()

	s := newTestStyleServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/style?src=acme/theme/style.css&scope=dashboard", nil)
	rec := httptest.NewRecorder()
	s.handleStyle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
	if !strings.Contains(rec.Body.String(), "#hive-dashboard-root") {
		t.Errorf("CSS not scoped: %s", rec.Body.String())
	}
}

func TestHandleStyle_ReportMode(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		// Include something that will be dropped (import).
		w.Write([]byte("@import url('evil.css');\n.card { color: blue; }"))
	}))
	defer backend.Close()
	orig := customStyleRawBaseURL
	customStyleRawBaseURL = backend.URL
	defer func() { customStyleRawBaseURL = orig }()
	customStyleCacheMu.Lock()
	customStyleCache = map[string]customStyleCacheEntry{}
	customStyleCacheMu.Unlock()

	s := newTestStyleServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/style?src=acme/theme/style.css&scope=dashboard&report=1", nil)
	rec := httptest.NewRecorder()
	s.handleStyle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var result struct {
		Source string                    `json:"source"`
		CSS    string                    `json:"css"`
		Report customStyleSanitizeReport `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if result.Report.Dropped == 0 {
		t.Error("expected dropped > 0 for @import")
	}
}

func TestHandleStyle_InvalidSource(t *testing.T) {
	s := newTestStyleServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/style?src=invalid", nil)
	rec := httptest.NewRecorder()
	s.handleStyle(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}

func TestHandleStyle_InvalidScope(t *testing.T) {
	s := newTestStyleServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/style?src=acme/theme/style.css&scope=evil", nil)
	rec := httptest.NewRecorder()
	s.handleStyle(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}

func TestHandleStyle_NotFound(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()
	orig := customStyleRawBaseURL
	customStyleRawBaseURL = backend.URL
	defer func() { customStyleRawBaseURL = orig }()
	customStyleCacheMu.Lock()
	customStyleCache = map[string]customStyleCacheEntry{}
	customStyleCacheMu.Unlock()

	s := newTestStyleServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/style?src=acme/theme/missing.css", nil)
	rec := httptest.NewRecorder()
	s.handleStyle(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestHandleLeaderboardStyle_SetsScope(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Write([]byte(".card { font-size: 14px; }"))
	}))
	defer backend.Close()
	orig := customStyleRawBaseURL
	customStyleRawBaseURL = backend.URL
	defer func() { customStyleRawBaseURL = orig }()
	customStyleCacheMu.Lock()
	customStyleCache = map[string]customStyleCacheEntry{}
	customStyleCacheMu.Unlock()

	s := newTestStyleServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/leaderboard/style?src=acme/theme/style.css", nil)
	rec := httptest.NewRecorder()
	s.handleLeaderboardStyle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "#tab-leaderboard") {
		t.Errorf("CSS not scoped to leaderboard: %s", rec.Body.String())
	}
}

func TestHandleStyle_NonCSSContentType(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte("alert(1)"))
	}))
	defer backend.Close()
	orig := customStyleRawBaseURL
	customStyleRawBaseURL = backend.URL
	defer func() { customStyleRawBaseURL = orig }()
	customStyleCacheMu.Lock()
	customStyleCache = map[string]customStyleCacheEntry{}
	customStyleCacheMu.Unlock()

	s := newTestStyleServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/style?src=acme/theme/style.css", nil)
	rec := httptest.NewRecorder()
	s.handleStyle(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 for non-CSS content-type (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestHandleStyle_CacheHit(t *testing.T) {
	hitCount := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount++
		w.Header().Set("Content-Type", "text/css")
		w.Write([]byte(".x { opacity: 1; }"))
	}))
	defer backend.Close()
	orig := customStyleRawBaseURL
	customStyleRawBaseURL = backend.URL
	defer func() { customStyleRawBaseURL = orig }()
	customStyleCacheMu.Lock()
	customStyleCache = map[string]customStyleCacheEntry{}
	customStyleCacheMu.Unlock()

	s := newTestStyleServer(t)

	// First request populates cache.
	req := httptest.NewRequest(http.MethodGet, "/api/style?src=acme/theme/style.css&scope=dashboard", nil)
	rec := httptest.NewRecorder()
	s.handleStyle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first req: status = %d", rec.Code)
	}

	// Second request should hit cache.
	req2 := httptest.NewRequest(http.MethodGet, "/api/style?src=acme/theme/style.css&scope=dashboard", nil)
	rec2 := httptest.NewRecorder()
	s.handleStyle(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second req: status = %d", rec2.Code)
	}

	if hitCount != 1 {
		t.Errorf("backend was hit %d times, want 1 (cache miss)", hitCount)
	}
}

// --- helpers ---

func newTestStyleServer(t *testing.T) *Server {
	t.Helper()
	var logBuf bytes.Buffer
	s := NewServer(0, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return s
}

// --- backward-compat wrappers ---

func TestValidateLeaderboardCustomStyleSource_Compat(t *testing.T) {
	src, err := validateLeaderboardCustomStyleSource("acme/theme/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if src.Owner != "acme" || src.Repo != "theme" {
		t.Errorf("unexpected source: %+v", src)
	}
}

func TestLeaderboardCustomStyleCacheKey(t *testing.T) {
	src := customStyleSource{Owner: "a", Repo: "b", Path: "c.css", Ref: "HEAD"}
	got := leaderboardCustomStyleCacheKey(src)
	if got == "" {
		t.Error("cache key is empty")
	}
}

func TestGetLeaderboardCustomStyle(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Write([]byte(".lb { margin: 0; }"))
	}))
	defer backend.Close()
	orig := customStyleRawBaseURL
	customStyleRawBaseURL = backend.URL
	defer func() { customStyleRawBaseURL = orig }()
	customStyleCacheMu.Lock()
	customStyleCache = map[string]customStyleCacheEntry{}
	customStyleCacheMu.Unlock()

	css, src, err := getLeaderboardCustomStyle(context.Background(), "acme/theme/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if src.Owner != "acme" {
		t.Errorf("source = %+v", src)
	}
	if !strings.Contains(string(css), "#tab-leaderboard") {
		t.Errorf("CSS not scoped: %s", string(css))
	}
}
