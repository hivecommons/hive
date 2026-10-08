package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestMetricsCollector_NilClient(t *testing.T) {
	// NewMetricsCollector with nil client should not panic
	mc := &MetricsCollector{
		metrics: make(map[string]any),
	}

	// Get should return empty map
	result := mc.Get()
	if result == nil {
		t.Fatal("expected non-nil map")
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestMetricsCollector_CollectArchitect(t *testing.T) {
	mc := &MetricsCollector{
		metrics: make(map[string]any),
	}
	result := mc.collectArchitect()
	if result["prs"] != 0 {
		t.Errorf("prs = %v", result["prs"])
	}
	if result["closed"] != 0 {
		t.Errorf("closed = %v", result["closed"])
	}
}

func TestMetricsCollector_CollectOutreach_NilClient(t *testing.T) {
	mc := &MetricsCollector{
		metrics: make(map[string]any),
	}
	result := mc.collectOutreach(context.TODO())
	if result["stars"] != 0 {
		t.Errorf("stars = %v", result["stars"])
	}
}

func TestMetricsCollector_CollectCoverage_NoBadgeURL(t *testing.T) {
	mc := &MetricsCollector{
		metrics: make(map[string]any),
	}
	result := mc.collectCoverage(context.Background())
	if result["coverage"] != 0 {
		t.Errorf("coverage = %v", result["coverage"])
	}
	const expectedTarget = 91
	if result["coverageTarget"] != expectedTarget {
		t.Errorf("coverageTarget = %v", result["coverageTarget"])
	}
}

func TestMetricsCollector_CountOutreachPRs_NilClient(t *testing.T) {
	mc := &MetricsCollector{
		metrics: make(map[string]any),
	}
	open, merged := mc.countOutreachPRs(context.TODO())
	if open != 0 || merged != 0 {
		t.Errorf("expected 0,0 got %d,%d", open, merged)
	}
}

func TestMetricsCollector_CountACMM_WithMock(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Simulate a page.tsx file with BADGE_PARTICIPANTS
	pageContent := `
const BADGE_PARTICIPANTS = new Set([
  "ProjectA",
  "ProjectB",
  "ProjectC",
]);

export default function Page() {
  return <div>Leaderboard</div>;
}
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// go-github calls /repos/{owner}/{repo}/contents/{path}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":     "file",
			"encoding": "base64",
			"content":  encodeBase64ForTest(pageContent),
		})
	}))
	defer srv.Close()

	ghClient := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"docs"}, logger)
	mc := &MetricsCollector{
		ghClient: ghClient,
		org:      "myorg",
		repo:     "hive",
		logger:   logger,
		metrics:  make(map[string]any),
	}

	count := mc.countACMM(context.Background(), "myorg", "hive")
	if count != 3 {
		t.Errorf("countACMM = %d, want 3", count)
	}
}

func TestMetricsCollector_CountACMM_NoSetBlock(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Page with no BADGE_PARTICIPANTS
	pageContent := `export default function Page() { return <div/>; }`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":     "file",
			"encoding": "base64",
			"content":  encodeBase64ForTest(pageContent),
		})
	}))
	defer srv.Close()

	ghClient := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"docs"}, logger)
	mc := &MetricsCollector{
		ghClient: ghClient,
		org:      "myorg",
		repo:     "hive",
		logger:   logger,
		metrics:  make(map[string]any),
	}
	count := mc.countACMM(context.Background(), "myorg", "hive")
	if count != 0 {
		t.Errorf("countACMM = %d, want 0", count)
	}
}

func TestMetricsCollector_CountACMM_FetchError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()

	ghClient := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"docs"}, logger)
	mc := &MetricsCollector{
		ghClient: ghClient,
		org:      "myorg",
		repo:     "hive",
		logger:   logger,
		metrics:  make(map[string]any),
	}
	count := mc.countACMM(context.Background(), "myorg", "hive")
	if count != 0 {
		t.Errorf("countACMM = %d, want 0", count)
	}
}

func TestMetricsCollector_CountACMM_HiveCommonsUsesKubestellarDocs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	var gotOwner string
	pageContent := `
const BADGE_PARTICIPANTS = new Set([
  "ProjectA",
]);
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		if len(parts) > 0 {
			gotOwner = parts[0]
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":     "file",
			"encoding": "base64",
			"content":  encodeBase64ForTest(pageContent),
		})
	}))
	defer srv.Close()

	ghClient := ghpkg.NewClientForTest(srv.URL, "hivecommons", []string{"docs"}, logger)
	mc := &MetricsCollector{
		ghClient: ghClient,
		org:      "hivecommons",
		repo:     "hive",
		logger:   logger,
		metrics:  make(map[string]any),
	}
	if count := mc.countACMM(context.Background(), "hivecommons", "hive"); count != 1 {
		t.Fatalf("countACMM = %d, want 1", count)
	}
	if gotOwner != "kubestellar" {
		t.Fatalf("ACMM docs owner = %q, want kubestellar", gotOwner)
	}
}

