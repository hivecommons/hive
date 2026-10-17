package github

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
)

func TestOriginMarkerRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	m := OriginMarker("hosted-available-oke-11-placeholder-r05x", at)
	if m != "<!-- hive:origin hive=hosted-available-oke-11-placeholder-r05x at=2026-10-16T20:00:00Z -->" {
		t.Fatalf("OriginMarker = %q", m)
	}
	hive, gotAt, ok := ParseOriginMarker("prose\n\n---\n🐝 **Hive Agent**: `x` | **SHA:** `abc`\n" + m + "\n")
	if !ok || hive != "hosted-available-oke-11-placeholder-r05x" || !gotAt.Equal(at) {
		t.Fatalf("ParseOriginMarker = %q %v %v", hive, gotAt, ok)
	}
	if OriginMarker("  ", at) != "" {
		t.Fatalf("blank hive id must render no marker")
	}
	if m := OriginMarker("hive-bold-fox", time.Time{}); m != "<!-- hive:origin hive=hive-bold-fox -->" {
		t.Fatalf("zero at = %q", m)
	}
	if _, _, ok := ParseOriginMarker("<!-- hive:origin at=2026-10-16T20:00:00Z -->"); ok {
		t.Fatalf("a marker without hive= must not parse")
	}
	if _, _, ok := ParseOriginMarker("<!-- hive:claim who=bob kind=human hive=other -->"); ok {
		t.Fatalf("claim markers are not origin markers")
	}
}

func TestOriginHivePrefersMarkerThenLegacyLabelThenNothing(t *testing.T) {
	marker := OriginMarker("hive-marker", time.Time{})
	cases := []struct {
		name       string
		body       string
		labels     []string
		wantHive   string
		wantSource OriginSource
	}{
		{"marker present", "body\n" + marker, []string{"agent/scanner", "hive/hive-label"}, "hive-marker", OriginSourceMarker},
		{"legacy label only", "body", []string{"agent/scanner", "hive/hive-label", "hive/selfreport"}, "hive-label", OriginSourceLabel},
		{"legacy label mixed case", "body", []string{"Hive/Hosted-X"}, "Hosted-X", OriginSourceLabel},
		{"neither", "body", []string{"agent/scanner", "hive/selfreport", "hive/advisory", "hive/claimed-by-scanner", "hive/likely-done", "hive-pause/hive-x"}, "", OriginSourceNone},
		{"empty", "", nil, "", OriginSourceNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hive, source := OriginHive(tc.body, tc.labels)
			if hive != tc.wantHive || source != tc.wantSource {
				t.Fatalf("OriginHive = %q %q, want %q %q", hive, source, tc.wantHive, tc.wantSource)
			}
		})
	}
}

// Every hive/* label this code base still reads as a signal must be excluded
// from the legacy-label fallback, or the retire sweep could treat it as a
// hive id. The list is pinned against the constants the readers use.
func TestLegacyHiveIDLabelExcludesEverySignalLabel(t *testing.T) {
	signals := []string{
		LikelyDoneLabel, PartiallyAddressedLabel, AlreadyDoneLabel, VerifiedOpenLabel,
		SupersededLabel, SupersessionKeepOpenLabel, issueHandoffLabel,
		"hive/selfreport", "hive/advisory", "hive/in-progress", "hive/covered-by-pr",
		claims.ClaimedByPrefix + "scanner",
	}
	for _, l := range signals {
		if !strings.HasPrefix(l, LegacyHiveIDLabelPrefix) {
			t.Fatalf("%q is not in the hive/ namespace; fix the test", l)
		}
		if id, ok := legacyHiveIDLabel(l); ok {
			t.Errorf("legacyHiveIDLabel(%q) = %q, want excluded as a signal label", l, id)
		}
	}
	if id, ok := legacyHiveIDLabel(HiveProvenanceLabel("hosted-available-oke-11-placeholder-r05x")); !ok || id != "hosted-available-oke-11-placeholder-r05x" {
		t.Fatalf("legacyHiveIDLabel(provenance) = %q %v", id, ok)
	}
	if _, ok := legacyHiveIDLabel("hive/"); ok {
		t.Fatalf("bare prefix must not parse")
	}
}
