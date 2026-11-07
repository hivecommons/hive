package knowledge

import (
	"reflect"
	"testing"
)

// RenderedSlugs (hivecommons/hive#17052) tells the mid-task advisor which
// entries the kick really carried, so it never re-sends a capped-out one and
// never counts a capped-out one as already delivered.

func TestPrimerRenderedSlugs_TracksKeptFactsOnly(t *testing.T) {
	pk := &PrimedKnowledge{Facts: renderFacts(20, 280), MaxBytes: 2048}
	if pk.RenderedSlugs() != nil {
		t.Fatal("slugs before Render should be nil")
	}
	_, omitted := pk.Render()
	got := pk.RenderedSlugs()
	if len(got)+omitted.Facts != len(pk.Facts) || len(got) == 0 {
		t.Fatalf("rendered %d + omitted %d != %d", len(got), omitted.Facts, len(pk.Facts))
	}
	var want []string
	for _, f := range pk.Facts[:len(got)] {
		want = append(want, f.Slug)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("slugs = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if pk.RenderedSlugs()[0] == "mutated" {
		t.Error("RenderedSlugs must return a copy")
	}

	// A second Render starts the list over instead of appending.
	pk.MaxBytes = 1 << 20
	pk.Render()
	if n := len(pk.RenderedSlugs()); n != len(pk.Facts) {
		t.Errorf("uncapped re-render kept %d slugs, want %d", n, len(pk.Facts))
	}
}
