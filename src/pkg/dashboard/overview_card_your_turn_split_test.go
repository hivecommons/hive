package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The Your turn group's bands say what the person has to do, read from the
// stored decision's verb and reason code. Every reason code the decision can
// produce lands in the Your turn group, on a band its kind's donut plots.
func TestCardTurnBandFromTheStoredDecision(t *testing.T) {
	want := map[string]string{
		OwnershipVerbInspect: cardBandInspect,
		OwnershipVerbDecide:  cardBandDecide,
		OwnershipVerbSteer:   cardBandDecide,
		OwnershipVerbSpec:    cardBandDecide,
		OwnershipVerbReview:  cardBandReview,
		OwnershipVerbTriage:  cardBandTriage,
		OwnershipVerbAdmit:   cardBandAdmit,
		OwnershipVerbClose:   cardBandToClose,
	}
	prOnly := map[string]string{
		OwnershipReasonReviewDecision: cardBandApprove,
		OwnershipReasonHandMerge:      cardBandHandMerge,
	}
	inOrder := func(order []string, key string) bool {
		for _, k := range order {
			if k == key {
				return true
			}
		}
		return false
	}
	if len(ownershipReasonVerbs) == 0 {
		t.Fatal("no reason codes registered")
	}
	for code, verb := range ownershipReasonVerbs {
		issue := decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1), ownership: cardTestOwnership(code)}, cardTestInputs(nil))
		pr := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice"), ownership: cardTestOwnership(code)}, cardTestInputs(nil))
		wantIssue, ok := want[verb]
		if !ok {
			wantIssue = cardBandTurnOther
		}
		wantPR := wantIssue
		if band, ok := prOnly[code]; ok {
			wantPR = band
		}
		if issue.Key != wantIssue || issue.ReasonCode != "ownership:"+code {
			t.Errorf("issue %s (%s): band %q code %q, want %q", code, verb, issue.Key, issue.ReasonCode, wantIssue)
		}
		if pr.Key != wantPR || pr.ReasonCode != "ownership:"+code {
			t.Errorf("PR %s (%s): band %q code %q, want %q", code, verb, pr.Key, pr.ReasonCode, wantPR)
		}
		if issue.Group != cardGroupYourTurn || pr.Group != cardGroupYourTurn {
			t.Errorf("%s: groups %q / %q, want your-turn", code, issue.Group, pr.Group)
		}
		if !inOrder(cardIssueBandOrder, issue.Key) {
			t.Errorf("issue %s: band %q is not on the issue donut", code, issue.Key)
		}
		if !inOrder(cardPRBandOrder, pr.Key) {
			t.Errorf("PR %s: band %q is not on the PR donut", code, pr.Key)
		}
	}
	// The live hive's PR codes, spelled out.
	for code, band := range map[string]string{
		OwnershipReasonSentinelFlagged:     cardBandInspect,
		OwnershipReasonParkedNeedsDecision: cardBandDecide,
		OwnershipReasonReviewDecision:      cardBandApprove,
		OwnershipReasonHandMerge:           cardBandHandMerge,
		OwnershipReasonPersonPRNeedsHuman:  cardBandReview,
		OwnershipReasonReviewRequiresHuman: cardBandReview,
		OwnershipReasonReviewLoopCap:       cardBandReview,
		OwnershipReasonPolicyHold:          cardBandReview,
	} {
		if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice"), ownership: cardTestOwnership(code)}, cardTestInputs(nil)); got.Key != band {
			t.Errorf("PR %s: band %q, want %q", code, got.Key, band)
		}
	}
	// A your-turn decision with a verb the card does not know is Other,
	// never dropped.
	unknown := &Ownership{YourTurn: true, Verb: "SOMETHING_NEW", ReasonCode: "some_new_reason", Reason: "reason"}
	if got := decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1), ownership: unknown}, cardTestInputs(nil)); got.Key != cardBandTurnOther {
		t.Errorf("unknown verb on an issue: band %q, want Other", got.Key)
	}
	if got := decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice"), ownership: unknown}, cardTestInputs(nil)); got.Key != cardBandTurnOther {
		t.Errorf("unknown verb on a PR: band %q, want Other", got.Key)
	}
}

