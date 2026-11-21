package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A second automatic review of the same head must not quietly clear the
// first one's requires_human or changes_requested. The relay keeps one report
// file per perspective (review.ReviewReportFilePrefix + repo, number,
// perspective, review head), not per run, so whichever run wrote last used to
// be the verdict. On projectbluefin/dakota-iso#376 (2026-11-21) a queue-mode
// review recorded security requires_human at 18:43:07, a review-swarm kick
// planned before that verdict existed ran on the same head and recorded
// approve into the same file 93 seconds later, and the next collect saw only
// the approve: the PR merged without the decision a person had been asked
// for. projectbluefin/bonedigger#116 ran the same race the other way round
// and held, so the outcome depended on which run finished last.
//
// The rule: a report that would replace a stricter non-approve report for the
// same PR head is kept out, unless the review that produced it was asked for
// after the stricter report was on disk. Only the planner asks for a review
// of a head it already holds a verdict for, and only on purpose: a
// review.revise_verdicts_before revisit, a body-edit revisit, or the whole
// perspective set re-asked after an incomplete re-review's entries expired.
// Those dispatches are planned after the verdict they revisit, so they still
// replace it. A queue-mode (advisory) review has no dispatch, and a kick
// planned before the verdict landed was not planned in view of it; neither
// may clear it, and neither may a sibling reviewer's verdict bound to another
// agent's row, or one the dispatched agent recorded before the kick reached
// it. A new head always replaces: the restrictive verdict judged other code.

// ReadReportFile reads one review report the relay stored, with the time it
// arrived (ReportArrival). ok is false when the file is missing or
// unreadable.
func ReadReportFile(path string) (PerspectiveReport, time.Time, bool) {
	raw, modTime, err := readCollectedReport(path)
	if err != nil {
		return PerspectiveReport{}, time.Time{}, false
	}
	var r PerspectiveReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return PerspectiveReport{}, time.Time{}, false
	}
	return r, ReportArrival(r, modTime), true
}

// ReportArrival is when the relay recorded r: the received_at it stamped
// into the report, or, for a file written before that field existed, the
// file's modification time. The stamp is what orders a stored verdict
// against a later dispatch; a copied, rsynced or restored report dir
// rewrites modification times, and read from them an old verdict could look
// newer than the re-review asked for after it, or the reverse.
func ReportArrival(r PerspectiveReport, modTime time.Time) time.Time {
	if !r.ReceivedAt.IsZero() {
		return r.ReceivedAt.UTC()
	}
	return modTime
}

// KeepsPriorReport reports whether next must not replace prior, the report
// already stored under the same file: prior is a requires_human,
// changes_requested or reject for a named head, next is a less strict verdict
// for that same head, and next's review was not dispatched after prior
// arrived (next.DispatchedAt; zero means no dispatch of its own, an advisory
// or sibling review). A prior the reviewer could not read because of a rate
// limit is no verdict and never holds.
//
// There is no exception for the same agent or the same dispatch: a reviewer
// cannot soften its own verdict on a head either. The relay cannot tell two
// reports from one kick apart from a queue-mode session and a swarm kick of
// the same agent on the same head. On a hive whose reviewer lanes take both
// (reviewer-2 on projectbluefin's hive did, in the dakota-iso#376 incident),
// a swarm kick typed into the busy session reads as delivered within
// seconds, the queue session's requires_human then binds that delivered row,
// and the kick's own approve would carry the same dispatch time and reporter.
func KeepsPriorReport(prior PerspectiveReport, priorReceivedAt time.Time, next PerspectiveReport) bool {
	if strings.TrimSpace(prior.HeadSHA) == "" || !SameHeadSHA(prior.HeadSHA, next.HeadSHA) {
		return false
	}
	if !retainsOverIncomplete(prior.Verdict) || verdictStrictness(next.Verdict) >= verdictStrictness(prior.Verdict) {
		return false
	}
	if _, unread := UnreadableReviewReason(prior); unread {
		return false
	}
	if !next.DispatchedAt.IsZero() && !priorReceivedAt.IsZero() && next.DispatchedAt.After(priorReceivedAt) {
		return false
	}
	return true
}

// SettledNonApprove reports whether v is a verdict a later, less strict
// automatic review of the same head may not replace: requires_human,
// changes_requested or reject.
func SettledNonApprove(v Verdict) bool { return retainsOverIncomplete(v) }

// PendingDelivered reports whether the review kick behind pending row p
// reached its agent: the row's own delivered_at (ConfirmDelivered), or, for a
// row written before that field, a recent-dispatch row for the same agent,
// PR head, body, perspective, review head and decision step, confirmed at or
// after p was planned. Before that, a verdict from the agent cannot be the
// kick's answer; it is a queue-mode review the agent ran on its own.
func PendingDelivered(state DispatchState, p PendingReview) bool {
	if !p.Delivered.IsZero() && !p.Delivered.Before(p.Dispatched) {
		return true
	}
	for _, r := range state.Recent {
		if r.Agent == p.Agent && r.Perspective == p.Perspective && r.Head == p.Head && r.Decision == p.Decision &&
			strings.TrimSpace(r.BodyDigest) == strings.TrimSpace(p.BodyDigest) &&
			samePRHead(r.Repo, r.Number, r.HeadSHA, p.Repo, p.Number, p.HeadSHA) &&
			!r.Confirmed.IsZero() && !r.Confirmed.Before(p.Dispatched) {
			return true
		}
	}
	return false
}

