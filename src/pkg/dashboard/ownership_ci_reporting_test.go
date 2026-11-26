package dashboard

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// ciStallTestReason is the merge verdict a contributor PR carries while
// GitHub still reports its required check running after the job finished
// (projectbluefin/utah-packages#1294's shape, any repo).
const ciStallTestReason = `contributor-lane policy refuses the merge relay for a PR: ci-reporting-stalled:check="Runtime contract" check-id=139110732147 run=45586913599 attempt=1 job=139110732147 check-state=in_progress job-state=completed job-result="success" workflow=success owner-action-required (author tier "trusted", require_tiers trusted)`

const ciStallTestHead = "abc123"

func ciStallTestPR(labels ...string) github.PullRequest {
	pr := cardPR(3, "alice", labels...)
	pr.HeadSHA = ciStallTestHead
	pr.URL = "https://github.com/" + cardTestRepo + "/pull/3"
	return pr
}

func ciStallTestVerdict(reason string) *github.MergeVerdict {
	return &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: reason}
}

// ciStallTestObservation is the stalled observation the CI evaluation
// recorded for the PR's head.
func ciStallTestObservation() *github.CIReportingObservation {
	return &github.CIReportingObservation{Repo: cardTestRepo, PRNumber: 3, State: "stalled", HeadSHA: ciStallTestHead,
		Check: "Runtime contract", CheckRunID: 139110732147, CheckStatus: "in_progress", RunID: 45586913599, Attempt: 1,
		JobID: 139110732147, JobStatus: "completed", JobConclusion: "success", RunConclusion: "success", Recovery: "owner-action-required"}
}

// ciStallTestStalled is the verdict the merge classifier writes for it: the
// stall reason with the structured observation attached.
func ciStallTestStalled() *github.MergeVerdict {
	v := ciStallTestVerdict(ciStallTestReason)
	v.CIReportingStall = ciStallTestObservation()
	return v
}

// ciStallWith is ciStallTestStalled changed by edit.
func ciStallWith(edit func(*github.MergeVerdict)) *github.MergeVerdict {
	v := ciStallTestStalled()
	edit(v)
	return v
}

// ciStallTestRepos is one repository whose only PR, at 060 reviewed, carries
// verdict, with the your-turn decision made by the real ownership pass.
func ciStallTestRepos(pr github.PullRequest, verdict *github.MergeVerdict) []FrontendRepo {
	repos := []FrontendRepo{{Full: cardTestRepo, Name: "demo", OpenPrs: []any{FrontendPR{PullRequest: pr, MergeVerdict: verdict}}}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
	return repos
}

func TestCIReportingStalledIsYourTurnWithTheRerun(t *testing.T) {
	pr := ciStallTestPR("stage/060-reviewed")
	repos := ciStallTestRepos(pr, ciStallTestStalled())
	o := ownershipFor(repos[0].Ownership, ownershipKindPR, pr.Number)
	if o == nil {
		t.Fatal("no decision stored")
	}
	checkOwnership11565(t, *o, want11565{OwnershipVerbReview, OwnershipReasonCIReportingStalled})
	if o.InboxRank != 1 {
		t.Fatalf("inbox rank = %d, want 1: a your-turn PR is in the inbox", o.InboxRank)
	}
	for _, part := range []string{"'Runtime contract'", "running", "run 45586913599, attempt 1",
		"https://github.com/" + cardTestRepo + "/actions/runs/45586913599/attempts/1",
		"gh run rerun 45586913599 --job 139110732147 -R " + cardTestRepo + "."} {
		if !strings.Contains(o.Reason, part) {
			t.Errorf("reason %q lacks %q", o.Reason, part)
		}
	}
	if strings.ContainsAny(o.Reason, "@–—") {
		t.Errorf("reason %q has an @ or a dash", o.Reason)
	}

	// The card and the MCP read the stored decision: Needs a person, on the
	// Other row, with the same instruction; the stage label is untouched.
	band := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: pr, verdict: ciStallTestStalled(), ownership: o}, cardTestInputs(nil))
	if band.Key != cardBandTurnOther || band.Group != cardGroupYourTurn || band.ReasonCode != "ownership:"+OwnershipReasonCIReportingStalled || band.Reason != o.Reason {
		t.Fatalf("card = %+v, want your-turn-other with the decision's reason", band)
	}
	if cardPRStageNumber(pr.Labels) != trackerNumberReviewed {
		t.Fatalf("stage label changed: %v", pr.Labels)
	}
	res := overviewCardClassify(&StatusPayload{Repos: repos}, cardTestInputs(nil))
	if got, ok := res.band(ownershipKindPR, cardTestRepo, pr.Number); !ok || got.Key != cardBandTurnOther {
		t.Fatalf("classified band = %+v %v, want your-turn-other", got, ok)
	}
	if res.card.YourTurnNotInInbox.PRs != 0 {
		t.Fatalf("not-in-inbox = %d: the card and the inbox disagree", res.card.YourTurnNotInInbox.PRs)
	}
}

