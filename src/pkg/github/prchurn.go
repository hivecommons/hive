package github

import (
	"fmt"
	"sort"
	"time"
)

// Churn guard (hivecommons/hive#7995).
//
// The duplicate-PR guard above answers "is somebody on this right now?". It
// cannot answer the question that actually stalled
// projectbluefin/documentation#1232: "has this issue already consumed seven
// pull requests, three of them merged, without anybody being able to say it is
// done?".
//
// That issue described an incremental refactor. Every cycle an agent took it,
// found remaining work, shipped the next slice, and the ledger booked the task
// shipped; the merged PR either carried no parseable claim at all or carried a
// weak one that releases for re-verification, so the issue came back on the
// next cooldown. Nothing in that loop can conclude "enough" — that is a
// maintainer's call — and nothing routed it to a maintainer.
//
// So the ledger keeps a second, cumulative record: which pull requests have
// been observed against each issue, and how they ended. Past a threshold the
// contribute queue stops offering the issue and withholds it for human triage
// instead, on the same operator-visible surface as an open-PR claim.
//
// What it is NOT: it does not close, comment on, label, or otherwise mutate
// the issue, and it is not evidence that the work is finished. It is a stop on
// re-offering, and it lapses by itself once the churn ages out.

const (
	// churnHistoryTTL bounds how long one observed pull request stays in an
	// issue's churn history without being re-observed.
	//
	// It is deliberately LONGER than mergedClaimScanWindow: each scan only
	// sees the pull requests closed or merged in the last 72 hours, and the
	// pattern this guard exists to catch plays out over a week or more
	// (#1232's seven PRs spanned 09-13 to 09-20). Accumulating across scans is
	// the whole mechanism — a single scan can never see the shape. It is still
	// finite, so an issue whose churn stops is offered again roughly two weeks
	// after the last pull request touched it, without an operator having to do
	// anything.
	churnHistoryTTL = 14 * 24 * time.Hour

	// ChurnMergedThreshold and ChurnClosedThreshold are how many merged (or
	// closed-unmerged) pull requests on ONE issue mean the next attempt should
	// not be dispatched automatically.
	//
	// Two, not three: at two merged PRs the issue has already demonstrated
	// that merging work against it does not settle it, and the third cycle is
	// the one this guard is meant to prevent rather than record. The cost of
	// being early is one issue waiting for a maintainer on a surface built to
	// be looked at; the cost of being late is another wasted task cycle per
	// merge, indefinitely.
	ChurnMergedThreshold = 2
	ChurnClosedThreshold = 2
)

// Pull request states recorded in an issue's churn history.
const (
	// PRStateOpen: observed by the open-PR scan.
	PRStateOpen = "open"
	// PRStateMerged: observed by the settle scan with a merge timestamp.
	PRStateMerged = "merged"
	// PRStateClosed: observed by the settle scan closed WITHOUT merging — the
	// state that produces no claim at all and so was previously invisible.
	PRStateClosed = "closed"
)

// IssuePRRecord is one pull request observed against one issue, in whatever
// state the scan that saw it last reported. It carries no issue-body or
// contributor data: numbers, a URL, an author login, and a state.
type IssuePRRecord struct {
	// Repo and Issue identify the issue, keyed exactly as IssueClaim does.
	Repo  string `json:"repo"`
	Issue int    `json:"issue"`
	// PRNumber / PRRepo / PRURL / PRAuthor identify the pull request. PRRepo
	// may differ from Repo for a cross-repo reference.
	PRNumber int    `json:"pr_number"`
	PRRepo   string `json:"pr_repo"`
	PRURL    string `json:"pr_url,omitempty"`
	PRAuthor string `json:"pr_author,omitempty"`
	// State is one of PRStateOpen / PRStateMerged / PRStateClosed.
	State string `json:"state"`
	// ObservedAt is when a scan last reported this pull request in this state.
	// It is refreshed on every re-observation, so the TTL measures time since
	// the pull request last appeared in a scan window, not since it was first
	// seen.
	ObservedAt time.Time `json:"observed_at"`
}

// Key identifies the issue this record belongs to.
func (r IssuePRRecord) Key() string { return claimKey(r.Repo, r.Issue) }

