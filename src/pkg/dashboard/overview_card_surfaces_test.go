package dashboard

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// cardPartitionStatus is a snapshot with a row in every card band, a forge
// lead on both kinds, a sidelined non-draft PR and a non-zero Unexplained.
// #31 to #34 are stored your-turn decisions for the PR-only Your turn bands
// (a sentinel flag, a person's approval, a hand merge) and for Other (a
// verb the card does not know).
func cardPartitionStatus() *StatusPayload {
	own := func(code string) Ownership { return yourTurn(code, "reason") }
	issue := func(n int, labels ...string) github.Issue {
		return github.Issue{Repo: cardTestRepo, Number: n, Labels: labels, CreatedAt: cardTestNow.Add(-48 * time.Hour), UpdatedAt: cardTestNow.Add(-time.Hour)}
	}
	assigned := issue(4)
	assigned.Assignees = []string{"alice"}
	pr := func(n int, author string, labels ...string) github.PullRequest {
		return github.PullRequest{Repo: cardTestRepo, Number: n, Author: author, Labels: labels, CreatedAt: cardTestNow.Add(-72 * time.Hour), UpdatedAt: cardTestNow.Add(-2 * time.Hour)}
	}
	needsAuthor := pr(22, "alice", "stage/071-needs-author")
	needsAuthor.Mergeable = github.MergeableNo
	bot := pr(23, "renovate[bot]", "stage/070-fixing")
	bot.HeadRef, bot.CIStatus, bot.FailingChecks = "renovate/foo", "failure", []string{"ci/test"}
	draft := pr(24, "alice")
	draft.Draft = true
	hive := pr(25, "hive-app[bot]", "stage/070-fixing")
	hive.AppAuthored = true
	// An App stale draft is listed on OpenPrs and as a sidelined draft
	// (client.go StaleDrafts); the card counts it once.
	staleDraft := pr(30, "hive-app[bot]")
	staleDraft.AppAuthored, staleDraft.Draft = true, true
	return &StatusPayload{
		HiveID:    "h1",
		Timestamp: cardTestNow.Format(time.RFC3339),
		Repos: []FrontendRepo{{
			Name: "demo", Full: cardTestRepo,
			Issues: 20, PRs: 17,
			ActionableIssues: []any{
				issue(1, "agent/strategist"), issue(2, ownershipLabelAlreadyDone), issue(3), assigned, issue(5), issue(13, "needs-triage"),
			},
			HeldIssues: []any{
				github.HoldItem{Repo: cardTestRepo, Number: 6, Type: "issue", Labels: []string{"hold"}, HoldSources: []string{github.HoldSourceLevelGate}},
				github.HoldItem{Repo: cardTestRepo, Number: 7, Type: "issue", Labels: []string{"hold"}, HoldSources: []string{github.HoldSourcePerson}},
			},
			NonActionableIssues: []any{
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 8, Bucket: repoWorkBucketHiveAdvisory, Reason: "Hive advisory report"},
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 9, Bucket: repoWorkBucketFiltered, Reason: "blocked", Labels: []string{"blocked"}},
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 10, Bucket: repoWorkBucketClaimedByPR},
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 11, Bucket: repoWorkBucketFiltered, Reason: ownershipLabelReporterConfirmation, Labels: []string{ownershipLabelReporterConfirmation}},
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 12, Bucket: repoWorkBucketFiltered, Reason: "needs-human", Labels: []string{"needs-human"}},
				// The claim guard's work-detail copy of actionable #3
				// (contributor_claim_guard.go): counted once, as #3.
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 3, Bucket: cardBucketActionable, Reason: "Contributor claim is advisory"},
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 14, Bucket: cardBucketClaimed, Reason: "Live issue claim by someone"},
				// A forge-cache seed the scanner has not placed: the remainder.
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 15, Bucket: cardBucketOther, Reason: forgeUnclassifiedReason},
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 16, Bucket: cardBucketPendingRelease, Reason: "Unanswered release owner step is parked for the owner"},
				// A webhook-added row with no ownership decision yet.
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 17, Bucket: repoWorkBucketReporterTriage, Reason: "Awaiting maintainer triage label"},
				// A bucket no rule knows: the one issue-side Unexplained.
				github.RepoWorkIssue{Repo: cardTestRepo, Number: 18, Bucket: "some_new_bucket"},
			},
			OpenPrs: []any{
				FrontendPR{PullRequest: pr(20, "alice", "needs-human")},
				FrontendPR{PullRequest: pr(21, "alice"), MergeVerdict: &github.MergeVerdict{State: github.MergeVerdictEligible}},
				FrontendPR{PullRequest: needsAuthor},
				FrontendPR{PullRequest: bot},
				FrontendPR{PullRequest: draft},
				FrontendPR{PullRequest: hive},
				FrontendPR{PullRequest: pr(26, "alice", "stage/085-needs-decision")},
				FrontendPR{PullRequest: staleDraft},
				FrontendPR{PullRequest: pr(31, "alice")},
				FrontendPR{PullRequest: pr(32, "alice")},
				FrontendPR{PullRequest: pr(33, "alice")},
				FrontendPR{PullRequest: pr(34, "alice")},
			},
			HeldPrs: []any{github.PullRequest{Repo: cardTestRepo, Number: 27, Author: "alice", Labels: []string{"hold"}}},
			NonActionablePrs: []any{
				github.RepoWorkPR{Repo: cardTestRepo, Number: 28, Author: "alice", Draft: true, Bucket: "draft"},
				github.RepoWorkPR{Repo: cardTestRepo, Number: 29, Author: "alice", Bucket: "filtered", Reason: "Exempt label"},
				github.RepoWorkPR{Repo: cardTestRepo, Number: 30, Author: "hive-app[bot]", Draft: true, AppAuthored: true, Bucket: "draft"},
			},
			Ownership: map[string]Ownership{
				ownershipKey(ownershipKindIssue, 1):  own(OwnershipReasonAgentFiledUnacknowledged),
				ownershipKey(ownershipKindIssue, 2):  own(OwnershipReasonVerifiedDone),
				ownershipKey(ownershipKindIssue, 5):  waitingElsewhere(),
				ownershipKey(ownershipKindIssue, 7):  waitingElsewhere(),
				ownershipKey(ownershipKindIssue, 10): waitingElsewhere(),
				ownershipKey(ownershipKindIssue, 11): waitingElsewhere(),
				ownershipKey(ownershipKindIssue, 12): own(OwnershipReasonParkedNeedsHuman),
				ownershipKey(ownershipKindPR, 20):    own(OwnershipReasonPersonPRNeedsHuman),
				ownershipKey(ownershipKindPR, 31):    own(OwnershipReasonSentinelFlagged),
				ownershipKey(ownershipKindPR, 32):    own(OwnershipReasonReviewDecision),
				ownershipKey(ownershipKindPR, 33):    own(OwnershipReasonHandMerge),
				ownershipKey(ownershipKindPR, 34):    {YourTurn: true, ReasonCode: "some_new_reason", Reason: "reason"},
			},
		}},
	}
}

