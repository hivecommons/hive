package standby

import (
	"testing"
	"time"
)

// row builds one ledger row of the given kind for the donor under test. Only
// Kind is read by the rule; the rest is what an operator reading the file sees.
func row(kind OutcomeKind, number int) Outcome {
	return Outcome{
		Key:          donorKey,
		Lane:         "quality",
		Repo:         "org/x",
		Number:       number,
		DispatchedAt: now.Add(-48 * time.Hour),
		Kind:         kind,
		OutcomeAt:    now.Add(-24 * time.Hour),
	}
}

var donorKey = LedgerKey("alice", configFor(T1))

// ledger builds a donor's rows in append order, oldest first.
func ledger(kinds ...OutcomeKind) []Outcome {
	rows := make([]Outcome, 0, len(kinds))
	for i, k := range kinds {
		rows = append(rows, row(k, 40+i))
	}
	return rows
}

func TestSuspendStateTable(t *testing.T) {
	cases := []struct {
		name      string
		rows      []Outcome
		threshold int
		suspended bool
		streak    int
	}{
		{
			// The state every hive starts in.
			name: "an empty ledger is not suspended",
			rows: nil,
		},
		{
			name:   "one closed-unmerged is not yet two",
			rows:   ledger(OutcomeClosedUnmerged),
			streak: 1,
		},
		{
			name:      "two closed-unmerged in a row suspends",
			rows:      ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged),
			suspended: true,
			streak:    2,
		},
		{
			name:      "the streak keeps counting past the threshold",
			rows:      ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged, OutcomeClosedUnmerged),
			suspended: true,
			streak:    3,
		},
		{
			// The load-bearing case: the rule is CONSECUTIVE, not cumulative.
			name:   "a merge in between resets the streak",
			rows:   ledger(OutcomeClosedUnmerged, OutcomeMerged, OutcomeClosedUnmerged),
			streak: 1,
		},
		{
			name: "a merge after two closures clears the suspension",
			rows: ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged, OutcomeMerged),
		},
		{
			// The owner's clear is a row, not an edit: the two closures that
			// suspended the donor are still in the file behind it.
			name: "the owner clear reinstates",
			rows: ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged, OutcomeCleared),
		},
		{
			name:      "a closure after the clear starts a fresh streak",
			rows:      ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged, OutcomeCleared, OutcomeClosedUnmerged),
			suspended: false,
			streak:    1,
		},
		{
			name:      "two closures after the clear suspend again",
			rows:      ledger(OutcomeClosedUnmerged, OutcomeCleared, OutcomeClosedUnmerged, OutcomeClosedUnmerged),
			suspended: true,
			streak:    2,
		},
		{
			// merged_after_rework is evidence in NEITHER direction. It must not
			// reset the streak the way a clean merge does, and it must not
			// count toward it the way a closure does.
			name:      "merged_after_rework counts as neither and preserves the streak",
			rows:      ledger(OutcomeClosedUnmerged, OutcomeMergedAfterRework, OutcomeClosedUnmerged),
			suspended: true,
			streak:    2,
		},
		{
			name:   "merged_after_rework alone suspends nobody",
			rows:   ledger(OutcomeMergedAfterRework, OutcomeMergedAfterRework, OutcomeMergedAfterRework),
			streak: 0,
		},
		{
			// An open PR is not an outcome yet.
			name:   "open rows are skipped",
			rows:   ledger(OutcomeClosedUnmerged, OutcomeOpen, OutcomeOpen),
			streak: 1,
		},
		{
			// A row written by a newer build, or a corrupted one, must not be
			// read as evidence against the donor.
			name:   "an unrecognized outcome is skipped, not counted",
			rows:   ledger(OutcomeClosedUnmerged, OutcomeKind("abandoned"), OutcomeUnknown),
			streak: 1,
		},
		{
			name:      "the threshold is configurable upward",
			rows:      ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged),
			threshold: 3,
			streak:    2,
		},
		{
			name:      "three closures reach a threshold of three",
			rows:      ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged, OutcomeClosedUnmerged),
			threshold: 3,
			suspended: true,
			streak:    3,
		},
		{
			name:      "a threshold of one suspends on the first closure",
			rows:      ledger(OutcomeClosedUnmerged),
			threshold: 1,
			suspended: true,
			streak:    1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			suspended, streak := SuspendState(tc.rows, tc.threshold)
			if suspended != tc.suspended {
				t.Errorf("SuspendState suspended = %v, want %v", suspended, tc.suspended)
			}
			if streak != tc.streak {
				t.Errorf("SuspendState streak = %d, want %d", streak, tc.streak)
			}
		})
	}
}

