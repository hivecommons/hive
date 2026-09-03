package hub

import (
	"strings"
	"testing"
	"time"
)

// driftNow is the fixed instant every table-driven case is evaluated at, so
// "3 hours ago" in a fixture means the same thing on every run.
var driftNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// healthyEntry is a claimed hive with NOTHING wrong: online, fresh heartbeat,
// on the fleet's branch and SHA, App installed, agents running, ACMM set,
// riding a rolling tag. Every case below starts from this and breaks exactly
// one thing, so a signal firing can only be attributed to that change.
func healthyEntry(id string) MyHiveEntry {
	return MyHiveEntry{
		RegistryEntry: RegistryEntry{
			ID:            id,
			Org:           "kubestellar",
			ACMMLevel:     2,
			AgentCount:    3,
			LastHeartbeat: rfc(driftNow.Add(-1 * time.Minute)),
			Online:        true,
			GitHash:       "abc1234",
			GitBranch:     "v2",
			ImageRef:      "ghcr.io/hivecommons/hive:v2-latest",
			Health:        map[string]any{"status": "ok"},
		},
		Role:       "owner",
		ProvStatus: "claimed",
	}
}

// normFleet is an eligible-sized fleet all on v2/abc1234, so computeFleetNorm
// yields a real norm and relative signals are actually exercised.
func normFleet() []MyHiveEntry {
	return []MyHiveEntry{healthyEntry("a"), healthyEntry("b"), healthyEntry("c")}
}

func driftKindsOf(r DriftReport) map[string]DriftSignal {
	out := make(map[string]DriftSignal, len(r.Signals))
	for _, s := range r.Signals {
		out[s.Kind] = s
	}
	return out
}

