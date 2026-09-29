package prfollowup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/turn"
)

func recordPR(t *testing.T, dir string, number int, at time.Time) {
	t.Helper()
	if err := RecordWithNote(context.Background(), dir, testAgent, testRepo, number, "", "s1", "Why: pr", at); err != nil {
		t.Fatal(err)
	}
}

func pointerExists(dir string, number int) bool {
	_, err := turn.FileStore{Dir: dir}.Load(context.Background(), PointerID(testRepo, number))
	return err == nil
}

// Pointers (and the notes they carry) are deleted when their PR merges or
// closes and when retention elapses; an open PR, an unknown state, a failed
// lookup and anything past the lookup budget keep theirs.
func TestSweep_MergedClosedExpiredAndKept(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordPR(t, dir, 1, now.Add(-10*time.Minute)) // merged
	recordPR(t, dir, 2, now.Add(-9*time.Minute))  // closed
	recordPR(t, dir, 3, now.Add(-8*time.Minute))  // open per lookup
	recordPR(t, dir, 4, now.Add(-7*time.Minute))  // lookup fails
	recordPR(t, dir, 5, now.Add(-6*time.Minute))  // open per this tick's enumeration
	recordPR(t, dir, 6, now.Add(-5*time.Minute))  // past the lookup budget
	recordPR(t, dir, 7, now.Add(-72*time.Hour))   // retention elapsed
	// Not pointers: the counter file and a corrupt pointer are never touched.
	if err := addStats(dir, Stats{Resumed: 1}, now); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, pointerFilePrefix+"corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	var looked []int
	var audited []string
	res := Sweep(context.Background(), SweepOptions{
		Dir:        dir,
		Open:       map[string]bool{ThreadsKey(testRepo, 5): true},
		Retention:  48 * time.Hour,
		MaxLookups: 4,
		State: func(_ context.Context, repo string, number int) (string, error) {
			looked = append(looked, number)
			switch number {
			case 1:
				return PRStateMerged, nil
			case 2:
				return PRStateClosed, nil
			case 3:
				return PRStateOpen, nil
			}
			return "", errors.New("api 502")
		},
		Audit: func(action, agent string, fields map[string]any) {
			if action == AuditActionPruned && agent == testAgent {
				reason, _ := fields["reason"].(string)
				audited = append(audited, reason)
			}
		},
	}, now)

	if res.Merged != 1 || res.Closed != 1 || res.Expired != 1 || res.Kept != 4 {
		t.Fatalf("sweep = %+v, want 1 merged, 1 closed, 1 expired, 4 kept", res)
	}
	for n, want := range map[int]bool{1: false, 2: false, 3: true, 4: true, 5: true, 6: true, 7: false} {
		if pointerExists(dir, n) != want {
			t.Errorf("pointer #%d exists = %v, want %v", n, !want, want)
		}
	}
	// Oldest first, #5 never looked up (known open), #7 expired without a
	// lookup, #6 beyond the budget of 4.
	if len(looked) != 4 || looked[0] != 1 || looked[3] != 4 {
		t.Fatalf("lookups = %v, want [1 2 3 4]", looked)
	}
	if _, err := os.Stat(corrupt); err != nil {
		t.Fatal("a corrupt pointer must not be deleted by the sweep")
	}
	s := stats(t, dir)
	if s.Resumed != 1 || s.Pruned[PruneMerged] != 1 || s.Pruned[PruneClosed] != 1 || s.Pruned[PruneExpired] != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if len(audited) != 3 {
		t.Fatalf("prune audits = %v, want 3", audited)
	}
}

func TestSweep_NoLookupsAndEdgeCases(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordPR(t, dir, 1, now)
	// Without a State func nothing can be classified: kept.
	if res := Sweep(context.Background(), SweepOptions{Dir: dir, MaxLookups: 5}, now); res.Kept != 1 || !pointerExists(dir, 1) {
		t.Fatalf("sweep without lookups = %+v", res)
	}
	// Retention disabled: an ancient pointer of an open PR is kept.
	if res := Sweep(context.Background(), SweepOptions{Dir: dir}, now.Add(365*24*time.Hour)); res.Kept != 1 {
		t.Fatalf("sweep with retention off = %+v", res)
	}
	// A cancelled context stops the sweep before it deletes anything.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if res := Sweep(cancelled, SweepOptions{Dir: dir, Retention: time.Nanosecond}, now.Add(time.Hour)); res != (SweepResult{}) || !pointerExists(dir, 1) {
		t.Fatalf("cancelled sweep = %+v", res)
	}
	if res := Sweep(context.Background(), SweepOptions{Dir: ""}, now); res != (SweepResult{}) {
		t.Fatalf("empty dir sweep = %+v", res)
	}
	if res := Sweep(context.Background(), SweepOptions{Dir: filepath.Join(dir, "missing")}, now); res != (SweepResult{}) {
		t.Fatalf("missing dir sweep = %+v", res)
	}

	// A pointer without a usable PR number is not a candidate.
	env := load(t, func() string { d := t.TempDir(); record(t, d, "s1", now); return d }())
	env.Variables[varNumber] = "zero"
	odd := t.TempDir()
	if err := (turn.FileStore{Dir: odd}).Persist(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got := sweepCandidates(odd); len(got) != 0 {
		t.Fatalf("candidates = %+v, want none", got)
	}
}

func TestPointerCreatedAt(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	if _, ok := PointerCreatedAt(context.Background(), dir, testRepo, testPR); ok {
		t.Fatal("no pointer must report false")
	}
	record(t, dir, "s1", now)
	got, ok := PointerCreatedAt(context.Background(), dir, testRepo, testPR)
	if !ok || !got.Equal(now) {
		t.Fatalf("PointerCreatedAt = %v, %v; want %v, true", got, ok, now)
	}
	// "hivecommons_hive" sanitises to the same file name, but it is a
	// different PR: never eligible through the collision.
	if _, ok := PointerCreatedAt(context.Background(), dir, "hivecommons_hive", testPR); ok {
		t.Fatal("a filename-colliding repo must not borrow the pointer")
	}
}
