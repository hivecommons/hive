package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

const reviewChangesHead = "c0ffee0123456789abcdef0123456789abcdef01"

func reviewChangesPR(number int, author string, labels ...string) github.PullRequest {
	return github.PullRequest{
		Repo: cardTestRepo, Number: number, Author: author, HeadSHA: reviewChangesHead, Labels: labels,
		CreatedAt: ownershipNow11565.Add(-96 * time.Hour), UpdatedAt: ownershipNow11565.Add(-time.Hour),
	}
}

// reviewChangesEnv is an ownership env whose review artifact holds a
// changes_requested verdict on the PR's current head, plus the given
// withheld fixes in the review dispatch state.
func reviewChangesEnv(pr github.PullRequest, withheld ...review.WithheldFix) ownershipEnv {
	env := ownershipEnv11565(5)
	env.reviewVerdicts = []review.Aggregate{{
		Repo: pr.Repo, Number: pr.Number, HeadSHA: reviewChangesHead, Verdict: review.VerdictChangesRequested,
		Perspectives: map[review.Perspective]review.Verdict{"correctness": review.VerdictChangesRequested},
		RecordedAt:   ownershipNow11565.Add(-2 * time.Hour),
	}}
	env.reviewSwarm.dispatch.Withheld = withheld
	env.mergeLaneTakesAuthor = func(string, string) bool { return false }
	return env
}

func otherBotWithheld(pr github.PullRequest, head string) review.WithheldFix {
	return review.WithheldFix{
		Repo: pr.Repo, Number: pr.Number, HeadSHA: head, Author: pr.Author,
		Setting: review.WithheldFixBotSetting, SettingEnabled: true,
		Excluded: review.FixDispatchExcludedOtherBotAuthor, Withheld: ownershipNow11565.Add(-2 * time.Hour),
	}
}

// In a repo whose auto-merge is off, a PR the hive's review returned for
// changes on its current head was marked a hand merge ("awaiting review
// approval; ... a person merges it"), so the inbox asked a person to merge
// what the review asked to change. The merge verdict now flags that head
// (ReviewChangesRequested) instead of marking it a hand merge.
func TestReviewChangesRequestedIsNotHandMerge(t *testing.T) {
	notApproved := &github.MergeVerdict{
		State:                  github.MergeVerdictOutstanding,
		Reason:                 "the hive's review did not approve this head (changes requested)",
		ReviewChangesRequested: true,
	}
	// What the classifier wrote before: the same PR, marked a hand merge.
	oldHandMerge := &github.MergeVerdict{
		State:     github.MergeVerdictOutstanding,
		Reason:    "awaiting review approval; " + stageTrackerAutoMergeOffGateText,
		HandMerge: true,
	}
	cases := []struct {
		name     string
		pr       github.PullRequest
		wantBand string
		wantCode string
	}{
		// A person's PR the review returned: it waits on its author.
		{"person PR, changes requested, 071", reviewChangesPR(21, "alice", "stage/071-needs-author", "role/untrusted", "author/human"), cardBandAuthorUpdate, "stage:071:author"},
		// A bot's PR with no withheld-fix record: the card follows the stage.
		{"bot PR without a withheld fix, 050", reviewChangesPR(22, "deps-bot[bot]", "stage/050-review-pending", "stage/stalled"), cardBandWorking, "stage:050:review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := reviewChangesEnv(tc.pr)
			cfg := config.DashboardIssueBandsConfig{}

			if got := decidePROwnershipGated(tc.pr, oldHandMerge, false, cfg, env); got.ReasonCode != OwnershipReasonHandMerge {
				t.Fatalf("old hand-merge verdict: ownership %+v, want %s (the test must tell the two verdicts apart)", got, OwnershipReasonHandMerge)
			}
			got := decidePROwnershipGated(tc.pr, notApproved, false, cfg, env)
			checkOwnership11565(t, got, notYours11565)

			band := decidePRCard(cardPRRow{repoFull: tc.pr.Repo, pr: tc.pr, verdict: notApproved, ownership: &got}, cardTestInputs(nil))
			if band.Key == cardBandHandMerge || band.Key == cardBandNotWork {
				t.Fatalf("card band = %q (%s), want neither merge-by-hand nor not-work", band.Key, band.ReasonCode)
			}
			if band.Key != tc.wantBand || band.ReasonCode != tc.wantCode {
				t.Errorf("card band %q code %q, want %q %q (reason %q)", band.Key, band.ReasonCode, tc.wantBand, tc.wantCode, band.Reason)
			}

			// The PR band reads the flagged verdict as in review, not as a
			// hand merge kept out of review.
			if _, _, inReview := prBandFlags(tc.pr, notApproved, labelSet(tc.pr.Labels), github.AutoMergeQueuedLabel, nil); !inReview {
				t.Errorf("prBandFlags inReview = false for the flagged verdict")
			}
		})
	}
}

