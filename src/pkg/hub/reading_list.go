package hub

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reading List: a dynamic feed of Hive Commons articles published on Substack
// (https://substack.com/@hivecommons). The browser cannot fetch Substack
// directly (CSP/CORS), so the hub fetches the publication's RSS feed
// server-side, parses the items, filters by date, and serves JSON.
//
// Reconciling "dynamic" with "always works": the result is cached in-memory for
// a few hours (so we don't hit Substack on every request) and, on any fetch or
// parse failure, we fall back to the last good cache — or, if we've never had a
// good fetch, to a baked-in seed list. New articles are picked up automatically
// as the feed updates, while the section is never empty.
const (
	// readingListAuthor is the byline shown on every card. The feed's
	// dc:creator carries the Substack display name, which is a publication
	// tagline rather than an author; the project name is what readers expect.
	readingListAuthor = "Hive Commons"

	// readingListCacheTTL is how long a good fetch is served before we refetch.
	readingListCacheTTL = 6 * time.Hour

	// readingListFetchTimeout bounds the server-side fetch of the feed.
	readingListFetchTimeout = 15 * time.Second

	// readingListMaxBytes caps how much of the feed we read (defensive).
	readingListMaxBytes = 8 << 20 // 8MB

	// readingListSourceFeed/Cache/Seed label the provenance of a response.
	readingListSourceFeed  = "substack"
	readingListSourceCache = "cache"
	readingListSourceSeed  = "seed"
)

// readingListCutoff is the inclusive lower bound for the date filter: articles
// published on or after April 1, 2026 are included; everything earlier is
// excluded.
var readingListCutoff = time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)

// readingListURLVar is the Hive Commons Substack RSS feed fetched server-side.
// It is a var (not a const) so tests can point fetchReadingList at an httptest
// server; production never reassigns it.
var readingListURLVar = "https://hivecommons.substack.com/feed"

// ReadingArticle is a single article in the reading list.
type ReadingArticle struct {
	Title     string `json:"title"`
	Author    string `json:"author"`
	URL       string `json:"url"`
	Date      string `json:"date"`      // RFC3339 date only, e.g. "2026-04-30"
	DateLabel string `json:"dateLabel"` // human label, e.g. "Apr 30, 2026"
}

// ReadingListResponse is the JSON shape returned by GET /api/reading-list.
type ReadingListResponse struct {
	Articles  []ReadingArticle `json:"articles"`
	UpdatedAt string           `json:"updatedAt"`
	Source    string           `json:"source"`
}

// readingListSeed is the baked-in seed list of the articles currently on the
// Hive Commons Substack, newest first. It is the always-available fallback and
// is kept in this one place. The frontend keeps a minimal copy for the
// no-JS/failed-fetch case. Articles still on the legacy Medium publication are
// not listed; they reappear here as they are republished on Substack.
var readingListSeed = []ReadingArticle{
	{Title: "Hive at v5: The Swarm Grew Up, and It Is Learning to Leave the Dashboard", Author: readingListAuthor, URL: "https://hivecommons.substack.com/p/hive-at-v5-the-swarm-grew-up-and", Date: "2026-09-22", DateLabel: "Sep 22, 2026"},
	{Title: "Sage Collar: The Idea Economy Needs a Name for the People Building It", Author: readingListAuthor, URL: "https://hivecommons.substack.com/p/sage-collar-the-idea-economy-needs", Date: "2026-09-22", DateLabel: "Sep 22, 2026"},
	{Title: "Hive Hub and Spoke: A Swarm Intelligence Platform for Open Source", Author: readingListAuthor, URL: "https://hivecommons.substack.com/p/hive-hub-and-spoke-a-swarm-intelligence", Date: "2026-09-22", DateLabel: "Sep 22, 2026"},
}

// readingListCache holds the last successfully-fetched list and its provenance.
// A single package-level cache is sufficient: the list is global, not per-hive.
type readingListCache struct {
	mu        sync.Mutex
	articles  []ReadingArticle
	fetchedAt time.Time
	source    string // readingListSourceFeed once a real fetch has succeeded
}

var rlCache = &readingListCache{}

