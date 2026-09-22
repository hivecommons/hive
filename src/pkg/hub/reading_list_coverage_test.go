package hub

import (
	"encoding/json"
	"fmt"
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

const rssItem = `<item><title><![CDATA[%s]]></title><link>%s</link><pubDate>%s</pubDate></item>`

func rssBody(items ...string) string {
	return `<?xml version="1.0"?><rss version="2.0"><channel>` + strings.Join(items, "") + `</channel></rss>`
}

func TestParseFeedDate(t *testing.T) {
	if _, ok := parseFeedDate(""); ok {
		t.Error("empty should not parse")
	}
	if _, ok := parseFeedDate("garbage"); ok {
		t.Error("garbage should not parse")
	}
	tm, ok := parseFeedDate("Tue, 22 Sep 2026 19:38:56 GMT")
	if !ok || tm.Year() != 2026 || tm.Month() != time.September || tm.Day() != 22 {
		t.Errorf("rfc1123 parse failed: %v %v", tm, ok)
	}
	if _, ok := parseFeedDate("Tue, 22 Sep 2026 19:38:56 +0000"); !ok {
		t.Error("rfc1123z should parse")
	}
	if _, ok := parseFeedDate("2026-04-30T01:51:06Z"); !ok {
		t.Error("rfc3339 should parse")
	}
}

func TestParseReadingList(t *testing.T) {
	// Two valid items after cutoff + one before cutoff (excluded) + one dupe
	// URL (deduped). Titles use CDATA the way Substack emits them.
	body := rssBody(
		fmt.Sprintf(rssItem, "New Article", "https://x/new", "Fri, 01 May 2026 00:00:00 GMT"),
		fmt.Sprintf(rssItem, "Newer Article", "https://x/newer", "Mon, 01 Jun 2026 00:00:00 GMT"),
		fmt.Sprintf(rssItem, "Pre-cutoff", "https://x/old", "Wed, 01 Jan 2025 00:00:00 GMT"),
		fmt.Sprintf(rssItem, "Dupe", "https://x/new", "Sat, 02 May 2026 00:00:00 GMT"),
	)
	arts, err := parseReadingList([]byte(body))
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("expected 2 articles, got %d: %+v", len(arts), arts)
	}
	if arts[0].Date < arts[1].Date {
		t.Errorf("not sorted newest-first: %+v", arts)
	}
	for _, a := range arts {
		if a.Author != readingListAuthor {
			t.Errorf("author = %q, want %q", a.Author, readingListAuthor)
		}
	}
}

func TestParseReadingListSkipsIncomplete(t *testing.T) {
	// Missing title / link -> skipped.
	body := rssBody(`<item><pubDate>Fri, 01 May 2026 00:00:00 GMT</pubDate></item>`)
	arts, err := parseReadingList([]byte(body))
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(arts) != 0 {
		t.Errorf("expected 0 articles, got %+v", arts)
	}
}

func TestParseReadingListRejectsNonXML(t *testing.T) {
	if _, err := parseReadingList([]byte("<html><body>not a feed")); err == nil {
		t.Error("expected error for non-XML body")
	}
}

func TestFetchReadingListSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(rssBody(fmt.Sprintf(rssItem, "H", "https://x/1", "Fri, 01 May 2026 00:00:00 GMT"))))
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
	rlCache.source = readingListSourceFeed
	rlCache.mu.Unlock()

	s := &HubServer{logger: slog.Default()}
	arts, _, source := s.readingList()
	if source != readingListSourceFeed || len(arts) != 1 || arts[0].Title != "Cached" {
		t.Errorf("expected fresh cache, got %q %+v", source, arts)
	}
}

func TestReadingListStaleCacheFallback(t *testing.T) {
	resetRLCache(t)
	// Stale cache + failing fetch -> serve last-good cache with "cache" source.
	rlCache.mu.Lock()
	rlCache.articles = []ReadingArticle{{Title: "Old", URL: "https://x/o"}}
	rlCache.fetchedAt = time.Now().Add(-2 * readingListCacheTTL)
	rlCache.source = readingListSourceFeed
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
