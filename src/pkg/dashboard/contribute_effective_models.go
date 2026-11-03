package dashboard

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/modelfamily"
)

const (
	effectiveModelsDefaultMinMergedPRs = 5
	// effectiveModelsWilsonZ is the 95% two-sided normal quantile used for
	// the Wilson lower bound on first-pass merge rate.
	effectiveModelsWilsonZ   = 1.96
	effectiveModelsMinPRsEnv = "HIVE_CONTRIBUTE_EFFECTIVE_MODELS_MIN_PRS"

	effectiveModelsFilterAll         = "all"
	effectiveModelsFilterContributor = "contributor"
	effectiveModelsFilterHive        = "hive"
)

type contributeEffectiveModelsResponse struct {
	Window       string                            `json:"window"`
	Filter       string                            `json:"filter"`
	MinMergedPRs int                               `json:"min_merged_prs"`
	Ranked       []contributeEffectiveModelRow     `json:"ranked"`
	Insufficient []contributeEffectiveModelRow     `json:"insufficient"`
	Coverage     contributeEffectiveModelsCoverage `json:"coverage"`
}

// contributeEffectiveModelsCoverage reports how much history fed the table.
// Closed attributed PRs come from a bounded rolling scan and runs from the
// task-run log plus its single rotated predecessor, so a 30d/All window can
// ask for more than is retained; the *_partial flags say so explicitly.
type contributeEffectiveModelsCoverage struct {
	ClosedPRLookbackDays int    `json:"closed_pr_lookback_days"`
	ClosedPRsPartial     bool   `json:"closed_prs_partial"`
	RunsSince            string `json:"runs_since,omitempty"`
	RunsPartial          bool   `json:"runs_partial"`
}

// contributeEffectiveModelRow is one canonical model. Score is the rank key:
// the Wilson 95% lower bound of first-pass merges over merged PRs, multiplied
// by (1 − failure rate) and (1 − nothing-to-ship rate) from the run log (a
// row with no runs keeps the bound as-is). Unattributed rows (model unknown)
// are never ranked.
type contributeEffectiveModelRow struct {
	Model              string                       `json:"model"`
	Backend            string                       `json:"backend"`
	Backends           []string                     `json:"backends,omitempty"`
	Sources            []governorPRModelSource      `json:"sources,omitempty"`
	Runtime            string                       `json:"runtime"`
	EffectivenessRank  int                          `json:"effectiveness_rank,omitempty"`
	Unattributed       bool                         `json:"unattributed,omitempty"`
	Score              float64                      `json:"score"`
	PRs                int                          `json:"prs"`
	MergedPRs          int                          `json:"merged_prs"`
	FirstPassMerged    int                          `json:"first_pass_merged"`
	FirstPassMergeRate float64                      `json:"first_pass_merge_rate"`
	AvgReviewRounds    float64                      `json:"avg_review_rounds"`
	AvgFixAttempts     float64                      `json:"avg_fix_attempts"`
	RunCount           int                          `json:"runs"`
	VerifiedPRRuns     int                          `json:"verified_pr_runs"`
	VerifiedPRRunRate  float64                      `json:"verified_pr_run_rate"`
	FailedRuns         int                          `json:"failed_runs"`
	FailureRate        float64                      `json:"failure_rate"`
	NothingToShipRuns  int                          `json:"nothing_to_ship_runs"`
	NothingToShipRate  float64                      `json:"nothing_to_ship_rate"`
	MostReworked       []governorPRMostReworkedItem `json:"most_reworked,omitempty"`
}

type effectiveRunStats struct {
	Runs              int
	VerifiedPRRuns    int
	FailedRuns        int
	NothingToShipRuns int
	backends          map[string]bool
	sources           map[string]*governorPRModelSource
}

func (s *Server) handleContributeEffectiveModels(w http.ResponseWriter, r *http.Request) {
	window := normalizeGovernorPRModelsWindow(r.URL.Query().Get("window"))
	filter := normalizeEffectiveModelsFilter(r.URL.Query().Get("filter"))
	minMergedPRs := effectiveModelsMinMergedPRs()
	if v := r.URL.Query().Get("min_merged_prs"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 1000 {
			minMergedPRs = n
		}
	}

	prs := s.lastActionableForPRModels().PRs.Attributed
	windowDur := governorPRModelsWindowDuration(window)
	runs, rotated, _ := readEffectiveModelRuns(s.effectiveModelTaskRunLogPath(), windowDur)
	now := time.Now()
	resp := aggregateContributeEffectiveModels(prs, runs, window, filter, minMergedPRs, now)
	resp.Coverage = effectiveModelsCoverage(windowDur, ghpkg.AttributedClosedPRLookback(), runs, rotated, now)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func effectiveModelsMinMergedPRs() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(effectiveModelsMinPRsEnv))); err == nil && n >= 1 && n <= 1000 {
		return n
	}
	return effectiveModelsDefaultMinMergedPRs
}