// cardPartitionServer is a server whose stash holds one card-only your-turn
// verdict (#13, needs-triage) and whose dispatch loader reads no file.
func cardPartitionServer() *Server {
	s := &Server{}
	s.cardDispatch.load = func() (review.DispatchState, time.Time, error) { return review.DispatchState{}, time.Time{}, nil }
	s.cardDispatch.stat = func() time.Time { return time.Time{} }
	st := newCardStash(cardTestNow)
	st.addAdmission(queueAdmissionSnapshot{withheld: []AdmissionWithheldItem{
		{Repo: cardTestRepo, Number: 13, Key: cardTestRepo + "#13", Reason: contributorAdmissionReasonLabelSkipped, SkippedLabel: "needs-triage"},
	}}, 1, cardTestNow)
	s.storeCardStash(st)
	return s
}

// cardPartitionWant is every row's band in cardPartitionStatus.
var cardPartitionWant = map[string]string{
	"issue#1": cardBandTriage, "issue#2": cardBandToClose, "issue#3": cardBandQueued, "issue#4": cardBandWorking,
	"issue#5": cardBandWaitingOnItem, "issue#6": cardBandHeld, "issue#7": cardBandHumanHold, "issue#8": cardBandNotWork,
	"issue#9": cardBandWaitingOnItem, "issue#10": cardBandCoveredByPR, "issue#11": cardBandReporterConfirm,
	"issue#12": cardBandReview, "issue#13": cardBandTriage, "issue#14": cardBandWorking,
	"issue#16": cardBandDecide, "issue#17": cardBandAdmit, "issue#18": cardBandUnexplained,
	"pr#20": cardBandReview, "pr#21": cardBandWillMerge, "pr#22": cardBandAuthorUpdate, "pr#23": cardBandBotRebase,
	"pr#24": cardBandDraft, "pr#25": cardBandWorking, "pr#26": cardBandUnexplained, "pr#27": cardBandHeld, "pr#28": cardBandDraft,
	"pr#30": cardBandDraft, "pr#31": cardBandInspect, "pr#32": cardBandApprove, "pr#33": cardBandHandMerge, "pr#34": cardBandTurnOther,
}

// cardPartitionNotRows are fixture rows the card leaves to the remainder.
var cardPartitionNotRows = []string{"issue#15", "pr#29"}

func cardClassifyForTest(s *Server, status *StatusPayload) overviewCardResult {
	return cardClassifyAt(s, status, cardTestNow)
}

func cardClassifyAt(s *Server, status *StatusPayload, now time.Time) overviewCardResult {
	in := s.overviewCardInputs(now)
	in.autoMergeLabel = s.autoMergeLabel()
	return overviewCardClassify(status, in)
}

