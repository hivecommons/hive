package dashboard

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

var cardTestNow = time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)

const cardTestRepo = "octo/demo"

func cardTestOwnership(code string) *Ownership {
	o := yourTurn(code, "reason "+code)
	return &o
}

func cardTestWaiting() *Ownership {
	o := waitingElsewhere()
	return &o
}

// cardTestStash is a stash with one withheld verdict for octo/demo#n.
func cardTestStash(n int, item AdmissionWithheldItem) *cardStash {
	item.Repo, item.Number, item.Key = cardTestRepo, n, cardTestRepo+"#"+strconv.Itoa(n)
	return &cardStash{at: cardTestNow, withheld: map[string]AdmissionWithheldItem{cardStashKey(cardTestRepo, n): item}}
}

func cardTestInputs(stash *cardStash) cardInputs {
	return cardInputs{now: cardTestNow, stash: stash, dispatchLoaded: true}
}

func cardIssue(n int, labels ...string) github.Issue {
	return github.Issue{Repo: cardTestRepo, Number: n, Labels: labels, CreatedAt: cardTestNow.Add(-48 * time.Hour), UpdatedAt: cardTestNow.Add(-time.Hour)}
}

func cardWork(n int, bucket, reason string, labels ...string) github.RepoWorkIssue {
	return github.RepoWorkIssue{Repo: cardTestRepo, Number: n, Bucket: bucket, Reason: reason, Labels: labels}
}

func TestDecideIssueCardPrecedence(t *testing.T) {
	linked := cardIssue(5)
	linked.LinkedPRs = []github.IssueLinkedPR{{Number: 50, State: "open"}}
	blockedBy := cardIssue(10, blockedWorkflowLabel)
	blockedBy.DependsOn = []github.IssueDependency{{Key: "octo/demo#9"}}
	claimed := cardIssue(30, "stage/020-claimed")
	assigned := cardIssue(31)
	assigned.Assignees = []string{"alice"}
	cases := []struct {
		name     string
		row      cardIssueRow
		in       cardInputs
		wantBand string
		wantCode string
	}{
		{"ownership close", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1), ownership: cardTestOwnership(OwnershipReasonVerifiedDone)}, cardTestInputs(nil), cardBandToClose, "ownership:verified_done"},
		{"ownership your turn", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(2), ownership: cardTestOwnership(OwnershipReasonParkedNeedsHuman)}, cardTestInputs(nil), cardBandReview, "ownership:parked_needs_human"},
		{"waiting: a person's hold", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(3, "hold"), held: true, holdSources: []string{github.HoldSourcePerson}, ownership: cardTestWaiting()}, cardTestInputs(nil), cardBandHumanHold, "wait:hold"},
		{"waiting: claimed_by_pr", cardIssueRow{repoFull: cardTestRepo, work: cardWork(4, repoWorkBucketClaimedByPR, ""), nonActionable: true, ownership: cardTestWaiting()}, cardTestInputs(nil), cardBandCoveredByPR, "wait:covered"},
		{"waiting: open linked PR", cardIssueRow{repoFull: cardTestRepo, issue: linked, ownership: cardTestWaiting()}, cardTestInputs(nil), cardBandCoveredByPR, "wait:covered"},
		{"waiting: hive PR closes the decision park", cardIssueRow{repoFull: cardTestRepo, work: cardWork(6, repoWorkBucketFiltered, config.DefaultNeedsDecisionLabel, config.DefaultNeedsDecisionLabel), nonActionable: true, hiveClosingPR: 60, ownership: cardTestWaiting()}, cardTestInputs(nil), cardBandCoveredByPR, "wait:hive_pr_closes"},
		{"waiting: another decision blocks it", cardIssueRow{repoFull: cardTestRepo, work: cardWork(7, repoWorkBucketFiltered, config.DefaultNeedsDecisionLabel, config.DefaultNeedsDecisionLabel), nonActionable: true, ownership: cardTestWaiting()}, cardTestInputs(nil), cardBandWaitingOnItem, "wait:decision_blocker"},
		{"waiting: reporter", cardIssueRow{repoFull: cardTestRepo, work: cardWork(8, repoWorkBucketFiltered, ownershipLabelReporterConfirmation, ownershipLabelReporterConfirmation), nonActionable: true, ownership: cardTestWaiting()}, cardTestInputs(nil), cardBandReporterConfirm, "wait:reporter"},
		{"waiting: a PR closes the filtered issue", cardIssueRow{repoFull: cardTestRepo, work: cardWork(9, repoWorkBucketFiltered, github.IssueFilterReason), nonActionable: true, closingPR: 90, ownership: cardTestWaiting()}, cardTestInputs(nil), cardBandCoveredByPR, "wait:closing_pr"},
		{"waiting: open blocked-by link", cardIssueRow{repoFull: cardTestRepo, issue: blockedBy, ownership: cardTestWaiting()}, cardTestInputs(nil), cardBandWaitingOnItem, "wait:blocked_by"},
		{"waiting: no cause matches (the row changed since the decision)", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(11), ownership: cardTestWaiting()}, cardTestInputs(nil), cardBandWaitingOnItem, "wait:unmatched"},
		{"held, no earlier match", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(12, "hold"), held: true, holdSources: []string{github.HoldSourceLevelGate}}, cardTestInputs(nil), cardBandHeld, "held:level_gate"},
		{"held with no recorded source", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(13, "hold"), held: true}, cardTestInputs(nil), cardBandHeld, "held:label"},
		{"claim label, no verdict", cardIssueRow{repoFull: cardTestRepo, issue: claimed}, cardTestInputs(nil), cardBandWorking, "flags:claimed"},
		{"no stash: assigned", cardIssueRow{repoFull: cardTestRepo, issue: assigned}, cardTestInputs(nil), cardBandWorking, "flags:no_verdict"},
		{"no stash: unclaimed", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(32)}, cardTestInputs(nil), cardBandQueued, "flags:no_verdict"},
		{"stash predates the issue", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(33)}, cardTestInputs(cardTestStash(99, AdmissionWithheldItem{Reason: withheldReasonTracker})), cardBandQueued, "flags:no_verdict"},
		{"capped stash", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(34)}, cardTestInputs(&cardStash{at: cardTestNow, capped: true}), cardBandQueued, "flags:no_verdict"},
		{"external key never matches", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(35)}, cardTestInputs(&cardStash{at: cardTestNow, withheld: map[string]AdmissionWithheldItem{"linear:abc-35": {Reason: withheldReasonTracker}}}), cardBandQueued, "flags:no_verdict"},
		{"admitted", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(36)}, cardTestInputs(&cardStash{at: cardTestNow, queued: map[string]bool{cardStashKey(cardTestRepo, 36): true}}), cardBandQueued, "verdict:admitted"},
		{"operator queue hold", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(37)}, cardTestInputs(&cardStash{at: cardTestNow, queueHeld: map[string]string{cardStashKey(cardTestRepo, 37): "waiting on design"}}), cardBandHumanHold, "verdict:queue_hold"},
		{"integration orchestration prefix", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(38)}, cardTestInputs(cardTestStash(38, AdmissionWithheldItem{Reason: "integration_orchestration: lane busy"})), cardBandWaitingOnItem, "verdict:integration_orchestration"},
		{"integration orchestration, no reason", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(39)}, cardTestInputs(cardTestStash(39, AdmissionWithheldItem{})), cardBandWaitingOnItem, "verdict:integration_orchestration"},
		{"unknown verdict", cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(40)}, cardTestInputs(cardTestStash(40, AdmissionWithheldItem{Reason: "something_new"})), cardBandUnexplained, "unexplained:verdict"},
	}
	for _, tc := range cases {
		got := decideIssueCard(tc.row, tc.in)
		if got.Key != tc.wantBand || got.ReasonCode != tc.wantCode {
			t.Errorf("%s: band %q code %q, want %q %q (reason %q)", tc.name, got.Key, got.ReasonCode, tc.wantBand, tc.wantCode, got.Reason)
		}
		if got.Group != cardBandDefs[got.Key].group || got.Group == "" {
			t.Errorf("%s: group %q for band %q", tc.name, got.Group, got.Key)
		}
		if got.Reason == "" {
			t.Errorf("%s: empty reason", tc.name)
		}
	}
}

