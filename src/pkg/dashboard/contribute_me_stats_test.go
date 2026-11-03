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

// TestContributeMeNoProfileIsAnEmptyState proves a signed-in visitor who never
// registered on this hive gets a 200 that SAYS there is no record —
// has_profile:false and no counters — rather than a zeroed record that reads
// like they contributed nothing, and rather than the 403 this used to be
// (#12111). The 403 was semantically wrong (the hive's own owner is routinely
// in this state) and operationally fatal on a hosted spoke, where the Ingress
// inherited a controller-wide custom-http-errors list that includes 403 and
// replaced the JSON with an HTML error page the card could not read.
func TestContributeMeNoProfileIsAnEmptyState(t *testing.T) {
	s := meStatsServer(t)
	rec := getAs(s, "/api/contribute/me", "ghost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contribute/me with no profile = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if got["github_username"] != "ghost" {
		t.Errorf("github_username = %v, want the caller's own identity so the card can address them", got["github_username"])
	}
	if hp, ok := got["has_profile"].(bool); !ok || hp {
		t.Errorf("has_profile = %v, want false", got["has_profile"])
	}
	for _, counter := range []string{"total_tasks_completed", "total_tasks_completed_with_pr", "total_tasks_failed", "tasks_completed_24h", "trust_tier"} {
		if _, present := got[counter]; present {
			t.Errorf("empty state carries %q; a reader must not mistake no record for a record of zero", counter)
		}
	}
}

