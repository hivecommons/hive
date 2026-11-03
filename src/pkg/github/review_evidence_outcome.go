package github

import (
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

// RecordReviewEvidenceOutcome stamps the PR's newest evidence bundle with the
// outcome the review-outcome ledger just resolved from the forge: closed
// without merge, or merged by someone other than this hive. It is called from
// the governor's ledger pass after ledger.Resolve, so it costs no GitHub call
// of its own (st is the state that pass already fetched). Record-only: it
// never touches a bundle that already states an outcome (a merge this hive
// performed, a backfilled record or an earlier outcome stamp), writes nothing
// for a PR that is still open, and logs and swallows every failure.
func (c *Client) RecordReviewEvidenceOutcome(repo string, number int, st PRState, now time.Time) {
	if c == nil || number <= 0 {
		return
	}
	repo = strings.TrimSpace(repo)
	settings, ok := c.reviewEvidenceSettings(repo)
	if !ok {
		return
	}
	outcome, ok := reviewEvidenceOutcomeFromState(st, now)
	if !ok {
		return
	}
	key, err := evidence.LoadSigningKey(settings.SigningKeyFile)
	if err != nil {
		c.logger.Warn("review evidence: signing key unusable, writing unsigned",
			slog.String("error", err.Error()))
		key = nil
	}
	if !st.MergedAt.IsZero() {
		// A merged revert the hive did not perform still names the PR it
		// undid; the state fetch already carried its text (#13241).
		c.RecordReviewEvidenceRevert(MergedPRText{Repo: repo, Number: number, HeadSHA: st.HeadSHA,
			Title: st.Title, Body: st.Body, MergedAt: st.MergedAt})
	}

	reviewEvidenceMu.Lock()
	defer reviewEvidenceMu.Unlock()

	found, err := FindVerifiedReviewEvidence("", repo, number, "", reviewEvidenceVerifierForKey(key))
	if err != nil || found.Entry == nil {
		return
	}
	b, path := found.Entry.Bundle, found.Entry.Path
	if b.Merge != nil || b.Backfilled || b.Backfill != nil || b.Outcome != nil {
		return
	}
	b.Outcome = outcome
	if outcome.State == evidence.BackfillStateMerged {
		b.Merge = evidence.OutcomeMergeEvent(outcome, b.HeadSHA)
	}
	b.UpdatedAt = now
	if err := b.Validate(); err != nil {
		c.logger.Warn("review evidence: outcome-stamped bundle invalid, not written",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	if err := evidence.Seal(b, key); err != nil {
		c.logger.Warn("review evidence: could not seal outcome-stamped bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	if err := writeReviewEvidence(path, b); err != nil {
		c.logger.Warn("review evidence: could not write outcome-stamped bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	c.logger.Info("review evidence: forge outcome recorded",
		slog.String("id", b.ID), slog.String("state", outcome.State), slog.String("merged_by", outcome.MergedBy))
}

// reviewEvidenceOutcomeFromState turns a fetched PR state into the Outcome to
// stamp, or ok=false when the PR is still open (nothing to record yet).
func reviewEvidenceOutcomeFromState(st PRState, now time.Time) (*evidence.Outcome, bool) {
	o := &evidence.Outcome{Source: evidence.OutcomeSourceLedger, At: now.UTC()}
	switch {
	case !st.MergedAt.IsZero():
		o.State = evidence.BackfillStateMerged
		o.MergedAt = st.MergedAt.UTC()
		o.MergedBy = strings.TrimSpace(st.MergedBy)
	case strings.EqualFold(strings.TrimSpace(st.State), "closed"):
		o.State = evidence.BackfillStateClosed
		if !st.ClosedAt.IsZero() {
			o.ClosedAt = st.ClosedAt.UTC()
		}
	default:
		return nil, false
	}
	return o, true
}