func TestComputeDriftSignals(t *testing.T) {
	norm := computeFleetNorm(normFleet())
	if norm.Branch != "v2" {
		t.Fatalf("test setup: expected norm branch v2, got %q", norm.Branch)
	}
	latest := map[string]string{"v2": "abc1234"}

	tests := []struct {
		name string
		// mutate breaks exactly one thing on a healthy hive.
		mutate func(*MyHiveEntry)
		// wantKind is the signal expected to fire ("" = expect no drift).
		wantKind string
		wantSev  DriftSeverity
		// wantReasonHas is a substring the reason must carry, so the test
		// pins the SPECIFICS (which branch, which check) not just the kind.
		wantReasonHas string
	}{
		{
			name:   "no drift on a healthy claimed hive",
			mutate: func(h *MyHiveEntry) {},
		},
		{
			name: "heartbeat stale",
			mutate: func(h *MyHiveEntry) {
				h.LastHeartbeat = rfc(driftNow.Add(-3 * time.Hour))
			},
			wantKind:      DriftKindHeartbeatStale,
			wantSev:       DriftCritical,
			wantReasonHas: "3 hours",
		},
		{
			name: "heartbeat never sent yields no stale signal",
			mutate: func(h *MyHiveEntry) {
				h.LastHeartbeat = ""
			},
		},
		{
			name: "malformed heartbeat timestamp yields no stale signal",
			mutate: func(h *MyHiveEntry) {
				h.LastHeartbeat = "not-a-time"
			},
		},
		{
			name: "pinned immutable sha tag",
			mutate: func(h *MyHiveEntry) {
				h.ImageRef = "ghcr.io/hivecommons/hive:63d8902"
			},
			wantKind:      DriftKindPinnedImage,
			wantSev:       DriftCritical,
			wantReasonHas: "63d8902",
		},
		{
			name: "digest pin is also not a rolling tag",
			mutate: func(h *MyHiveEntry) {
				h.ImageRef = "ghcr.io/hivecommons/hive@sha256:" + strings.Repeat("d", 64)
			},
			wantKind: DriftKindPinnedImage,
			wantSev:  DriftCritical,
		},
		{
			name: "unknown image ref is not reported as pinned",
			mutate: func(h *MyHiveEntry) {
				h.ImageRef = ""
			},
		},
		{
			name: "registry port is not mistaken for a tag",
			mutate: func(h *MyHiveEntry) {
				h.ImageRef = "registry.internal:5000/hivecommons/hive:v2-latest"
			},
		},
		{
			name: "github app required but not installed",
			mutate: func(h *MyHiveEntry) {
				h.GitHubAppRequired = true
			},
			wantKind:      DriftKindAppMissing,
			wantSev:       DriftCritical,
			wantReasonHas: "not installed",
		},
		{
			name: "github app installed with insufficient permissions",
			mutate: func(h *MyHiveEntry) {
				h.GitHubAppRequired = true
				h.GitHubAppPermIssue = "missing contents:write"
			},
			wantKind:      DriftKindAppPermIssue,
			wantSev:       DriftCritical,
			wantReasonHas: "missing contents:write",
		},
		{
			name: "health degraded names the failing check",
			mutate: func(h *MyHiveEntry) {
				h.Health = map[string]any{
					"status": "degraded",
					"checks": []any{
						map[string]any{"name": "ready", "status": "pass"},
						map[string]any{"name": "github_auth", "status": "fail", "detail": "token error"},
					},
				}
			},
			wantKind:      DriftKindHealthDegraded,
			wantSev:       DriftWarn,
			wantReasonHas: "github_auth (fail: token error)",
		},
		{
			name: "health critical escalates severity",
			mutate: func(h *MyHiveEntry) {
				h.Health = map[string]any{
					"status": "critical",
					"checks": []any{map[string]any{"name": "agents", "status": "fail"}},
				}
			},
			wantKind:      DriftKindHealthDegraded,
			wantSev:       DriftCritical,
			wantReasonHas: "agents (fail)",
		},
		{
			name: "health degraded with no check data still explains itself",
			mutate: func(h *MyHiveEntry) {
				h.Health = map[string]any{"status": "degraded"}
			},
			wantKind:      DriftKindHealthDegraded,
			wantSev:       DriftWarn,
			wantReasonHas: "no failing check",
		},
		{
			name: "malformed health checks do not panic and yield no names",
			mutate: func(h *MyHiveEntry) {
				h.Health = map[string]any{
					"status": "degraded",
					"checks": "not-an-array",
				}
			},
			wantKind:      DriftKindHealthDegraded,
			wantSev:       DriftWarn,
			wantReasonHas: "no failing check",
		},
		{
			name: "upgrade stuck past the stale timeout",
			mutate: func(h *MyHiveEntry) {
				h.Upgrading = true
				h.UpgradeTarget = "v2-latest"
				h.UpgradeStartedAt = driftNow.Add(-2 * time.Hour)
			},
			wantKind:      DriftKindUpgradeStuck,
			wantSev:       DriftCritical,
			wantReasonHas: "v2-latest",
		},
		{
			name: "upgrade actively in flight raises the informational upgrading signal",
			mutate: func(h *MyHiveEntry) {
				h.Upgrading = true
				h.UpgradeTarget = "v2-latest"
				h.UpgradeStartedAt = driftNow.Add(-1 * time.Minute)
			},
			wantKind:      DriftKindUpgrading,
			wantSev:       DriftInfo,
			wantReasonHas: "v2-latest",
		},
		{
			// The exactly-one-signal assertion below doubles as the guard that
			// a stuck or stamp-less upgrade never ALSO reads as "upgrading":
			// the two upgrade-stuck cases in this table would fail with two
			// signals if the new kind leaked past the staleness bound.
			name: "upgrade with no recorded start time is a warning",
			mutate: func(h *MyHiveEntry) {
				h.Upgrading = true
			},
			wantKind:      DriftKindUpgradeStuck,
			wantSev:       DriftWarn,
			wantReasonHas: "no recorded start time",
		},
		{
			// A reported failure is terminal, not in-flight: even if the
			// Upgrading flag and a fresh stamp linger on the row, a hive that
			// failed its upgrade must not show as making progress.
			name: "failed upgrade is not 'upgrading'",
			mutate: func(h *MyHiveEntry) {
				h.Upgrading = true
				h.UpgradeTarget = "v2-latest"
				h.UpgradeStartedAt = driftNow.Add(-1 * time.Minute)
				h.UpgradeFailed = true
				h.UpgradeFailedAt = driftNow.Add(-30 * time.Second)
			},
		},
		{
			name: "acmm unset on a claimed hive",
			mutate: func(h *MyHiveEntry) {
				h.ACMMLevel = 0
			},
			wantKind:      DriftKindACMMUnset,
			wantSev:       DriftWarn,
			wantReasonHas: "ACMM level is unset",
		},
		{
			name: "zero agents on a claimed hive",
			mutate: func(h *MyHiveEntry) {
				h.AgentCount = 0
			},
			wantKind:      DriftKindNoAgents,
			wantSev:       DriftWarn,
			wantReasonHas: "No agents",
		},
		{
			name: "branch differs from the fleet norm",
			mutate: func(h *MyHiveEntry) {
				h.GitBranch = "v3"
			},
			wantKind:      DriftKindBranchMismatch,
			wantSev:       DriftWarn,
			wantReasonHas: "Running branch v3",
		},
		{
			name: "version behind the branch latest",
			mutate: func(h *MyHiveEntry) {
				h.GitHash = "0000fff"
			},
			wantKind:      DriftKindVersionBehind,
			wantSev:       DriftInfo,
			wantReasonHas: "abc1234",
		},
		{
			name: "long git hash matching the short latest is not behind",
			mutate: func(h *MyHiveEntry) {
				h.GitHash = "abc1234deadbeef"
			},
		},
		{
			name: "offline hive is not judged on version",
			mutate: func(h *MyHiveEntry) {
				h.Online = false
				h.GitHash = "0000fff"
				h.GitBranch = "v3"
				// Heartbeat is still fresh, so the stale signal stays quiet and
				// the case isolates the relative-signal suppression.
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := healthyEntry("subject")
			tc.mutate(&h)
			got := computeDrift(h, norm, latest, driftNow)

			if tc.wantKind == "" {
				if got.Count != 0 || len(got.Signals) != 0 {
					t.Fatalf("expected no drift, got %d signals: %+v", got.Count, got.Signals)
				}
				if got.WorstSeverity != "" {
					t.Fatalf("expected empty WorstSeverity with no signals, got %q", got.WorstSeverity)
				}
				return
			}

			byKind := driftKindsOf(got)
			sig, ok := byKind[tc.wantKind]
			if !ok {
				t.Fatalf("expected signal %q, got %+v", tc.wantKind, got.Signals)
			}
			if sig.Severity != tc.wantSev {
				t.Errorf("severity: want %q, got %q", tc.wantSev, sig.Severity)
			}
			if tc.wantReasonHas != "" && !strings.Contains(sig.Reason, tc.wantReasonHas) {
				t.Errorf("reason %q does not contain %q", sig.Reason, tc.wantReasonHas)
			}
			if got.Count != len(got.Signals) {
				t.Errorf("Count %d != len(Signals) %d", got.Count, len(got.Signals))
			}
			// Exactly one thing was broken, so exactly one signal must fire —
			// this is what catches a new signal accidentally double-flagging.
			if len(got.Signals) != 1 {
				t.Errorf("expected exactly 1 signal for a single-fault fixture, got %+v", got.Signals)
			}
		})
	}
}

