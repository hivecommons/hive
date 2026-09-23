package review

import (
	"strings"
	"testing"
	"time"
)

// The fix gate (hivecommons/hive#8421): a review-fix kick tells an agent to
// push onto the PR branch, so it may only follow a changes_requested verdict
// on a PR the hive itself opened, unless review.fix_human_prs is on.
// all_authors widens what is REVIEWED and must never imply pushing.

func changesRequestedFor(pr PullRequest) Aggregate {
	return Aggregate{Repo: "acme/" + pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Verdict: VerdictChangesRequested,
		Reasons: []string{"guard nil input"}}
}

func fixGateOpts(fixHumanPRs bool) DispatchOptions {
	return DispatchOptions{
		RequireApproval: true,
		FanOut:          true,
		AllAuthors:      true,
		FixHumanPRs:     fixHumanPRs,
		PostComments:    true,
		ProjectOrg:      "acme",
		AIAuthor:        "hive-bot[bot]",
		Agents: []AgentCapability{
			reviewer("r1"),
			{Name: DefaultFixerAgent, Enabled: true, UsesKick: true},
		},
	}
}

// TestHumanPRFixWithheldWhenFixHumanPRsOff is the incident: all_authors on,
// a person's PR, changes_requested. The review stands; nothing is pushed;
// the refusal is recorded once with the PR, the author and the setting.
func TestHumanPRFixWithheldWhenFixHumanPRsOff(t *testing.T) {
	pr := humanPR("sha1")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(false))

	if len(plan.FixKicks) != 0 {
		t.Fatalf("fix kick dispatched for a human-authored PR with fix_human_prs off: %+v", plan.FixKicks)
	}
	if len(plan.State.Human) != 0 {
		t.Fatalf("withholding a push is not a human hold: %+v", plan.State.Human)
	}
	if len(plan.WithheldFixes) != 1 || len(plan.State.Withheld) != 1 {
		t.Fatalf("withheld = plan %d / state %d, want 1 / 1", len(plan.WithheldFixes), len(plan.State.Withheld))
	}
	w := plan.WithheldFixes[0]
	if w.Repo != "acme/hive" || w.Number != 7 || w.HeadSHA != "sha1" {
		t.Fatalf("withheld entry names the wrong PR: %+v", w)
	}
	if w.Author != "clubanderson" {
		t.Fatalf("withheld entry must carry the PR author for the audit trail, got %q", w.Author)
	}
	if w.Setting != WithheldFixSetting {
		t.Fatalf("withheld entry must cite the setting that permits the push, got %q", w.Setting)
	}
	// Pin the literal: the audit trail and the docs name this key, so the
	// constant cannot drift away from it unnoticed.
	if WithheldFixSetting != "review.fix_human_prs" {
		t.Fatalf("WithheldFixSetting = %q, want review.fix_human_prs", WithheldFixSetting)
	}
	if w.Withheld.IsZero() {
		t.Fatal("withheld entry has no timestamp")
	}

	// Reported once per head: the next cycle finds the state entry and
	// produces nothing new to audit, while the state keeps the record.
	again := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, plan.State, fixGateOpts(false))
	if len(again.WithheldFixes) != 0 {
		t.Fatalf("refusal re-reported on the next cycle: %+v", again.WithheldFixes)
	}
	if len(again.State.Withheld) != 1 || len(again.FixKicks) != 0 {
		t.Fatalf("second cycle: withheld=%d fixes=%d, want 1 / 0", len(again.State.Withheld), len(again.FixKicks))
	}
}

// TestHumanPRFixDispatchedWhenFixHumanPRsOn is the opt-in: the operator
// granted the push, so the fixer is kicked exactly as for the hive's own PRs.
func TestHumanPRFixDispatchedWhenFixHumanPRsOn(t *testing.T) {
	pr := humanPR("sha1")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(true))

	if len(plan.FixKicks) != 1 || plan.FixKicks[0].Agent != DefaultFixerAgent {
		t.Fatalf("fix kick with fix_human_prs on = %+v, want one to %s", plan.FixKicks, DefaultFixerAgent)
	}
	if len(plan.WithheldFixes) != 0 || len(plan.State.Withheld) != 0 {
		t.Fatalf("nothing should be withheld when the push is permitted: %+v", plan.State.Withheld)
	}
	if !strings.Contains(plan.FixKicks[0].Message, "push a new commit to the PR branch") {
		t.Fatalf("fix prompt no longer asks for the push:\n%s", plan.FixKicks[0].Message)
	}
}

