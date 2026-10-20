package evidence

import (
	"strings"
	"testing"
	"time"
)

func outcomeTestBundle(repo string, n int, head string, created time.Time) *Bundle {
	return &Bundle{
		SchemaVersion: SchemaVersion, ID: BundleID(repo, n, head), Repo: repo, Number: n, HeadSHA: head,
		BaseSHA: "base", Author: Author{Login: "bee", Kind: AuthorHuman}, CreatedAt: created, UpdatedAt: created,
	}
}

// A forge-observed Outcome stamped on a live bundle (by the review-outcome
// ledger) makes the PR's outcome known: merged elsewhere carries a merge
// event and is flagged MergedElsewhere; closed is Closed; a hive-performed
// merge stays a plain merge.
func TestFoldPRsRecordedOutcome(t *testing.T) {
	reviewed := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	at := reviewed.Add(2 * time.Hour)

	elsewhere := outcomeTestBundle("o/r", 1, "aaa", reviewed)
	elsewhere.Outcome = &Outcome{Source: OutcomeSourceLedger, State: BackfillStateMerged, At: at, MergedBy: "alice", MergedAt: at.Add(-time.Minute)}
	elsewhere.Merge = OutcomeMergeEvent(elsewhere.Outcome, elsewhere.HeadSHA)

	closed := outcomeTestBundle("o/r", 2, "bbb", reviewed)
	closed.Outcome = &Outcome{Source: OutcomeSourceLedger, State: BackfillStateClosed, At: at, ClosedAt: at.Add(-time.Minute)}

	hive := outcomeTestBundle("o/r", 3, "ccc", reviewed)
	hive.Merge = &MergeEvent{Actor: "hive[bot]", Method: "squash", At: at, SHA: "m3"}

	unknown := outcomeTestBundle("o/r", 4, "ddd", reviewed)

	for _, b := range []*Bundle{elsewhere, closed, hive} {
		if err := b.Validate(); err != nil {
			t.Fatalf("%s does not validate: %v", b.ID, err)
		}
	}

	prs := FoldPRs([]*Bundle{elsewhere, closed, hive, unknown}, "", "")

	p1 := prs[PRKey("o/r", 1)]
	if !p1.OutcomeKnown() || p1.Merge == nil || !p1.MergedElsewhere || p1.Closed {
		t.Errorf("merged elsewhere: known=%v merge=%v elsewhere=%v closed=%v", p1.OutcomeKnown(), p1.Merge, p1.MergedElsewhere, p1.Closed)
	}
	if p1.Merge.Actor != "alice" || p1.Merge.Method != MergeUnrecorded || p1.Merge.SHA != "aaa" {
		t.Errorf("merge event = %+v", *p1.Merge)
	}
	p2 := prs[PRKey("o/r", 2)]
	if !p2.OutcomeKnown() || !p2.Closed || p2.Merge != nil || p2.MergedElsewhere {
		t.Errorf("closed: known=%v closed=%v merge=%v elsewhere=%v", p2.OutcomeKnown(), p2.Closed, p2.Merge, p2.MergedElsewhere)
	}
	p3 := prs[PRKey("o/r", 3)]
	if !p3.OutcomeKnown() || p3.Merge == nil || p3.MergedElsewhere {
		t.Errorf("hive merge: known=%v merge=%v elsewhere=%v", p3.OutcomeKnown(), p3.Merge, p3.MergedElsewhere)
	}
	if p4 := prs[PRKey("o/r", 4)]; p4.OutcomeKnown() {
		t.Errorf("live-only bundle must stay unknown, got outcome %+v merge %+v", p4.Outcome, p4.Merge)
	}
}

