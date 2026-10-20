package evidence

import (
	"strconv"
	"strings"
	"time"
)

// PR is one pull request's bundles folded into a single view. Bundle is the
// newest record (verdicts, CI, sentinel, shadow); OpenedAt, Merge, the four
// change-failure link lists and Closed come from whichever record of the
// same PR knows them. A live bundle is created when review starts and only
// gains a merge event when the hive itself merges, while a backfilled bundle
// (hivecommons/hive#12371) is stamped with the PR's open time and its forge
// outcome; folding the two keeps the verdicts and the outcome together, and
// readers window on PR open time rather than on when any record happened to
// be written.
//
// GET /api/evidence/summary, the hub heartbeat evidence block and
// `hivectl evidence export` all read bundles through FoldPRs so their
// per-PR counts reconcile (hivecommons/hive#12516).
type PR struct {
	*Bundle
	OpenedAt     time.Time
	Merge        *MergeEvent
	RevertOf     []string
	FollowupOf   []string
	RevertedBy   []string
	FollowedUpBy []string
	// IssueReopens unions the issue_reopens of every record of the PR
	// (hivecommons/hive#13241).
	IssueReopens []IssueReopen
	Closed       bool
	// Backfilled is true when at least one record of the PR was reconstructed
	// from forge history (hivecommons/hive#12371). Such a record states the
	// PR's outcome (merged, closed or still open at backfill time); a live
	// bundle only ever states a merge the hive itself performed.
	Backfilled bool
	// Outcome is the newest forge-observed outcome recorded on any record of
	// the PR, or applied from the review-outcome ledger by ApplyOutcomeHints.
	Outcome *Outcome
	// MergedElsewhere is true when Merge was learned from the forge (Outcome
	// with state merged) rather than performed by this hive.
	MergedElsewhere bool
}

// OutcomeKnown reports whether the folded records state what happened to
// the PR. A merge, a backfilled record or a forge-observed Outcome is a known
// outcome; a live-only bundle with none of them is not: the PR may be open,
// merged by someone other than this hive, or closed by hand, and nothing on
// disk says which. Readers that compute rates against an outcome must leave
// these PRs out of the denominator and report them as unknown rather than
// count them as "not merged" (hivecommons/hive#12514 follow-up).
func (p *PR) OutcomeKnown() bool {
	return p != nil && (p.Merge != nil || p.Backfilled || p.Outcome != nil)
}

// OutcomeHint is what the review-outcome ledger knows about a PR: its state
// (open, closed or merged) and the forge's timestamps and merger for it.
type OutcomeHint struct {
	State    string
	MergedBy string
	MergedAt time.Time
	ClosedAt time.Time
	// At is when the ledger observed the state.
	At time.Time
}

// ApplyOutcomeHints resolves the PRs in prs whose outcome is not known from
// their records using hints keyed by PRKey. It is the read-time counterpart
// of the ledger's on-disk Outcome stamp: a PR that merged or closed before
// the ledger could write to its bundle (or that the governor never saw open)
// still gets its outcome from the ledger's row. A hinted merge becomes a
// Merge event with Actor MergedBy and Method MergeUnrecorded, flagged
// MergedElsewhere; a hinted close sets Closed; an open hint only makes the
// outcome known. PRs whose records already state an outcome are untouched.
func ApplyOutcomeHints(prs map[string]*PR, hints map[string]OutcomeHint) {
	for key, p := range prs {
		if p == nil || p.OutcomeKnown() {
			continue
		}
		h, ok := hints[key]
		if !ok {
			continue
		}
		o := &Outcome{Source: OutcomeSourceLedger, State: h.State, At: h.At, MergedBy: h.MergedBy, MergedAt: h.MergedAt, ClosedAt: h.ClosedAt}
		switch h.State {
		case BackfillStateMerged:
			p.Merge = OutcomeMergeEvent(o, p.HeadSHA)
			p.MergedElsewhere = true
		case BackfillStateClosed:
			p.Closed = true
		case BackfillStateOpen:
		default:
			continue
		}
		p.Outcome = o
	}
}

