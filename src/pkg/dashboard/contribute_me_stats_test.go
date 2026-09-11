package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedStatsProfile writes a contributor profile carrying the three cumulative
// counters the "Your contribution" panel reads back (#6543).
func seedStatsProfile(t *testing.T, username string, completed, withPR, failed int) {
	t.Helper()
	p := &ContributorProfile{
		GitHubUsername: username,
		ContributorID:  "c-" + username,
		TrustTier:      "contributor",
		RegisteredAt:   time.Now().UTC().Format(time.RFC3339),
		TasksCompleted: completed,
		TasksWithPR:    withPR,
		TasksFailed:    failed,
	}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save profile: %v", err)
	}
}

// meStatsServer builds a server with an isolated contributor store and metrics
// file, with the contribute routes registered.
func meStatsServer(t *testing.T) *Server {
	t.Helper()
	setupContributeEnv(t)
	t.Setenv("HIVE_METRICS_FILE", filepath.Join(t.TempDir(), "metrics.json"))
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	return s
}

// getAs issues a GET as an identified viewer (the hub-injected identity header
// resolveViewerUsername reads), matching the interests-endpoint test style.
func getAs(s *Server, path, user string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// TestContributeMeAnonymousRejected proves the endpoint refuses an anonymous
// caller rather than answering with somebody's numbers. /api/contribute* is a
// PUBLIC prefix (isPublicPath), so this 401 is the only gate there is.
func TestContributeMeAnonymousRejected(t *testing.T) {
	s := meStatsServer(t)
	if rec := getAs(s, "/api/contribute/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/contribute/me = %d, want 401", rec.Code)
	}
}

// TestContributeMeNoProfileForbidden proves a signed-in visitor who never
// registered on this hive gets 403, not a zeroed record that reads like they
// contributed nothing.
func TestContributeMeNoProfileForbidden(t *testing.T) {
	s := meStatsServer(t)
	if rec := getAs(s, "/api/contribute/me", "ghost"); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/contribute/me with no profile = %d, want 403", rec.Code)
	}
}

// TestContributeMeReportsOwnTotals is the core of #6543: the PR count that
// auto-promotion already reads is finally readable by the person who earned it,
// and it is DISTINCT from the bare completion count.
func TestContributeMeReportsOwnTotals(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "clanker", 120, 14, 3)

	rec := getAs(s, "/api/contribute/me", "clanker")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contribute/me = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Username  string `json:"github_username"`
		Tier      string `json:"trust_tier"`
		Completed int    `json:"total_tasks_completed"`
		WithPR    int    `json:"total_tasks_completed_with_pr"`
		Failed    int    `json:"total_tasks_failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if got.Username != "clanker" || got.Tier != "contributor" {
		t.Fatalf("identity wrong: %+v", got)
	}
	if got.Completed != 120 || got.WithPR != 14 || got.Failed != 3 {
		t.Fatalf("counters = completed %d / with-PR %d / failed %d, want 120/14/3",
			got.Completed, got.WithPR, got.Failed)
	}
	if got.WithPR == got.Completed {
		t.Fatal("with-PR must stay distinct from completed — that distinction is the point of the feature")
	}
}

// TestContributeMeNeverAnswersForSomeoneElse proves there is no username
// parameter to aim at another contributor: a query string naming someone else is
// ignored, and the caller still sees only their own record.
func TestContributeMeNeverAnswersForSomeoneElse(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "alice", 100, 40, 1)
	seedStatsProfile(t, "bob", 2, 0, 0)

	rec := getAs(s, "/api/contribute/me?id=alice&username=alice&user=alice", "bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Username  string `json:"github_username"`
		Completed int    `json:"total_tasks_completed"`
		WithPR    int    `json:"total_tasks_completed_with_pr"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Username != "bob" || got.Completed != 2 || got.WithPR != 0 {
		t.Fatalf("caller bob got %+v — must never be answered with alice's record", got)
	}
}

