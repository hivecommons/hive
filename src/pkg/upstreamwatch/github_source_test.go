package upstreamwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

// newTestClient returns a go-github client whose BaseURL points at mux.
func newTestClient(t *testing.T, mux *http.ServeMux) *gh.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := gh.NewClient(srv.Client())
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.BaseURL = base
	return client
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, body)
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

func TestSplitRepo(t *testing.T) {
	if o, r, ok := SplitRepo(" acme/widgets "); !ok || o != "acme" || r != "widgets" {
		t.Fatalf("SplitRepo = %q %q %v", o, r, ok)
	}
	for _, bad := range []string{"", "widgets", "/widgets", "acme/", "a/b/c"} {
		if _, _, ok := SplitRepo(bad); ok {
			t.Errorf("SplitRepo(%q) accepted", bad)
		}
	}
}

func TestRefHelpers(t *testing.T) {
	if got := RefPR(42); got != "upstream#42" {
		t.Errorf("RefPR = %q", got)
	}
	if got := RefRelease("v1.2.3"); got != "release:v1.2.3" {
		t.Errorf("RefRelease = %q", got)
	}
}

func TestWantsSource(t *testing.T) {
	empty := &GitHubSource{}
	if !empty.wantsSource(config.UpstreamSourcePRs) || !empty.wantsSource(config.UpstreamSourceReleases) {
		t.Error("empty Sources must enable both")
	}
	prsOnly := &GitHubSource{cfg: config.UpstreamWatchRepo{Sources: []string{config.UpstreamSourcePRs}}}
	if !prsOnly.wantsSource(config.UpstreamSourcePRs) || prsOnly.wantsSource(config.UpstreamSourceReleases) {
		t.Error("prs-only cfg must not enable releases")
	}
}

func TestLabelHelpers(t *testing.T) {
	if labelSet(nil) != nil {
		t.Error("labelSet(nil) must be nil")
	}
	if labelSet([]string{"", "   "}) != nil {
		t.Error("labelSet of whitespace must be nil")
	}
	set := labelSet([]string{"Bug", "security"})
	if len(set) != 2 || !set["bug"] || !set["security"] {
		t.Errorf("labelSet = %v", set)
	}
	if !anyLabel([]string{"Security"}, set) {
		t.Error("anyLabel must be case-insensitive")
	}
	if anyLabel([]string{"docs"}, set) {
		t.Error("anyLabel must reject unmatched")
	}
	labels := labelNames([]*gh.Label{{Name: strPtr("bug")}, nil, {Name: strPtr("")}, {Name: strPtr("security")}})
	if len(labels) != 2 || labels[0] != "bug" || labels[1] != "security" {
		t.Errorf("labelNames = %v", labels)
	}
}

func strPtr(s string) *string { return &s }

// explicitSource is the common fixture: upstream pinned, both sources on.
func explicitSource(t *testing.T, mux *http.ServeMux) *GitHubSource {
	t.Helper()
	return NewGitHubSource(newTestClient(t, mux), "fork-org", "fork-repo", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets",
	})
}

func TestNewGitHubSource_InvalidUpstreamFallsBack(t *testing.T) {
	// A malformed Upstream string must not be captured as a pinned
	// upstream — the fork-parent lookup must run on first List instead.
	s := NewGitHubSource(nil, "f", "r", config.UpstreamWatchRepo{Upstream: "not-a-repo"})
	if s.upstream != nil {
		t.Fatalf("upstream = %+v, want nil for malformed cfg.Upstream", s.upstream)
	}
	s2 := NewGitHubSource(nil, "f", "r", config.UpstreamWatchRepo{Upstream: "owner/name"})
	if s2.upstream == nil || s2.upstream.owner != "owner" || s2.upstream.repo != "name" {
		t.Fatalf("explicit upstream not captured: %+v", s2.upstream)
	}
}

// ---------------------------------------------------------------------------
// resolveUpstream
// ---------------------------------------------------------------------------