// OutcomeMergeEvent is the merge event a forge-observed merged Outcome
// stands for: the merger the forge named (or MergeUnrecorded), method
// MergeUnrecorded because this hive did not perform it, and the bundle's
// head as the SHA since the merge commit is not known without another fetch
// (a backfilled record does the same). The time falls back to the
// observation so the event always validates.
func OutcomeMergeEvent(o *Outcome, head string) *MergeEvent {
	m := &MergeEvent{Actor: o.MergedBy, Method: MergeUnrecorded, At: o.MergedAt, SHA: strings.TrimSpace(head)}
	if m.Actor == "" {
		m.Actor = MergeUnrecorded
	}
	if m.At.IsZero() {
		m.At = o.At
	}
	if m.SHA == "" {
		m.SHA = MergeUnrecorded
	}
	return m
}

// PRKey is the case-insensitive owner/name#number key FoldPRs groups by.
func PRKey(repo string, number int) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// FoldPRs groups bundles by PR after the repo (case-insensitive, empty = all)
// and author kind (empty = all) filter, keyed by PRKey. Nil bundles are
// skipped.
func FoldPRs(bundles []*Bundle, repo, kind string) map[string]*PR {
	prs := map[string]*PR{}
	for _, b := range bundles {
		if b == nil {
			continue
		}
		if repo != "" && !strings.EqualFold(b.Repo, repo) {
			continue
		}
		if kind != "" && b.Author.Kind != kind {
			continue
		}
		key := PRKey(b.Repo, b.Number)
		opened := bundleOpenedAt(b)
		p := prs[key]
		if p == nil {
			p = &PR{Bundle: b, OpenedAt: opened}
			prs[key] = p
		} else {
			if b.CreatedAt.After(p.CreatedAt) || (b.CreatedAt.Equal(p.CreatedAt) && b.UpdatedAt.After(p.UpdatedAt)) {
				p.Bundle = b
			}
			if opened.Before(p.OpenedAt) {
				p.OpenedAt = opened
			}
		}
		if b.Merge != nil && (p.Merge == nil || b.Merge.At.After(p.Merge.At)) {
			p.Merge = b.Merge
		}
		p.RevertOf = longerLinks(p.RevertOf, b.RevertOf)
		p.FollowupOf = longerLinks(p.FollowupOf, b.FollowupOf)
		p.RevertedBy = longerLinks(p.RevertedBy, b.RevertedBy)
		p.FollowedUpBy = longerLinks(p.FollowedUpBy, b.FollowedUpBy)
		p.IssueReopens = mergeIssueReopens(p.IssueReopens, b.IssueReopens)
		if bundleClosed(b) {
			p.Closed = true
		}
		if b.Backfilled || b.Backfill != nil {
			p.Backfilled = true
		}
		if b.Outcome != nil && (p.Outcome == nil || b.Outcome.At.After(p.Outcome.At)) {
			p.Outcome = b.Outcome
		}
	}
	for _, p := range prs {
		if p.Merge != nil {
			p.Closed = false
		}
		p.MergedElsewhere = p.Merge != nil && p.Outcome != nil && p.Outcome.State == BackfillStateMerged
	}
	return prs
}

// longerLinks keeps the link list of whichever record recorded more links.
func longerLinks(cur, next []string) []string {
	if len(next) > len(cur) {
		return next
	}
	return cur
}

// bundleClosed reports whether a bundle records a close without merge, from
// a backfilled record or a forge-observed Outcome.
func bundleClosed(b *Bundle) bool {
	if b.Merge != nil {
		return false
	}
	return (b.Backfill != nil && b.Backfill.State == BackfillStateClosed) ||
		(b.Outcome != nil && b.Outcome.State == BackfillStateClosed)
}

// bundleOpenedAt is the PR open time a bundle knows: merge.opened_at when
// captured, else the record's created_at (a backfilled record's created_at
// is the PR's open time; a live record's is when review started).
func bundleOpenedAt(b *Bundle) time.Time {
	if b.Merge != nil && !b.Merge.OpenedAt.IsZero() && b.Merge.OpenedAt.Before(b.CreatedAt) {
		return b.Merge.OpenedAt
	}
	return b.CreatedAt
}
