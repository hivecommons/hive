package fleetreport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNormalizeLogins(t *testing.T) {
	got := NormalizeLogins([]string{"@Alice", "github:bob", "alice", "ibmid:5500087VJB", "not a login", "-dash", "x--y", "", "carol-1"})
	if strings.Join(got, ",") != "alice,bob,carol-1" {
		t.Fatalf("NormalizeLogins = %v", got)
	}
	if OwnerMentions([]string{"bob", "alice"}, false) != "@alice, @bob" || OwnerMentions(nil, false) != "—" || OwnerMentions([]string{"alice"}, true) != AffectedOwnersAnonymous {
		t.Fatalf("OwnerMentions renders wrong")
	}
}

func TestBuildRuntimeReportNamesOwnersInTableAndAffectedBlock(t *testing.T) {
	now := time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC)
	ev := Evidence{Class: ClassRestartStorm, Component: "agent-runtime", Severity: "high", Count: 4, Message: "restart storm: agent quality restarted 4 times in 10m"}
	obs := Observation{HiveID: "hive-a", Version: "v5.1.0", HiveSlug: "hosted-aslom", Owners: []string{"github:Alice", "bob", "alice"}}
	r := BuildRuntimeReport(obs, ev, "fp", "b04ca59b7312017e", "hosted-aslom", "v5.1.0", "abcdef1", now)
	for _, want := range []string{
		"| owner(s) | @alice, @bob |",
		AffectedBlockStart + "\n**Affected hives (1):**\n- hosted-aslom (instance b04ca59b7312017e) — owner(s): @alice, @bob — seen 1, last 2026-10-17T11:00:00Z\nseen_count: 1\n" + AffectedBlockEnd,
	} {
		if !strings.Contains(r.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, r.Body)
		}
	}
	if strings.Join(r.Owners, ",") != "alice,bob" || r.OwnersHidden {
		t.Fatalf("report owners = %v hidden=%v", r.Owners, r.OwnersHidden)
	}
	// The owners travel on the wire as an explicit field.
	raw, _ := json.Marshal(r)
	var wire struct {
		Owners []string `json:"owners"`
	}
	_ = json.Unmarshal(raw, &wire)
	if strings.Join(wire.Owners, ",") != "alice,bob" {
		t.Fatalf("wire owners = %v", wire.Owners)
	}
}

func TestBuildRuntimeReportAnonymousOwnersOptOut(t *testing.T) {
	now := time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC)
	ev := Evidence{Class: ClassRestartStorm, Component: "agent-runtime", Severity: "high", Count: 1, Message: "restart storm"}
	obs := Observation{HiveID: "hive-a", Version: "v5.1.0", HiveSlug: "hosted-aslom", Owners: []string{"alice"}, OwnersHidden: true}
	r := BuildRuntimeReport(obs, ev, "fp", "inst", "hosted-aslom", "v5.1.0", "abcdef1", now)
	if !strings.Contains(r.Body, "| owner(s) | (anonymous) |") || !strings.Contains(r.Body, "- hosted-aslom (instance inst) — owner(s): (anonymous) — seen 1, last 2026-10-17T11:00:00Z") {
		t.Fatalf("anonymous body:\n%s", r.Body)
	}
	if strings.Contains(r.Body, "alice") || r.Owners != nil || !r.OwnersHidden {
		t.Fatalf("opted-out hive leaked its owner: %v\n%s", r.Owners, r.Body)
	}
	// A later report that is NOT hidden never re-attaches owners to a hidden line.
	later := time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC)
	kept, _ := UpdateAffected(r.Body, AffectedHive{Hive: "hosted-aslom", Owners: []string{"alice"}, OwnersHidden: true, Last: later}, nil, true)
	if !strings.Contains(kept, "- hosted-aslom (instance inst) — owner(s): (anonymous) — seen 2, last 2026-10-18T11:00:00Z") {
		t.Fatalf("hidden line after re-report:\n%s", kept)
	}
}

