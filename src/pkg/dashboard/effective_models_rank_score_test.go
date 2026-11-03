package dashboard

import (
	"math"
	"strconv"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// liveModelRow mirrors one row of the "PRs by model" panel as seen on the
// dashboard (rank mode, 7d): merged PRs, how many merged first pass, and the
// run-log rates that feed the composite score.
type liveModelRow struct {
	model, backend string
	merged         int
	firstPass      int
	runs           int
	failed         int
	noShip         int
}

func liveModelFixture(now time.Time) ([]ghpkg.PullRequest, []TaskRunRecord) {
	rows := []liveModelRow{
		{"gpt-6-astra", "codex", 5, 5, 100, 51, 21},
		{"claude-opus-5.5", "copilot", 22, 21, 0, 0, 0},
		{"gpt-6.1-sol", "codex", 9, 8, 100, 2, 35},
		{"unknown", "unknown", 5, 4, 0, 0, 0},
		{"openai-codex/gpt-6.1-sol", "pi", 10, 7, 100, 0, 28},
		{"claude-fable-5.1", "copilot", 313, 203, 0, 0, 0},
		{"unknown", "codex", 37, 23, 100, 0, 36},
		{"claude-sonnet-5.5", "copilot", 17, 10, 0, 0, 0},
		{"claude-opus-5-5", "claude", 15, 6, 100, 0, 45},
		{"claude-sonnet-5-5", "claude", 1, 1, 0, 0, 0},
		{"openai-codex/gpt-5.6-sol:high", "omp", 1, 1, 0, 0, 0},
	}
	var prs []ghpkg.PullRequest
	var runs []TaskRunRecord
	n := 0
	for _, r := range rows {
		for i := 0; i < r.merged; i++ {
			n++
			prs = append(prs, ghpkg.PullRequest{
				Repo: "org/repo", Number: n, HiveAttributed: true, HiveModel: r.model, HiveBackend: r.backend,
				CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: i < r.firstPass},
			})
		}
		for i := 0; i < r.runs; i++ {
			rec := TaskRunRecord{TS: now.Format(time.RFC3339), Backend: r.backend, Model: r.model, Outcome: outcomeCompleted, PRVerified: true}
			switch {
			case i < r.failed:
				rec.Outcome = outcomeFailed
				rec.PRVerified = false
			case i < r.failed+r.noShip:
				rec.PRVerified = false
			}
			runs = append(runs, rec)
		}
	}
	return prs, runs
}

func TestWilsonLowerBoundPenalisesSmallSamples(t *testing.T) {
	if got := wilsonLowerBound(5, 5); math.Abs(got-0.566) > 0.01 {
		t.Fatalf("wilson(5/5) = %.3f, want ≈0.566", got)
	}
	if got := wilsonLowerBound(21, 22); math.Abs(got-0.781) > 0.01 {
		t.Fatalf("wilson(21/22) = %.3f, want ≈0.781", got)
	}
	if wilsonLowerBound(5, 5) >= wilsonLowerBound(21, 22) {
		t.Fatal("5/5 must not beat 21/22 on the lower bound")
	}
	if wilsonLowerBound(0, 0) != 0 || wilsonLowerBound(0, 7) != 0 {
		t.Fatal("empty samples must score 0")
	}
}

