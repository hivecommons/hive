package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// deciderVerdict is a requires_human verdict on head asking question,
// recorded age before ownershipNow11565.
func deciderVerdict(pr github.PullRequest, head, question string, age time.Duration) review.Aggregate {
	a := verdict12377(pr, head, review.VerdictRequiresHuman)
	a.DecisionRequested = true
	a.DecisionQuestion = question
	a.RecordedAt = ownershipNow11565.Add(-age)
	return a
}

const deciderPlainReason = "Hive's review needs a human decision"

// The hive's review names who should decide in its decision question
// ("@carol should decide whether ...").
// Within reviewDeciderAnswerWindow the row waits on that person; past it, it
// is the owner's turn again with the same reason code. Every other shape
// keeps today's decision. Display only: the holds are unchanged.
func TestReviewDeciderOwnership(t *testing.T) {
	pr := withHead12329(personPR11565(), "head-2") // author alice
	day := ownershipNow11565.Add(-2 * time.Hour).UTC().Format("Jan 2")
	lapsed := ownershipNow11565.Add(-4 * 24 * time.Hour)
	lapsedDay := lapsed.UTC().Format("Jan 2")

	for _, tc := range []struct {
		name     string
		question string
		age      time.Duration
		head     string // verdict head; "" = the PR's head
		dispatch review.DispatchState
		reqs     []review.HumanChangeRequest
		owners   []string
		self     []string
		reject   bool
		mutate   func(*review.Aggregate)
		// wantWait is the waiting reason; otherwise the your-turn code and
		// reason.
		wantWait   string
		wantCode   string
		wantReason string
	}{
		{name: "one other login within the window", question: "@carol should decide whether the release job may skip its smoke test", age: 2 * time.Hour,
			wantWait: "Waits on carol: Hive's review asked on " + day},
		{name: "one other login past the window", question: "@carol should decide whether the release job may skip its smoke test", age: 4 * 24 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: "Hive's review needs a human decision: carol has not answered since " + lapsedDay},
		{name: "exactly at the window is your turn", question: "@carol should decide", age: reviewDeciderAnswerWindow,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: "Hive's review needs a human decision: carol has not answered since " + ownershipNow11565.Add(-reviewDeciderAnswerWindow).UTC().Format("Jan 2")},
		{name: "spelling kept", question: "@Erin42 should decide whether this config change can land", age: 2 * time.Hour,
			wantWait: "Waits on Erin42: Hive's review asked on " + day},
		{name: "the author named, no maintainer wording", question: "@alice should decide whether the service may send usage pings by default", age: 2 * time.Hour,
			wantWait: "Waits on alice: Hive's review asked on " + day},
		{name: "the author named with maintainer wording", question: "@alice: a maintainer must confirm the new network permission still applies", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "the author named, A Maintainer in any case", question: "@alice A Maintainer must let the build check run", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "the author and one other login: the other decides", question: "@alice: @carol should decide", age: 2 * time.Hour,
			wantWait: "Waits on carol: Hive's review asked on " + day},
		{name: "nobody named", question: "Wait for the build check to run on this head", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "two other logins", question: "@carol or @Erin42 should decide", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "a team is not one person", question: "@example-org/maintainers should decide", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "bots and the hive's own login are dropped", question: "@renovate[bot] @copilot @hive-app[bot] @hive-app: @carol should decide", age: 2 * time.Hour,
			self: []string{"hive-app[bot]"}, wantWait: "Waits on carol: Hive's review asked on " + day},
		{name: "the dashboard owner named stays your turn", question: "@carol should decide", age: 2 * time.Hour, owners: []string{"carol"},
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "the verdict's own dispatcher hold is part of the question", question: "@carol should decide", age: 2 * time.Hour,
			dispatch: dispatchHold12377(pr, "head-2", "review verdict requires_human"), wantWait: "Waits on carol: Hive's review asked on " + day},
		{name: "a fix-cap dispatcher hold keeps today's decision", question: "@carol should decide", age: 2 * time.Hour,
			dispatch: dispatchHold12377(pr, "head-2", "review fix cap reached (6/6)"), wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "a reject keeps today's decision", question: "@carol should decide", age: 2 * time.Hour, reject: true,
			wantCode: OwnershipReasonReviewRejected, wantReason: "Hive's review rejected this pull request"},
		{name: "a question on an earlier head keeps today's decision", question: "@carol should decide", age: 2 * time.Hour, head: "head-1",
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "the policy's maintainer placeholder names nobody", question: "@maintainer should decide whether this sandbox widening may land", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "the maintainers placeholder next to the author names nobody", question: "@alice @maintainers: confirm the switch", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "a fix-cap verdict keeps its question but is the owner's", question: "@carol should decide", age: 2 * time.Hour,
			dispatch: dispatchHold12377(pr, "head-2", "fix cycle cap reached (3/3)"),
			mutate:   func(a *review.Aggregate) { a.Reasons = []string{"fix cycle cap reached (3/3)"} },
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "a fix-cap counter keeps today's decision", question: "@carol should decide", age: 2 * time.Hour,
			mutate:   func(a *review.Aggregate) { a.FixAttempts, a.MaxFixAttempts = 3, 3 },
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "a requires_human verdict that asked no decision keeps today's decision", question: "@carol should decide", age: 2 * time.Hour,
			mutate:   func(a *review.Aggregate) { a.DecisionRequested = false },
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "a login in a code span is not a mention", question: "`@carol` wrote the config; @Erin42 should decide", age: 2 * time.Hour,
			wantWait: "Waits on Erin42: Hive's review asked on " + day},
		{name: "a login in a fenced block is not a mention", question: "```\nowner: @carol\n```\n@Erin42 should decide", age: 2 * time.Hour,
			wantWait: "Waits on Erin42: Hive's review asked on " + day},
		{name: "app accounts without a bot suffix are dropped", question: "@github-actions @codex @coderabbitai @dependabot: @carol should decide", age: 2 * time.Hour,
			wantWait: "Waits on carol: Hive's review asked on " + day},
		{name: "the author named, maintainers should confirm", question: "@alice: maintainers should confirm the switch", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "the author named, the owner must approve", question: "@alice the Owner must approve this", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "the author named, an approver", question: "@alice: an approver has to sign off", age: 2 * time.Hour,
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
		{name: "the AI author is the hive's own login", question: "@hive-ai: @carol should decide", age: 2 * time.Hour,
			self: []string{"hive-ai"}, wantWait: "Waits on carol: Hive's review asked on " + day},
		{name: "the dashboard owner in another case stays your turn", question: "@Carol should decide", age: 2 * time.Hour, owners: []string{"carol"},
			wantCode: OwnershipReasonReviewRequiresHuman, wantReason: deciderPlainReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := tc.head
			if head == "" {
				head = pr.HeadSHA
			}
			v := deciderVerdict(pr, head, tc.question, tc.age)
			if tc.reject {
				v.Verdict, v.RequiresHuman = review.VerdictReject, false
			}
			if tc.mutate != nil {
				tc.mutate(&v)
			}
			env := answeredEnv(t, []review.Aggregate{v}, tc.dispatch, tc.reqs...)
			env.reviewSwarm.ownerLogins = tc.owners
			env.reviewSwarm.selfLogins = tc.self
			got := decidePROwnership(pr, false, env)
			if tc.wantWait != "" {
				if got.YourTurn || !got.WaitingElsewhere || got.ReasonCode != "" || got.Reason != tc.wantWait || !reviewDeciderWaiting(&got) {
					t.Fatalf("got %+v, want waiting %q", got, tc.wantWait)
				}
				if strings.Contains(got.Reason, "@") {
					t.Fatalf("reason %q mentions a login with @", got.Reason)
				}
				return
			}
			checkOwnership11565(t, got, want11565{OwnershipVerbReview, tc.wantCode})
			if got.Reason != tc.wantReason || got.WaitingElsewhere || reviewDeciderWaiting(&got) {
				t.Fatalf("got %+v, want reason %q", got, tc.wantReason)
			}
		})
	}
}