// terminal reports whether this state can never change again. A merged or
// closed pull request is final; an open one is not. insertHistoryLocked uses
// it so a stale "open" sighting can never overwrite the outcome.
func (r IssuePRRecord) terminal() bool {
	return r.State == PRStateMerged || r.State == PRStateClosed
}

// ClaimScan is one complete pass of the claim scanner: the claims the
// duplicate-PR guard consumes, plus the churn history the same two listings
// already contained and used to discard. Returning both from one pass is what
// keeps the churn guard free of extra GitHub calls — the closed-PR listing the
// merged settle scan already pages through is exactly the listing that knows
// how many pull requests died on an issue.
type ClaimScan struct {
	Claims  []IssueClaim
	History []IssuePRRecord
}

// prHistoryFromClaims projects the issues one pull request is linked to into
// churn records in the given state. It reuses claimsFromPR's output rather
// than re-parsing, so the churn history covers exactly the links the claim
// ledger understands — closing keywords, the branch-name heuristic, and
// non-closing references.
//
// Both tiers count. A "Refs #N" PR that merged is still a pull request spent
// on that issue, and the incremental shape this guard exists to catch is
// precisely the one whose PRs decline to claim a close.
func prHistoryFromClaims(claims []IssueClaim, state string, now time.Time) []IssuePRRecord {
	if len(claims) == 0 {
		return nil
	}
	out := make([]IssuePRRecord, 0, len(claims))
	for _, c := range claims {
		if c.Issue <= 0 || c.Repo == "" {
			continue
		}
		out = append(out, IssuePRRecord{
			Repo:       c.Repo,
			Issue:      c.Issue,
			PRNumber:   c.PRNumber,
			PRRepo:     c.PRRepo,
			PRURL:      c.PRURL,
			PRAuthor:   c.PRAuthor,
			State:      state,
			ObservedAt: now,
		})
	}
	return out
}

// IssueChurn summarises every pull request currently remembered against one
// issue, split by how each one ended.
type IssueChurn struct {
	Repo   string
	Issue  int
	Open   []IssuePRRecord
	Merged []IssuePRRecord
	// ClosedUnmerged is the count that has no other home: such a pull request
	// produces no claim, so before #7995 it left no trace anywhere.
	ClosedUnmerged []IssuePRRecord
}

// NeedsHumanTriage reports whether this issue has churned enough that the next
// attempt should go to a maintainer instead of to an agent or a contributor.
func (c IssueChurn) NeedsHumanTriage() bool {
	return len(c.Merged) >= ChurnMergedThreshold || len(c.ClosedUnmerged) >= ChurnClosedThreshold
}

// Reason is the operator-facing sentence behind a withheld churning issue. It
// states the counts rather than the rule, because the counts are what a
// maintainer needs in order to answer the question being asked of them.
func (c IssueChurn) Reason() string {
	return fmt.Sprintf(
		"An issue with %d merged and %d closed PRs needs a maintainer to say what is left",
		len(c.Merged), len(c.ClosedUnmerged))
}

// PRNumbers renders the pull requests behind the judgment, merged first then
// closed-unmerged, each as "#N" (or "owner/repo#N" when the pull request lives
// in another repository). Open pull requests are omitted: they are the
// open-PR claim gate's business, not evidence of churn.
func (c IssueChurn) PRNumbers() []string {
	out := make([]string, 0, len(c.Merged)+len(c.ClosedUnmerged))
	for _, group := range [][]IssuePRRecord{c.Merged, c.ClosedUnmerged} {
		for _, r := range group {
			if r.PRRepo != "" && r.PRRepo != c.Repo {
				out = append(out, claimKey(r.PRRepo, r.PRNumber))
				continue
			}
			out = append(out, fmt.Sprintf("#%d", r.PRNumber))
		}
	}
	return out
}

// RecordPRHistory merges a scan's churn records into the ledger and prunes
// anything past churnHistoryTTL.
//
// Unlike Reconcile it NEVER replaces: absence from a scan is not evidence that
// a pull request did not happen, only that it has left the 72-hour settle
// window. Records leave by aging out, which is the one bound on how long a
// churn judgment can stand. A nil ledger is a no-op; the caller persists.
func (l *ClaimLedger) RecordPRHistory(records []IssuePRRecord) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range records {
		l.insertHistoryLocked(r)
	}
	l.pruneHistoryLocked()
}

