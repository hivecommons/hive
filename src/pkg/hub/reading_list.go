package hub

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reading List: a dynamic feed of KubeStellar Medium articles dated April 2026
// and later. The browser cannot fetch Medium directly (CSP/CORS) and Medium
// exposes no RSS/JSON API for a curated list, so the hub fetches the list page
// server-side, parses the article markup, filters by date, and serves JSON.
//
// Reconciling "dynamic" with "always works": the result is cached in-memory for
// a few hours (so we don't hit Medium on every request) and, on any fetch or
// parse failure, we fall back to the last good cache — or, if we've never had a
// good fetch, to a baked-in seed list. New Apr-2026+ articles are picked up
// automatically as Medium updates, while the section is never empty.
const (
	// readingListMediumBase resolves bare "/slug" article hrefs to absolute URLs.
	readingListMediumBase = "https://kubestellar.medium.com"

	// readingListCacheTTL is how long a good fetch is served before we refetch.
	readingListCacheTTL = 6 * time.Hour

	// readingListFetchTimeout bounds the server-side fetch of the Medium page.
	readingListFetchTimeout = 15 * time.Second

	// readingListMaxBytes caps how much of the Medium HTML we read (defensive).
	readingListMaxBytes = 8 << 20 // 8MB

	// readingListSourceMedium/Cache/Seed label the provenance of a response.
	readingListSourceMedium = "medium"
	readingListSourceCache  = "cache"
	readingListSourceSeed   = "seed"
)

// readingListCutoff is the inclusive lower bound for the date filter: articles
// published on or after April 1, 2026 are included; everything earlier (2025,
// 2024, …) is excluded.
var readingListCutoff = time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)

// readingListURLVar is the KubeStellar Medium curated list fetched server-side.
// It is a var (not a const) so tests can point fetchReadingList at an httptest
// server; production never reassigns it.
var readingListURLVar = "https://kubestellar.medium.com/list/reading-list"

// ReadingArticle is a single Medium article in the reading list.
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

// readingListSeed is the baked-in seed list of the current Apr-2026+ articles,
// newest first. It is the always-available fallback and is kept in this one
// place. The frontend keeps a minimal copy for the no-JS/failed-fetch case.
var readingListSeed = []ReadingArticle{
	{Title: "We're All Wasting Our Time", Author: "KubeStellar", URL: readingListMediumBase + "/were-all-wasting-our-time-4d2e8507cd79", Date: "2026-07-01", DateLabel: "Jul 1, 2026"},
	{Title: "When each step is fine but the destination isn't", Author: "KubeStellar", URL: readingListMediumBase + "/when-each-step-is-fine-but-the-destination-isnt-fb466b557d8d", Date: "2026-06-29", DateLabel: "Jun 29, 2026"},
	{Title: "Terminal as Interface: Structured Event Streaming for Reliable AI Agent Orchestration", Author: "KubeStellar", URL: readingListMediumBase + "/terminal-as-interface-structured-event-streaming-for-reliable-ai-agent-orchestration-692a996a74af", Date: "2026-06-09", DateLabel: "Jun 9, 2026"},
	{Title: "Hive Hub and Spoke: A Swarm Intelligence Platform for Open Source", Author: "KubeStellar", URL: readingListMediumBase + "/hive-hub-and-spoke-a-swarm-intelligence-platform-for-open-source-7efa1465e2bf", Date: "2026-06-08", DateLabel: "Jun 8, 2026"},
	{Title: "When the Feedback Loops Started Closing Themselves", Author: "KubeStellar", URL: readingListMediumBase + "/when-the-feedback-loops-started-closing-themselves-b9381a7e9773", Date: "2026-05-13", DateLabel: "May 13, 2026"},
	{Title: "Plaque: The Silent Killer of AI Agent Reliability", Author: "KubeStellar", URL: readingListMediumBase + "/plaque-the-silent-killer-of-ai-agent-reliability-f5c5318c4e9c", Date: "2026-05-11", DateLabel: "May 11, 2026"},
	{Title: "Intentional Amnesia: Why I Wipe My AI Agents' Memories Every 15 Minutes", Author: "KubeStellar", URL: readingListMediumBase + "/intentional-amnesia-why-i-wipe-my-ai-agents-memories-every-15-minutes-1c27e9dcbf0a", Date: "2026-05-04", DateLabel: "May 4, 2026"},
	{Title: "The Test Suite That Made Autonomy Possible", Author: "KubeStellar", URL: readingListMediumBase + "/the-test-suite-that-made-autonomy-possible-127d2fcc0b66", Date: "2026-04-30", DateLabel: "Apr 30, 2026"},
	{Title: "The JSON File That Decides What My AI Gets to Work On", Author: "Andy Anderson", URL: readingListMediumBase + "/the-json-file-that-decides-what-my-ai-gets-to-work-on-c50da6c18c14", Date: "2026-04-16", DateLabel: "Apr 16, 2026"},
	{Title: "I Purposely Built a Codebase That Teaches Itself", Author: "Andy Anderson", URL: readingListMediumBase + "/i-purposely-built-a-codebase-that-teaches-itself-dbf34915b148", Date: "2026-04-07", DateLabel: "Apr 7, 2026"},
}