// A person's recorded answer on the head keeps today's answered path ahead
// of the named decider: the row waits on the hive's fix, not on the person
// the review asked.
func TestReviewDeciderAnsweredHeadKeepsAnsweredPath(t *testing.T) {
	pr := withHead12329(personPR11565(), "head-2")
	v := deciderVerdict(pr, "head-2", "@carol should decide whether operators may pick a different cache backend", 2*time.Hour)
	answer := decisionAnswer(pr, "head-2", review.HumanChangeRequestKindChanges, ownershipNow11565.Add(-time.Hour))
	got := decidePROwnership(pr, false, answeredEnv(t, []review.Aggregate{v}, review.DispatchState{}, answer))
	if got.YourTurn || !got.WaitingElsewhere || reviewDeciderWaiting(&got) || !strings.Contains(got.Reason, " answered Hive's review on this head") || !reviewAnsweredWaiting(&got) {
		t.Fatalf("got %+v, want the answered path", got)
	}
}

// The card agrees with the decision: a row waiting on the named decider is
// in Waiting on someone else (Waiting on another item) with the decision's
// reason as card_reason, and once the window has passed it is a Review row
// in Your turn with the decision's reason. The overview breakdown names the
// cause instead of counting it as the adjudicator wait.
func TestReviewDeciderCardBandAgrees(t *testing.T) {
	pr := withHead12329(personPR11565(), "head-2")
	for _, tc := range []struct {
		name      string
		age       time.Duration
		wantBand  string
		wantGroup string
		wantCode  string
	}{
		{name: "within the window", age: 2 * time.Hour, wantBand: cardBandWaitingOnItem, wantGroup: cardGroupWaiting, wantCode: "wait:review_decider"},
		{name: "past the window", age: 4 * 24 * time.Hour, wantBand: cardBandReview, wantGroup: cardGroupYourTurn, wantCode: "ownership:" + OwnershipReasonReviewRequiresHuman},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := deciderVerdict(pr, "head-2", "@carol should decide whether the release job may skip its smoke test", tc.age)
			o := decidePROwnership(pr, false, answeredEnv(t, []review.Aggregate{v}, review.DispatchState{}))
			band := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: pr, ownership: &o}, cardTestInputs(nil))
			if band.Key != tc.wantBand || band.Group != tc.wantGroup || band.ReasonCode != tc.wantCode {
				t.Fatalf("band %+v, want %s/%s/%s", band, tc.wantGroup, tc.wantBand, tc.wantCode)
			}
			if band.Reason != o.Reason || band.Reason == "" {
				t.Fatalf("card_reason %q, want the decision's reason %q", band.Reason, o.Reason)
			}
			if cols := overviewCardCells(band); cols["card_reason"] != o.Reason {
				t.Fatalf("export card_reason %v, want %q", cols["card_reason"], o.Reason)
			}

			status := &StatusPayload{HiveID: "hive-test", Repos: []FrontendRepo{{
				Name: "r", Full: "o/r",
				OpenPrs:   []any{pr},
				Ownership: map[string]Ownership{ownershipKey(ownershipKindPR, pr.Number): o},
			}}}
			part := overviewPartitionOf(status, "")
			if tc.wantGroup == cardGroupWaiting {
				if part.prs.waiting != 1 || part.prs.waitingByCause[waitingCauseDecider] != 1 || part.prs.waitingByCause[waitingCauseAdjudicator] != 0 {
					t.Fatalf("prs partition %+v, want one waiting row with cause %q", part.prs, waitingCauseDecider)
				}
				return
			}
			if part.prs.waiting != 0 || part.prs.yourTurnByCode[OwnershipReasonReviewRequiresHuman] != 1 {
				t.Fatalf("prs partition %+v, want one your-turn %s row", part.prs, OwnershipReasonReviewRequiresHuman)
			}
		})
	}
}

