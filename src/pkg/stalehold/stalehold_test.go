package stalehold

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testRepo   = "acme/widget"
	testNumber = 7
	headA      = "aaaaaaa"
	headB      = "bbbbbbb"
	testAfter  = 2 * time.Hour
)

var testStart = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

func testOpts() Options {
	return Options{After: testAfter, HoldLabels: []string{"hold", "do-not-merge/hold"}, ReleaseLabel: "hold/until-green"}
}

func obs(head, ci string, labels ...string) Observation {
	return Observation{Repo: testRepo, Number: testNumber, HeadSHA: head, Labels: labels, CI: ci}
}

func kinds(actions []Action) []ActionKind {
	out := make([]ActionKind, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Kind)
	}
	return out
}

func TestHeldGreenPRIsRaisedOnceAfterThreshold(t *testing.T) {
	s := Load("")
	o := obs(headA, CIGreen, "size/L", "do-not-merge/hold")
	if got := s.Sweep([]Observation{o}, testStart, testOpts()); len(got) != 0 {
		t.Fatalf("first sighting raised %v", kinds(got))
	}
	if got := s.Sweep([]Observation{o}, testStart.Add(testAfter-time.Minute), testOpts()); len(got) != 0 {
		t.Fatalf("raised before the threshold: %v", kinds(got))
	}
	got := s.Sweep([]Observation{o}, testStart.Add(testAfter), testOpts())
	if len(got) != 1 || got[0].Kind != ActionNotify {
		t.Fatalf("want one notify at the threshold, got %v", kinds(got))
	}
	a := got[0]
	if a.Repo != testRepo || a.Number != testNumber || a.HeadSHA != headA || a.GreenFor != testAfter {
		t.Fatalf("unexpected action %+v", a)
	}
	if len(a.HoldLabels) != 1 || a.HoldLabels[0] != "do-not-merge/hold" {
		t.Fatalf("hold labels = %v", a.HoldLabels)
	}
	// Undelivered: proposed again on the next pass.
	if got := s.Sweep([]Observation{o}, testStart.Add(testAfter+time.Minute), testOpts()); len(got) != 1 {
		t.Fatalf("an undelivered notify must be retried, got %v", kinds(got))
	}
	s.MarkNotified(testRepo, testNumber, headA)
	if got := s.Sweep([]Observation{o}, testStart.Add(10*testAfter), testOpts()); len(got) != 0 {
		t.Fatalf("notified head raised again: %v", kinds(got))
	}
}

func TestNewHeadRearmsAndRedPausesWithoutRearming(t *testing.T) {
	s := Load("")
	opts := testOpts()
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart, opts)
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart.Add(testAfter), opts)
	s.MarkNotified(testRepo, testNumber, headA)

	// A red re-run on the same head stops the clock but keeps the marker.
	s.Sweep([]Observation{obs(headA, CIRed, "hold")}, testStart.Add(3*testAfter), opts)
	if e, _ := s.Entry(testRepo, testNumber); !e.GreenSince.IsZero() || e.NotifiedSHA != headA {
		t.Fatalf("red pass: entry %+v", e)
	}
	if got := s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart.Add(10*testAfter), opts); len(got) != 0 {
		t.Fatalf("same head re-raised after a red flip: %v", kinds(got))
	}

	// A new head restarts the clock and re-arms the notification.
	at := testStart.Add(20 * testAfter)
	s.Sweep([]Observation{obs(headB, CIGreen, "hold")}, at, opts)
	if e, _ := s.Entry(testRepo, testNumber); e.HeadSHA != headB || !e.GreenSince.Equal(at) || e.NotifiedSHA != "" {
		t.Fatalf("new head: entry %+v", e)
	}
	got := s.Sweep([]Observation{obs(headB, CIGreen, "hold")}, at.Add(testAfter), opts)
	if len(got) != 1 || got[0].HeadSHA != headB {
		t.Fatalf("new head not raised: %+v", got)
	}
	// MarkNotified for a head the ledger no longer tracks is ignored.
	s.MarkNotified(testRepo, testNumber, headA)
	if e, _ := s.Entry(testRepo, testNumber); e.NotifiedSHA != "" {
		t.Fatalf("stale MarkNotified applied: %+v", e)
	}
}