// cardTestBucketTable is every bucket (and filtered reason) the hive
// records on a non-actionable issue row, with the band a row with no
// stored ownership decision gets. "" is not a card row: the row is in the
// Outside remainder. TestCardBucketTableCoversEveryRecordedBucket pins the
// list to the source.
var cardTestBucketTable = []struct {
	bucket, reason string
	wantBand       string
}{
	{repoWorkBucketHiveAdvisory, "Hive advisory report", cardBandNotWork},
	{repoWorkBucketDependencyDashboard, "Dependency dashboard control panel", cardBandNotWork},
	{repoWorkBucketFiltered, cardFilteredExempt, cardBandNotWork},
	{repoWorkBucketFiltered, cardFilteredTracking, cardBandNotWork},
	{repoWorkBucketFiltered, "blocked", cardBandWaitingOnItem},
	{repoWorkBucketFiltered, "blocked on octo/demo#12", cardBandWaitingOnItem},
	{repoWorkBucketFiltered, ownershipLabelReporterConfirmation, cardBandReporterConfirm},
	{repoWorkBucketFiltered, config.DefaultSHAHoldTrustedLabel, cardBandReporterConfirm},
	{repoWorkBucketFiltered, config.PossiblyExistsStageLabel, cardBandTriage},
	{repoWorkBucketFiltered, config.DefaultNeedsHumanLabel, cardBandReview},
	{repoWorkBucketFiltered, config.DefaultNeedsDirectionLabel, cardBandDecide},
	{repoWorkBucketFiltered, config.DefaultNeedsDecisionLabel, cardBandDecide},
	{repoWorkBucketFiltered, config.LegacyNeedsDecisionLabel, cardBandDecide},
	{repoWorkBucketFiltered, config.PRNeedsDecisionStageLabel, cardBandDecide},
	{repoWorkBucketFiltered, config.DefaultNeedsSpecLabel, cardBandDecide},
	{repoWorkBucketFiltered, github.IssueFilterReason, cardBandNotWork},
	{repoWorkBucketFiltered, cardAgentProposalReason, cardBandNotWork},
	{cardBucketInProgress, "In progress by agent", cardBandWorking},
	{cardBucketClaimed, "Live issue claim by someone", cardBandWorking},
	{repoWorkBucketClaimedByPR, "Open external PR #7 covers this issue", cardBandCoveredByPR},
	{repoWorkBucketReporterTriage, "Awaiting maintainer triage label", cardBandAdmit},
	{cardBucketPendingRelease, "Unanswered release owner step is parked for the owner", cardBandDecide},
	{cardBucketActionable, "Contributor claim is advisory", cardBandQueued},
	{cardBucketOther, forgeUnclassifiedReason, ""},
	{cardBucketOther, webhookUnclassifiedReason, ""},
}