// cardTurnCase is one row the card placed in the single Your turn band (or
// Yours to close, or Unexplained) before the split.
type cardTurnCase struct {
	name string
	kind string
	o    *Ownership
	band func() OverviewCardBand
	want string
}

// cardTurnCases covers every rule that returns a Your turn band: the stored
// decision and each card-only rule, on both kinds.
func cardTurnCases() []cardTurnCase {
	issueRow := func(row cardIssueRow, in cardInputs) func() OverviewCardBand {
		row.repoFull = cardTestRepo
		return func() OverviewCardBand { return decideIssueCard(row, in) }
	}
	prRow := func(row cardPRRow, in cardInputs) func() OverviewCardBand {
		row.repoFull = cardTestRepo
		return func() OverviewCardBand { return decidePRCard(row, in) }
	}
	filtered := func(reason string) cardIssueRow {
		return cardIssueRow{work: cardWork(1, repoWorkBucketFiltered, reason), nonActionable: true}
	}
	verdict := func(item AdmissionWithheldItem) cardInputs { return cardTestInputs(cardTestStash(1, item)) }
	person := cardWork(1, repoWorkBucketFiltered, github.IssueFilterReason)
	person.AuthorIsHuman = true
	sharedCI := cardPR(1, "alice")
	sharedCI.Mergeable = github.MergeableNo
	sharedCI.SharedCIIncident = 2
	hive := cardPR(1, "hive-app[bot]", "stage/070-fixing")
	hive.AppAuthored = true
	exhausted := cardTestInputs(nil)
	exhausted.dispatch = review.DispatchState{FixAttempts: []review.FixAttempt{{Repo: cardTestRepo, Number: 1, Attempts: 99}}}
	bot := cardPR(1, "renovate[bot]", "stage/070-fixing")
	bot.HeadRef = "renovate/foo"
	baseRed := cardStageStash(1, cardStageFacts{Stage: stagelabels.ReviewPending, Reason: "base-branch-red:main", EnteredAt: cardTestNow.Add(-time.Hour)})
	botStage := cardStageStash(1, cardStageFacts{Stage: stagelabels.Fixing, EnteredAt: cardTestNow.Add(-time.Hour)})
	own := cardTestOwnership
	none := cardTestInputs(nil)
	// A non-hive bot's PR the stored decision holds for needs-human: red,
	// conflicting, and green and mergeable.
	botNeedsHuman := func(mutate func(*github.PullRequest)) cardPRRow {
		pr := cardPR(1, "some-app[bot]", "needs-human", "stage/070-fixing")
		pr.HeadRef = "renovate/foo"
		mutate(&pr)
		return cardPRRow{pr: pr, ownership: own(OwnershipReasonPersonPRNeedsHuman)}
	}
	botRed := botNeedsHuman(func(pr *github.PullRequest) { pr.CIStatus, pr.FailingChecks = "failure", []string{"ci/test"} })
	botConflict := botNeedsHuman(func(pr *github.PullRequest) { pr.Mergeable, pr.MergeableState = github.MergeableNo, "dirty" })
	botGreen := botNeedsHuman(func(pr *github.PullRequest) { pr.Mergeable, pr.MergeableState = github.MergeableYes, "clean" })
	personRed := cardPR(1, "alice", "needs-human")
	personRed.CIStatus, personRed.FailingChecks = "failure", []string{"ci/test"}
	return []cardTurnCase{
		// Issues, card-only rules (no stored your-turn decision).
		{"reporter triage", ownershipKindIssue, nil, issueRow(cardIssueRow{work: cardWork(1, repoWorkBucketReporterTriage, "Awaiting maintainer triage label"), nonActionable: true}, none), cardBandAdmit},
		{"release step", ownershipKindIssue, nil, issueRow(cardIssueRow{work: cardWork(1, cardBucketPendingRelease, "Unanswered release owner step is parked for the owner"), nonActionable: true}, none), cardBandDecide},
		{"possibly exists", ownershipKindIssue, nil, issueRow(filtered(config.PossiblyExistsStageLabel), none), cardBandTriage},
		{"parked needs-human", ownershipKindIssue, nil, issueRow(filtered(config.DefaultNeedsHumanLabel), none), cardBandReview},
		{"parked needs-decision", ownershipKindIssue, nil, issueRow(filtered(config.DefaultNeedsDecisionLabel), none), cardBandDecide},
		{"parked needs-direction", ownershipKindIssue, nil, issueRow(filtered(config.DefaultNeedsDirectionLabel), none), cardBandDecide},
		{"parked needs-spec", ownershipKindIssue, nil, issueRow(filtered(config.DefaultNeedsSpecLabel), none), cardBandDecide},
		{"issue filter, a person's issue", ownershipKindIssue, nil, issueRow(cardIssueRow{work: person, nonActionable: true}, none), cardBandAdmit},
		{"verdict: needs a decision", ownershipKindIssue, nil, issueRow(cardIssueRow{issue: cardIssue(1)}, verdict(AdmissionWithheldItem{Reason: completionVerdictNeedsDecision})), cardBandDecide},
		{"verdict: churn", ownershipKindIssue, nil, issueRow(cardIssueRow{issue: cardIssue(1)}, verdict(AdmissionWithheldItem{Reason: contributorAdmissionReasonIssueChurn, ChurnMerged: 3})), cardBandTriage},
		{"verdict: triage label", ownershipKindIssue, nil, issueRow(cardIssueRow{issue: cardIssue(1)}, verdict(AdmissionWithheldItem{Reason: contributorAdmissionReasonLabelSkipped, SkippedLabel: "needs-triage"})), cardBandTriage},
		{"verdict: decision label", ownershipKindIssue, nil, issueRow(cardIssueRow{issue: cardIssue(1)}, verdict(AdmissionWithheldItem{Reason: contributorAdmissionReasonLabelSkipped, SkippedLabel: config.DefaultNeedsDecisionLabel})), cardBandDecide},
		// Issues, the stored decision.
		{"issue sentinel", ownershipKindIssue, own(OwnershipReasonSentinelFlagged), nil, cardBandInspect},
		{"issue parked needs-human", ownershipKindIssue, own(OwnershipReasonParkedNeedsHuman), nil, cardBandReview},
		{"issue untrusted reporter", ownershipKindIssue, own(OwnershipReasonUntrustedReporter), nil, cardBandAdmit},
		{"issue agent proposal", ownershipKindIssue, own(OwnershipReasonAgentFiledUnacknowledged), nil, cardBandTriage},
		{"issue verified done", ownershipKindIssue, own(OwnershipReasonVerifiedDone), nil, cardBandToClose},
		{"issue unknown bucket", ownershipKindIssue, nil, issueRow(cardIssueRow{work: cardWork(1, "some_new_bucket", ""), nonActionable: true}, none), cardBandUnexplained},
		// PRs, card-only rules.
		{"answered bot PR", ownershipKindPR, cardTestWaiting(), prRow(cardPRRow{pr: cardPR(1, "renovate[bot]"), ownership: cardTestWaiting()}, none), cardBandTurnOther},
		{"red base branch", ownershipKindPR, nil, prRow(cardPRRow{pr: cardPR(1, "alice", "stage/050-review-pending")}, cardTestInputs(baseRed)), cardBandTurnOther},
		{"shared CI incident", ownershipKindPR, nil, prRow(cardPRRow{pr: sharedCI}, none), cardBandTurnOther},
		{"nobody fixing it", ownershipKindPR, nil, prRow(cardPRRow{pr: hive}, exhausted), cardBandTurnOther},
		{"bot PR the hive will not ask", ownershipKindPR, nil, prRow(cardPRRow{pr: bot}, cardTestInputs(botStage)), cardBandTurnOther},
		// PRs, the stored decision.
		{"PR sentinel", ownershipKindPR, own(OwnershipReasonSentinelFlagged), nil, cardBandInspect},
		{"PR approval", ownershipKindPR, own(OwnershipReasonReviewDecision), nil, cardBandApprove},
		{"PR hand merge", ownershipKindPR, own(OwnershipReasonHandMerge), nil, cardBandHandMerge},
		{"PR person needs human", ownershipKindPR, own(OwnershipReasonPersonPRNeedsHuman), nil, cardBandReview},
		{"PR person needs human, red", ownershipKindPR, own(OwnershipReasonPersonPRNeedsHuman), prRow(cardPRRow{pr: personRed, ownership: own(OwnershipReasonPersonPRNeedsHuman)}, none), cardBandReview},
		{"bot PR needs human, red", ownershipKindPR, own(OwnershipReasonPersonPRNeedsHuman), prRow(botRed, none), cardBandTurnOther},
		{"bot PR needs human, conflict", ownershipKindPR, own(OwnershipReasonPersonPRNeedsHuman), prRow(botConflict, none), cardBandTurnOther},
		{"bot PR needs human, green", ownershipKindPR, own(OwnershipReasonPersonPRNeedsHuman), prRow(botGreen, none), cardBandReview},
		{"PR parked for a decision", ownershipKindPR, own(OwnershipReasonParkedNeedsDecision), nil, cardBandDecide},
		{"PR superseded", ownershipKindPR, own(OwnershipReasonSupersededRecommendedClose), nil, cardBandToClose},
		{"PR 085 not your turn", ownershipKindPR, nil, prRow(cardPRRow{pr: cardPR(1, "alice", "stage/085-needs-decision")}, none), cardBandUnexplained},
	}
}

