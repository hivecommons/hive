package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================
// reading_list.go
// ============================================================

// resetRLCache clears the package-level reading-list cache so tests are
// independent of one another and of execution order.
func resetRLCache(t *testing.T) {
	t.Helper()
	rlCache.mu.Lock()
	rlCache.articles = nil
	rlCache.fetchedAt = time.Time{}
	rlCache.source = ""
	rlCache.mu.Unlock()
}

func TestFirstGroup(t *testing.T) {
	if got := firstGroup(mediumHeadlinePattern, `"headline":"Hello"`); got != "Hello" {
		t.Errorf("firstGroup match = %q, want Hello", got)
	}
	if got := firstGroup(mediumHeadlinePattern, `no match here`); got != "" {
		t.Errorf("firstGroup no-match = %q, want empty", got)
	}
}

func TestJSONUnescape(t *testing.T) {
	if got := jsonUnescape(""); got != "" {
		t.Errorf("empty = %q", got)
	}
	if got := jsonUnescape(`Café`); got != "Café" {
		t.Errorf("unicode = %q, want Café", got)
	}
	if got := jsonUnescape(`a\tb`); got != "a\tb" {
		t.Errorf("tab = %q", got)
	}
	// Invalid escape -> returns input unchanged.
	if got := jsonUnescape(`bad\x`); got != `bad\x` {
		t.Errorf("invalid = %q, want unchanged", got)
	}
}

func TestParseISODate(t *testing.T) {
	if _, ok := parseISODate(""); ok {
		t.Error("empty should not parse")
	}
	if _, ok := parseISODate("garbage"); ok {
		t.Error("garbage should not parse")
	}
	tm, ok := parseISODate("2026-04-30T01:51:06Z")
	if !ok || tm.Year() != 2026 || tm.Month() != time.April {
		t.Errorf("rfc3339 parse failed: %v %v", tm, ok)
	}
	// Date-only fallback (not RFC3339).
	tm2, ok := parseISODate("2026-05-11 extra")
	if !ok || tm2.Day() != 11 {
		t.Errorf("date-only fallback failed: %v %v", tm2, ok)
	}
}

func TestParseReadingList(t *testing.T) {
	// Two valid postings after cutoff + one before cutoff (excluded) + one
	// dupe URL (deduped).
	body := `
	{"@type":"SocialMediaPosting","headline":"New Article","datePublished":"2026-05-01T00:00:00Z","mainEntityOfPage":"https://x/new","author":{"name":"Alice"}}
	{"@type":"SocialMediaPosting","headline":"Older Article","datePublished":"2026-06-01T00:00:00Z","mainEntityOfPage":"https://x/older","author":{"name":"Bob"}}
	{"@type":"SocialMediaPosting","headline":"Pre-cutoff","datePublished":"2025-01-01T00:00:00Z","mainEntityOfPage":"https://x/old","author":{"name":"Carol"}}
	{"@type":"SocialMediaPosting","headline":"Dupe","datePublished":"2026-05-02T00:00:00Z","mainEntityOfPage":"https://x/new","author":{"name":"Dave"}}
	`
	arts := parseReadingList(body)
	if len(arts) != 2 {
		t.Fatalf("expected 2 articles, got %d: %+v", len(arts), arts)
	}
	// Newest first.
	if arts[0].Date < arts[1].Date {
		t.Errorf("not sorted newest-first: %+v", arts)
	}
}

func TestParseReadingListSkipsIncomplete(t *testing.T) {
	// Missing headline / URL -> skipped.
	body := `{"@type":"SocialMediaPosting","datePublished":"2026-05-01T00:00:00Z","author":{"name":"Alice"}}`
	if arts := parseReadingList(body); len(arts) != 0 {
		t.Errorf("expected 0 articles, got %+v", arts)
	}
}

func TestFetchReadingListSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"@type":"SocialMediaPosting","headline":"H","datePublished":"2026-05-01T00:00:00Z","mainEntityOfPage":"https://x/1","author":{"name":"A"}}`))
	}))
	defer srv.Close()

	old := readingListURLVar
	readingListURLVar = srv.URL
	defer func() { readingListURLVar = old }()

	s := &HubServer{logger: slog.Default()}
	arts, err := s.fetchReadingList()
	if err != nil {
		t.Fatalf("fetch err: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 article, got %d", len(arts))
	}
}

func TestReadingListFallbackToSeed(t *testing.T) {
	resetRLCache(t)
	// Point at a closed port so fetch fails and seed is served.
	old := readingListURLVar
	readingListURLVar = "http://127.0.0.1:1"
	defer func() { readingListURLVar = old }()

	s := &HubServer{logger: slog.Default()}
	arts, _, source := s.readingList()
	if source != readingListSourceSeed {
		t.Errorf("expected seed source, got %q", source)
	}
	if len(arts) == 0 {
		t.Error("seed should be non-empty")
	}
}

func TestReadingListServesFreshCache(t *testing.T) {
	resetRLCache(t)
	rlCache.mu.Lock()
	rlCache.articles = []ReadingArticle{{Title: "Cached", URL: "https://x/c"}}
	rlCache.fetchedAt = time.Now()
	rlCache.source = readingListSourceMedium
	rlCache.mu.Unlock()

	s := &HubServer{logger: slog.Default()}
	arts, _, source := s.readingList()
	if source != readingListSourceMedium || len(arts) != 1 || arts[0].Title != "Cached" {
		t.Errorf("expected fresh cache, got %q %+v", source, arts)
	}
}

func TestReadingListStaleCacheFallback(t *testing.T) {
	resetRLCache(t)
	// Stale cache + failing fetch -> serve last-good cache with "cache" source.
	rlCache.mu.Lock()
	rlCache.articles = []ReadingArticle{{Title: "Old", URL: "https://x/o"}}
	rlCache.fetchedAt = time.Now().Add(-2 * readingListCacheTTL)
	rlCache.source = readingListSourceMedium
	rlCache.mu.Unlock()

	old := readingListURLVar
	readingListURLVar = "http://127.0.0.1:1"
	defer func() { readingListURLVar = old }()

	s := &HubServer{logger: slog.Default()}
	_, _, source := s.readingList()
	if source != readingListSourceCache {
		t.Errorf("expected cache source, got %q", source)
	}
}

func TestHandleReadingList(t *testing.T) {
	resetRLCache(t)
	old := readingListURLVar
	readingListURLVar = "http://127.0.0.1:1"
	defer func() { readingListURLVar = old }()

	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/reading-list", nil)
	s.handleReadingList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var resp ReadingListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Articles) == 0 {
		t.Error("expected articles in response")
	}
}