func TestDecideIssueCardBuckets(t *testing.T) {
	for _, tc := range cardTestBucketTable {
		row := cardIssueRow{repoFull: cardTestRepo, work: cardWork(1, tc.bucket, tc.reason), nonActionable: true}
		got := decideIssueCard(row, cardTestInputs(nil))
		if got.Key != tc.wantBand {
			t.Errorf("%s / %q: band %q, want %q", tc.bucket, tc.reason, got.Key, tc.wantBand)
		}
		if got.Key != "" && !strings.HasPrefix(got.ReasonCode, "bucket:") && !strings.HasPrefix(got.ReasonCode, "flags:") {
			t.Errorf("%s / %q: code %q", tc.bucket, tc.reason, got.ReasonCode)
		}
	}
	// Only an unknown bucket or filtered reason is Unexplained.
	for _, tc := range []struct{ bucket, reason string }{{repoWorkBucketFiltered, "some new reason"}, {"some_new_bucket", ""}} {
		row := cardIssueRow{repoFull: cardTestRepo, work: cardWork(1, tc.bucket, tc.reason), nonActionable: true}
		if got := decideIssueCard(row, cardTestInputs(nil)); got.Key != cardBandUnexplained {
			t.Errorf("%s / %q: band %q, want Unexplained", tc.bucket, tc.reason, got.Key)
		}
	}
}

// The issue filter's rows with no stored decision are placed the way the
// your-turn decision would place them (rule 9b).
func TestDecideIssueCardIssueFilterRows(t *testing.T) {
	person := cardWork(1, repoWorkBucketFiltered, github.IssueFilterReason)
	person.AuthorIsHuman = true
	decided := cardWork(1, repoWorkBucketFiltered, github.IssueFilterReason, ownershipLabelApprovedDirection)
	agent := cardWork(1, repoWorkBucketFiltered, github.IssueFilterReason, "agent/scanner")
	bot := cardWork(1, repoWorkBucketFiltered, github.IssueFilterReason)
	for _, tc := range []struct {
		name string
		row  cardIssueRow
		want string
	}{
		{"a person's issue", cardIssueRow{repoFull: cardTestRepo, work: person, nonActionable: true}, cardBandAdmit},
		{"decided", cardIssueRow{repoFull: cardTestRepo, work: decided, nonActionable: true}, cardBandAdmit},
		{"an agent's finding", cardIssueRow{repoFull: cardTestRepo, work: agent, nonActionable: true}, cardBandAdmit},
		{"a bot's issue", cardIssueRow{repoFull: cardTestRepo, work: bot, nonActionable: true}, cardBandNotWork},
		{"a PR closes it", cardIssueRow{repoFull: cardTestRepo, work: person, nonActionable: true, closingPR: 9}, cardBandCoveredByPR},
		{"ownership left it alone", cardIssueRow{repoFull: cardTestRepo, work: person, nonActionable: true, ownership: &Ownership{}}, cardBandNotWork},
	} {
		if got := decideIssueCard(tc.row, cardTestInputs(nil)); got.Key != tc.want {
			t.Errorf("%s: band %q, want %q", tc.name, got.Key, tc.want)
		}
	}
}

// The SHA-hold label is this hive's configured one (Issues.EffectiveSHAHoldTrustedLabel).
func TestDecideIssueCardConfiguredSHAHoldLabel(t *testing.T) {
	label := "awaiting-sha"
	cfg := &config.Config{}
	cfg.Issues.SHAHoldTrustedLabel = &label
	in := cardTestInputs(nil)
	in.cfg = cfg
	row := cardIssueRow{repoFull: cardTestRepo, work: cardWork(1, repoWorkBucketFiltered, label, label), nonActionable: true}
	if got := decideIssueCard(row, in); got.Key != cardBandReporterConfirm {
		t.Errorf("configured SHA-hold label: band %q, want %q", got.Key, cardBandReporterConfirm)
	}
	if got := decideIssueCard(row, cardTestInputs(nil)); got.Key != cardBandUnexplained {
		t.Errorf("without the setting the label is unknown: band %q", got.Key)
	}
}

func TestCardBlockedReasonUsesTheRecordedFacts(t *testing.T) {
	base := cardWork(1, repoWorkBucketFiltered, "blocked")
	withBlockedOn := base
	withBlockedOn.BlockedOn = []string{"octo/demo#3", "issue octo/other#4"}
	named := base
	named.Reason = "blocked on octo/demo#7"
	counted := base
	counted.OpenBlockedBy = 2
	recheck := base
	recheck.RecheckAfter = time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		w    github.RepoWorkIssue
		want string
	}{
		{"blocked on", withBlockedOn, "Blocked on octo/demo#3, issue octo/other#4; check they are still open and moving."},
		{"named reason", named, "blocked on octo/demo#7"},
		{"open blocked-by count", counted, "GitHub lists 2 open blockers."},
		{"recheck after", recheck, "The hive rechecks it after 2026-12-01 09:00Z."},
		{"nothing recorded", base, "Labelled blocked; nothing records what it waits on. Check the blocker is still real."},
	} {
		if got := cardBlockedReason(tc.w); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every reason the contributor queue's ladder can give lands in a known
// band, never Unexplained.
func TestDecideIssueCardEveryWithheldReason(t *testing.T) {
	want := map[string]string{
		contributorAdmissionReasonOpenPRClaim:       cardBandCoveredByPR,
		contributorAdmissionReasonOpenPRLinked:      cardBandCoveredByPR,
		contributorAdmissionReasonIssueClaim:        cardBandWorking,
		contributorAdmissionReasonMergedClaimStale:  cardBandToClose,
		contributorAdmissionReasonIssueChurn:        cardBandToClose,
		contributorAdmissionReasonWorkflowBlocked:   cardBandWaitingOnItem,
		contributorAdmissionReasonLabelSkipped:      cardBandNotWork,
		contributorAdmissionReasonDependencyBlocked: cardBandWaitingOnItem,
		contributorAdmissionReasonDependencyUnknown: cardBandWaitingOnItem,
		contributorAdmissionReasonOwnerGrace:        cardBandQueued,
		contributorAdmissionReasonCoveringPRStalled: cardBandCoveredByPR,
		withheldReasonDisabledRepo:                  cardBandNotWork,
		withheldReasonRepoUnmintable:                cardBandQueued,
		withheldReasonTracker:                       cardBandNotWork,
		withheldReasonCooldown:                      cardBandQueued,
		withheldReasonNoWorkNeeded:                  cardBandToClose,
		completionVerdictNeedsDecision:              cardBandDecide,
		withheldReasonFailureCooldown:               cardBandQueued,
		withheldReasonInFlight:                      cardBandWorking,
		withheldReasonContributorFilter:             cardBandNotWork,
		withheldReasonAssignedToOther:               cardBandWorking,
	}
	for reason := range withheldReasonLabels {
		band, ok := want[reason]
		if !ok {
			t.Errorf("withheld reason %q has no expected card band in this test: add it", reason)
			continue
		}
		row := cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1)}
		got := decideIssueCard(row, cardTestInputs(cardTestStash(1, AdmissionWithheldItem{Reason: reason, SkippedLabel: "epic", ChurnMerged: 3})))
		if got.Key != band || got.ReasonCode != "verdict:"+reason {
			t.Errorf("verdict %q: band %q code %q, want %q", reason, got.Key, got.ReasonCode, band)
		}
	}
	churn := decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1)}, cardTestInputs(cardTestStash(1, AdmissionWithheldItem{Reason: contributorAdmissionReasonIssueChurn, ChurnMerged: 3})))
	if !strings.Contains(churn.Reason, "3 PRs merged") {
		t.Errorf("churn reason = %q, want the merged count", churn.Reason)
	}
	stalled := decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1)}, cardTestInputs(cardTestStash(1, AdmissionWithheldItem{Reason: contributorAdmissionReasonCoveringPRStalled, ClaimURL: "https://github.com/octo/demo/pull/77"})))
	if !strings.Contains(stalled.Reason, "PR #77 stalled") {
		t.Errorf("stalled reason = %q, want it to name PR #77", stalled.Reason)
	}
}

