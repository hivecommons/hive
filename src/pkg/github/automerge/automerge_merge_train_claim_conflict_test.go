package automerge

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

func claimPR(n int, login, body string, at time.Time) *gh.PullRequest {
	return &gh.PullRequest{
		Number: gh.Int(n), Title: gh.String("t"), Body: gh.String(body), State: gh.String("open"),
		CreatedAt: &gh.Timestamp{Time: at}, MergeableState: gh.String("clean"),
		User: &gh.User{Login: gh.String(login)},
		Head: &gh.PullRequestBranch{SHA: gh.String("sha-" + login)},
		Base: &gh.PullRequestBranch{Ref: gh.String("v5")},
	}
}

// hivecommons/hive#15810: the hive's own #15783 (self lane) and a trusted
// human's #15793 (trusted lane) both closed #15773 and both marched toward
// merge because each lane's one-PR-per-issue rule saw only its own PRs.
// Now any lane candidate whose issue another open PR of the repo claims is
// held as a claim conflict — in every lane, so neither is admitted — with
// the log line naming the pair.
func TestPlanClaimConflictsHoldsCrossLanePair(t *testing.T) {
	var logs bytes.Buffer
	c := &Engine{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	t0 := time.Date(2026, 11, 3, 10, 25, 0, 0, time.UTC)
	hive, human := claimPR(15783, "hive-app[bot]", "Closes #15773", t0), claimPR(15793, "clubanderson", "Fixes #15773", t0.Add(28*time.Minute))
	prs := []*gh.PullRequest{hive, human}
	selfOnly := func(p *gh.PullRequest) string {
		if p.GetUser().GetLogin() != "hive-app[bot]" {
			return "author:not-self"
		}
		return ""
	}
	trustedOnly := func(p *gh.PullRequest) string {
		if p.GetUser().GetLogin() != "clubanderson" {
			return "author:not-trusted"
		}
		return ""
	}

	self := &mergeTrainPlan{lane: stageLaneSelf, displayRepo: "o/r"}
	kept := c.planDuplicateHolds(context.Background(), self, stageLaneSelf, "o", "r", "o/r", prs, selfOnly)
	if len(kept) != 1 || kept[0].GetNumber() != 15793 {
		t.Fatalf("self lane kept %v, want only the other lane's PR left (itself not a self candidate)", kept)
	}
	h, held := self.claimConflictOf(15783)
	if !held || h.Other != 15793 || h.Issue != 15773 {
		t.Fatalf("self hold = %+v held=%v", h, held)
	}
	if _, dup := self.duplicateOf(15783); dup {
		t.Fatal("a cross-lane pair is a claim conflict, not a same-lane duplicate pick")
	}

	trusted := &mergeTrainPlan{lane: stageLaneTrusted, displayRepo: "o/r"}
	c.planDuplicateHolds(context.Background(), trusted, stageLaneTrusted, "o", "r", "o/r", prs, trustedOnly)
	h, held = trusted.claimConflictOf(15793)
	if !held || h.Other != 15783 || h.Issue != 15773 {
		t.Fatalf("trusted hold = %+v held=%v", h, held)
	}
	if !strings.Contains(logs.String(), "same-issue claim conflict undecided: #15783 vs #15793") {
		t.Fatalf("log missing the pair:\n%s", logs.String())
	}

	reason := c.mergeTrainClaimConflictSkip(self, 15783)
	mt, ok := prstage.ParseMergeTrain(reason)
	if !ok || mt.State != prstage.MergeTrainStateClaimConflict || mt.Front != 15793 || mt.Position != 15773 {
		t.Fatalf("reason %q parsed %+v ok=%v", reason, mt, ok)
	}
	if !strings.HasPrefix(mt.Sentence(), "held · duplicate of #15793 · needs decision") {
		t.Fatalf("sentence = %q", mt.Sentence())
	}
}

// A recorded decision on the candidate's head is the answer the gate waits
// for: that PR is admitted (the other lane's still holds its own); an
// adoption pair (`Supersedes #N`) is one change in flight, not a conflict;
// a `Refs` line is not a claim.
func TestPlanClaimConflictsDecidedOrAdoptionAdmits(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 10, 25, 0, 0, time.UTC)
	all := func(*gh.PullRequest) string { return "" }
	hiveOnly := func(p *gh.PullRequest) string {
		if p.GetUser().GetLogin() != "hive-app[bot]" {
			return "author:not-self"
		}
		return ""
	}

	store := review.NewHumanChangeRequests("")
	if err := store.Record(review.HumanChangeRequest{Repo: "o/r", Number: 15793, HeadSHA: "sha-clubanderson", Source: review.HumanChangeRequestSourceDecision, SourceID: 1, Actor: "clubanderson", Body: "approve"}); err != nil {
		t.Fatal(err)
	}
	c := &Engine{humanChangeRequests: store}
	prs := []*gh.PullRequest{claimPR(15783, "hive-app[bot]", "Closes #15773", t0), claimPR(15793, "clubanderson", "Fixes #15773", t0.Add(time.Hour))}
	trusted := &mergeTrainPlan{lane: stageLaneTrusted}
	kept := c.planDuplicateHolds(context.Background(), trusted, stageLaneTrusted, "o", "r", "o/r", prs, func(p *gh.PullRequest) string {
		if p.GetUser().GetLogin() != "clubanderson" {
			return "author:not-trusted"
		}
		return ""
	})
	if _, held := trusted.claimConflictOf(15793); held || len(kept) != 2 {
		t.Fatalf("a decided head is admitted; held=%v kept=%d", held, len(kept))
	}
	self := &mergeTrainPlan{lane: stageLaneSelf}
	c.planDuplicateHolds(context.Background(), self, stageLaneSelf, "o", "r", "o/r", prs, hiveOnly)
	if _, held := self.claimConflictOf(15783); !held {
		t.Fatal("the undecided side stays held")
	}

	c = &Engine{}
	adoption := []*gh.PullRequest{claimPR(15783, "hive-app[bot]", "Closes #15773\n\nSupersedes #15793", t0), claimPR(15793, "clubanderson", "Fixes #15773", t0.Add(time.Hour))}
	plan := &mergeTrainPlan{lane: stageLaneSelf}
	c.planDuplicateHolds(context.Background(), plan, stageLaneSelf, "o", "r", "o/r", adoption, hiveOnly)
	if plan.claimConflicts != nil {
		t.Fatalf("an adoption pair is not a conflict: %+v", plan.claimConflicts)
	}

	refs := []*gh.PullRequest{claimPR(15783, "hive-app[bot]", "Closes #15773", t0), claimPR(15793, "clubanderson", "Refs #15773", t0.Add(time.Hour))}
	plan = &mergeTrainPlan{lane: stageLaneSelf}
	c.planDuplicateHolds(context.Background(), plan, stageLaneSelf, "o", "r", "o/r", refs, hiveOnly)
	if plan.claimConflicts != nil {
		t.Fatalf("a Refs line is not a claim: %+v", plan.claimConflicts)
	}

	// Two candidates of one lane stay with the one-PR-per-issue winner
	// rule, which already sees them both.
	plan = &mergeTrainPlan{lane: stageLaneSelf}
	c.planDuplicateHolds(context.Background(), plan, stageLaneSelf, "o", "r", "o/r", prs, all)
	if plan.claimConflicts != nil {
		t.Fatalf("same-lane pair is the duplicate rule's: %+v", plan.claimConflicts)
	}
	if _, dup := plan.duplicateOf(15793); !dup {
		t.Fatal("same-lane loser held as duplicate")
	}
}

