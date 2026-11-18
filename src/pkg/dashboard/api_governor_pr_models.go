package dashboard

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/modelfamily"
)

const (
	governorPRModelsWindow7d  = "7d"
	governorPRModelsWindow30d = "30d"
	governorPRModelsWindowAll = "all"

	governorPRModelsDays7  = 7
	governorPRModelsDays30 = 30
)

type governorPRModelsResponse struct {
	Window       string                               `json:"window"`
	MinMergedPRs int                                  `json:"min_merged_prs"`
	Total        int                                  `json:"total"`
	Buckets      []governorPRModelBucket              `json:"buckets"`
	Unknown      int                                  `json:"unknown"`
	MostReworked []governorPRMostReworkedItem         `json:"most_reworked,omitempty"`
	FixerQueue   map[string]governor.FixerQueueStatus `json:"fixer_queue,omitempty"`
	// Coverage discloses how much history the counts rest on (same shape as
	// /api/contribute/effective-models). EarliestPRCreatedAt is the oldest
	// created_at among the PRs counted, omitted when there are none.
	Coverage            contributeEffectiveModelsCoverage `json:"coverage"`
	EarliestPRCreatedAt string                            `json:"earliest_pr_created_at,omitempty"`
	// HiveCostCoverage describes the launch-cost snapshot behind each
	// bucket's hive_cost (#18717). Always present; Ready=false means no
	// bucket carries hive_cost.
	HiveCostCoverage governorPRModelsHiveCostCoverage `json:"hive_cost_coverage"`
}

// governorPRModelBucket is one canonical model (modelfamily.Canonical folds
// "openai-codex/gpt-6.1-sol", "gpt-6.1-sol" and "gpt-6.1-sol:high" into one
// row). Backend lists the distinct backends that drove it, comma-joined;
// Sources keeps the raw model/backend spellings and their counts so the
// dashboard can show them in a tooltip.
type governorPRModelBucket struct {
	Model          string                       `json:"model"`
	Backend        string                       `json:"backend"`
	Backends       []string                     `json:"backends,omitempty"`
	Sources        []governorPRModelSource      `json:"sources,omitempty"`
	PRs            int                          `json:"prs"`
	Merged         int                          `json:"merged"`
	ClosedUnmerged int                          `json:"closed_unmerged"`
	Open           int                          `json:"open"`
	Rework         governorPRModelRework        `json:"rework"`
	Effectiveness  governorPRModelEffectiveness `json:"effectiveness"`
	Agents         []governorPRModelAgentBucket `json:"agents,omitempty"`
	// Efforts splits the bucket by the trailer's effort= value ("unknown"
	// when absent), sorted by PRs descending (hivecommons/hive#18715).
	Efforts []governorPRModelEffort `json:"efforts,omitempty"`
	// HiveCost is what this model's PRs cost the hive to review and repair
	// (#18717); omitted when the launch-cost snapshot is not ready.
	HiveCost *governorPRModelHiveCost `json:"hive_cost,omitempty"`

	// ClosedUnmergedReasons splits ClosedUnmerged by why the PR was closed
	// (#18719); the three counts always sum to ClosedUnmerged.
	ClosedUnmergedReasons governorPRModelCloseReasons `json:"closed_unmerged_reasons"`
	// Repos splits the bucket by canonical owner/repo (effectivePRRepo), PRs
	// descending. Weeks splits it by ISO week (Monday, UTC) of created_at,
	// oldest first, and is omitted for window=7d. Rework figures in both
	// cover merged PRs only, as Rework does.
	Repos []governorPRModelRepoBucket `json:"repos,omitempty"`
	Weeks []governorPRModelWeekBucket `json:"weeks,omitempty"`
}

// governorPRModelRepoBucket is one repository's share of a model bucket.
type governorPRModelRepoBucket struct {
	Repo               string  `json:"repo"`
	PRs                int     `json:"prs"`
	Merged             int     `json:"merged"`
	ClosedUnmerged     int     `json:"closed_unmerged"`
	Open               int     `json:"open"`
	FirstPassMerged    int     `json:"first_pass_merged"`
	FirstPassMergeRate float64 `json:"first_pass_merge_rate"`
	AvgReviewRounds    float64 `json:"avg_review_rounds"`
	AvgFixAttempts     float64 `json:"avg_fix_attempts"`
}

