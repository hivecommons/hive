package github

import (
	"strings"
	"testing"
)

// A dedupe checklist shaped like hivecommons/hive#15734: parked at 045 with
// "Decision required: yes", no triage/advisory label, its items led by the
// reference they are about.
func advisoryItemsBody() string {
	return "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: yes\n\n## Near-duplicates: close one\n\n" +
		"| Keep | Merge into it |\n|---|---|\n| PR #9 | PR #1 |\n\n" +
		"- [ ] #1 — close as superseded by #9\n" +
		"- [ ] PR #2 — add `Fixes #3` to its body\n"
}

func newAdvisoryItemsFixture(t *testing.T) *dedupeFixture {
	f := newDedupeFixture(t, "yes")
	f.body = advisoryItemsBody()
	f.labels = []string{"agent/dedupe", issueNeedsDecisionLabel}
	f.refStates = map[int]string{1: "closed", 2: "closed", 9: "open"}
	f.refMerged = map[int]bool{2: true}
	return f
}

func TestAdvisoryChecklistClosesOnceEveryItemResolved(t *testing.T) {
	f := newAdvisoryItemsFixture(t)
	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeClosed, advisoryItemsResolvedReason)
	if len(f.edits) != 1 || !strings.Contains(f.edits[0], "- [x] #1 — close") || !strings.Contains(f.edits[0], "- [x] PR #2 — add") {
		t.Fatalf("edits = %q, want both items ticked in one edit", f.edits)
	}
	want := dedupeAdvisoryCloseMarker + "\nAll 2 items resolved (#1 closed, #2 merged) — closing this advisory."
	if len(f.posted) != 1 || f.posted[0] != want {
		t.Fatalf("posted = %q, want %q", f.posted, want)
	}
	if !f.patched {
		t.Fatal("checklist was not closed")
	}
	if f.refGets != 2 {
		t.Fatalf("ref reads = %d, want 2 (the table's #9 is not an item)", f.refGets)
	}
}

func TestAdvisoryChecklistTicksResolvedItemsAndWaitsForTheRest(t *testing.T) {
	f := newAdvisoryItemsFixture(t)
	f.refStates[2] = "open"
	f.refMerged = nil
	assertDedupeResult(t, f.run(CloseOnMergeOptions{}), CloseOnMergeNoop, advisoryItemsTickedReason)
	if len(f.edits) != 1 || !strings.Contains(f.edits[0], "- [x] #1 — close") || !strings.Contains(f.edits[0], "- [ ] PR #2") {
		t.Fatalf("edits = %q, want only #1 ticked", f.edits)
	}
	if f.patched || len(f.posted) != 0 {
		t.Fatalf("patched=%v posted=%q, want the checklist left open", f.patched, f.posted)
	}

	// Nothing new: no edit, no comment.
	assertDedupeResult(t, f.run(CloseOnMergeOptions{}), CloseOnMergeNoop, advisoryItemsOpenReason)
	if len(f.edits) != 1 || f.patched {
		t.Fatalf("edits=%d patched=%v, want no write while #2 is open", len(f.edits), f.patched)
	}

	// #2 closes: the last item is ticked and the checklist closes; the
	// earlier-ticked #1 is read again for the comment.
	f.refStates[2] = "closed"
	assertDedupeResult(t, f.run(CloseOnMergeOptions{}), CloseOnMergeClosed, advisoryItemsResolvedReason)
	if !strings.Contains(f.body, "- [x] PR #2") || len(f.posted) != 1 || !strings.Contains(f.posted[0], "All 2 items resolved (#1 closed, #2 closed)") {
		t.Fatalf("body=%q posted=%q", f.body, f.posted)
	}
}

