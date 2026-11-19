package stageengine

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// SimulatedLane is the one lane observation a simulated PR carries when
// the caller supplied none (RFC #17620 phase 3c). The built-in PR deriver
// classifies from the automerge lanes' reason strings, which a hand-built
// Facts has no source for; this derives the reason a lane would most
// plausibly have recorded from the head-level facts, so hive-default can
// be simulated without a live sweep. The result is a best effort, never a
// substitute for the lanes: the simulate API marks it in the response.
func SimulatedLane(f Facts) LaneFact {
	lane := prstage.StageLaneContributor
	switch {
	case f.Author.SelfAuthored:
		lane = prstage.StageLaneSelf
	case f.Author.Role == stagelabels.RoleTrusted:
		lane = prstage.StageLaneTrusted
	}
	l := LaneFact{Lane: lane, Role: f.Author.Role}
	pr := f.PR
	if pr == nil {
		pr = &PRFacts{}
	}
	switch {
	case f.Merged:
		l.Merged = true
	case !f.Open:
		l.Reason = "closed"
	case f.Draft:
		l.Reason = "draft"
	case pr.Conflicting:
		l.Reason = "conflicting"
	case pr.ChangesRequested:
		l.Reason = "changes-requested"
	case len(f.Park.Holds) > 0:
		l.Reason = "held"
	case f.Park.NeedsHuman:
		l.Reason = "label:needs-human"
	case pr.Checks.Known && len(pr.Checks.Red) > 0:
		l.Reason = "check-failing:" + pr.Checks.Red[0]
	case !pr.Checks.Known || pr.Checks.Pending > 0:
		l.Reason = "check-pending"
	case pr.Review.State == stagelabels.VerdictPending || pr.Review.State == stagelabels.VerdictUnavailable:
		l.Reason = prstage.ContributorLaneReasonNoVerdict
	case f.Author.Role == stagelabels.RoleUntrusted || f.Author.Role == stagelabels.RoleNewcomer:
		// Green and reviewed, but the lane cannot vouch for the author: a
		// person's merge click is all that is missing (090 candidate).
		l.Reason = "untrusted-author-role"
	}
	return l
}

// SimulateRef builds the Ref a simulation derives under: kind is "issue"
// or "pr"; repo is owner/repo (any shape is accepted, the deriver only
// reads the kind); number may be zero.
func SimulateRef(kind, owner, repo string, number int) (Ref, error) {
	var k stagelabels.Item
	switch kind {
	case "issue":
		k = stagelabels.IssueItem
	case "pr":
		k = stagelabels.PRItem
	default:
		return Ref{}, fmt.Errorf("kind must be issue or pr, got %q", kind)
	}
	return Ref{Owner: owner, Repo: repo, Number: number, Kind: k}, nil
}
