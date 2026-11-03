package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
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
	Window       string                       `json:"window"`
	MinMergedPRs int                          `json:"min_merged_prs"`
	Total        int                          `json:"total"`
	Buckets      []governorPRModelBucket      `json:"buckets"`
	Unknown      int                          `json:"unknown"`
	MostReworked []governorPRMostReworkedItem `json:"most_reworked,omitempty"`
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
}

// governorPRModelSource is one raw attribution spelling folded into a bucket.
type governorPRModelSource struct {
	Model   string `json:"model"`
	Backend string `json:"backend"`
	PRs     int    `json:"prs"`
	Merged  int    `json:"merged"`
}

type governorPRModelRework struct {
	SamplePRs            int     `json:"sample_prs"`
	FirstPassMerged      int     `json:"first_pass_merged"`
	FirstPassMergeRate   float64 `json:"first_pass_merge_rate"`
	AvgReviewRounds      float64 `json:"avg_review_rounds"`
	WorstReviewRounds    int     `json:"worst_review_rounds"`
	AvgFixAttempts       float64 `json:"avg_fix_attempts"`
	WorstFixAttempts     int     `json:"worst_fix_attempts"`
	AvgFollowUpCommits   float64 `json:"avg_follow_up_commits"`
	HumanChangeRequests  int     `json:"human_change_requests"`
	MedianTimeToMergeMin int     `json:"median_time_to_merge_minutes"`
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
	runs, _, _ := readEffectiveModelRuns(s.effectiveModelTaskRunLogPath(), governorPRModelsWindowDuration(window))
	effective := aggregateEffectiveModelsFromPRAggregation(resp, aggregateEffectiveRunStats(runs, effectiveModelsFilterAll), effectiveModelsFilterAll, minMergedPRs)
	applyGovernorPRModelEffectiveness(&resp, effective, minMergedPRs)
	w.Header().Set("Content-Type", "application/json")
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
	cutoff := time.Time{}
	switch window {
	case governorPRModelsWindow7d:
		cutoff = now.AddDate(0, 0, -governorPRModelsDays7)
	case governorPRModelsWindow30d:
		cutoff = now.AddDate(0, 0, -governorPRModelsDays30)
	}

	type bucketState struct {
		governorPRModelBucket
		agents   map[string]*governorPRModelAgentBucket
		sources  map[string]*governorPRModelSource
		backends map[string]bool
		rework   reworkAccumulator
	}
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
		if model == modelfamily.Unknown {
			resp.Unknown++
		}
	}

	resp.Buckets = make([]governorPRModelBucket, 0, len(buckets))
	for _, b := range buckets {
		b.Rework = b.rework.finish()
		b.Backends, b.Backend = joinedBackends(b.backends)
		for _, src := range b.sources {
			b.Sources = append(b.Sources, *src)
		}
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
		}
	}
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
