package hub

import (
	"testing"
	"time"
)

// Regression cover for the advisory-staleness FALSE NEGATIVES found while
// investigating #4167: cases where a hive's digest was genuinely wedged and the
// hub reported nothing. Each test names the scenario it locks down.

// FALSE NEGATIVE 1 — self-suppressing post failure.
//
// The 403 that wedges the digest is the SAME event that makes the spoke raise
// GitHubAppRequired and classify itself "write-forbidden". Gating the reported
// error on appCanWriteForAdvisory therefore let the failure silence its own
// alarm: participation was proven, the failure was proven, and the pill stayed
// dark. A DELIVERED App that cannot write must be flagged.
func TestAdvisoryStale_DeliveredAppWriteFailureIsSurfaced(t *testing.T) {
	cases := []struct {
		name  string
		mutfn func(*RegistryEntry)
	}{
		{"write-forbidden with banner raised", func(e *RegistryEntry) {
			e.GitHubAppState = "write-forbidden"
			e.GitHubAppRequired = true
		}},
		{"insufficient-permissions", func(e *RegistryEntry) {
			e.GitHubAppState = "insufficient-permissions"
		}},
		{"wrong-installation", func(e *RegistryEntry) {
			e.GitHubAppState = "wrong-installation"
		}},
		{"key-invalid", func(e *RegistryEntry) {
			e.GitHubAppState = "key-invalid"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := advisoryModeEntry()
			e.AdvisoryError = "403 Resource not accessible by integration"
			tc.mutfn(&e)
			stale, reason := advisoryStale(e, advNow)
			if !stale {
				t.Fatalf("a delivered App whose advisory post FAILED must be flagged stale")
			}
			if reason == "" {
				t.Fatalf("the flag must carry the reported cause")
			}
		})
	}
}

// The onboarding suppression must survive that change: an App nobody has
// installed or keyed yet is expected to fail, and must stay quiet.
func TestAdvisoryStale_UndeliveredAppStillSuppressesError(t *testing.T) {
	cases := []struct {
		name  string
		mutfn func(*RegistryEntry)
	}{
		{"not-installed", func(e *RegistryEntry) { e.GitHubAppState = "not-installed" }},
		{"no-app-assigned", func(e *RegistryEntry) { e.GitHubAppState = "no-app-assigned" }},
		{"key-missing", func(e *RegistryEntry) { e.GitHubAppState = "key-missing" }},
		{"pending install", func(e *RegistryEntry) { e.PendingGitHubAppInstall = true }},
		{"old spoke: banner up, no state", func(e *RegistryEntry) {
			e.GitHubAppState = ""
			e.GitHubAppRequired = true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := advisoryModeEntry()
			e.AdvisoryError = "403 Resource not accessible by integration"
			tc.mutfn(&e)
			if stale, reason := advisoryStale(e, advNow); stale {
				t.Fatalf("an undelivered App's post error must not alarm, got %q", reason)
			}
		})
	}
}

// The AGE path keeps the broad app-can-write gate: with no error reported there
// is no evidence the hive even attempted a post, so a non-writing App's silence
// stays expected.
func TestAdvisoryStale_AgePathKeepsBroadAppGate(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = rfc3339Ago(advisoryStaleThreshold + time.Hour)
	e.GitHubAppState = "write-forbidden"
	if stale, _ := advisoryStale(e, advNow); stale {
		t.Fatalf("an aged digest with NO reported error must stay suppressed for a non-writing App")
	}
}

// FALSE NEGATIVE 2 — restart amnesia.
//
// advisoryLastPostedAt lives only in the spoke's memory, so every restart
// reports it EMPTY until the next successful post. Empty is exactly what gate 1
// reads as "not an advisory participant", so a hive whose digest path wedges
// across a restart can never be flagged: it cannot post (so the field never
// returns) and the hub cannot tell it from a PR-only hive. The hub therefore
// carries the last known value forward.
func TestHeartbeat_CarriesAdvisoryPostTimeAcrossRestart(t *testing.T) {
	posted := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	prev := RegistryEntry{
		ID:                   "hosted-restart",
		PrimaryRepo:          "acme/widgets",
		AdvisoryLastPostedAt: posted,
	}
	fresh := RegistryEntry{
		ID:          "hosted-restart",
		PrimaryRepo: "acme/widgets",
		// The restarted spoke reports nothing.
		AdvisoryLastPostedAt: "",
	}
	carryAdvisoryPostTime(&fresh, prev)
	if fresh.AdvisoryLastPostedAt != posted {
		t.Fatalf("last advisory post time must survive a spoke restart, got %q", fresh.AdvisoryLastPostedAt)
	}
	// And the carried value must age into a stale flag, which is the whole
	// point of preserving it.
	fresh.GitHubAppState = "ok"
	if stale, _ := advisoryStale(fresh, time.Now()); !stale {
		t.Fatalf("a carried-forward post time older than the threshold must flag stale")
	}
}

// A fresh report always wins over the carried value — the spoke is the source
// of truth whenever it has one.
func TestHeartbeat_FreshAdvisoryPostTimeWins(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	prev := RegistryEntry{ID: "h", PrimaryRepo: "acme/widgets", AdvisoryLastPostedAt: "2020-01-01T00:00:00Z"}
	fresh := RegistryEntry{ID: "h", PrimaryRepo: "acme/widgets", AdvisoryLastPostedAt: now}
	carryAdvisoryPostTime(&fresh, prev)
	if fresh.AdvisoryLastPostedAt != now {
		t.Fatalf("a reported post time must never be overwritten by the carried one")
	}
}

// A reclaimed placeholder is a DIFFERENT project on the same hive ID, so it must
// not inherit the previous tenant's advisory history and be alarmed for it.
func TestHeartbeat_AdvisoryPostTimeNotCarriedAcrossRepoChange(t *testing.T) {
	prev := RegistryEntry{ID: "h", PrimaryRepo: "acme/widgets", AdvisoryLastPostedAt: "2020-01-01T00:00:00Z"}
	fresh := RegistryEntry{ID: "h", PrimaryRepo: "other/project"}
	carryAdvisoryPostTime(&fresh, prev)
	if fresh.AdvisoryLastPostedAt != "" {
		t.Fatalf("a re-tenanted hive must start with a clean advisory history, got %q", fresh.AdvisoryLastPostedAt)
	}
}
