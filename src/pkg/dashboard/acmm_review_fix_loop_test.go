package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// Hive's review-fix loop applies review feedback without any of the four
// workflow files acmm:copilot-review-apply looks for (#20050). These tests pin
// that the evaluator credits the loop from Hive's own recorded verdicts, that
// the credit counts as detected (it can advance a level, unlike a waiver), and
// that it never replaces a real file detection.

func withACMMReviewArtifact(t *testing.T, art review.Artifact) {
	t.Helper()
	orig := acmmLoadReviewArtifact
	acmmLoadReviewArtifact = func() (review.Artifact, error) { return art, nil }
	t.Cleanup(func() { acmmLoadReviewArtifact = orig })
}

func reviewFixLoopAggregate(repo, head string, verdict review.Verdict, at time.Time) review.Aggregate {
	return review.Aggregate{
		Repo:              repo,
		Number:            7,
		HeadSHA:           head,
		Verdict:           verdict,
		HiveAuthored:      true,
		HiveAuthoredKnown: true,
		ReportReceipts: []review.ReportReceipt{
			{Perspective: review.PerspectiveCorrectness, ReceivedAt: at},
		},
	}
}

// acmmReviewApplyMux serves repo1 with the given names in .github/workflows.
func acmmReviewApplyMux(t *testing.T, workflows ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/contents/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path[len("/repos/myorg/repo1/contents/"):]
		w.Header().Set("Content-Type", "application/json")
		switch path {
		case "":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"name": "README.md", "type": "file"},
				{"name": "go.mod", "type": "file"},
				{"name": "CLAUDE.md", "type": "file"},
				{"name": "CONTRIBUTING.md", "type": "file"},
			})
		case ".github/workflows":
			entries := []map[string]interface{}{{"name": "ci.yml", "type": "file"}}
			for _, name := range workflows {
				entries = append(entries, map[string]interface{}{"name": name, "type": "file"})
			}
			_ = json.NewEncoder(w).Encode(entries)
		case ".github", ".github/ISSUE_TEMPLATE", ".github/prompts", ".github/agents", ".claude", "docs", "docs/security":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestEvaluateAllRepos_ReviewFixLoopCreditsReviewApply(t *testing.T) {
	now := time.Now()
	withACMMReviewArtifact(t, review.Artifact{})
	base := acmmWaiverServer(t, acmmReviewApplyMux(t)).evaluateAllRepos()
	if c := acmmResultFor(t, base, acmmReviewApplyID); c.Passed {
		t.Fatalf("fixture repo passed %s without evidence", acmmReviewApplyID)
	}
	baseL4 := acmmLevelFor(t, base, 4)

	withACMMReviewArtifact(t, review.Artifact{Items: []review.Aggregate{
		reviewFixLoopAggregate("myorg/repo1", "head-a", review.VerdictChangesRequested, now.Add(-6*24*time.Hour)),
		reviewFixLoopAggregate("myorg/repo1", "head-b", review.VerdictApprove, now.Add(-5*24*time.Hour)),
	}})
	eval := acmmWaiverServer(t, acmmReviewApplyMux(t)).evaluateAllRepos()
	c := acmmResultFor(t, eval, acmmReviewApplyID)
	if !c.Passed || c.Waived || c.SatisfiedBy == "" {
		t.Fatalf("review-fix loop not credited: passed=%v waived=%v satisfied_by=%q", c.Passed, c.Waived, c.SatisfiedBy)
	}
	if c.SatisfiedBy != acmmHiveReviewFixLoopProvider {
		t.Errorf("satisfied_by = %q, want %q", c.SatisfiedBy, acmmHiveReviewFixLoopProvider)
	}
	gotL4 := acmmLevelFor(t, eval, 4)
	if gotL4.Detected != baseL4.Detected+1 {
		t.Errorf("L4 detected = %d, want %d: Hive credit must count toward the detected score", gotL4.Detected, baseL4.Detected+1)
	}
	for _, row := range eval.CriteriaResults {
		if row.ID == acmmReviewApplyID && (!row.Passed || row.Waived || row.SatisfiedBy == "") {
			t.Errorf("aggregate row not credited: %+v", row)
		}
	}
}