// governorPRModelWeekBucket is one ISO week's share of a model bucket.
// WeekStart is the Monday (UTC) as YYYY-MM-DD.
type governorPRModelWeekBucket struct {
	WeekStart          string  `json:"week_start"`
	PRs                int     `json:"prs"`
	Merged             int     `json:"merged"`
	FirstPassMerged    int     `json:"first_pass_merged"`
	FirstPassMergeRate float64 `json:"first_pass_merge_rate"`
	AvgReviewRounds    float64 `json:"avg_review_rounds"`
	AvgFixAttempts     float64 `json:"avg_fix_attempts"`
}

// governorPRModelWeekStart returns the Monday 00:00 UTC that starts t's ISO
// week, as YYYY-MM-DD.
func governorPRModelWeekStart(t time.Time) string {
	t = t.UTC()
	offset := (int(t.Weekday()) + 6) % 7 // Monday = 0
	return time.Date(t.Year(), t.Month(), t.Day()-offset, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

// governorPRModelRepoSlices finishes the per-repo slices, PRs descending.
func governorPRModelRepoSlices(repos map[string]*governorPRModelSliceState, names map[string]string) []governorPRModelRepoBucket {
	out := make([]governorPRModelRepoBucket, 0, len(repos))
	for key, st := range repos {
		rw := st.rework.finish()
		out = append(out, governorPRModelRepoBucket{
			Repo: names[key], PRs: st.prs, Merged: st.merged, ClosedUnmerged: st.closedUnmerged, Open: st.open,
			FirstPassMerged: rw.FirstPassMerged, FirstPassMergeRate: rw.FirstPassMergeRate,
			AvgReviewRounds: rw.AvgReviewRounds, AvgFixAttempts: rw.AvgFixAttempts,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PRs != out[j].PRs {
			return out[i].PRs > out[j].PRs
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

// governorPRModelWeekSlices finishes the per-week slices, oldest first.
func governorPRModelWeekSlices(weeks map[string]*governorPRModelSliceState) []governorPRModelWeekBucket {
	out := make([]governorPRModelWeekBucket, 0, len(weeks))
	for week, st := range weeks {
		rw := st.rework.finish()
		out = append(out, governorPRModelWeekBucket{
			WeekStart: week, PRs: st.prs, Merged: st.merged,
			FirstPassMerged: rw.FirstPassMerged, FirstPassMergeRate: rw.FirstPassMergeRate,
			AvgReviewRounds: rw.AvgReviewRounds, AvgFixAttempts: rw.AvgFixAttempts,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WeekStart < out[j].WeekStart })
	return out
}

// governorPRModelSource is one raw attribution spelling folded into a bucket.
// The outcome and rework fields are computed over this source's PRs the same
// way the bucket's Rework is computed over merged PRs (hivecommons/hive#18715).
type governorPRModelSource struct {
	Model   string `json:"model"`
	Backend string `json:"backend"`
	PRs     int    `json:"prs"`
	Merged  int    `json:"merged"`
	governorPRModelSliceStats
}

// governorPRModelEffort is one effort= value within a bucket.
type governorPRModelEffort struct {
	Effort string `json:"effort"`
	PRs    int    `json:"prs"`
	Merged int    `json:"merged"`
	governorPRModelSliceStats
}

// governorPRModelSliceStats are the outcome and rework figures shared by a
// bucket's per-source and per-effort slices.
type governorPRModelSliceStats struct {
	ClosedUnmerged     int     `json:"closed_unmerged"`
	Open               int     `json:"open"`
	SamplePRs          int     `json:"sample_prs"`
	FirstPassMerged    int     `json:"first_pass_merged"`
	FirstPassMergeRate float64 `json:"first_pass_merge_rate"`
	AvgReviewRounds    float64 `json:"avg_review_rounds"`
	AvgFixAttempts     float64 `json:"avg_fix_attempts"`
}

// governorPRModelSliceState accumulates one per-source or per-effort slice.
type governorPRModelSliceState struct {
	prs, merged, closedUnmerged, open int
	rework                            reworkAccumulator
}

func (st *governorPRModelSliceState) add(outcome string, rework ghpkg.PRReworkStats) {
	addOutcome(&st.prs, &st.merged, &st.closedUnmerged, &st.open, outcome)
	if outcome == "merged" {
		st.rework.add(rework)
	}
}

func (st *governorPRModelSliceState) finish() governorPRModelSliceStats {
	r := st.rework.finish()
	return governorPRModelSliceStats{
		ClosedUnmerged:     st.closedUnmerged,
		Open:               st.open,
		SamplePRs:          r.SamplePRs,
		FirstPassMerged:    r.FirstPassMerged,
		FirstPassMergeRate: r.FirstPassMergeRate,
		AvgReviewRounds:    r.AvgReviewRounds,
		AvgFixAttempts:     r.AvgFixAttempts,
	}
}

type governorPRModelRework struct {
	SamplePRs          int     `json:"sample_prs"`
	FirstPassMerged    int     `json:"first_pass_merged"`
	FirstPassMergeRate float64 `json:"first_pass_merge_rate"`
	// FirstPassMergeRateCI is the Wilson 95% interval [low, high]; omitted
	// when no merged PRs were sampled.
	FirstPassMergeRateCI []float64 `json:"first_pass_merge_rate_ci,omitempty"`
	AvgReviewRounds      float64   `json:"avg_review_rounds"`
	WorstReviewRounds    int       `json:"worst_review_rounds"`
	AvgFixAttempts       float64   `json:"avg_fix_attempts"`
	WorstFixAttempts     int       `json:"worst_fix_attempts"`
	AvgFollowUpCommits   float64   `json:"avg_follow_up_commits"`
	HumanChangeRequests  int       `json:"human_change_requests"`
	MedianTimeToMergeMin int       `json:"median_time_to_merge_minutes"`
}

type governorPRModelEffectiveness struct {
	Rank               int     `json:"rank,omitempty"`
	Eligible           bool    `json:"eligible"`
	Unattributed       bool    `json:"unattributed,omitempty"`
	Score              float64 `json:"score"`
	MergedPRs          int     `json:"merged_prs"`
	FirstPassMerged    int     `json:"first_pass_merged"`
	FirstPassMergeRate float64 `json:"first_pass_merge_rate"`
	RunCount           int     `json:"runs"`
	VerifiedPRRunRate  float64 `json:"verified_pr_run_rate"`
	FailureRate        float64 `json:"failure_rate"`
	NothingToShipRate  float64 `json:"nothing_to_ship_rate"`
	// Wilson 95% intervals [low, high]; omitted when the denominator is 0
	// (merged PRs for the first-pass rate, runs for the three run rates).
	FirstPassMergeRateCI []float64 `json:"first_pass_merge_rate_ci,omitempty"`
	VerifiedPRRunRateCI  []float64 `json:"verified_pr_run_rate_ci,omitempty"`
	FailureRateCI        []float64 `json:"failure_rate_ci,omitempty"`
	NothingToShipRateCI  []float64 `json:"nothing_to_ship_rate_ci,omitempty"`
}

type governorPRMostReworkedItem struct {
	Repo                string   `json:"repo"`
	Number              int      `json:"number"`
	Title               string   `json:"title,omitempty"`
	Model               string   `json:"model"`
	Backend             string   `json:"backend"`
	URL                 string   `json:"url,omitempty"`
	ReviewRounds        int      `json:"review_rounds"`
	FixAttempts         int      `json:"fix_attempts"`
	FollowUpCommits     int      `json:"follow_up_commits"`
	FixerModels         []string `json:"fixer_models,omitempty"`
	HumanChangeRequests int      `json:"human_change_requests,omitempty"`
}

type governorPRModelAgentBucket struct {
	Agent          string `json:"agent"`
	Backend        string `json:"backend"`
	PRs            int    `json:"prs"`
	Merged         int    `json:"merged"`
	ClosedUnmerged int    `json:"closed_unmerged"`
	Open           int    `json:"open"`
}

func (s *Server) handleGovernorPRModels(w http.ResponseWriter, r *http.Request) {
	window := normalizeGovernorPRModelsWindow(r.URL.Query().Get("window"))
	minMergedPRs := effectiveModelsMinMergedPRs()
	actionable := s.lastActionableForPRModels()
	resp := aggregateGovernorPRModels(actionable.PRs.Attributed, window, time.Now())
	applyGovernorPRModelCloseReasons(&resp, actionable.PRs.Attributed, governorPRModelsSupersededKeys(actionable.PRs.Attributed, actionable.PRs.Items), time.Now())
	resp.FixerQueue = s.governorFixerQueue()
	now := time.Now()
	runs, rotated, _ := readEffectiveModelRuns(s.effectiveModelTaskRunLogPath(), governorPRModelsWindowDuration(window))
	resp.Coverage = effectiveModelsCoverage(governorPRModelsWindowDuration(window), ghpkg.AttributedClosedPRLookback(), runs, rotated, now)
	effective := aggregateEffectiveModelsFromPRAggregation(resp, aggregateEffectiveRunStats(runs, effectiveModelsFilterAll), effectiveModelsFilterAll, minMergedPRs)
	applyGovernorPRModelEffectiveness(&resp, effective, minMergedPRs)
	launchCost, launchCostReady := s.launchCostSnapshotForPRModels()
	applyGovernorPRModelHiveCost(&resp, actionable.PRs.Attributed, launchCost, launchCostReady, now)
	w.Header().Set("Content-Type", "application/json")
	if model, ok := governorPRModelsQueryModel(r); ok {
		_ = json.NewEncoder(w).Encode(withGovernorPRModelPRs(resp, actionable.PRs.Attributed, model, now))
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) lastActionableForPRModels() *ghpkg.ActionableResult {
	if s == nil || s.deps == nil || s.deps.Scheduler == nil {
		return &ghpkg.ActionableResult{}
	}
	if a := s.deps.Scheduler.GetLastActionable(); a != nil {
		return a
	}
	return &ghpkg.ActionableResult{}
}

func normalizeGovernorPRModelsWindow(window string) string {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case governorPRModelsWindow30d:
		return governorPRModelsWindow30d
	case governorPRModelsWindowAll:
		return governorPRModelsWindowAll
	default:
		return governorPRModelsWindow7d
	}
}

func aggregateGovernorPRModels(prs []ghpkg.PullRequest, window string, now time.Time) governorPRModelsResponse {
	window = normalizeGovernorPRModelsWindow(window)
	cutoff := governorPRModelsCutoff(window, now)

	type bucketState struct {
		governorPRModelBucket
		agents   map[string]*governorPRModelAgentBucket
		sources  map[string]*governorPRModelSource
		backends map[string]bool
		rework   reworkAccumulator
		// Per-source and per-effort slice accumulators (#18715).
		sourceStats map[string]*governorPRModelSliceState
		efforts     map[string]*governorPRModelSliceState
		// Per-repo and per-week slices (#18716); repoNames keeps the first
		// spelling seen for each case-folded repo key.
		repos     map[string]*governorPRModelSliceState
		repoNames map[string]string
		weeks     map[string]*governorPRModelSliceState
	}
	withWeeks := window != governorPRModelsWindow7d
	buckets := map[string]*bucketState{}
	resp := governorPRModelsResponse{Window: window, MinMergedPRs: effectiveModelsMinMergedPRs()}
	for _, pr := range prs {
		if !pr.HiveAttributed {
			continue
		}
		if !cutoff.IsZero() && pr.CreatedAt.Before(cutoff) {
			continue
		}
		rawModel := ghpkg.NormalizeAttributionModel(pr.HiveModel)
		model := modelfamily.Canonical(pr.HiveModel)
		backend := ghpkg.NormalizeAttributionValue(pr.HiveBackend)
		agent := ghpkg.NormalizeAttributionValue(pr.HiveAgent)
		if agent == "unknown" {
			agent = ""
		}
		b := buckets[model]
		if b == nil {
			b = &bucketState{
				governorPRModelBucket: governorPRModelBucket{Model: model},
				agents:                map[string]*governorPRModelAgentBucket{},
				sources:               map[string]*governorPRModelSource{},
				backends:              map[string]bool{},
				sourceStats:           map[string]*governorPRModelSliceState{},
				efforts:               map[string]*governorPRModelSliceState{},
				repos:                 map[string]*governorPRModelSliceState{},
				repoNames:             map[string]string{},
				weeks:                 map[string]*governorPRModelSliceState{},
			}
			buckets[model] = b
		}
		b.backends[backend] = true
		outcome := governorPRModelOutcome(pr)
		addOutcome(&b.PRs, &b.Merged, &b.ClosedUnmerged, &b.Open, outcome)
		srcKey := rawModel + "\x00" + backend
		src := b.sources[srcKey]
		if src == nil {
			src = &governorPRModelSource{Model: rawModel, Backend: backend}
			b.sources[srcKey] = src
		}
		src.PRs++
		if outcome == "merged" {
			src.Merged++
		}
		if outcome == "merged" {
			b.rework.add(pr.Rework)
		}
		governorPRModelSlice(b.sourceStats, srcKey).add(outcome, pr.Rework)
		governorPRModelSlice(b.efforts, governorPRModelEffortKey(pr.HiveEffort)).add(outcome, pr.Rework)
		repoName := strings.TrimSpace(effectivePRRepo(pr))
		repoKey := strings.ToLower(repoName)
		if _, ok := b.repoNames[repoKey]; !ok {
			b.repoNames[repoKey] = repoName
		}
		governorPRModelSlice(b.repos, repoKey).add(outcome, pr.Rework)
		if withWeeks && !pr.CreatedAt.IsZero() {
			governorPRModelSlice(b.weeks, governorPRModelWeekStart(pr.CreatedAt)).add(outcome, pr.Rework)
		}
		if score := pr.Rework.ReviewRounds + pr.Rework.FixAttempts; score > 0 {
			resp.MostReworked = append(resp.MostReworked, governorPRMostReworkedItem{
				Repo:                pr.Repo,
				Number:              pr.Number,
				Title:               pr.Title,
				Model:               model,
				Backend:             backend,
				URL:                 pr.URL,
				ReviewRounds:        pr.Rework.ReviewRounds,
				FixAttempts:         pr.Rework.FixAttempts,
				FollowUpCommits:     pr.Rework.FollowUpCommits,
				FixerModels:         append([]string(nil), pr.Rework.FixerModels...),
				HumanChangeRequests: pr.Rework.HumanChangeRequests,
			})
		}
		agentKey := agent + "\x00" + backend
		ab := b.agents[agentKey]
		if ab == nil {
			ab = &governorPRModelAgentBucket{Agent: agent, Backend: backend}
			b.agents[agentKey] = ab
		}
		addOutcome(&ab.PRs, &ab.Merged, &ab.ClosedUnmerged, &ab.Open, outcome)
		resp.Total++
		if !pr.CreatedAt.IsZero() {
			if ts := pr.CreatedAt.UTC().Format(time.RFC3339); resp.EarliestPRCreatedAt == "" || ts < resp.EarliestPRCreatedAt {
				resp.EarliestPRCreatedAt = ts
			}
		}
		if model == modelfamily.Unknown {
			resp.Unknown++
		}
	}

	resp.Buckets = make([]governorPRModelBucket, 0, len(buckets))
	for _, b := range buckets {
		b.Rework = b.rework.finish()
		b.Repos = governorPRModelRepoSlices(b.repos, b.repoNames)
		if withWeeks && len(b.weeks) > 0 {
			b.Weeks = governorPRModelWeekSlices(b.weeks)
		}
		b.Backends, b.Backend = joinedBackends(b.backends)
		for key, src := range b.sources {
			if st := b.sourceStats[key]; st != nil {
				src.governorPRModelSliceStats = st.finish()
			}
			b.Sources = append(b.Sources, *src)
		}
		b.Efforts = finishGovernorPRModelEfforts(b.efforts)
		sort.Slice(b.Sources, func(i, j int) bool {
			if b.Sources[i].PRs != b.Sources[j].PRs {
				return b.Sources[i].PRs > b.Sources[j].PRs
			}
			if b.Sources[i].Model != b.Sources[j].Model {
				return b.Sources[i].Model < b.Sources[j].Model
			}
			return b.Sources[i].Backend < b.Sources[j].Backend
		})
		for _, ab := range b.agents {
			b.Agents = append(b.Agents, *ab)
		}
		sort.Slice(b.Agents, func(i, j int) bool {
			if b.Agents[i].PRs != b.Agents[j].PRs {
				return b.Agents[i].PRs > b.Agents[j].PRs
			}
			return b.Agents[i].Agent < b.Agents[j].Agent
		})
		resp.Buckets = append(resp.Buckets, b.governorPRModelBucket)
	}
	sort.Slice(resp.Buckets, func(i, j int) bool {
		if resp.Buckets[i].PRs != resp.Buckets[j].PRs {
			return resp.Buckets[i].PRs > resp.Buckets[j].PRs
		}
		if resp.Buckets[i].Model != resp.Buckets[j].Model {
			return resp.Buckets[i].Model < resp.Buckets[j].Model
		}
		return resp.Buckets[i].Backend < resp.Buckets[j].Backend
	})
	sort.Slice(resp.MostReworked, func(i, j int) bool {
		a := resp.MostReworked[i].ReviewRounds + resp.MostReworked[i].FixAttempts
		b := resp.MostReworked[j].ReviewRounds + resp.MostReworked[j].FixAttempts
		if a != b {
			return a > b
		}
		if resp.MostReworked[i].FollowUpCommits != resp.MostReworked[j].FollowUpCommits {
			return resp.MostReworked[i].FollowUpCommits > resp.MostReworked[j].FollowUpCommits
		}
		return resp.MostReworked[i].Repo < resp.MostReworked[j].Repo
	})
	if len(resp.MostReworked) > 10 {
		resp.MostReworked = resp.MostReworked[:10]
	}
	return resp
}

// governorPRModelSlice returns the accumulator for key, creating it.
func governorPRModelSlice(m map[string]*governorPRModelSliceState, key string) *governorPRModelSliceState {
	st := m[key]
	if st == nil {
		st = &governorPRModelSliceState{}
		m[key] = st
	}
	return st
}

// governorPRModelEffortKey normalises a PR's effort= value; absent is "unknown".
func governorPRModelEffortKey(effort string) string {
	return ghpkg.NormalizeAttributionValue(effort)
}

// finishGovernorPRModelEfforts renders the per-effort slices sorted by PRs
// descending, then effort name.
func finishGovernorPRModelEfforts(m map[string]*governorPRModelSliceState) []governorPRModelEffort {
	out := make([]governorPRModelEffort, 0, len(m))
	for effort, st := range m {
		out = append(out, governorPRModelEffort{Effort: effort, PRs: st.prs, Merged: st.merged, governorPRModelSliceStats: st.finish()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PRs != out[j].PRs {
			return out[i].PRs > out[j].PRs
		}
		return out[i].Effort < out[j].Effort
	})
	return out
}

// joinedBackends returns the sorted distinct backends and their comma-joined
// display form ("codex, pi").
func joinedBackends(set map[string]bool) ([]string, string) {
	out := make([]string, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	sort.Strings(out)
	return out, strings.Join(out, ", ")
}

func applyGovernorPRModelEffectiveness(resp *governorPRModelsResponse, effective contributeEffectiveModelsResponse, minMergedPRs int) {
	if resp == nil {
		return
	}
	resp.MinMergedPRs = minMergedPRs
	rows := map[string]contributeEffectiveModelRow{}
	for _, row := range effective.Ranked {
		rows[row.Model] = row
	}
	for _, row := range effective.Insufficient {
		rows[row.Model] = row
	}
	for i := range resp.Buckets {
		row, ok := rows[resp.Buckets[i].Model]
		if !ok {
			continue
		}
		resp.Buckets[i].Effectiveness = governorPRModelEffectiveness{
			Rank:               row.EffectivenessRank,
			Eligible:           row.EffectivenessRank > 0,
			Unattributed:       row.Unattributed,
			Score:              row.Score,
			MergedPRs:          row.MergedPRs,
			FirstPassMerged:    row.FirstPassMerged,
			FirstPassMergeRate: row.FirstPassMergeRate,
			RunCount:           row.RunCount,
			VerifiedPRRunRate:  row.VerifiedPRRunRate,
			FailureRate:        row.FailureRate,
			NothingToShipRate:  row.NothingToShipRate,

			FirstPassMergeRateCI: wilsonCI(row.FirstPassMerged, row.MergedPRs),
			VerifiedPRRunRateCI:  wilsonCI(rateCount(row.VerifiedPRRunRate, row.RunCount), row.RunCount),
			FailureRateCI:        wilsonCI(rateCount(row.FailureRate, row.RunCount), row.RunCount),
			NothingToShipRateCI:  wilsonCI(rateCount(row.NothingToShipRate, row.RunCount), row.RunCount),
		}
	}
}

// rateCount recovers the integer numerator behind rate = k/n.
func rateCount(rate float64, n int) int {
	return int(math.Round(rate * float64(n)))
}

type reworkAccumulator struct {
	sample             int
	firstPass          int
	reviewRounds       int
	worstReviewRounds  int
	fixAttempts        int
	worstFixAttempts   int
	followUpCommits    int
	humanChanges       int
	timeToMergeMinutes []int
}

func (a *reworkAccumulator) add(r ghpkg.PRReworkStats) {
	a.sample++
	if r.FirstPass {
		a.firstPass++
	}
	a.reviewRounds += r.ReviewRounds
	if r.ReviewRounds > a.worstReviewRounds {
		a.worstReviewRounds = r.ReviewRounds
	}
	a.fixAttempts += r.FixAttempts
	if r.FixAttempts > a.worstFixAttempts {
		a.worstFixAttempts = r.FixAttempts
	}
	a.followUpCommits += r.FollowUpCommits
	a.humanChanges += r.HumanChangeRequests
	if r.TimeToMergeMinutes > 0 {
		a.timeToMergeMinutes = append(a.timeToMergeMinutes, r.TimeToMergeMinutes)
	}
}

func (a reworkAccumulator) finish() governorPRModelRework {
	out := governorPRModelRework{
		FirstPassMergeRateCI: wilsonCI(a.firstPass, a.sample),
		SamplePRs:            a.sample,
		FirstPassMerged:      a.firstPass,
		WorstReviewRounds:    a.worstReviewRounds,
		WorstFixAttempts:     a.worstFixAttempts,
		HumanChangeRequests:  a.humanChanges,
		MedianTimeToMergeMin: medianInt(a.timeToMergeMinutes),
	}
	if a.sample > 0 {
		out.FirstPassMergeRate = float64(a.firstPass) / float64(a.sample)
		out.AvgReviewRounds = float64(a.reviewRounds) / float64(a.sample)
		out.AvgFixAttempts = float64(a.fixAttempts) / float64(a.sample)
		out.AvgFollowUpCommits = float64(a.followUpCommits) / float64(a.sample)
	}
	return out
}

func medianInt(values []int) int {
	if len(values) == 0 {
		return 0
	}
	cp := append([]int(nil), values...)
	sort.Ints(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

func governorPRModelOutcome(pr ghpkg.PullRequest) string {
	if !pr.MergedAt.IsZero() {
		return "merged"
	}
	if strings.EqualFold(pr.State, "closed") || !pr.ClosedAt.IsZero() {
		return "closed_unmerged"
	}
	return "open"
}

func addOutcome(total, merged, closedUnmerged, open *int, outcome string) {
	(*total)++
	switch outcome {
	case "merged":
		(*merged)++
	case "closed_unmerged":
		(*closedUnmerged)++
	default:
		(*open)++
	}
}