func TestAffectedBlockPerHiveOwnersRoundTrip(t *testing.T) {
	t1 := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	body := "intro\n\n" + AffectedBlock{Hives: []AffectedHive{{Hive: "hive-a", Instance: "aaaa1111", Owners: []string{"alice"}, Seen: 1, Last: t1}}, Repos: []string{"hivecommons/hive"}, Seen: 1}.Render() + "\n\ntrailer"

	blk, ok := ParseAffected(body)
	if !ok || len(blk.Hives) != 1 || blk.Hives[0].Instance != "aaaa1111" || strings.Join(blk.Hives[0].Owners, ",") != "alice" || blk.Hives[0].Seen != 1 || !blk.Hives[0].Last.Equal(t1) || blk.Seen != 1 {
		t.Fatalf("parse = %#v ok=%v", blk, ok)
	}

	// A second hive joins with its own owners.
	updated, changed := UpdateAffected(body, AffectedHive{Hive: "hive-b", Instance: "bbbb2222", Owners: []string{"Bob", "carol"}, Last: t2}, []string{"acme/widgets"}, true)
	want := AffectedBlockStart + "\n**Affected hives (2):**\n" +
		"- hive-a (instance aaaa1111) — owner(s): @alice — seen 1, last 2026-10-17T09:00:00Z\n" +
		"- hive-b (instance bbbb2222) — owner(s): @bob, @carol — seen 1, last 2026-10-17T10:00:00Z\n" +
		"**Affected repos (2):** acme/widgets, hivecommons/hive\n**Affected repo owners (2):** acme, hivecommons\nseen_count: 2\n" + AffectedBlockEnd
	if !changed || !strings.Contains(updated, want) || !strings.HasPrefix(updated, "intro\n\n") || !strings.HasSuffix(updated, "\n\ntrailer") {
		t.Fatalf("join:\n%s\nwant:\n%s", updated, want)
	}

	// The same hive re-reporting with nothing new is a no-op.
	same, changed := UpdateAffected(updated, AffectedHive{Hive: "hive-b", Owners: []string{"carol", "bob"}, Last: t2.Add(time.Hour)}, nil, false)
	if changed || same != updated {
		t.Fatalf("idempotent re-report changed the body:\n%s", same)
	}

	// A bump on a known hive raises its own seen and last-seen plus the total.
	t3 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	bumped, changed := UpdateAffected(updated, AffectedHive{Hive: "hive-a", Last: t3}, nil, true)
	if !changed || !strings.Contains(bumped, "- hive-a (instance aaaa1111) — owner(s): @alice — seen 2, last 2026-10-17T12:00:00Z") || !strings.Contains(bumped, "seen_count: 3") {
		t.Fatalf("bump:\n%s", bumped)
	}
}

func TestAffectedBlockLegacyInlineGainsOwnersOnNextSweep(t *testing.T) {
	// The exact block hivecommons/hive#12529 carried.
	legacy := "<!-- hive:selfreport fp=92fd hive=hosted-llm-d-incubation-llm-d-fast-m-yfzz -->\n\n" +
		AffectedBlockStart + "\n**Affected hives (1):** hosted-llm-d-incubation-llm-d-fast-m-yfzz\nseen_count: 1\n" + AffectedBlockEnd + "\n"
	hives, seen, ok := ParseAffectedBlock(legacy)
	if !ok || seen != 1 || strings.Join(hives, ",") != "hosted-llm-d-incubation-llm-d-fast-m-yfzz" {
		t.Fatalf("legacy parse = %v %d %v", hives, seen, ok)
	}
	now := time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC)
	entry := AffectedHive{Hive: "hosted-llm-d-incubation-llm-d-fast-m-yfzz", Instance: "b04ca59b7312017e", Owners: []string{"clubanderson"}, Last: now}
	// Even without a bump (the hive already listed itself) the sweep fills in
	// its identity because the rendered block differs.
	upgraded, changed := UpdateAffected(legacy, entry, nil, false)
	want := "**Affected hives (1):**\n- hosted-llm-d-incubation-llm-d-fast-m-yfzz (instance b04ca59b7312017e) — owner(s): @clubanderson — seen 1, last 2026-10-17T11:00:00Z\nseen_count: 1\n"
	if !changed || !strings.Contains(upgraded, want) || !strings.HasPrefix(upgraded, "<!-- hive:selfreport fp=92fd") {
		t.Fatalf("legacy upgrade:\n%s", upgraded)
	}
	// Legacy "Affected owners" (repo owners) lines are re-derived, never parsed.
	legacyRepos := AffectedBlockStart + "\n**Affected hives (1):** hive-a\n**Affected repos (1):** acme/widgets\n**Affected owners (1):** acme\nseen_count: 1\n" + AffectedBlockEnd
	blk, ok := ParseAffected(legacyRepos)
	if !ok || len(blk.Hives) != 1 || len(blk.Hives[0].Owners) != 0 || strings.Join(blk.Repos, ",") != "acme/widgets" {
		t.Fatalf("legacy repos parse = %#v", blk)
	}
	if out := blk.Render(); !strings.Contains(out, "**Affected repo owners (1):** acme") || strings.Contains(out, "**Affected owners") {
		t.Fatalf("render:\n%s", out)
	}
}