func TestEvaluateAllRepos_ReviewFixLoopNegativeCases(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	a := reviewFixLoopAggregate("myorg/repo1", "head-a", review.VerdictChangesRequested, now.Add(-6*day))
	contributorA, contributorB := a, reviewFixLoopAggregate("myorg/repo1", "head-b", review.VerdictApprove, now.Add(-5*day))
	contributorA.HiveAuthored, contributorB.HiveAuthored = false, false
	unknownA, unknownB := contributorA, contributorB
	unknownA.HiveAuthoredKnown, unknownB.HiveAuthoredKnown = false, false
	const fullHead = "0123456789abcdef0123456789abcdef01234567"
	fullA := reviewFixLoopAggregate("myorg/repo1", fullHead, review.VerdictChangesRequested, now.Add(-6*day))
	shortB := reviewFixLoopAggregate("myorg/repo1", "0123456", review.VerdictApprove, now.Add(-5*day))
	// A was reviewed twice; B arrived after A's first report but before its
	// latest, so B is not a review of a head pushed after the last ask.
	twiceA := reviewFixLoopAggregate("myorg/repo1", "head-a", review.VerdictChangesRequested, now.Add(-6*day))
	twiceA.ReportReceipts = append(twiceA.ReportReceipts, review.ReportReceipt{Perspective: review.PerspectiveSecurity, ReceivedAt: now.Add(-4 * day)})
	betweenB := reviewFixLoopAggregate("myorg/repo1", "head-b", review.VerdictApprove, now.Add(-5*day))
	cases := map[string][]review.Aggregate{
		"contributor PR":              {contributorA, contributorB},
		"authorship unknown":          {unknownA, unknownB},
		"same head in short form":     {fullA, shortB},
		"B before A's latest receipt": {twiceA, betweenB},
		"no head B":                   {a},
		"B received before A":         {a, reviewFixLoopAggregate("myorg/repo1", "head-b", review.VerdictApprove, now.Add(-7*day))},
		"B outside the window":        {reviewFixLoopAggregate("myorg/repo1", "head-a", review.VerdictChangesRequested, now.Add(-41*day)), reviewFixLoopAggregate("myorg/repo1", "head-b", review.VerdictApprove, now.Add(-40*day))},
		"A approved":                  {reviewFixLoopAggregate("myorg/repo1", "head-a", review.VerdictApprove, now.Add(-6*day)), reviewFixLoopAggregate("myorg/repo1", "head-b", review.VerdictApprove, now.Add(-5*day))},
		"another repo's review":       {reviewFixLoopAggregate("myorg/other", "head-a", review.VerdictChangesRequested, now.Add(-6*day)), reviewFixLoopAggregate("myorg/other", "head-b", review.VerdictApprove, now.Add(-5*day))},
	}
	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			withACMMReviewArtifact(t, review.Artifact{Items: items})
			c := acmmResultFor(t, acmmWaiverServer(t, acmmReviewApplyMux(t)).evaluateAllRepos(), acmmReviewApplyID)
			if c.Passed {
				t.Fatalf("credited without a qualifying review-fix cycle: satisfied_by=%q reason=%q", c.SatisfiedBy, c.SatisfiedReason)
			}
		})
	}
}

func TestEvaluateAllRepos_ReviewApplyFileDetectionKeepsSatisfiedByEmpty(t *testing.T) {
	now := time.Now()
	withACMMReviewArtifact(t, review.Artifact{Items: []review.Aggregate{
		reviewFixLoopAggregate("myorg/repo1", "head-a", review.VerdictChangesRequested, now.Add(-6*24*time.Hour)),
		reviewFixLoopAggregate("myorg/repo1", "head-b", review.VerdictApprove, now.Add(-5*24*time.Hour)),
	}})
	c := acmmResultFor(t, acmmWaiverServer(t, acmmReviewApplyMux(t, "auto-review.yml")).evaluateAllRepos(), acmmReviewApplyID)
	if !c.Passed || c.Waived || c.SatisfiedBy != "" {
		t.Fatalf("file detection: passed=%v waived=%v satisfied_by=%q, want passed by detection with empty satisfied_by", c.Passed, c.Waived, c.SatisfiedBy)
	}
}

func TestACMMIssueContent_ReviewApplyNamesAlternatives(t *testing.T) {
	var reviewApply, other *ACMMCriterion
	for i := range universalCriteria {
		switch universalCriteria[i].ID {
		case acmmReviewApplyID:
			reviewApply = &universalCriteria[i]
		case "acmm:ai-fix-workflow":
			other = &universalCriteria[i]
		}
	}
	if reviewApply == nil || other == nil {
		t.Fatal("fixture criteria missing from universalCriteria")
	}

	_, body := acmmIssueContent(reviewApply)
	for _, want := range []string{
		"workflow file names only",
		"token",
		"Hive's review-fix loop",
		".acmm.yml",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review-apply gap issue missing %q; got:\n%s", want, body)
		}
	}
	for _, p := range reviewApply.Patterns {
		if !strings.Contains(body, "`"+p+"`") {
			t.Errorf("review-apply gap issue missing pattern %q", p)
		}
	}

	_, otherBody := acmmIssueContent(other)
	if !strings.Contains(otherBody, "Create one of the files listed above. The ACMM evaluation checks for file existence") {
		t.Errorf("other criteria lost the standard how-to-fix text; got:\n%s", otherBody)
	}
	if strings.Contains(otherBody, "review-fix loop") {
		t.Errorf("review-apply wording leaked into another criterion's gap issue:\n%s", otherBody)
	}
}
