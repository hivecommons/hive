package dashboard

import (
	"net/http"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/modelfamily"
)

// WallRawModelRuns is one raw model spelling folded into a canonical
// run-stats row, with its own run count (#18718).
type WallRawModelRuns struct {
	Model string `json:"model"`
	Runs  int    `json:"runs"`
}

// runStatsGroupCanonical reports whether a run-stats request asked for
// group=canonical. Any other value, or none, keeps today's per-raw-model rows.
func runStatsGroupCanonical(r *http.Request) bool {
	return r != nil && strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("group")), "canonical")
}

// groupWallModelStatsByCanonical merges run-stats rows that share a canonical
// model id (modelfamily.Canonical, the rule PRs-by-model uses). Runs and the
// verified and failed counts are summed and the two rates recomputed from the
// sums; abandoned, scenarios and failure_kinds (#18721) are summed too, so a merged row says exactly what one row over all its spellings
// would. Model becomes the canonical id; RawModels lists each spelling with
// its own runs, most runs first. Output is sorted by model, like wallStatsList.
func groupWallModelStatsByCanonical(rows []WallModelStats) []WallModelStats {
	by := map[string]*WallModelStats{}
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		canonical := row.CanonicalModel
		if canonical == "" {
			canonical = modelfamily.Canonical(row.Model)
		}
		g := by[canonical]
		if g == nil {
			g = &WallModelStats{Model: canonical, CanonicalModel: canonical, RawModels: []WallRawModelRuns{}}
			by[canonical] = g
			order = append(order, canonical)
		}
		g.Runs += row.Runs
		g.verifiedRuns += row.verifiedRuns
		g.failedRuns += row.failedRuns
		g.Abandoned += row.Abandoned
		for sc, n := range row.Scenarios {
			if g.Scenarios == nil {
				g.Scenarios = map[string]int{}
			}
			g.Scenarios[sc] += n
		}
		g.FailureKinds.Environment += row.FailureKinds.Environment
		g.FailureKinds.Task += row.FailureKinds.Task
		g.FailureKinds.Unspecified += row.FailureKinds.Unspecified
		g.RawModels = append(g.RawModels, WallRawModelRuns{Model: row.Model, Runs: row.Runs})
	}
	out := make([]WallModelStats, 0, len(order))
	for _, canonical := range order {
		g := by[canonical]
		if g.Runs > 0 {
			g.VerifiedPRShare = float64(g.verifiedRuns) / float64(g.Runs)
			g.FailureRate = float64(g.failedRuns) / float64(g.Runs)
		}
		sort.SliceStable(g.RawModels, func(i, j int) bool {
			if g.RawModels[i].Runs != g.RawModels[j].Runs {
				return g.RawModels[i].Runs > g.RawModels[j].Runs
			}
			return g.RawModels[i].Model < g.RawModels[j].Model
		})
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Model) < strings.ToLower(out[j].Model) })
	return out
}