func TestMetricsCollector_CountAdopters_WithMock(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	adoptersContent := `# ADOPTERS
| Organization | Description |
| --- | --- |
| Acme Corp | Production use |
| Globex Inc | Testing |
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":     "file",
			"encoding": "base64",
			"content":  encodeBase64ForTest(adoptersContent),
		})
	}))
	defer srv.Close()

	ghClient := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"repo1"}, logger)
	mc := &MetricsCollector{
		ghClient: ghClient,
		org:      "myorg",
		repo:     "repo1",
		logger:   logger,
		metrics:  make(map[string]any),
	}

	count := mc.countAdopters(context.Background(), "myorg", "repo1")
	if count != 2 {
		t.Errorf("countAdopters = %d, want 2", count)
	}
}

func TestMetricsCollector_CollectCoverage_ParsesBadgePct(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"message": "85%"})
	}))
	defer srv.Close()

	mc := &MetricsCollector{
		badgeURL: srv.URL,
		metrics:  make(map[string]any),
	}
	result := mc.collectCoverage(context.Background())
	if result["coverage"] != 85 {
		t.Errorf("coverage = %v, want 85", result["coverage"])
	}
}

func TestMetricsCollector_CollectCoverage_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer srv.Close()

	mc := &MetricsCollector{
		badgeURL: srv.URL,
		metrics:  make(map[string]any),
	}
	result := mc.collectCoverage(context.Background())
	if result["coverage"] != 0 {
		t.Errorf("coverage = %v, want 0", result["coverage"])
	}
}

func TestMetricsCollector_NewMetricsCollector(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMetricsCollector(nil, "org", "repo", "http://badge.io", "bot", "MyProject", logger)
	if mc == nil {
		t.Fatal("expected non-nil MetricsCollector")
	}
	if mc.org != "org" {
		t.Errorf("org = %q", mc.org)
	}
	if mc.projectName != "MyProject" {
		t.Errorf("projectName = %q", mc.projectName)
	}
}

func TestMetricsCollector_NewMetricsCollector_EmptyProjectName(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMetricsCollector(nil, "org", "repo", "", "", "", logger)
	if mc == nil {
		t.Fatal("expected non-nil MetricsCollector")
	}
}

func TestMetricsCollector_Get_PopulatedMetrics(t *testing.T) {
	mc := &MetricsCollector{
		metrics: map[string]any{
			"outreach":      map[string]any{"stars": 100},
			"ci-maintainer": map[string]any{"coverage": 90},
		},
	}
	result := mc.Get()
	if result["outreach"] == nil {
		t.Error("expected outreach in result")
	}
	outreach := result["outreach"].(map[string]any)
	if outreach["stars"] != 100 {
		t.Errorf("stars = %v", outreach["stars"])
	}
}

func TestMetricsCollector_Collect(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := &MetricsCollector{
		org:     "myorg",
		repo:    "repo1",
		logger:  logger,
		metrics: make(map[string]any),
	}
	mc.collect(context.Background())

	result := mc.Get()
	if result["outreach"] == nil {
		t.Error("expected outreach in collected metrics")
	}
	if result["ci-maintainer"] == nil {
		t.Error("expected ci-maintainer in collected metrics")
	}
	if result["architect"] == nil {
		t.Error("expected architect in collected metrics")
	}
}

func TestMetricsCollector_SaveAndLoadDisk(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := &MetricsCollector{
		logger:  logger,
		metrics: make(map[string]any),
	}

	testMetrics := map[string]any{
		"outreach": map[string]any{"stars": 100},
	}
	mc.saveToDisk(testMetrics)
	// saveToDisk writes to /data/metrics/ which may fail on macOS (read-only)
	// Just verify it doesn't panic
}

func TestMetricsCollector_Start_CancelledContext(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := NewMetricsCollector(nil, "org", "repo", "", "", "Project", logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Start should return quickly with cancelled context
	done := make(chan struct{})
	go func() {
		mc.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancelled")
	}
}

// encodeBase64ForTest is a helper for the test server to encode file contents.
func encodeBase64ForTest(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func TestMetricsCollector_CollectPRIssueCounts_NilClient(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := &MetricsCollector{
		repo:    "repo1",
		logger:  logger,
		metrics: make(map[string]any),
	}
	mc.collectPRIssueCounts(context.Background())
	if got := mc.GetPRIssueCounts(); got != nil {
		t.Errorf("GetPRIssueCounts() = %+v, want nil", got)
	}
}

func TestMetricsCollector_CollectPRIssueCounts_EmptyRepo(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := &MetricsCollector{
		logger:  logger,
		metrics: make(map[string]any),
	}
	mc.collectPRIssueCounts(context.Background())
	if got := mc.GetPRIssueCounts(); got != nil {
		t.Errorf("GetPRIssueCounts() = %+v, want nil", got)
	}
}

func TestMetricsCollector_CollectPRIssueCounts_WithMock(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "author:app/hive-bot") && !strings.Contains(q, "author:hive-bot[bot]") {
			t.Fatalf("query missing hive author qualifier: %q", q)
		}
		total := 0
		switch {
		case strings.Contains(q, "repo:myorg/repo1") && strings.Contains(q, "type:pr"):
			total = 8
		case strings.Contains(q, "repo:myorg/repo1") && strings.Contains(q, "type:issue"):
			total = 5
		case strings.Contains(q, "repo:myorg/repo2") && strings.Contains(q, "type:pr"):
			total = 13
		case strings.Contains(q, "repo:myorg/repo2") && strings.Contains(q, "type:issue"):
			total = 21
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := &MetricsCollector{
		ghClient: ghpkg.NewClientForTest(srv.URL, "myorg", []string{"repo1"}, logger),
		org:      "myorg",
		repo:     "repo1",
		repos:    []string{"repo1", "repo2"},
		aiAuthor: "hive-bot[bot]",
		logger:   logger,
		metrics:  make(map[string]any),
	}
	mc.collectPRIssueCounts(context.Background())

	got := mc.GetPRIssueCounts()
	if got == nil {
		t.Fatal("expected non-nil PR/issue counts")
	}
	if got.MergedPRs != 21 {
		t.Errorf("MergedPRs = %d, want 21", got.MergedPRs)
	}
	if got.ClosedIssues != 26 {
		t.Errorf("ClosedIssues = %d, want 26", got.ClosedIssues)
	}
}

func TestMetricsCollector_PRIssueCounts_SaveAndLoadDisk(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mc := &MetricsCollector{
		logger:  logger,
		metrics: make(map[string]any),
	}
	// savePRIssueCountsToDisk writes to /data/metrics/ which may fail in a
	// sandboxed test environment (read-only fs); just verify it doesn't panic.
	mc.savePRIssueCountsToDisk(&ghpkg.PRIssueCounts{MergedPRs: 3, ClosedIssues: 2, UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
	// loadPRIssueCountsFromDisk on a missing/inaccessible file should also not panic.
	mc.loadPRIssueCountsFromDisk()
}

// An SVG badge (octocov's default, shields.io) carries its number in a <text>
// element, so it is usable directly — no JSON endpoint needed.
func TestMetricsCollector_CollectCoverage_SVGBadge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		fmt.Fprint(w, octocovBadgeSVG)
	}))
	defer srv.Close()

	mc := &MetricsCollector{badgeURL: srv.URL, metrics: make(map[string]any), logger: covBLogger()}
	result := mc.collectCoverage(context.Background())
	if result["coverage"] != 98 {
		t.Errorf("coverage = %v, want 98 (98.5%% truncated)", result["coverage"])
	}
}

// repo://<ref>/<path> reads the badge from the hive's own primary repo through
// the GitHub client — the only route that works for a private repo, whose
// raw.githubusercontent.com URLs 404 without a token.
func TestMetricsCollector_CollectCoverage_RepoScheme(t *testing.T) {
	var gotPath, gotRef string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRef = r.URL.Path, r.URL.Query().Get("ref")
		svg := strings.ReplaceAll(octocovBadgeSVG, "98.5%", "85.0%")
		json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "encoding": "base64", "name": "coverage.svg", "path": "coverage.svg",
			"content": base64.StdEncoding.EncodeToString([]byte(svg)),
		})
	}))
	defer gh.Close()

	mc := &MetricsCollector{
		ghClient: ghpkg.NewClientForTest(gh.URL, "myorg", []string{"repo1"}, covBLogger()),
		org:      "myorg",
		repo:     "repo1",
		badgeURL: "repo://badges/coverage.svg",
		metrics:  make(map[string]any),
		logger:   covBLogger(),
	}
	result := mc.collectCoverage(context.Background())
	if result["coverage"] != 85 {
		t.Errorf("coverage = %v, want 85", result["coverage"])
	}
	if gotPath != "/repos/myorg/repo1/contents/coverage.svg" || gotRef != "badges" {
		t.Errorf("fetched %s?ref=%s, want /repos/myorg/repo1/contents/coverage.svg?ref=badges", gotPath, gotRef)
	}
}

// Without a GitHub client the repo:// form has nothing to read with and must
// report 0 rather than fall back to fetching "repo://..." over HTTP.
func TestMetricsCollector_CollectCoverage_RepoScheme_NoClient(t *testing.T) {
	mc := &MetricsCollector{badgeURL: "repo://badges/coverage.svg", metrics: make(map[string]any), logger: covBLogger()}
	if result := mc.collectCoverage(context.Background()); result["coverage"] != 0 {
		t.Errorf("coverage = %v, want 0", result["coverage"])
	}
	mc.badgeURL = "repo://no-path"
	if result := mc.collectCoverage(context.Background()); result["coverage"] != 0 {
		t.Errorf("malformed repo:// form: coverage = %v, want 0", result["coverage"])
	}
}

func TestParseCoverageBadge(t *testing.T) {
	cases := []struct {
		body string
		want int
		ok   bool
	}{
		{`{"schemaVersion":1,"label":"coverage","message":"85%","color":"green"}`, 85, true},
		{`{"message":"85.7%"}`, 85, true},
		{`{"message":"0%"}`, 0, true},
		{`{"message":"n/a"}`, 0, false},
		{`<svg><text>coverage</text><text>98.5%</text></svg>`, 98, true},
		// The real shape: the gradient's y2="100%" attribute comes before the
		// text. Read the raw markup and a 98.5% repo reports 100 (observed).
		{octocovBadgeSVG, 98, true},
		{`<svg><linearGradient y2="100%"/><text>coverage</text><text>0%</text></svg>`, 0, true},
		{`<svg><linearGradient y2="100%"/><text>coverage</text><text>unknown</text></svg>`, 0, false},
		{`coverage: 100%`, 100, true},
		{`coverage: 250%`, 0, false},
		{`not a badge`, 0, false},
		{``, 0, false},
	}
	for _, c := range cases {
		got, ok := parseCoverageBadge(c.body)
		if got != c.want || ok != c.ok {
			t.Errorf("parseCoverageBadge(%q) = %d,%v want %d,%v", c.body, got, ok, c.want, c.ok)
		}
	}
}

// octocovBadgeSVG is the badge octocov (and shields.io) actually publishes:
// a linearGradient whose y2="100%" attribute precedes the <text> nodes that
// carry the rendered percentage.
const octocovBadgeSVG = `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="133.5" height="20" role="img" aria-label="octocov::badge">
    <title>octocov::badge</title>
    <linearGradient id="s" x2="0" y2="100%">
        <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
        <stop offset="1" stop-opacity=".1"/>
    </linearGradient>
    <clipPath id="r"><rect width="133.5" height="20" rx="3" fill="#fff"/></clipPath>
    <g clip-path="url(#r)">
        <rect width="83.5" height="20" fill="#24292E"/>
        <rect x="83.5" width="50" height="20" fill="#97CA00"/>
        <rect width="133.5" height="20" fill="url(#s)"/>
    </g>
    <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" text-rendering="geometricPrecision" font-size="110">
        <text aria-hidden="true" x="495" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)">coverage</text>
        <text x="495" y="140" transform="scale(.1)" fill="#fff">coverage</text>
        <text aria-hidden="true" x="1085" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)">98.5%</text>
        <text x="1085" y="140" transform="scale(.1)" fill="#fff">98.5%</text>
    </g>
</svg>`
