package fleetreport

import (
	"strings"
	"testing"
	"time"
)

// TestBuildRuntimeReportNamesHiveAndLinksSpoke (#13211): a hub-managed spoke
// that knows its display name and dashboard URL renders both in the evidence
// table and on its affected-block line.
func TestBuildRuntimeReportNamesHiveAndLinksSpoke(t *testing.T) {
	now := time.Date(2026, 10, 20, 16, 53, 31, 0, time.UTC)
	obs := Observation{HiveID: "hive-a", HiveSlug: "hosted-oke-ba44", HiveName: "kellya", SpokeURL: "https://kellya.hive.hivecommons.dev/", Owners: []string{"clubanderson"}}
	ev := Evidence{Class: ClassModelDiscoveryDegraded, Component: "inference-gateway", Severity: "medium", Message: "model discovery degraded", FirstSeen: now, Count: 1}
	r := BuildRuntimeReport(obs, ev, "fp", "2e281aad9b0e1e15", "hosted-oke-ba44", "v5.186.4", "54e5c90", now)
	for _, want := range []string{
		"| hive | hosted-oke-ba44 |\n| hive name | kellya |\n| spoke | [kellya](https://kellya.hive.hivecommons.dev) |\n| anonymous instance | 2e281aad9b0e1e15 |",
		AffectedBlockStart + "\n**Affected hives (1):**\n- kellya (hosted-oke-ba44) (instance 2e281aad9b0e1e15) — [open](https://kellya.hive.hivecommons.dev) — owner(s): @clubanderson — seen 1, last 2026-10-20T16:53:31Z\nseen_count: 1\n" + AffectedBlockEnd,
	} {
		if !strings.Contains(r.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, r.Body)
		}
	}
	if r.HiveName != "kellya" || r.SpokeURL != "https://kellya.hive.hivecommons.dev" {
		t.Fatalf("report identity = %q %q", r.HiveName, r.SpokeURL)
	}
	blk, ok := ParseAffected(r.Body)
	if !ok || len(blk.Hives) != 1 || blk.Hives[0].Name != "kellya" || blk.Hives[0].URL != "https://kellya.hive.hivecommons.dev" || blk.Hives[0].Hive != "hosted-oke-ba44" || blk.Hives[0].Instance != "2e281aad9b0e1e15" {
		t.Fatalf("parsed = %+v", blk.Hives)
	}
	if again := blk.Render(); !strings.Contains(r.Body, again) {
		t.Fatalf("render not idempotent:\n%s", again)
	}
}

// TestBuildRuntimeReportWithoutIdentityFallsBack: no name and no URL (an
// anonymous hive or a spoke that predates the fields) renders the hive id,
// "—" rows, and the pre-#13211 affected line unchanged.
func TestBuildRuntimeReportWithoutIdentityFallsBack(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ev := Evidence{Class: ClassRestartStorm, Component: "agent-runtime", Severity: "high", FirstSeen: now, Count: 1}
	r := BuildRuntimeReport(Observation{HiveID: "hive-a"}, ev, "fp", "inst", "", "v5", "c", now)
	for _, want := range []string{
		"| hive | inst |\n| hive name | — |\n| spoke | — |\n| anonymous instance | inst |",
		"**Affected hives (1):**\n- inst — owner(s): unknown — seen 1, last 2026-10-20T12:00:00Z\n",
	} {
		if !strings.Contains(r.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, r.Body)
		}
	}
	if strings.Contains(r.Body, "[open]") || r.HiveName != "" || r.SpokeURL != "" {
		t.Fatalf("anonymous report leaked identity: %+v", r)
	}
	// URL without a name links the hive id.
	r = BuildRuntimeReport(Observation{HiveSlug: "hosted-x", SpokeURL: "https://x.example.com"}, ev, "fp", "inst", "hosted-x", "v5", "c", now)
	if !strings.Contains(r.Body, "| hive name | — |\n| spoke | [hosted-x](https://x.example.com) |") || !strings.Contains(r.Body, "- hosted-x (instance inst) — [open](https://x.example.com) — owner(s): unknown") {
		t.Fatalf("url-only body:\n%s", r.Body)
	}
}

