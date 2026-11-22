package posture

import (
	"strings"
	"testing"
)

const hardened = "Name:\thive\nNoNewPrivs:\t1\nSeccomp:\t2\nCapEff:\t0000000000000000\n"
const loose = "Name:\thive\nNoNewPrivs:\t0\nSeccomp:\t0\nCapEff:\t00000000a80425fb\n"

func TestParseStatusAndGaps(t *testing.T) {
	st, err := ParseStatus(strings.NewReader(hardened))
	if err != nil {
		t.Fatal(err)
	}
	if g := Gaps(st, true, false); len(g) != 0 {
		t.Fatalf("hardened gaps = %v", g)
	}
	st, err = ParseStatus(strings.NewReader(loose))
	if err != nil {
		t.Fatal(err)
	}
	if g := Gaps(st, true, true); len(g) != 4 {
		t.Fatalf("loose gaps = %v, want 4", g)
	}
	if g := Gaps(st, false, true); len(g) != 3 {
		t.Fatalf("rootfs unchecked gaps = %v, want 3", g)
	}
}

func TestMissingFieldsAreGaps(t *testing.T) {
	st, _ := ParseStatus(strings.NewReader("Name:\tx\n"))
	if g := Gaps(st, false, false); len(g) != 3 {
		t.Fatalf("gaps = %v, want 3", g)
	}
}

func TestParseStatusRejectsGarbage(t *testing.T) {
	if _, err := ParseStatus(strings.NewReader("CapEff:\tzz\n")); err == nil {
		t.Fatal("want error")
	}
}

func TestRequireHardened(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "": false, "0": false} {
		if got := RequireHardened(func(string) string { return v }); got != want {
			t.Fatalf("%q = %v", v, got)
		}
	}
}
