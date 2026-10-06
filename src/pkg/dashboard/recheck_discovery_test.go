package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestRecheckDiscoveryFeedFiltersSinceAndCaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<feed>
<entry><title>old</title><updated>2026-01-01T00:00:00Z</updated><summary>old summary</summary><link href="https://example.com/old"/></entry>
<entry><title>new one</title><updated>2026-10-05T12:00:00Z</updated><summary>new summary</summary><link href="https://example.com/new1"/></entry>
<entry><title>new two</title><updated>2026-10-05T13:00:00Z</updated><summary>new summary</summary><link href="https://example.com/new2"/></entry>
</feed>`))
	}))
	defer srv.Close()

	collector := recheckDiscoveryCollector{cfg: config.SpektacularConfig{}, client: srv.Client()}
	items, err := collector.feedItems(context.Background(), config.SpektacularRecheckDiscoverySource{
		Kind: "standards_feed", Name: "standards", URLOrRepo: srv.URL, MaxItems: 1,
	}, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("feedItems: %v", err)
	}
	if len(items) != 1 || items[0].Title != "new one" || items[0].Kind != "standards_feed" {
		t.Fatalf("items = %#v, want first new item only", items)
	}
}

func TestRecheckDiscoveryCollectRecordsFailureNotFatal(t *testing.T) {
	cfg := config.SpektacularConfig{}
	cfg.Recheck.Discovery.MaxTotalItems = 1
	cfg.Recheck.Discovery.Sources = []config.SpektacularRecheckDiscoverySource{
		{Kind: "standards_feed", Name: "bad", URLOrRepo: "http://127.0.0.1:1/nope"},
		{Kind: "standards_feed", Name: "good", URLOrRepo: "https://example.com/feed.xml"},
	}

	collector := recheckDiscoveryCollector{
		cfg: cfg,
		client: recheckRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "127.0.0.1:1" {
				return nil, errors.New("dial blocked")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`<rss><channel><item><title>ok</title><link>https://example.com/ok</link><pubDate>Mon, 05 Oct 2026 12:00:00 +0000</pubDate><description>ok</description></item></channel></rss>`)),
				Header:     make(http.Header),
			}, nil
		}).Client(),
	}
	items, failures := collector.collect(context.Background(), Campaign{}, time.Time{})
	if len(items) != 1 || items[0].Title != "ok" {
		t.Fatalf("items = %#v, want successful source evidence", items)
	}
	if len(failures) != 1 || failures[0].Name != "bad" {
		t.Fatalf("failures = %#v, want bad source recorded", failures)
	}
}

func TestRecheckDiscoveryGitHubReleasesFiltersSincePrereleaseAndCaps(t *testing.T) {
	const releasesPath = "/repos/owner/repo/releases"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != releasesPath {
			t.Fatalf("path = %s, want %s", r.URL.Path, releasesPath)
		}
		_, _ = w.Write([]byte(`[
	{"name":"old","html_url":"https://github.com/owner/repo/releases/tag/v1","published_at":"2026-01-01T00:00:00Z","body":"old"},
	{"name":"pre","html_url":"https://github.com/owner/repo/releases/tag/v2-rc1","published_at":"2026-10-05T11:00:00Z","prerelease":true,"body":"pre"},
	{"name":"new","html_url":"https://github.com/owner/repo/releases/tag/v2","published_at":"2026-10-05T12:00:00Z","body":"new"}
	]`))
	}))
	defer srv.Close()
	client := gh.NewClient(srv.Client())
	client.BaseURL = mustURL(t, srv.URL+"/")
	collector := recheckDiscoveryCollector{cfg: config.SpektacularConfig{}, gh: client}
	items, err := collector.githubReleases(context.Background(), config.SpektacularRecheckDiscoverySource{
		Kind: "upstream_release", Name: "up", URLOrRepo: "owner/repo", MaxItems: 1,
	}, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("githubReleases: %v", err)
	}
	if len(items) != 1 || items[0].Title != "new" || items[0].Kind != "upstream_release" {
		t.Fatalf("items = %#v, want only non-prerelease new release", items)
	}
}

func TestRecheckDiscoveryRepoActivityIncludesMergedPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[{"title":"merged feature","html_url":"https://github.com/owner/repo/pull/7","merged_at":"2026-10-05T12:00:00Z","body":"body"}]`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	client := gh.NewClient(srv.Client())
	client.BaseURL = mustURL(t, srv.URL+"/")
	collector := recheckDiscoveryCollector{cfg: config.SpektacularConfig{}, gh: client}
	items, err := collector.githubActivity(context.Background(), config.SpektacularRecheckDiscoverySource{
		Kind: "repo_activity", Name: "activity", URLOrRepo: "owner/repo",
	}, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("githubActivity: %v", err)
	}
	if len(items) != 1 || items[0].Title != "merged feature" || items[0].Kind != "repo_activity" {
		t.Fatalf("items = %#v, want merged PR activity", items)
	}
}

func TestRecheckDiscoveryLandscapeExtractsGitHubRepos(t *testing.T) {
	repos := githubReposFromMarkdown(`[one](https://github.com/owner/repo) and [dup](https://github.com/owner/repo) plus https://github.com/other/project/releases`)
	if len(repos) != 2 || repos[0] != "owner/repo" || repos[1] != "other/project" {
		t.Fatalf("repos = %#v, want stable unique GitHub repos", repos)
	}
}

func TestRecheckSpecPromptTitleIncludesExternalEvidence(t *testing.T) {
	drift := campaignDriftFromArchive(knowledge.InceptionCampaignArchive{Drift: &knowledge.CampaignDrift{
		ExternalCount: 1,
		External: []knowledge.CampaignExternalEvidence{{
			Source:  "source",
			Kind:    "standards_feed",
			Title:   "signal",
			URL:     "https://example.com/signal",
			Summary: "summary",
		}},
	}})
	if drift == nil || drift.ExternalCount != 1 || !strings.Contains(drift.External[0].Summary, "summary") {
		t.Fatalf("drift = %#v, want external evidence shape", drift)
	}
}

type recheckRoundTripFunc func(*http.Request) (*http.Response, error)

func (f recheckRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func (f recheckRoundTripFunc) Client() *http.Client { return &http.Client{Transport: f} }

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	u, err := url.Parse(value)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", value, err)
	}
	return u
}