// TestContributeMe24hWindow proves the 24-hour figure sums the trailing 24
// hourly buckets of the caller's OWN series — the number the issue could not be
// derived client-side before the rings were aligned.
func TestContributeMe24hWindow(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "worker", 40, 9, 0)

	// 30 rollups: the first seeds the baseline, then "worker" completes one task
	// per hour. The trailing 24 buckets must therefore sum to exactly 24.
	store := s.contributeMetricsStore()
	for i := 0; i < 30; i++ {
		store.rollup(rollupSample{
			queueDepth: 1,
			fleetSize:  1,
			userTotals: map[string]int{"worker": i},
			now:        time.Now(),
		})
	}

	rec := getAs(s, "/api/contribute/me", "worker")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Recent    int  `json:"tasks_completed_24h"`
		Window    int  `json:"window_hours"`
		Covered   int  `json:"window_hours_covered"`
		HaveHours bool `json:"history_available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if !got.HaveHours {
		t.Fatal("history_available = false, want true — 30 buckets were rolled up")
	}
	if got.Window != recentWindowBuckets {
		t.Fatalf("window_hours = %d, want %d", got.Window, recentWindowBuckets)
	}
	if got.Covered != recentWindowBuckets {
		t.Fatalf("window_hours_covered = %d, want a full %d", got.Covered, recentWindowBuckets)
	}
	if got.Recent != recentWindowBuckets {
		t.Fatalf("tasks_completed_24h = %d, want %d (one completion per hour)", got.Recent, recentWindowBuckets)
	}
}

// TestContributeMeShortHistoryIsHonest proves a spoke with less than a day of
// history says how much it actually has, instead of labelling six hours "24h".
func TestContributeMeShortHistoryIsHonest(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "fresh", 5, 2, 0)

	store := s.contributeMetricsStore()
	for i := 0; i < 6; i++ {
		store.rollup(rollupSample{
			queueDepth: 0, fleetSize: 1,
			userTotals: map[string]int{"fresh": i},
			now:        time.Now(),
		})
	}

	rec := getAs(s, "/api/contribute/me", "fresh")
	var got struct {
		Recent    int  `json:"tasks_completed_24h"`
		Covered   int  `json:"window_hours_covered"`
		HaveHours bool `json:"history_available"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.HaveHours {
		t.Fatal("history_available = false, want true")
	}
	if got.Covered != 6 {
		t.Fatalf("window_hours_covered = %d, want 6 — the real depth of history", got.Covered)
	}
	if got.Recent != 5 {
		t.Fatalf("tasks_completed_24h = %d, want 5 (buckets 2..6 each carry 1)", got.Recent)
	}
}

// TestContributeMeNoHistoryIsNotZero proves a contributor with no hourly series
// yet is reported as "not measured" rather than as a real zero. Conflating the
// two would tell a brand-new contributor they did nothing today.
func TestContributeMeNoHistoryIsNotZero(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "newbie", 0, 0, 0)

	rec := getAs(s, "/api/contribute/me", "newbie")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	var got struct {
		Recent    int  `json:"tasks_completed_24h"`
		Covered   int  `json:"window_hours_covered"`
		HaveHours bool `json:"history_available"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.HaveHours {
		t.Fatal("history_available = true with no rollups, want false")
	}
	if got.Recent != 0 || got.Covered != 0 {
		t.Fatalf("unmeasured window should report 0/0, got %d/%d", got.Recent, got.Covered)
	}
}

// TestContributeMeMatchesProfileCase proves the 24h lookup keys off the STORED
// github_username, not the case the session handed us. The metrics rings are
// keyed on the stored string, so keying off the caller would silently degrade a
// real figure to "no history" for anyone whose login case differs.
func TestContributeMeMatchesProfileCase(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "MixedCase", 10, 4, 0)

	store := s.contributeMetricsStore()
	for i := 0; i < 4; i++ {
		store.rollup(rollupSample{
			queueDepth: 0, fleetSize: 1,
			userTotals: map[string]int{"MixedCase": i},
			now:        time.Now(),
		})
	}

	rec := getAs(s, "/api/contribute/me", "mixedcase")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Username  string `json:"github_username"`
		Recent    int    `json:"tasks_completed_24h"`
		HaveHours bool   `json:"history_available"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Username != "MixedCase" {
		t.Fatalf("github_username = %q, want the stored case %q", got.Username, "MixedCase")
	}
	if !got.HaveHours || got.Recent != 3 {
		t.Fatalf("24h figure lost to a case mismatch: available=%v recent=%d, want true/3",
			got.HaveHours, got.Recent)
	}
}

// TestContributionPanelWiredOnOpsPage pins the client-side pieces of the "Your
// contribution" panel on the rendered /contribute page, in the strings.Contains
// style the other Operations-page tests use, so the panel cannot silently
// disappear the way the sparkline homes are pinned above it.
func TestContributionPanelWiredOnOpsPage(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`id="cc-mine-card"`,      // the panel itself
		`id="cc-mine-body"`,      // the tile row mount
		`id="spark-mine"`,        // its own 7-day trend slot
		"/api/contribute/me",     // the self-service fetch target
		"function ccLoadMine(",   // the loader
		"function ccRenderMine(", // the painter
		"ccLoadMine();",          // actually invoked from opsPoll
		"Issues worked (24h)",    // the three asks from the issue, verbatim
		"Issues worked (total)",
		"PRs produced",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered contribute page missing contribution-panel marker %q", want)
		}
	}
}
