package dashboard

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Evidence card "Issue opens : PR merges": hive-authored issue opens against
// hive-merged PRs, judged against the governor's cadence band
// (pkg/evidence/opens_merges.go). Two existing sources, no new persistence:
//
//   - opens: the audit log's issue creates performed by the hive's own GitHub
//     identity — agent-requested (AuditActionAgentIssueCreated) and hive
//     automation (AuditActionHiveIssueCreated) — the same actions the Work
//     balance card's audit fallback reads. Issues humans or third-party bots
//     open directly on GitHub never appear in the audit log, so they are
//     excluded by construction.
//   - merges: folded evidence bundles whose PR merged, where either this hive
//     performed the merge (not MergedElsewhere) or the author is an agent —
//     the Outcome by author kind card's agent-authored definition.
//
// Read-only reporting; nothing here is consulted by merge eligibility.

// evidenceOpensMergesOpenActions are the audit actions that count as a
// hive-authored issue open.
var evidenceOpensMergesOpenActions = map[string]bool{
	ghpkg.AuditActionAgentIssueCreated: true,
	ghpkg.AuditActionHiveIssueCreated:  true,
}

// evidenceHiveMergedPR reports whether a folded PR counts as a hive merge.
func evidenceHiveMergedPR(p *evidence.PR) bool {
	if p == nil || p.Bundle == nil || p.Merge == nil || p.Merge.At.IsZero() {
		return false
	}
	return !p.MergedElsewhere || p.Author.Kind == evidence.AuthorAgent
}

// evidenceOpensMergesEvents collects hive issue opens from the audit entries
// and hive PR merges from the folded PRs, optionally restricted to one repo.
func evidenceOpensMergesEvents(entries []AuditEntry, prs map[string]*evidence.PR, repo string) []evidence.OpensMergesEvent {
	var out []evidence.OpensMergesEvent
	for _, e := range entries {
		if !evidenceOpensMergesOpenActions[strings.TrimSpace(e.Action)] {
			continue
		}
		t, ok := prThroughputTime(e)
		if !ok {
			continue
		}
		r, ok := auditEntryRepo(e)
		if !ok || (repo != "" && !strings.EqualFold(r, repo)) {
			continue
		}
		out = append(out, evidence.OpensMergesEvent{Repo: r, At: t})
	}
	for _, p := range prs {
		if !evidenceHiveMergedPR(p) || (repo != "" && !strings.EqualFold(p.Repo, repo)) {
			continue
		}
		out = append(out, evidence.OpensMergesEvent{Repo: p.Repo, At: p.Merge.At, Merge: true})
	}
	return out
}

// evidenceGovernorMode is the governor's current cadence mode, or "" when
// no governor is wired (the card then uses the parity band).
func (s *Server) evidenceGovernorMode() string {
	if s == nil || s.deps == nil || s.deps.Governor == nil {
		return ""
	}
	return string(s.deps.Governor.GetState().Mode)
}

// buildEvidenceOpensMerges runs the card's aggregation for the window.
func buildEvidenceOpensMerges(events []evidence.OpensMergesEvent, mode string, win evidenceWindow, now time.Time) evidence.OpensMerges {
	return evidence.BuildOpensMerges(evidence.OpensMergesInput{
		Events:   events,
		Mode:     mode,
		InWindow: func(t time.Time) bool { return evidenceWindowHasTime(win, t) },
		Now:      now,
	})
}
