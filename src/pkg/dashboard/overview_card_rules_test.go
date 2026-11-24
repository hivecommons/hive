package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// The card's dependency-bot helpers are read-only copies of cmd/hive rules.
// They are checked against the same truth table cmd/hive's originals are
// (cmd/hive/dependency_bot_rules_test.go), so a drift fails one side.
func TestCardDependencyBotRulesMatchTheSharedFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/dependency_bot_rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string             `json:"name"`
		PR        github.PullRequest `json:"pr"`
		RedCI     bool               `json:"red_ci"`
		Conflict  bool               `json:"conflict"`
		Candidate bool               `json:"candidate"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty fixture")
	}
	for _, tc := range cases {
		if got := cardDependencyBotRedCI(tc.PR); got != tc.RedCI {
			t.Errorf("%s: cardDependencyBotRedCI = %v, fixture says %v", tc.Name, got, tc.RedCI)
		}
		if got := cardConflictMergeState(tc.PR); got != tc.Conflict {
			t.Errorf("%s: cardConflictMergeState = %v, fixture says %v", tc.Name, got, tc.Conflict)
		}
		if got := cardBotRebaseCandidate(tc.PR); got != tc.Candidate {
			t.Errorf("%s: cardBotRebaseCandidate = %v, fixture says %v", tc.Name, got, tc.Candidate)
		}
	}
}

// cardGateAllows is a read-only copy of Gate.Decide for
// KindBotRebaseRequest. It must answer as Decide does across the policy
// matrix: level, opt-in label, per-repo mode, require_label and
// unlabeled_min_level overrides, the operator pause, listed repos, the
// backfill guard (on and off, item older or newer than the cutoff, repo
// seen or not) and the hive's own items.
func TestCardGateAllowsMatchesDecide(t *testing.T) {
	const repo = "octo/demo"
	firstSeen := cardTestNow.Add(-100 * time.Hour)
	f, t6 := false, 6
	cases := 0
	for _, level := range []int{0, 5, 6} {
		for _, labelled := range []bool{false, true} {
			for _, mode := range []writeback.RepoMode{writeback.RepoModeDefault, writeback.RepoModeOff, writeback.RepoModeIssuesOnly, writeback.RepoModeLabelsOnly, writeback.RepoModeFull} {
				for _, noRequire := range []bool{false, true} {
					for _, unlabeled := range []bool{false, true} {
						for _, paused := range []bool{false, true} {
							for _, listed := range []bool{true, false} {
								for _, older := range []bool{false, true} {
									for _, backfill := range []bool{false, true} {
										for _, hive := range []bool{false, true} {
											for _, seen := range []bool{true, false} {
												level, paused, listed := level, paused, listed
												wcfg := writeback.Config{Kinds: map[string]writeback.KindPolicyOverride{}}
												if mode != writeback.RepoModeDefault {
													wcfg.PerRepo = map[string]writeback.RepoOverride{repo: {Mode: mode}}
												}
												override := writeback.KindPolicyOverride{}
												if noRequire {
													override.RequireLabel = &f
												}
												if unlabeled {
													override.UnlabeledMinLevel = &t6
												}
												wcfg.Kinds[string(writeback.KindBotRebaseRequest)] = override
												if backfill {
													b := true
													wcfg.Unsolicited.Backfill = &b
												}
												levelFor := func(string) int { return level }
												listedFn := func(string) bool { return listed }
												pauseFn := func(string) (writeback.RepoPauseInfo, bool) { return writeback.RepoPauseInfo{}, paused }
												now := firstSeen
												if !seen {
													now = cardTestNow
												}
												g := writeback.New(wcfg, writeback.Deps{LevelFor: levelFor, RepoListed: listedFn, RepoPause: pauseFn, Now: func() time.Time { return now }}, "")
												if seen {
													g.ObserveRepo(repo)
												}
												pr := cardBotPR(5, "renovate[bot]", "renovate/foo")
												pr.AppAuthored = hive
												pr.CreatedAt = cardTestNow.Add(-time.Hour)
												if older {
													pr.CreatedAt = firstSeen.Add(-time.Hour)
												}
												if labelled {
													pr.Labels = []string{writeback.DefaultOptInLabel}
												}
												policy := cardGatePolicy{cfg: g.Config(), repos: g.Snapshot().Repos, now: now, levelFor: levelFor, listed: listedFn,
													paused: func(r string) bool { _, p := pauseFn(r); return p }}
												item := cardBotRebaseItem(pr)
												got := cardGateAllows(policy, repo, item)
												want := g.Decide(repo, item, writeback.KindBotRebaseRequest).Allowed
												cases++
												if got != want {
													t.Errorf("level %d labelled %v mode %q no-require %v unlabeled %v paused %v listed %v older %v backfill %v hive %v seen %v: copy %v, Decide %v",
														level, labelled, mode, noRequire, unlabeled, paused, listed, older, backfill, hive, seen, got, want)
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if cases == 0 {
		t.Fatal("no cases")
	}
}

// The publish-time copy writes nothing to the gate: no ledger file, no
// refusal record, whatever the PR. Serving reads only the stash.
func TestCardGatePolicyWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writeback.json")
	g := writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 6 }, Now: func() time.Time { return cardTestNow }}, path)
	level := 6
	cfg := &config.Config{ACMMLevel: &level}
	cfg.Project.Repos = []string{cardTestRepo}
	s := &Server{deps: &Dependencies{Config: cfg}}
	s.SetWritebackGate(g)
	old := cardBotPR(5, "renovate[bot]", "renovate/foo")
	old.CreatedAt = cardTestNow.Add(-30 * 24 * time.Hour)
	labelled := cardBotPR(6, "renovate[bot]", "renovate/bar")
	labelled.Labels = []string{writeback.DefaultOptInLabel}
	status := &StatusPayload{Timestamp: cardTestNow.Format(time.RFC3339), Repos: []FrontendRepo{{Full: cardTestRepo, OpenPrs: []any{FrontendPR{PullRequest: old}, FrontendPR{PullRequest: labelled}}}}}
	st := s.buildCardStash(status)
	if !st.botRebaseGated || st.botRebase[cardStashKey(cardTestRepo, 5)] || !st.botRebase[cardStashKey(cardTestRepo, 6)] {
		t.Fatalf("stash bot-rebase answers = %v (gated %v)", st.botRebase, st.botRebaseGated)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the gate ledger was written (%v)", err)
	}
	if n := len(g.Recent()); n != 0 {
		t.Fatalf("%d refusal(s) recorded", n)
	}
	if allowed, known := cardBotRebaseAllowed(cardTestInputs(nil), cardTestRepo, old); !allowed || !known {
		t.Error("no stash (no gate recorded) must allow, as a nil gate does in cmd/hive")
	}
	oldAllowed, oldKnown := cardBotRebaseAllowed(cardTestInputs(st), cardTestRepo, old)
	newAllowed, newKnown := cardBotRebaseAllowed(cardTestInputs(st), cardTestRepo, labelled)
	if oldAllowed || !oldKnown || !newAllowed || !newKnown {
		t.Error("serving must read the stash's answers")
	}
	if _, known := cardBotRebaseAllowed(cardTestInputs(st), cardTestRepo, cardBotPR(7, "renovate[bot]", "renovate/new")); known {
		t.Error("a PR the stash has no answer for must not be known")
	}
}

// The waiting-cause helpers only tell apart the waiting returns of the
// your-turn decision. Each case is a row the decision itself makes wait;
// the helper must name the return that did it.
func TestCardIssueWaitingCauseMatchesTheOwnershipDecision(t *testing.T) {
	env := ownershipEnv{now: cardTestNow, hiveLevel: config.MaxACMMLevel}
	decision := config.DefaultNeedsDecisionLabel
	linked := cardIssue(3)
	linked.LinkedPRs = []github.IssueLinkedPR{{Number: 30, State: "open"}}
	blocked := cardIssue(8, blockedWorkflowLabel)
	blocked.DependsOn = []github.IssueDependency{{Key: "octo/demo#80"}}
	maintainerReply := &github.IssuePersonComment{Login: "maintainer", AuthorAssociation: "MEMBER", CreatedAt: cardTestNow.Add(-time.Hour), Mentions: []string{"jeefy"}}
	replied := cardWork(9, repoWorkBucketReporterTriage, "Awaiting maintainer triage label")
	replied.Author, replied.LatestPersonComment = "reporter", maintainerReply
	cases := []struct {
		name string
		in   issueOwnershipInput
		row  cardIssueRow
		want string
	}{
		{"a person's hold",
			issueOwnershipInput{repo: cardTestRepo, number: 1, labels: []string{"hold"}, held: true, holdSources: []string{github.HoldSourcePerson}},
			cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1, "hold"), held: true, holdSources: []string{github.HoldSourcePerson}},
			cardWaitHold},
		{"claimed_by_pr",
			issueOwnershipInput{repo: cardTestRepo, number: 2, bucket: repoWorkBucketClaimedByPR},
			cardIssueRow{repoFull: cardTestRepo, work: cardWork(2, repoWorkBucketClaimedByPR, ""), nonActionable: true},
			cardWaitCoveredByPR},
		{"open linked PR",
			issueOwnershipInput{repo: cardTestRepo, number: 3, actionable: true, openLinkedPR: true},
			cardIssueRow{repoFull: cardTestRepo, issue: linked},
			cardWaitCoveredByPR},
		{"hive PR closes the decision park",
			issueOwnershipInput{repo: cardTestRepo, number: 4, labels: []string{decision}, bucket: repoWorkBucketFiltered, bucketReason: decision, hiveClosingPR: 40},
			cardIssueRow{repoFull: cardTestRepo, work: cardWork(4, repoWorkBucketFiltered, decision, decision), nonActionable: true, hiveClosingPR: 40},
			cardWaitHiveClosingPR},
		{"another decision blocks it",
			issueOwnershipInput{repo: cardTestRepo, number: 5, labels: []string{decision}, bucket: repoWorkBucketFiltered, bucketReason: decision, decisionBlocker: "#50"},
			cardIssueRow{repoFull: cardTestRepo, work: cardWork(5, repoWorkBucketFiltered, decision, decision), nonActionable: true},
			cardWaitDecisionBlocker},
		{"fresh reporter confirmation",
			issueOwnershipInput{repo: cardTestRepo, number: 6, labels: []string{ownershipLabelReporterConfirmation}, bucket: repoWorkBucketFiltered, bucketReason: ownershipLabelReporterConfirmation},
			cardIssueRow{repoFull: cardTestRepo, work: cardWork(6, repoWorkBucketFiltered, ownershipLabelReporterConfirmation, ownershipLabelReporterConfirmation), nonActionable: true},
			cardWaitReporter},
		{"issue-filter row a PR closes",
			issueOwnershipInput{repo: cardTestRepo, number: 7, bucket: repoWorkBucketFiltered, bucketReason: github.IssueFilterReason, closingPR: 70},
			cardIssueRow{repoFull: cardTestRepo, work: cardWork(7, repoWorkBucketFiltered, github.IssueFilterReason), nonActionable: true, closingPR: 70},
			cardWaitClosingPR},
		{"blocked with an open blocked-by link",
			issueOwnershipInput{repo: cardTestRepo, number: 8, labels: []string{blockedWorkflowLabel}, actionable: true, openBlockers: blocked.OpenBlockers()},
			cardIssueRow{repoFull: cardTestRepo, issue: blocked},
			cardWaitBlockedBy},
		{"untrusted reporter's issue a maintainer replied on last",
			issueOwnershipInput{repo: cardTestRepo, number: 9, bucket: repoWorkBucketReporterTriage, author: "reporter", latestPersonComment: maintainerReply},
			cardIssueRow{repoFull: cardTestRepo, work: replied, nonActionable: true},
			cardWaitMaintainerReply},
	}
	for _, tc := range cases {
		o := decideIssueOwnershipGated(tc.in, env)
		if !o.WaitingElsewhere {
			t.Fatalf("%s: the fixture does not wait in ownership (%+v)", tc.name, o)
		}
		if got := cardIssueWaitingCause(tc.row, cardIssueParking(tc.row, nil), cardTestInputs(nil)); got != tc.want {
			t.Errorf("%s: card cause %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCardPRWaitingCauseMatchesTheOwnershipDecision(t *testing.T) {
	env := ownershipEnv{now: cardTestNow, hiveLevel: 5, reviewerLaneAvailable: func(string) bool { return true }}
	held := cardPR(1, "alice", "hold")
	held.HoldSources = []string{github.HoldSourceDashboardPause}
	if o := decidePROwnershipGated(held, nil, true, config.DashboardIssueBandsConfig{}, env); !o.WaitingElsewhere {
		t.Fatalf("held fixture does not wait: %+v", o)
	}
	if got := cardPRWaitingCause(held, true); got != cardWaitHold {
		t.Errorf("held: %q, want %q", got, cardWaitHold)
	}
	adjudicated := cardPR(2, "hive-app[bot]", ownershipLabelNeedsHuman)
	adjudicated.AppAuthored = true
	adjudicated.CreatedAt, adjudicated.UpdatedAt = time.Time{}, time.Time{}
	if o := decidePROwnershipGated(adjudicated, nil, false, config.DashboardIssueBandsConfig{}, env); !o.WaitingElsewhere {
		t.Fatalf("adjudicator fixture does not wait: %+v", o)
	}
	if got := cardPRWaitingCause(adjudicated, false); got != cardWaitAdjudicator {
		t.Errorf("adjudicator: %q, want %q", got, cardWaitAdjudicator)
	}
	// The third waiting return (a person answered the review) is the only
	// one left once the hold and the adjudicator wait are ruled out:
	// reviewHoldOwnership returns early inside the adjudicator wait.
	if got := cardPRWaitingCause(cardPR(3, "alice"), false); got != cardWaitAnswered {
		t.Errorf("answered: %q, want %q", got, cardWaitAnswered)
	}
}

// The copied dependency-bot constants; cmd/hive's
// TestDependencyBotConstantsTheCardCopies pins the originals to the same
// literals.
func TestCardDependencyBotConstants(t *testing.T) {
	if cardBotConflictGrace != 24*time.Hour || cardBotRedCIGrace != 4*time.Hour || cardDependabotAuthor != "dependabot[bot]" {
		t.Fatalf("conflict grace %v, red-CI grace %v, Dependabot %q", cardBotConflictGrace, cardBotRedCIGrace, cardDependabotAuthor)
	}
}