func TestResolveUpstream_FromParent(t *testing.T) {
	mux := http.NewServeMux()
	var parentCalls int32
	mux.HandleFunc("/repos/fork-org/fork-repo", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&parentCalls, 1)
		writeJSON(w, `{"name":"fork-repo","parent":{"full_name":"upstream/widgets"}}`)
	})
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })
	mux.HandleFunc("/repos/upstream/widgets/releases", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })

	s := NewGitHubSource(newTestClient(t, mux), "fork-org", "fork-repo", config.UpstreamWatchRepo{})
	if _, err := s.List(context.Background(), time.Time{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if s.upstream == nil || s.upstream.owner != "upstream" || s.upstream.repo != "widgets" {
		t.Fatalf("upstream = %+v", s.upstream)
	}
	// Second call must reuse the cached upstream, not re-hit the fork repo.
	if _, err := s.List(context.Background(), time.Time{}); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if n := atomic.LoadInt32(&parentCalls); n != 1 {
		t.Fatalf("parent lookup called %d times, want 1 (cached after first List)", n)
	}
}

func TestResolveUpstream_ExplicitWinsOverParent(t *testing.T) {
	mux := http.NewServeMux()
	// A /repos/fork-org/fork-repo handler is intentionally absent — if the
	// explicit Upstream is honoured the test server is never asked for it.
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })
	mux.HandleFunc("/repos/upstream/widgets/releases", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })
	s := explicitSource(t, mux)
	if _, err := s.List(context.Background(), time.Time{}); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestResolveUpstream_NoParent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/fork-org/fork-repo", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"name":"fork-repo"}`)
	})
	s := NewGitHubSource(newTestClient(t, mux), "fork-org", "fork-repo", config.UpstreamWatchRepo{})
	_, err := s.List(context.Background(), time.Time{})
	if err == nil || !strings.Contains(err.Error(), "no parent") {
		t.Fatalf("err = %v, want no-parent error", err)
	}
}

func TestResolveUpstream_ParentMalformed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/fork-org/fork-repo", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"parent":{"full_name":"only-owner"}}`)
	})
	s := NewGitHubSource(newTestClient(t, mux), "fork-org", "fork-repo", config.UpstreamWatchRepo{})
	_, err := s.List(context.Background(), time.Time{})
	if err == nil || !strings.Contains(err.Error(), "not owner/repo") {
		t.Fatalf("err = %v, want parent full_name error", err)
	}
}

func TestResolveUpstream_GetError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/fork-org/fork-repo", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	s := NewGitHubSource(newTestClient(t, mux), "fork-org", "fork-repo", config.UpstreamWatchRepo{})
	if _, err := s.List(context.Background(), time.Time{}); err == nil {
		t.Fatal("500 from parent lookup did not surface")
	}
}

// ---------------------------------------------------------------------------
// List: full-coverage happy path
// ---------------------------------------------------------------------------

type pullStub struct {
	Number    int              `json:"number"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	HTMLURL   string           `json:"html_url"`
	UpdatedAt string           `json:"updated_at"`
	MergedAt  *string          `json:"merged_at,omitempty"`
	Labels    []map[string]any `json:"labels,omitempty"`
}

type releaseStub struct {
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name,omitempty"`
	Body        string  `json:"body,omitempty"`
	HTMLURL     string  `json:"html_url,omitempty"`
	PublishedAt *string `json:"published_at,omitempty"`
	CreatedAt   *string `json:"created_at,omitempty"`
	Draft       bool    `json:"draft,omitempty"`
}

func pStr(s string) *string { return &s }