// A threshold nobody set must mean the documented default. Reading the zero
// value literally — streak 0 >= threshold 0 — would suspend every donor on
// this hive, including ones with an empty ledger, on the first evaluation.
func TestNonPositiveThresholdIsTheDefaultNotZero(t *testing.T) {
	for _, threshold := range []int{0, -1, -7} {
		if suspended, streak := SuspendState(nil, threshold); suspended || streak != 0 {
			t.Errorf("threshold %d: empty ledger suspended = %v (streak %d), want false", threshold, suspended, streak)
		}
		if suspended, _ := SuspendState(ledger(OutcomeClosedUnmerged), threshold); suspended {
			t.Errorf("threshold %d: one closure suspended the donor", threshold)
		}
		if suspended, _ := SuspendState(ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged), threshold); !suspended {
			t.Errorf("threshold %d: two closures did not suspend the donor", threshold)
		}
	}
}

func TestSuspendedUsesTheDefaultThreshold(t *testing.T) {
	if Suspended(nil) {
		t.Error("an empty ledger is suspended")
	}
	if Suspended(ledger(OutcomeClosedUnmerged)) {
		t.Error("one closed-unmerged PR suspended the donor")
	}
	if !Suspended(ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged)) {
		t.Error("two closed-unmerged PRs did not suspend the donor")
	}
	if Suspended(ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged, OutcomeCleared)) {
		t.Error("the owner clear did not reinstate the donor")
	}
}

// The ledger is keyed on the contributor-plus-configuration tuple. One
// configuration's closures must never suspend another — including the same
// person on a different model, who the RFC treats as a different donor.
func TestSuspensionIsPerConfigurationNotPerPerson(t *testing.T) {
	strong := LedgerKey("alice", configFor(T1))
	weak := LedgerKey("alice", configFor(T3))
	if strong == weak {
		t.Fatal("two configurations for the same contributor share a ledger key")
	}

	mixed := []Outcome{
		{Key: weak, Kind: OutcomeClosedUnmerged},
		{Key: strong, Kind: OutcomeMerged},
		{Key: weak, Kind: OutcomeClosedUnmerged},
	}

	if !Suspended(OutcomesFor(mixed, weak)) {
		t.Error("the configuration with two closures is not suspended")
	}
	if Suspended(OutcomesFor(mixed, strong)) {
		t.Error("the other configuration was suspended by its sibling's closures")
	}
	// Handing the whole ledger in unfiltered would be the bug this filter
	// exists to prevent; assert the filter actually narrows.
	if got := len(OutcomesFor(mixed, weak)); got != 2 {
		t.Errorf("OutcomesFor(weak) returned %d rows, want 2", got)
	}
}

func TestLedgerKeyFoldsSpelling(t *testing.T) {
	lower := LedgerKey("alice", Configuration{Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "high"})
	shouty := LedgerKey("  Alice ", Configuration{Backend: "Claude", Model: "Claude-Opus-5", ReasoningEffort: " HIGH"})
	if lower != shouty {
		t.Errorf("spelling changed the ledger key: %q vs %q", lower, shouty)
	}
	if want := "alice|claude|claude-opus-5|high||"; lower != want {
		t.Errorf("LedgerKey = %q, want %q", lower, want)
	}
	// A different advisor is a different configuration, so a different donor.
	withAdvisor := LedgerKey("alice", Configuration{
		Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "high", AdvisorModel: "claude-haiku-4-5",
	})
	if withAdvisor == lower {
		t.Error("adding an advisor model did not change the ledger key")
	}
}

// OutcomesFor must not hand the caller a window onto its own ledger slice.
func TestOutcomesForDoesNotAlias(t *testing.T) {
	rows := ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged)
	got := OutcomesFor(rows, donorKey)
	if len(got) != 2 {
		t.Fatalf("OutcomesFor returned %d rows, want 2", len(got))
	}
	got[0].Kind = OutcomeMerged
	if rows[0].Kind != OutcomeClosedUnmerged {
		t.Error("mutating the returned rows changed the caller's ledger")
	}
}

func TestNormalizeOutcome(t *testing.T) {
	cases := map[string]OutcomeKind{
		"closed_unmerged":      OutcomeClosedUnmerged,
		"  Closed_Unmerged  ":  OutcomeClosedUnmerged,
		"MERGED":               OutcomeMerged,
		"merged_after_rework":  OutcomeMergedAfterRework,
		"cleared":              OutcomeCleared,
		"open":                 OutcomeOpen,
		"":                     OutcomeUnknown,
		"closed":               OutcomeUnknown,
		"closed-unmerged":      OutcomeUnknown,
		"merged_after_reworks": OutcomeUnknown,
	}
	for in, want := range cases {
		if got := NormalizeOutcome(in); got != want {
			t.Errorf("NormalizeOutcome(%q) = %q, want %q", in, got, want)
		}
	}
	if got := OutcomeUnknown.String(); got != "unknown" {
		t.Errorf("OutcomeUnknown.String() = %q, want %q", got, "unknown")
	}
	if got := OutcomeClosedUnmerged.String(); got != "closed_unmerged" {
		t.Errorf("OutcomeClosedUnmerged.String() = %q, want %q", got, "closed_unmerged")
	}
}