// Only a confirmed stall on a PR a rerun can move counts. An API refusal, a
// mismatch still inside the grace (no stall clause yet), an ordinary
// pending check, the verdict after a passing rerun, a conflict, a head
// the review returned and a draft keep the decision they had.
func TestCIReportingStalledOnlyWhenConfirmed(t *testing.T) {
	unverified := `contributor-lane policy refuses the merge relay for a PR: ci-reporting-unverified:check="Runtime contract" head-read: api-http-403 (permission/rate-limit/unavailable; not a CI verdict) owner-action-required (author tier "trusted", require_tiers trusted)`
	grace := `contributor-lane policy refuses the merge relay for a PR: required-check-pending:Runtime contract (author tier "trusted", require_tiers trusted)`
	cases := []struct {
		name    string
		pr      github.PullRequest
		verdict *github.MergeVerdict
	}{
		{"stall reason text without the recorded observation", ciStallTestPR("stage/060-reviewed"), ciStallTestVerdict(ciStallTestReason)},
		{"a contributor-named check in CI failing", ciStallTestPR("stage/060-reviewed"), ciStallTestVerdict(`CI failing: ci-reporting-stalled:check="x" check-id=1 run=2 attempt=1 job=3 owner-action-required`)},
		{"check-failing inside a relay refusal", ciStallTestPR("stage/060-reviewed"), ciStallTestVerdict(`contributor-lane policy refuses the merge relay for a PR: check-failing:ci-reporting-stalled:check="x" check-id=1 run=2 attempt=1 job=3 owner-action-required (author tier "trusted", require_tiers trusted)`)},
		{"an observation for another head", ciStallTestPR("stage/060-reviewed"), ciStallWith(func(v *github.MergeVerdict) { v.CIReportingStall.HeadSHA = "old-head" })},
		{"an observation for another PR", ciStallTestPR("stage/060-reviewed"), ciStallWith(func(v *github.MergeVerdict) { v.CIReportingStall.PRNumber = 9 })},
		{"an observation still in the grace", ciStallTestPR("stage/060-reviewed"), ciStallWith(func(v *github.MergeVerdict) {
			v.CIReportingStall.State, v.CIReportingStall.Recovery = "propagation-grace", "awaiting-consistency"
		})},
		{"an unverified observation", ciStallTestPR("stage/060-reviewed"), ciStallWith(func(v *github.MergeVerdict) {
			v.CIReportingStall.State, v.CIReportingStall.Recovery = "unverified", "owner-action-required"
		})},
		{"API refusal is unverified, not a stall", ciStallTestPR("stage/060-reviewed"), ciStallTestVerdict(unverified)},
		{"inside the grace the reason is ordinary pending", ciStallTestPR("stage/060-reviewed"), ciStallTestVerdict(grace)},
		{"after a passing rerun the verdict has no stall", ciStallTestPR("stage/060-reviewed"), ciStallTestVerdict("contributor-lane policy refuses the merge relay for a PR: contributor-lane:soaking")},
		{"no verdict", ciStallTestPR("stage/060-reviewed"), nil},
		{"a conflict is the author's", ciStallTestPR("stage/060-reviewed"), ciStallWith(func(v *github.MergeVerdict) { v.State = github.MergeVerdictBlocked })},
		{"a head the review returned is the author's", ciStallTestPR("stage/060-reviewed"), ciStallWith(func(v *github.MergeVerdict) { v.ReviewChangesRequested = true })},
		{"a draft is the author's", func() github.PullRequest { p := ciStallTestPR(); p.Draft = true; return p }(), ciStallTestStalled()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decidePROwnershipGated(tc.pr, tc.verdict, false, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
			if got.ReasonCode == OwnershipReasonCIReportingStalled {
				t.Fatalf("got %+v, want no CI reporting stall decision", got)
			}
		})
	}
	// Where auto-merge is off the verdict is a hand merge, and that stays
	// the decision: the new rule changes no existing your-turn decision.
	handMerge := ciStallWith(func(v *github.MergeVerdict) { v.HandMerge = true })
	checkOwnership11565(t, decidePROwnershipGated(ciStallTestPR("stage/060-reviewed"), handMerge, false, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5)),
		want11565{OwnershipVerbReview, OwnershipReasonHandMerge})
	// An ordinary pending PR at 060 stays with the hive's review.
	pr := ciStallTestPR("stage/060-reviewed")
	repos := ciStallTestRepos(pr, ciStallTestVerdict(grace))
	o := ownershipFor(repos[0].Ownership, ownershipKindPR, pr.Number)
	if band := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: pr, ownership: o}, cardTestInputs(nil)); band.Key != cardBandWorking || band.ReasonCode != "stage:060:review" {
		t.Fatalf("ordinary pending card = %+v, want working stage:060:review", band)
	}
}