func normalizeEffectiveModelsFilter(filter string) string {
	switch strings.ToLower(strings.TrimSpace(filter)) {
	case effectiveModelsFilterContributor:
		return effectiveModelsFilterContributor
	case effectiveModelsFilterHive:
		return effectiveModelsFilterHive
	default:
		return effectiveModelsFilterAll
	}
}

func governorPRModelsWindowDuration(window string) time.Duration {
	switch normalizeGovernorPRModelsWindow(window) {
	case governorPRModelsWindow7d:
		return governorPRModelsDays7 * 24 * time.Hour
	case governorPRModelsWindow30d:
		return governorPRModelsDays30 * 24 * time.Hour
	default:
		return 0
	}
}

func (s *Server) effectiveModelTaskRunLogPath() string {
	if s != nil && s.contributeHub != nil {
		return s.contributeHub.taskRunLogPath()
	}
	if s != nil {
		return filepath.Join(s.contributorsDirOrDefault(), taskRunLogFileName)
	}
	return taskRunLogPath
}

// readEffectiveModelRuns reads the rotated ".1" predecessor and the live
// task-run log (oldest first) so 30d/All windows see every retained run, not
// just the records written since the last rotation. rotated reports whether a
// rotation has happened, i.e. whether older history may have been discarded.
func readEffectiveModelRuns(path string, window time.Duration) ([]TaskRunRecord, bool, error) {
	taskRunMu.Lock()
	prev, prevErr := os.ReadFile(path + ".1")
	data, err := os.ReadFile(path)
	taskRunMu.Unlock()
	rotated := prevErr == nil
	if prevErr != nil && !os.IsNotExist(prevErr) {
		return nil, false, prevErr
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, rotated, err
	}
	cutoff := ""
	if window > 0 {
		cutoff = time.Now().UTC().Add(-window).Format(time.RFC3339)
	}
	var out []TaskRunRecord
	for _, line := range strings.Split(string(prev)+"\n"+string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec TaskRunRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if cutoff != "" && rec.TS < cutoff {
			continue
		}
		out = append(out, rec)
	}
	return out, rotated, nil
}

// effectiveModelsCoverage describes the history behind a response. Closed PRs
// are partial whenever the window (All = unbounded) reaches past the scan's
// lookback. Runs are partial only once the log has rotated and the oldest
// retained run is newer than the window start.
func effectiveModelsCoverage(window, closedPRLookback time.Duration, runs []TaskRunRecord, rotated bool, now time.Time) contributeEffectiveModelsCoverage {
	cov := contributeEffectiveModelsCoverage{
		ClosedPRLookbackDays: int(closedPRLookback / (24 * time.Hour)),
		ClosedPRsPartial:     window <= 0 || window > closedPRLookback,
	}
	for _, rec := range runs {
		if rec.TS != "" && (cov.RunsSince == "" || rec.TS < cov.RunsSince) {
			cov.RunsSince = rec.TS
		}
	}
	if rotated {
		if window <= 0 || cov.RunsSince == "" {
			cov.RunsPartial = true
		} else {
			cov.RunsPartial = cov.RunsSince > now.UTC().Add(-window).Format(time.RFC3339)
		}
	}
	return cov
}

func aggregateContributeEffectiveModels(prs []ghpkg.PullRequest, runs []TaskRunRecord, window, filter string, minMergedPRs int, now time.Time) contributeEffectiveModelsResponse {
	window = normalizeGovernorPRModelsWindow(window)
	filter = normalizeEffectiveModelsFilter(filter)
	if minMergedPRs < 1 {
		minMergedPRs = effectiveModelsDefaultMinMergedPRs
	}
	contributorPRs := contributorPRKeysFromRuns(runs)
	filtered := make([]ghpkg.PullRequest, 0, len(prs))
	for _, pr := range prs {
		key := effectivePRKey(effectivePRRepo(pr), pr.Number)
		isContributor := contributorPRs[key]
		if filter == effectiveModelsFilterContributor && !isContributor {
			continue
		}
		if filter == effectiveModelsFilterHive && isContributor {
			continue
		}
		filtered = append(filtered, pr)
	}
	prAgg := aggregateGovernorPRModels(filtered, window, now)
	return aggregateEffectiveModelsFromPRAggregation(prAgg, aggregateEffectiveRunStats(runs, filter), filter, minMergedPRs)
}