// RecentDispatchedAt is when the review behind a recent-dispatch row was
// planned: its dispatched_at. A row written before ConfirmDelivered carried
// that has none, and gets zero (not a deliberate re-review) rather than its
// delivery time: a kick delivered after a stricter verdict arrived may still
// have been planned before it, which is exactly the dakota-iso#376 race.
// Fail closed: for the recent rows' 24-hour retention after an upgrade, a
// deliberate re-review bound only to such a row does not clear a stricter
// verdict; a person's decision or a new head still does.
func RecentDispatchedAt(r RecentReview) time.Time {
	return r.Dispatched
}

// LiveReviews is the plan's pending rows that still stand for a review in
// flight: planned (dispatched_at set) less than pendingTTL ago (zero means
// DefaultPendingReviewTTL, negative never expires), on a head whose redispatch
// budget is not spent. It is what the queue-mode reviewer's dispatched mark
// is drawn from.
func (p *DispatchPlan) LiveReviews(pendingTTL time.Duration, now time.Time) []PendingReview {
	if p == nil {
		return nil
	}
	var out []PendingReview
	for _, e := range p.inFlight(DispatchOptions{PendingTTL: pendingTTL}, now) {
		if !e.Dispatched.IsZero() {
			out = append(out, e)
		}
	}
	return out
}

// EffectivePendingReviewTTL resolves review.pending_ttl the way the planner
// does: zero is DefaultPendingReviewTTL, negative disables expiry (0).
func EffectivePendingReviewTTL(d time.Duration) time.Duration {
	return DispatchOptions{PendingTTL: d}.effectivePendingTTL()
}

// RecordedReport is one perspective report on disk, as the planner sees it
// before the verdict artifact has folded it in.
type RecordedReport struct {
	Repo        string
	Number      int
	HeadSHA     string
	BodyDigest  string
	Perspective Perspective
	Verdict     Verdict
	ReceivedAt  time.Time
}

// RecordedReportsFor lists the plain perspective reports on disk for prs
// (review heads' raw reports and decided reports are left out: the heads
// path tracks those itself). The eval cycle collects the report dir into the
// artifact before it plans, but a verdict the relay records after that
// collect is invisible to the planner until the next cycle, and without this
// the planner reads the head as unreviewed and asks again. A missing dir or
// an unreadable file is skipped: this only ever withholds a kick.
func RecordedReportsFor(dir string, prs []PullRequest) []RecordedReport {
	dir = ReportDir(dir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(prs) == 0 {
		return nil
	}
	prefixes := make([]string, 0, len(prs))
	for _, pr := range prs {
		prefixes = append(prefixes, strings.ToLower(ReportFilePrefix(pr.Repo, pr.Number)))
	}
	var out []RecordedReport
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ReviewReportFilePrefix) || !strings.HasSuffix(name, ReviewReportFileSuffix) {
			continue
		}
		lower := strings.ToLower(name)
		selected := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(lower, prefix) {
				selected = true
				break
			}
		}
		if !selected {
			continue
		}
		r, receivedAt, ok := ReadReportFile(filepath.Join(dir, name)) // received_at, else mtime
		if !ok || strings.TrimSpace(r.Head) != "" || r.Decided || strings.TrimSpace(r.HeadSHA) == "" {
			continue
		}
		if _, unread := UnreadableReviewReason(r); unread {
			continue
		}
		out = append(out, RecordedReport{Repo: r.Repo, Number: r.Number, HeadSHA: r.HeadSHA, BodyDigest: r.BodyDigest,
			Perspective: r.Perspective, Verdict: r.Verdict, ReceivedAt: receivedAt})
	}
	return out
}

// recordedDelivered is the perspectives of pr's current head and body that
// have a report on disk the artifact does not hold yet, received within the
// pending TTL. The TTL bounds it: a report that never reaches the artifact
// (a collect that keeps failing) withholds the head for one TTL, the same
// time an unanswered dispatch does, and then the head is asked again.
func recordedDelivered(pr PullRequest, opts DispatchOptions, now time.Time) map[Perspective]Verdict {
	if len(opts.RecordedReports) == 0 {
		return nil
	}
	ttl := opts.effectivePendingTTL()
	var out map[Perspective]Verdict
	for _, r := range opts.RecordedReports {
		if !samePRHead(fullRepoName(r.Repo, opts.ProjectOrg), r.Number, r.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) ||
			strings.TrimSpace(r.BodyDigest) != strings.TrimSpace(pr.BodyDigest) {
			continue
		}
		if ttl > 0 && !r.ReceivedAt.IsZero() && now.Sub(r.ReceivedAt) >= ttl {
			continue
		}
		if out == nil {
			out = map[Perspective]Verdict{}
		}
		setStrictestPerspectiveVerdict(out, r.Perspective, r.Verdict)
	}
	return out
}
