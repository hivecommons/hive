package hub

import (
	"testing"
	"time"
)

func TestForgeUnreachableLabel(t *testing.T) {
	cases := []struct {
		name string
		e    RegistryEntry
		want string
	}{
		{"host", RegistryEntry{Online: true, ForgeUnreachable: true, GitHubHost: "github.ibm.com"}, "forge unreachable (github.ibm.com)"},
		{"no host", RegistryEntry{Online: true, ForgeUnreachable: true}, "forge unreachable"},
		{"old spoke", RegistryEntry{Online: true, GitHubHost: "github.ibm.com"}, ""},
		{"offline", RegistryEntry{ForgeUnreachable: true, GitHubHost: "github.ibm.com"}, ""},
	}
	for _, tc := range cases {
		if got := forgeUnreachableLabel(tc.e); got != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestForgeUnreachable_NotCountedAgainstHealth(t *testing.T) {
	e := RegistryEntry{
		ID: "h1", Online: true, ACMMLevel: 3,
		GitHubAppState: "unreachable", GitHubAppRequired: true,
	}
	rollup := agentFleetRollup{Known: 1, Running: 1, Expected: 1}
	app := GitHubAppHealth{Bucket: ghAppBucketBroken}
	now := time.Now()

	before := hiveHealthFor(e, rollup, app, 0, now)
	if before.State != HealthStateRed {
		t.Fatalf("baseline broken-app beat should be red, got %q", before.State)
	}

	e.ForgeUnreachable = true
	after := hiveHealthFor(e, rollup, app, 0, now)
	if after.State == HealthStateUnknown || after.State == HealthStateRed {
		t.Fatalf("forge-unreachable hive must not be penalised, got %q (%s)", after.State, after.Reason)
	}
}

// A beat carrying both flags still has cached stats: the hub must not assert
// health from them, but it names the forge as the cause instead of a generic
// stale verdict.
func TestForgeUnreachable_StaleStatsStayUnknown(t *testing.T) {
	e := RegistryEntry{
		ID: "h1", Online: true, StatsStale: true, ForgeUnreachable: true, ACMMLevel: 3,
	}
	rollup := agentFleetRollup{Known: 1, Running: 1, Expected: 1}
	v := hiveHealthFor(e, rollup, GitHubAppHealth{Bucket: ghAppBucketOK}, 0, time.Now())
	if v.State != HealthStateUnknown {
		t.Fatalf("stale stats must stay unknown even when forge unreachable, got %q", v.State)
	}
	if v.Reason != "heartbeat stats stale (forge unreachable)" {
		t.Fatalf("reason = %q", v.Reason)
	}
}

func TestForgeUnreachable_NotStaleAdvisory(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = rfc3339Ago(30 * 24 * time.Hour)
	if stale, _ := advisoryStale(e, advNow); !stale {
		t.Fatal("baseline: aged digest should be stale")
	}
	e.ForgeUnreachable = true
	if stale, reason := advisoryStale(e, advNow); stale {
		t.Fatalf("forge-unreachable hive must not be stale-advisory, got %q", reason)
	}
}

func TestForgeUnreachable_WithoutForgeChecks(t *testing.T) {
	got := withoutForgeChecks([]string{"github_auth", "disk"})
	if len(got) != 1 || got[0] != "disk" {
		t.Fatalf("got %v", got)
	}
}