func aggregateEffectiveRunStats(runs []TaskRunRecord, filter string) map[string]*effectiveRunStats {
	runStats := map[string]*effectiveRunStats{}
	if normalizeEffectiveModelsFilter(filter) == effectiveModelsFilterHive {
		return runStats
	}
	for _, rec := range runs {
		rawModel := ghpkg.NormalizeAttributionModel(rec.Model)
		model := modelfamily.Canonical(rec.Model)
		backend := ghpkg.NormalizeAttributionValue(rec.Backend)
		st := runStats[model]
		if st == nil {
			st = &effectiveRunStats{backends: map[string]bool{}, sources: map[string]*governorPRModelSource{}}
			runStats[model] = st
		}
		st.backends[backend] = true
		srcKey := rawModel + "\x00" + backend
		if st.sources[srcKey] == nil {
			st.sources[srcKey] = &governorPRModelSource{Model: rawModel, Backend: backend}
		}
		st.Runs++
		if rec.PRVerified {
			st.VerifiedPRRuns++
		}
		if rec.Outcome == outcomeFailed {
			st.FailedRuns++
		}
		if rec.Outcome == outcomeCompleted && !rec.PRVerified {
			st.NothingToShipRuns++
		}
	}
	return runStats
}

func aggregateEffectiveModelsFromPRAggregation(prAgg governorPRModelsResponse, runStats map[string]*effectiveRunStats, filter string, minMergedPRs int) contributeEffectiveModelsResponse {
	filter = normalizeEffectiveModelsFilter(filter)
	if minMergedPRs < 1 {
		minMergedPRs = effectiveModelsDefaultMinMergedPRs
	}
	rows := map[string]*contributeEffectiveModelRow{}
	for _, b := range prAgg.Buckets {
		row := effectiveRowForModel(rows, b.Model)
		row.Backends = append([]string(nil), b.Backends...)
		row.Backend = b.Backend
		row.Sources = append([]governorPRModelSource(nil), b.Sources...)
		row.PRs = b.PRs
		row.MergedPRs = b.Merged
		row.FirstPassMerged = b.Rework.FirstPassMerged
		row.FirstPassMergeRate = b.Rework.FirstPassMergeRate
		row.AvgReviewRounds = b.Rework.AvgReviewRounds
		row.AvgFixAttempts = b.Rework.AvgFixAttempts
	}
	for model, st := range runStats {
		row := effectiveRowForModel(rows, model)
		if row.Backend == "" {
			row.Backends, row.Backend = joinedBackends(st.backends)
			for _, src := range st.sources {
				row.Sources = append(row.Sources, *src)
			}
			sort.Slice(row.Sources, func(i, j int) bool {
				if row.Sources[i].Model != row.Sources[j].Model {
					return row.Sources[i].Model < row.Sources[j].Model
				}
				return row.Sources[i].Backend < row.Sources[j].Backend
			})
		}
		row.RunCount = st.Runs
		row.VerifiedPRRuns = st.VerifiedPRRuns
		row.FailedRuns = st.FailedRuns
		row.NothingToShipRuns = st.NothingToShipRuns
		if st.Runs > 0 {
			row.VerifiedPRRunRate = float64(st.VerifiedPRRuns) / float64(st.Runs)
			row.FailureRate = float64(st.FailedRuns) / float64(st.Runs)
			row.NothingToShipRate = float64(st.NothingToShipRuns) / float64(st.Runs)
		}
	}
	for _, item := range prAgg.MostReworked {
		row := effectiveRowForModel(rows, item.Model)
		row.MostReworked = append(row.MostReworked, item)
	}

	resp := contributeEffectiveModelsResponse{Window: prAgg.Window, Filter: filter, MinMergedPRs: minMergedPRs}
	for _, row := range rows {
		row.Runtime = effectiveModelRuntimeForSources(row.Model, row.Backend, row.Sources)
		row.Unattributed = row.Model == modelfamily.Unknown
		row.Score = effectiveModelScore(*row)
		if row.MergedPRs >= minMergedPRs && !row.Unattributed {
			resp.Ranked = append(resp.Ranked, *row)
		} else {
			resp.Insufficient = append(resp.Insufficient, *row)
		}
	}
	sortEffectiveModelRows(resp.Ranked, true)
	sortEffectiveModelRows(resp.Insufficient, false)
	for i := range resp.Ranked {
		resp.Ranked[i].EffectivenessRank = i + 1
	}
	return resp
}

func effectiveRowForModel(rows map[string]*contributeEffectiveModelRow, model string) *contributeEffectiveModelRow {
	row := rows[model]
	if row == nil {
		row = &contributeEffectiveModelRow{Model: model}
		rows[model] = row
	}
	return row
}