// The PR waiting breakdown names each waiting return of decidePROwnership:
// a person's answer on the head is "a person answered the review", not the
// adjudicator wait, which stays for a hive needs-human PR within it.
func TestOverviewPRWaitingCauses(t *testing.T) {
	answeredPR := withHead12329(personPR11565(), "head-2")
	v := verdict12377(answeredPR, "head-2", review.VerdictRequiresHuman)
	answer := decisionAnswer(answeredPR, "head-2", review.HumanChangeRequestKindChanges, ownershipNow11565.Add(-time.Hour))
	answered := decidePROwnership(answeredPR, false, answeredEnv(t, []review.Aggregate{v}, review.DispatchState{}, answer))
	if !answered.WaitingElsewhere {
		t.Fatalf("answered = %+v, want waiting elsewhere", answered)
	}
	adjudicatedPR := hivePR11565(ownershipLabelNeedsHuman)
	adjudicatedPR.Number = 4
	status := &StatusPayload{HiveID: "hive-test", Repos: []FrontendRepo{{
		Name: "r", Full: "o/r",
		OpenPrs: []any{answeredPR, adjudicatedPR},
		Ownership: map[string]Ownership{
			ownershipKey(ownershipKindPR, answeredPR.Number):    answered,
			ownershipKey(ownershipKindPR, adjudicatedPR.Number): waitingElsewhereFor(adjudicatorWaitReason(ownershipNow11565)),
		},
	}}}
	part := overviewPartitionOf(status, "")
	if got := part.prs.waitingByCause; part.prs.waiting != 2 || got[waitingCauseAnswered] != 1 || got[waitingCauseAdjudicator] != 1 {
		t.Fatalf("prs waiting %d by cause %+v, want answered 1, adjudicator 1", part.prs.waiting, got)
	}
	for _, row := range part.prs.waitingBreakdown("") {
		if row.Key == "waiting-"+waitingCauseAnswered && row.Label != overviewWaitingCauseLabels[waitingCauseAnswered] {
			t.Fatalf("answered row %+v", row)
		}
	}
}