func TestDecideIssueCardSkippedLabels(t *testing.T) {
	for _, tc := range []struct {
		label, want string
	}{
		{"needs-triage", cardBandTriage},
		{ownershipLabelAlreadyDone, cardBandToClose},
		{config.DefaultNeedsDecisionLabel, cardBandDecide},
		{config.LegacyNeedsDecisionLabel, cardBandDecide},
		{"epic", cardBandNotWork},
		{"tracking", cardBandNotWork},
		{"discussion", cardBandNotWork},
		{"question", cardBandNotWork},
		{"anything-else", cardBandNotWork},
	} {
		row := cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1)}
		got := decideIssueCard(row, cardTestInputs(cardTestStash(1, AdmissionWithheldItem{Reason: contributorAdmissionReasonLabelSkipped, SkippedLabel: tc.label})))
		if got.Key != tc.want {
			t.Errorf("label_skipped %q: band %q, want %q", tc.label, got.Key, tc.want)
		}
	}
}

func TestDecideIssueCardDisabledRepoFollowsTheRoutedLane(t *testing.T) {
	for _, canOpen := range []bool{true, false} {
		in := cardTestInputs(cardTestStash(1, AdmissionWithheldItem{Reason: withheldReasonDisabledRepo}))
		in.laneCanOpenPR = func(string, string) bool { return canOpen }
		issue := cardIssue(1)
		issue.Lane = "scanner"
		got := decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: issue}, in)
		want := cardBandNotWork
		if canOpen {
			want = cardBandQueued
		}
		if got.Key != want {
			t.Errorf("lane can open PRs = %v: band %q, want %q", canOpen, got.Key, want)
		}
	}
}

func cardPR(n int, author string, labels ...string) github.PullRequest {
	return github.PullRequest{Repo: cardTestRepo, Number: n, Author: author, Labels: labels, CreatedAt: cardTestNow.Add(-72 * time.Hour), UpdatedAt: cardTestNow.Add(-2 * time.Hour)}
}

func cardBotPR(n int, author, headRef string) github.PullRequest {
	pr := cardPR(n, author)
	pr.HeadRef = headRef
	pr.CIStatus = "failure"
	pr.FailingChecks = []string{"ci/test"}
	return pr
}

func cardStageStash(n int, facts cardStageFacts) *cardStash {
	return &cardStash{stages: map[string]cardStageFacts{cardStashKey(cardTestRepo, n): facts}}
}

