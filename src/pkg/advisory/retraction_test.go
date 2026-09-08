package advisory

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// Regression coverage for hivecommons/hive#2364.
//
// The 2026-09-05 09:32 EDT digest on that tracker rendered 10 findings out of
// 292, and one of its five CRITICAL slots was held by a finding the reporting
// agent had already withdrawn in the finding's own detail. The maintainer
// working the tracker wrote it up as "refuted by its own detail line and
// should be closed rather than fixed -- its bead is still open at critical,
// which is why it keeps consuming a slot".
//
// #5945 had already taught applyTopN to demote on the three
// nobody-re-checked-this signals. A retraction is a different thing: the one
// party who ever looked went back, looked again, and said it does not stand.
// That was the only such signal the ranking ignored.

// retractedDetail is the live finding's detail, verbatim from the digest.
const retractedDetail = "REVISED: Per-hive identity binding was MOVED from " +
	"heartbeatBearerOK to verifyHeartbeatBearer in hub_keys.go, not removed. " +
	"heartbeatBearerOK is now a thin prefix-check, and identity verification " +
	"happens via verifyHeartbeatBearer which has tests in " +
	"heartbeat_identity_test.go. The change is refactoring, not a security " +
	"regression. Downgrading priority."

// TestFindingRetracted_TwoSignalRule is the whole safety argument. A revision
// marker alone must NOT retract: an agent revising a finding UPWARD writes the
// same lead-in, and demoting that would bury the most urgent thing in the
// report under a caption telling the reader to disregard it.
func TestFindingRetracted_TwoSignalRule(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		want   bool
	}{
		{"the live #2364 finding", retractedDetail, true},
		{
			"a revision that RAISES severity is not a retraction",
			"REVISED: found a second call site with the same gap; raising this to critical.",
			false,
		},
		{
			"a marker with no withdrawal cue is not a retraction",
			"REVISED: added the exact line numbers and the failing test name.",
			false,
		},
		{
			"a withdrawal cue with no marker is not a retraction",
			"The scheduler downgrades the request when the queue is full.",
			false,
		},
		{
			"the word revised mid-sentence is reporting, not retracting",
			"The workflow was revised in #4102 and the guard was never restored; downgrade risk.",
			false,
		},
		{"an empty detail retracts nothing", "", false},
		{"markdown decoration is absorbed", "**CORRECTION:** this was a false positive.", true},
		{"quote decoration is absorbed", "> RETRACTED - no longer applies after #5900.", true},
		{"leading whitespace is tolerated", "   revised: withdrawing this finding.", true},
		{"case does not matter", "ReViSeD: this is incorrect, see below.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findingRetracted(Finding{Detail: tc.detail}); got != tc.want {
				t.Errorf("findingRetracted(%q) = %v, want %v", tc.detail, got, tc.want)
			}
		})
	}
}

// The retraction lives in the notes, never the title -- which is exactly why
// the bead's priority never moved and the finding kept its critical slot.
func TestFindingRetracted_ReadsTheDetailNotTheTitle(t *testing.T) {
	f := Finding{Title: "REVISED: downgrading this", Detail: "the guard is still missing"}
	if findingRetracted(f) {
		t.Error("a retraction-shaped TITLE retracted the finding; only the detail may")
	}
}

// retractedFinding builds the live #2364 finding at a given severity.
func retractedFinding(sev, title string) Finding {
	return Finding{Agent: "quality", Type: "regression-risk", Severity: sev, Title: title, Detail: retractedDetail}
}

