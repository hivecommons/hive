package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// TestHandleACMMRecommendationEmpty verifies the endpoint is safe with a
// zero/empty server (no deps, no status): it returns 200 JSON with a valid
// recommendation and never panics.
func TestHandleACMMRecommendationEmpty(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/acmm-recommendation", nil)
	rr := httptest.NewRecorder()
	s.handleACMMRecommendation(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}

	var rec acmmadvisor.Recommendation
	if err := json.Unmarshal(rr.Body.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	// Empty status → conservative L1, no quality agent → advise stay.
	if rec.CurrentLevel != acmmadvisor.MinLevel {
		t.Fatalf("CurrentLevel = %d, want %d", rec.CurrentLevel, acmmadvisor.MinLevel)
	}
	if rec.Advise != acmmadvisor.AdviseStay {
		t.Fatalf("Advise = %q, want stay (no quality agent)", rec.Advise)
	}
	if rec.Rationale == "" {
		t.Fatal("Rationale must be populated")
	}
}

// TestHandleACMMRecommendationWithSignals drives the endpoint with a live
// config + status snapshot and asserts the advisor consumes the real signals.
func TestHandleACMMRecommendationWithSignals(t *testing.T) {
	s := newTestServer()

	// L1 with a quality agent present → the gentle L1→L2 gate is satisfied.
	lvl := 1
	s.deps = &Dependencies{
		Config: &config.Config{
			ACMMLevel: &lvl,
			Agents: map[string]config.AgentConfig{
				qualityAgentName: {},
			},
		},
	}

	// Publish a status snapshot carrying live queue/coverage signals.
	status := minimalPayload()
	status.ACMMLevel = 1
	status.Governor.Issues = 3
	status.Hold.Total = 2
	status.AgentMetrics = map[string]any{
		agentMetricsCIMaintainerKey: map[string]any{
			agentMetricsCoverageKey: 42,
		},
	}
	s.UpdateStatus(status)

	req := httptest.NewRequest(http.MethodGet, "/api/acmm-recommendation", nil)
	rr := httptest.NewRecorder()
	s.handleACMMRecommendation(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var rec acmmadvisor.Recommendation
	if err := json.Unmarshal(rr.Body.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.CurrentLevel != 1 {
		t.Fatalf("CurrentLevel = %d, want 1", rec.CurrentLevel)
	}
	// L1→L2 only requires a quality agent, which is present → advise raise.
	if rec.Advise != acmmadvisor.AdviseRaise {
		t.Fatalf("Advise = %q, want raise (quality agent present at L1); rationale=%q", rec.Advise, rec.Rationale)
	}
	if rec.TargetLevel != 2 {
		t.Fatalf("TargetLevel = %d, want 2", rec.TargetLevel)
	}
}

// TestBuildACMMStatusInputs_ReadsLiveSignals unit-tests the signal mapping in
// isolation, including the coverage extraction and nil-safety.
func TestBuildACMMStatusInputs_ReadsLiveSignals(t *testing.T) {
	resetGreenStreakCache(t)
	s := newTestServer()
	lvl := 3
	s.deps = &Dependencies{
		Config: &config.Config{
			ACMMLevel: &lvl,
			Agents:    map[string]config.AgentConfig{qualityAgentName: {}},
		},
	}
	status := minimalPayload()
	status.Governor.Issues = 7
	status.Hold.Total = 4
	status.AgentMetrics = map[string]any{
		agentMetricsCIMaintainerKey: map[string]any{
			// float64 form (as after a JSON round-trip) must also parse.
			agentMetricsCoverageKey: float64(88),
		},
	}
	s.UpdateStatus(status)

	in := s.buildACMMStatusInputs()
	if in.CurrentLevel != 3 {
		t.Fatalf("CurrentLevel = %d, want 3", in.CurrentLevel)
	}
	if !in.HasQualityAgent {
		t.Fatal("HasQualityAgent = false, want true")
	}
	if in.ActionableIssues != 7 {
		t.Fatalf("ActionableIssues = %d, want 7", in.ActionableIssues)
	}
	if in.HoldCount != 4 {
		t.Fatalf("HoldCount = %d, want 4", in.HoldCount)
	}
	if in.CoveragePct != 88 {
		t.Fatalf("CoveragePct = %v, want 88", in.CoveragePct)
	}
	// GreenStreak must read zero here: no streak has ever been measured in this
	// test (the cache is cleared above), so the signal is unknown, not a
	// measured zero, and must never be fabricated (#5226).
	// MergeSuccessRate must also read zero here: no fleet-stats collector is
	// wired into deps, so the signal is unknown, not measured (#3972).
	if in.GreenStreak != 0 || in.MergeSuccessRate != 0 {
		t.Fatalf("GreenStreak/MergeSuccessRate should be 0, got %d / %v", in.GreenStreak, in.MergeSuccessRate)
	}
}

// resetGreenStreakCache clears the package-level green-CI streak cache and
// restores it when the test ends, so streak-sensitive tests do not leak state
// into each other.
func resetGreenStreakCache(t *testing.T) {
	t.Helper()
	cachedGreenStreakMu.Lock()
	prevStreak, prevOK := cachedGreenStreak, cachedGreenStreakOK
	cachedGreenStreak, cachedGreenStreakOK = 0, false
	cachedGreenStreakMu.Unlock()
	t.Cleanup(func() {
		cachedGreenStreakMu.Lock()
		cachedGreenStreak, cachedGreenStreakOK = prevStreak, prevOK
		cachedGreenStreakMu.Unlock()
	})
}

// TestBuildACMMStatusInputs_GreenStreak verifies the #5226 wiring: when the
// status-build path has measured a real green-CI streak, that REAL VALUE
// reaches the advisor input — it is no longer the hardcoded zero this code
// carried before. The unmeasured case must stay at the conservative zero so an
// absent measurement is never reported as a measured streak.
func TestBuildACMMStatusInputs_GreenStreak(t *testing.T) {
	cases := []struct {
		name     string
		streak   int
		measured bool
		want     int
	}{
		{
			// The load-bearing case: a measured streak of 7 must arrive as 7.
			// Against the pre-#5226 code this fails (it would read 0), which is
			// what makes this an assertion of substance rather than shape.
			name: "measured streak flows through",
			// A value above greenStreakL4 (5) and below greenStreakL6 (12), so
			// it is unmistakably a real reading rather than a boundary artifact.
			streak: 7, measured: true, want: 7,
		},
		{
			// A measured zero is legitimate: CI ran and the latest run is red.
			name:   "measured zero (latest run red) is honest",
			streak: 0, measured: true, want: 0,
		},
		{
			// Never measured: must NOT be reported as a streak.
			name:   "unmeasured stays conservative",
			streak: 99, measured: false, want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetGreenStreakCache(t)
			if tc.measured {
				cachedGreenStreakMu.Lock()
				cachedGreenStreak, cachedGreenStreakOK = tc.streak, true
				cachedGreenStreakMu.Unlock()
			}
			s := newTestServer()
			lvl := 3
			s.deps = &Dependencies{Config: &config.Config{ACMMLevel: &lvl}}
			s.UpdateStatus(minimalPayload())

			if got := s.buildACMMStatusInputs().GreenStreak; got != tc.want {
				t.Fatalf("GreenStreak = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestStatusPayloadCarriesACMMAdvice verifies the #5225 wiring end-to-end: the
// status payload must carry a recommendation whose CONTENT reflects the live
// signals, not merely a non-nil struct. A hive at L3 with a measured green
// streak, quality agent and coverage must produce advice that evaluates the
// L3→L4 criteria and names the real streak value in its checklist.
func TestStatusPayloadCarriesACMMAdvice(t *testing.T) {
	resetGreenStreakCache(t)
	cachedGreenStreakMu.Lock()
	cachedGreenStreak, cachedGreenStreakOK = 7, true
	cachedGreenStreakMu.Unlock()

	s := newTestServer()
	lvl := 3
	s.deps = &Dependencies{
		Config: &config.Config{
			ACMMLevel: &lvl,
			Agents:    map[string]config.AgentConfig{qualityAgentName: {}},
		},
	}
	s.UpdateStatus(minimalPayload())

	in := s.buildACMMStatusInputs()
	if in.GreenStreak != 7 {
		t.Fatalf("advisor input GreenStreak = %d, want the measured 7", in.GreenStreak)
	}
	rec := acmmadvisor.RecommendFromStatus(in)
	if rec.CurrentLevel != 3 {
		t.Fatalf("recommendation CurrentLevel = %d, want 3", rec.CurrentLevel)
	}
	// The streak criterion must appear as MET: 7 clears the L4 floor of 5.
	// This asserts the real value drove a real verdict — a payload carrying a
	// permanently-zero streak would land this criterion in Unmet instead.
	var found bool
	for _, c := range rec.Met {
		if strings.Contains(c.Name, "Green-CI streak") {
			found = true
		}
	}
	if !found {
		var unmet []string
		for _, c := range rec.Unmet {
			unmet = append(unmet, c.Name)
		}
		t.Fatalf("Green-CI streak should be MET with a measured streak of 7; unmet = %v", unmet)
	}
}

// TestBuildACMMStatusInputs_MergeSuccessRate verifies the #3972 wiring: when
// the fleet-stats collector holds a completed collect, the advisor input is
// the real merged/(merged+rejected) ratio — no longer the hardcoded zero —
// and when counts are absent or the window is empty the input stays at the
// honest, conservative zero instead of a fabricated rate.
func TestBuildACMMStatusInputs_MergeSuccessRate(t *testing.T) {
	// seededFleetStats builds a ready collector carrying the given counts
	// through its public persistence API (the tracker's fields moved to
	// pkg/dashboard/collect with the collector and are no longer pokeable).
	seededFleetStats := func(counts ghpkg.FleetContribCounts) *collect.FleetStatsCollector {
		blob, err := json.Marshal(map[string]any{"counts": counts, "collected_at": time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "fleet-stats.json")
		if err := os.WriteFile(path, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		fc := collect.NewFleetStatsCollector(nil, "bot", "org", nil)
		fc.EnablePersistence(path)
		return fc
	}

	cases := []struct {
		name string
		fc   *collect.FleetStatsCollector
		want float64
	}{
		{
			name: "measured ratio flows through",
			// 30 merged / 10 rejected over the window → 0.75. Before #3972
			// this input was hardcoded to 0, so this case fails against the
			// unfixed behavior.
			fc:   seededFleetStats(ghpkg.FleetContribCounts{PRsMerged: 30, PRsRejected: 10}),
			want: 0.75,
		},
		{
			name: "nil collector stays zero",
			fc:   nil,
			want: 0,
		},
		{
			name: "collector never collected stays zero",
			fc:   collect.NewFleetStatsCollector(nil, "bot", "org", nil),
			want: 0,
		},
		{
			name: "empty window stays zero, not a fabricated 1.0",
			// A successful collect that found no resolved PRs: the rate is
			// unknown, and the advisor must not see a measured value.
			fc:   seededFleetStats(ghpkg.FleetContribCounts{}),
			want: 0,
		},
		{
			name: "all rejected reads as measured zero",
			fc:   seededFleetStats(ghpkg.FleetContribCounts{PRsRejected: 5}),
			want: 0,
		},
		{
			name: "all merged reads as measured 1.0",
			fc:   seededFleetStats(ghpkg.FleetContribCounts{PRsMerged: 4}),
			want: 1.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			s.deps = &Dependencies{FleetStats: tc.fc}
			in := s.buildACMMStatusInputs()
			if in.MergeSuccessRate != tc.want {
				t.Fatalf("MergeSuccessRate = %v, want %v", in.MergeSuccessRate, tc.want)
			}
		})
	}
}

// TestBuildACMMStatusInputs_NilSafe confirms zero signals on a bare server.
func TestBuildACMMStatusInputs_NilSafe(t *testing.T) {
	s := newTestServer()
	in := s.buildACMMStatusInputs()
	if in.CurrentLevel != acmmadvisor.MinLevel {
		t.Fatalf("CurrentLevel = %d, want %d", in.CurrentLevel, acmmadvisor.MinLevel)
	}
	if in.HasQualityAgent {
		t.Fatal("HasQualityAgent should be false on a bare server")
	}
	if in.CoveragePct != 0 || in.ActionableIssues != 0 || in.HoldCount != 0 {
		t.Fatalf("expected zero signals, got %+v", in)
	}
}

// TestCoverageFromAgentMetrics covers the numeric-form and malformed cases.
func TestCoverageFromAgentMetrics(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want float64
	}{
		{"nil map", nil, 0},
		{"missing ci-maintainer", map[string]any{"x": 1}, 0},
		{"int", map[string]any{agentMetricsCIMaintainerKey: map[string]any{agentMetricsCoverageKey: 91}}, 91},
		{"float64", map[string]any{agentMetricsCIMaintainerKey: map[string]any{agentMetricsCoverageKey: float64(75)}}, 75},
		{"wrong type", map[string]any{agentMetricsCIMaintainerKey: map[string]any{agentMetricsCoverageKey: "nope"}}, 0},
		{"ci not a map", map[string]any{agentMetricsCIMaintainerKey: 5}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coverageFromAgentMetrics(c.in); got != c.want {
				t.Fatalf("coverageFromAgentMetrics = %v, want %v", got, c.want)
			}
		})
	}
}

// TestHandleACMMRecommendationViaMux exercises route registration end-to-end.
func TestHandleACMMRecommendationViaMux(t *testing.T) {
	s := newTestServer()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/acmm-recommendation", s.handleACMMRecommendation)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/acmm-recommendation")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var rec acmmadvisor.Recommendation
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
