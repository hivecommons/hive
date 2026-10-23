package review

import (
	"strings"
)

// VerdictLabelSet is review.verdict_labels resolved for planning
// (hivecommons/hive#13833): the label each review state maps to. An empty
// field means that state carries no label.
type VerdictLabelSet struct {
	Reviewing        string
	Approve          string
	ChangesRequested string
	RequiresHuman    string
}

// Enabled reports whether any state maps to a label.
func (s VerdictLabelSet) Enabled() bool {
	return len(s.managed()) > 0
}

func (s VerdictLabelSet) managed() []string {
	var out []string
	for _, l := range []string{s.Reviewing, s.Approve, s.ChangesRequested, s.RequiresHuman} {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// VerdictLabelChange is one PR whose verdict label is out of step: Add is the
// label it should carry ("" when it should carry none), Remove the managed
// labels it carries that no longer apply.
type VerdictLabelChange struct {
	Repo   string
	Number int
	Add    string
	Remove []string
}

// PlanVerdictLabels decides, for each PR, which verdict label its current
// head should carry and returns only the PRs whose labels differ, so the
// steady state costs no writes.
//
// The state is read from the same two records the review swarm routes on:
// the verdict artifact's aggregate for the current head (settled verdicts),
// and the dispatch state's pending entries (a review in flight). A head with
// neither carries no verdict label, which is also what clears a previous
// head's label the moment a new head is pushed.
func PlanVerdictLabels(set VerdictLabelSet, prs []PullRequest, artifact Artifact, state DispatchState, maxChanges int) []VerdictLabelChange {
	if !set.Enabled() {
		return nil
	}
	managed := set.managed()
	var changes []VerdictLabelChange
	for _, pr := range prs {
		if maxChanges > 0 && len(changes) >= maxChanges {
			break
		}
		if pr.Number <= 0 || strings.TrimSpace(pr.HeadSHA) == "" {
			continue
		}
		want := strings.TrimSpace(set.desiredFor(pr, artifact, state))
		ch := VerdictLabelChange{Repo: pr.Repo, Number: pr.Number}
		if want != "" && !hasLabelFold(pr.Labels, want) {
			ch.Add = want
		}
		for _, l := range managed {
			if strings.EqualFold(l, want) {
				continue
			}
			if hasLabelFold(pr.Labels, l) {
				ch.Remove = append(ch.Remove, l)
			}
		}
		if ch.Add != "" || len(ch.Remove) > 0 {
			changes = append(changes, ch)
		}
	}
	return changes
}

// desiredFor returns the label pr's current head should carry.
func (s VerdictLabelSet) desiredFor(pr PullRequest, artifact Artifact, state DispatchState) string {
	if agg, ok := aggregateForPRLoose(artifact, pr); ok {
		switch agg.Verdict {
		case VerdictApprove:
			return s.Approve
		case VerdictChangesRequested:
			return s.ChangesRequested
		case VerdictRequiresHuman, VerdictReject:
			return s.RequiresHuman
		default:
			// incomplete: some perspectives (or heads) are still out.
			return s.Reviewing
		}
	}
	for _, p := range state.Pending {
		if p.Number == pr.Number && sameRepoLoose(p.Repo, pr.Repo) && SameHeadSHA(p.HeadSHA, pr.HeadSHA) {
			return s.Reviewing
		}
	}
	return ""
}

// aggregateForPRLoose is Artifact.AggregateFor with repo names compared
// case-insensitively and bare names matched against owner/name: aggregates
// carry GitHub's full name while governed PRs may carry a bare repo.
func aggregateForPRLoose(a Artifact, pr PullRequest) (Aggregate, bool) {
	var best Aggregate
	var found bool
	for _, item := range a.Items {
		item = item.Normalize()
		if item.Number != pr.Number || !sameRepoLoose(item.Repo, pr.Repo) || !SameHeadSHA(item.HeadSHA, pr.HeadSHA) {
			continue
		}
		if !found || item.RecordedAt.After(best.RecordedAt) {
			best = item
			found = true
		}
	}
	return best, found
}

// sameRepoLoose compares repo names case-insensitively, treating a bare name
// as equal to any owner/name with that name.
func sameRepoLoose(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if !strings.Contains(a, "/") {
		return strings.HasSuffix(b, "/"+a)
	}
	if !strings.Contains(b, "/") {
		return strings.HasSuffix(a, "/"+b)
	}
	return false
}