func TestList_HappyPath(t *testing.T) {
	since := mustTime(t, "2026-01-01T00:00:00Z")

	mux := http.NewServeMux()

	// Three PRs: one merged after since with matching label (kept), one
	// merged before since (dropped), one closed-not-merged (dropped). The
	// kept PR exercises the files-paging path.
	kept := pullStub{
		Number:    42,
		Title:     "fix: null panic",
		Body:      "body",
		HTMLURL:   "https://x/pr/42",
		UpdatedAt: "2026-02-02T00:00:00Z",
		MergedAt:  pStr("2026-02-01T12:00:00Z"),
		Labels:    []map[string]any{{"name": "bug"}, {"name": "security"}},
	}
	oldMerged := "2025-12-31T00:00:00Z"
	old := pullStub{Number: 10, Title: "old", UpdatedAt: "2025-12-31T00:00:00Z", MergedAt: &oldMerged}
	closedNotMerged := pullStub{Number: 11, Title: "closed", UpdatedAt: "2026-02-02T00:00:00Z"}
	page1, _ := json.Marshal([]pullStub{kept, closedNotMerged, old})

	var pullCalls int32
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pullCalls, 1)
		if got := r.URL.Query().Get("state"); got != "closed" {
			t.Errorf("state = %q", got)
		}
		if got := r.URL.Query().Get("sort"); got != "updated" {
			t.Errorf("sort = %q", got)
		}
		if got := r.URL.Query().Get("direction"); got != "desc" {
			t.Errorf("direction = %q", got)
		}
		writeJSON(w, string(page1))
	})

	var filesCalls int32
	mux.HandleFunc("/repos/upstream/widgets/pulls/42/files", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&filesCalls, 1)
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, `[{"filename":"b.go","additions":3,"deletions":1}]`)
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/upstream/widgets/pulls/42/files?page=2>; rel="next"`)
		writeJSON(w, `[{"filename":"a.go","additions":5,"deletions":2},{"filename":"","additions":0,"deletions":0},null]`)
	})

	rels := []releaseStub{
		{TagName: "v2.0.0", Name: "", Body: "notes", HTMLURL: "https://x/r/v2", PublishedAt: pStr("2026-03-01T00:00:00Z")},
		{TagName: "v1.0.0", PublishedAt: pStr("2025-01-01T00:00:00Z")},                    // dropped: older
		{TagName: "v2.1.0-draft", PublishedAt: pStr("2026-03-05T00:00:00Z"), Draft: true}, // dropped: draft
		{TagName: "v2.2.0", Name: "Two Two", PublishedAt: nil},                            // dropped: no publish date
	}
	relBody, _ := json.Marshal(rels)
	mux.HandleFunc("/repos/upstream/widgets/releases", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, string(relBody))
	})

	s := explicitSource(t, mux)
	items, err := s.List(context.Background(), since)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	// Oldest first: the merged PR (2026-02-01) before the release (2026-03-01).
	if items[0].Kind != KindPR || items[0].Ref != "upstream#42" || items[0].Title != "fix: null panic" ||
		items[0].HTMLURL != "https://x/pr/42" || items[0].Body != "body" {
		t.Fatalf("pr item = %+v", items[0])
	}
	if !items[0].Timestamp.Equal(mustTime(t, "2026-02-01T12:00:00Z")) {
		t.Errorf("pr timestamp = %v", items[0].Timestamp)
	}
	if len(items[0].Labels) != 2 || items[0].Labels[0] != "bug" || items[0].Labels[1] != "security" {
		t.Errorf("pr labels = %v", items[0].Labels)
	}
	if len(items[0].Files) != 2 || items[0].Files[0] != "a.go" || items[0].Files[1] != "b.go" {
		t.Errorf("pr files = %v", items[0].Files)
	}
	if items[0].Additions != 8 || items[0].Deletions != 3 {
		t.Errorf("pr totals = %d/%d", items[0].Additions, items[0].Deletions)
	}
	if items[1].Kind != KindRelease || items[1].Ref != "release:v2.0.0" || items[1].Title != "v2.0.0" ||
		items[1].HTMLURL != "https://x/r/v2" || items[1].Body != "notes" {
		t.Fatalf("release item = %+v", items[1])
	}
	if !items[1].Timestamp.Equal(mustTime(t, "2026-03-01T00:00:00Z")) {
		t.Errorf("release timestamp = %v", items[1].Timestamp)
	}
	if len(items[1].Labels) != 0 || len(items[1].Files) != 0 || items[1].Additions != 0 || items[1].Deletions != 0 {
		t.Errorf("release should carry no PR-only fields: %+v", items[1])
	}
	if atomic.LoadInt32(&pullCalls) != 1 {
		t.Errorf("pulls called %d times", pullCalls)
	}
	if atomic.LoadInt32(&filesCalls) != 2 {
		t.Errorf("files called %d times (want paginate)", filesCalls)
	}
}

// ---------------------------------------------------------------------------
// mergedPRs filter and pagination
// ---------------------------------------------------------------------------

func TestList_PRLabelFilter(t *testing.T) {
	since := mustTime(t, "2026-01-01T00:00:00Z")
	mergedAt := "2026-02-01T00:00:00Z"
	prs := []pullStub{
		{Number: 1, Title: "a", UpdatedAt: "2026-02-02T00:00:00Z", MergedAt: &mergedAt, Labels: []map[string]any{{"name": "docs"}}},
		{Number: 2, Title: "b", UpdatedAt: "2026-02-02T00:00:00Z", MergedAt: &mergedAt, Labels: []map[string]any{{"name": "Bug"}}},
	}
	body, _ := json.Marshal(prs)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, string(body)) })
	mux.HandleFunc("/repos/upstream/widgets/pulls/2/files", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })

	s := NewGitHubSource(newTestClient(t, mux), "fork-org", "fork-repo", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets",
		Sources:  []string{config.UpstreamSourcePRs},
		PRLabels: []string{"bug"},
	})
	items, err := s.List(context.Background(), since)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Ref != "upstream#2" {
		t.Fatalf("items = %+v, want only PR #2 (bug label)", items)
	}
}

func TestList_PRPaginationAndAllOlderShortcut(t *testing.T) {
	since := mustTime(t, "2026-01-01T00:00:00Z")
	mergedAt := "2026-02-01T00:00:00Z"
	oldMerged := "2025-06-01T00:00:00Z"
	page1 := []pullStub{{Number: 1, Title: "p1-1", UpdatedAt: "2026-02-05T00:00:00Z", MergedAt: &mergedAt}}
	page2 := []pullStub{{Number: 2, Title: "p2-1", UpdatedAt: "2025-06-02T00:00:00Z", MergedAt: &oldMerged}}
	// Third page would be fetched only if the allOlder shortcut failed;
	// the handler notices and fails the test if that happens.
	b1, _ := json.Marshal(page1)
	b2, _ := json.Marshal(page2)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/upstream/widgets/pulls?page=2>; rel="next"`)
			writeJSON(w, string(b1))
		case "2":
			w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/upstream/widgets/pulls?page=3>; rel="next"`)
			writeJSON(w, string(b2))
		default:
			t.Errorf("unexpected PR page fetch: %q", r.URL.Query().Get("page"))
			writeJSON(w, `[]`)
		}
	})
	mux.HandleFunc("/repos/upstream/widgets/pulls/1/files", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })

	s := NewGitHubSource(newTestClient(t, mux), "fork-org", "fork-repo", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets",
		Sources:  []string{config.UpstreamSourcePRs},
	})
	items, err := s.List(context.Background(), since)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Ref != "upstream#1" {
		t.Fatalf("items = %+v", items)
	}
}

func TestList_PRMergedAtBoundary(t *testing.T) {
	// merged_at == since must be excluded; the API's semantic is "after".
	since := mustTime(t, "2026-02-01T00:00:00Z")
	same := "2026-02-01T00:00:00Z"
	prs := []pullStub{{Number: 1, Title: "edge", UpdatedAt: "2026-02-01T00:00:00Z", MergedAt: &same}}
	body, _ := json.Marshal(prs)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, string(body)) })
	s := NewGitHubSource(newTestClient(t, mux), "f", "r", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets", Sources: []string{config.UpstreamSourcePRs},
	})
	items, err := s.List(context.Background(), since)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want empty on boundary", items)
	}
}

func TestList_PRListError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	s := explicitSource(t, mux)
	if _, err := s.List(context.Background(), time.Time{}); err == nil {
		t.Fatal("403 did not surface")
	}
}

func TestList_PRFilesError(t *testing.T) {
	mergedAt := "2026-02-01T00:00:00Z"
	prs := []pullStub{{Number: 1, Title: "p", UpdatedAt: "2026-02-02T00:00:00Z", MergedAt: &mergedAt}}
	body, _ := json.Marshal(prs)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, string(body)) })
	mux.HandleFunc("/repos/upstream/widgets/pulls/1/files", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	s := NewGitHubSource(newTestClient(t, mux), "f", "r", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets", Sources: []string{config.UpstreamSourcePRs},
	})
	_, err := s.List(context.Background(), mustTime(t, "2026-01-01T00:00:00Z"))
	if err == nil || !strings.Contains(err.Error(), "list files") {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// releases filter
// ---------------------------------------------------------------------------

func TestList_ReleasesOnly(t *testing.T) {
	// With Sources = {releases}, the PR endpoint must not be touched. The
	// handler fails the test if it is.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("PR endpoint called with releases-only cfg")
	})
	mux.HandleFunc("/repos/upstream/widgets/releases", func(w http.ResponseWriter, _ *http.Request) {
		rels := []releaseStub{{TagName: "v3.0.0", PublishedAt: pStr("2026-04-01T00:00:00Z")}}
		b, _ := json.Marshal(rels)
		writeJSON(w, string(b))
	})
	s := NewGitHubSource(newTestClient(t, mux), "f", "r", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets", Sources: []string{config.UpstreamSourceReleases},
	})
	items, err := s.List(context.Background(), mustTime(t, "2026-01-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Kind != KindRelease || items[0].Title != "v3.0.0" {
		t.Fatalf("items = %+v", items)
	}
}

func TestList_ReleasesPagination(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })
	mux.HandleFunc("/repos/upstream/widgets/releases", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			rels := []releaseStub{{TagName: "v5", PublishedAt: pStr("2026-05-01T00:00:00Z")}}
			b, _ := json.Marshal(rels)
			writeJSON(w, string(b))
			return
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/upstream/widgets/releases?page=2>; rel="next"`)
		rels := []releaseStub{{TagName: "v4", PublishedAt: pStr("2026-04-01T00:00:00Z")}}
		b, _ := json.Marshal(rels)
		writeJSON(w, string(b))
	})
	s := explicitSource(t, mux)
	items, err := s.List(context.Background(), mustTime(t, "2026-01-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 || items[0].Ref != "release:v4" || items[1].Ref != "release:v5" {
		t.Fatalf("items = %+v, want v4 then v5 oldest-first", items)
	}
}

