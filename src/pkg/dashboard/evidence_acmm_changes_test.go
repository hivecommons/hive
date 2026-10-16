package dashboard

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

func TestEvidenceACMMChanges(t *testing.T) {
	bundle := func(repo, at string, level int) *evidence.Bundle {
		ts, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		return &evidence.Bundle{Repo: repo, Number: 1, CreatedAt: ts, Policy: evidence.Policy{ACMMLevel: level}}
	}
	// Deliberately out of order, including multiple bundles for the same PR,
	// a return to a known level, and an observation crossing a UTC month.
	bundles := []*evidence.Bundle{
		bundle("O/R", "2026-04-10T00:00:00Z", 2),
		bundle("o/r", "2026-02-10T00:00:00Z", 3),
		bundle("o/r", "2026-02-01T00:30:00+02:00", 2),
		bundle("o/r", "2026-03-10T00:00:00Z", 3),
		bundle("o/other", "2026-01-10T00:00:00Z", 4),
		bundle("o/r", "2026-01-01T00:00:00Z", 0),
		bundle("o/r", "2026-01-01T00:00:00Z", 7),
		nil,
		{Repo: "o/r", Policy: evidence.Policy{ACMMLevel: 5}},
	}
	want := []EvidenceACMMChange{{Repo: "o/r", Month: "2026-01", Level: 2}, {Repo: "o/r", Month: "2026-02", Level: 3}}
	if got := evidenceACMMChanges(bundles, "O/R"); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	// The summary must retain early policy observations even when the latest
	// bundle per PR, selected author kind, or PR window would hide them.
	resp := buildEvidenceSummary(bundles[:6], time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC), 24, "o/r", evidence.AuthorHuman)
	if !reflect.DeepEqual(resp.ACMMChanges, want) {
		t.Fatalf("summary changes = %+v, want %+v", resp.ACMMChanges, want)
	}
	if len(evidenceACMMChanges(bundles, "")) != 3 {
		t.Fatal("all-repo markers missing")
	}
	if evidenceACMMChanges(nil, "") != nil {
		t.Fatal("empty input must omit markers")
	}
}
