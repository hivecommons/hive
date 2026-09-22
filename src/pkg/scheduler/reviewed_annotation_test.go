package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// ${PR_LIST} must mark a PR whose current head already carries a hive verdict,
// keyed exactly as the dispatch lane keys it: fully-qualified repo + number +
// head SHA. The enumeration may carry the bare governor repo name while the
// verdict artifact always carries owner/name (#8133), and a pushed head must
// clear the mark.
func TestReviewedAnnotation(t *testing.T) {
	verdicts := review.Artifact{Items: []review.Aggregate{
		{Repo: "projectbluefin/actions", Number: 548, HeadSHA: "fc92b3aa4c71a5b68f64f089c03a07f8af290981", Verdict: review.VerdictApprove},
	}}
	cases := []struct {
		name string
		pr   github.PullRequest
		want string
	}{
		{"bare repo, same head", github.PullRequest{Repo: "actions", Number: 548, HeadSHA: "fc92b3aa4c71a5b68f64f089c03a07f8af290981"}, " [hive-reviewed: approve@fc92b3a]"},
		{"qualified repo, same head", github.PullRequest{Repo: "projectbluefin/actions", Number: 548, HeadSHA: "fc92b3aa4c71a5b68f64f089c03a07f8af290981"}, " [hive-reviewed: approve@fc92b3a]"},
		{"new head clears the mark", github.PullRequest{Repo: "actions", Number: 548, HeadSHA: "0000000000000000000000000000000000000000"}, ""},
		{"other PR", github.PullRequest{Repo: "actions", Number: 549, HeadSHA: "fc92b3aa4c71a5b68f64f089c03a07f8af290981"}, ""},
		{"no head known", github.PullRequest{Repo: "actions", Number: 548}, ""},
	}
	for _, tc := range cases {
		if got := reviewedAnnotation(verdicts, tc.pr, "projectbluefin"); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	if got := reviewedAnnotation(review.Artifact{}, cases[0].pr, "projectbluefin"); got != "" {
		t.Errorf("empty artifact must annotate nothing, got %q", got)
	}
}

func TestFormatPRList_CarriesReviewedAnnotation(t *testing.T) {
	s := newSchedulerWithIoscan(false)
	s.cfg.Project.Org = "projectbluefin"
	dir := t.TempDir()
	orig := review.ReviewVerdictsPath
	review.ReviewVerdictsPath = dir + "/review-verdicts.json"
	t.Cleanup(func() { review.ReviewVerdictsPath = orig })
	art := review.Artifact{Items: []review.Aggregate{{Repo: "projectbluefin/actions", Number: 548, HeadSHA: "fc92b3aa4c71a5b68f64f089c03a07f8af290981", Verdict: review.VerdictRequiresHuman}}}
	if err := review.WriteArtifact(review.ReviewVerdictsPath, art); err != nil {
		t.Fatal(err)
	}
	actionable := &github.ActionableResult{}
	actionable.PRs.Items = []github.PullRequest{
		{Repo: "actions", Number: 548, Title: "gate", Author: "a", HeadSHA: "fc92b3aa4c71a5b68f64f089c03a07f8af290981"},
		{Repo: "actions", Number: 554, Title: "gate too", Author: "b", HeadSHA: "1111111111111111111111111111111111111111"},
	}
	out, _ := s.formatPRListWithPolicyForAgent(actionable, "reviewer")
	if !strings.Contains(out, "#548") || !strings.Contains(out, "[hive-reviewed: requires_human@fc92b3a]") {
		t.Fatalf("reviewed PR not annotated: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "#554") && strings.Contains(line, "hive-reviewed") {
			t.Fatalf("unreviewed PR must not be annotated: %q", line)
		}
	}
}
