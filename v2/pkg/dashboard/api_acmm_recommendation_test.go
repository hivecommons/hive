package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/acmmadvisor"
	"github.com/kubestellar/hive/v2/pkg/config"
	ghpkg "github.com/kubestellar/hive/v2/pkg/github"
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
	// GreenStreak is still untracked and must remain zero (never fabricated).
	// MergeSuccessRate must also read zero here: no fleet-stats collector is
	// wired into deps, so the signal is unknown, not measured (#3972).
	if in.GreenStreak != 0 || in.MergeSuccessRate != 0 {
		t.Fatalf("GreenStreak/MergeSuccessRate should be 0, got %d / %v", in.GreenStreak, in.MergeSuccessRate)
	}
}

// TestBuildACMMStatusInputs_MergeSuccessRate verifies the #3972 wiring: when
// the fleet-stats collector holds a completed collect, the advisor input is
// the real merged/(merged+rejected) ratio — no longer the hardcoded zero —
// and when counts are absent or the window is empty the input stays at the
// honest, conservative zero instead of a fabricated rate.
func TestBuildACMMStatusInputs_MergeSuccessRate(t *testing.T) {
	cases := []struct {
		name string
		fc   *FleetStatsCollector
		want float64
	}{
		{
			name: "measured ratio flows through",
			// 30 merged / 10 rejected over the window → 0.75. Before #3972
			// this input was hardcoded to 0, so this case fails against the
			// unfixed behavior.
			fc:   &FleetStatsCollector{counts: ghpkg.FleetContribCounts{PRsMerged: 30, PRsRejected: 10}, ready: true},
			want: 0.75,
		},
		{
			name: "nil collector stays zero",
			fc:   nil,
			want: 0,
		},
		{
			name: "collector never collected stays zero",
			fc:   &FleetStatsCollector{},
			want: 0,
		},
		{
			name: "empty window stays zero, not a fabricated 1.0",
			// A successful collect that found no resolved PRs: the rate is
			// unknown, and the advisor must not see a measured value.
			fc:   &FleetStatsCollector{counts: ghpkg.FleetContribCounts{}, ready: true},
			want: 0,
		},
		{
			name: "all rejected reads as measured zero",
			fc:   &FleetStatsCollector{counts: ghpkg.FleetContribCounts{PRsRejected: 5}, ready: true},
			want: 0,
		},
		{
			name: "all merged reads as measured 1.0",
			fc:   &FleetStatsCollector{counts: ghpkg.FleetContribCounts{PRsMerged: 4}, ready: true},
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