func TestAdvisoryChecklistItemWithoutLeadingRefWaitsForAPerson(t *testing.T) {
	f := newAdvisoryItemsFixture(t)
	f.body += "- [ ] Add `Fixes #3` to PR #2's body\n"
	assertDedupeResult(t, f.run(CloseOnMergeOptions{}), CloseOnMergeNoop, advisoryItemsTickedReason)
	if f.patched || len(f.posted) != 0 {
		t.Fatalf("patched=%v posted=%q, want the checklist left open", f.patched, f.posted)
	}
	// A person ticks it; the next sweep closes the checklist.
	f.body = strings.Replace(f.body, "- [ ] Add", "- [x] Add", 1)
	assertDedupeResult(t, f.run(CloseOnMergeOptions{}), CloseOnMergeClosed, advisoryItemsResolvedReason)
	if len(f.posted) != 1 || !strings.Contains(f.posted[0], "All 3 items resolved (#1 closed, #2 merged, item 3 resolved)") {
		t.Fatalf("posted = %q", f.posted)
	}
}

func TestAdvisoryChecklistItemPassLeavesAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts CloseOnMergeOptions
	}{
		{"dedupe.advisory_autoclose off", CloseOnMergeOptions{DisableAdvisoryAutoclose: true}},
		{"not authored by this hive", CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "other-hive[bot]"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdvisoryItemsFixture(t)
			if results := f.run(tc.opts); len(results) != 0 {
				t.Fatalf("results = %+v, want none", results)
			}
			if len(f.edits) != 0 || f.patched || len(f.posted) != 0 || f.refGets != 0 {
				t.Fatalf("edits=%q patched=%v posted=%q refGets=%d, want untouched", f.edits, f.patched, f.posted, f.refGets)
			}
		})
	}
	t.Run("authored by this hive", func(t *testing.T) {
		f := newAdvisoryItemsFixture(t)
		assertDedupeResult(t, f.run(CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}}), CloseOnMergeClosed, advisoryItemsResolvedReason)
	})
}

// A triage/advisory checklist whose items are still open keeps the label
// path's own rules (refs, expiry) after the item pass ticked what it could.
func TestAdvisoryChecklistLabelledFallsThroughWhileItemsOpen(t *testing.T) {
	f := newAdvisoryItemsFixture(t)
	f.body = strings.Replace(advisoryItemsBody(), "Decision required: yes", "Decision required: no", 1)
	f.labels = []string{dedupeAdvisoryLabelName}
	f.refStates[2] = "open"
	assertDedupeResult(t, f.run(CloseOnMergeOptions{}), CloseOnMergeNoop, "dedupe_advisory_refs_open")
	if len(f.edits) != 1 || !strings.Contains(f.edits[0], "- [x] #1") {
		t.Fatalf("edits = %q, want #1 ticked", f.edits)
	}
}

func TestAdvisoryChecklistProgress(t *testing.T) {
	for _, tc := range []struct {
		body            string
		items, resolved int
	}{
		{advisoryItemsBody(), 2, 0},
		{"- [x] #1 done\n- [X] PR #2 done\n- [ ] #3 open\n* [ ] o/x#4 elsewhere\n", 4, 2},
		{"no task list; #1 and #2 in prose\n", 0, 0},
	} {
		items, resolved := AdvisoryChecklistProgress(tc.body)
		if items != tc.items || resolved != tc.resolved {
			t.Errorf("AdvisoryChecklistProgress(%q) = %d/%d, want %d/%d", tc.body, resolved, items, tc.resolved, tc.items)
		}
	}
}

func TestAdvisoryChecklistItemsRefs(t *testing.T) {
	items := advisoryChecklistItems("- [ ] #1 a\n- [ ] PR #2 b\n- [ ] issue o/x#3 c\n- [ ] Add Fixes #4\n", "o/r")
	want := []ClaimedRef{{Repo: "o/r", Issue: 1}, {Repo: "o/r", Issue: 2}, {Repo: "o/x", Issue: 3}, {}}
	if len(items) != len(want) {
		t.Fatalf("items = %+v", items)
	}
	for i, it := range items {
		if it.Ref != want[i] {
			t.Errorf("item %d ref = %+v, want %+v", i, it.Ref, want[i])
		}
	}
}
