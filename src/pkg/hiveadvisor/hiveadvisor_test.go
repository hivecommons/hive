package hiveadvisor

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
)

func TestModePolarity(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		mode string
		sig  Signals
		want string
	}{
		{"idle increases throughput", "idle", Signals{RepoCount: 1}, "widen-hive-repos"},
		{"quiet raises maturity", "quiet", Signals{RepoCount: 4, ACMM: acmmadvisor.Recommendation{Advise: acmmadvisor.AdviseRaise, CurrentLevel: 3, TargetLevel: 4}}, "raise-acmm-level"},
		{"busy holds merge side", "busy", Signals{QueueIssues: 3, QueuePRs: 3, HoldCount: 1}, "rebalance-merge-side"},
		{"surge reduces inflow", "surge", Signals{QueueIssues: 20, QueuePRs: 8}, "throttle-pr-producing-lanes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Recommend(Request{Now: now, Signals: mergeMode(tt.sig, tt.mode), TopN: 1})
			if len(got.Recommendations) != 1 || got.Recommendations[0].ID != tt.want {
				t.Fatalf("top recommendation = %#v, want %s", got.Recommendations, tt.want)
			}
		})
	}
}

func TestRankingTieBreakDeterministic(t *testing.T) {
	in := []Recommendation{
		rec("zeta", 50, "z", "z"),
		rec("alpha", 50, "a", "a"),
		rec("middle", 60, "m", "m"),
	}
	got := rank(in, 0)
	gotIDs := ids(got)
	want := []string{"middle", "alpha", "zeta"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("rank order = %v, want %v", gotIDs, want)
	}
	if ids(in)[0] != "zeta" {
		t.Fatalf("rank mutated input: %v", ids(in))
	}
}

func TestEpochFreezeAndEarlyModeInvalidation(t *testing.T) {
	start := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	first := Recommend(Request{Now: start, Signals: Signals{Mode: "idle", RepoCount: 1}})
	if first.Frozen {
		t.Fatal("first computation should not be marked frozen")
	}

	changedSignals := Signals{Mode: "idle", DisabledAgentCount: 2, RepoCount: 10}
	second := Recommend(Request{Now: start.Add(48 * time.Hour), Signals: changedSignals, Previous: &first.Epoch})
	if !second.Frozen {
		t.Fatal("same mode inside epoch should reuse frozen recommendations")
	}
	if !reflect.DeepEqual(ids(second.Recommendations), ids(first.Recommendations)) {
		t.Fatalf("frozen ids = %v, want %v", ids(second.Recommendations), ids(first.Recommendations))
	}
	if second.NextReviewInDays != 5 {
		t.Fatalf("next review days = %d, want 5", second.NextReviewInDays)
	}

	third := Recommend(Request{Now: start.Add(48 * time.Hour), Signals: Signals{Mode: "surge", QueueIssues: 10}, Previous: &first.Epoch})
	if third.Frozen {
		t.Fatal("mode change should refresh early, not freeze")
	}
	if third.Epoch.Mode != "SURGE" || ids(third.Recommendations)[0] != "throttle-pr-producing-lanes" {
		t.Fatalf("mode invalidation result = %#v", third)
	}
}

func TestEpochExpiresAfterWeek(t *testing.T) {
	start := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	first := Recommend(Request{Now: start, Signals: Signals{Mode: "quiet", RepoCount: 1}})
	second := Recommend(Request{Now: start.Add(EpochLength + time.Second), Signals: Signals{Mode: "quiet", DisabledAgentCount: 1, RepoCount: 4}, Previous: &first.Epoch})
	if second.Frozen {
		t.Fatal("expired epoch should refresh")
	}
	if got := ids(second.Recommendations)[0]; got != "enable-disabled-agent" {
		t.Fatalf("refreshed top = %s", got)
	}
}

func TestTopNTruncation(t *testing.T) {
	got := Recommend(Request{Now: time.Now(), TopN: 2, Signals: Signals{Mode: "surge", QueueIssues: 20, HoldCount: 3, BudgetUsedPct: 95}})
	if len(got.Recommendations) != 2 {
		t.Fatalf("recommendations = %d, want 2", len(got.Recommendations))
	}
	want := []string{"fix-merge-blockers", "throttle-pr-producing-lanes"}
	if !reflect.DeepEqual(ids(got.Recommendations), want) {
		t.Fatalf("top 2 = %v, want %v", ids(got.Recommendations), want)
	}
}

func TestEmptyAndDegenerateInputs(t *testing.T) {
	got := Recommend(Request{Signals: Signals{Mode: "???", QueueIssues: -1, BudgetUsedPct: 250}})
	if got.Epoch.Mode != "IDLE" {
		t.Fatalf("mode = %s, want IDLE", got.Epoch.Mode)
	}
	if len(got.Recommendations) == 0 {
		t.Fatal("expected a calm fallback recommendation")
	}
	for _, s := range got.Recommendations[0].Signals {
		if s.Name == "queue_total" && s.Value != "0" {
			t.Fatalf("negative queue leaked into signals: %#v", got.Recommendations[0].Signals)
		}
	}
}

func ids(recs []Recommendation) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

func mergeMode(s Signals, mode string) Signals {
	s.Mode = mode
	return s
}
