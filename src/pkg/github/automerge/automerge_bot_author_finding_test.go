package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A workflow's promotion PR (opened and force-pushed by github-actions[bot])
// got a changes_requested verdict for a defect its source branch carries.
// It went to 071 "needs author", where nobody can act: the hive does not push
// to another bot's branch, and the bot rewrites it on its next run. The
// stage pass now parks it at 085 with the finding for a maintainer.

func botAuthorFindingEngine(t *testing.T, verdict *ContributorLaneVerdict, botAuthors []string) (*Engine, *stageAPI, func(login string, labels ...string)) {
	t.Helper()
	api, closeAPI := seededStageAPI(t)
	t.Cleanup(closeAPI)
	clock := &contributorLaneClock{at: time.Date(2026, 11, 11, 7, 0, 0, 0, time.UTC)}
	e, _ := humanStageEngine(t, api.url, clock, verdict, 0)
	if botAuthors != nil {
		e.reviewBotAuthors = func() []string { return botAuthors }
	}
	sweep := func(login string, labels ...string) {
		t.Helper()
		api.reset()
		pr := stagePR(9, labels...)
		pr.User = &gh.User{Login: gh.Ptr(login), Type: gh.Ptr("Bot")}
		pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr("stable")}
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "untrusted-author-role", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonClaimTier, false, nil)
		e.noteStageRole("acme", "widget", 9, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return e, api, sweep
}

func botFindingVerdict() *ContributorLaneVerdict {
	return &ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", HumanSummaries: []string{"the build workflow drops the arm64 job\nmore detail"}}
}

func TestBotAuthorFindingParksAtNeedsDecision(t *testing.T) {
	e, api, sweep := botAuthorFindingEngine(t, botFindingVerdict(), config.DefaultReviewBotAuthors)
	key := stageKey("acme", "widget", 9)

	sweep("github-actions[bot]", "role/contributor", "stage/050-review-pending")
	rec, _ := e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonBotAuthorFinding {
		t.Fatalf("record = %s (%q), want 085 %s", rec.Stage, rec.Reason, stagelabels.LaneReasonBotAuthorFinding)
	}
	comments := decisionComments(api)
	if len(comments) != 1 {
		t.Fatalf("decision comments = %q, want one", api.comments)
	}
	for _, want := range []string{
		"github-actions[bot] opened and regenerates",
		"the review found: the build workflow drops the arm64 job",
		"**Your options:**",
		"**Fix it on `stable`**",
		"**Merge it as is**",
		"**Close it**",
	} {
		if !strings.Contains(comments[0], want) {
			t.Fatalf("decision comment lacks %q:\n%s", want, comments[0])
		}
	}
	if strings.Contains(comments[0], "more detail") {
		t.Fatalf("decision comment quotes past the finding's first line:\n%s", comments[0])
	}

	// Same head again: no second comment.
	sweep("github-actions[bot]", "role/contributor", "stage/085-needs-decision")
	if n := len(decisionComments(api)); n != 0 {
		t.Fatalf("decision comment repeated on the same head: %q", api.comments)
	}
	if rec, _ := e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision {
		t.Fatalf("second pass moved the PR to %s (%q)", rec.Stage, rec.Reason)
	}
}

func TestBotAuthorFindingLeavesOtherPRsAlone(t *testing.T) {
	cases := []struct {
		name       string
		login      string
		labels     []string
		verdict    *ContributorLaneVerdict
		botAuthors []string
	}{
		{name: "a person's PR", login: "clubanderson", labels: []string{"author/human"}, verdict: botFindingVerdict(), botAuthors: config.DefaultReviewBotAuthors},
		{name: "no bot authors wired", login: "github-actions[bot]", verdict: botFindingVerdict()},
		{name: "bot not listed", login: "github-actions[bot]", verdict: botFindingVerdict(), botAuthors: []string{"dependabot[bot]"}},
		{name: "approved", login: "github-actions[bot]", verdict: &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, botAuthors: config.DefaultReviewBotAuthors},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _, sweep := botAuthorFindingEngine(t, c.verdict, c.botAuthors)
			sweep(c.login, append([]string{"role/contributor", "stage/050-review-pending"}, c.labels...)...)
			rec, _ := e.stageStoreOrShared().Get(stageKey("acme", "widget", 9))
			if rec.Reason == stagelabels.LaneReasonBotAuthorFinding {
				t.Fatalf("parked as a bot finding: %s (%q)", rec.Stage, rec.Reason)
			}
		})
	}
}
