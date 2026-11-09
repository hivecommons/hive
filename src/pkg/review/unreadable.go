package review

import (
	"regexp"
	"strings"
)

// A reviewer that could not read the PR is told to say so with
// requires_human (buildReadInstruction) rather than guess. That is the right
// answer when the diff is genuinely unreviewable — too large, masked — and a
// person has to look. It is the wrong answer when the cause is the hive's own
// plumbing: the agent's GitHub API cap (agents.github_api_hourly_cap, the
// proxy's 429) or GitHub's rate limit. Then nothing about the PR needs a
// person; the read has to be tried again once the limit resets.
//
// Recorded as a verdict, that report did real damage
// (projectbluefin/testsuite#1116, 2026-11-07): five perspectives each wrote
// "requires human: could not read the delta: every GitHub read returned HTTP
// 429 … Not reviewed". The security one counted as "the security review
// withheld approval" (IsSecuritySensitiveFinding), the PR parked at
// stage/085-needs-decision, and the owner's delegate asked a person to rule
// on a security finding that did not exist. On a contributor PR without the
// security one, the same requires_human would have resolved hands-free to
// approve — an unread head marked merge-eligible.
//
// UnreadableReviewReason recognises such a report, so the relay can decline
// to record it as a verdict: the dispatch row it answered stays open,
// expires after review.pending_ttl, and the planner asks again.

// unreadableReviewTerms say the reviewer did not read the change.
var unreadableReviewTerms = []string{
	"could not read", "couldn't read", "cannot read", "can't read", "unable to read",
	"not readable", "unreadable", "could not fetch", "couldn't fetch", "unable to fetch",
	"could not load", "could not access", "unable to access", "not reviewed",
	"could not review", "unable to review", "could not be read",
}

// rateLimitCauseRe names a rate limit as the reason: an HTTP 429, GitHub's
// rate limit (primary or secondary), or the hive proxy's per-agent hourly
// API cap. "429" is matched as a whole number so a line number or count that
// happens to contain it does not.
var rateLimitCauseRe = regexp.MustCompile(`(?i)(?:\b429\b|too many requests|rate[- ]?limit|hourly cap|api cap|api (?:hourly )?budget|quota exhausted|quota exceeded)`)

// UnreadableReviewReason reports whether r is a requires_human that only
// says the reviewer could not read the PR because of a rate limit, and
// returns the reviewer's own summary as the reason.
//
// It is deliberately narrow. The report must:
//   - be requires_human (an approve or changes_requested is a judgement);
//   - not carry the reviewer's explicit high-risk marker;
//   - not ask a decision question (the needs-decision marker): a reviewer
//     that asks the owner something read enough to ask it, even when the
//     PR it read is about rate limits;
//   - raise no in-scope finding (a finding means it read something);
//   - say, in its summary or decision question, both that it could not read
//     the change and that a rate limit was the cause.
//
// A requires_human for a diff that is too large, or for any other reason
// than a rate limit, still parks the PR as before.
func UnreadableReviewReason(r PerspectiveReport) (string, bool) {
	if r.Verdict != VerdictRequiresHuman || r.DecisionRiskHigh || r.DecisionRequested {
		return "", false
	}
	for _, f := range r.Findings {
		if InScopeFinding(f) {
			return "", false
		}
	}
	text := strings.TrimSpace(r.Summary + "\n" + r.DecisionQuestion)
	if text == "" || !rateLimitCauseRe.MatchString(text) {
		return "", false
	}
	lower := strings.ToLower(text)
	for _, term := range unreadableReviewTerms {
		if strings.Contains(lower, term) {
			return strings.TrimSpace(r.Summary), true
		}
	}
	return "", false
}

// CapStoppedReviewReason reports whether r is a requires_human that the
// relay should set aside because the hive proxy refused the reviewer's
// GitHub reads during the session (its agents.github_api_hourly_cap or the
// shared reserve floor). The caller supplies that fact from the proxy's own
// record; this decides only whether the report is the kind such a refusal
// can produce. It returns the reviewer's summary as the reason.
//
// UnreadableReviewReason needs the reviewer to say, in words it knows, that
// a rate limit kept it from reading. Reviewers do not keep to those words:
// on projectbluefin/utah-packages#959 (2026-11-09) the reviewer wrote
// "GitHub reads stopped at the user's request after the hourly cap; …
// could not be verified", and on projectbluefin/utah#1161 "not assessed:
// GitHub API cap prevented reading". Each parked its PR at 085. The cap
// event itself is the evidence, so the wording does not matter here.
//
// The report must:
//   - be requires_human (an approve or changes_requested is a judgement);
//   - not carry the reviewer's explicit high-risk marker;
//   - raise no in-scope finding (a finding is something it read and judged);
//   - not name a PR that already carries the change (RedundantOf), which the
//     hive's own already-landed close acts on.
//
// A decision question does not keep it: a session the proxy cut short asks
// for "a review with access to the changes" (projectbluefin/finpilot#659),
// and a real question is asked again by the re-review that reads the PR.
func CapStoppedReviewReason(r PerspectiveReport) (string, bool) {
	if r.Verdict != VerdictRequiresHuman || r.DecisionRiskHigh || r.RedundantOf > 0 {
		return "", false
	}
	for _, f := range r.Findings {
		if InScopeFinding(f) {
			return "", false
		}
	}
	reason := strings.TrimSpace(r.Summary)
	if reason == "" {
		reason = strings.TrimSpace(r.DecisionQuestion)
	}
	if reason == "" {
		reason = "requires_human from a review session the hive proxy refused GitHub reads"
	}
	return reason, true
}