// TestContributeMeHubOwnerWithoutProfile is the shape of #12111 exactly: the
// hub-proxied spoke's OWNER, identified by the hub's injected headers, who
// has never pointed a contributor agent at their own hive. They must get the
// same readable empty state as any other signed-in visitor — a 2xx, so no
// Ingress error interception can stand between them and it.
func TestContributeMeHubOwnerWithoutProfile(t *testing.T) {
	s := ownerTokenServer(t, "clubanderson", true)
	seedStatsProfile(t, "mrbobbytables", 10, 4, 0)

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/me", nil)
	req.Header.Set("X-Hive-User", "clubanderson")
	req.Header.Set("X-Hive-Role", "owner")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner without profile GET /api/contribute/me = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("Content-Type = %q, want JSON", ct)
	}
	var got struct {
		Username   string `json:"github_username"`
		HasProfile *bool  `json:"has_profile"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Username != "clubanderson" || got.HasProfile == nil || *got.HasProfile {
		t.Fatalf("owner got %s, want github_username=clubanderson has_profile=false", rec.Body.String())
	}
}

// TestContributeMeProfileSaysSo pins has_profile:true on the populated record,
// so a client can key the two shapes apart on one field in both directions.
func TestContributeMeProfileSaysSo(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "present", 1, 1, 0)
	rec := getAs(s, "/api/contribute/me", "present")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contribute/me = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		HasProfile *bool `json:"has_profile"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HasProfile == nil || !*got.HasProfile {
		t.Fatalf("populated record has_profile = %v, want true", got.HasProfile)
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
		Username           string `json:"github_username"`
		Tier               string `json:"trust_tier"`
		EligibleForTrusted bool   `json:"eligible_for_trusted"`
		Completed          int    `json:"total_tasks_completed"`
		WithPR             int    `json:"total_tasks_completed_with_pr"`
		Failed             int    `json:"total_tasks_failed"`
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
	if got.EligibleForTrusted {
		t.Fatal("below-threshold contributor should not be eligible for trusted")
	}
}

func TestContributeMeReportsTrustedEligibility(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "eligible", 25, contributorTrustedAt, 0)

	rec := getAs(s, "/api/contribute/me", "eligible")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contribute/me = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		EligibleForTrusted bool `json:"eligible_for_trusted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if !got.EligibleForTrusted {
		t.Fatal("eligible_for_trusted = false, want true")
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

// meWithPRRollups seeds a profile and drives n rollups in which the contributor
// completes `done` tasks and ships `prs` pull requests per hour (#7894).
func meWithPRRollups(t *testing.T, s *Server, user string, n, done, prs int) {
	t.Helper()
	seedStatsProfile(t, user, n*done, n*prs, 0)
	store := s.contributeMetricsStore()
	for i := 0; i < n; i++ {
		store.rollup(rollupSample{
			queueDepth: 0, fleetSize: 1,
			userTotals:   map[string]int{user: i * done},
			userPRTotals: map[string]int{user: i * prs},
			now:          time.Now(),
		})
	}
}

// mePRFields is the slice of the /me payload the PR tile reads.
type mePRFields struct {
	Recent    int  `json:"tasks_completed_24h"`
	PRs       int  `json:"prs_produced_24h"`
	Covered   int  `json:"window_hours_covered"`
	PRCovered int  `json:"pr_window_hours_covered"`
	HaveHours bool `json:"history_available"`
	HavePRs   bool `json:"pr_history_available"`
}

// TestContributeMePRs24hWindow is the core of #7894: the 24-hour PR figure sums
// the trailing 24 buckets of the caller's OWN PR ring, and it is DISTINCT from
// the 24-hour completion figure the card already showed.
func TestContributeMePRs24hWindow(t *testing.T) {
	s := meStatsServer(t)
	meWithPRRollups(t, s, "shipper", 30, 3, 1) // 3 completions/h, 1 PR/h

	rec := getAs(s, "/api/contribute/me", "shipper")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got mePRFields
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if !got.HavePRs {
		t.Fatal("pr_history_available = false, want true — 30 buckets were rolled up")
	}
	if got.PRCovered != recentWindowBuckets {
		t.Fatalf("pr_window_hours_covered = %d, want a full %d", got.PRCovered, recentWindowBuckets)
	}
	if got.PRs != recentWindowBuckets {
		t.Fatalf("prs_produced_24h = %d, want %d (one PR per hour)", got.PRs, recentWindowBuckets)
	}
	if got.Recent != 3*recentWindowBuckets {
		t.Fatalf("tasks_completed_24h = %d, want %d — the PR figure must not replace it", got.Recent, 3*recentWindowBuckets)
	}
}

// TestContributeMePRsShortHistoryIsHonest proves the PR figure carries its own
// coverage: a contributor with a day of completion history but a PR ring that
// only started three hours ago is told "3h", not a day of zero pull requests.
func TestContributeMePRsShortHistoryIsHonest(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "veteran", 40, 12, 0)
	store := s.contributeMetricsStore()
	// A day of completion-only history (a spoke from before the PR ring existed).
	for i := 0; i < 26; i++ {
		store.rollup(rollupSample{userTotals: map[string]int{"veteran": i}, now: time.Now()})
	}
	// Then three post-upgrade hours that sample PRs: seed, +1 PR, +0.
	store.seededTotals = false
	for _, withPR := range []int{10, 11, 11} {
		store.rollup(rollupSample{
			userTotals:   map[string]int{"veteran": 30},
			userPRTotals: map[string]int{"veteran": withPR},
			now:          time.Now(),
		})
	}

	rec := getAs(s, "/api/contribute/me", "veteran")
	var got mePRFields
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.HaveHours || got.Covered != recentWindowBuckets {
		t.Fatalf("completion window = covered %d / available %v, want %d/true", got.Covered, got.HaveHours, recentWindowBuckets)
	}
	if !got.HavePRs || got.PRCovered != 3 || got.PRs != 1 {
		t.Fatalf("PR window = %d PRs / covered %d / available %v, want 1/3/true — the real depth of PR history",
			got.PRs, got.PRCovered, got.HavePRs)
	}
}

// TestContributeMePRsNoHistoryIsNotZero proves a contributor whose PR ring has
// not started yet — every contributor between an upgrade and the first rollup
// after it — is reported as "not measured" for PRs even while their completion
// series is available, rather than as a real zero.
func TestContributeMePRsNoHistoryIsNotZero(t *testing.T) {
	s := meStatsServer(t)
	seedStatsProfile(t, "pre", 10, 4, 0)
	store := s.contributeMetricsStore()
	for i := 0; i < 5; i++ {
		store.rollup(rollupSample{userTotals: map[string]int{"pre": i}, now: time.Now()})
	}

	rec := getAs(s, "/api/contribute/me", "pre")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	var got mePRFields
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.HaveHours {
		t.Fatal("history_available = false, want true — the completion ring exists")
	}
	if got.HavePRs {
		t.Fatal("pr_history_available = true with no PR rollups, want false")
	}
	if got.PRs != 0 || got.PRCovered != 0 {
		t.Fatalf("unmeasured PR window should report 0/0, got %d/%d", got.PRs, got.PRCovered)
	}
}

// TestContributeMePRsMatchProfileCase proves the PR lookup keys off the STORED
// github_username like the completion lookup does, so a session with a
// different login case does not degrade the PR figure to "no history".
func TestContributeMePRsMatchProfileCase(t *testing.T) {
	s := meStatsServer(t)
	meWithPRRollups(t, s, "MixedCase", 4, 1, 1)

	rec := getAs(s, "/api/contribute/me", "mixedcase")
	var got mePRFields
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.HavePRs || got.PRs != 3 {
		t.Fatalf("PR figure lost to a case mismatch: available=%v prs=%d, want true/3", got.HavePRs, got.PRs)
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
		"PRs produced (total)",
		// #7894: the 24h PR tile, its own availability/coverage fields, and the
		// footnote that now speaks for both 24h figures.
		"PRs produced (24h)",
		"function ccMineRecent(",
		"d.prs_produced_24h",
		"d.pr_history_available",
		"d.pr_window_hours_covered",
		"Both 24h figures are summed from the same hourly rollup",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered contribute page missing contribution-panel marker %q", want)
		}
	}
}