// Another bot's PR whose current head the review returned for changes, and
// whose fix the dispatcher withheld because that bot owns the branch, is the
// repo maintainers' to close or wait out: your turn, on the card's Other row,
// never "Agents are working it". Every other shape keeps its decision.
func TestBotPRReviewChangesIsYourTurn(t *testing.T) {
	const olderHead = "0123456789abcdef0123456789abcdef01234567"
	bot := reviewChangesPR(10, "deps-bot[bot]", "stage/050-review-pending", "stage/stalled")
	adopted := reviewChangesPR(11, "deps-bot[bot]", "stage/074-adopted-by-hive")
	botNeedsAuthor := reviewChangesPR(14, "deps-bot[bot]", "stage/071-needs-author")
	botNeedsAuthor.CIStatus, botNeedsAuthor.FailingChecks = "failure", []string{"ci/test"}
	person := reviewChangesPR(12, "alice", "stage/071-needs-author", "author/human")
	hive := reviewChangesPR(13, "hive-app[bot]", "stage/050-review-pending")
	hive.HiveAgent = "scanner"
	notHive := func(pr github.PullRequest) review.WithheldFix {
		return review.WithheldFix{Repo: pr.Repo, Number: pr.Number, HeadSHA: reviewChangesHead, Author: pr.Author, Setting: review.WithheldFixSetting,
			Excluded: review.FixDispatchExcludedNotHiveAuthored, Withheld: ownershipNow11565.Add(-2 * time.Hour)}
	}
	legacy := otherBotWithheld(bot, reviewChangesHead)
	legacy.Excluded = ""
	legacyAdopted := otherBotWithheld(adopted, reviewChangesHead)
	legacyAdopted.Excluded = ""

	yours := want11565{OwnershipVerbReview, OwnershipReasonBotPRReviewChanges}
	cases := []struct {
		name     string
		pr       github.PullRequest
		withheld []review.WithheldFix
		edit     func(*ownershipEnv)
		want     want11565
		wantBand string
		wantCode string
	}{
		{"bot, other-bot fix withheld on the current head", bot, []review.WithheldFix{otherBotWithheld(bot, reviewChangesHead)}, nil,
			yours, cardBandTurnOther, "ownership:" + OwnershipReasonBotPRReviewChanges},
		{"bot, the same record written before the exclusion code", bot, []review.WithheldFix{legacy}, nil,
			yours, cardBandTurnOther, "ownership:" + OwnershipReasonBotPRReviewChanges},
		// At 070/071 the card's bot-rebase rule would otherwise speak for it.
		{"bot at 071, other-bot fix withheld on the current head", botNeedsAuthor, []review.WithheldFix{otherBotWithheld(botNeedsAuthor, reviewChangesHead)}, nil,
			yours, cardBandTurnOther, "ownership:" + OwnershipReasonBotPRReviewChanges},
		{"bot, other-bot fix withheld on an older head only", bot, []review.WithheldFix{otherBotWithheld(bot, olderHead)}, nil,
			notYours11565, cardBandWorking, "stage:050:review"},
		{"bot, no withheld fix (an operator's bot_authors lets the fixer push)", bot, nil, nil,
			notYours11565, cardBandWorking, "stage:050:review"},
		{"bot adopted in place, no withheld fix", adopted, nil, nil,
			notYours11565, cardBandWorking, "stage:074:adopted"},
		{"bot adopted, a pre-code record citing bot_authors", adopted, []review.WithheldFix{legacyAdopted}, nil,
			notYours11565, cardBandWorking, "stage:074:adopted"},
		// A coded record stays for its head; a later in-place adoption or a
		// pending fix means the hive's fixer does push after all.
		{"bot adopted in place after a coded record", adopted, []review.WithheldFix{otherBotWithheld(adopted, reviewChangesHead)}, nil,
			notYours11565, cardBandWorking, "stage:074:adopted"},
		{"bot, coded record and a pending fix for the head", bot, []review.WithheldFix{otherBotWithheld(bot, reviewChangesHead)}, func(env *ownershipEnv) {
			env.reviewSwarm.dispatch.Fixes = []review.PendingFix{{Repo: bot.Repo, Number: bot.Number, HeadSHA: reviewChangesHead, Agent: "fixer", Dispatched: ownershipNow11565.Add(-time.Hour)}}
		}, notYours11565, cardBandWorking, "stage:050:review"},
		// An answer is never carried out on another bot's branch and a
		// release does not apply to changes_requested: it stays the
		// maintainers' call.
		{"bot, a person answered the head after the verdict", bot, []review.WithheldFix{otherBotWithheld(bot, reviewChangesHead)}, func(env *ownershipEnv) {
			env.reviewSwarm.answer = func(string, int, string) (review.HumanChangeRequest, bool) {
				return review.HumanChangeRequest{HeadSHA: reviewChangesHead, Actor: "alice", RecordedAt: ownershipNow11565.Add(-time.Hour)}, true
			}
		}, yours, cardBandTurnOther, "ownership:" + OwnershipReasonBotPRReviewChanges},
		{"bot, a recorded hold release after the verdict", bot, []review.WithheldFix{otherBotWithheld(bot, reviewChangesHead)}, func(env *ownershipEnv) {
			env.reviewSwarm.releases = map[string]github.ReviewHoldRelease{
				"octo/demo#10": {Via: "review", State: "APPROVED", By: "alice", Person: true, At: ownershipNow11565.Add(-time.Hour), HoldHeadSHA: reviewChangesHead},
			}
		}, yours, cardBandTurnOther, "ownership:" + OwnershipReasonBotPRReviewChanges},
		{"person PR, fix withheld as not hive-authored", person, []review.WithheldFix{notHive(person)}, nil,
			notYours11565, cardBandAuthorUpdate, "stage:071:author"},
		{"hive PR, even with an other-bot record", hive, []review.WithheldFix{otherBotWithheld(hive, reviewChangesHead)}, nil,
			notYours11565, cardBandWorking, "stage:050:review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := reviewChangesEnv(tc.pr, tc.withheld...)
			if tc.edit != nil {
				tc.edit(&env)
			}
			got := decidePROwnershipGated(tc.pr, nil, false, config.DashboardIssueBandsConfig{}, env)
			checkOwnership11565(t, got, tc.want)
			if tc.want == yours {
				if got.Reason != botPRReviewChangesReason {
					t.Errorf("reason = %q, want %q", got.Reason, botPRReviewChangesReason)
				}
				if strings.ContainsAny(got.Reason, "\u2014\u2013@") {
					t.Errorf("reason has a dash or an at sign: %q", got.Reason)
				}
			}
			band := decidePRCard(cardPRRow{repoFull: tc.pr.Repo, pr: tc.pr, ownership: &got}, cardTestInputs(nil))
			if band.Key != tc.wantBand || band.ReasonCode != tc.wantCode {
				t.Errorf("card band %q code %q, want %q %q (reason %q)", band.Key, band.ReasonCode, tc.wantBand, tc.wantCode, band.Reason)
			}
			if tc.want == yours && band.Group != cardGroupYourTurn {
				t.Errorf("card group = %q, want %q", band.Group, cardGroupYourTurn)
			}
		})
	}

	// Precedence: a sentinel flag and a person's hold still win.
	flagged := reviewChangesPR(10, "deps-bot[bot]", "stage/050-review-pending", config.DefaultSentinelLabel)
	checkOwnership11565(t, decidePROwnershipGated(flagged, nil, false, config.DashboardIssueBandsConfig{}, reviewChangesEnv(flagged, otherBotWithheld(flagged, reviewChangesHead))),
		want11565{OwnershipVerbInspect, OwnershipReasonSentinelFlagged})
	held := reviewChangesPR(10, "deps-bot[bot]", "hold")
	held.HoldSources = []string{github.HoldSourcePerson}
	if got := decidePROwnershipGated(held, nil, true, config.DashboardIssueBandsConfig{}, reviewChangesEnv(held, otherBotWithheld(held, reviewChangesHead))); got.ReasonCode == OwnershipReasonBotPRReviewChanges {
		t.Fatalf("person-held bot PR = %+v, want its hold-source decision", got)
	}
}