func TestOverviewCardPlacesEveryRowOnce(t *testing.T) {
	s := cardPartitionServer()
	status := cardPartitionStatus()
	res := cardClassifyForTest(s, status)
	if len(res.rows) != len(cardPartitionWant) {
		t.Errorf("classified %d rows, want %d", len(res.rows), len(cardPartitionWant))
	}
	for key, want := range cardPartitionWant {
		kind, num, _ := strings.Cut(key, "#")
		n := 0
		for _, c := range num {
			n = n*10 + int(c-'0')
		}
		got, ok := res.band(kind, cardTestRepo, n)
		if !ok || got.Key != want {
			t.Errorf("%s: band %q (%s), want %q", key, got.Key, got.ReasonCode, want)
		}
		if strings.ContainsAny(got.Reason, "\u2014\u2013@") {
			t.Errorf("%s: reason has a dash or a mention: %q", key, got.Reason)
		}
	}
	for _, key := range cardPartitionNotRows {
		kind, num, _ := strings.Cut(key, "#")
		n := 0
		for _, c := range num {
			n = n*10 + int(c-'0')
		}
		if _, ok := res.band(kind, cardTestRepo, n); ok {
			t.Errorf("%s must not be a card row; it is in the remainder", key)
		}
	}
	// A duplicate takes the first copy's band: #3 stays Queued, not the
	// work-detail copy's, and the stale draft is one Draft row.
	if b, _ := res.band(ownershipKindIssue, cardTestRepo, 3); b.Key != cardBandQueued {
		t.Errorf("duplicate #3 = %q, want the actionable copy's band", b.Key)
	}
	// ownership.your_turn implies the Your turn group.
	for _, repo := range status.Repos {
		for key, o := range repo.Ownership {
			kind, num, _ := strings.Cut(key, "#")
			n := 0
			for _, c := range num {
				n = n*10 + int(c-'0')
			}
			if b, _ := res.band(kind, cardTestRepo, n); o.YourTurn && b.Group != cardGroupYourTurn {
				t.Errorf("%s is your turn in ownership but in group %q", key, b.Group)
			}
		}
	}
	assertCardTextClean(t, "partition card", res.card)
}

// The six tiles add up to the displayed totals on any hive: with a forge
// lead, with unlisted items, with sidelined non-draft PRs, and all at once.
func TestOverviewCardTilesSumToTheDisplayedTotals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*FrontendRepo)
	}{
		{"as listed", func(r *FrontendRepo) { r.Issues, r.PRs, r.NonActionablePrs = 0, 0, r.NonActionablePrs[:1] }},
		{"forge lead", func(r *FrontendRepo) { r.Issues, r.PRs = 40, 30; r.NonActionablePrs = r.NonActionablePrs[:1] }},
		{"sidelined non-draft PRs", func(r *FrontendRepo) { r.Issues, r.PRs = 0, 0 }},
		{"unlisted items", func(r *FrontendRepo) {
			r.WorkBreakdown = &github.RepoWorkBreakdown{Issues: github.RepoIssueBreakdown{Actionable: 20}, PRs: github.RepoPRBreakdown{Actionable: 20}}
			r.UnlistedIssues, r.UnlistedPRs = 7, 9
		}},
		{"duplicates on both lists", func(r *FrontendRepo) {
			r.HeldIssues = append(append([]any(nil), r.HeldIssues...), github.HoldItem{Repo: cardTestRepo, Number: 1, Type: "issue", Labels: []string{"hold"}})
			r.HeldPrs = append(append([]any(nil), r.HeldPrs...), github.PullRequest{Repo: cardTestRepo, Number: 20, Labels: []string{"hold"}})
		}},
		{"an issues-feed echo of a PR", func(r *FrontendRepo) {
			r.ActionableIssues = append(append([]any(nil), r.ActionableIssues...), github.Issue{Repo: cardTestRepo, Number: 21, URL: "https://github.com/octo/demo/pull/21"})
		}},
		{"all at once", func(r *FrontendRepo) {
			r.Issues, r.PRs = 50, 50
			r.UnlistedIssues, r.UnlistedPRs = 3, 3
		}},
	} {
		status := cardPartitionStatus()
		tc.mutate(&status.Repos[0])
		card := cardClassifyForTest(cardPartitionServer(), status).card
		totals := overviewTotals(status)
		sum, issues, prs := 0, 0, 0
		for _, tile := range card.Tiles {
			sum += tile.Count
			issues += tile.Issues
			prs += tile.PRs
			if tile.Count != tile.Issues+tile.PRs {
				t.Errorf("%s: tile %s count %d != %d + %d", tc.name, tile.Key, tile.Count, tile.Issues, tile.PRs)
			}
		}
		if issues != totals.Issues.Forge || prs != totals.PRs.Forge || sum != totals.Issues.Forge+totals.PRs.Forge {
			t.Errorf("%s: tiles %d issues + %d PRs, displayed %d + %d (remainder %+v)", tc.name, issues, prs, totals.Issues.Forge, totals.PRs.Forge, card.Remainder)
		}
		res := cardClassifyForTest(cardPartitionServer(), status)
		if res.issueRows > totals.Issues.Forge || res.prRows > totals.PRs.Forge {
			t.Errorf("%s: card rows %d/%d exceed the displayed totals %d/%d", tc.name, res.issueRows, res.prRows, totals.Issues.Forge, totals.PRs.Forge)
		}
		if card.Remainder.Issues < 0 || card.Remainder.PRs < 0 {
			t.Errorf("%s: negative remainder %+v", tc.name, card.Remainder)
		}
		for i, group := range card.Groups {
			tile := card.Tiles[i]
			if group.Key != tile.Key {
				t.Fatalf("%s: group %q and tile %q out of order", tc.name, group.Key, tile.Key)
			}
			wantIssues, wantPRs := group.Issues, group.PRs
			if group.Key == cardGroupOutside {
				wantIssues += card.Remainder.Issues
				wantPRs += card.Remainder.PRs
			}
			if tile.Issues != wantIssues || tile.PRs != wantPRs {
				t.Errorf("%s: tile %s %d/%d, group %d/%d", tc.name, tile.Key, tile.Issues, tile.PRs, group.Issues, group.PRs)
			}
		}
	}
}

