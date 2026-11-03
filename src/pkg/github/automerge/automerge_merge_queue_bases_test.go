package automerge

import (
	"testing"
	"time"
)

// TestMergeQueueBasesSharedAcrossEngines pins that one MergeQueueBases passed
// through Options is what every engine reads and writes: the governor builds a
// one-shot engine per tick for the queued, trusted-author and contributor-lane
// sweeps, and the merge-eligibility classifier reads the same memory.
func TestMergeQueueBasesSharedAcrossEngines(t *testing.T) {
	now := time.Date(2026, 10, 25, 17, 25, 0, 0, time.UTC)
	shared := NewMergeQueueBases()
	a := New(nil, Options{MergeQueueBases: shared, Now: func() time.Time { return now }})
	b := New(nil, Options{MergeQueueBases: shared, Now: func() time.Time { return now }})
	private := New(nil, Options{Now: func() time.Time { return now }})

	a.noteMergeQueueBase("projectbluefin", "bluefin-bling", "main")
	if !b.mergeQueueBaseKnown("projectbluefin", "bluefin-bling", "main") {
		t.Error("an engine sharing the memory does not see the base another engine noted")
	}
	if !shared.Required("ProjectBluefin/Bluefin-Bling", "main", now) {
		t.Error("Required is not case-insensitive on the repo")
	}
	if shared.Required("projectbluefin/bluefin-bling", "testing", now) || shared.Required("projectbluefin/bluefin-bling", "", now) {
		t.Error("another base, or an unknown one, reads as merge-queue-only")
	}
	if private.mergeQueueBaseKnown("projectbluefin", "bluefin-bling", "main") {
		t.Error("an engine built without Options.MergeQueueBases sees the shared memory")
	}
	if shared.Required("projectbluefin/bluefin-bling", "main", now.Add(mergeQueueBaseRecheck)) {
		t.Error("entry did not expire after mergeQueueBaseRecheck")
	}
	var nilMem *MergeQueueBases
	nilMem.Note("o/r", "main", now)
	if nilMem.Required("o/r", "main", now) {
		t.Error("a nil memory reports a base")
	}
}

// TestMergeQueueBasesHoldBaseRefresh pins the refresh hold the current-base
// gate reads (projectbluefin/finpilot#599): it outlasts the 30-minute merge
// recheck by mergeQueueBaseRefreshHold after the last refusal, each hold
// keeps Required true for another mergeQueueBaseRecheck, a hold never extends
// the refusal itself, and a new refusal re-arms it.
func TestMergeQueueBasesHoldBaseRefresh(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 9, 31, 0, 0, time.UTC)
	m := NewMergeQueueBases()
	if m.HoldBaseRefresh("projectbluefin/finpilot", "main", t0) {
		t.Fatal("a base that never refused holds the refresh")
	}
	m.Note("projectbluefin/finpilot", "main", t0)

	at := t0.Add(101 * time.Minute)
	if m.Required("projectbluefin/finpilot", "main", at) {
		t.Fatal("Required past mergeQueueBaseRecheck with no hold")
	}
	if !m.HoldBaseRefresh("ProjectBluefin/FinPilot", "main", at) {
		t.Fatal("refresh not held 1h41m after the refusal")
	}
	if !m.Required("projectbluefin/finpilot", "main", at.Add(mergeQueueBaseRecheck-time.Second)) {
		t.Fatal("a hold did not keep Required true for mergeQueueBaseRecheck")
	}
	if m.Required("projectbluefin/finpilot", "main", at.Add(mergeQueueBaseRecheck)) {
		t.Fatal("a hold kept Required true past mergeQueueBaseRecheck")
	}
	if m.HoldBaseRefresh("projectbluefin/finpilot", "testing", at) || m.HoldBaseRefresh("projectbluefin/finpilot", "", at) {
		t.Fatal("another base, or an unknown one, holds the refresh")
	}

	// Holds alone never outlast the refusal.
	for at = at.Add(time.Hour); at.Before(t0.Add(mergeQueueBaseRefreshHold)); at = at.Add(time.Hour) {
		if !m.HoldBaseRefresh("projectbluefin/finpilot", "main", at) {
			t.Fatalf("refresh not held %s after the refusal", at.Sub(t0))
		}
	}
	if m.HoldBaseRefresh("projectbluefin/finpilot", "main", t0.Add(mergeQueueBaseRefreshHold)) {
		t.Fatal("holds extended the refresh hold past mergeQueueBaseRefreshHold")
	}
	if m.Required("projectbluefin/finpilot", "main", t0.Add(mergeQueueBaseRefreshHold)) {
		t.Fatal("expired entry still Required")
	}

	// A refusal that keeps coming keeps the hold.
	t1 := t0.Add(30 * time.Hour)
	m.Note("projectbluefin/finpilot", "main", t1)
	if !m.HoldBaseRefresh("projectbluefin/finpilot", "main", t1.Add(mergeQueueBaseRefreshHold-time.Minute)) {
		t.Fatal("a new refusal did not re-arm the refresh hold")
	}
	var nilMem *MergeQueueBases
	if nilMem.HoldBaseRefresh("o/r", "main", t0) {
		t.Error("a nil memory holds the refresh")
	}
}