// TestApplyTopN_RetractedLosesItsSlotAcrossBands is the reported defect: a
// withdrawn CRITICAL was holding a top-N slot while live findings of LOWER
// severity went unshown. This is the one demotion that crosses severity bands.
func TestApplyTopN_RetractedLosesItsSlotAcrossBands(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			retractedFinding("critical", "heartbeatBearerOK per-hive binding removed without covering test"),
			{Agent: "quality", Severity: "low", Title: "a live low finding nobody withdrew"},
		},
	}

	capped, overflow := applyTopN(byAgent, 1, nil)
	if overflow != 1 {
		t.Fatalf("overflow = %d, want 1", overflow)
	}
	kept := keptTitles(capped)
	if len(kept) != 1 {
		t.Fatalf("kept %d findings under a cap of 1: %v", len(kept), kept)
	}
	if kept[0] != "a live low finding nobody withdrew" {
		t.Errorf("the single slot went to %q; a withdrawn critical must not outrank a live low", kept[0])
	}
}

// The discriminating counterpart. Without it the suite would pass on an
// implementation that demoted every critical, or every regression-risk finding.
func TestApplyTopN_UnwithdrawnCriticalStillWins(t *testing.T) {
	live := retractedFinding("critical", "heartbeatBearerOK per-hive binding removed without covering test")
	// Same finding, same severity, same agent -- only the withdrawal is gone.
	live.Detail = "REVISED: added the exact line numbers and the failing test name."
	byAgent := map[string][]Finding{
		"quality": {
			live,
			{Agent: "quality", Severity: "low", Title: "a live low finding nobody withdrew"},
		},
	}

	capped, _ := applyTopN(byAgent, 1, nil)
	kept := keptTitles(capped)
	if len(kept) != 1 || !strings.Contains(kept[0], "heartbeatBearerOK") {
		t.Errorf("the slot went to %v; a critical nobody withdrew must still win it", kept)
	}
}

// A withdrawn finding is DEMOTED, never dropped: the bead is still open and
// only a maintainer closes it, so rendering 9 findings under a cap of 10 would
// hide the very entry that wants closing.
func TestApplyTopN_RetractedBackfillsRatherThanDisappearing(t *testing.T) {
	byAgent := map[string][]Finding{
		"quality": {
			retractedFinding("critical", "heartbeatBearerOK per-hive binding removed without covering test"),
			{Agent: "quality", Severity: "low", Title: "a live low finding nobody withdrew"},
			{Agent: "quality", Severity: "low", Title: "another live low finding"},
		},
	}

	// Cap of 3 with 3 findings returns early, so use a cap that still ranks.
	byAgent["quality"] = append(byAgent["quality"], Finding{Agent: "quality", Severity: "low", Title: "a third live low finding"})
	capped, _ := applyTopN(byAgent, 3, nil)
	kept := keptTitles(capped)
	if len(kept) != 3 {
		t.Fatalf("kept %d under a cap of 3: %v", len(kept), kept)
	}
	var sawRetracted bool
	for _, k := range kept {
		if strings.Contains(k, "heartbeatBearerOK") {
			sawRetracted = true
		}
	}
	if sawRetracted {
		t.Error("the withdrawn finding took a slot three live findings could fill")
	}

	// Now with only two live findings, the third slot has no live claimant and
	// the withdrawn one must fill it rather than leaving the digest short.
	byAgent2 := map[string][]Finding{
		"quality": {
			retractedFinding("critical", "heartbeatBearerOK per-hive binding removed without covering test"),
			{Agent: "quality", Severity: "low", Title: "a live low finding nobody withdrew"},
			{Agent: "quality", Severity: "low", Title: "another live low finding"},
			{Agent: "quality", Severity: "low", Title: "a fourth live low finding"},
		},
	}
	capped2, _ := applyTopN(byAgent2, 4, nil)
	if got := len(keptTitles(capped2)); got != 4 {
		t.Errorf("kept %d under a cap of 4; a withdrawn finding must backfill an unclaimed slot", got)
	}
}