func TestOverviewCardNotInInboxCountsOnlyCardOnlyRows(t *testing.T) {
	card := cardClassifyForTest(cardPartitionServer(), cardPartitionStatus()).card
	// #13 (needs-triage), #16 (release step) and #17 (no decision yet) are
	// the card-only Your turn rows; the two Unexplained rows are listed
	// apart, not in the not-in-inbox count.
	if card.YourTurnNotInInbox != (OverviewCardKinds{Issues: 3}) {
		t.Errorf("your_turn_not_in_inbox = %+v, want 3 issues", card.YourTurnNotInInbox)
	}
	if card.Unexplained != (OverviewCardKinds{Issues: 1, PRs: 1}) {
		t.Errorf("unexplained = %+v", card.Unexplained)
	}
	yt := card.tile(cardGroupYourTurn)
	if yt.Issues != 7 || yt.PRs != 6 {
		t.Errorf("your-turn tile = %d issues, %d PRs; want 7, 6 (Unexplained counts inside)", yt.Issues, yt.PRs)
	}
}

// cardGolden is the counted shape of the card the golden file pins.
type cardGolden struct {
	Issues             map[string]int               `json:"issues"`
	PRs                map[string]int               `json:"prs"`
	Groups             map[string]OverviewCardKinds `json:"groups"`
	Tiles              map[string]int               `json:"tiles"`
	YourTurnNotInInbox OverviewCardKinds            `json:"your_turn_not_in_inbox"`
	Unexplained        OverviewCardKinds            `json:"unexplained"`
	Remainder          OverviewCardKinds            `json:"remainder"`
}

func TestOverviewCardGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/overview_card_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var want cardGolden
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	card := cardClassifyForTest(cardPartitionServer(), cardPartitionStatus()).card
	got := cardGolden{Issues: map[string]int{}, PRs: map[string]int{}, Groups: map[string]OverviewCardKinds{}, Tiles: map[string]int{},
		YourTurnNotInInbox: card.YourTurnNotInInbox, Unexplained: card.Unexplained, Remainder: card.Remainder}
	for _, b := range card.Issues {
		got.Issues[b.Key] = b.Count
	}
	for _, b := range card.PRs {
		got.PRs[b.Key] = b.Count
	}
	for _, g := range card.Groups {
		got.Groups[g.Key] = OverviewCardKinds{Issues: g.Issues, PRs: g.PRs}
	}
	for _, tile := range card.Tiles {
		got.Tiles[tile.Key] = tile.Count
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("card differs from testdata/overview_card_golden.json:\n%s", gotJSON)
	}
	// Every band has a row somewhere in the fixture.
	for _, key := range cardBandOrder {
		if got.Issues[key]+got.PRs[key] == 0 {
			t.Errorf("the golden fixture has no row in band %q", key)
		}
	}
	if !card.Admission.Present || card.Admission.Verdicts != 1 {
		t.Errorf("admission = %+v", card.Admission)
	}
}