// wilsonLowerBound is the lower end of the Wilson score interval for
// successes/n at effectiveModelsWilsonZ: 5/5 reads ~0.57, 21/22 ~0.78,
// 203/313 ~0.60, so a perfect score on a handful of PRs cannot out-rank a
// near-perfect score on many.
func wilsonLowerBound(successes, n int) float64 {
	if n <= 0 || successes <= 0 {
		return 0
	}
	if successes > n {
		successes = n
	}
	z := effectiveModelsWilsonZ
	nf := float64(n)
	p := float64(successes) / nf
	denom := 1 + z*z/nf
	centre := p + z*z/(2*nf)
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	return (centre - margin) / denom
}

// effectiveModelScore is the composite rank key: Wilson lower bound of
// first-pass merges over merged PRs × (1 − failure rate) × (1 − nothing-to-ship
// rate). Run-log rates are 0 when no runs were recorded, so the factor is 1.
func effectiveModelScore(row contributeEffectiveModelRow) float64 {
	score := wilsonLowerBound(row.FirstPassMerged, row.MergedPRs)
	score *= math.Max(0, 1-row.FailureRate)
	score *= math.Max(0, 1-row.NothingToShipRate)
	return score
}

// effectiveModelRuntimeForSources classifies a canonical row from its raw
// spellings (the provider prefix that marks a Pi row hosted or local is
// stripped from the canonical id): one verdict when every source agrees,
// "mixed" when hosted and local both appear.
func effectiveModelRuntimeForSources(model, backend string, sources []governorPRModelSource) string {
	if len(sources) == 0 {
		return effectiveModelRuntime(model, backend)
	}
	seen := map[string]bool{}
	for _, src := range sources {
		seen[effectiveModelRuntime(src.Model, src.Backend)] = true
	}
	switch {
	case seen["hosted"] && seen["local"]:
		return "mixed"
	case seen["hosted"]:
		return "hosted"
	case seen["local"]:
		return "local"
	default:
		return "unknown"
	}
}

func contributorPRKeysFromRuns(runs []TaskRunRecord) map[string]bool {
	out := map[string]bool{}
	for _, rec := range runs {
		if !rec.PRVerified {
			continue
		}
		repo, number := parsePRURL(rec.PRURL)
		if repo == "" || number == 0 {
			continue
		}
		out[effectivePRKey(repo, number)] = true
	}
	return out
}

func parsePRURL(raw string) (string, int) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", 0
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", 0
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", 0
	}
	return parts[0] + "/" + parts[1], n
}

// effectivePRRepo returns the canonical owner/repo for a PR record. pr.Repo
// may be a short configured name ("documentation") while run-log keys come
// from full PR URLs, so the URL is authoritative when it parses; it also keeps
// same-named repositories under different owners distinct.
func effectivePRRepo(pr ghpkg.PullRequest) string {
	if repo, n := parsePRURL(pr.URL); repo != "" && n == pr.Number {
		return repo
	}
	return pr.Repo
}

func effectivePRKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// effectiveModelHostedProviders are Pi provider prefixes ("provider/model")
// whose inference runs on a hosted API even though the Pi CLI runs locally.
var effectiveModelHostedProviders = map[string]bool{
	"anthropic":      true,
	"azure-openai":   true,
	"github-copilot": true,
	"google":         true,
	"groq":           true,
	"mistral":        true,
	"openai":         true,
	"openai-codex":   true,
	"openrouter":     true,
	"xai":            true,
}

// effectiveModelRuntime classifies where inference runs, not where the CLI
// runs: a Pi row is local only when its model names a local provider, hosted
// when it names a hosted one, and unknown otherwise.
func effectiveModelRuntime(model, backend string) string {
	b := strings.ToLower(strings.TrimSpace(backend))
	m := strings.ToLower(strings.TrimSpace(model))
	provider := ""
	if i := strings.Index(m, "/"); i > 0 {
		provider = m[:i]
	}
	switch {
	case strings.Contains(m, "gguf") || strings.Contains(m, "lemonade/") || strings.Contains(m, "ollama"):
		return "local"
	case b == "copilot" || b == "claude" || b == "openai" || b == "gemini":
		return "hosted"
	case b == "pi" && effectiveModelHostedProviders[provider]:
		return "hosted"
	default:
		return "unknown"
	}
}

func sortEffectiveModelRows(rows []contributeEffectiveModelRow, ranked bool) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Unattributed != b.Unattributed {
			return !a.Unattributed
		}
		if ranked && a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.MergedPRs != b.MergedPRs {
			return a.MergedPRs > b.MergedPRs
		}
		if a.AvgReviewRounds != b.AvgReviewRounds {
			return a.AvgReviewRounds < b.AvgReviewRounds
		}
		if a.AvgFixAttempts != b.AvgFixAttempts {
			return a.AvgFixAttempts < b.AvgFixAttempts
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Backend < b.Backend
	})
}