// Placeholders legitimately have no App, no agents and ACMM 0. Flagging them
// would bury real signals under pool noise — the reason the model scopes those
// three signals to claimed hives.
func TestComputeDriftSkipsClaimedOnlySignalsForPlaceholders(t *testing.T) {
	norm := computeFleetNorm(normFleet())
	latest := map[string]string{"v2": "abc1234"}

	for _, tc := range []struct {
		name  string
		setup func(*MyHiveEntry)
	}{
		{"provStatus available", func(h *MyHiveEntry) { h.ProvStatus = statusAvailable }},
		{"available- org prefix", func(h *MyHiveEntry) { h.ProvStatus = ""; h.Org = "available-pool-7" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := healthyEntry("slot")
			h.GitHubAppRequired = true
			h.ACMMLevel = 0
			h.AgentCount = 0
			h.GitBranch = "v3"
			h.GitHash = "0000fff"
			tc.setup(&h)

			got := computeDrift(h, norm, latest, driftNow)
			for _, s := range got.Signals {
				switch s.Kind {
				case DriftKindAppMissing, DriftKindAppPermIssue, DriftKindACMMUnset,
					DriftKindNoAgents, DriftKindBranchMismatch, DriftKindVersionBehind:
					t.Errorf("placeholder was flagged with claimed-only signal %q: %s", s.Kind, s.Reason)
				}
			}
		})
	}
}

