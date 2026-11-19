package github

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestParseVerificationMarkers(t *testing.T) {
	body := "Scanner verification: done.\n" + VerificationMarker(VerificationVerdictComplete, 19172, 19179) +
		"\n<!-- hive:verification verdict=partial prs=#5,acme/other#6,junk -->\n<!-- hive:verification verdict=maybe prs=1 -->"
	got := parseVerificationMarkers(body)
	if len(got) != 2 {
		t.Fatalf("markers = %+v, want the complete and partial ones (unknown verdict ignored)", got)
	}
	if got[0].Verdict != VerificationVerdictComplete || len(got[0].PRs) != 2 || got[0].PRs[0] != (ChildRef{Number: 19172}) {
		t.Errorf("complete marker = %+v", got[0])
	}
	if got[1].Verdict != VerificationVerdictPartial || len(got[1].PRs) != 2 || got[1].PRs[1] != (ChildRef{Repo: "acme/other", Number: 6}) {
		t.Errorf("partial marker = %+v (a non-number PR token is dropped)", got[1])
	}
	if VerificationMarker(VerificationVerdictPartial) != "<!-- hive:verification verdict=partial -->" {
		t.Errorf("marker without PRs = %q", VerificationMarker(VerificationVerdictPartial))
	}
}

func TestDecideVerifyCloseGuards(t *testing.T) {
	merged := []verifyClosePR{{Ref: ChildRef{Number: 21}, Cited: true, Merged: true}}
	ok := verifyCloseFacts{LatestVerdict: VerificationVerdictComplete, PRs: merged}
	if a, why := decideVerifyClose(ok); a != verifyCloseClose {
		t.Fatalf("complete, cited PR merged: action=%d (%s), want close", a, why)
	}
	cases := []struct {
		name string
		mut  func(*verifyCloseFacts)
		want verifyCloseAction
	}{
		{"held", func(f *verifyCloseFacts) { f.Held = true }, verifyCloseSkip},
		{"tracking", func(f *verifyCloseFacts) { f.Tracking = "label" }, verifyCloseSkip},
		{"umbrella", func(f *verifyCloseFacts) { f.Umbrella = true }, verifyCloseSkip},
		{"open children", func(f *verifyCloseFacts) { f.OpenChildren = true }, verifyCloseSkip},
		{"reopened by a person", func(f *verifyCloseFacts) { f.ReopenedBy = "alice" }, verifyCloseSkip},
		{"lane label removed by a person", func(f *verifyCloseFacts) { f.LaneRemovedBy = "alice" }, verifyCloseSkip},
		{"no marker", func(f *verifyCloseFacts) { f.NoVerdict = true }, verifyCloseSkip},
		{"partial", func(f *verifyCloseFacts) { f.LatestVerdict = VerificationVerdictPartial }, verifyClosePartial},
		{"notes disagree", func(f *verifyCloseFacts) { f.VerdictsDisagree = true }, verifyClosePark},
		{"cited PR not merged", func(f *verifyCloseFacts) {
			f.PRs = []verifyClosePR{{Ref: ChildRef{Number: 21}, Cited: true, Open: true}}
		}, verifyClosePark},
		{"open linked PR", func(f *verifyCloseFacts) {
			f.PRs = append(append([]verifyClosePR(nil), merged...), verifyClosePR{Ref: ChildRef{Number: 22}, Open: true})
		}, verifyClosePark},
		{"no cited PR", func(f *verifyCloseFacts) { f.PRs = nil }, verifyClosePark},
		{"too many PRs", func(f *verifyCloseFacts) { f.TooManyPRs = true }, verifyClosePark},
		{"reporter confirms", func(f *verifyCloseFacts) { f.ReporterGate = "human-filed bug" }, verifyClosePark},
	}
	for _, tc := range cases {
		f := ok
		f.PRs = append([]verifyClosePR(nil), ok.PRs...)
		tc.mut(&f)
		if a, why := decideVerifyClose(f); a != tc.want {
			t.Errorf("%s: action=%d (%s), want %d", tc.name, a, why, tc.want)
		}
	}
	linkedClosed := ok
	linkedClosed.PRs = append(append([]verifyClosePR(nil), merged...), verifyClosePR{Ref: ChildRef{Number: 23}})
	if a, _ := decideVerifyClose(linkedClosed); a != verifyCloseClose {
		t.Error("a linked PR closed unmerged does not block the close")
	}
}