func TestDecidePRCardPrecedence(t *testing.T) {
	hive := cardPR(1, "hive-app[bot]")
	hive.AppAuthored = true
	adjudicated := hive
	adjudicated.Labels = []string{ownershipLabelNeedsHuman}
	clanker := cardPR(1, "relay")
	clanker.HiveAttributed = true
	eligible := cardPR(1, "alice")
	draft := cardPR(1, "alice")
	draft.Draft = true
	cases := []struct {
		name     string
		row      cardPRRow
		wantBand string
		wantCode string
	}{
		{"ownership close", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice"), ownership: cardTestOwnership(OwnershipReasonSupersededRecommendedClose)}, cardBandToClose, "ownership:superseded_recommended_close"},
		{"ownership your turn", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice"), ownership: cardTestOwnership(OwnershipReasonPersonPRNeedsHuman)}, cardBandReview, "ownership:person_pr_needs_human"},
		{"draft listed", cardPRRow{repoFull: cardTestRepo, pr: draft}, cardBandDraft, "flags:draft"},
		{"draft sidelined", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice"), sidelinedDraft: true}, cardBandDraft, "flags:draft"},
		{"waiting: a person's hold", cardPRRow{repoFull: cardTestRepo, pr: func() github.PullRequest {
			p := cardPR(1, "alice", "hold")
			p.HoldSources = []string{github.HoldSourcePerson}
			return p
		}(), held: true, ownership: cardTestWaiting()}, cardBandHumanHold, "wait:hold"},
		{"waiting: adjudicator", cardPRRow{repoFull: cardTestRepo, pr: adjudicated, ownership: cardTestWaiting()}, cardBandWorking, "wait:adjudicator"},
		{"waiting: answered, hive", cardPRRow{repoFull: cardTestRepo, pr: hive, ownership: cardTestWaiting()}, cardBandWorking, "wait:answered:hive"},
		{"waiting: answered, person", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice"), ownership: cardTestWaiting()}, cardBandAuthorUpdate, "wait:answered:person"},
		{"waiting: answered, clanker", cardPRRow{repoFull: cardTestRepo, pr: clanker, ownership: cardTestWaiting()}, cardBandAuthorUpdate, "wait:answered:clanker"},
		{"waiting: answered, bot", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "renovate[bot]"), ownership: cardTestWaiting()}, cardBandTurnOther, "wait:answered:bot"},
		{"held list", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice"), held: true}, cardBandHeld, "held:label"},
		{"hold label", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "hold")}, cardBandHeld, "held:label"},
		{"eligible verdict", cardPRRow{repoFull: cardTestRepo, pr: eligible, verdict: &github.MergeVerdict{State: github.MergeVerdictEligible}}, cardBandWillMerge, "verdict:eligible"},
		{"080", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/080-awaiting-merge-admission")}, cardBandWillMerge, "stage:080:merge"},
		{"095", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/095-pre-merge-soak")}, cardBandWillMerge, "stage:095:merge"},
		{"100", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/100-merge-queue")}, cardBandWillMerge, "stage:100:merge"},
		{"050", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/050-review-pending")}, cardBandWorking, "stage:050:review"},
		{"074", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/074-adopted-by-hive")}, cardBandWorking, "stage:074:adopted"},
		{"070 person", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/070-fixing")}, cardBandUnexplained, "unexplained:stage_070_author"},
		{"071 person", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/071-needs-author")}, cardBandAuthorUpdate, "stage:071:author"},
		{"072 clanker", cardPRRow{repoFull: cardTestRepo, pr: func() github.PullRequest {
			p := clanker
			p.Labels = []string{"stage/072-returned-to-contributor"}
			return p
		}()}, cardBandAuthorUpdate, "stage:072:author"},
		{"085 not your turn", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/085-needs-decision")}, cardBandUnexplained, "unexplained:stage_085_not_your_turn"},
		{"090 not your turn", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/090-ready-for-human-merge")}, cardBandUnexplained, "unexplained:stage_090_not_your_turn"},
		{"unknown stage", cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/040-covered-by-pr")}, cardBandUnexplained, "unexplained:stage_unknown"},
	}
	for _, tc := range cases {
		got := decidePRCard(tc.row, cardTestInputs(nil))
		if got.Key != tc.wantBand || got.ReasonCode != tc.wantCode {
			t.Errorf("%s: band %q code %q, want %q %q (reason %q)", tc.name, got.Key, got.ReasonCode, tc.wantBand, tc.wantCode, got.Reason)
		}
		if got.Group == "" || got.Reason == "" {
			t.Errorf("%s: group %q reason %q", tc.name, got.Group, got.Reason)
		}
	}
}

func TestDecidePRCardStageRecordFacts(t *testing.T) {
	baseRed := cardStageStash(1, cardStageFacts{Stage: stagelabels.ReviewPending, Reason: "base-branch-red:main", EnteredAt: cardTestNow.Add(-time.Hour)})
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/050-review-pending")}, cardTestInputs(baseRed)); got.Key != cardBandTurnOther || got.ReasonCode != "stage:050:base_red" {
		t.Errorf("050 base red: %+v", got)
	}
	shared := cardStageStash(1, cardStageFacts{Stage: stagelabels.Reviewed, Reason: "shared-ci-incident:runner"})
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/060-reviewed")}, cardTestInputs(shared)); got.Key != cardBandTurnOther {
		t.Errorf("060 shared CI incident: %+v", got)
	}
	// No stage label: the stashed record supplies the stage (server#594).
	fromRecord := cardStageStash(1, cardStageFacts{Stage: stagelabels.NeedsAuthor, EnteredAt: cardTestNow.Add(-time.Hour)})
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice")}, cardTestInputs(fromRecord)); got.Key != cardBandAuthorUpdate {
		t.Errorf("071 from the record: %+v", got)
	}
	// A record that moved after the rows were built never produces
	// Unexplained; the guard applies only to a stage taken from the record.
	newer := cardStageStash(1, cardStageFacts{Stage: stagelabels.NeedsDecision, EnteredAt: cardTestNow.Add(time.Minute)})
	newer.builtAt = cardTestNow.Add(-time.Minute)
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice")}, cardTestInputs(newer)); got.Key == cardBandUnexplained {
		t.Errorf("record newer than the build gave Unexplained: %+v", got)
	}
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/085-needs-decision")}, cardTestInputs(newer)); got.Key != cardBandUnexplained {
		t.Errorf("an 085 label the rows carry is not hidden by a newer record: %+v", got)
	}
	older := cardStageStash(1, cardStageFacts{Stage: stagelabels.NeedsDecision, EnteredAt: cardTestNow.Add(-time.Hour)})
	older.builtAt = cardTestNow.Add(-time.Minute)
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice")}, cardTestInputs(older)); got.Key != cardBandUnexplained {
		t.Errorf("085 record older than the build and not your turn: %+v, want Unexplained", got)
	}
	stalled := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice", "stage/071-needs-author", stagelabels.StalledLabel)}, cardTestInputs(nil))
	if stalled.Key != cardBandAuthorUpdate || !strings.Contains(stalled.Reason, "Stalled") {
		t.Errorf("stalled 071: %+v", stalled)
	}
}

func TestDecidePRCardPersonTakeover(t *testing.T) {
	level := 6
	cfg := &config.Config{ACMMLevel: &level}
	cfg.Governor.Labels.Stage.Adoption = config.StageAdoptionConfig{PersonAfter: "24h", PersonHumanRepos: []string{cardTestRepo}}
	noticed := cardStageStash(1, cardStageFacts{Stage: stagelabels.NeedsAuthor, PersonNoticeAt: cardTestNow.Add(-time.Hour)})
	pr := cardPR(1, "alice", "stage/071-needs-author", "author/human")
	in := cardTestInputs(noticed)
	in.cfg = cfg
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: pr}, in); got.Key != cardBandWorking || got.ReasonCode != "stage:071:takeover" {
		t.Errorf("071 with the takeover notice: %+v", got)
	}
	in.stash = cardStageStash(1, cardStageFacts{Stage: stagelabels.NeedsAuthor})
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: pr}, in); got.Key != cardBandAuthorUpdate {
		t.Errorf("071 without the notice: %+v", got)
	}
}

func TestDecidePRCardHiveFixing(t *testing.T) {
	pr := cardPR(3, "hive-app[bot]", "stage/070-fixing")
	pr.AppAuthored = true
	exhausted := review.DispatchState{FixAttempts: []review.FixAttempt{{Repo: cardTestRepo, Number: 3, Attempts: 99}}}
	row := cardPRRow{repoFull: cardTestRepo, pr: pr}

	in := cardTestInputs(nil)
	in.dispatch = exhausted
	if got := decidePRCard(row, in); got.Key != cardBandTurnOther || got.ReasonCode != "stage:070:fixer_excluded" || !strings.HasPrefix(got.Reason, "Nobody is fixing it: ") {
		t.Errorf("070 excluded, nobody on it: %+v", got)
	}
	in.workers = map[string]stageTrackerWorker{cardStashKey(cardTestRepo, 3): {Agent: "fixer"}}
	if got := decidePRCard(row, in); got.Key != cardBandWorking {
		t.Errorf("070 excluded, a worker on it: %+v", got)
	}
	in = cardTestInputs(nil)
	if got := decidePRCard(row, in); got.Key != cardBandWorking || got.ReasonCode != "stage:070:fixing" {
		t.Errorf("070 not excluded: %+v", got)
	}
	in.dispatchLoaded = false
	in.dispatch = exhausted
	if got := decidePRCard(row, in); got.Key != cardBandWorking || got.ReasonCode != "stage:070:dispatch_unread" || !strings.Contains(got.Reason, "fix dispatch state not read yet") {
		t.Errorf("070 before the dispatch state is read: %+v", got)
	}
}

func TestDecidePRCardDependencyBots(t *testing.T) {
	// gate: 1 allows, -1 refuses, 0 no gate installed, 2 a gate but no
	// answer for this PR (added since the publish). The answer is the one
	// the stash recorded at publish (cardGateAllows).
	const allowed, refuse, noGate, unchecked = 1, -1, 0, 2
	entered := func(ago time.Duration) *cardStash {
		return cardStageStash(5, cardStageFacts{Stage: stagelabels.Fixing, EnteredAt: cardTestNow.Add(-ago)})
	}
	withGate := func(st *cardStash, gate int) *cardStash {
		switch gate {
		case allowed, refuse:
			st.botRebaseGated = true
			st.botRebase = map[string]bool{cardStashKey(cardTestRepo, 5): gate == allowed}
		case unchecked:
			st.botRebaseGated = true
			st.botRebase = map[string]bool{}
		}
		return st
	}
	unrecorded := cardStageStash(99, cardStageFacts{})
	noTime := cardBotPR(5, "renovate[bot]", "renovate/foo")
	noTime.Labels = []string{"stage/070-fixing"}
	noTime.UpdatedAt = time.Time{}

	renovate := cardBotPR(5, "renovate[bot]", "renovate/foo")
	renovate.Labels = []string{"stage/070-fixing", writeback.DefaultOptInLabel}
	optionalOnly := renovate
	optionalOnly.Protection = &github.ProtectionFacts{RequiredChecksKnown: true, RequiredChecksAllReported: true}
	changesOnly := cardPR(5, "renovate[bot]", "stage/070-fixing")
	changesOnly.HeadRef = "renovate/foo"
	dependabot := cardBotPR(5, "dependabot[bot]", "dependabot/npm/foo")
	dependabot.Labels = []string{"stage/070-fixing"}
	actions := cardBotPR(5, "github-actions[bot]", "update/foo")
	actions.Labels = []string{"stage/071-needs-author"}
	conflicting := cardPR(5, "renovate[bot]", "stage/070-fixing", writeback.DefaultOptInLabel)
	conflicting.HeadRef = "renovate/foo"
	conflicting.MergeableState = "dirty"

	cases := []struct {
		name     string
		pr       github.PullRequest
		stash    *cardStash
		gate     int
		wantBand string
		wantCode string
	}{
		{"gate allows, inside the bound", renovate, entered(time.Hour), allowed, cardBandBotRebase, "stage:070:bot_rebase"},
		{"gate allows, past the red-CI bound", renovate, entered(5 * time.Hour), allowed, cardBandTurnOther, "stage:070:bot_overdue"},
		{"conflict uses the 24h bound", conflicting, entered(5 * time.Hour), allowed, cardBandBotRebase, "stage:070:bot_rebase"},
		{"conflict past the 24h bound", conflicting, entered(25 * time.Hour), allowed, cardBandTurnOther, "stage:070:bot_overdue"},
		{"gate refuses", renovate, entered(time.Hour), refuse, cardBandTurnOther, "stage:070:bot_no_rebase"},
		{"nil gate allows", renovate, entered(time.Hour), noGate, cardBandBotRebase, "stage:070:bot_rebase"},
		{"no entry time: the bound runs from the last update", renovate, unrecorded, noGate, cardBandBotRebase, "stage:070:bot_rebase"},
		{"no entry time and no update time", noTime, unrecorded, noGate, cardBandTurnOther, "stage:070:bot_unknown"},
		{"added since the publish: not yet checked", renovate, entered(time.Hour), unchecked, cardBandTurnOther, "stage:070:bot_gate_unknown"},
		{"Dependabot is never asked", dependabot, entered(time.Hour), allowed, cardBandTurnOther, "stage:070:bot_no_rebase"},
		{"github-actions is not a dependency bot", actions, entered(time.Hour), allowed, cardBandTurnOther, "stage:071:bot_no_rebase"},
		{"optional check only", optionalOnly, entered(time.Hour), allowed, cardBandTurnOther, "stage:070:bot_no_rebase"},
		{"changes requested only", changesOnly, entered(time.Hour), allowed, cardBandTurnOther, "stage:070:bot_no_rebase"},
	}
	for _, tc := range cases {
		in := cardTestInputs(withGate(tc.stash, tc.gate))
		got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: tc.pr}, in)
		if got.Key != tc.wantBand || got.ReasonCode != tc.wantCode {
			t.Errorf("%s: band %q code %q, want %q %q (reason %q)", tc.name, got.Key, got.ReasonCode, tc.wantBand, tc.wantCode, got.Reason)
		}
		if strings.Contains(got.Reason, "asked") {
			t.Errorf("%s: the reason must never say the bot was asked: %q", tc.name, got.Reason)
		}
	}
}