func TestUnknownCIChangesNothing(t *testing.T) {
	s := Load("")
	opts := testOpts()
	s.Sweep([]Observation{obs(headA, CIUnknown, "hold")}, testStart, opts)
	if e, ok := s.Entry(testRepo, testNumber); !ok || !e.GreenSince.IsZero() {
		t.Fatalf("unknown CI started the clock: %+v", e)
	}
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart, opts)
	s.Sweep([]Observation{obs(headA, CIUnknown, "hold")}, testStart.Add(time.Hour), opts)
	if e, _ := s.Entry(testRepo, testNumber); !e.GreenSince.Equal(testStart) {
		t.Fatalf("unknown CI reset the clock: %+v", e)
	}
	got := s.Sweep([]Observation{obs(headA, CIUnknown, "hold")}, testStart.Add(testAfter), opts)
	if len(got) != 0 {
		t.Fatalf("unknown CI raised: %v", kinds(got))
	}
}

func TestReleasedHoldForgetsThePR(t *testing.T) {
	s := Load("")
	opts := testOpts()
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart, opts)
	s.Sweep([]Observation{obs(headA, CIGreen, "lgtm")}, testStart.Add(time.Minute), opts)
	if _, ok := s.Entry(testRepo, testNumber); ok {
		t.Fatal("entry kept after the hold was released")
	}
	// Re-held later: a fresh incident with a fresh clock.
	at := testStart.Add(5 * testAfter)
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, at, opts)
	if e, _ := s.Entry(testRepo, testNumber); !e.GreenSince.Equal(at) {
		t.Fatalf("re-hold: entry %+v", e)
	}
}

func TestOnlyConfiguredHoldLabelsCount(t *testing.T) {
	s := Load("")
	opts := testOpts()
	for _, labels := range [][]string{
		{"on-hold"}, {"needs-human"}, {"do-not-merge/holder"}, {"hive-pause/h1"}, {"  "},
	} {
		s.Sweep([]Observation{obs(headA, CIGreen, labels...)}, testStart, opts)
		if got := s.Sweep([]Observation{obs(headA, CIGreen, labels...)}, testStart.Add(testAfter), opts); len(got) != 0 {
			t.Fatalf("labels %v raised %v", labels, kinds(got))
		}
	}
	s.Sweep([]Observation{obs(headA, CIGreen, "HOLD")}, testStart, opts)
	if got := s.Sweep([]Observation{obs(headA, CIGreen, "HOLD")}, testStart.Add(testAfter), opts); len(got) != 1 || got[0].HoldLabels[0] != "HOLD" {
		t.Fatalf("case-insensitive hold not raised: %+v", got)
	}
}

func TestDisabledThresholdNeverNotifies(t *testing.T) {
	s := Load("")
	opts := testOpts()
	opts.After = 0
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart, opts)
	if got := s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart.Add(100*testAfter), opts); len(got) != 0 {
		t.Fatalf("disabled threshold raised %v", kinds(got))
	}
}

func TestReleaseOnlyWithOptInLabelAndGreenCI(t *testing.T) {
	s := Load("")
	opts := testOpts()
	// A plain hold is never released, however long it has been green.
	s.Sweep([]Observation{obs(headA, CIGreen, "do-not-merge/hold")}, testStart, opts)
	for _, a := range s.Sweep([]Observation{obs(headA, CIGreen, "do-not-merge/hold")}, testStart.Add(100*testAfter), opts) {
		if a.Kind == ActionRelease {
			t.Fatalf("plain hold released: %+v", a)
		}
	}
	// Opt-in but not green yet: tracked as a hold, never released.
	for _, ci := range []string{CIRed, CIUnknown} {
		for _, a := range s.Sweep([]Observation{obs(headA, ci, "do-not-merge/hold", "hold/until-green")}, testStart, opts) {
			if a.Kind == ActionRelease {
				t.Fatalf("released on CI %q", ci)
			}
		}
	}
	got := s.Sweep([]Observation{obs(headA, CIGreen, "do-not-merge/hold", "Hold/Until-Green", "hold")}, testStart, opts)
	if len(got) != 1 || got[0].Kind != ActionRelease {
		t.Fatalf("want one release, got %v", kinds(got))
	}
	a := got[0]
	if a.ReleaseLabel != "Hold/Until-Green" || len(a.HoldLabels) != 2 || a.HoldLabels[0] != "do-not-merge/hold" || a.HoldLabels[1] != "hold" {
		t.Fatalf("release action %+v", a)
	}
	if _, ok := s.Entry(testRepo, testNumber); ok {
		t.Fatal("ledger kept an entry for a released PR")
	}
	// No head SHA: nothing to vouch for, no release.
	if got := s.Sweep([]Observation{obs("", CIGreen, "hold", "hold/until-green")}, testStart, opts); len(got) != 0 {
		t.Fatalf("released without a head: %v", kinds(got))
	}
	// Releases disabled.
	opts.ReleaseLabel = ""
	for _, a := range s.Sweep([]Observation{obs(headA, CIGreen, "hold", "hold/until-green")}, testStart, opts) {
		if a.Kind == ActionRelease {
			t.Fatal("released with releases disabled")
		}
	}
}