func TestVerifyCloseCandidate(t *testing.T) {
	base := IssueStageInput{Repo: "acme/widgets", Number: 7, Labels: []string{PartiallyAddressedLabel}}
	if !verifyCloseCandidate(base) {
		t.Fatal("a partially-addressed issue is a candidate")
	}
	for name, in := range map[string]IssueStageInput{
		"plain issue":   {Labels: []string{"bug"}},
		"held":          {Labels: []string{PartiallyAddressedLabel}, Held: true},
		"hold label":    {Labels: []string{PartiallyAddressedLabel, "hold"}},
		"tracker":       {Labels: []string{PartiallyAddressedLabel}, TrackerSignal: true},
		"open children": {Labels: []string{PartiallyAddressedLabel}, Children: &IssueChildren{Total: 2, Completed: 1}},
	} {
		if verifyCloseCandidate(in) {
			t.Errorf("%s: must not be read", name)
		}
	}
	if !verifyCloseCandidate(IssueStageInput{Labels: []string{VerifyCloseLabel}}) {
		t.Error("an issue already in the lane is re-read so it can close once the guards hold")
	}
}

type verifyCloseCase struct {
	labels   []string
	body     string
	timeline []map[string]any
	// pullRequests maps PR number to merged_at ("" = open / unmerged).
	pullRequests map[int]string
	// closedPRs are PR numbers that are closed (merged or not).
	closedPRs []int
}

// runVerifyCloseSweeps sweeps issue #7 at stageNow() and again one settle
// interval later. It returns whether it closed after each sweep, the
// comments posted and the labels added.
func runVerifyCloseSweeps(t *testing.T, tc verifyCloseCase) (afterFirst, afterSecond bool, comments, added []string, timelineGets int) {
	t.Helper()
	resetTrackerReadsForTest(t)
	fake := newIssueStageServer(t)
	c := NewClientForTest(fake.srv.URL, "acme", []string{"widgets"}, testLogger())
	c.SetAppBotLogin("hive[bot]")
	c.SetIssueStageLabels(func() IssueStagePolicy { return IssueStagePolicy{Enabled: true} })
	store := stagelabels.NewStore("", stageNow)
	store.Observe(stagelabels.Key("acme/widgets", 7), stagelabels.Actionable, "actionable")
	fake.mu.Lock()
	fake.bodies[7] = tc.body
	fake.pullRequests = tc.pullRequests
	for _, n := range tc.closedPRs {
		fake.closed[n] = "completed"
	}
	fake.timelines = map[int][]map[string]any{7: tc.timeline}
	fake.mu.Unlock()

	build := func() *ActionableResult {
		result := &ActionableResult{}
		result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 7, Title: "Slice 5: knowledge advise route", Body: tc.body,
			Labels: append([]string{stagelabels.Actionable.Label()}, tc.labels...)}})
		return result
	}
	syncSeeded(t, fake, c, build(), store, stageNow())
	fake.mu.Lock()
	_, afterFirst = fake.closed[7]
	fake.mu.Unlock()
	syncSeeded(t, fake, c, build(), store, stageNow().Add(verifyCloseSettleInterval+time.Minute))
	fake.mu.Lock()
	defer fake.mu.Unlock()
	_, afterSecond = fake.closed[7]
	return afterFirst, afterSecond, append([]string(nil), fake.commentBodies[7]...), append([]string(nil), fake.added[7]...), fake.timelineGets
}

func scannerNote(marker string) map[string]any {
	return timelineEvent("commented", "hive[bot]", "Bot", "Scanner verification: #21 implements it.\n"+marker, "2026-11-19T04:54:00Z")
}

func TestVerifiedClose_CompleteClosesAfterSettleWithEvidence(t *testing.T) {
	first, second, comments, _, _ := runVerifyCloseSweeps(t, verifyCloseCase{
		labels:       []string{PartiallyAddressedLabel},
		timeline:     []map[string]any{scannerNote(VerificationMarker(VerificationVerdictComplete, 21))},
		pullRequests: map[int]string{21: "2026-11-19T06:00:00Z"},
		closedPRs:    []int{21},
	})
	if first {
		t.Fatal("the first read must only record the evidence, not close")
	}
	if !second {
		t.Fatal("the same complete verdict on a second read must close the issue")
	}
	var evidence string
	for _, c := range comments {
		if strings.Contains(c, verifyCloseEvidenceMarker) {
			evidence = c
		}
	}
	if evidence == "" || !strings.Contains(evidence, "PR #21: merged") || !strings.Contains(evidence, parentAutoCloseMarker) {
		t.Fatalf("evidence comment = %q", comments)
	}
}