// A placeholder still runs a real spoke, so universal signals must still fire.
func TestComputeDriftStillFlagsPlaceholderUniversalSignals(t *testing.T) {
	h := healthyEntry("slot")
	h.ProvStatus = statusAvailable
	h.LastHeartbeat = rfc(driftNow.Add(-2 * time.Hour))
	h.ImageRef = "ghcr.io/hivecommons/hive:63d8902"

	got := computeDrift(h, fleetNorm{}, nil, driftNow)
	byKind := driftKindsOf(got)
	if _, ok := byKind[DriftKindHeartbeatStale]; !ok {
		t.Errorf("stale heartbeat must be flagged on placeholders too: %+v", got.Signals)
	}
	if _, ok := byKind[DriftKindPinnedImage]; !ok {
		t.Errorf("pinned image must be flagged on placeholders too: %+v", got.Signals)
	}
}

// While a hive is ACTIVELY upgrading, its version-behind and branch-mismatch
// signals are suppressed: mid-upgrade drift from the fleet is the expected
// state the upgrade exists to resolve, and the whole point of the "Upgrading"
// pill is separating "being fixed" from "needs attention". The moment the
// upgrade goes stuck (past staleUpgradeTimeout) the suppression lifts and the
// relative drift is flagged again.
func TestComputeDriftUpgradingSuppressesFleetRelativeSignals(t *testing.T) {
	norm := computeFleetNorm(normFleet())
	latest := map[string]string{"v2": "abc1234"}

	// Baseline: the same divergences WITHOUT an upgrade in flight do flag, so
	// the suppression assertions below cannot pass vacuously.
	behind := healthyEntry("behind")
	behind.GitHash = "0000fff"
	if got := driftKindsOf(computeDrift(behind, norm, latest, driftNow)); got[DriftKindVersionBehind] == (DriftSignal{}) {
		t.Fatalf("test setup: expected version-behind without an upgrade, got %+v", got)
	}
	offBranch := healthyEntry("off-branch")
	offBranch.GitBranch = "v3"
	if got := driftKindsOf(computeDrift(offBranch, norm, latest, driftNow)); got[DriftKindBranchMismatch] == (DriftSignal{}) {
		t.Fatalf("test setup: expected branch-mismatch without an upgrade, got %+v", got)
	}

	// Same divergences with an ACTIVE upgrade: only "upgrading" fires.
	for _, tc := range []struct {
		name       string
		mutate     func(*MyHiveEntry)
		suppressed string
	}{
		{"version-behind suppressed mid-upgrade", func(h *MyHiveEntry) { h.GitHash = "0000fff" }, DriftKindVersionBehind},
		{"branch-mismatch suppressed mid-upgrade", func(h *MyHiveEntry) { h.GitBranch = "v3" }, DriftKindBranchMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := healthyEntry("subject")
			tc.mutate(&h)
			h.Upgrading = true
			h.UpgradeTarget = "v2-latest"
			h.UpgradeStartedAt = driftNow.Add(-1 * time.Minute)

			got := computeDrift(h, norm, latest, driftNow)
			byKind := driftKindsOf(got)
			if _, ok := byKind[tc.suppressed]; ok {
				t.Errorf("%s must be suppressed while actively upgrading: %+v", tc.suppressed, got.Signals)
			}
			if _, ok := byKind[DriftKindUpgrading]; !ok {
				t.Errorf("expected the upgrading signal, got %+v", got.Signals)
			}
			if len(got.Signals) != 1 {
				t.Errorf("expected exactly the upgrading signal, got %+v", got.Signals)
			}
		})
	}

	// A STUCK upgrade does not suppress: past the bound the hive is back to
	// "needs attention", relative drift included.
	stuck := healthyEntry("stuck")
	stuck.GitHash = "0000fff"
	stuck.Upgrading = true
	stuck.UpgradeTarget = "v2-latest"
	stuck.UpgradeStartedAt = driftNow.Add(-2 * time.Hour)
	byKind := driftKindsOf(computeDrift(stuck, norm, latest, driftNow))
	if _, ok := byKind[DriftKindUpgradeStuck]; !ok {
		t.Errorf("expected upgrade-stuck on a wedged upgrade, got %+v", byKind)
	}
	if _, ok := byKind[DriftKindVersionBehind]; !ok {
		t.Errorf("a stuck upgrade must not keep suppressing version-behind, got %+v", byKind)
	}
	if _, ok := byKind[DriftKindUpgrading]; ok {
		t.Errorf("a stuck upgrade must not read as 'upgrading', got %+v", byKind)
	}
}