// A zero now cannot measure the window: today's decision stands.
func TestReviewDeciderZeroNowKeepsToday(t *testing.T) {
	pr := withHead12329(personPR11565(), "head-2")
	env := answeredEnv(t, []review.Aggregate{deciderVerdict(pr, "head-2", "@carol should decide", 2*time.Hour)}, review.DispatchState{})
	env.now = time.Time{}
	got := decidePROwnership(pr, false, env)
	checkOwnership11565(t, got, want11565{OwnershipVerbReview, OwnershipReasonReviewRequiresHuman})
	if got.Reason != deciderPlainReason {
		t.Fatalf("reason %q", got.Reason)
	}
}

// The window starts when the person was first asked, not at the newest
// verdict: a new head (or a re-drawn verdict) asking the same person again
// keeps the original date and lapses on time. A verdict in between that asked
// someone else starts a new run.
func TestReviewDeciderWindowSpansTheBlockingChain(t *testing.T) {
	pr := withHead12329(personPR11565(), "head-2")
	q := "@carol should decide whether the release job may skip its smoke test"
	day := func(age time.Duration) string { return ownershipNow11565.Add(-age).UTC().Format("Jan 2") }
	for _, tc := range []struct {
		name       string
		verdicts   []review.Aggregate
		wantWait   string
		wantReason string
	}{
		{name: "asked again on a new head, within the window",
			verdicts: []review.Aggregate{deciderVerdict(pr, "head-1", q, 2*24*time.Hour), deciderVerdict(pr, "head-2", q, time.Hour)},
			wantWait: "Waits on carol: Hive's review asked on " + day(2*24*time.Hour)},
		{name: "asked again on a new head, past the window from the first ask",
			verdicts:   []review.Aggregate{deciderVerdict(pr, "head-1", q, 4*24*time.Hour), deciderVerdict(pr, "head-2", q, time.Hour)},
			wantReason: "Hive's review needs a human decision: carol has not answered since " + day(4*24*time.Hour)},
		{name: "someone else was asked in between",
			verdicts: []review.Aggregate{deciderVerdict(pr, "head-0", q, 5*24*time.Hour), deciderVerdict(pr, "head-1", "@Erin42 should decide", 4*24*time.Hour), deciderVerdict(pr, "head-2", q, time.Hour)},
			wantWait: "Waits on carol: Hive's review asked on " + day(time.Hour)},
		{name: "an approving verdict in between ends the chain",
			verdicts: []review.Aggregate{deciderVerdict(pr, "head-0", q, 5*24*time.Hour), func() review.Aggregate {
				a := verdict12377(pr, "head-1", review.VerdictApprove)
				a.RecordedAt = ownershipNow11565.Add(-4 * 24 * time.Hour)
				return a
			}(), deciderVerdict(pr, "head-2", q, time.Hour)},
			wantWait: "Waits on carol: Hive's review asked on " + day(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decidePROwnership(pr, false, answeredEnv(t, tc.verdicts, review.DispatchState{}))
			if tc.wantWait != "" {
				if !got.WaitingElsewhere || got.Reason != tc.wantWait {
					t.Fatalf("got %+v, want waiting %q", got, tc.wantWait)
				}
				return
			}
			checkOwnership11565(t, got, want11565{OwnershipVerbReview, OwnershipReasonReviewRequiresHuman})
			if got.Reason != tc.wantReason {
				t.Fatalf("reason %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// The date carries the year only when it is not now's year.
func TestReviewDeciderDate(t *testing.T) {
	now := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	if got := reviewDeciderDate(time.Date(2025, 12, 30, 23, 0, 0, 0, time.UTC), now); got != "Dec 30, 2025" {
		t.Errorf("last year = %q", got)
	}
	if got := reviewDeciderDate(time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC), now); got != "Jan 1" {
		t.Errorf("this year = %q", got)
	}
	if got := reviewDeciderWaitReason("carol", time.Date(2025, 12, 30, 23, 0, 0, 0, time.UTC), now); got != "Waits on carol: Hive's review asked on Dec 30, 2025" {
		t.Errorf("wait reason = %q", got)
	}
}

// The env built from configuration knows the hive's own logins (the App bot,
// the additional bots, the AI author) and the dashboard owners, normalised.
func TestReviewDeciderLoginsFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.GitHub.AppID, cfg.GitHub.InstallationID, cfg.GitHub.AppSlug = 4242, 7, "hive-app"
	cfg.GitHub.AdditionalBotLogins = []string{"other-hive[bot]"}
	cfg.Project.AIAuthor = "hive-ai"
	cfg.Dashboard.AuthorizedUsers = []string{"github:Dave", "Carol:owner", "heidi:read"}
	self, owners := reviewDeciderLoginsFor(cfg)
	bot := cfg.GitHub.BotLogin()
	if bot == "" || !strings.HasSuffix(bot, "[bot]") {
		t.Fatalf("test config has no App bot login: %q", bot)
	}
	for _, want := range []string{bot, "other-hive[bot]", "hive-ai"} {
		found := false
		for _, l := range self {
			found = found || l == want
		}
		if !found {
			t.Errorf("self logins %q lack %q", self, want)
		}
	}
	if strings.Join(owners, ",") != "carol,dave" {
		t.Errorf("owners = %q, want carol,dave", owners)
	}
	env := newOwnershipEnv(cfg, nil, "", ownershipNow11565, nil)
	if !env.reviewSwarm.ignoredDeciderMention(strings.TrimSuffix(bot, "[bot]")) || !env.reviewSwarm.ignoredDeciderMention("Hive-AI") || !env.reviewSwarm.ignoredDeciderMention("other-hive") {
		t.Errorf("env does not drop the hive's own logins: %q", env.reviewSwarm.selfLogins)
	}
	if env.reviewSwarm.ignoredDeciderMention("carol") {
		t.Error("a person is dropped as a bot")
	}
	if strings.Join(env.reviewSwarm.ownerLogins, ",") != "carol,dave" {
		t.Errorf("env owners = %q", env.reviewSwarm.ownerLogins)
	}
	if self, owners := reviewDeciderLoginsFor(nil); self != nil || owners != nil {
		t.Errorf("nil config = %q %q", self, owners)
	}
}

// A waiting PR row no rule names is "other", never "answered".
func TestOverviewPRWaitingCauseOther(t *testing.T) {
	pr := personPR11565()
	status := &StatusPayload{HiveID: "hive-test", Repos: []FrontendRepo{{
		Name: "r", Full: "o/r",
		OpenPrs:   []any{pr},
		Ownership: map[string]Ownership{ownershipKey(ownershipKindPR, pr.Number): waitingElsewhereFor("Waits on something new")},
	}}}
	part := overviewPartitionOf(status, "")
	if got := part.prs.waitingByCause; part.prs.waiting != 1 || got[waitingCauseOther] != 1 || got[waitingCauseAnswered] != 0 {
		t.Fatalf("prs waiting %d by cause %+v, want other 1", part.prs.waiting, got)
	}
}