func TestVerifiedClose_GuardUnmetParksForAPerson(t *testing.T) {
	for name, tc := range map[string]verifyCloseCase{
		"cited PR still open": {
			timeline:     []map[string]any{scannerNote(VerificationMarker(VerificationVerdictComplete, 21))},
			pullRequests: map[int]string{21: ""},
		},
		"open linked PR": {
			timeline: []map[string]any{
				scannerNote(VerificationMarker(VerificationVerdictComplete, 21)),
				{"event": "cross-referenced", "created_at": "2026-11-19T05:00:00Z", "source": map[string]any{"type": "issue", "issue": map[string]any{
					"number": 22, "pull_request": map[string]any{"url": "https://api.example/pulls"},
					"repository": map[string]any{"full_name": "acme/widgets"},
				}}},
			},
			pullRequests: map[int]string{21: "2026-11-19T06:00:00Z", 22: ""},
			closedPRs:    []int{21},
		},
		"notes disagree": {
			timeline: []map[string]any{
				scannerNote(VerificationMarker(VerificationVerdictPartial, 21)),
				scannerNote(VerificationMarker(VerificationVerdictComplete, 21)),
			},
			pullRequests: map[int]string{21: "2026-11-19T06:00:00Z"},
			closedPRs:    []int{21},
		},
	} {
		tc.labels = []string{PartiallyAddressedLabel}
		_, second, _, added, _ := runVerifyCloseSweeps(t, tc)
		if second {
			t.Errorf("%s: an unmet guard must not close", name)
		}
		if !slices.Contains(added, VerifyCloseLabel) {
			t.Errorf("%s: labels added = %v, want %s (the verify-close lane)", name, added, VerifyCloseLabel)
		}
	}
}

func TestVerifiedClose_PartialLabelsVerifiedOpen(t *testing.T) {
	_, second, _, added, _ := runVerifyCloseSweeps(t, verifyCloseCase{
		labels:   []string{PartiallyAddressedLabel},
		timeline: []map[string]any{scannerNote(VerificationMarker(VerificationVerdictPartial, 21))},
	})
	if second || !slices.Contains(added, VerifiedOpenLabel) || slices.Contains(added, VerifyCloseLabel) {
		t.Fatalf("partial verdict: closed=%v added=%v, want hive/verified-open only", second, added)
	}
}

func TestVerifiedClose_NeverTouched(t *testing.T) {
	complete := []map[string]any{scannerNote(VerificationMarker(VerificationVerdictComplete, 21))}
	prs := map[int]string{21: "2026-11-19T06:00:00Z"}
	for name, tc := range map[string]verifyCloseCase{
		"hold":     {labels: []string{PartiallyAddressedLabel, "hold"}, timeline: complete},
		"tracking": {labels: []string{PartiallyAddressedLabel, "hive/tracking"}, timeline: complete},
		"umbrella": {labels: []string{PartiallyAddressedLabel, "epic"}, timeline: complete},
		"reopened by a person": {labels: []string{PartiallyAddressedLabel}, timeline: append(append([]map[string]any(nil), complete...),
			timelineEvent("reopened", "alice", "User", "", "2026-11-19T07:00:00Z"))},
		"lane label removed by a person": {labels: []string{PartiallyAddressedLabel}, timeline: append(append([]map[string]any(nil), complete...),
			map[string]any{"event": "unlabeled", "created_at": "2026-11-19T07:00:00Z", "label": map[string]any{"name": VerifyCloseLabel}, "actor": map[string]any{"login": "alice", "type": "User"}})},
		"marker from someone else": {labels: []string{PartiallyAddressedLabel}, timeline: []map[string]any{
			timelineEvent("commented", "mallory", "User", VerificationMarker(VerificationVerdictComplete, 21), "2026-11-19T04:54:00Z")}},
		"no marker, free text only": {labels: []string{PartiallyAddressedLabel}, timeline: []map[string]any{
			timelineEvent("commented", "hive[bot]", "Bot", "Scanner verification: this can be closed.", "2026-11-19T04:54:00Z")}},
	} {
		tc.pullRequests = prs
		tc.closedPRs = []int{21}
		_, second, _, added, _ := runVerifyCloseSweeps(t, tc)
		if second || slices.Contains(added, VerifyCloseLabel) {
			t.Errorf("%s: closed=%v added=%v, want nothing", name, second, added)
		}
	}
}

func TestVerifiedClose_PlainIssueCostsNoReads(t *testing.T) {
	_, second, _, _, gets := runVerifyCloseSweeps(t, verifyCloseCase{
		labels:   []string{"bug"},
		timeline: []map[string]any{scannerNote(VerificationMarker(VerificationVerdictComplete, 21))},
	})
	if second || gets != 0 {
		t.Fatalf("an issue no merged PR partly addressed is never read: closed=%v timeline reads=%d", second, gets)
	}
}

func TestVerifyCloseLabelParksInDecisionBucket(t *testing.T) {
	if got := hardSuppressIssueLabel([]string{VerifyCloseLabel}); got != VerifyCloseLabel {
		t.Fatalf("hardSuppressIssueLabel = %q, want %s: the lane keeps agents off the issue", got, VerifyCloseLabel)
	}
	if got := parkedOnPersonLabel(IssueStageInput{Labels: []string{VerifyCloseLabel}}); got != VerifyCloseLabel {
		t.Fatalf("parkedOnPersonLabel = %q, want the lane label (045)", got)
	}
}