// A PR a maintainer fixed and merged is not evidence the configuration
// produces mergeable work — and not evidence it produces garbage either.
func TestClassifyMergeDetectsHumanRework(t *testing.T) {
	cases := []struct {
		name       string
		donor      string
		newAuthors []string
		want       OutcomeKind
	}{
		{
			name:  "merged with the donor as sole author",
			donor: "alice",
			want:  OutcomeMerged,
		},
		{
			name:       "a maintainer pushed to it before merging",
			donor:      "alice",
			newAuthors: []string{"maintainer-bob"},
			want:       OutcomeMergedAfterRework,
		},
		{
			name:       "the donor pushing again is not rework",
			donor:      "alice",
			newAuthors: []string{"alice"},
			want:       OutcomeMerged,
		},
		{
			name:       "spelling of the donor login does not manufacture a second author",
			donor:      "Alice",
			newAuthors: []string{" alice "},
			want:       OutcomeMerged,
		},
		{
			name:       "an unattributed commit is not a second author",
			donor:      "alice",
			newAuthors: []string{"", "  "},
			want:       OutcomeMerged,
		},
		{
			name:       "one other author among the donor's own commits is enough",
			donor:      "alice",
			newAuthors: []string{"alice", "maintainer-bob"},
			want:       OutcomeMergedAfterRework,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyMerge(tc.donor, tc.newAuthors); got != tc.want {
				t.Errorf("ClassifyMerge = %q, want %q", got, tc.want)
			}
			if got := ReworkedByOther(tc.donor, tc.newAuthors); got != (tc.want == OutcomeMergedAfterRework) {
				t.Errorf("ReworkedByOther = %v, disagrees with ClassifyMerge = %q", got, tc.want)
			}
		})
	}
}

// The rework classification is what keeps a reworked merge out of BOTH the
// reset path and the streak: record one as a plain merge and a donor who
// needed a human twice looks clean; record one as a closure and a donor whose
// work merged looks suspendable. Assert the two ends together.
func TestReworkedMergeNeitherResetsNorCounts(t *testing.T) {
	reworked := ClassifyMerge("alice", []string{"maintainer-bob"})
	clean := ClassifyMerge("alice", nil)

	// As a reset: a clean merge between two closures clears the streak, a
	// reworked one does not.
	if suspended, _ := SuspendState(ledger(OutcomeClosedUnmerged, clean, OutcomeClosedUnmerged), 0); suspended {
		t.Error("a clean merge between two closures did not break the streak")
	}
	if suspended, _ := SuspendState(ledger(OutcomeClosedUnmerged, reworked, OutcomeClosedUnmerged), 0); !suspended {
		t.Error("a reworked merge between two closures broke the streak")
	}

	// As evidence: on its own, a reworked merge suspends nobody.
	if suspended, streak := SuspendState(ledger(reworked, reworked), 0); suspended || streak != 0 {
		t.Errorf("two reworked merges: suspended = %v, streak = %d, want false/0", suspended, streak)
	}
}

// Candidate.Suspended is the field the matching path fills from this rule.
// Assert the seam: a suspended configuration is refused by Qualifies with the
// suspended reason, and the refusal carries no floor value.
func TestSuspendedConfigurationDoesNotQualify(t *testing.T) {
	tm := tiers(t)
	lane := LanePolicy{Floor: T1, DailyCap: 3}

	c := approved(T1)
	if ok, reason := Qualifies(c, lane, noItem, tm, now); !ok {
		t.Fatalf("the donor did not qualify before any outcomes: %q", reason)
	}

	rows := ledger(OutcomeClosedUnmerged, OutcomeClosedUnmerged)
	c.Suspended = Suspended(rows)
	ok, reason := Qualifies(c, lane, noItem, tm, now)
	if ok {
		t.Error("a suspended configuration qualified")
	}
	if reason != ReasonSuspended {
		t.Errorf("reason = %q, want %q", reason, ReasonSuspended)
	}
	if s := reason.String(); s != "suspended" {
		t.Errorf("reason renders as %q, want %q", s, "suspended")
	}
	// The refusal states its class and never the delta: no floor value, no
	// tier, no streak count anywhere in the reason a surface would render.
	for _, leak := range []string{"T1", "T2", "T3", "2", "floor"} {
		if contains(reason.String(), leak) {
			t.Errorf("the suspended reason leaks %q: %q", leak, reason)
		}
	}

	// After the owner's clear the donor qualifies again, without the ledger
	// losing the rows that suspended them.
	cleared := append(rows, row(OutcomeCleared, 99))
	c.Suspended = Suspended(cleared)
	if ok, reason := Qualifies(c, lane, noItem, tm, now); !ok {
		t.Errorf("a cleared configuration did not qualify: %q", reason)
	}
	if len(cleared) != 3 {
		t.Errorf("the clear dropped ledger rows: %d rows, want 3", len(cleared))
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
