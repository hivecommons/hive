package stagelabels

import (
	"testing"
	"time"
)

// SupersedingCopy tells a supersession (the hive works a copy; the
// original's branch stays its author's) from an in-place adoption (the hive
// owns the PR's own branch). projectbluefin/chairlift#787 was superseded by
// #796, kept stage/074-adopted-by-hive, and the review fixer pushed to its
// Renovate branch as if it had been adopted in place.
func TestSupersedingCopy(t *testing.T) {
	now := time.Date(2026, 11, 16, 18, 30, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })

	superseded := Key("o/chairlift", 787)
	s.Observe(superseded, AdoptedByHive, "adopted")
	s.MarkAdopted(superseded, "4c585ea", 796, now)
	rec, _ := s.Get(superseded)
	if n, ok := rec.SupersedingCopy(); !ok || n != 796 {
		t.Fatalf("supersede adoption: SupersedingCopy() = %d, %v; want 796, true", n, ok)
	}
	// The original moving on past the adopted head does not end it.
	if n, ok := (Record{AdoptedHead: "4c585ea", AdoptedBy: 796, AdoptMode: AdoptModeSupersede}).SupersedingCopy(); !ok || n != 796 {
		t.Fatalf("moved head: SupersedingCopy() = %d, %v", n, ok)
	}

	inPlace := Key("o/utah", 1091)
	s.Observe(inPlace, AdoptedByHive, "adopted")
	s.MarkAdoptedInPlace(inPlace, "7e9dfd7", "contributor-offline", now)
	rec, _ = s.Get(inPlace)
	if n, ok := rec.SupersedingCopy(); ok || n != 0 {
		t.Fatalf("in-place adoption: SupersedingCopy() = %d, %v; want 0, false", n, ok)
	}
	// An in-place adoption after an earlier, finished supersession is in place.
	if _, ok := (Record{AdoptedHead: "h", AdoptMode: AdoptModeInPlace, SupersededBy: 90}).SupersedingCopy(); ok {
		t.Fatal("live in-place adoption read as a supersession")
	}

	cases := []struct {
		name string
		rec  Record
		n    int
		ok   bool
	}{
		{"no adoption", Record{}, 0, false},
		{"merged copy, adoption fields kept", Record{SupersededBy: 90}, 90, true},
		{"attempted, nothing opened", Record{AdoptedHead: "h", AdoptMode: AdoptModeSupersede}, 0, true},
		{"pre-mode record with a copy", Record{AdoptedHead: "h", AdoptedBy: 12}, 12, true},
	}
	for _, tc := range cases {
		if n, ok := tc.rec.SupersedingCopy(); n != tc.n || ok != tc.ok {
			t.Errorf("%s: SupersedingCopy() = %d, %v; want %d, %v", tc.name, n, ok, tc.n, tc.ok)
		}
	}

	s.ClearAdoption(superseded)
	rec, _ = s.Get(superseded)
	if _, ok := rec.SupersedingCopy(); ok {
		t.Fatal("cleared adoption still reads as a supersession")
	}
}