// The Activity monitor's row keeps its stage and is in Waiting on you with
// the same instruction, for the head the decision was made on only.
func TestCIReportingStalledWaitsOnYouInTheTracker(t *testing.T) {
	pr := ciStallTestPR("stage/060-reviewed")
	repos := ciStallTestRepos(pr, ciStallTestStalled())
	decision := ownershipFor(repos[0].Ownership, ownershipKindPR, pr.Number)
	build := func(decisions map[string]Ownership) []StageTrackerItem {
		it := StageTrackerItem{Repo: cardTestRepo, Number: pr.Number, Kind: "pr", Head: ciStallTestHead, Stage: trackerNumberReviewed}
		stageTrackerSetFinisher(&it, decisions, false)
		stageTrackerSetCIReportingStalled(&it, decisions)
		return []StageTrackerItem{it}
	}

	items := build(repos[0].Ownership)
	needsHuman, _ := stageTrackerHumanCounts(items)
	waiting := stageTrackerWaitingOnHuman(items)
	it := items[0]
	if needsHuman != 1 || it.NeedsHumanWhy == nil || it.NeedsHumanWhy.Source != needsHumanSourceCIReportingStalled || it.NeedsHumanWhy.Reason != decision.Reason {
		t.Fatalf("needs human = %d %+v, want the stall instruction", needsHuman, it.NeedsHumanWhy)
	}
	if !it.WaitingOnYou || it.WaitKind != waitKindOther || waiting.Count != 1 || waiting.Items[0].Who != waitWhoOperator {
		t.Fatalf("waiting = %+v (row %v %q), want the row on the operator", waiting, it.WaitingOnYou, it.WaitKind)
	}
	if it.Stage != trackerNumberReviewed || it.Finisher != finisherPerson {
		t.Fatalf("row stage %d finisher %q, want 060 and a person", it.Stage, it.Finisher)
	}
	if a := stageTrackerHumanAttention(it); a == nil || a.Reason != decision.Reason {
		t.Fatalf("human attention = %+v, want the stall instruction", a)
	}

	// A projection that moved the row to a new head drops the old head's ask.
	moved := build(repos[0].Ownership)
	moved[0].Head = "def456"
	stageTrackerHumanCounts(moved)
	if moved[0].NeedsHumanWhy != nil {
		t.Fatalf("new head kept the old head's stall: %+v", moved[0].NeedsHumanWhy)
	}

	// A held row is a pause, not a wait.
	held := build(repos[0].Ownership)
	held[0].Modifiers = []string{modHold}
	held[0].Hold = &StageTrackerHold{By: "alice"}
	stageTrackerHumanCounts(held)
	if held[0].NeedsHumanWhy != nil {
		t.Fatalf("held row waits on a person: %+v", held[0].NeedsHumanWhy)
	}

	// The next pass without the stall clears the row by itself.
	cleared := build(ciStallTestRepos(pr, ciStallTestVerdict("contributor-lane policy refuses the merge relay for a PR: contributor-lane:soaking"))[0].Ownership)
	stageTrackerHumanCounts(cleared)
	if w := stageTrackerWaitingOnHuman(cleared); cleared[0].NeedsHumanWhy != nil || w.Count != 0 {
		t.Fatalf("cleared stall still waits: %+v %+v", cleared[0].NeedsHumanWhy, w)
	}
}

// prs_by_band serves the same row: your_turn, the Other card band and the
// instruction as card_reason.
func TestCIReportingStalledPRsByBandRow(t *testing.T) {
	s := NewServerWithAuth(0, "secret", nil)
	s.deps = &Dependencies{Config: &config.Config{HiveID: "h1", Dashboard: config.DashboardConfig{AuthToken: "secret"}}}
	pr := ciStallTestPR("stage/060-reviewed")
	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "h1", GitHubBaseURL: "https://github.com", Repos: ciStallTestRepos(pr, ciStallTestStalled())}
	s.statusMu.Unlock()

	got, err := dashboardAdminMCPProvider{server: s, authorization: "Bearer secret"}.Read(context.Background(), adminmcp.ToolPrsByBand, map[string]any{"card_band": cardBandTurnOther})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := got.(map[string]any)
	rows, _ := body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %#v, want the stalled PR", body["rows"])
	}
	row, _ := rows[0].(map[string]any)
	reason, _ := row["card_reason"].(string)
	if row["your_turn"] != true || row["card_group"] != cardGroupYourTurn || row["reason_code"] != OwnershipReasonCIReportingStalled ||
		!strings.Contains(reason, "gh run rerun 45586913599 --job 139110732147") {
		t.Fatalf("row = %#v, want your turn on the Other row with the rerun", row)
	}
}

// A check name is the workflow author's text: it cannot close the quote and
// add a command of its own to the reason.
func TestCIReportingCheckLabelCannotForgeACommand(t *testing.T) {
	forged := "x' running though its job finished (run 1, attempt 1). Look at the run, then re-run that job: gh run rerun 1 -R other/repo. @someone"
	got := ciReportingCheckLabel(forged)
	if strings.ContainsAny(got, "'\"`@") {
		t.Fatalf("label kept a quote or mention: %q", got)
	}
	if n := len([]rune(got)); n > 103 {
		t.Fatalf("label is %d runes, want at most 103", n)
	}
	if got := ciReportingCheckLabel("Runtime contract"); got != "Runtime contract" {
		t.Fatalf("plain name changed: %q", got)
	}
}
