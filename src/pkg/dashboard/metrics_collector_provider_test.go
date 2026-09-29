package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// adoptersClient serves an ADOPTERS.md with n adopter rows.
func adoptersClient(t *testing.T, n int) *ghpkg.Client {
	t.Helper()
	body := "| Organization | Contact |\n|---|---|\n"
	for i := 0; i < n; i++ {
		body += "| Org | x |\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"encoding": "base64",
			"content":  encodeBase64ForTest(body),
		})
	}))
	t.Cleanup(srv.Close)
	return ghpkg.NewClientForTest(srv.URL, "myorg", []string{"repo1"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// #9621: the metrics collector must read the hive's CURRENT GitHub client on
// every collect, so a client first delivered over the heartbeat (App-less
// boot) or rebuilt after an App credential change is used without a restart.
func TestMetricsCollector_ProviderFollowsClientSwap(t *testing.T) {
	var cur atomic.Pointer[ghpkg.Client]
	mc := &MetricsCollector{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics: make(map[string]any),
		org:     "myorg",
		repo:    "repo1",
	}
	mc.SetGitHubClientProvider(cur.Load)

	if got := mc.countAdopters(context.Background(), "myorg", "repo1"); got != 0 {
		t.Fatalf("countAdopters with no client yet = %d, want 0", got)
	}
	if open, merged := mc.countOutreachPRs(context.Background()); open != 0 || merged != 0 {
		t.Fatalf("countOutreachPRs with no client yet = %d,%d, want 0,0", open, merged)
	}

	cur.Store(adoptersClient(t, 2))
	if got := mc.countAdopters(context.Background(), "myorg", "repo1"); got != 2 {
		t.Fatalf("countAdopters after first client = %d, want 2", got)
	}
	cur.Store(adoptersClient(t, 5))
	if got := mc.countAdopters(context.Background(), "myorg", "repo1"); got != 5 {
		t.Fatalf("countAdopters after rebuild = %d, want 5 (collector kept the old client)", got)
	}
}

func TestMetricsCollector_ProviderOverridesConstructorClient(t *testing.T) {
	boot := adoptersClient(t, 1)
	rebuilt := adoptersClient(t, 3)
	mc := &MetricsCollector{ghClient: boot, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), metrics: make(map[string]any)}
	if mc.client() != boot {
		t.Fatal("without a provider the constructor client must be used")
	}
	mc.SetGitHubClientProvider(func() *ghpkg.Client { return rebuilt })
	if mc.client() != rebuilt {
		t.Fatal("provider did not override the captured constructor client")
	}
	if got := mc.countACMM(context.Background(), "myorg", "repo1"); got != 0 {
		// The adopters body has no BADGE_PARTICIPANTS block; the point is
		// that the call went through the provider's client without panicking.
		t.Fatalf("countACMM = %d, want 0", got)
	}
	var nilMC *MetricsCollector
	nilMC.SetGitHubClientProvider(func() *ghpkg.Client { return rebuilt }) // must not panic
}

func TestMetricsCollector_NilProviderClientSkipsGitHubReads(t *testing.T) {
	mc := &MetricsCollector{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics:  make(map[string]any),
		org:      "myorg",
		repo:     "repo1",
		badgeURL: coverageBadgeRepoScheme + "main/coverage.json",
	}
	mc.SetGitHubClientProvider(func() *ghpkg.Client { return nil })
	mc.collectMTTR(context.Background())
	mc.collectPRIssueCounts(context.Background())
	if mc.GetMTTR() != nil || mc.GetPRIssueCounts() != nil {
		t.Fatal("collect with no client recorded results")
	}
	if got := mc.countACMM(context.Background(), "myorg", "repo1"); got != 0 {
		t.Fatalf("countACMM with no client = %d, want 0", got)
	}
	if _, ok := mc.fetchCoverageBadge(context.Background()); ok {
		t.Fatal("repo:// coverage badge read succeeded with no client")
	}
	if out := mc.collectOutreach(context.Background()); out["stars"] != 0 {
		t.Fatalf("collectOutreach with no client = %v, want zero stars", out["stars"])
	}
}