// insertHistoryLocked stores one record, keyed by issue and pull request.
// Callers must hold l.mu (or own the ledger exclusively, as LoadClaimLedger
// does).
//
// A later observation wins EXCEPT when it would demote a terminal state back
// to open: a merged or closed pull request cannot reopen, so an "open"
// sighting arriving after one — from a retried page, a clock skew, or a stale
// persisted ledger — is the stale one.
func (l *ClaimLedger) insertHistoryLocked(r IssuePRRecord) {
	if r.Issue <= 0 || r.Repo == "" || r.PRNumber <= 0 || r.State == "" {
		return
	}
	if r.ObservedAt.IsZero() {
		r.ObservedAt = l.now()
	}
	if l.history == nil {
		l.history = make(map[string]map[int]IssuePRRecord)
	}
	key := r.Key()
	byPR, ok := l.history[key]
	if !ok {
		byPR = make(map[int]IssuePRRecord, 1)
		l.history[key] = byPR
	}
	if existing, ok := byPR[r.PRNumber]; ok && existing.terminal() && !r.terminal() {
		// Keep the outcome, but let the sighting refresh the TTL: the pull
		// request is demonstrably still being observed.
		if r.ObservedAt.After(existing.ObservedAt) {
			existing.ObservedAt = r.ObservedAt
			byPR[r.PRNumber] = existing
		}
		return
	}
	byPR[r.PRNumber] = r
}

// pruneHistoryLocked drops records past churnHistoryTTL and any issue left
// with none. Callers must hold l.mu.
func (l *ClaimLedger) pruneHistoryLocked() {
	if l.history == nil {
		return
	}
	cutoff := l.now().Add(-l.churnTTL)
	for key, byPR := range l.history {
		for number, r := range byPR {
			if r.ObservedAt.Before(cutoff) {
				delete(byPR, number)
			}
		}
		if len(byPR) == 0 {
			delete(l.history, key)
		}
	}
}

// Churn returns what the ledger remembers about the pull requests spent on one
// issue. ok is false when it remembers none, so a caller can tell "no churn"
// from "never observed" without inspecting the zero value.
func (l *ClaimLedger) Churn(repo string, issue int) (IssueChurn, bool) {
	if l == nil {
		return IssueChurn{}, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	byPR, ok := l.history[claimKey(repo, issue)]
	if !ok || len(byPR) == 0 {
		return IssueChurn{}, false
	}
	churn := IssueChurn{Repo: repo, Issue: issue}
	cutoff := l.now().Add(-l.churnTTL)
	for _, r := range byPR {
		// Read-path expiry as well as the prune: a ledger that has not
		// reconciled since a long outage must not act on records the prune
		// would have dropped.
		if r.ObservedAt.Before(cutoff) {
			continue
		}
		switch r.State {
		case PRStateMerged:
			churn.Merged = append(churn.Merged, r)
		case PRStateClosed:
			churn.ClosedUnmerged = append(churn.ClosedUnmerged, r)
		case PRStateOpen:
			churn.Open = append(churn.Open, r)
		}
	}
	if len(churn.Open)+len(churn.Merged)+len(churn.ClosedUnmerged) == 0 {
		return IssueChurn{}, false
	}
	sortPRRecords(churn.Open)
	sortPRRecords(churn.Merged)
	sortPRRecords(churn.ClosedUnmerged)
	return churn, true
}

// PRHistory returns every remembered record, sorted for a stable on-disk file.
func (l *ClaimLedger) PRHistory() []IssuePRRecord {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	var out []IssuePRRecord
	for _, byPR := range l.history {
		for _, r := range byPR {
			out = append(out, r)
		}
	}
	l.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		if out[i].Issue != out[j].Issue {
			return out[i].Issue < out[j].Issue
		}
		return out[i].PRNumber < out[j].PRNumber
	})
	return out
}

// SetChurnHistoryTTL overrides how long a churn record survives. Intended for
// tests. A non-positive duration is ignored rather than treated as "remember
// nothing", so a mis-set value cannot silently disable the guard.
func (l *ClaimLedger) SetChurnHistoryTTL(d time.Duration) {
	if l == nil || d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.churnTTL = d
}

func sortPRRecords(records []IssuePRRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].PRRepo != records[j].PRRepo {
			return records[i].PRRepo < records[j].PRRepo
		}
		return records[i].PRNumber < records[j].PRNumber
	})
}