// A targeted (webhook-nudged) pass hands the lane only the nudged PR; the
// gate still groups against the full list the legacy /pulls listing
// returned for the repo this pass, so a CI-green nudge for the human's PR
// alone does not slip past the undecided conflict with the hive's.
func TestPlanClaimConflictsSeesClaimantsOutsideNarrowedList(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 10, 25, 0, 0, time.UTC)
	hive, human := claimPR(15783, "hive-app[bot]", "Closes #15773", t0), claimPR(15793, "clubanderson", "Fixes #15773", t0.Add(time.Hour))
	c := &Engine{}
	c.rememberClaimants("o/r", []*gh.PullRequest{hive, human})
	plan := &mergeTrainPlan{lane: stageLaneTrusted}
	kept := c.planDuplicateHolds(context.Background(), plan, stageLaneTrusted, "o", "r", "o/r", []*gh.PullRequest{human}, func(*gh.PullRequest) string { return "" })
	if h, held := plan.claimConflictOf(15793); !held || h.Other != 15783 || h.Issue != 15773 || len(kept) != 0 {
		t.Fatalf("narrowed pass: hold=%+v held=%v kept=%d", h, held, len(kept))
	}
	// Another repo's memory does not leak in.
	plan = &mergeTrainPlan{lane: stageLaneTrusted}
	c.planDuplicateHolds(context.Background(), plan, stageLaneTrusted, "o", "other", "o/other", []*gh.PullRequest{human}, func(*gh.PullRequest) string { return "" })
	if plan.claimConflicts != nil {
		t.Fatalf("other repo held: %+v", plan.claimConflicts)
	}
}
