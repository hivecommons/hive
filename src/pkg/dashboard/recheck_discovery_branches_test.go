package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestCollectRecheckDiscoverySkipsWhenUnconfigured(t *testing.T) {
	ctx := context.Background()
	var nilServer *Server
	if items, failures := nilServer.collectRecheckDiscovery(ctx, Campaign{}, time.Time{}); items != nil || failures != nil {
		t.Fatalf("nil server = %#v / %#v, want nil, nil", items, failures)
	}
	noDeps := &Server{}
	if items, failures := noDeps.collectRecheckDiscovery(ctx, Campaign{}, time.Time{}); items != nil || failures != nil {
		t.Fatalf("nil deps = %#v / %#v, want nil, nil", items, failures)
	}
	noConfig := &Server{deps: &Dependencies{}}
	if items, failures := noConfig.collectRecheckDiscovery(ctx, Campaign{}, time.Time{}); items != nil || failures != nil {
		t.Fatalf("nil config = %#v / %#v, want nil, nil", items, failures)
	}

	cfg := &config.Config{}
	cfg.Runs.Spektacular.Recheck.Discovery.Enabled = true
	disabledSources := &Server{deps: &Dependencies{Config: cfg}}
	if items, failures := disabledSources.collectRecheckDiscovery(ctx, Campaign{}, time.Time{}); items != nil || failures != nil {
		t.Fatalf("enabled without sources = %#v / %#v, want nil, nil", items, failures)
	}

	cfg2 := &config.Config{}
	cfg2.Runs.Spektacular.Recheck.Discovery.Sources = []config.SpektacularRecheckDiscoverySource{{Kind: "standards_feed", Name: "x", URLOrRepo: "https://example.com/feed"}}
	disabledFlag := &Server{deps: &Dependencies{Config: cfg2}}
	if items, failures := disabledFlag.collectRecheckDiscovery(ctx, Campaign{}, time.Time{}); items != nil || failures != nil {
		t.Fatalf("sources without enabled = %#v / %#v, want nil, nil", items, failures)
	}
}

