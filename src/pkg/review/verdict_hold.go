package review

import (
	"strings"
	"time"
)

// VerdictHold is a review verdict that asks a person to decide before the
// pull request merges: a requires_human or reject aggregate that no later
// approving aggregate has cleared (hivecommons/hive#12374).
//
// Verdicts are recorded per head, but a hold is a property of the pull
// request: a fix push after the verdict moves the head and leaves the new
// head unreviewed, and that must not read as "nobody objected". The hold
// stands until a merge-eligible approve aggregate is recorded after it (on
// any head) or a person releases it on GitHub.
type VerdictHold struct {
	Verdict Verdict `json:"verdict"`
	HeadSHA string  `json:"head_sha,omitempty"`
	// RecordedAt is when the holding verdict was first recorded
	// (Aggregate.FirstRecorded), not when its reports were last collected:
	// a person's release after it must survive artifact refreshes
	// (hivecommons/hive#12957).
	RecordedAt time.Time `json:"recorded_at,omitzero"`
	Reasons    []string  `json:"reasons,omitempty"`
}

// BlocksSelfMerge reports whether an aggregate asks for a person:
// requires_human or reject. An aggregate with no perspectives means no report
// was ever collected; that is an unreviewed PR, not one awaiting a decision,
// the same rule holdForHuman applies.
func BlocksSelfMerge(a Aggregate) bool {
	if len(a.Perspectives) == 0 {
		return false
	}
	return a.RequiresHuman || a.Verdict == VerdictRequiresHuman || a.Verdict == VerdictReject
}

func clearsVerdictHold(a Aggregate) bool {
	return a.Verdict == VerdictApprove && a.MergeEligible
}

// LatestVerdictHold returns the hold one pull request's verdicts place on it.
// items must all belong to that pull request; their heads may differ. The
// most recently first-recorded decisive aggregate wins: a blocking one holds the PR, a
// merge-eligible approve releases it. changes_requested and incomplete
// aggregates neither hold nor release.
func LatestVerdictHold(items []Aggregate) (VerdictHold, bool) {
	var latest *Aggregate
	for i := range items {
		a := &items[i]
		if !BlocksSelfMerge(*a) && !clearsVerdictHold(*a) {
			continue
		}
		if latest == nil || a.FirstRecorded().After(latest.FirstRecorded()) {
			latest = a
		}
	}
	if latest == nil || !BlocksSelfMerge(*latest) {
		return VerdictHold{}, false
	}
	verdict := latest.Verdict
	if verdict != VerdictReject {
		verdict = VerdictRequiresHuman
	}
	return VerdictHold{Verdict: verdict, HeadSHA: latest.HeadSHA, RecordedAt: latest.FirstRecorded(), Reasons: latest.Reasons}, true
}

// VerdictHoldFor is LatestVerdictHold over the artifact's entries for
// repo#number. repo is owner/repo and matches case-insensitively.
func (a Artifact) VerdictHoldFor(repo string, number int) (VerdictHold, bool) {
	wantRepo := strings.TrimSpace(repo)
	var items []Aggregate
	for _, item := range a.Items {
		if item.Number == number && strings.EqualFold(strings.TrimSpace(item.Repo), wantRepo) {
			items = append(items, item)
		}
	}
	return LatestVerdictHold(items)
}
