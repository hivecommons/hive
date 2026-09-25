package panes

import (
	"strings"
	"testing"
)

func TestTruncateMiddleKeepsSchemeAndTail(t *testing.T) {
	const url = "wss://hosted-projectbluefin-knuckle-gjvq.hive.hivecommons.dev/contribute"
	got := truncateMiddle(url, 36)
	if !strings.HasPrefix(got, "wss://") {
		t.Fatalf("truncateMiddle(%q) = %q, want scheme preserved", url, got)
	}
	if !strings.HasSuffix(got, "ons.dev/contribute") {
		t.Fatalf("truncateMiddle(%q) = %q, want distinguishing tail preserved", url, got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("truncateMiddle(%q) = %q, want ellipsis", url, got)
	}
}

func TestHivesOverlayColumnSizingUsesAvailableWidth(t *testing.T) {
	const hub = "wss://hosted-projectbluefin-knuckle-gjvq.hive.hivecommons.dev/contribute"
	o := NewHivesOverlay().SetHives([]HiveRow{{
		Name:          "hosted-projectbluefin-knuckle-gjvq",
		Hub:           hub,
		ContributorID: "contrib_a1b2",
		Active:        true,
	}}, "/cfg/profiles.yml", "/cfg/contributor.env", "ranked")

	cases := []struct {
		width       int
		wantHubFit  bool
		wantNameFit bool
	}{
		{80, false, false},
		{120, true, false},
		{200, true, true},
	}
	for _, tc := range cases {
		cols := o.hiveColumns(tc.width - 6)
		if got := cols.hub >= len([]rune(hub)); got != tc.wantHubFit {
			t.Fatalf("width %d hub width = %d, fit = %v, want %v", tc.width, cols.hub, got, tc.wantHubFit)
		}
		if got := cols.name >= len([]rune(o.rows[0].Name)); got != tc.wantNameFit {
			t.Fatalf("width %d name width = %d, fit = %v, want %v", tc.width, cols.name, got, tc.wantNameFit)
		}
	}
}