// No stage at all (engine off, shadow mode, deriver abstained, a new PR)
// is never Unexplained.
func TestDecidePRCardNoStageIsNeverUnexplained(t *testing.T) {
	newPR := cardPR(1, "alice")
	newPR.CreatedAt = cardTestNow.Add(-time.Minute)
	blockedPerson := cardPR(1, "alice")
	blockedPerson.Mergeable = github.MergeableNo
	blockedHive := blockedPerson
	blockedHive.AppAuthored = true
	sharedCI := blockedPerson
	sharedCI.SharedCIIncident = 2
	blockedBot := cardBotPR(1, "renovate[bot]", "renovate/x")
	blockedBot.Mergeable = github.MergeableNo
	queued := cardPR(1, "alice", github.AutoMergeQueuedLabel)
	cases := []struct {
		name     string
		pr       github.PullRequest
		wantBand string
	}{
		{"old, nothing blocks", cardPR(1, "alice"), cardBandWorking},
		{"new", newPR, cardBandWorking},
		{"blocked person", blockedPerson, cardBandAuthorUpdate},
		{"blocked hive", blockedHive, cardBandWorking},
		{"shared CI incident", sharedCI, cardBandTurnOther},
		{"blocked bot", blockedBot, cardBandBotRebase},
		{"queued for auto-merge", queued, cardBandWillMerge},
	}
	for _, tc := range cases {
		in := cardTestInputs(nil)
		got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: tc.pr}, in)
		if got.Key != tc.wantBand || !strings.HasPrefix(got.ReasonCode, "flags:no_stage") {
			t.Errorf("%s: band %q code %q, want %q", tc.name, got.Key, got.ReasonCode, tc.wantBand)
		}
	}
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: newPR}, cardTestInputs(nil)); got.ReasonCode != "flags:no_stage:new" {
		t.Errorf("new PR code %q", got.ReasonCode)
	}
}