// The caption is the other half. A demoted finding that still renders with no
// explanation reads as a live problem sitting oddly low in the report; the
// point is to tell a maintainer this bead wants CLOSING rather than fixing.
func TestFormatDigestMarkdown_CaptionsWithdrawnFindings(t *testing.T) {
	d := &Digest{
		ByAgent: map[string][]Finding{
			"quality": {retractedFinding("critical", "heartbeatBearerOK per-hive binding removed without covering test")},
		},
		TotalCount: 1,
	}
	out := FormatDigestMarkdown(d, DigestOptions{})

	if !strings.Contains(out, "withdrawn by the reporting agent") {
		t.Errorf("a withdrawn finding rendered with no caption; got:\n%s", out)
	}
	if !strings.Contains(out, "closing rather than fixing") {
		t.Errorf("the caption does not say what the maintainer should DO; got:\n%s", out)
	}
	// The detail carrying the withdrawal has to be right there under it, or the
	// caption is an assertion the reader cannot check.
	if !strings.Contains(out, "Downgrading priority") {
		t.Errorf("the withdrawal text itself is not rendered; got:\n%s", out)
	}
}

// A finding nobody withdrew must not pick up the caption -- that would tell a
// maintainer to close a live critical.
func TestFormatDigestMarkdown_LiveFindingKeepsNoWithdrawalCaption(t *testing.T) {
	f := retractedFinding("critical", "heartbeatBearerOK per-hive binding removed without covering test")
	f.Detail = "REVISED: added the exact line numbers and the failing test name."
	d := &Digest{ByAgent: map[string][]Finding{"quality": {f}}, TotalCount: 1}

	if out := FormatDigestMarkdown(d, DigestOptions{}); strings.Contains(out, "withdrawn by the reporting agent") {
		t.Errorf("a live finding was captioned as withdrawn; got:\n%s", out)
	}
}

// End to end from the bead store, because the withdrawal lives in the bead's
// NOTES and the severity comes from its PRIORITY. That split is the whole
// mechanism of the bug: the agent edited the notes, beadPriorityToSeverity
// kept reading the priority, and the finding went on rendering at critical.
func TestBuildDigestFromBeads_WithdrawnFindingYieldsItsSlot(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	withdrawn, err := store.Create("heartbeatBearerOK per-hive binding removed without covering test",
		beads.TypeAdvisory, severityToPriority("critical"), "quality", "")
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	if err := store.Update(withdrawn.ID, func(b *beads.Bead) { b.Notes = retractedDetail }); err != nil {
		t.Fatalf("setting notes: %v", err)
	}
	if _, err := store.Create("gateway health faults are dropped on the merge path",
		beads.TypeAdvisory, severityToPriority("medium"), "quality", ""); err != nil {
		t.Fatalf("creating bead: %v", err)
	}

	d := BuildDigestFromBeads(map[string]*beads.Store{"quality": store}, "advisory", DigestOptions{MaxFindings: 1})

	all := digestFindings(d)
	if len(all) != 1 {
		t.Fatalf("%d findings rendered under a cap of 1", len(all))
	}
	if strings.Contains(all[0].Title, "heartbeatBearerOK") {
		t.Errorf("the withdrawn critical took the only slot; a live medium was available")
	}
	// The bead is still OPEN -- the pipeline demotes, it does not close.
	if len(d.RecentlyResolved) != 0 {
		t.Errorf("a withdrawn finding was reported as resolved; only a maintainer closes the bead")
	}
}

// "revision <sha>" is this package's PROVENANCE idiom (provenanceRefPattern),
// not a withdrawal. Reading it as one would caption a #5130 stale-provenance
// finding as withdrawn and demote it across every severity band. Pinned here
// because the two vocabularies genuinely overlap and the next person adding a
// marker word will not know that.
func TestFindingRetracted_ProvenanceVocabularyIsNotAWithdrawal(t *testing.T) {
	f := Finding{Detail: "revision c9546a8a24b3dded3146e3ab7a93dd99edc56fa3 - downgrading the linked issue"}
	if findingRetracted(f) {
		t.Error("a provenance line was read as a retraction; 'revision <sha>' states where evidence came from")
	}
}
