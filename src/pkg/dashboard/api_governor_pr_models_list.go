package dashboard

import (
	"net/http"
	"sort"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/modelfamily"
)

// governorPRModelPRsCap bounds the per-model PR list returned by
// /api/governor/pr-models?model=…; past it PRsTruncated is set.
const governorPRModelPRsCap = 200

// governorPRModelsWithPRsResponse is the /api/governor/pr-models response
// when a model filter is given: the usual body plus that model's PRs. PRs is
// a pointer so an unknown model encodes as "prs": [] rather than dropping the
// field; without a filter this type is never used, so the default response is
// unchanged.
type governorPRModelsWithPRsResponse struct {
	governorPRModelsResponse
	PRs          *[]governorPRModelPRItem `json:"prs,omitempty"`
	PRsTruncated bool                     `json:"prs_truncated,omitempty"`
}

// governorPRModelPRItem is one PR in a model's list: the most_reworked fields
// plus its outcome, author agent, timestamps and first-pass flag. No PR body,
// the same privacy as most_reworked.
type governorPRModelPRItem struct {
	governorPRMostReworkedItem
	Outcome   string `json:"outcome"`
	Agent     string `json:"agent,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	MergedAt  string `json:"merged_at,omitempty"`
	FirstPass bool   `json:"first_pass"`
}

// governorPRModelsQueryModel returns the canonical model named by the
// request's model= parameter, and false when the parameter is absent or blank.
func governorPRModelsQueryModel(r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("model"))
	if raw == "" {
		return "", false
	}
	return modelfamily.Canonical(raw), true
}

// governorPRModelPRList lists every attributed PR in the window whose bucket
// (modelfamily.Canonical of its model) is model, using the same window and
// attribution filters as aggregateGovernorPRModels so the list length matches
// the bucket's prs count. Sorted by review rounds + fix attempts descending,
// then created_at descending; capped at governorPRModelPRsCap.
func governorPRModelPRList(prs []ghpkg.PullRequest, window, model string, now time.Time) ([]governorPRModelPRItem, bool) {
	window = normalizeGovernorPRModelsWindow(window)
	cutoff := time.Time{}
	switch window {
	case governorPRModelsWindow7d:
		cutoff = now.AddDate(0, 0, -governorPRModelsDays7)
	case governorPRModelsWindow30d:
		cutoff = now.AddDate(0, 0, -governorPRModelsDays30)
	}
	type listed struct {
		item    governorPRModelPRItem
		created time.Time
	}
	var rows []listed
	for _, pr := range prs {
		if !pr.HiveAttributed {
			continue
		}
		if !cutoff.IsZero() && pr.CreatedAt.Before(cutoff) {
			continue
		}
		if modelfamily.Canonical(pr.HiveModel) != model {
			continue
		}
		agent := ghpkg.NormalizeAttributionValue(pr.HiveAgent)
		if agent == "unknown" {
			agent = ""
		}
		item := governorPRModelPRItem{
			governorPRMostReworkedItem: governorPRMostReworkedItem{
				Repo:                pr.Repo,
				Number:              pr.Number,
				Title:               pr.Title,
				Model:               model,
				Backend:             ghpkg.NormalizeAttributionValue(pr.HiveBackend),
				URL:                 pr.URL,
				ReviewRounds:        pr.Rework.ReviewRounds,
				FixAttempts:         pr.Rework.FixAttempts,
				FollowUpCommits:     pr.Rework.FollowUpCommits,
				FixerModels:         append([]string(nil), pr.Rework.FixerModels...),
				HumanChangeRequests: pr.Rework.HumanChangeRequests,
			},
			Outcome:   governorPRModelOutcome(pr),
			Agent:     agent,
			FirstPass: pr.Rework.FirstPass,
		}
		if !pr.CreatedAt.IsZero() {
			item.CreatedAt = pr.CreatedAt.UTC().Format(time.RFC3339)
		}
		if !pr.MergedAt.IsZero() {
			item.MergedAt = pr.MergedAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, listed{item: item, created: pr.CreatedAt})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a := rows[i].item.ReviewRounds + rows[i].item.FixAttempts
		b := rows[j].item.ReviewRounds + rows[j].item.FixAttempts
		if a != b {
			return a > b
		}
		if !rows[i].created.Equal(rows[j].created) {
			return rows[i].created.After(rows[j].created)
		}
		if rows[i].item.Repo != rows[j].item.Repo {
			return rows[i].item.Repo < rows[j].item.Repo
		}
		return rows[i].item.Number > rows[j].item.Number
	})
	truncated := len(rows) > governorPRModelPRsCap
	if truncated {
		rows = rows[:governorPRModelPRsCap]
	}
	out := make([]governorPRModelPRItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.item)
	}
	return out, truncated
}

// withGovernorPRModelPRs wraps resp with model's PR list for the same window.
func withGovernorPRModelPRs(resp governorPRModelsResponse, prs []ghpkg.PullRequest, model string, now time.Time) governorPRModelsWithPRsResponse {
	list, truncated := governorPRModelPRList(prs, resp.Window, model, now)
	return governorPRModelsWithPRsResponse{governorPRModelsResponse: resp, PRs: &list, PRsTruncated: truncated}
}