func TestCardText(t *testing.T) {
	for in, want := range map[string]string{
		"Flagged by the sentinel \u2014 inspect it": "Flagged by the sentinel; inspect it",
		"a \u2013 b":                      "a; b",
		"2026\u20132027":                  "2026,2027",
		"x\u2014y":                        "x,y",
		"a \u2012 b \u2015 c":             "a; b; c",
		"asked @alice and @bob-2 to look": "asked alice and bob-2 to look",
		"mail me at a@example.com":        "mail me at a@example.com",
		"plain":                           "plain",
		"Hive will not merge this PR: ... \u2014 a person merges it": "Hive will not merge this PR: ...; a person merges it",
	} {
		if got := cardText(in); got != want {
			t.Errorf("cardText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCardSpecsAreCompleteAndDashFree(t *testing.T) {
	inOrder := func(order []string) map[string]bool {
		m := map[string]bool{}
		for _, k := range order {
			m[k] = true
		}
		return m
	}
	all := inOrder(cardBandOrder)
	if len(all) != len(cardBandDefs) {
		t.Fatalf("cardBandOrder has %d keys, cardBandDefs %d", len(all), len(cardBandDefs))
	}
	for key, def := range cardBandDefs {
		if !all[key] {
			t.Errorf("band %q is not in cardBandOrder", key)
		}
		if _, ok := cardGroupDefs[def.group]; !ok {
			t.Errorf("band %q has unknown group %q", key, def.group)
		}
	}
	for _, order := range [][]string{cardIssueBandOrder, cardPRBandOrder} {
		for _, key := range order {
			if _, ok := cardBandDefs[key]; !ok {
				t.Errorf("donut key %q has no spec", key)
			}
		}
	}
	if len(cardGroupOrder) != 6 || len(cardGroupDefs) != 6 {
		t.Fatalf("want six groups, got %d/%d", len(cardGroupOrder), len(cardGroupDefs))
	}
	card := buildOverviewCard(newCardTally(), FrontendOverviewTotals{}, nil)
	assertCardTextClean(t, "empty card", card)
}

// assertCardTextClean fails on an em dash, an en dash or an at sign in any
// label or rule of the card.
func assertCardTextClean(t *testing.T, what string, card *OverviewCard) {
	t.Helper()
	check := func(where, s string) {
		if strings.ContainsAny(s, "\u2014\u2013@") {
			t.Errorf("%s: %s has a dash or a mention: %q", what, where, s)
		}
	}
	for _, specs := range [][]OverviewCardBandSpec{card.Issues, card.PRs} {
		for _, b := range specs {
			check("band "+b.Key, b.Label+" "+b.Rule)
		}
	}
	for _, g := range card.Groups {
		check("group "+g.Key, g.Label+" "+g.Rule)
	}
	for _, tile := range card.Tiles {
		check("tile "+tile.Key, tile.Label+" "+tile.Rule)
		for _, r := range tile.Breakdown {
			check("tile row "+r.Key, r.Label+" "+r.Rule)
		}
	}
}

// Every bucket the hive records on a non-actionable issue row is in
// cardTestBucketTable, so no routine row can land in Unexplained. The scan
// reads the literal buckets the enumeration, its passes, the webhook
// classifier and the forge-cache seed write.
func TestCardBucketTableCoversEveryRecordedBucket(t *testing.T) {
	known := map[string]bool{"draft": true} // the sidelined-PR bucket
	reasons := map[string]bool{}
	for _, tc := range cardTestBucketTable {
		known[tc.bucket] = true
		if tc.bucket == repoWorkBucketFiltered {
			reasons[tc.reason] = true
		}
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`appendRepoWorkIssueDetail\(result, issue, "([^"]+)"`),
		regexp.MustCompile(`workIssue\(repo, issue, labels, "([^"]+)"`),
		regexp.MustCompile(`sidelined\("([^"]+)"`),
		regexp.MustCompile(`Bucket: +"([^"]+)"`),
	}
	filteredReason := regexp.MustCompile(`"filtered", "([^"]+)"`)
	files, err := filepath.Glob("../github/*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "open_items.go", "webhook_tracker_refresh.go")
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				found++
				if !known[m[1]] {
					t.Errorf("%s records bucket %q, which cardTestBucketTable (and cardIssueBucketBand) does not place", f, m[1])
				}
			}
		}
		for _, m := range filteredReason.FindAllStringSubmatch(string(data), -1) {
			if !reasons[m[1]] {
				t.Errorf("%s records filtered reason %q, which cardTestBucketTable does not place", f, m[1])
			}
		}
	}
	if found == 0 {
		t.Fatal("the scan found no bucket; the patterns no longer match the source")
	}
	// The buckets pkg/github names by constant.
	for _, b := range []string{cardBucketInProgress, cardBucketClaimed, repoWorkBucketClaimedByPR, repoWorkBucketReporterTriage} {
		if !known[b] {
			t.Errorf("bucket %q has no row in cardTestBucketTable", b)
		}
	}
}

