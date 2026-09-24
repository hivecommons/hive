package dashboard

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	effectiveModelsDefaultMinMergedPRs = 5
	effectiveModelsMinPRsEnv           = "HIVE_CONTRIBUTE_EFFECTIVE_MODELS_MIN_PRS"

	effectiveModelsFilterAll         = "all"
	effectiveModelsFilterContributor = "contributor"
	effectiveModelsFilterHive        = "hive"
)

type contributeEffectiveModelsResponse struct {
	Window       string                        `json:"window"`
	Filter       string                        `json:"filter"`
	MinMergedPRs int                           `json:"min_merged_prs"`
	Ranked       []contributeEffectiveModelRow `json:"ranked"`
	Insufficient []contributeEffectiveModelRow `json:"insufficient"`
}

type contributeEffectiveModelRow struct {
	Model              string                       `json:"model"`
	Backend            string                       `json:"backend"`
	Runtime            string                       `json:"runtime"`
	EffectivenessRank  int                          `json:"effectiveness_rank,omitempty"`
	PRs                int                          `json:"prs"`
	MergedPRs          int                          `json:"merged_prs"`
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
	runs, _ := readEffectiveModelRuns(s.effectiveModelTaskRunLogPath(), governorPRModelsWindowDuration(window))
	resp := aggregateContributeEffectiveModels(prs, runs, window, filter, minMergedPRs, time.Now())
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

func readEffectiveModelRuns(path string, window time.Duration) ([]TaskRunRecord, error) {
	taskRunMu.Lock()
	data, err := os.ReadFile(path)
	taskRunMu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cutoff := ""
	if window > 0 {
		cutoff = time.Now().UTC().Add(-window).Format(time.RFC3339)
	}
	var out []TaskRunRecord
	for _, line := range strings.Split(string(data), "\n") {
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
	return out, nil
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
		key := effectivePRKey(pr.Repo, pr.Number)
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
		model := ghpkg.NormalizeAttributionModel(rec.Model)
		backend := ghpkg.NormalizeAttributionValue(rec.Backend)
		key := effectiveModelKey(model, backend)
		st := runStats[key]
		if st == nil {
			st = &effectiveRunStats{}
			runStats[key] = st
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
		row := effectiveRowForKey(rows, b.Model, b.Backend)
		row.PRs = b.PRs
		row.MergedPRs = b.Merged
		row.FirstPassMergeRate = b.Rework.FirstPassMergeRate
		row.AvgReviewRounds = b.Rework.AvgReviewRounds
		row.AvgFixAttempts = b.Rework.AvgFixAttempts
	}
	for key, st := range runStats {
		parts := strings.Split(key, "\x00")
		if len(parts) != 2 {
			continue
		}
		row := effectiveRowForKey(rows, parts[0], parts[1])
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
		row := effectiveRowForKey(rows, item.Model, item.Backend)
		row.MostReworked = append(row.MostReworked, item)
	}

	resp := contributeEffectiveModelsResponse{Window: prAgg.Window, Filter: filter, MinMergedPRs: minMergedPRs}
	for _, row := range rows {
		row.Runtime = effectiveModelRuntime(row.Model, row.Backend)
		if row.MergedPRs >= minMergedPRs {
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

func effectiveModelKey(model, backend string) string {
	return model + "\x00" + backend
}

func effectiveRowForKey(rows map[string]*contributeEffectiveModelRow, model, backend string) *contributeEffectiveModelRow {
	key := model + "\x00" + backend
	row := rows[key]
	if row == nil {
		row = &contributeEffectiveModelRow{Model: model, Backend: backend}
		rows[key] = row
	}
	return row
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

func effectivePRKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

func effectiveModelRuntime(model, backend string) string {
	b := strings.ToLower(strings.TrimSpace(backend))
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case b == "pi" || strings.Contains(m, "gguf") || strings.Contains(m, "lemonade/") || strings.Contains(m, "ollama"):
		return "local"
	case b == "copilot" || b == "claude" || b == "openai" || b == "gemini":
		return "hosted"
	default:
		return "unknown"
	}
}

func sortEffectiveModelRows(rows []contributeEffectiveModelRow, ranked bool) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if ranked && a.FirstPassMergeRate != b.FirstPassMergeRate {
			return a.FirstPassMergeRate > b.FirstPassMergeRate
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