func TestInvalidObservationsAreIgnored(t *testing.T) {
	s := Load("")
	got := s.Sweep([]Observation{
		{Repo: "", Number: testNumber, HeadSHA: headA, Labels: []string{"hold"}, CI: CIGreen},
		{Repo: testRepo, Number: 0, HeadSHA: headA, Labels: []string{"hold"}, CI: CIGreen},
	}, testStart, testOpts())
	if len(got) != 0 || len(s.entries) != 0 {
		t.Fatalf("invalid observations tracked: %v %v", kinds(got), s.entries)
	}
}

func TestLedgerPersistsAndPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics", "stale-holds.json")
	s := Load(path)
	opts := testOpts()
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart, opts)
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart.Add(testAfter), opts)
	s.MarkNotified(testRepo, testNumber, headA)

	// A restart keeps both the clock and the notified marker.
	reloaded := Load(path)
	e, ok := reloaded.Entry("ACME/widget", testNumber)
	if !ok || !e.GreenSince.Equal(testStart) || e.NotifiedSHA != headA {
		t.Fatalf("reloaded entry %+v ok=%v", e, ok)
	}
	if got := reloaded.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart.Add(3*testAfter), opts); len(got) != 0 {
		t.Fatalf("restart re-raised a notified head: %v", kinds(got))
	}

	// Missing from a pass (a failed enumeration) keeps the entry; long
	// unseen prunes it.
	last := testStart.Add(3 * testAfter)
	reloaded.Sweep(nil, last.Add(PruneAfter), opts)
	if _, ok := reloaded.Entry(testRepo, testNumber); !ok {
		t.Fatal("pruned before PruneAfter")
	}
	reloaded.Sweep(nil, last.Add(PruneAfter+time.Minute), opts)
	if _, ok := reloaded.Entry(testRepo, testNumber); ok {
		t.Fatal("not pruned after PruneAfter")
	}
}

func TestLoadToleratesMissingAndCorruptLedgers(t *testing.T) {
	dir := t.TempDir()
	if s := Load(filepath.Join(dir, "missing.json")); s == nil || len(s.entries) != 0 {
		t.Fatal("missing ledger not empty")
	}
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), ledgerFileMode); err != nil {
		t.Fatal(err)
	}
	if s := Load(corrupt); len(s.entries) != 0 {
		t.Fatal("corrupt ledger not empty")
	}
	withNull := filepath.Join(dir, "null.json")
	if err := os.WriteFile(withNull, []byte(`{"acme/widget#7": null}`), ledgerFileMode); err != nil {
		t.Fatal(err)
	}
	s := Load(withNull)
	if _, ok := s.Entry(testRepo, testNumber); ok {
		t.Fatal("null entry loaded")
	}
	// A nil entry must not break a sweep.
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart, testOpts())
}

func TestSaveFailureIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, ledgerFileMode); err != nil {
		t.Fatal(err)
	}
	// The ledger's parent is a regular file: MkdirAll fails, the sweep
	// still works in memory.
	s := Load(filepath.Join(blocker, "stale-holds.json"))
	s.Sweep([]Observation{obs(headA, CIGreen, "hold")}, testStart, testOpts())
	if _, ok := s.Entry(testRepo, testNumber); !ok {
		t.Fatal("in-memory entry lost after a failed save")
	}
}
