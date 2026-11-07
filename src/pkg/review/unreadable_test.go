package review

import (
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// The five summaries the reviewer wrote on projectbluefin/testsuite#1116 at
// head ae22434 (2026-11-07), when every GitHub read hit the agent API cap.
var testsuite1116Summaries = map[Perspective]string{
	PerspectiveCorrectness:     "Could not read the delta d397ece..ae22434: every GitHub read returned HTTP 429 (hive agent API hourly cap, 181-211/400) and no local checkout of projectbluefin/testsuite exists on this host. Not reviewed; previous correctness verdict (approve) was for head d397ece only.",
	PerspectiveDocsCurrency:    "Delta touches docs/skills/test-authoring/suite-map/SKILL.md but could not be read (GitHub API 429 cap, no local checkout); could not confirm whether prior finding SKILL.md:170 (immutability checks still marked @pending) is fixed. Status unknown.",
	PerspectiveIntentAlignment: "Delta not readable (GitHub API 429 cap, no local checkout); could not re-check prior finding tests/common/features/common_immutable.feature:13 ('Closes #838' may over-claim scope) nor read issue #838. Status of prior finding unknown.",
	PerspectiveSecurity:        "Delta not readable (GitHub API 429 cap, no local checkout). Not reviewed at this head.",
	PerspectiveStyle:           "Delta not readable (GitHub API 429 cap, no local checkout). Not reviewed at this head.",
}

func TestUnreadableReviewReasonRecognisesRateLimitedReviews(t *testing.T) {
	for p, summary := range testsuite1116Summaries {
		r := PerspectiveReport{Perspective: p, Verdict: VerdictRequiresHuman}
		r.Summary = summary
		reason, ok := UnreadableReviewReason(r)
		if !ok {
			t.Errorf("%s: rate-limited unreadable review not recognised: %q", p, summary)
			continue
		}
		if reason != summary {
			t.Errorf("%s: reason = %q, want the reviewer's summary", p, reason)
		}
	}
	others := []string{
		"Could not fetch the diff: gh returned 429 Too Many Requests.",
		"Unable to read the PR files: API rate limit exceeded for installation.",
		"I cannot read the diff; hive: agent GitHub API hourly cap reached (400/400).",
		"Could not read the diff: secondary rate-limit hit on every request.",
	}
	for _, summary := range others {
		r := PerspectiveReport{Perspective: PerspectiveCorrectness, Verdict: VerdictRequiresHuman}
		r.Summary = summary
		if _, ok := UnreadableReviewReason(r); !ok {
			t.Errorf("not recognised: %q", summary)
		}
	}
}

func TestUnreadableReviewReasonLeavesRealHoldsAlone(t *testing.T) {
	unread := testsuite1116Summaries[PerspectiveSecurity]
	cases := []struct {
		name   string
		report PerspectiveReport
	}{
		{"approve is a judgement", unreadableTestReport(PerspectiveSecurity, VerdictApprove, unread, nil)},
		{"changes_requested is a judgement", unreadableTestReport(PerspectiveSecurity, VerdictChangesRequested, unread, nil)},
		{"too large is a real hold", unreadableTestReport(PerspectiveCorrectness, VerdictRequiresHuman, "Could not read the diff: it is too large (12k lines). A person needs to review it.", nil)},
		{"rate limit without an unread claim", unreadableTestReport(PerspectiveSecurity, VerdictRequiresHuman, "The new client retries a 429 forever without backoff; a person should decide the rate-limit policy.", nil)},
		{"security finding stays a security finding", unreadableTestReport(PerspectiveSecurity, VerdictRequiresHuman, "Token is written to the log.", nil)},
		{"an in-scope finding means it read something", unreadableTestReport(PerspectiveSecurity, VerdictRequiresHuman, unread,
			[]outputschema.Finding{{Title: "secret leaks into logs", Severity: outputschema.SeverityHigh, File: "a.go", Line: 3}})},
		{"empty summary", unreadableTestReport(PerspectiveSecurity, VerdictRequiresHuman, "", nil)},
	}
	for _, tc := range cases {
		if reason, ok := UnreadableReviewReason(tc.report); ok {
			t.Errorf("%s: recognised as unreadable (%q), want a judgement kept", tc.name, reason)
		}
	}
	marked := unreadableTestReport(PerspectiveSecurity, VerdictRequiresHuman, unread, nil)
	marked.DecisionRiskHigh = true
	if _, ok := UnreadableReviewReason(marked); ok {
		t.Error("the reviewer's explicit high-risk marker was overridden")
	}
	outOfScope := unreadableTestReport(PerspectiveSecurity, VerdictRequiresHuman, unread,
		[]outputschema.Finding{{Title: "pre-existing", Severity: outputschema.SeverityLow, ReviewScope: "out-of-scope"}})
	if _, ok := UnreadableReviewReason(outOfScope); !ok {
		t.Error("an out-of-scope note kept a rate-limited unread review as a verdict")
	}
}

func unreadableTestReport(p Perspective, v Verdict, summary string, findings []outputschema.Finding) PerspectiveReport {
	r := PerspectiveReport{Perspective: p, Verdict: v}
	r.Summary = summary
	r.Findings = findings
	return r
}
