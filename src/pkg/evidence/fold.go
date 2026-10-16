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
	Closed       bool
	// Backfilled is true when at least one record of the PR was reconstructed
	// from forge history (hivecommons/hive#12371). Such a record states the
	// PR's outcome (merged, closed or still open at backfill time); a live
	// bundle only ever states a merge the hive itself performed.
	Backfilled bool
}

// OutcomeKnown reports whether the folded records state what happened to
// the PR. A merge or a backfilled record is a known outcome; a live-only
// bundle with no merge event is not: the PR may be open, merged by someone
// other than this hive, or closed by hand, and nothing on disk says which.
// Readers that compute rates against an outcome must leave these PRs out of
// the denominator and report them as unknown rather than count them as
// "not merged" (hivecommons/hive#12514 follow-up).
func (p *PR) OutcomeKnown() bool {
	return p != nil && (p.Merge != nil || p.Backfilled)
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
		if bundleClosed(b) {
			p.Closed = true
		}
		if b.Backfilled || b.Backfill != nil {
			p.Backfilled = true
		}
	}
	for _, p := range prs {
		if p.Merge != nil {
			p.Closed = false
		}
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

// bundleClosed reports whether a bundle records a close without merge.
func bundleClosed(b *Bundle) bool {
	return b.Merge == nil && b.Backfill != nil && b.Backfill.State == BackfillStateClosed
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