func TestList_ReleasesError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })
	mux.HandleFunc("/repos/upstream/widgets/releases", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	})
	s := explicitSource(t, mux)
	if _, err := s.List(context.Background(), time.Time{}); err == nil {
		t.Fatal("500 did not surface")
	}
}

func TestList_SortsOldestFirstAcrossKinds(t *testing.T) {
	since := mustTime(t, "2026-01-01T00:00:00Z")
	m1 := "2026-05-01T00:00:00Z"
	m2 := "2026-02-01T00:00:00Z"
	prs := []pullStub{
		{Number: 1, Title: "late pr", UpdatedAt: "2026-05-02T00:00:00Z", MergedAt: &m1},
		{Number: 2, Title: "early pr", UpdatedAt: "2026-02-02T00:00:00Z", MergedAt: &m2},
	}
	prBody, _ := json.Marshal(prs)
	rels := []releaseStub{
		{TagName: "mid", PublishedAt: pStr("2026-03-15T00:00:00Z")},
		{TagName: "latest", PublishedAt: pStr("2026-06-01T00:00:00Z")},
	}
	relBody, _ := json.Marshal(rels)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, string(prBody)) })
	mux.HandleFunc("/repos/upstream/widgets/pulls/1/files", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })
	mux.HandleFunc("/repos/upstream/widgets/pulls/2/files", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })
	mux.HandleFunc("/repos/upstream/widgets/releases", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, string(relBody)) })

	items, err := explicitSource(t, mux).List(context.Background(), since)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4", len(items))
	}
	gotOrder := []string{items[0].Ref, items[1].Ref, items[2].Ref, items[3].Ref}
	want := []string{"upstream#2", "release:mid", "upstream#1", "release:latest"}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("order = %v, want %v", gotOrder, want)
		}
	}
}