func TestComputeDriftWorstSeverityWins(t *testing.T) {
	h := healthyEntry("mixed")
	h.ACMMLevel = 0                                 // warn
	h.AgentCount = 0                                // warn
	h.ImageRef = "ghcr.io/hivecommons/hive:63d8902" // critical

	got := computeDrift(h, fleetNorm{}, nil, driftNow)
	if got.WorstSeverity != DriftCritical {
		t.Errorf("want critical, got %q from %+v", got.WorstSeverity, got.Signals)
	}
	if got.Count != 3 {
		t.Errorf("want 3 signals, got %d: %+v", got.Count, got.Signals)
	}
}

func TestComputeFleetNorm(t *testing.T) {
	tests := []struct {
		name       string
		hives      []MyHiveEntry
		wantBranch string
		wantSHA    string
	}{
		{
			name:  "empty fleet has no norm",
			hives: nil,
		},
		{
			name:  "below the minimum sample has no norm",
			hives: []MyHiveEntry{healthyEntry("a"), healthyEntry("b")},
		},
		{
			name:       "modal branch and sha",
			hives:      normFleet(),
			wantBranch: "v2",
			wantSHA:    "abc1234",
		},
		{
			name: "minority branch does not become the norm",
			hives: func() []MyHiveEntry {
				f := normFleet()
				f[0].GitBranch = "v3"
				f[0].GitHash = "9999999"
				return f
			}(),
			wantBranch: "v2",
			wantSHA:    "abc1234",
		},
		{
			name: "offline hives do not vote",
			hives: func() []MyHiveEntry {
				// Three v3 hives, all offline, plus three online v2 hives.
				var f []MyHiveEntry
				for _, id := range []string{"x", "y", "z"} {
					h := healthyEntry(id)
					h.Online = false
					h.GitBranch = "v3"
					f = append(f, h)
				}
				return append(f, normFleet()...)
			}(),
			wantBranch: "v2",
			wantSHA:    "abc1234",
		},
		{
			name: "placeholders do not vote",
			hives: func() []MyHiveEntry {
				var f []MyHiveEntry
				for _, id := range []string{"p1", "p2", "p3", "p4"} {
					h := healthyEntry(id)
					h.ProvStatus = statusAvailable
					h.GitBranch = "pool"
					f = append(f, h)
				}
				return append(f, normFleet()...)
			}(),
			wantBranch: "v2",
			wantSHA:    "abc1234",
		},
		{
			name: "sha norm is scoped to the norm branch",
			hives: func() []MyHiveEntry {
				f := normFleet()
				// A v3 hive with a different SHA must not drag the v2 SHA norm.
				odd := healthyEntry("odd")
				odd.GitBranch = "v3"
				odd.GitHash = "fffffff"
				return append(f, odd)
			}(),
			wantBranch: "v2",
			wantSHA:    "abc1234",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeFleetNorm(tc.hives)
			if got.Branch != tc.wantBranch {
				t.Errorf("branch: want %q, got %q", tc.wantBranch, got.Branch)
			}
			if got.SHA != tc.wantSHA {
				t.Errorf("sha: want %q, got %q", tc.wantSHA, got.SHA)
			}
		})
	}
}