// ApplyOutcomeHints resolves only PRs whose records state no outcome, from
// the review-outcome ledger's rows: merged becomes a MergedElsewhere merge
// event, closed sets Closed, open only makes the outcome known, and a PR
// without a hint or with a record that already knows stays as it was.
func TestApplyOutcomeHints(t *testing.T) {
	reviewed := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	at := reviewed.Add(3 * time.Hour)
	mk := func(n int, head string) *Bundle { return outcomeTestBundle("o/r", n, head, reviewed) }

	merged, closed, open, none, hiveMerged := mk(1, "aaa"), mk(2, "bbb"), mk(3, "ccc"), mk(4, "ddd"), mk(5, "eee")
	hiveMerged.Merge = &MergeEvent{Actor: "hive[bot]", Method: "squash", At: at, SHA: "m5"}

	prs := FoldPRs([]*Bundle{merged, closed, open, none, hiveMerged}, "", "")
	ApplyOutcomeHints(prs, map[string]OutcomeHint{
		PRKey("o/r", 1): {State: BackfillStateMerged, MergedAt: at, At: at},
		PRKey("o/r", 2): {State: BackfillStateClosed, ClosedAt: at, At: at},
		PRKey("o/r", 3): {State: BackfillStateOpen, At: at},
		PRKey("o/r", 5): {State: BackfillStateClosed, ClosedAt: at, At: at},
		PRKey("o/r", 9): {State: BackfillStateMerged, MergedAt: at, At: at},
	})

	p1 := prs[PRKey("o/r", 1)]
	if !p1.OutcomeKnown() || p1.Merge == nil || !p1.MergedElsewhere || p1.Outcome == nil || p1.Outcome.Source != OutcomeSourceLedger {
		t.Errorf("merged hint: known=%v merge=%v elsewhere=%v outcome=%+v", p1.OutcomeKnown(), p1.Merge, p1.MergedElsewhere, p1.Outcome)
	}
	if p1.Merge != nil && (p1.Merge.Actor != MergeUnrecorded || p1.Merge.SHA != "aaa" || !p1.Merge.At.Equal(at)) {
		t.Errorf("merged hint event = %+v", *p1.Merge)
	}
	if p2 := prs[PRKey("o/r", 2)]; !p2.OutcomeKnown() || !p2.Closed || p2.Merge != nil {
		t.Errorf("closed hint: known=%v closed=%v merge=%v", p2.OutcomeKnown(), p2.Closed, p2.Merge)
	}
	if p3 := prs[PRKey("o/r", 3)]; !p3.OutcomeKnown() || p3.Closed || p3.Merge != nil {
		t.Errorf("open hint: known=%v closed=%v merge=%v", p3.OutcomeKnown(), p3.Closed, p3.Merge)
	}
	if p4 := prs[PRKey("o/r", 4)]; p4.OutcomeKnown() {
		t.Errorf("no hint: must stay unknown, got %+v", p4.Outcome)
	}
	if p5 := prs[PRKey("o/r", 5)]; p5.Closed || p5.MergedElsewhere || p5.Outcome != nil {
		t.Errorf("hive merge must not be overridden by a hint: closed=%v elsewhere=%v outcome=%+v", p5.Closed, p5.MergedElsewhere, p5.Outcome)
	}
	ApplyOutcomeHints(prs, nil)
	ApplyOutcomeHints(nil, map[string]OutcomeHint{PRKey("o/r", 1): {State: BackfillStateOpen}})
}

// An Outcome must name its source and time, use a known state, and a merged
// one must come with a merge event.
func TestValidateOutcome(t *testing.T) {
	at := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	b := outcomeTestBundle("o/r", 1, "aaa", at)
	cases := map[string]*Outcome{
		"missing source":  {State: BackfillStateClosed, At: at},
		"missing at":      {Source: OutcomeSourceLedger, State: BackfillStateClosed},
		"bad state":       {Source: OutcomeSourceLedger, State: "reopened", At: at},
		"merged no event": {Source: OutcomeSourceLedger, State: BackfillStateMerged, At: at},
	}
	for name, o := range cases {
		b.Outcome = o
		if err := b.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	b.Outcome = &Outcome{Source: OutcomeSourceLedger, State: BackfillStateClosed, At: at}
	if err := b.Validate(); err != nil {
		t.Errorf("closed outcome: %v", err)
	}
}

func TestOutcomeBundleMatchesSchema(t *testing.T) {
	root := loadSchema(t)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	b := outcomeTestBundle("o/r", 1, "h1", at.Add(-time.Hour))
	b.Outcome = &Outcome{Source: OutcomeSourceLedger, State: BackfillStateMerged, At: at, MergedBy: "alice", MergedAt: at.Add(-time.Minute)}
	b.Merge = OutcomeMergeEvent(b.Outcome, b.HeadSHA)
	if err := Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if errs := validateSchema(root, root, toGeneric(t, b), "$"); len(errs) > 0 {
		t.Fatalf("schema:\n%s", strings.Join(errs, "\n"))
	}
	closed := outcomeTestBundle("o/r", 2, "h2", at.Add(-time.Hour))
	closed.Outcome = &Outcome{Source: OutcomeSourceLedger, State: BackfillStateClosed, At: at, ClosedAt: at.Add(-time.Minute)}
	if err := Seal(closed, nil); err != nil {
		t.Fatal(err)
	}
	if err := closed.Validate(); err != nil {
		t.Fatalf("Validate closed: %v", err)
	}
	if errs := validateSchema(root, root, toGeneric(t, closed), "$"); len(errs) > 0 {
		t.Fatalf("schema closed:\n%s", strings.Join(errs, "\n"))
	}
}