func TestCollectRecheckDiscoveryRunsConfiguredSources(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<rss><channel><item><title>signal</title><link>https://example.com/s</link><pubDate>Mon, 05 Oct 2026 12:00:00 +0000</pubDate><description>d</description></item></channel></rss>`))
	}))
	defer feed.Close()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/releases" {
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"name":"v9","html_url":"https://github.com/owner/repo/releases/tag/v9","published_at":"2026-10-06T00:00:00Z","body":"b"}]`))
	}))
	defer ghSrv.Close()

	cfg := &config.Config{}
	disc := &cfg.Runs.Spektacular.Recheck.Discovery
	disc.Enabled = true
	disc.Timeout = 5 * time.Second
	disc.Sources = []config.SpektacularRecheckDiscoverySource{
		{Kind: "standards_feed", Name: "feed", URLOrRepo: feed.URL},
		{Kind: "upstream_release", Name: "up", URLOrRepo: "owner/repo"},
		{Kind: "unknown_kind", Name: "weird", URLOrRepo: "x"},
	}
	s := &Server{deps: &Dependencies{
		Config:   cfg,
		GHClient: github.NewClientForTest(ghSrv.URL, "owner", []string{"repo"}, slog.Default()),
	}}
	items, failures := s.collectRecheckDiscovery(context.Background(), Campaign{}, time.Time{})
	if len(items) != 2 {
		t.Fatalf("items = %#v, want feed item and release", items)
	}
	if items[0].Kind != "standards_feed" || items[1].Kind != "upstream_release" {
		t.Fatalf("kinds = %q/%q, want source order preserved", items[0].Kind, items[1].Kind)
	}
	if len(failures) != 1 || failures[0].Name != "weird" || failures[0].Reason != "bad_kind" {
		t.Fatalf("failures = %#v, want bad_kind for unknown source", failures)
	}
}

func TestRecheckDiscoveryCollectSourceDispatch(t *testing.T) {
	c := recheckDiscoveryCollector{cfg: config.SpektacularConfig{}}
	ctx := context.Background()
	cases := map[string]string{
		"upstream_release": "github_unavailable",
		" repo_activity ":  "github_unavailable",
		"standards_feed":   "bad_feed_url",
		"":                 "bad_kind",
		"nope":             "bad_kind",
	}
	for kind, want := range cases {
		_, err := c.collectSource(ctx, config.SpektacularRecheckDiscoverySource{Kind: kind, URLOrRepo: "owner/repo"}, Campaign{}, time.Time{})
		if err == nil || err.Error() != want {
			t.Errorf("collectSource(%q) err = %v, want %s", kind, err, want)
		}
	}
}

func TestRecheckDiscoveryCollectStopsAtMaxTotal(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`<rss><channel><item><title>a</title></item><item><title>b</title></item></channel></rss>`))
	}))
	defer srv.Close()
	cfg := config.SpektacularConfig{}
	cfg.Recheck.Discovery.MaxTotalItems = 1
	cfg.Recheck.Discovery.Sources = []config.SpektacularRecheckDiscoverySource{
		{Kind: "standards_feed", Name: "one", URLOrRepo: srv.URL},
		{Kind: "standards_feed", Name: "two", URLOrRepo: srv.URL},
	}
	c := recheckDiscoveryCollector{cfg: cfg, client: srv.Client()}
	items, failures := c.collect(context.Background(), Campaign{}, time.Time{})
	if len(items) != 1 || items[0].Title != "a" {
		t.Fatalf("items = %#v, want first item only (per-source overflow trimmed)", items)
	}
	if len(failures) != 0 {
		t.Fatalf("failures = %#v, want none", failures)
	}
	if calls != 1 {
		t.Fatalf("feed fetched %d times, want 1: second source must be skipped once the total cap is reached", calls)
	}
}

func TestRecheckDiscoveryLandscapeReadsDocsAndCollectsReleases(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if _, err := readLandscape(); err == nil || err.Error() != "landscape_unavailable" {
		t.Fatalf("readLandscape without docs err = %v, want landscape_unavailable", err)
	}
	c := recheckDiscoveryCollector{cfg: config.SpektacularConfig{}}
	if _, err := c.landscape(context.Background(), config.SpektacularRecheckDiscoverySource{Kind: "landscape"}, Campaign{}, time.Time{}); err == nil {
		t.Fatal("landscape without docs: want error")
	}

	// The src/ fallback location is honoured when docs/landscape.md is absent.
	if err := os.MkdirAll(filepath.Join(dir, "src", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	fallback := "see https://github.com/fb/only"
	if err := os.WriteFile(filepath.Join(dir, "src", "docs", "landscape.md"), []byte(fallback), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := readLandscape()
	if err != nil || string(raw) != fallback {
		t.Fatalf("readLandscape fallback = %q, %v", raw, err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	markdown := "- https://github.com/alpha/one.git\n- https://github.com/beta/two\n- https://github.com/gamma/missing\n"
	if err := os.WriteFile(filepath.Join(dir, "docs", "landscape.md"), []byte(markdown), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/alpha/one/releases":
			_, _ = w.Write([]byte(`[{"name":"one-old","html_url":"https://github.com/alpha/one/releases/tag/v1","published_at":"2026-10-01T00:00:00Z"}]`))
		case "/repos/beta/two/releases":
			_, _ = w.Write([]byte(`[{"name":"two-new","html_url":"https://github.com/beta/two/releases/tag/v2","published_at":"2026-10-07T00:00:00Z"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := gh.NewClient(srv.Client())
	client.BaseURL = mustURL(t, srv.URL+"/")
	c = recheckDiscoveryCollector{cfg: config.SpektacularConfig{}, gh: client}

	items, err := c.landscape(context.Background(), config.SpektacularRecheckDiscoverySource{Kind: "landscape", Name: "cncf"}, Campaign{}, time.Time{})
	if err != nil {
		t.Fatalf("landscape: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %#v, want one release per reachable repo (missing repo skipped)", items)
	}
	if items[0].Title != "two-new" || items[1].Title != "one-old" {
		t.Fatalf("order = %q,%q, want newest first", items[0].Title, items[1].Title)
	}
	for _, item := range items {
		if item.Source != "cncf" || item.Kind != "landscape" {
			t.Fatalf("item = %#v, want source/kind rewritten to the landscape source", item)
		}
	}

	// Per-source cap returns early with the collected subset sorted.
	capped, err := c.landscape(context.Background(), config.SpektacularRecheckDiscoverySource{Kind: "landscape", MaxItems: 1}, Campaign{}, time.Time{})
	if err != nil || len(capped) != 1 || capped[0].Title != "one-old" {
		t.Fatalf("capped = %#v, %v; want first repo's release only", capped, err)
	}
	if capped[0].Kind != "landscape" {
		t.Fatalf("capped item = %#v, want landscape kind", capped[0])
	}
}

func TestRecheckDiscoveryGitHubErrorPaths(t *testing.T) {
	ctx := context.Background()
	noGH := recheckDiscoveryCollector{cfg: config.SpektacularConfig{}}
	if _, err := noGH.githubReleases(ctx, config.SpektacularRecheckDiscoverySource{URLOrRepo: "owner/repo"}, time.Time{}); err == nil || err.Error() != "github_unavailable" {
		t.Fatalf("githubReleases without client err = %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/broken/releases":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/repos/owner/repo/releases":
			_, _ = w.Write([]byte(`[{"name":"draft","draft":true,"published_at":"2026-10-07T00:00:00Z"}]`))
		case "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[
	{"title":"open pr","html_url":"https://github.com/owner/repo/pull/1"},
	{"title":"old merge","html_url":"https://github.com/owner/repo/pull/2","merged_at":"2026-01-01T00:00:00Z"},
	{"title":"new merge","html_url":"https://github.com/owner/repo/pull/3","merged_at":"2026-10-06T00:00:00Z"}
	]`))
		case "/repos/owner/nopulls/releases":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/owner/nopulls/pulls":
			http.Error(w, "boom", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := gh.NewClient(srv.Client())
	client.BaseURL = mustURL(t, srv.URL+"/")
	c := recheckDiscoveryCollector{cfg: config.SpektacularConfig{}, gh: client}

	if _, err := c.githubReleases(ctx, config.SpektacularRecheckDiscoverySource{URLOrRepo: "just-a-name"}, time.Time{}); err == nil || err.Error() != "bad_repo" {
		t.Fatalf("bad slug err = %v, want bad_repo", err)
	}
	if _, err := c.githubReleases(ctx, config.SpektacularRecheckDiscoverySource{URLOrRepo: "owner/broken"}, time.Time{}); err == nil {
		t.Fatal("API 500: want error")
	}
	if _, err := c.githubActivity(ctx, config.SpektacularRecheckDiscoverySource{URLOrRepo: "owner/broken"}, time.Time{}); err == nil {
		t.Fatal("activity with failing releases: want error")
	}
	if _, err := c.githubActivity(ctx, config.SpektacularRecheckDiscoverySource{URLOrRepo: "owner/nopulls"}, time.Time{}); err == nil {
		t.Fatal("activity with failing pulls: want error")
	}

	items, err := c.githubActivity(ctx, config.SpektacularRecheckDiscoverySource{Name: "act", URLOrRepo: "https://github.com/owner/repo.git"}, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("githubActivity: %v", err)
	}
	if len(items) != 1 || items[0].Title != "new merge" {
		t.Fatalf("items = %#v, want only the merged-after-since PR (draft release, open and old PRs dropped)", items)
	}
}

func TestRecheckDiscoveryFeedItemsErrorPaths(t *testing.T) {
	ctx := context.Background()
	c := recheckDiscoveryCollector{cfg: config.SpektacularConfig{}}
	for _, bad := range []string{"", "not a url", "/relative/only", "mailto:x"} {
		if _, err := c.feedItems(ctx, config.SpektacularRecheckDiscoverySource{URLOrRepo: bad}, time.Time{}); err == nil || err.Error() != "bad_feed_url" {
			t.Errorf("feedItems(%q) err = %v, want bad_feed_url", bad, err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusGone)
			return
		}
		_, _ = w.Write([]byte(`<feed><entry><title>undated</title><link href="https://example.com/u"/></entry></feed>`))
	}))
	defer srv.Close()
	c.client = srv.Client()
	if _, err := c.feedItems(ctx, config.SpektacularRecheckDiscoverySource{URLOrRepo: srv.URL + "/gone"}, time.Time{}); err == nil || err.Error() != "http_410" {
		t.Fatalf("non-2xx err = %v, want http_410", err)
	}

	// Entries without a timestamp are kept even when a since bound is set.
	items, err := c.feedItems(ctx, config.SpektacularRecheckDiscoverySource{Name: "f", URLOrRepo: srv.URL + "/ok"}, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil || len(items) != 1 || items[0].Title != "undated" || !items[0].PublishedAt.IsZero() {
		t.Fatalf("undated items = %#v, %v", items, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.feedItems(cancelled, config.SpektacularRecheckDiscoverySource{URLOrRepo: srv.URL + "/ok"}, time.Time{}); err == nil {
		t.Fatal("cancelled context: want transport error")
	}
}

func TestParseFeedEvidenceAtomPrefersSummaryThenContent(t *testing.T) {
	items := parseFeedEvidence("src", []byte(`<feed>
<entry><title> A </title><updated>2026-10-05T12:00:00Z</updated><content>content only</content></entry>
<entry><title>B</title><summary>sum</summary><content>content</content><link href="https://example.com/b"/><link href="https://example.com/b2"/></entry>
</feed>`))
	if len(items) != 2 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].Title != "A" || items[0].Summary != "content only" || items[0].URL != "" || items[0].PublishedAt.IsZero() {
		t.Fatalf("entry A = %#v", items[0])
	}
	if items[1].Summary != "sum" || items[1].URL != "https://example.com/b" || !items[1].PublishedAt.IsZero() {
		t.Fatalf("entry B = %#v", items[1])
	}
	if got := parseFeedEvidence("src", []byte(`not xml at all`)); len(got) != 0 {
		t.Fatalf("garbage body = %#v, want empty", got)
	}
	if got := parseFeedEvidence("src", []byte(`<feed></feed>`)); len(got) != 0 {
		t.Fatalf("empty atom = %#v, want empty", got)
	}
}

func TestParseFeedTimeLayouts(t *testing.T) {
	want := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, in := range []string{
		" 2026-10-05T12:00:00Z ",
		"2026-10-05T12:00:00.000Z",
		"Mon, 05 Oct 2026 12:00:00 +0000",
		"Mon, 05 Oct 2026 12:00:00 UTC",
	} {
		if got := parseFeedTime(in); !got.Equal(want) {
			t.Errorf("parseFeedTime(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "yesterday", "2026-10-05"} {
		if got := parseFeedTime(in); !got.IsZero() {
			t.Errorf("parseFeedTime(%q) = %v, want zero", in, got)
		}
	}
}

func TestSplitRepoSlugForms(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		ok          bool
	}{
		{"owner/repo", "owner", "repo", true},
		{"  owner / repo  ", "owner", "repo", true},
		{"https://github.com/owner/repo", "owner", "repo", true},
		{"https://github.com/owner/repo.git", "owner", "repo", true},
		{"https://github.com/owner/repo/releases/tag/v1", "owner", "repo", true},
		{"owner", "", "", false},
		{"/repo", "", "", false},
		{"owner/", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		owner, repo, ok := splitRepoSlug(tc.in)
		if ok != tc.ok || (ok && (owner != tc.owner || repo != tc.repo)) {
			t.Errorf("splitRepoSlug(%q) = %q,%q,%v; want %q,%q,%v", tc.in, owner, repo, ok, tc.owner, tc.repo, tc.ok)
		}
	}
}

func TestTruncateDiscoveryRunes(t *testing.T) {
	if got := truncateDiscoveryRunes("  héllo wörld  ", 0); got != "héllo wörld" {
		t.Fatalf("no cap = %q", got)
	}
	if got := truncateDiscoveryRunes("héllo", 10); got != "héllo" {
		t.Fatalf("under cap = %q", got)
	}
	if got := truncateDiscoveryRunes("héllo wörld", 5); got != "héllo" {
		t.Fatalf("rune cap = %q, want whole runes not bytes", got)
	}
}

func TestDiscoveryFailureReason(t *testing.T) {
	if got := discoveryFailureReason(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
	if got := discoveryFailureReason(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("deadline = %q", got)
	}
	if got := discoveryFailureReason(errors.New(" multi\nline\n")); got != "multi line" {
		t.Fatalf("multiline = %q", got)
	}
}

func TestSortEvidenceNewestFirstStable(t *testing.T) {
	ts := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	items := []knowledge.CampaignExternalEvidence{
		{Title: "a", PublishedAt: ts(1)},
		{Title: "b", PublishedAt: ts(3)},
		{Title: "c", PublishedAt: ts(3)},
		{Title: "d"},
	}
	sortEvidence(items)
	got := []string{items[0].Title, items[1].Title, items[2].Title, items[3].Title}
	if strings.Join(got, "") != "bcad" {
		t.Fatalf("order = %v, want b c a d (newest first, equal timestamps stable, zero last)", got)
	}
}