// ---------------------------------------------------------------------------
// truncation and the release early stop
// ---------------------------------------------------------------------------

// pagedHandler serves body for every page and always links a next page,
// counting the calls.
func pagedHandler(path, body string, calls *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(calls, 1)
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, path, n+1))
		writeJSON(w, body)
	}
}

func TestList_PRTruncatedAtPageCap(t *testing.T) {
	since := mustTime(t, "2026-01-01T00:00:00Z")
	mergedAt := "2026-02-01T00:00:00Z"
	body, _ := json.Marshal([]pullStub{{Number: 1, Title: "new", UpdatedAt: "2026-02-02T00:00:00Z", MergedAt: &mergedAt}})
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", pagedHandler("/repos/upstream/widgets/pulls", string(body), &calls))
	mux.HandleFunc("/repos/upstream/widgets/pulls/1/files", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("touched files fetched for a truncated listing")
		writeJSON(w, `[]`)
	})
	s := NewGitHubSource(newTestClient(t, mux), "f", "r", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets", Sources: []string{config.UpstreamSourcePRs},
	})
	items, err := s.List(context.Background(), since)
	var trunc *TruncatedError
	if !errors.As(err, &trunc) || len(items) != 0 {
		t.Fatalf("items/err = %v/%v, want none and a *TruncatedError", items, err)
	}
	if trunc.Kind != KindPR || trunc.Limit != maxPRPages*githubPerPage || !trunc.Since.Equal(since) {
		t.Errorf("TruncatedError = %+v", trunc)
	}
	if got := trunc.Window(); !strings.Contains(got, "more than 1000 pull requests updated since 2026-01-01") {
		t.Errorf("Window = %q", got)
	}
	if atomic.LoadInt32(&calls) != maxPRPages {
		t.Errorf("pulls called %d times, want %d", calls, maxPRPages)
	}
}