// A PR with no stage whose head the review returned for changes waits on its
// author when a person or a contributor agent opened it: the card says Author
// to update the PR, and on a repo whose auto-merge is off the new-PR
// narration names the author as the next mover. A hive PR and a bot PR keep
// their earlier placement.
func TestNoStageReviewChangesWaitsOnAuthor(t *testing.T) {
	flagged := &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "CI failing: build", ReviewChangesRequested: true}
	person := reviewChangesPR(31, "alice")
	clanker := reviewChangesPR(32, "alice")
	clanker.HiveAttributed = true
	hive := reviewChangesPR(33, "hive-app[bot]")
	hive.HiveAgent = "scanner"
	bot := reviewChangesPR(34, "deps-bot[bot]")
	cases := []struct {
		name     string
		pr       github.PullRequest
		verdict  *github.MergeVerdict
		wantBand string
		wantCode string
	}{
		{"person", person, flagged, cardBandAuthorUpdate, "flags:no_stage:review_changes"},
		{"contributor agent", clanker, flagged, cardBandAuthorUpdate, "flags:no_stage:review_changes"},
		{"person, no flag", person, &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "awaiting review approval"}, cardBandWorking, "flags:no_stage:open"},
		{"hive", hive, flagged, cardBandWorking, "flags:no_stage:open"},
		{"bot", bot, flagged, cardBandWorking, "flags:no_stage:open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			band := decidePRCard(cardPRRow{repoFull: tc.pr.Repo, pr: tc.pr, verdict: tc.verdict}, cardTestInputs(nil))
			if band.Key != tc.wantBand || band.ReasonCode != tc.wantCode {
				t.Errorf("card band %q code %q, want %q %q (reason %q)", band.Key, band.ReasonCode, tc.wantBand, tc.wantCode, band.Reason)
			}
		})
	}

	fresh := func() *StageTrackerNew {
		return &StageTrackerNew{NextMover: StageTrackerMoverHive, NextMoverText: "the stage-label pass", Text: "New"}
	}
	got := stageTrackerAutoMergeOffNew(fresh(), flagged, authorKindHuman)
	if got.NextMover != StageTrackerMoverAuthor || !strings.Contains(got.Text, "asked for changes") || !strings.Contains(got.NextMoverText, "author") {
		t.Fatalf("person: mover %q text %q next %q, want the author as next mover", got.NextMover, got.Text, got.NextMoverText)
	}
	if strings.ContainsAny(got.Text+got.NextMoverText, "\u2014\u2013@") {
		t.Errorf("narration has a dash or an at sign: %q / %q", got.Text, got.NextMoverText)
	}
	it := StageTrackerItem{Author: "alice"}
	applyStageTrackerNew(&it, got)
	if it.Actor != "alice" {
		t.Errorf("actor = %q, want the author's login", it.Actor)
	}
	if got := stageTrackerAutoMergeOffNew(fresh(), flagged, authorKindClanker); got.NextMover != StageTrackerMoverAuthor {
		t.Errorf("contributor agent: mover %q, want author", got.NextMover)
	}
	for _, kind := range []string{authorKindHive, ""} {
		if got := stageTrackerAutoMergeOffNew(fresh(), flagged, kind); got.NextMover != StageTrackerMoverHive {
			t.Errorf("author kind %q: mover %q, want hive", kind, got.NextMover)
		}
	}
}