// A tie must resolve the same way every request; a norm that flickered would
// flag half the fleet on alternating page loads.
func TestComputeFleetNormTieIsDeterministic(t *testing.T) {
	build := func() []MyHiveEntry {
		var f []MyHiveEntry
		for i, br := range []string{"v2", "v2", "v3", "v3"} {
			h := healthyEntry(string(rune('a' + i)))
			h.GitBranch = br
			f = append(f, h)
		}
		return f
	}
	first := computeFleetNorm(build()).Branch
	for i := 0; i < 20; i++ {
		if got := computeFleetNorm(build()).Branch; got != first {
			t.Fatalf("tie-break not deterministic: %q then %q", first, got)
		}
	}
	if first != "v2" {
		t.Errorf("tie should break to the lexically smaller branch, got %q", first)
	}
}

func TestAnnotateDrift(t *testing.T) {
	hives := normFleet()
	hives = append(hives, func() MyHiveEntry {
		h := healthyEntry("bad")
		h.GitHubAppRequired = true
		return h
	}())

	annotateDrift(hives, map[string]string{"v2": "abc1234"}, driftNow)

	for _, h := range hives[:3] {
		if h.Drift.Count != 0 {
			t.Errorf("healthy hive %s got drift: %+v", h.ID, h.Drift.Signals)
		}
	}
	last := hives[len(hives)-1]
	if last.Drift.Count != 1 || last.Drift.WorstSeverity != DriftCritical {
		t.Errorf("bad hive: want 1 critical signal, got %+v", last.Drift)
	}

	// Nil/empty input must not panic.
	annotateDrift(nil, nil, driftNow)
	annotateDrift([]MyHiveEntry{}, nil, driftNow)
}

// When the hub has not resolved a latest SHA for the branch, version drift
// falls back to the fleet's modal SHA rather than going silent.
func TestComputeDriftVersionFallsBackToFleetModalSHA(t *testing.T) {
	norm := computeFleetNorm(normFleet())
	h := healthyEntry("behind")
	h.GitHash = "0000fff"

	got := computeDrift(h, norm, nil /* no latest_shas */, driftNow)
	sig, ok := driftKindsOf(got)[DriftKindVersionBehind]
	if !ok {
		t.Fatalf("expected version drift, got %+v", got.Signals)
	}
	if !strings.Contains(sig.Reason, "most of the fleet") {
		t.Errorf("expected fleet-modal wording, got %q", sig.Reason)
	}
}

// With no derivable norm, relative signals must be silent rather than guessed.
func TestComputeDriftSkipsRelativeSignalsWithoutNorm(t *testing.T) {
	h := healthyEntry("lonely")
	h.GitBranch = "some-feature-branch"
	h.GitHash = "0000fff"

	got := computeDrift(h, fleetNorm{}, map[string]string{"v2": "abc1234"}, driftNow)
	for _, s := range got.Signals {
		if s.Kind == DriftKindBranchMismatch || s.Kind == DriftKindVersionBehind {
			t.Errorf("relative signal %q fired with no fleet norm: %s", s.Kind, s.Reason)
		}
	}
}

func TestImageTagOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"ghcr.io/hivecommons/hive:v2-latest", "v2-latest"},
		{"ghcr.io/hivecommons/hive:63d8902", "63d8902"},
		{"ghcr.io/hivecommons/hive", ""},
		{"registry.internal:5000/hivecommons/hive", ""},
		{"registry.internal:5000/hivecommons/hive:v2-latest", "v2-latest"},
		{"ghcr.io/hivecommons/hive@sha256:" + strings.Repeat("a", 64), ""},
		{"ghcr.io/hivecommons/hive:v2-latest@sha256:" + strings.Repeat("a", 64), "v2-latest"},
	}
	for _, tc := range tests {
		if got := imageTagOf(tc.in); got != tc.want {
			t.Errorf("imageTagOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHealthFailingChecks(t *testing.T) {
	tests := []struct {
		name   string
		health map[string]any
		want   []string
	}{
		{"nil health", nil, nil},
		{"no checks key", map[string]any{"status": "ok"}, nil},
		{"checks wrong type", map[string]any{"checks": 42}, nil},
		{"checks entry wrong type", map[string]any{"checks": []any{"oops"}}, nil},
		{
			name: "passing and skipped are not failures",
			health: map[string]any{"checks": []any{
				map[string]any{"name": "a", "status": "pass"},
				map[string]any{"name": "b", "status": "skip"},
			}},
			want: nil,
		},
		{
			name: "fail and warn are reported with detail",
			health: map[string]any{"checks": []any{
				map[string]any{"name": "a", "status": "fail", "detail": "boom"},
				map[string]any{"name": "b", "status": "warn"},
			}},
			want: []string{"a (fail: boom)", "b (warn)"},
		},
		{
			name: "missing name is labelled not dropped",
			health: map[string]any{"checks": []any{
				map[string]any{"status": "fail"},
			}},
			want: []string{"unnamed check (fail)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := healthFailingChecks(tc.health)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Many failing checks must summarize rather than produce an unreadable reason.
func TestComputeDriftTruncatesLongCheckLists(t *testing.T) {
	var checks []any
	for _, n := range []string{"c1", "c2", "c3", "c4", "c5"} {
		checks = append(checks, map[string]any{"name": n, "status": "fail"})
	}
	h := healthyEntry("noisy")
	h.Health = map[string]any{"status": "degraded", "checks": checks}

	got := computeDrift(h, fleetNorm{}, nil, driftNow)
	sig := driftKindsOf(got)[DriftKindHealthDegraded]
	if !strings.Contains(sig.Reason, "and 2 more checks") {
		t.Errorf("expected a truncation summary, got %q", sig.Reason)
	}
	if strings.Contains(sig.Reason, "c4") {
		t.Errorf("reason should not name checks past the cap: %q", sig.Reason)
	}
}

func TestDriftHumanDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{10 * time.Second, "1 min"},
		{1 * time.Minute, "1 min"},
		{45 * time.Minute, "45 min"},
		{1 * time.Hour, "1 hour"},
		{5 * time.Hour, "5 hours"},
		{25 * time.Hour, "1 day"},
		{72 * time.Hour, "3 days"},
	}
	for _, tc := range tests {
		if got := driftHumanDuration(tc.in); got != tc.want {
			t.Errorf("driftHumanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestModalString(t *testing.T) {
	tests := []struct {
		name  string
		in    []string
		want  string
		wantN int
	}{
		{"nil", nil, "", 0},
		{"all empty", []string{"", "  "}, "", 0},
		{"clear winner", []string{"v2", "v2", "v3"}, "v2", 2},
		{"empties are ignored", []string{"", "v3", "v3"}, "v3", 2},
		{"tie breaks lexically", []string{"v3", "v2"}, "v2", 1},
		{"whitespace is trimmed together", []string{" v2 ", "v2"}, "v2", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, n := modalString(tc.in)
			if got != tc.want || n != tc.wantN {
				t.Errorf("modalString(%v) = (%q, %d), want (%q, %d)", tc.in, got, n, tc.want, tc.wantN)
			}
		})
	}
}

func TestIsPlaceholderEntry(t *testing.T) {
	tests := []struct {
		name string
		in   MyHiveEntry
		want bool
	}{
		{"claimed", MyHiveEntry{ProvStatus: "claimed", RegistryEntry: RegistryEntry{Org: "kubestellar"}}, false},
		{"available status", MyHiveEntry{ProvStatus: statusAvailable}, true},
		{"available org prefix", MyHiveEntry{RegistryEntry: RegistryEntry{Org: "available-7"}}, true},
		{"empty entry", MyHiveEntry{}, false},
		{"org merely containing available", MyHiveEntry{RegistryEntry: RegistryEntry{Org: "not-available-here"}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPlaceholderEntry(tc.in); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