func TestNormalizeHiveNameAndSpokeURL(t *testing.T) {
	for in, want := range map[string]string{
		"  kellya ":                      "kellya",
		"hive's hive":                    "hive's hive",
		"a | b — c [d] `e` <f>":          "a b c d e f",
		"Acme (prod)":                    "Acme (prod)",
		"multi\n line\tname":             "multi line name",
		"ghp_" + strings.Repeat("a", 36): "REDACTED",
		"":                               "",
		strings.Repeat("x", 120):         strings.Repeat("x", 100),
	} {
		if got := NormalizeHiveName(in); got != want {
			t.Errorf("NormalizeHiveName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"https://Kellya.Hive.hivecommons.dev/":               "https://kellya.hive.hivecommons.dev",
		"https://user:pw@x.example.com/path/?q=1#frag":       "https://x.example.com/path",
		"http://x.example.com:8080":                          "http://x.example.com:8080",
		"ftp://x.example.com":                                "",
		"x.example.com":                                      "",
		"https://":                                           "",
		"https://x.example.com/a b":                          "",
		"https://x.example.com/)[open](https://evil.example": "",
		"javascript:alert(1)":                                "",
		"":                                                   "",
	} {
		if got := NormalizeSpokeURL(in); got != want {
			t.Errorf("NormalizeSpokeURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SpokeLink("", "", "https://x.example.com"); got != "[open](https://x.example.com)" {
		t.Fatalf("SpokeLink nameless = %q", got)
	}
}

// TestAffectedLineNameWithParenthesesRoundTrips: a display name that itself
// contains parentheses still parses back to name + hive id.
func TestAffectedLineNameWithParenthesesRoundTrips(t *testing.T) {
	in := AffectedHive{Hive: "hosted-x", Name: "Acme (prod)", URL: "https://x.example.com", Instance: "abcd", Owners: []string{"alice"}, Seen: 2}
	line := in.normalize().renderLine()
	if line != "- Acme (prod) (hosted-x) (instance abcd) — [open](https://x.example.com) — owner(s): @alice — seen 2" {
		t.Fatalf("line = %q", line)
	}
	got, ok := parseAffectedLine(line)
	if !ok || got.Name != "Acme (prod)" || got.Hive != "hosted-x" || got.URL != "https://x.example.com" || got.Instance != "abcd" || got.Seen != 2 {
		t.Fatalf("parsed = %+v", got)
	}
	// Name equal to the hive id is redundant and dropped.
	if h := (AffectedHive{Hive: "hosted-x", Name: "hosted-x"}).normalize(); h.Name != "" {
		t.Fatalf("redundant name kept: %+v", h)
	}
}

// TestUpdateAffectedAddsIdentityOnJoin: a hive that first reported without
// name/URL gains both on its next join; a later report with neither keeps
// them; a renamed hive's newer name wins.
func TestUpdateAffectedAddsIdentityOnJoin(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	body := "report\n\n" + AffectedBlock{Hives: []AffectedHive{{Hive: "hosted-x", Instance: "abcd", Last: now}}, Seen: 1}.Render()
	updated, changed := UpdateAffected(body, AffectedHive{Hive: "hosted-x", Name: "kellya", URL: "https://x.example.com", Last: now.Add(time.Hour)}, nil, true)
	if !changed || !strings.Contains(updated, "- kellya (hosted-x) (instance abcd) — [open](https://x.example.com) — owner(s): unknown — seen 2, last 2026-10-20T13:00:00Z") {
		t.Fatalf("updated = %q", updated)
	}
	kept, changed := UpdateAffected(updated, AffectedHive{Hive: "hosted-x", Last: now.Add(2 * time.Hour)}, nil, true)
	if !changed || !strings.Contains(kept, "- kellya (hosted-x) (instance abcd) — [open](https://x.example.com) — owner(s): unknown — seen 3") {
		t.Fatalf("kept = %q", kept)
	}
	renamed, changed := UpdateAffected(kept, AffectedHive{Hive: "hosted-x", Name: "kellya-prod"}, nil, false)
	if !changed || !strings.Contains(renamed, "- kellya-prod (hosted-x) (instance abcd) — [open](https://x.example.com) — owner(s): unknown — seen 3") {
		t.Fatalf("renamed = %q", renamed)
	}
	if _, changed := UpdateAffected(renamed, AffectedHive{Hive: "hosted-x", Name: "kellya-prod", URL: "https://x.example.com"}, nil, false); changed {
		t.Fatal("identical identity must not change the body")
	}
}

// TestUpsertHiveIdentity covers the hub filling the evidence rows of an
// older spoke's body, overlaying a current spoke's rows, and leaving a body
// alone when it has nothing to add.
func TestUpsertHiveIdentity(t *testing.T) {
	legacy := "## Evidence\n\n| Field | Value |\n|---|---|\n| hive | hosted-x |\n| anonymous instance | abcd |\n| owner(s) | — |\n"
	got, changed := UpsertHiveIdentity(legacy, "kellya", "https://x.example.com")
	if !changed || got != "## Evidence\n\n| Field | Value |\n|---|---|\n| hive | hosted-x |\n| hive name | kellya |\n| spoke | [kellya](https://x.example.com) |\n| anonymous instance | abcd |\n| owner(s) | — |\n" {
		t.Fatalf("legacy upsert = %q", got)
	}
	// Overlay: the hub's name wins, the spoke's URL is kept when the hub has none.
	current := "| hive | hosted-x |\n| hive name | spoke-name |\n| spoke | [spoke-name](https://spoke.example.com) |\n| anonymous instance | abcd |"
	got, changed = UpsertHiveIdentity(current, "hub-name", "")
	if !changed || got != "| hive | hosted-x |\n| hive name | hub-name |\n| spoke | [hub-name](https://spoke.example.com) |\n| anonymous instance | abcd |" {
		t.Fatalf("overlay = %q", got)
	}
	// Nothing to add, or already identical: untouched.
	if _, changed := UpsertHiveIdentity(current, "", ""); changed {
		t.Fatal("empty identity changed the body")
	}
	if _, changed := UpsertHiveIdentity(current, "spoke-name", "https://spoke.example.com"); changed {
		t.Fatal("identical identity changed the body")
	}
	// A URL-only hub fill on a dash row keeps the hive id as the link label.
	dashes := "| hive | hosted-x |\n| hive name | — |\n| spoke | — |"
	got, changed = UpsertHiveIdentity(dashes, "", "https://x.example.com")
	if !changed || got != "| hive | hosted-x |\n| hive name | — |\n| spoke | [hosted-x](https://x.example.com) |" {
		t.Fatalf("dash fill = %q", got)
	}
	// No evidence table (an observation or human-filed body): untouched.
	if _, changed := UpsertHiveIdentity("human filed body", "kellya", "https://x.example.com"); changed {
		t.Fatal("body without a hive row changed")
	}
}
