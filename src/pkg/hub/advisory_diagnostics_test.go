package hub

import (
	"testing"
	"time"
)

// The diagnostics are pure MEASUREMENT: every hive must be classified by the
// gate that actually decided its verdict, and hidden staleness must be counted
// — that count is the prevalence answer #4167 asks for.

func TestDiagnoseAdvisory_ClassifiesEachGate(t *testing.T) {
	old := rfc3339Ago(advisoryStaleThreshold + time.Hour)

	cases := []struct {
		name        string
		entry       RegistryEntry
		wantClass   string
		wantHidden  bool
		wantHasName bool
	}{
		{
			name:      "fresh digest",
			entry:     func() RegistryEntry { e := advisoryModeEntry(); e.Online = true; return e }(),
			wantClass: advisoryClassFresh,
		},
		{
			name: "flagged stale",
			entry: func() RegistryEntry {
				e := advisoryModeEntry()
				e.Online = true
				e.AdvisoryLastPostedAt = old
				return e
			}(),
			wantClass: advisoryClassStale,
		},
		{
			name: "flagged stale but offline — no row pill renders",
			entry: func() RegistryEntry {
				e := advisoryModeEntry()
				e.Online = false
				e.AdvisoryLastPostedAt = old
				return e
			}(),
			wantClass:  advisoryClassStale,
			wantHidden: true,
		},
		{
			name:      "not participating",
			entry:     RegistryEntry{ID: "pr-only", Online: true, GitHubAppState: "ok"},
			wantClass: advisoryClassNotParticipating,
		},
		{
			name: "error suppressed by undelivered app",
			entry: func() RegistryEntry {
				e := advisoryModeEntry()
				e.Online = true
				e.GitHubAppState = "not-installed"
				e.AdvisoryError = "403 Resource not accessible by integration"
				return e
			}(),
			wantClass:  advisoryClassAppUndelivered,
			wantHidden: true,
		},
		{
			name: "aged digest suppressed by non-writing app",
			entry: func() RegistryEntry {
				e := advisoryModeEntry()
				e.Online = true
				e.GitHubAppState = "write-forbidden"
				e.AdvisoryLastPostedAt = old
				return e
			}(),
			wantClass:  advisoryClassAppCannotWrite,
			wantHidden: true,
		},
		{
			name: "unparseable timestamp",
			entry: func() RegistryEntry {
				e := advisoryModeEntry()
				e.Online = true
				e.AdvisoryLastPostedAt = "not-a-timestamp"
				return e
			}(),
			wantClass: advisoryClassUnknownTimestamp,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := diagnoseAdvisory(tc.entry, advNow)
			if d.Class != tc.wantClass {
				t.Fatalf("class = %q, want %q (reason %q)", d.Class, tc.wantClass, d.Reason)
			}
			if d.HiddenStale != tc.wantHidden {
				t.Fatalf("hidden_stale = %v, want %v", d.HiddenStale, tc.wantHidden)
			}
		})
	}
}

// A suppressed hive whose digest is NOT actually aged is not hidden staleness —
// nothing is being concealed, so it must not inflate the prevalence count.
func TestDiagnoseAdvisory_FreshDigestUnderNonWritingAppIsNotHidden(t *testing.T) {
	e := advisoryModeEntry() // posted 5 minutes ago
	e.Online = true
	e.GitHubAppState = "write-forbidden"
	d := diagnoseAdvisory(e, advNow)
	if d.Class != advisoryClassAppCannotWrite {
		t.Fatalf("class = %q, want %q", d.Class, advisoryClassAppCannotWrite)
	}
	if d.HiddenStale {
		t.Fatalf("a digest posted 5 minutes ago is not hidden staleness")
	}
}

func TestBuildAdvisoryDiagnostics_CountsAndExcludesPlaceholders(t *testing.T) {
	old := rfc3339Ago(advisoryStaleThreshold + time.Hour)
	entries := []RegistryEntry{
		// Healthy.
		func() RegistryEntry { e := advisoryModeEntry(); e.ID = "a"; e.Online = true; return e }(),
		// Flagged.
		func() RegistryEntry {
			e := advisoryModeEntry()
			e.ID, e.Online, e.AdvisoryLastPostedAt = "b", true, old
			return e
		}(),
		// Hidden: aged under a non-writing App.
		func() RegistryEntry {
			e := advisoryModeEntry()
			e.ID, e.Online, e.AdvisoryLastPostedAt, e.GitHubAppState = "c", true, old, "write-forbidden"
			return e
		}(),
		// Unassigned pool slot: must not be counted at all.
		{ID: "placeholder", Org: placeholderOrgPrefix + "1", ProvStatus: statusAvailable},
	}

	rep := buildAdvisoryDiagnostics(entries, advNow)
	if rep.TotalHives != 3 {
		t.Fatalf("placeholders must be excluded: total = %d, want 3", rep.TotalHives)
	}
	if rep.Counts[advisoryClassFresh] != 1 || rep.Counts[advisoryClassStale] != 1 ||
		rep.Counts[advisoryClassAppCannotWrite] != 1 {
		t.Fatalf("unexpected class counts: %#v", rep.Counts)
	}
	if rep.HiddenStale != 1 {
		t.Fatalf("hidden_stale = %d, want 1", rep.HiddenStale)
	}
	// Problems sort first, hidden staleness ahead of the flagged one.
	if len(rep.Hives) != 3 || rep.Hives[0].HiveID != "c" || rep.Hives[1].HiveID != "b" {
		t.Fatalf("report must lead with hidden then flagged staleness, got %v",
			[]string{rep.Hives[0].HiveID, rep.Hives[1].HiveID, rep.Hives[2].HiveID})
	}
}

// A freshly-claimed hive keeps reporting the "available-" org until it adopts
// the pushed config, so the placeholder exclusion must use the shared
// status-aware predicate. Dropping it here would hide exactly the newly
// onboarded population most likely to have an undelivered App.
func TestBuildAdvisoryDiagnostics_IncludesAssignedHiveStillReportingPlaceholderOrg(t *testing.T) {
	e := advisoryModeEntry()
	e.ID, e.Online = "just-claimed", true
	e.Org = placeholderOrgPrefix + "7"
	e.ProvStatus = statusAssigned
	e.GitHubAppState = "not-installed"
	e.AdvisoryError = "403 Resource not accessible by integration"

	rep := buildAdvisoryDiagnostics([]RegistryEntry{e}, advNow)
	if rep.TotalHives != 1 {
		t.Fatalf("an assigned hive must be measured even while it reports the placeholder org, total = %d", rep.TotalHives)
	}
	if rep.Counts[advisoryClassAppUndelivered] != 1 {
		t.Fatalf("unexpected class counts: %#v", rep.Counts)
	}
}

// The age is reported in minutes so an operator can see HOW stale, and is -1
// (not 0) when there is no parseable timestamp — unknown must never read as
// "posted just now".
func TestDiagnoseAdvisory_AgeMinutes(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = rfc3339Ago(120 * time.Minute)
	if got := diagnoseAdvisory(e, advNow).AgeMinutes; got != 120 {
		t.Fatalf("age_minutes = %d, want 120", got)
	}
	e.AdvisoryLastPostedAt = ""
	e.AdvisoryError = "boom"
	if got := diagnoseAdvisory(e, advNow).AgeMinutes; got != -1 {
		t.Fatalf("unknown age must be -1, got %d", got)
	}
}