// The split changes only the breakdown: over a row for every rule that used
// to return the single Your turn band (plus Yours to close and Unexplained),
// every row stays in the Your turn group, the tile counts all of them, its
// sub-rows sum to the tile, each donut plots every one of its kind's rows,
// and the not-in-inbox count is what the single band gave.
func TestCardYourTurnSplitKeepsTheTileCount(t *testing.T) {
	cases := cardTurnCases()
	tally := newCardTally()
	wantKinds := OverviewCardKinds{}
	oldYourTurn := OverviewCardKinds{}
	notInInbox := OverviewCardKinds{}
	for _, tc := range cases {
		var got OverviewCardBand
		if tc.band != nil {
			got = tc.band()
		} else if tc.kind == ownershipKindPR {
			got = decidePRCard(cardPRRow{repoFull: cardTestRepo, pr: cardPR(1, "alice"), ownership: tc.o}, cardTestInputs(nil))
		} else {
			got = decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1), ownership: tc.o}, cardTestInputs(nil))
		}
		if got.Key != tc.want || got.Group != cardGroupYourTurn {
			t.Errorf("%s: band %q group %q (%s), want %q in your-turn", tc.name, got.Key, got.Group, got.ReasonCode, tc.want)
		}
		if strings.ContainsAny(got.Reason, "—–@") {
			t.Errorf("%s: reason has a dash or a mention: %q", tc.name, got.Reason)
		}
		tally.add(tc.kind, got, tc.o)
		// The single band of before the split: every Your turn row but
		// Yours to close and Unexplained.
		old := tc.want != cardBandToClose && tc.want != cardBandUnexplained
		inbox := tc.o != nil && tc.o.YourTurn
		if tc.kind == ownershipKindPR {
			wantKinds.PRs++
			if old {
				oldYourTurn.PRs++
			}
			if tc.want != cardBandUnexplained && !inbox {
				notInInbox.PRs++
			}
		} else {
			wantKinds.Issues++
			if old {
				oldYourTurn.Issues++
			}
			if tc.want != cardBandUnexplained && !inbox {
				notInInbox.Issues++
			}
		}
	}
	card := buildOverviewCard(tally, FrontendOverviewTotals{}, nil)
	tile := card.tile(cardGroupYourTurn)
	if tile.Count != len(cases) || tile.Issues != wantKinds.Issues || tile.PRs != wantKinds.PRs {
		t.Errorf("your-turn tile = %d (%d issues, %d PRs), want %d (%+v)", tile.Count, tile.Issues, tile.PRs, len(cases), wantKinds)
	}
	sum, subRows := 0, OverviewCardKinds{}
	for _, row := range tile.Breakdown {
		if cardBandDefs[row.Key].group != cardGroupYourTurn {
			t.Errorf("your-turn breakdown lists %q", row.Key)
		}
		sum += row.Count
		if row.Key != cardBandToClose && row.Key != cardBandUnexplained {
			subRows.Issues += row.Issues
			subRows.PRs += row.PRs
		}
	}
	if sum != tile.Count {
		t.Errorf("your-turn sub-rows sum to %d, tile %d", sum, tile.Count)
	}
	if subRows != oldYourTurn {
		t.Errorf("the what-to-do sub-rows count %+v, the single Your turn band counted %+v", subRows, oldYourTurn)
	}
	for _, donut := range []struct {
		kind  string
		specs []OverviewCardBandSpec
		want  int
	}{{"issues", card.Issues, wantKinds.Issues}, {"prs", card.PRs, wantKinds.PRs}} {
		n := 0
		for _, spec := range donut.specs {
			if spec.Group == cardGroupYourTurn {
				n += spec.Count
			}
		}
		if n != donut.want {
			t.Errorf("%s donut plots %d Your turn rows, want %d", donut.kind, n, donut.want)
		}
	}
	if card.YourTurnNotInInbox != notInInbox {
		t.Errorf("your_turn_not_in_inbox = %+v, want %+v", card.YourTurnNotInInbox, notInInbox)
	}
	for _, g := range card.Groups {
		if g.Key == cardGroupYourTurn && !g.Headed {
			t.Error("the Your turn group is headed, like Waiting on someone else")
		}
	}
	assertCardTextClean(t, "your-turn split card", card)
}

