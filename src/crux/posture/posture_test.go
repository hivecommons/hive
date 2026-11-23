package posture

import (
	"io"
	"log/slog"
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
	for _, in := range []string{"CapEff:\tzz\n", "NoNewPrivs:\tx\n", "Seccomp:\tx\n"} {
		if _, err := ParseStatus(strings.NewReader(in)); err == nil {
			t.Fatalf("%q: want error", in)
		}
	}
}

func TestRequireHardened(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "": false, "0": false} {
		if got := RequireHardened(func(string) string { return v }); got != want {
			t.Fatalf("%q = %v", v, got)
		}
	}
}

func TestBootWarnsAndFailsOnlyWhenRequired(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gaps := Check()
	if err := Boot(logger, func(string) string { return "" }); err != nil {
		t.Fatalf("Boot without %s = %v, want nil", RequireHardenedEnv, err)
	}
	err := Boot(logger, func(k string) string {
		if k == RequireHardenedEnv {
			return "1"
		}
		return ""
	})
	if (err != nil) != (len(gaps) > 0) {
		t.Fatalf("Boot required err = %v with %d gaps", err, len(gaps))
	}
	if err != nil && !strings.Contains(err.Error(), RequireHardenedEnv) {
		t.Fatalf("error %q does not name %s", err, RequireHardenedEnv)
	}
}
