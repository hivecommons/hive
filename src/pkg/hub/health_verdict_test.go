package hub

import (
	"testing"
	"time"
)

// ractivity builds a one-repo RepoActivityWire with the given create/merge
// timestamps (issue and merge), leaving other stats empty.
func ractivity(repo, issueAt, prAt, mergeAt, advisoryAt string) RepoActivityWire {
	return RepoActivityWire{
		Repo:     repo,
		Issues:   ActivityStatWire{Count: 1, NewestAt: issueAt},
		PRs:      ActivityStatWire{Count: 1, NewestAt: prAt},
		Merges:   ActivityStatWire{Count: 1, NewestAt: mergeAt},
		Advisory: ActivityStatWire{Count: 1, NewestAt: advisoryAt},
	}
}

func okRollup() agentFleetRollup {
	return agentFleetRollup{Expected: 2, Running: 2, Able: 2, Known: 2, Problems: 0}
}

func okApp() GitHubAppHealth { return GitHubAppHealth{Bucket: ghAppBucketOK} }

// withActivity attaches a repo-activity summary to an entry and returns it.
func withActivity(e RegistryEntry, repos ...RepoActivityWire) RegistryEntry {
	e.RepoActivity = repos
	return e
}

// The verdict must band by ACMM level exactly as the operator defined:
// L1 no-output→green, L2 advisory-fresh→green, L3–L5 creates (unmerged is fine),
// L6 merges (unmerged+queued→red), with precondition + unknown gates on top.
func TestHiveHealthFor_ACMMBands(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	recent := rfc(now.Add(-1 * time.Hour))   // within 12h
	oldTs := rfc(now.Add(-30 * time.Hour))   // outside 12h
	freshAdv := rfc(now.Add(-2 * time.Hour)) // within advisory aging window

	base := func(level int) RegistryEntry {
		return RegistryEntry{Online: true, ACMMLevel: level}
	}

	tests := []struct {
		name      string
		entry     RegistryEntry
		rollup    agentFleetRollup
		app       GitHubAppHealth
		queued    int
		wantState string
		wantKind  string
	}{
		{
			name:      "L1 inception — no output ever, still green",
			entry:     base(1),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    5,
			wantState: HealthStateGreen,
			wantKind:  "none",
		},
		{
			name:      "L1 inception — App broken is NOT a fault (no output to enable)",
			entry:     base(1),
			rollup:    okRollup(),
			app:       GitHubAppHealth{Bucket: ghAppBucketBroken},
			queued:    5,
			wantState: HealthStateGreen,
			wantKind:  "none",
		},
		{
			name: "L2 advisory fresh — green",
			entry: func() RegistryEntry {
				e := base(2)
				e.AdvisoryLastPostedAt = freshAdv
				return e
			}(),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    3,
			wantState: HealthStateGreen,
			wantKind:  "advisory",
		},
		{
			name:      "L4 creates recent — green (unmerged is fine)",
			entry:     withActivity(base(4), ractivity("o/r", recent, recent, "", "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    4,
			wantState: HealthStateGreen,
			wantKind:  "creates",
		},
		{
			name:      "L4 creates STALE with queued work — red",
			entry:     withActivity(base(4), ractivity("o/r", oldTs, oldTs, "", "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    7,
			wantState: HealthStateRed,
			wantKind:  "creates",
		},
		{
			name:      "L4 creates stale but queue EMPTY — green (idle, nothing to do)",
			entry:     withActivity(base(4), ractivity("o/r", oldTs, oldTs, "", "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    0,
			wantState: HealthStateGreen,
			wantKind:  "creates",
		},
		{
			name:      "L6 merge recent — green",
			entry:     withActivity(base(6), ractivity("o/r", recent, recent, recent, "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    2,
			wantState: HealthStateGreen,
			wantKind:  "merges",
		},
		{
			name:      "L6 created but NOT merged, work queued — red (this is what L6 catches)",
			entry:     withActivity(base(6), ractivity("o/r", recent, recent, oldTs, "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    3,
			wantState: HealthStateRed,
			wantKind:  "merges",
		},
		{
			name:      "L4 App broken — red precondition (output impossible)",
			entry:     withActivity(base(4), ractivity("o/r", recent, recent, "", "")),
			rollup:    okRollup(),
			app:       GitHubAppHealth{Bucket: ghAppBucketBroken},
			queued:    3,
			wantState: HealthStateRed,
			wantKind:  "creates",
		},
		{
			name:      "L4 no agents running — red precondition",
			entry:     withActivity(base(4), ractivity("o/r", recent, recent, "", "")),
			rollup:    agentFleetRollup{Expected: 2, Running: 0, Known: 2},
			app:       okApp(),
			queued:    3,
			wantState: HealthStateRed,
			wantKind:  "creates",
		},
		{
			name:      "offline — unknown, never green",
			entry:     func() RegistryEntry { e := base(4); e.Online = false; return e }(),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    3,
			wantState: HealthStateUnknown,
			wantKind:  "creates",
		},
		{
			name:      "legacy spoke (Known==0) — unknown, never green",
			entry:     withActivity(base(4), ractivity("o/r", recent, recent, "", "")),
			rollup:    agentFleetRollup{Expected: 2, Running: 2, Known: 0},
			app:       okApp(),
			queued:    3,
			wantState: HealthStateUnknown,
			wantKind:  "creates",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := hiveHealthFor(tc.entry, tc.rollup, tc.app, tc.queued, now)
			if v.State != tc.wantState {
				t.Errorf("state = %q, want %q (reason: %q)", v.State, tc.wantState, v.Reason)
			}
			if v.OutputKind != tc.wantKind {
				t.Errorf("outputKind = %q, want %q", v.OutputKind, tc.wantKind)
			}
		})
	}
}

func TestSanitizeRepoActivity(t *testing.T) {
	// nil in → nil out
	if got := sanitizeRepoActivity(nil); got != nil {
		t.Errorf("nil in should be nil out, got %+v", got)
	}
	// all-junk rows collapse to nil (not an empty "reported zero" summary)
	junk := []RepoActivityWire{{Repo: "   "}}
	if got := sanitizeRepoActivity(junk); got != nil {
		t.Errorf("all-junk should be nil, got %+v", got)
	}
	// negative counts clamp to 0; bad NewestAt → ""; good row survives
	in := []RepoActivityWire{{
		Repo:   "org/name",
		Issues: ActivityStatWire{Count: -5, NewestAt: "not-a-time"},
		PRs:    ActivityStatWire{Count: 3, NewestAt: "2026-08-22T10:00:00Z"},
	}}
	got := sanitizeRepoActivity(in)
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].Issues.Count != 0 || got[0].Issues.NewestAt != "" {
		t.Errorf("negative count / bad ts not sanitized: %+v", got[0].Issues)
	}
	if got[0].PRs.Count != 3 || got[0].PRs.NewestAt == "" {
		t.Errorf("good PR stat mangled: %+v", got[0].PRs)
	}
}