// The served copy carries the card; everything else is unchanged: with the
// card fields and overview_card taken out, it marshals exactly as the copy
// with the card fields zeroed (the pre-card shape).
func TestOverviewCardLeavesTheRestOfStatusUnchanged(t *testing.T) {
	s := cardPartitionServer()
	served := s.statusWithOverviewBands(cardPartitionStatus(), cardTestNow)
	if served.OverviewCard == nil {
		t.Fatal("no overview_card")
	}
	raw, err := json.Marshal(served)
	if err != nil {
		t.Fatal(err)
	}
	var withCard any
	if err := json.Unmarshal(raw, &withCard); err != nil {
		t.Fatal(err)
	}
	stripCardKeys(withCard)
	legacy := *served
	legacy.OverviewCard = nil
	legacy.Repos = append([]FrontendRepo(nil), served.Repos...)
	for i := range legacy.Repos {
		r := &legacy.Repos[i]
		r.ActionableIssues = zeroCardFields(r.ActionableIssues)
		r.HeldIssues = zeroCardFields(r.HeldIssues)
		r.NonActionableIssues = zeroCardFields(r.NonActionableIssues)
		r.OpenPrs = zeroCardFields(r.OpenPrs)
		r.HeldPrs = zeroCardFields(r.HeldPrs)
		r.NonActionablePrs = zeroCardFields(r.NonActionablePrs)
	}
	legacyRaw, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatal(err)
	}
	var legacyAny any
	if err := json.Unmarshal(legacyRaw, &legacyAny); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(withCard)
	b, _ := json.Marshal(legacyAny)
	if string(a) != string(b) {
		t.Fatalf("status differs beyond the card fields:\nwith card stripped: %s\npre-card shape:     %s", a, b)
	}
	// Status rows carry the band, group and code, never the sentence.
	if strings.Contains(string(raw), `"card_reason"`) {
		t.Error("status rows must not carry card_reason")
	}
	for _, want := range []string{`"card_band"`, `"card_group"`, `"card_reason_code"`, `"overview_card"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("status JSON lacks %s", want)
		}
	}
}

func stripCardKeys(v any) {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range []string{"card_band", "card_group", "card_reason_code", "card_reason", "card_churn_prs", "overview_card"} {
			delete(x, key)
		}
		for _, child := range x {
			stripCardKeys(child)
		}
	case []any:
		for _, child := range x {
			stripCardKeys(child)
		}
	}
}

func zeroCardFields(rows []any) []any {
	if rows == nil {
		return nil
	}
	out := make([]any, 0, len(rows))
	for _, raw := range rows {
		switch v := raw.(type) {
		case overviewStatusIssue:
			v.overviewCardRowFields = overviewCardRowFields{}
			out = append(out, v)
		case overviewStatusHold:
			v.overviewCardRowFields = overviewCardRowFields{}
			out = append(out, v)
		case overviewStatusPR:
			v.overviewCardRowFields = overviewCardRowFields{}
			out = append(out, v)
		case overviewStatusNonActionable:
			v.overviewCardRowFields = overviewCardRowFields{}
			out = append(out, v)
		case overviewStatusNonActionablePR:
			out = append(out, v.RepoWorkPR)
		default:
			out = append(out, raw)
		}
	}
	return out
}

// Every reader of NonActionablePrs on a served copy still decodes the
// wrapped drafts.
func TestOverviewCardDraftWrapperDecodes(t *testing.T) {
	served := cardPartitionServer().statusWithOverviewBands(cardPartitionStatus(), cardTestNow)
	repo := served.Repos[0]
	wrapped := 0
	for _, raw := range repo.NonActionablePrs {
		if w, ok := raw.(overviewStatusNonActionablePR); ok {
			wrapped++
			if w.CardBand != cardBandDraft || w.CardGroup != cardGroupOutside {
				t.Errorf("draft wrapper card fields = %+v", w.overviewCardRowFields)
			}
		}
		if _, ok := repoWorkPR(raw); !ok {
			t.Errorf("repoWorkPR cannot decode %T", raw)
		}
		if _, ok := repoWorkPRRow(raw); !ok {
			t.Errorf("repoWorkPRRow cannot decode %T", raw)
		}
	}
	if wrapped != 2 {
		t.Errorf("%d draft rows wrapped, want 2 (the non-draft stays as it is)", wrapped)
	}
	ptr := &overviewStatusNonActionablePR{RepoWorkPR: github.RepoWorkPR{Number: 5}}
	if w, ok := repoWorkPR(ptr); !ok || w.Number != 5 {
		t.Error("repoWorkPR does not decode a wrapper pointer")
	}
	if w, ok := repoWorkPRRow(ptr); !ok || w.Number != 5 {
		t.Error("repoWorkPRRow does not decode a wrapper pointer")
	}
	if got := len(frontendRepoDraftPRs(repo)); got != 2 {
		t.Errorf("frontendRepoDraftPRs on the served copy = %d, want 2", got)
	}
	sidelined := map[int]bool{}
	for _, it := range repoOpenItems(repo) {
		if it.Kind == "pr" && it.Sidelined {
			sidelined[it.Number] = true
		}
	}
	if !sidelined[28] || !sidelined[29] {
		t.Errorf("repoOpenItems on the served copy lists sidelined PRs %v, want #28 and #29", sidelined)
	}
	_ = computeCriticalPath(served.Repos)
}

// open_items reads the PR equation's blocked term, which exists.
func TestOpenItemsBlockedReadsTheBlockedTerm(t *testing.T) {
	served := cardPartitionServer().statusWithOverviewBands(cardPartitionStatus(), cardTestNow)
	want := overviewTermCount(served.ActionableNow.PREquation, overviewTermBlocked)
	if want == 0 {
		t.Fatal("fixture has no blocked PR term")
	}
	if got := served.OpenItems.Buckets.Blocked.PRs; got != want {
		t.Errorf("open items Blocked PRs = %d, equation blocked = %d", got, want)
	}
}

func TestOverviewCardHistorySamples(t *testing.T) {
	s := cardPartitionServer()
	status := cardPartitionStatus()
	var e TrendHistoryEntry
	s.attachOverviewKPI(&e, status, cardTestNow)
	card := cardClassifyForTest(s, status).card
	for name, pair := range map[string]struct {
		got  *int
		tile string
	}{
		"your turn":  {e.OverviewCardYourTurn, cardGroupYourTurn},
		"waiting":    {e.OverviewCardWaiting, cardGroupWaiting},
		"held":       {e.OverviewCardHeld, cardGroupHeld},
		"blocked":    {e.OverviewCardBlocked, cardGroupBlocked},
		"actionable": {e.OverviewCardActionable, cardGroupActionable},
		"outside":    {e.OverviewCardOutside, cardGroupOutside},
	} {
		if pair.got == nil || *pair.got != card.tile(pair.tile).Count {
			t.Errorf("%s sample = %v, tile = %d", name, pair.got, card.tile(pair.tile).Count)
		}
	}
	// The equation-term samples keep their own meaning.
	part := overviewPartitionOf(status, s.autoMergeLabel())
	if e.OverviewYourTurn != part.issues.yourTurn+part.prs.yourTurn || e.OverviewBlocked != part.prs.blocked {
		t.Errorf("old samples changed: %+v", e)
	}
	payload := overviewKPIHistoryPayload([]TrendHistoryEntry{{Timestamp: 1, OverviewKPI: true}, e})
	if payload[0].OverviewCardYourTurn != nil {
		t.Error("an entry from before the card must omit the card samples")
	}
	if payload[1].OverviewCardOutside == nil || *payload[1].OverviewCardOutside != card.tile(cardGroupOutside).Count {
		t.Errorf("payload card outside = %v", payload[1].OverviewCardOutside)
	}
}

func cardExport(t *testing.T, s *Server, kind, format, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/overview/"+kind+"."+format+query, nil)
	w := httptest.NewRecorder()
	s.handleOverviewExport(w, req, kind, format)
	return w
}

func TestOverviewExportCardColumnsAndFilters(t *testing.T) {
	s := cardPartitionServer()
	status := cardPartitionStatus()
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	// The handler classifies at the wall clock; so does the comparison.
	card := cardClassifyAt(s, status, time.Now().UTC()).card

	legacyIssueColumns := []string{"band", "band_rule", "bucket", "slice", "repo", "number", "title", "url", "state_signals", "labels", "assignees", "agent_role", "acknowledged", "held", "hold_reason", "linked_prs", "updated_at", "stale", "your_turn", "verb", "reason_code", "reason", "inbox_rank", "waiting_elsewhere"}
	legacyPRColumns := []string{"band", "band_rule", "slice", "repo", "number", "title", "url", "author", "draft", "merge_verdict", "mergeable", "failing_checks", "review_decision", "review_class", "held", "hold_reason", "labels", "updated_at", "stale", "signals", "your_turn", "verb", "reason_code", "reason", "inbox_rank", "waiting_elsewhere"}
	if got := overviewCSVColumns(overviewKindIssues); !reflect.DeepEqual(got, append(append(append([]string{}, legacyIssueColumns...), overviewCardColumns...), overviewCardChurnPRsColumn)) {
		t.Errorf("issue columns = %v", got)
	}
	if got := overviewCSVColumns(overviewKindPRs); !reflect.DeepEqual(got, append(append([]string{}, legacyPRColumns...), overviewCardColumns...)) {
		t.Errorf("PR columns = %v", got)
	}

	for _, kind := range []string{overviewKindIssues, overviewKindPRs} {
		specs := card.Issues
		if kind == overviewKindPRs {
			specs = card.PRs
		}
		var all overviewJSONResponse
		if err := json.Unmarshal(cardExport(t, s, kind, "json", "").Body.Bytes(), &all); err != nil {
			t.Fatal(err)
		}
		if len(all.CardBands) != len(specs) || len(all.CardGroups) != len(cardGroupOrder) {
			t.Fatalf("%s: card_bands %d, card_groups %d", kind, len(all.CardBands), len(all.CardGroups))
		}
		remainder := card.Remainder.Issues
		if kind == overviewKindPRs {
			remainder = card.Remainder.PRs
		}
		if all.CardRemainder != remainder {
			t.Errorf("%s: card_remainder %d, status remainder %d", kind, all.CardRemainder, remainder)
		}
		for i, spec := range specs {
			if all.CardBands[i].Key != spec.Key || all.CardBands[i].Count != spec.Count {
				t.Errorf("%s: card_bands[%d] = %+v, status %+v", kind, i, all.CardBands[i], spec)
			}
			// Each card_band download returns exactly the rows its band counts.
			var one overviewJSONResponse
			if err := json.Unmarshal(cardExport(t, s, kind, "json", "?card_band="+spec.Key).Body.Bytes(), &one); err != nil {
				t.Fatal(err)
			}
			if len(one.Rows) != spec.Count {
				t.Errorf("%s card_band=%s: %d rows, band counts %d", kind, spec.Key, len(one.Rows), spec.Count)
			}
			for _, row := range one.Rows {
				m, _ := row.(map[string]any)
				if m["card_band"] != spec.Key || m["card_reason"] == "" || m["card_reason_code"] == "" {
					t.Errorf("%s card_band=%s row = %v", kind, spec.Key, m)
				}
			}
		}
		for _, group := range all.CardGroups {
			var one overviewJSONResponse
			if err := json.Unmarshal(cardExport(t, s, kind, "json", "?card_group="+group.Key).Body.Bytes(), &one); err != nil {
				t.Fatal(err)
			}
			if len(one.Rows) != group.Count {
				t.Errorf("%s card_group=%s: %d rows, group counts %d", kind, group.Key, len(one.Rows), group.Count)
			}
		}
		if w := cardExport(t, s, kind, "json", "?card_band=nope"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "valid card bands") {
			t.Errorf("%s: unknown card_band = %d %q", kind, w.Code, w.Body.String())
		}
		if w := cardExport(t, s, kind, "json", "?card_group=nope"); w.Code != http.StatusBadRequest {
			t.Errorf("%s: unknown card_group = %d", kind, w.Code)
		}
	}
	var issues overviewJSONResponse
	if err := json.Unmarshal(cardExport(t, s, overviewKindIssues, "json", "").Body.Bytes(), &issues); err != nil {
		t.Fatal(err)
	}
	for _, g := range issues.CardGroups {
		if g.Key == cardGroupYourTurn && (g.NotInInbox != 3 || g.Unexplained != 1) {
			t.Errorf("issue your-turn group = %+v, want 3 not in the inbox and 1 unexplained", g)
		}
	}
	// The default export is today's row set.
	rows, _ := s.overviewRows(status, overviewKindIssues, s.issueBandsConfig(), overviewFilters{}, cardTestNow)
	if len(issues.Rows) != len(rows) {
		t.Errorf("default export %d rows, overviewRows %d", len(issues.Rows), len(rows))
	}
	csv := cardExport(t, s, overviewKindPRs, "csv", "").Body.String()
	header := strings.SplitN(strings.TrimPrefix(csv, "\ufeff"), "\r\n", 2)[0]
	if !strings.HasSuffix(header, ",waiting_elsewhere,card_band,card_group,card_reason,card_reason_code") {
		t.Errorf("PR CSV header = %q", header)
	}
}

// The Overview legend's Status subtotal exports every group but Your turn
// in one link, card_group=<the other groups, comma-joined>. That export
// returns exactly the rows those groups count, which are the donut's rows
// outside the Your turn bands, and none of the Your turn rows.
func TestOverviewExportCardGroupStatusSection(t *testing.T) {
	s := cardPartitionServer()
	status := cardPartitionStatus()
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	card := cardClassifyAt(s, status, time.Now().UTC()).card
	for _, kind := range []string{overviewKindIssues, overviewKindPRs} {
		specs := card.Issues
		if kind == overviewKindPRs {
			specs = card.PRs
		}
		var all overviewJSONResponse
		if err := json.Unmarshal(cardExport(t, s, kind, "json", "").Body.Bytes(), &all); err != nil {
			t.Fatal(err)
		}
		others := make([]string, 0, len(all.CardGroups))
		want := 0
		for _, group := range all.CardGroups {
			if group.Key == cardGroupYourTurn {
				continue
			}
			others = append(others, group.Key)
			want += group.Count
		}
		donut := 0
		for _, spec := range specs {
			if spec.Group != cardGroupYourTurn {
				donut += spec.Count
			}
		}
		if want != donut {
			t.Errorf("%s: the other groups count %d, the donut's other bands %d", kind, want, donut)
		}
		w := cardExport(t, s, kind, "json", "?card_group="+strings.Join(others, ","))
		if w.Code != http.StatusOK {
			t.Fatalf("%s card_group=%s: status %d %q", kind, strings.Join(others, ","), w.Code, w.Body.String())
		}
		var section overviewJSONResponse
		if err := json.Unmarshal(w.Body.Bytes(), &section); err != nil {
			t.Fatal(err)
		}
		if want == 0 {
			t.Fatalf("%s: the fixture has no rows outside Your turn", kind)
		}
		if len(section.Rows) != want {
			t.Errorf("%s: Status export %d rows, the other groups count %d", kind, len(section.Rows), want)
		}
		for _, row := range section.Rows {
			m, _ := row.(map[string]any)
			if m["card_group"] == cardGroupYourTurn {
				t.Errorf("%s: Status export has a Your turn row %v", kind, m)
			}
		}
	}
}

// adminmcp cannot import this package, so issues_by_band and prs_by_band
// carry their own copy of the card keys; it must match the card.
func TestAdminMCPCardKeysMatchTheCard(t *testing.T) {
	want := map[string][]string{adminmcp.ToolIssuesByBand: cardIssueBandOrder, adminmcp.ToolPrsByBand: cardPRBandOrder}
	for _, tool := range adminmcp.Tools() {
		name, _ := tool["name"].(string)
		bands, ok := want[name]
		if !ok {
			continue
		}
		props, _ := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		band, _ := props["card_band"].(map[string]any)
		// The donut's keys, then the alias of the single Your turn band of
		// before the split.
		if got, _ := band["enum"].([]string); !reflect.DeepEqual(got, append(append([]string{}, bands...), cardBandYourTurnAlias)) {
			t.Errorf("%s card_band enum = %v, card = %v", name, got, bands)
		}
		group, _ := props["card_group"].(map[string]any)
		if got, _ := group["enum"].([]string); !reflect.DeepEqual(got, cardGroupOrder) {
			t.Errorf("%s card_group enum = %v, card = %v", name, got, cardGroupOrder)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("tool %s not found", name)
	}
	// The your-turn alias keeps an issue churn row by its card reason code;
	// adminmcp spells that code itself, so it must be the card's. A plain
	// Yours to close row stays out.
	churn := decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1)}, cardTestInputs(cardTestStash(1, churnItem(3, 0))))
	if churn.Key != cardBandToClose || churn.ReasonCode != cardChurnReasonCode {
		t.Fatalf("churn row band %q code %q, want %q %q", churn.Key, churn.ReasonCode, cardBandToClose, cardChurnReasonCode)
	}
	closeRow := cardBand(cardBandToClose, "ownership:"+OwnershipReasonVerifiedDone, "reason")
	row := func(n int, b OverviewCardBand) any {
		return map[string]any{"number": float64(n), "repo": cardTestRepo, "card_band": b.Key, "card_group": b.Group, "card_reason_code": b.ReasonCode}
	}
	data := map[string]any{"kind": "issues", "rows": []any{row(1, churn), row(2, closeRow)}}
	out, err := adminmcp.BandReadResult(adminmcp.ToolIssuesByBand, data, map[string]any{"card_band": cardBandYourTurnAlias})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := out.(map[string]any)
	kept, _ := body["rows"].([]any)
	if len(kept) != 1 || kept[0].(map[string]any)["number"] != float64(1) {
		t.Errorf("issues_by_band card_band=your-turn kept %v, want only the churn row", kept)
	}
}

// Both publishers give the same card: one payload with the eval cycle's
// post-steps (merge verdicts again), one with the refresh's (an advisory
// digest).
func TestOverviewCardSameForBothPublishers(t *testing.T) {
	s := cardPartitionServer()
	evalCycle := cardPartitionStatus()
	verdicts := map[string]github.MergeVerdict{}
	for _, raw := range evalCycle.Repos[0].OpenPrs {
		if fp, ok := raw.(FrontendPR); ok && fp.MergeVerdict != nil {
			verdicts[github.MergeVerdictKey(fp.PullRequest)] = *fp.MergeVerdict
		}
	}
	AttachMergeVerdicts(evalCycle, verdicts)
	refresh := cardPartitionStatus()
	refresh.AdvisoryDigest = map[string]any{"summary": "x"}
	a := s.statusWithOverviewBands(evalCycle, cardTestNow)
	b := s.statusWithOverviewBands(refresh, cardTestNow)
	if !reflect.DeepEqual(a.OverviewCard, b.OverviewCard) {
		t.Fatal("the two publishers' cards differ")
	}
	ja, _ := json.Marshal(a.Repos)
	jb, _ := json.Marshal(b.Repos)
	if string(ja) != string(jb) {
		t.Fatal("the two publishers' rows differ")
	}
}

// The card is display only. Its identifiers live in the card files and the
// serving files that stamp or decode them, and nowhere else: no lane,
// ownership file or decoder elsewhere may read a card field. Its own code
// asks no forge, writes no store, reads no file and records nothing at the
// gate.
func TestOverviewCardSourceGuard(t *testing.T) {
	cardFiles := []string{"overview_card.go", "overview_card_rules.go", "overview_card_stash.go"}
	idents := map[string]bool{"overviewStatusNonActionablePR": true}
	fset := token.NewFileSet()
	for _, f := range cardFiles {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				idents[d.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						idents[sp.Name.Name] = true
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							idents[n.Name] = true
						}
					}
				}
			}
		}
	}
	// Method names that are too generic to search for.
	for _, generic := range []string{"tile", "band", "rowFields", "number", "labels", "bucket", "bucketReason", "reporterGate", "openBlockers", "add", "verdictFor", "stageFacts"} {
		delete(idents, generic)
	}
	names := make([]string, 0, len(idents))
	for name := range idents {
		names = append(names, regexp.QuoteMeta(name))
	}
	sort.Strings(names)
	pattern := regexp.MustCompile(`\b(` + strings.Join(names, "|") + `|card_band|card_group|card_reason)\b`)
	allowed := map[string]bool{
		"pkg/dashboard/overview_card.go": true, "pkg/dashboard/overview_card_rules.go": true, "pkg/dashboard/overview_card_stash.go": true,
		"pkg/dashboard/overview_status.go": true, "pkg/dashboard/overview_export.go": true, "pkg/dashboard/server.go": true,
		"pkg/dashboard/api_repo_hold.go": true, "pkg/dashboard/meetings_hive_status.go": true,
		"pkg/dashboard/stage_tracker_build.go": true, "pkg/dashboard/critical_path.go": true,
		"pkg/adminmcp/tools_band_ops.go": true, "pkg/adminmcp/adminmcp.go": true,
	}
	for _, dir := range []string{"../dashboard", "../adminmcp", "../../cmd/hive", "../github", "../stagelabels", "../review", "../writeback"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			rel := filepath.ToSlash(filepath.Join("pkg", filepath.Base(dir), name))
			if dir == "../../cmd/hive" {
				rel = "cmd/hive/" + name
			}
			if m := pattern.FindString(string(data)); m != "" && !allowed[rel] {
				t.Errorf("%s references the Overview card (%s); only the card and its serving files may", rel, m)
			}
		}
	}
	forbidden := []string{"stageStoreOrShared", "LoadDispatchState", ".Allowed(", "AllowedPreview", ".Observe(", "github.Client", "GHClient", "os.ReadFile", "http."}
	for _, f := range []string{"overview_card.go", "overview_card_rules.go"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(data), bad) {
				t.Errorf("%s uses %s; the card's classifiers read only their inputs", f, bad)
			}
		}
	}
	// The stash file reads the store only through Get, and the dispatch
	// file only in cardFixDispatch's loader.
	stash, err := os.ReadFile("overview_card_stash.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{".Observe(", ".Save(", ".Allowed(", "AllowedPreview", "github.Client"} {
		if strings.Contains(string(stash), bad) {
			t.Errorf("overview_card_stash.go uses %s", bad)
		}
	}
	// No string literal in the card's code carries an em or en dash or an
	// at sign: everything it emits is dash-free and mention-free.
	for _, f := range cardFiles {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING && strings.ContainsAny(lit.Value, "\u2014\u2013@") {
				t.Errorf("%s: string literal %s has a dash or an at sign", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
}