// A hive that renames its hard-suppress labels still records the canonical
// bucket as the filtered reason (IssueFilterConfig.HardSuppressIssueBucket);
// a parked needs-human row with no decision yet is Review, as ownership
// would say.
func TestDecideIssueCardParkedWithRenamedLabels(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.IssueFilter.HardSuppressLabels.NeedsHuman = []string{"hive/needs-human"}
	in := cardTestInputs(nil)
	in.cfg = cfg
	row := cardIssueRow{repoFull: cardTestRepo, work: cardWork(1, repoWorkBucketFiltered, config.DefaultNeedsHumanLabel, "hive/needs-human"), nonActionable: true}
	if got := decideIssueCard(row, in); got.Key != cardBandReview || got.ReasonCode != "bucket:filtered:parked" {
		t.Errorf("renamed needs-human park: %+v", got)
	}
}

// The card tells the waiting returns of the your-turn decision apart
// (cardIssueWaitingCause); a row whose cause it cannot name is display lag
// (wait:unmatched), not Unexplained. A new waiting return in the decision
// must be added to the copy and to
// TestCardIssueWaitingCauseMatchesTheOwnershipDecision: this count fails
// first.
func TestCardIssueWaitingReturnsAreCounted(t *testing.T) {
	for _, tc := range []struct {
		file, fn string
		want     int
	}{
		{"ownership.go", "decideIssueOwnership", 6},
		{"ownership_gates.go", "blockedIssueOwnership", 1},
	} {
		data, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		start := strings.Index(src, "func "+tc.fn+"(")
		if start < 0 {
			t.Fatalf("%s not found in %s", tc.fn, tc.file)
		}
		end := strings.Index(src[start:], "\n}\n")
		body := src[start : start+end]
		if got := strings.Count(body, "waitingElsewhere(") + strings.Count(body, "waitingElsewhereFor("); got != tc.want {
			t.Errorf("%s has %d waiting returns, the card's copy knows %d: update cardIssueWaitingCause and its tests", tc.fn, got, tc.want)
		}
	}
}

// The buckets pkg/github writes through constants keep the values the card
// knows: a rename there would otherwise send rows to Unexplained.
func TestCardBucketConstantsMatchPkgGithub(t *testing.T) {
	known := map[string]bool{"draft": true}
	for _, tc := range cardTestBucketTable {
		known[tc.bucket] = true
	}
	re := regexp.MustCompile(`\b(inProgressWorkBucket|workBucket\w*)\s*=\s*"([^"]+)"`)
	files, err := filepath.Glob("../github/*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			found++
			if !known[m[2]] {
				t.Errorf("%s: %s = %q is a bucket the card does not place", f, m[1], m[2])
			}
		}
	}
	if found == 0 {
		t.Fatal("no bucket constant found; the pattern no longer matches pkg/github")
	}
	if string(stageengine.BucketReporterTriage) != repoWorkBucketReporterTriage {
		t.Errorf("stageengine.BucketReporterTriage = %q, the card knows %q", stageengine.BucketReporterTriage, repoWorkBucketReporterTriage)
	}
}