// handleReadingList serves GET /api/reading-list. It returns a cached response
// when fresh, otherwise refetches from the feed; on failure it serves the last
// good cache or the baked-in seed. It never fails the request.
func (s *HubServer) handleReadingList(w http.ResponseWriter, r *http.Request) {
	articles, updatedAt, source := s.readingList()

	resp := ReadingListResponse{
		Articles:  articles,
		UpdatedAt: updatedAt.UTC().Format(time.RFC3339),
		Source:    source,
	}
	data, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// readingList returns the current article list, its as-of time, and provenance,
// refetching from the feed when the cache is stale. It is safe for concurrent use.
func (s *HubServer) readingList() ([]ReadingArticle, time.Time, string) {
	rlCache.mu.Lock()
	defer rlCache.mu.Unlock()

	// Serve a fresh cache without touching the network.
	if len(rlCache.articles) > 0 && time.Since(rlCache.fetchedAt) < readingListCacheTTL {
		return rlCache.articles, rlCache.fetchedAt, rlCache.source
	}

	// Cache is empty or stale: try a live fetch.
	articles, err := s.fetchReadingList()
	if err == nil && len(articles) > 0 {
		rlCache.articles = articles
		rlCache.fetchedAt = time.Now()
		rlCache.source = readingListSourceFeed
		return rlCache.articles, rlCache.fetchedAt, rlCache.source
	}
	if err != nil {
		s.logger.Warn("reading list fetch failed, serving fallback", "error", err)
	} else {
		s.logger.Warn("reading list parse yielded zero articles, serving fallback")
	}

	// Fetch failed: serve the last good cache if we have one.
	if len(rlCache.articles) > 0 {
		return rlCache.articles, rlCache.fetchedAt, readingListSourceCache
	}

	// No cache ever: serve the baked-in seed (and remember it so relative
	// staleness math has a timestamp, but keep source as seed).
	return readingListSeed, time.Now(), readingListSourceSeed
}

// fetchReadingList fetches the Substack RSS feed server-side, parses the
// items, filters to the cutoff date, and returns them newest-first. It
// follows the outbound-HTTP pattern used elsewhere in this package
// (&http.Client{Timeout: ...}, http.NewRequest).
func (s *HubServer) fetchReadingList() ([]ReadingArticle, error) {
	req, err := http.NewRequest(http.MethodGet, readingListURLVar, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; HiveHub/1.0; +https://hive.hivecommons.dev)")
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")

	client := &http.Client{Timeout: readingListFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, readingListMaxBytes))
	if err != nil {
		return nil, err
	}
	return parseReadingList(body)
}

// feedDocument is the subset of RSS 2.0 the reading list needs. Substack's feed
// wraps title in CDATA, which encoding/xml unwraps for us; pubDate is RFC 1123
// with a "GMT" zone; link is the canonical post URL without tracking params.
type feedDocument struct {
	Channel struct {
		Items []feedEntry `xml:"item"`
	} `xml:"channel"`
}

type feedEntry struct {
	Title   string `xml:"title"`
	Link    string `xml:"link"`
	PubDate string `xml:"pubDate"`
}

// parseReadingList extracts articles from an RSS 2.0 feed body, filtering to
// the cutoff date and returning them newest-first. It is deliberately
// defensive: any item it cannot fully parse is skipped rather than fatal, and
// a zero-length result signals the caller to fall back to cache/seed. A body
// that is not XML at all is an error so the caller can log it.
func parseReadingList(body []byte) ([]ReadingArticle, error) {
	var feed feedDocument
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, err
	}

	seen := make(map[string]bool) // dedupe by URL
	var articles []ReadingArticle
	for _, it := range feed.Channel.Items {
		date, ok := parseFeedDate(it.PubDate)
		if !ok || date.Before(readingListCutoff) {
			continue
		}
		title := strings.TrimSpace(it.Title)
		url := strings.TrimSpace(it.Link)
		if title == "" || url == "" || seen[url] {
			continue
		}
		seen[url] = true
		articles = append(articles, ReadingArticle{
			Title:     title,
			Author:    readingListAuthor,
			URL:       url,
			Date:      date.Format("2006-01-02"),
			DateLabel: date.Format("Jan 2, 2006"),
		})
	}

	// Newest first.
	sort.SliceStable(articles, func(i, j int) bool {
		return articles[i].Date > articles[j].Date
	})
	return articles, nil
}

// parseFeedDate parses an RSS pubDate. Substack emits RFC 1123 ("Tue, 22 Sep
// 2026 19:38:56 GMT"); RFC 1123Z and RFC 3339 are accepted for other feeds.
// It returns ok=false when empty or unparseable.
func parseFeedDate(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