// The partition fixture's Your turn tile is what the single band gave:
// 13 rows, of which 5 issues and 5 PRs were the one Your turn band.
func TestCardYourTurnSplitPartitionFixture(t *testing.T) {
	card := cardClassifyForTest(cardPartitionServer(), cardPartitionStatus()).card
	tile := card.tile(cardGroupYourTurn)
	if tile.Count != 13 {
		t.Errorf("your-turn tile = %d, want 13", tile.Count)
	}
	sub := OverviewCardKinds{}
	sum := 0
	for _, row := range tile.Breakdown {
		sum += row.Count
		if row.Key != cardBandToClose && row.Key != cardBandUnexplained {
			sub.Issues += row.Issues
			sub.PRs += row.PRs
		}
	}
	if sum != tile.Count {
		t.Errorf("sub-rows sum to %d, tile %d", sum, tile.Count)
	}
	if sub != (OverviewCardKinds{Issues: 5, PRs: 5}) {
		t.Errorf("what-to-do sub-rows = %+v, want 5 issues and 5 PRs", sub)
	}
}

// A parked row with no decision yet takes the park the decision reads:
// the recorded bucket, its labels in the decision's order.
func TestCardYourTurnSplitParkFollowsTheDecisionsLabelOrder(t *testing.T) {
	w := cardWork(1, repoWorkBucketFiltered, config.DefaultNeedsDecisionLabel, config.DefaultNeedsDecisionLabel, config.DefaultNeedsHumanLabel)
	row := cardIssueRow{repoFull: cardTestRepo, work: w, nonActionable: true}
	want := cardTurnBand(parkedOwnership(cardIssueParking(row, nil)).Verb, "", ownershipKindIssue)
	if got := decideIssueCard(row, cardTestInputs(nil)); got.Key != want || got.ReasonCode != "bucket:filtered:parked" {
		t.Errorf("parked with two park labels: band %q code %q, want %q", got.Key, got.ReasonCode, want)
	}
	if want != cardBandReview {
		t.Errorf("needs-human comes first in the decision's order; got %q", want)
	}
}

