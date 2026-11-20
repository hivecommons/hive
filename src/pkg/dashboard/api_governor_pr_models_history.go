package dashboard

import (
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The PRs-by-model and Most-effective-models cards read the persisted
// attributed PR history (pkg/github/attributed_pr_history.go) under the live
// enumeration. The live records win for any PR both hold: they carry the
// freshest state, and they are the only source for open PRs. The history adds
// the closed PRs the bounded live scan no longer lists, so a 30d window counts
// a PR closed and left alone for longer than the scan's lookback.

// prModelsAttributedPRs is the attributed-PR sample behind both cards and the
// history's coverage. Coverage.Available is false without a GitHub client or
// with no history store; the cards then rest on the live scan, as before.
func (s *Server) prModelsAttributedPRs(actionable *ghpkg.ActionableResult) ([]ghpkg.PullRequest, ghpkg.AttributedPRCoverage) {
	var live []ghpkg.PullRequest
	if actionable != nil {
		live = actionable.PRs.Attributed
	}
	if s == nil || s.deps == nil || s.deps.GHClient == nil {
		return live, ghpkg.AttributedPRCoverage{}
	}
	stored, cov := s.deps.GHClient.AttributedPRHistory()
	return mergeAttributedPRHistory(live, stored), cov
}

// mergeAttributedPRHistory appends to live the stored PRs it does not already
// hold, keyed the way the cards key PRs (effectivePRKey of effectivePRRepo).
func mergeAttributedPRHistory(live, stored []ghpkg.PullRequest) []ghpkg.PullRequest {
	if len(stored) == 0 {
		return live
	}
	seen := make(map[string]bool, len(live)+len(stored))
	out := make([]ghpkg.PullRequest, 0, len(live)+len(stored))
	for _, pr := range live {
		seen[effectivePRKey(effectivePRRepo(pr), pr.Number)] = true
		out = append(out, pr)
	}
	for _, pr := range stored {
		key := effectivePRKey(effectivePRRepo(pr), pr.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, pr)
	}
	return out
}

// applyAttributedPRHistoryCoverage sets the closed-PR half of cov from the
// history's real reach. Windows are by PR creation, and the history holds
// every attributed PR created at or after hist.Since, so a window is partial
// only when it starts before that (All: unless the history is full). The
// lookback days and closed_prs_since report that start; for a full history it
// is the oldest PR held. Without a history cov is left as the live scan's.
func applyAttributedPRHistoryCoverage(cov *contributeEffectiveModelsCoverage, hist ghpkg.AttributedPRCoverage, prs []ghpkg.PullRequest, window time.Duration, now time.Time) {
	if cov == nil || !hist.Available {
		return
	}
	since := hist.Since
	if hist.Full {
		cov.ClosedPRsPartial = false
		since = time.Time{}
		for _, pr := range prs {
			if pr.HiveAttributed && !pr.CreatedAt.IsZero() && (since.IsZero() || pr.CreatedAt.Before(since)) {
				since = pr.CreatedAt
			}
		}
	} else {
		cov.ClosedPRsPartial = window <= 0 || now.Add(-window).Before(since)
	}
	if since.IsZero() {
		return
	}
	cov.ClosedPRsSince = since.UTC().Format(time.RFC3339)
	if days := int(now.Sub(since) / (24 * time.Hour)); days > cov.ClosedPRLookbackDays {
		cov.ClosedPRLookbackDays = days
	}
}
