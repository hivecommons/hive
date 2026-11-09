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
		"Could not read the diff; hive: the hive's shared GitHub API budget is low (379 left, reserve floor 400); this is not this agent's own cap.",
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
	// hivecommons/hive#16994 review: a reviewer that read a PR about rate
	// limiting and asks the owner a question uses both kinds of words.
	asked := unreadableTestReport(PerspectiveCorrectness, VerdictRequiresHuman,
		"I could not review the retry budget against production numbers; @owner should decide whether the hourly cap may be bypassed here.", nil)
	if _, ok := UnreadableReviewReason(asked); !ok {
		t.Fatal("test premise: without the decision marker this wording reads as unreadable")
	}
	asked.DecisionRequested = true
	asked.DecisionQuestion = "May the hourly cap be bypassed for this retry path?"
	if _, ok := UnreadableReviewReason(asked); ok {
		t.Error("an explicit decision question was dropped as an unreadable review")
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

// Summaries the reviewer wrote on 2026-11-09 after the proxy refused its
// GitHub reads at its 300/h cap. Each parked its PR at 085, because none says
// "could not read" in the words UnreadableReviewReason knows.
var capStoppedSummaries = []string{
	// projectbluefin/utah-packages#959 @d09267d
	"Previous review had no findings to re-check. Read the full PR diff and 11-file delta; commit history identifies the delta as a merge of main containing #957. No concrete new correctness defect found. GitHub reads stopped at the user's request after the hourly cap; head-file surroundings, source-lock consumers, build behavior and head-specific tests could not be verified. Requires completion of those checks before correctness approval.",
	"Previous review had no findings to re-check. Delta documentation distinguishes recipe import from build, publication and runtime proof, consistent with the backlog record. No verified documentation defect found. Claims about HTTP rejection, source fallback behavior and build-root dependencies could not be checked against their consumers after GitHub reads stopped; documentation assessment remains incomplete.",
	"Previous review had no findings to re-check. Delta adds pinned firmware sources and a driver options file through a merge of main. No verified secret, injection or permission regression found in the diff. Source verification consumers and driver-option behavior could not be inspected after GitHub reads were stopped at the user's request; security assessment remains incomplete, not evidence of a vulnerability.",
	// projectbluefin/utah#1161 @615fe80
	"Correctness not assessed: GitHub returned HTTP 429 before PR body or diff could be read. Available checkout is utah-packages, lacks requested head, and no Utah checkout was located. Requires review with repository access; no defect or CI claim asserted.",
	"Documentation currency not assessed: GitHub API cap prevented reading changes and related documentation; requested head unavailable locally. Cannot determine whether operator-facing descriptions remain accurate.",
	"Security not assessed: GitHub API hourly cap prevented reading PR body, diff, and review threads; requested repository and head unavailable locally. Cannot evaluate secret exposure or trust-boundary changes without code.",
}

func TestCapStoppedReviewReasonDoesNotDependOnWording(t *testing.T) {
	for _, summary := range capStoppedSummaries {
		r := unreadableTestReport(PerspectiveCorrectness, VerdictRequiresHuman, summary, nil)
		if _, ok := UnreadableReviewReason(r); ok {
			t.Fatalf("test premise: the wording match recognises %q", summary)
		}
		reason, ok := CapStoppedReviewReason(r)
		if !ok {
			t.Errorf("requires_human from a cut-short session not set aside: %q", summary)
			continue
		}
		if reason != summary {
			t.Errorf("reason = %q, want the reviewer's summary", reason)
		}
	}
	// projectbluefin/finpilot#659: the cut-short session asked the owner for
	// "a review with access to the proposed changes".
	asked := unreadableTestReport(PerspectiveCorrectness, VerdictRequiresHuman, "", nil)
	asked.DecisionRequested = true
	asked.DecisionQuestion = "obtain a review with access to the proposed changes before deciding whether to merge."
	if reason, ok := CapStoppedReviewReason(asked); !ok || reason != asked.DecisionQuestion {
		t.Errorf("decision question from a cut-short session = %q, %v; want set aside with the question as reason", reason, ok)
	}
}

func TestCapStoppedReviewReasonKeepsJudgements(t *testing.T) {
	summary := capStoppedSummaries[0]
	cases := []struct {
		name   string
		report PerspectiveReport
	}{
		{"approve is a judgement", unreadableTestReport(PerspectiveSecurity, VerdictApprove, summary, nil)},
		{"changes_requested is a judgement", unreadableTestReport(PerspectiveSecurity, VerdictChangesRequested, summary, nil)},
		{"an in-scope finding means it read and judged something", unreadableTestReport(PerspectiveSecurity, VerdictRequiresHuman, summary,
			[]outputschema.Finding{{Title: "secret leaks into logs", Severity: outputschema.SeverityHigh, File: "a.go", Line: 3}})},
	}
	for _, tc := range cases {
		if reason, ok := CapStoppedReviewReason(tc.report); ok {
			t.Errorf("%s: set aside (%q), want kept", tc.name, reason)
		}
	}
	marked := unreadableTestReport(PerspectiveSecurity, VerdictRequiresHuman, summary, nil)
	marked.DecisionRiskHigh = true
	if _, ok := CapStoppedReviewReason(marked); ok {
		t.Error("the reviewer's explicit high-risk marker was overridden")
	}
	redundant := unreadableTestReport(PerspectiveIntentAlignment, VerdictRequiresHuman, summary, nil)
	redundant.RedundantOf = 957
	if _, ok := CapStoppedReviewReason(redundant); ok {
		t.Error("an already-landed report was set aside")
	}
	outOfScope := unreadableTestReport(PerspectiveSecurity, VerdictRequiresHuman, summary,
		[]outputschema.Finding{{Title: "pre-existing", Severity: outputschema.SeverityLow, ReviewScope: "out-of-scope"}})
	if _, ok := CapStoppedReviewReason(outOfScope); !ok {
		t.Error("an out-of-scope note kept a cut-short requires_human as a verdict")
	}
}