// card_band=your-turn, the single Your turn band of before the split, still
// works: it returns the rows that band held (every Your turn band but Yours
// to close and Unexplained), joins other card_band values as a union, and
// card_group=your-turn returns the whole group.
func TestCardYourTurnSplitBandAlias(t *testing.T) {
	s := cardPartitionServer()
	status := cardPartitionStatus()
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	// The handler classifies at the wall clock; so does the comparison.
	card := cardClassifyAt(s, status, time.Now().UTC()).card
	rows := func(kind, query string) []map[string]any {
		t.Helper()
		w := cardExport(t, s, kind, "json", query)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %q", kind, query, w.Code, w.Body.String())
		}
		var body overviewJSONResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := make([]map[string]any, 0, len(body.Rows))
		for _, row := range body.Rows {
			m, _ := row.(map[string]any)
			out = append(out, m)
		}
		return out
	}
	for _, kind := range []string{overviewKindIssues, overviewKindPRs} {
		specs := card.Issues
		if kind == overviewKindPRs {
			specs = card.PRs
		}
		action, toClose, group := 0, 0, 0
		for _, spec := range specs {
			if cardTurnAction(spec.Key) {
				action += spec.Count
			}
			if spec.Key == cardBandToClose {
				toClose = spec.Count
			}
			if spec.Group == cardGroupYourTurn {
				group += spec.Count
			}
		}
		if action == 0 {
			t.Fatalf("%s: the fixture has no Your turn action row", kind)
		}
		alias := rows(kind, "?card_band=your-turn")
		if len(alias) != action {
			t.Errorf("%s card_band=your-turn: %d rows, want the %d action rows", kind, len(alias), action)
		}
		for _, m := range alias {
			band, _ := m["card_band"].(string)
			if !cardTurnAction(band) || m["card_group"] != cardGroupYourTurn {
				t.Errorf("%s card_band=your-turn returned a %q row", kind, band)
			}
		}
		if got := len(rows(kind, "?card_band=your-turn&card_band=to-close")); got != action+toClose {
			t.Errorf("%s card_band=your-turn plus to-close: %d rows, want %d", kind, got, action+toClose)
		}
		if got := len(rows(kind, "?card_group=your-turn")); got != group || group <= action {
			t.Errorf("%s card_group=your-turn: %d rows, group %d, action rows %d", kind, got, group, action)
		}
	}
}