func TestEffectiveModelsRankUsesConfidenceWeightedScoreOnLiveRows(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	prs, runs := liveModelFixture(now)
	got := aggregateContributeEffectiveModels(prs, runs, "7d", "all", 5, now)

	index := func(model string) int {
		for i, r := range got.Ranked {
			if r.Model == model {
				return i
			}
		}
		return -1
	}
	// claude-fable-5.1 (203/313, no run failures) has the tightest bound.
	if index("claude-fable-5.1") != 0 {
		t.Fatalf("claude-fable-5.1 should rank #1; ranked = %s", rankedSummary(got.Ranked))
	}
	astra, opus := index("gpt-6-astra"), index("claude-opus-5.5")
	if astra < 0 || astra <= opus {
		t.Fatalf("gpt-6-astra (5/5 but 51%% fail) must not out-rank claude-opus-5.5; ranked = %s", rankedSummary(got.Ranked))
	}
	if astra != len(got.Ranked)-1 {
		t.Fatalf("gpt-6-astra (5/5 on 6 PRs, 51%% fail, 21%% no-ship) should be last of the ranked rows; ranked = %s", rankedSummary(got.Ranked))
	}
	for i, r := range got.Ranked {
		if r.Model == "unknown" {
			t.Fatalf("unknown model must never be ranked, found at #%d", i+1)
		}
		if r.EffectivenessRank != i+1 {
			t.Fatalf("rank[%d] = %d", i, r.EffectivenessRank)
		}
		if i > 0 && got.Ranked[i-1].Score < r.Score {
			t.Fatalf("ranked rows not sorted by score: %s", rankedSummary(got.Ranked))
		}
	}

	// Normalisation: the spellings of one model fold into one row that
	// carries both backends and the raw ids.
	sol := got.Ranked[index("gpt-6.1-sol")]
	if sol.MergedPRs != 19 || sol.Backend != "codex, pi" || len(sol.Sources) != 2 {
		t.Fatalf("gpt-6.1-sol should fold gpt-6.1-sol/codex + openai-codex/gpt-6.1-sol/pi: %+v", sol)
	}
	opusRow := got.Ranked[opus]
	if opusRow.MergedPRs != 37 || opusRow.Backend != "claude, copilot" {
		t.Fatalf("claude-opus-5.5 should fold claude-opus-5-5/claude: %+v", opusRow)
	}
	if index("claude-opus-5-5") >= 0 || index("openai-codex/gpt-6.1-sol") >= 0 {
		t.Fatalf("raw spellings leaked as separate ranked rows: %s", rankedSummary(got.Ranked))
	}

	var unattributed []contributeEffectiveModelRow
	for _, r := range got.Insufficient {
		if r.Unattributed {
			unattributed = append(unattributed, r)
		}
	}
	if len(unattributed) != 1 || unattributed[0].MergedPRs != 42 || unattributed[0].EffectivenessRank != 0 {
		t.Fatalf("unknown rows should fold into one unranked unattributed row: %+v", got.Insufficient)
	}
	if last := got.Insufficient[len(got.Insufficient)-1]; !last.Unattributed {
		t.Fatalf("unattributed row should sort last among insufficient: %+v", got.Insufficient)
	}
	for _, r := range got.Insufficient {
		if r.Model == "gpt-5.6-sol" && (len(r.Sources) != 1 || r.Sources[0].Model != "openai-codex/gpt-5.6-sol:high") {
			t.Fatalf("gpt-5.6-sol should keep its raw id in Sources: %+v", r)
		}
	}
}

func TestGovernorPRModelsEffectivenessCarriesScoreAndUnattributed(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	prs, runs := liveModelFixture(now)
	resp := aggregateGovernorPRModels(prs, "7d", now)
	applyGovernorPRModelEffectiveness(&resp, aggregateContributeEffectiveModels(prs, runs, "7d", "all", 5, now), 5)
	for _, b := range resp.Buckets {
		switch b.Model {
		case "unknown":
			if !b.Effectiveness.Unattributed || b.Effectiveness.Rank != 0 || b.Effectiveness.Eligible {
				t.Fatalf("unknown bucket effectiveness = %+v", b.Effectiveness)
			}
		case "claude-opus-5.5":
			if b.Effectiveness.Rank == 0 || !b.Effectiveness.Eligible || b.Effectiveness.Score <= 0 || b.Effectiveness.FirstPassMerged != 27 {
				t.Fatalf("opus bucket effectiveness = %+v", b.Effectiveness)
			}
		}
	}
}

func rankedSummary(rows []contributeEffectiveModelRow) string {
	out := ""
	for _, r := range rows {
		out += r.Model + "(" + strconv.FormatFloat(r.Score, 'f', 3, 64) + ") "
	}
	return out
}
