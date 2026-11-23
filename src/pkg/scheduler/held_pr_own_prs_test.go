package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// cncf/techdocs#424 (hivecommons/hive#19325): the quality agent opened a
// PR whose topic and labels read as another lane's, a MEMBER requested
// changes, and the kick said "no human review yet" with nothing marking the
// PR as the agent's own. The agent decided "not mine" on every run.
func TestHeldPRCoordinationMarksOwnPRAndChangesRequested(t *testing.T) {
	s := heldPRCoordinationScheduler(t, 3, "ISSUES_AND_PRS")
	s.SetPRAuthorLookup(func() map[string]string {
		return map[string]string{"acme/widget#424": "quality", "acme/widget#500": "scanner"}
	})
	s.SetTopicClaimLookup(func() []TopicClaim {
		return []TopicClaim{{Agent: "techdocs-drafter", Key: "docs-analysis", Repo: "acme/widget", PR: 424}}
	})
	actionable := &github.ActionableResult{
		PRs: github.PRResult{Held: []github.PullRequest{
			{Repo: "acme/widget", Number: 424, Protection: &github.ProtectionFacts{
				ReviewDecision:     github.ReviewDecisionChangesRequested,
				ChangesRequestedBy: []string{"nate"},
			}},
			{Repo: "acme/widget", Number: 500, Protection: &github.ProtectionFacts{}},
		}},
		Hold: github.HoldResult{Total: 2, PRs: 2, Items: []github.HoldItem{
			{Repo: "acme/widget", Number: 424, Type: "pr", Title: "Docs analysis for widget"},
			{Repo: "acme/widget", Number: 500, Type: "pr", Title: "Scanner fix"},
		}},
	}
	msg := s.BuildAgentMessage("quality", nil, actionable)
	for _, want := range []string{
		"acme/widget#424 [YOURS: you opened it] [topic: docs-analysis] Docs analysis for widget — CHANGES REQUESTED by @nate (unaddressed)",
		"acme/widget#500 Scanner fix — no human review yet",
		"is yours whatever its lane, topic",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("kick missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "#500 [YOURS") {
		t.Fatalf("another agent's PR marked as yours:\n%s", msg)
	}
}

// A replica sees its lane's PRs as its own; a different lane does not.
func TestPRsOpenedByLaneAndRepoSpelling(t *testing.T) {
	s := heldPRCoordinationScheduler(t, 3, "ISSUES_AND_PRS")
	s.cfg.Agents["quality-2"] = config.AgentConfig{ReplicaOf: "quality"}
	s.SetPRAuthorLookup(func() map[string]string {
		return map[string]string{"acme/widget#7": "quality-2", "widget#8": "quality", "acme/widget#9": "scanner", "bad": "quality", "acme/widget#x": "quality"}
	})
	got := s.prsOpenedBy("quality")
	if len(got) != 2 {
		t.Fatalf("opened = %+v, want #7 and #8", got)
	}
	if ownPRTag(got, "acme/widget", 7) == "" || ownPRTag(got, "acme/widget", 8) == "" || ownPRTag(got, "acme/widget", 9) != "" {
		t.Fatalf("tags wrong for %+v", got)
	}
	if ownPRTag(got, "other/widget2", 7) != "" {
		t.Fatal("tag matched another repo")
	}
	s.SetPRAuthorLookup(nil)
	if got := s.prsOpenedBy("quality"); got != nil {
		t.Fatalf("no lookup = %+v", got)
	}
}

// GitHub's own decision is shown when no latest human verdict was derived.
func TestHeldPRReviewStateFallsBackToReviewDecision(t *testing.T) {
	cases := []struct {
		f    github.ProtectionFacts
		want string
	}{
		{github.ProtectionFacts{ReviewDecision: github.ReviewDecisionChangesRequested}, "CHANGES REQUESTED (unaddressed)"},
		{github.ProtectionFacts{ReviewDecision: github.ReviewDecisionChangesRequested, ChangesRequestedBy: []string{"a", "b"}}, "CHANGES REQUESTED by @a, @b (unaddressed)"},
		{github.ProtectionFacts{ReviewDecision: github.ReviewDecisionApproved}, "approved"},
		{github.ProtectionFacts{}, "no human review yet"},
		{github.ProtectionFacts{LatestHumanReviewState: github.ReviewDecisionChangesRequested, LatestHumanReviewBy: "nate",
			LatestHumanReviewSubmittedAt: time.Date(2026, 11, 18, 20, 25, 0, 0, time.UTC)}, "CHANGES REQUESTED by @nate (unaddressed)"},
	}
	for _, tc := range cases {
		f := tc.f
		if got := heldPRReviewState(github.PullRequest{Number: 1, Protection: &f}); got != tc.want {
			t.Errorf("state(%+v) = %q, want %q", tc.f, got, tc.want)
		}
	}
}