// Every band on a donut, and every headed group, has its colour class.
func TestCardYourTurnSplitBandsHaveColours(t *testing.T) {
	html := indexHTML(t)
	for _, donut := range []struct {
		kind  string
		order []string
	}{{"issue", cardIssueBandOrder}, {"pr", cardPRBandOrder}} {
		for _, key := range donut.order {
			if class := ".overview-card-" + donut.kind + "-" + key; !strings.Contains(html, class+",") && !strings.Contains(html, class+" {") {
				t.Errorf("index.html has no %s class", class)
			}
		}
	}
	for _, group := range cardGroupOrder {
		if cardGroupDefs[group].headed && !strings.Contains(html, ".overview-card-group-"+group+" {") {
			t.Errorf("headed group %s has no colour class", group)
		}
	}
	for _, token := range []string{"--card-band-inspect", "--card-band-decide", "--card-band-approve", "--card-band-merge-by-hand", "--card-band-review", "--card-band-triage", "--card-band-admit", "--card-band-your-turn-other"} {
		if strings.Count(html, token+":") != 2 {
			t.Errorf("%s is not defined once for dark and once for light mode", token)
		}
	}
}

// A non-hive bot's PR the stored decision holds for needs-human is Other
// when its branch is red or conflicting, with text that says what clears
// it, and Review when it is green; it keeps the ownership reason code and
// the Your turn group either way. A person's PR is untouched.
func TestCardBotNeedsHumanPR(t *testing.T) {
	code := "ownership:" + OwnershipReasonPersonPRNeedsHuman
	for _, tc := range cardTurnCases() {
		if tc.kind != ownershipKindPR || tc.o == nil || tc.o.ReasonCode != OwnershipReasonPersonPRNeedsHuman || tc.band == nil {
			continue
		}
		got := tc.band()
		if got.Key != tc.want || got.Group != cardGroupYourTurn || got.ReasonCode != code {
			t.Errorf("%s: band %q group %q code %q, want %q in your-turn with %q", tc.name, got.Key, got.Group, got.ReasonCode, tc.want, code)
		}
		wantReason := tc.o.Reason
		if tc.want == cardBandTurnOther {
			wantReason = cardBotNeedsHumanReason
		}
		if got.Reason != wantReason {
			t.Errorf("%s: reason %q, want %q", tc.name, got.Reason, wantReason)
		}
		if strings.ContainsAny(got.Reason, "\u2014\u2013@") {
			t.Errorf("%s: reason has a dash or an at sign: %q", tc.name, got.Reason)
		}
	}
	if !strings.Contains(cardBotNeedsHumanReason, "needs-human") || !strings.Contains(cardBotNeedsHumanReason, "Ask the bot to rebase it (tick its rebase box), close it, or fix it by hand.") {
		t.Errorf("reason %q does not say what clears it", cardBotNeedsHumanReason)
	}
}