func TestList_PRLastPageAtCapIsNotTruncated(t *testing.T) {
	since := mustTime(t, "2026-01-01T00:00:00Z")
	mergedAt := "2026-02-01T00:00:00Z"
	body, _ := json.Marshal([]pullStub{{Number: 1, Title: "new", UpdatedAt: "2026-02-02T00:00:00Z", MergedAt: &mergedAt}})
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		if n := atomic.AddInt32(&calls, 1); n < maxPRPages {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/upstream/widgets/pulls?page=%d>; rel="next"`, r.Host, n+1))
		}
		writeJSON(w, string(body))
	})
	mux.HandleFunc("/repos/upstream/widgets/pulls/1/files", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `[]`) })
	s := NewGitHubSource(newTestClient(t, mux), "f", "r", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets", Sources: []string{config.UpstreamSourcePRs},
	})
	items, err := s.List(context.Background(), since)
	if err != nil || len(items) != maxPRPages {
		t.Fatalf("items/err = %d/%v, want %d items and no error", len(items), err, maxPRPages)
	}
}

func TestList_ReleasesTruncatedAtPageCap(t *testing.T) {
	since := mustTime(t, "2026-01-01T00:00:00Z")
	body, _ := json.Marshal([]releaseStub{{TagName: "v9", PublishedAt: pStr("2026-04-01T00:00:00Z")}})
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/releases", pagedHandler("/repos/upstream/widgets/releases", string(body), &calls))
	s := NewGitHubSource(newTestClient(t, mux), "f", "r", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets", Sources: []string{config.UpstreamSourceReleases},
	})
	items, err := s.List(context.Background(), since)
	var trunc *TruncatedError
	if !errors.As(err, &trunc) || len(items) != 0 {
		t.Fatalf("items/err = %v/%v, want none and a *TruncatedError", items, err)
	}
	if trunc.Kind != KindRelease || trunc.Limit != maxReleasePages*githubPerPage {
		t.Errorf("TruncatedError = %+v", trunc)
	}
	if got := trunc.Window(); !strings.Contains(got, "more than 500 releases") {
		t.Errorf("Window = %q", got)
	}
	if atomic.LoadInt32(&calls) != maxReleasePages {
		t.Errorf("releases called %d times, want %d", calls, maxReleasePages)
	}
}

func TestList_ReleasesAllOlderShortcut(t *testing.T) {
	since := mustTime(t, "2026-01-01T00:00:00Z")
	b1, _ := json.Marshal([]releaseStub{{TagName: "v2", PublishedAt: pStr("2026-02-01T00:00:00Z"), CreatedAt: pStr("2026-02-01T00:00:00Z")}})
	b2, _ := json.Marshal([]releaseStub{
		{TagName: "v1", PublishedAt: pStr("2025-06-01T00:00:00Z"), CreatedAt: pStr("2025-06-01T00:00:00Z")},
		{TagName: "v1-draft", Draft: true, CreatedAt: pStr("2025-05-01T00:00:00Z")},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets/releases", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/upstream/widgets/releases?page=2>; rel="next"`)
			writeJSON(w, string(b1))
		case "2":
			w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/upstream/widgets/releases?page=3>; rel="next"`)
			writeJSON(w, string(b2))
		default:
			t.Errorf("unexpected release page fetch: %q", r.URL.Query().Get("page"))
			writeJSON(w, `[]`)
		}
	})
	s := NewGitHubSource(newTestClient(t, mux), "f", "r", config.UpstreamWatchRepo{
		Upstream: "upstream/widgets", Sources: []string{config.UpstreamSourceReleases},
	})
	items, err := s.List(context.Background(), since)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Ref != "release:v2" {
		t.Fatalf("items = %+v, want only v2", items)
	}
}

// ---------------------------------------------------------------------------
// ForkPoint
// ---------------------------------------------------------------------------

func forkPointMux(t *testing.T, compare http.HandlerFunc) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"default_branch":"main"}`)
	})
	mux.HandleFunc("/repos/fork-org/fork-repo", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"default_branch":"trunk"}`)
	})
	mux.HandleFunc("/repos/upstream/widgets/compare/", func(w http.ResponseWriter, r *http.Request) {
		if want := "/repos/upstream/widgets/compare/main...fork-org:trunk"; r.URL.Path != want {
			t.Errorf("compare path = %q, want %q", r.URL.Path, want)
		}
		compare(w, r)
	})
	return mux
}

func TestForkPoint(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		status  int
		want    time.Time
		wantErr string
	}{
		{
			name: "committer date of the merge base",
			body: `{"merge_base_commit":{"sha":"abc","commit":{"committer":{"date":"2026-10-01T12:00:00Z"},"author":{"date":"2026-09-30T00:00:00Z"}}}}`,
			want: mustTime(t, "2026-10-01T12:00:00Z"),
		},
		{
			name: "author date when no committer date",
			body: `{"merge_base_commit":{"sha":"abc","commit":{"author":{"date":"2026-09-30T00:00:00Z"}}}}`,
			want: mustTime(t, "2026-09-30T00:00:00Z"),
		},
		{name: "no merge base", body: `{}`, wantErr: "no merge base"},
		{name: "no commit date", body: `{"merge_base_commit":{"sha":"abc","commit":{}}}`, wantErr: "no commit date"},
		{name: "compare fails", status: http.StatusNotFound, wantErr: "compare upstream/widgets main...fork-org:trunk"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := forkPointMux(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != 0 {
					http.Error(w, "not found", tc.status)
					return
				}
				writeJSON(w, tc.body)
			})
			at, err := explicitSource(t, mux).ForkPoint(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !at.IsZero() {
					t.Fatalf("at/err = %v/%v, want zero and an error mentioning %q", at, err, tc.wantErr)
				}
				return
			}
			if err != nil || !at.Equal(tc.want) {
				t.Fatalf("at/err = %v/%v, want %v", at, err, tc.want)
			}
		})
	}
}

func TestForkPoint_DefaultBranchError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/upstream/widgets", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	_, err := explicitSource(t, mux).ForkPoint(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read default branch of upstream/widgets") {
		t.Fatalf("err = %v", err)
	}
}