// TestAgentPRFixUnaffectedByFixHumanPRs guards the hive's own PRs: the gate
// is about other people's branches, and must not touch agent-authored work
// in either position of the toggle.
func TestAgentPRFixUnaffectedByFixHumanPRs(t *testing.T) {
	for _, on := range []bool{false, true} {
		pr := dispatchPR("sha1")
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(on))
		if len(plan.FixKicks) != 1 {
			t.Fatalf("fix_human_prs=%v: agent-authored PR got %d fix kicks, want 1", on, len(plan.FixKicks))
		}
		if len(plan.WithheldFixes) != 0 {
			t.Fatalf("fix_human_prs=%v: agent-authored PR was withheld: %+v", on, plan.WithheldFixes)
		}
	}
}

// TestHiveMediatedPRUnderPersonLoginIsStillFixed: a PR an agent opened on a
// person's credentials shows the person as author. The attribution trailer
// and the audit-trail agent are the two ways the hive recognises it as its
// own, and either one keeps the fix path open without the toggle.
func TestHiveMediatedPRUnderPersonLoginIsStillFixed(t *testing.T) {
	attributed := humanPR("sha1")
	attributed.HiveAttributed = true
	audited := humanPR("sha1")
	audited.AuthorAgent = "scanner"
	for name, pr := range map[string]PullRequest{"hive_attributed": attributed, "author_agent": audited} {
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(false))
		if len(plan.FixKicks) != 1 || len(plan.WithheldFixes) != 0 {
			t.Fatalf("%s: fixes=%d withheld=%d, want 1 / 0", name, len(plan.FixKicks), len(plan.WithheldFixes))
		}
	}
}

// TestWithheldFixPrunedWhenHeadMoves: a new push is a new decision. The
// record for the old head goes with it, like pending reviews and holds.
func TestWithheldFixPrunedWhenHeadMoves(t *testing.T) {
	state := DispatchState{Withheld: []WithheldFix{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Setting: WithheldFixSetting, Withheld: time.Now().UTC()}}}
	pr := humanPR("sha2")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, state, fixGateOpts(false))
	if len(plan.State.Withheld) != 0 {
		t.Fatalf("stale withheld entry survived a head change: %+v", plan.State.Withheld)
	}
}

// TestHumanPRReviewPromptProposesFixesInComment: with the push withheld, the
// reviewer is told to carry the fix in the comment as a suggestion or patch,
// and not to touch the branch. The hive's own PRs, and a hive that opted
// in, get the ordinary prompt.
func TestHumanPRReviewPromptProposesFixesInComment(t *testing.T) {
	const marker = "PROPOSE FIXES, DO NOT PUSH THEM."
	cases := []struct {
		name     string
		pr       PullRequest
		on       bool
		combined bool
		want     bool
	}{
		{name: "human_off", pr: humanPR("sha1"), want: true},
		{name: "human_off_combined", pr: humanPR("sha1"), combined: true, want: true},
		{name: "human_on", pr: humanPR("sha1"), on: true},
		{name: "agent_off", pr: dispatchPR("sha1")},
	}
	for _, tc := range cases {
		opts := fixGateOpts(tc.on)
		opts.CombinedPerspectives = tc.combined
		plan := PlanDispatch([]PullRequest{tc.pr}, Artifact{}, DispatchState{}, opts)
		if len(plan.ReviewKicks) != 1 {
			t.Fatalf("%s: got %d review kicks, want 1", tc.name, len(plan.ReviewKicks))
		}
		msg := plan.ReviewKicks[0].Message
		if got := strings.Contains(msg, marker); got != tc.want {
			t.Fatalf("%s: propose-only instruction present=%v, want %v:\n%s", tc.name, got, tc.want, msg)
		}
		if tc.want && (!strings.Contains(msg, "```suggestion") || !strings.Contains(msg, "```diff") || !strings.Contains(msg, "review.fix_human_prs")) {
			t.Fatalf("%s: propose-only instruction lacks the suggestion/patch guidance or the setting name:\n%s", tc.name, msg)
		}
	}
}