// readingListCache holds the last successfully-fetched list and its provenance.
// A single package-level cache is sufficient: the list is global, not per-hive.
type readingListCache struct {
	mu        sync.Mutex
	articles  []ReadingArticle
	fetchedAt time.Time
	source    string // readingListSourceMedium once a real fetch has succeeded
}

var rlCache = &readingListCache{}

// handleReadingList serves GET /api/reading-list. It returns a cached response
// when fresh, otherwise refetches from Medium; on failure it serves the last
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
// refetching from Medium when the cache is stale. It is safe for concurrent use.
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
		rlCache.source = readingListSourceMedium
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

// fetchReadingList fetches the Medium list page server-side, parses the
// articles, filters to the cutoff date, and returns them newest-first. It
// follows the outbound-HTTP pattern used elsewhere in this package
// (&http.Client{Timeout: ...}, http.NewRequest).
func (s *HubServer) fetchReadingList() ([]ReadingArticle, error) {
	req, err := http.NewRequest(http.MethodGet, readingListURLVar, nil)
	if err != nil {
		return nil, err
	}
	// A browser-like UA gets the fully server-rendered list markup.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; HiveHub/1.0; +https://hive.kubestellar.io)")
	req.Header.Set("Accept", "text/html")

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
	return parseReadingList(string(body)), nil
}

// mediumPostingPattern matches each per-article JSON-LD "SocialMediaPosting"
// object Medium embeds in the list page. Medium ships one such object per
// article with clean, structured fields (headline, author, datePublished,
// mainEntityOfPage) — far more stable than scraping presentational HTML tags.
// The [^{}] body match is deliberately shallow: these objects contain nested
// author/publisher objects, so we capture the whole flat run of the posting up
// to its author sub-object and pull individual fields with the patterns below.
var mediumPostingPattern = regexp.MustCompile(`"@type":"SocialMediaPosting"`)

var (
	// mediumHeadlinePattern captures an article headline (JSON-escaped).
	mediumHeadlinePattern = regexp.MustCompile(`"headline":"((?:[^"\\]|\\.)*)"`)
	// mediumAuthorNamePattern captures the first author "name" after a posting.
	mediumAuthorNamePattern = regexp.MustCompile(`"name":"((?:[^"\\]|\\.)*)"`)
	// mediumDatePublishedPattern captures the ISO-8601 publish timestamp.
	mediumDatePublishedPattern = regexp.MustCompile(`"datePublished":"([^"]+)"`)
	// mediumMainEntityPattern captures the canonical article URL.
	mediumMainEntityPattern = regexp.MustCompile(`"mainEntityOfPage":"([^"]+)"`)
)

// parseReadingList extracts articles from Medium list-page HTML by reading the
// embedded JSON-LD "SocialMediaPosting" objects (one per article), filtering to
// the cutoff date, and returning them newest-first. It is deliberately
// defensive: Medium markup changes, so any object it cannot fully parse is
// skipped rather than fatal, and a zero-length result signals the caller to
// fall back to cache/seed.
func parseReadingList(htmlBody string) []ReadingArticle {
	seen := make(map[string]bool) // dedupe by URL
	var articles []ReadingArticle

	// Each posting object is a bounded window starting at its @type marker and
	// ending at the next posting (or end of document).
	starts := mediumPostingPattern.FindAllStringIndex(htmlBody, -1)
	for i, s := range starts {
		start := s[0]
		end := len(htmlBody)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		obj := htmlBody[start:end]

		iso := firstGroup(mediumDatePublishedPattern, obj)
		date, ok := parseISODate(iso)
		if !ok || date.Before(readingListCutoff) {
			continue
		}

		title := jsonUnescape(firstGroup(mediumHeadlinePattern, obj))
		url := jsonUnescape(firstGroup(mediumMainEntityPattern, obj))
		author := jsonUnescape(firstGroup(mediumAuthorNamePattern, obj))
		if title == "" || url == "" || seen[url] {
			continue
		}

		seen[url] = true
		articles = append(articles, ReadingArticle{
			Title:     title,
			Author:    author,
			URL:       url,
			Date:      date.Format("2006-01-02"),
			DateLabel: date.Format("Jan 2, 2006"),
		})
	}

	// Newest first.
	sort.SliceStable(articles, func(i, j int) bool {
		return articles[i].Date > articles[j].Date
	})
	return articles
}

// firstGroup returns the first capture group of re in s, or "".
func firstGroup(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}

// jsonUnescape decodes a JSON string body (the inside of the quotes) into its
// literal text, resolving \uXXXX and common escapes. On any decode error it
// returns the input unchanged so a partial value is still usable.
func jsonUnescape(s string) string {
	if s == "" {
		return ""
	}
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return s
	}
	return strings.TrimSpace(out)
}

// parseISODate parses an ISO-8601 timestamp (e.g. "2026-04-30T01:51:06Z") from
// Medium's datePublished field into a UTC date. It returns ok=false when empty
// or unparseable.
func parseISODate(iso string) (time.Time, bool) {
	iso = strings.TrimSpace(iso)
	if iso == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, iso); err == nil {
		return t.UTC(), true
	}
	// Fall back to a date-only prefix if the full timestamp doesn't parse.
	if len(iso) >= 10 {
		if t, err := time.Parse("2006-01-02", iso[:10]); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
