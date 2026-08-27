package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func ptrInt(v int) *int { return &v }

func freshHeartbeat() string { return time.Now().UTC().Format(time.RFC3339) }
func staleHeartbeat() string {
	return time.Now().Add(-maxHeartbeatAge - time.Minute).UTC().Format(time.RFC3339)
}

func TestClampFleetCount(t *testing.T) {
	tests := []struct {
		name string
		in   *int
		want *int
	}{
		{"nil stays nil", nil, nil},
		{"negative becomes nil", ptrInt(-5), nil},
		{"zero preserved", ptrInt(0), ptrInt(0)},
		{"normal preserved", ptrInt(42), ptrInt(42)},
		{"over max clamped", ptrInt(maxFleetCount + 1), ptrInt(maxFleetCount)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampFleetCount(tt.in)
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("nil mismatch: got %v want %v", got, tt.want)
			}
			if got != nil && *got != *tt.want {
				t.Errorf("got %d want %d", *got, *tt.want)
			}
			// Must not alias the caller's pointer.
			if got != nil && got == tt.in {
				t.Error("clampFleetCount returned aliased pointer")
			}
		})
	}
}

func TestComputeFleetStats(t *testing.T) {
	tests := []struct {
		name  string
		hives []RegistryEntry
		want  FleetStats
	}{
		{
			name: "aggregates public online hives, dedupes repos",
			hives: []RegistryEntry{
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r1", "r2"},
					PRsMerged90d: ptrInt(10), PRsRejected90d: ptrInt(2), CVEsClosed: ptrInt(1)},
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r2", "a/r3"},
					PRsMerged90d: ptrInt(5), PRsRejected90d: ptrInt(1), CVEsClosed: ptrInt(3)},
			},
			// repos: a/r1, a/r2, a/r3 = 3 distinct; a/r2 reported twice + as "r2".
			want: FleetStats{ReposManaged: 3, PRsMerged: 15, PRsRejected: 3, CVEsClosed: 4, Hives: 2, TotalHives: 2, Reporting: 2, Eligible: 2},
		},
		{
			// A PRIVATE hive now COUNTS: these totals are anonymous sums, and
			// excluding private hives described about a third of the fleet
			// (~18 of 51 spokes are public). Naming a hive is a different
			// disclosure and is still gated on IsPublic in the registry listing.
			// OFFLINE hives are still skipped — their counts are not current.
			name: "counts private hives, skips offline ones",
			hives: []RegistryEntry{
				{IsPublic: false, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(99)},
				{IsPublic: true, Online: false, Org: "a", Repos: []string{"r2"}, PRsMerged90d: ptrInt(99)},
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r3"}, PRsMerged90d: ptrInt(7)},
			},
			// TotalHives is 3: all three are assigned (Org set); one is offline so
			// it's in the total but not "up" (Hives 2).
			want: FleetStats{ReposManaged: 2, PRsMerged: 106, Hives: 2, TotalHives: 3, Reporting: 2, Eligible: 2},
		},
		{
			name: "nil counts are not aggregated as zero",
			hives: []RegistryEntry{
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}}, // all nil
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r2"}, PRsMerged90d: ptrInt(4)},
			},
			want: FleetStats{ReposManaged: 2, PRsMerged: 4, Hives: 2, TotalHives: 2, Reporting: 1, Eligible: 2},
		},
		{
			name: "empty repo strings are skipped",
			hives: []RegistryEntry{
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"", "r1", ""},
					PRsMerged90d: ptrInt(2)},
			},
			want: FleetStats{ReposManaged: 1, PRsMerged: 2, Hives: 1, TotalHives: 1, Reporting: 1, Eligible: 1},
		},
		{
			// Unassigned placeholders are counted as AVAILABLE and excluded from
			// the assigned totals — "hives up / total" is about the working fleet,
			// not idle inventory. Availability mirrors the dashboard EXACTLY:
			// ProvStatus=="available" OR an "available-" org prefix. The immutable
			// "hosted-available-" ID prefix is deliberately NOT a signal (a claimed
			// placeholder keeps that ID forever). Here: h1 (up) + h2 (down) are
			// assigned; the "available-pool" org rows are available; the ID-only row
			// with a cleared org is a CLAIMED placeholder → assigned, offline.
			name: "placeholders count as available via provStatus/org, not the ID prefix",
			hives: []RegistryEntry{
				{ID: "h1", IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(3)},
				{ID: "h2", IsPublic: false, Online: false, Org: "b", Repos: []string{"r2"}, PRsMerged90d: ptrInt(1)},
				{ID: "hosted-available-oke-01-placeholder-aa01", Online: true, Org: "available-pool"}, // org marker → available
				{ID: "hosted-available-oke-02-placeholder-aa02", Online: false, Org: ""},              // claimed: ID prefix alone is NOT available → assigned, down
				{ID: "regular-id", Online: true, Org: "available-pool"},                               // org marker → available
			},
			// Assigned total = h1 + h2 + the ID-only row (3); assigned & up = h1 (1);
			// available = the two "available-pool" rows (2).
			want: FleetStats{ReposManaged: 1, PRsMerged: 3, Hives: 1, TotalHives: 3, AvailableHives: 2, Reporting: 1, Eligible: 1},
		},
		{
			// A CLAIMED placeholder whose org was rewritten off the pool prefix
			// (live example id="hosted-available-oke-01-placeholder-bb95",
			// org="TradingAsBuddies") is ASSIGNED — its status is not "available"
			// and its org has no "available-" prefix. The "hosted-available-" ID it
			// still carries must NOT resurrect it as available. This is the exact
			// over-count that showed 38 available when only 17 are unclaimed.
			name: "claimed placeholder with rewritten org is assigned, not available via ID prefix",
			hives: []RegistryEntry{
				{ID: "h1", IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(9)},
				{ID: "hosted-available-oke-01-placeholder-bb95", Online: true, Org: "TradingAsBuddies", Repos: []string{"leftover/repo"}, PRsMerged90d: ptrInt(4)},
			},
			// Both assigned & up; the placeholder's rewritten org means 0 available.
			want: FleetStats{ReposManaged: 2, PRsMerged: 13, Hives: 2, TotalHives: 2, AvailableHives: 0, Reporting: 2, Eligible: 2},
		},
		{
			// THE OVER-COUNT BUG, now via the explicit statusAssigned the claim
			// paths set: a CLAIMED placeholder KEEPS its "hosted-available-" ID and
			// a leftover "available-pool" org, but its provStatus is "assigned".
			// Availability keys off provStatus=="available" (false) and the org
			// prefix — and here the claim rewrote neither the ID nor the org, so
			// this case pins that provStatus alone must win: assigned, NOT available.
			name: "claimed placeholder keeps hosted-available ID/org but statusAssigned makes it assigned",
			hives: []RegistryEntry{
				{ID: "h1", IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(4), ProvStatus: statusAssigned},
				{ID: "hosted-available-oke-01-placeholder-cc11", Online: true, Org: "claimed-org",
					Repos: []string{"real/repo"}, PRsMerged90d: ptrInt(6), ProvStatus: statusAssigned},
			},
			want: FleetStats{ReposManaged: 2, PRsMerged: 10, Hives: 2, TotalHives: 2, AvailableHives: 0, Reporting: 2, Eligible: 2},
		},
		{
			// ProvStatus=="available" is authoritative even for a hive whose ID does
			// NOT carry the "hosted-available-" prefix and whose org is not a pool
			// prefix — a placeholder whose ID/org markers alone would miss it. It
			// still counts as AVAILABLE, matching the dashboard's Unassigned section.
			name: "provStatus available is authoritative regardless of ID",
			hives: []RegistryEntry{
				{ID: "h1", IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(5)},
				{ID: "regular-looking-id", Online: true, Org: "some-org", ProvStatus: statusAvailable},
			},
			want: FleetStats{ReposManaged: 1, PRsMerged: 5, Hives: 1, TotalHives: 1, AvailableHives: 1, Reporting: 1, Eligible: 1},
		},
		{
			// Reconciliation shape mirroring the live fleet the bug was found on:
			// 2 hosted-available-* IDs, one still unclaimed (ProvStatus available →
			// AVAILABLE) and one claimed (statusAssigned + org rewritten off the pool
			// prefix → ASSIGNED). The claimed one must NOT be double-counted as
			// available even though its ID prefix matches. Available=1, assigned
			// total=2 (the plain hive + the claimed placeholder).
			name: "mixed hosted-available pool splits by provStatus",
			hives: []RegistryEntry{
				{ID: "h1", IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(2)},
				{ID: "hosted-available-oke-01-placeholder-dd01", Online: true, Org: "available-pool", ProvStatus: statusAvailable},
				{ID: "hosted-available-oke-02-placeholder-dd02", Online: true, Org: "claimed-org",
					Repos: []string{"claimed/repo"}, PRsMerged90d: ptrInt(3), ProvStatus: statusAssigned},
			},
			want: FleetStats{ReposManaged: 2, PRsMerged: 5, Hives: 2, TotalHives: 2, AvailableHives: 1, Reporting: 2, Eligible: 2},
		},
		{
			name:  "no hives yields empty",
			hives: nil,
			want:  FleetStats{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &HubServer{logger: slog.Default()}
			s.registry.Hives = tt.hives
			got := s.computeFleetStats()
			if got != tt.want {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestHandleFleetStats(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(),
			Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(8), CVEsClosed: ptrInt(2)},
		// Stale (old heartbeat) — markStaleHives flips it offline, so excluded.
		{ID: "h2", IsPublic: true, Online: true, LastHeartbeat: staleHeartbeat(),
			Org: "a", Repos: []string{"r2"}, PRsMerged90d: ptrInt(100)},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/fleet-stats", nil)
	s.handleFleetStats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var fs FleetStats
	if err := json.Unmarshal(rec.Body.Bytes(), &fs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fs.PRsMerged != 8 {
		t.Errorf("PRsMerged = %d, want 8 (stale hive must be excluded)", fs.PRsMerged)
	}
	if fs.CVEsClosed != 2 {
		t.Errorf("CVEsClosed = %d, want 2", fs.CVEsClosed)
	}
	if fs.Hives != 1 {
		t.Errorf("Hives = %d, want 1", fs.Hives)
	}
	if fs.ReposManaged != 1 {
		t.Errorf("ReposManaged = %d, want 1", fs.ReposManaged)
	}
	if fs.UpdatedAt == "" {
		t.Error("UpdatedAt empty")
	}
}

// TestComputeFleetStatsAgesOutStaleCollections verifies that a hive whose last
// successful collect is older than fleetStatsMaxAge is excluded from the totals
// and counted as stale. Carrying counts forward across restarts keeps a number
// available, but a frozen number must not masquerade as a current one.
func TestComputeFleetStatsAgesOutStaleCollections(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		{IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"},
			PRsMerged90d: ptrInt(10), FleetStatsCollectedAt: time.Now()},
		{IsPublic: true, Online: true, Org: "a", Repos: []string{"r2"},
			PRsMerged90d:          ptrInt(999),
			FleetStatsCollectedAt: time.Now().Add(-2 * fleetStatsMaxAge)},
	}
	got := s.computeFleetStats()
	if got.PRsMerged != 10 {
		t.Errorf("PRsMerged = %d, want 10 (stale collect must not be summed)", got.PRsMerged)
	}
	if got.Stale != 1 {
		t.Errorf("Stale = %d, want 1", got.Stale)
	}
	if got.Reporting != 1 || got.Eligible != 2 {
		t.Errorf("Reporting/Eligible = %d/%d, want 1/2", got.Reporting, got.Eligible)
	}
}

// TestComputeFleetStatsZeroCollectedAtIsTrusted pins the upgrade-skew guard: a
// spoke too old to report a collection timestamp must still contribute rather
// than being aged out on a zero time.
func TestComputeFleetStatsZeroCollectedAtIsTrusted(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		{IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(5)},
	}
	got := s.computeFleetStats()
	if got.PRsMerged != 5 || got.Reporting != 1 || got.Stale != 0 {
		t.Errorf("got %+v, want PRsMerged=5 Reporting=1 Stale=0", got)
	}
}

// TestComputeFleetStatsContributorsTotal pins the hub-landing-page fix: the
// "Contributors" tile must show the fleet-wide REGISTERED contributor total
// (ContributorsTotal, summed from each spoke's ContributorCount), not the
// currently-active count (Contributors, summed from ActiveContributors) —
// the active count legitimately drops to 0 whenever nobody happens to be
// connected at collection time, which made the tile crater to 0. The two
// totals must be independent: a hive can have many registered contributors
// and zero active ones right now, and vice versa.
func TestComputeFleetStatsContributorsTotal(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		// Registered contributors but nobody active right now — the case that
		// used to crater the tile to 0.
		{IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"},
			ContributorCount: 12, ActiveContributors: 0},
		// Some active, fewer registered visible in this snapshot — still summed
		// independently.
		{IsPublic: true, Online: true, Org: "a", Repos: []string{"r2"},
			ContributorCount: 3, ActiveContributors: 2},
		// Offline hives are excluded from both totals, same as every other
		// fleet-wide count.
		{IsPublic: true, Online: false, Org: "a", Repos: []string{"r3"},
			ContributorCount: 100, ActiveContributors: 100},
	}
	got := s.computeFleetStats()
	if got.ContributorsTotal != 15 {
		t.Errorf("ContributorsTotal = %d, want 15 (12+3, offline hive excluded)", got.ContributorsTotal)
	}
	if got.Contributors != 2 {
		t.Errorf("Contributors = %d, want 2 (active count unaffected by the fix)", got.Contributors)
	}
}

// TestFleetStatsTrustworthy is the regression guard for the reported bug: a
// total assembled from a small minority of the fleet must NOT be presented as
// a fleet-wide figure. 2 reporting out of 50 eligible is the live shape of the
// "statistics are missing again" report.
// TestFleetStatsTrustworthy pins the contract AFTER the coverage gate was
// removed: a total is publishable whenever at least one hive reported it.
//
// The previous version of this test called "2 of 50" the reported regression
// and required false — it encoded SUPPRESSION as the fix. That was wrong. These
// totals are a sum of facts, not a sample: each hive counts its own merged PRs
// and the hub adds them (repos deduplicated via a set), so 2 of 50 hives
// reporting 12,819 PRs is exactly what those two did, not a guess at a fleet
// figure. Hiding it made the page read "this fleet did nothing", the one
// interpretation that is false.
//
// Coverage is now communicated instead of gated: the landing page always states
// "N of M hives reporting" beside the number.
func TestFleetStatsTrustworthy(t *testing.T) {
	tests := []struct {
		name      string
		reporting int
		eligible  int
		want      bool
		why       string
	}{
		{"2 of 50 is a real count, not a bad estimate", 2, 50, true,
			"publishing it with its coverage beats a blank strip that implies zero activity"},
		{"a single reporting hive still has real data", 1, 40, true,
			"one hive's merged PRs are still merged PRs"},
		{"nothing eligible", 0, 0, false, "no hives, no total"},
		{"eligible but none reporting", 0, 12, false,
			"nothing to publish — this is the ONLY case that hides the strip"},
		{"whole fleet reporting", 50, 50, true, "complete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := FleetStats{Reporting: tt.reporting, Eligible: tt.eligible}
			if got := fs.FleetStatsTrustworthy(); got != tt.want {
				t.Errorf("Trustworthy(%d/%d) = %v, want %v — %s",
					tt.reporting, tt.eligible, got, tt.want, tt.why)
			}
		})
	}
}

// TestFleetStatsLKGNoLongerNeedsAThresholdToInitialise pins the trap that kept
// the strip blank for months.
//
// The last-known-good aggregate was RECORDED only while Trustworthy was true,
// and SERVED only while it was false. On a fleet that never once cleared the
// 50% bar it was therefore never written, so the fallback built for exactly
// that situation had nothing to serve: the safety net could only be filled by
// the condition it existed to protect against. Live coverage was 2/18, 7/18 and
// 10/50 on the day this was found.
func TestFleetStatsLKGNoLongerNeedsAThresholdToInitialise(t *testing.T) {
	// The historical coverage levels that could never record an LKG before.
	for _, c := range []struct{ reporting, eligible int }{{2, 18}, {7, 18}, {10, 50}} {
		fs := FleetStats{Reporting: c.reporting, Eligible: c.eligible}
		if !fs.FleetStatsTrustworthy() {
			t.Errorf("%d of %d still cannot publish or record an LKG; the strip stays blank forever",
				c.reporting, c.eligible)
		}
	}
}

// TestComputeFleetStatsSkipsRepolessHives ensures placeholder hives (no repos)
// are not counted as eligible — they have nothing to contribute and must not
// drag the reporting fraction down as though data were missing.
func TestComputeFleetStatsSkipsRepolessHives(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		{IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(3)},
		{IsPublic: true, Online: true, Org: "a", Repos: nil},
		{IsPublic: true, Online: true, Org: "a", Repos: []string{}},
	}
	got := s.computeFleetStats()
	if got.Eligible != 1 || got.Reporting != 1 {
		t.Errorf("Reporting/Eligible = %d/%d, want 1/1", got.Reporting, got.Eligible)
	}
	if !got.FleetStatsTrustworthy() {
		t.Error("a fully-reporting fleet of one must be trustworthy")
	}
}

// fleetStatsLKGTestServer builds a hub whose registry save path is redirected
// into a temp dir, so recordFleetStatsLKG's requestSave cannot touch /data.
func fleetStatsLKGTestServer(t *testing.T) *HubServer {
	t.Helper()
	// Per-server field, not the registryPath global: mutating the global races
	// leaked saveLoop goroutines from servers built by other tests.
	return &HubServer{logger: slog.Default(), registryPath: t.TempDir() + "/reg.json"}
}

func decodeFleetStats(t *testing.T, s *HubServer) FleetStats {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleFleetStats(rec, httptest.NewRequest(http.MethodGet, "/api/fleet-stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var fs FleetStats
	if err := json.Unmarshal(rec.Body.Bytes(), &fs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return fs
}

// A collection that clears the coverage bar must be recorded as last-known-good
// and served as fresh (not stale).
func TestHandleFleetStats_TrustworthyRecordsLKG(t *testing.T) {
	s := fleetStatsLKGTestServer(t)
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(),
			Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(20)},
		{ID: "h2", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(),
			Org: "a", Repos: []string{"r2"}, PRsMerged90d: ptrInt(6)},
	}

	fs := decodeFleetStats(t, s)
	if !fs.Trustworthy {
		t.Fatalf("Trustworthy = false, want true (2 of 2 reporting)")
	}
	if fs.StaleData {
		t.Error("StaleData = true, want false for a fresh trustworthy aggregate")
	}
	if fs.PRsMerged != 26 {
		t.Errorf("PRsMerged = %d, want 26", fs.PRsMerged)
	}
	if fs.AsOf != fs.UpdatedAt {
		t.Errorf("AsOf = %q, want it to equal UpdatedAt %q when fresh", fs.AsOf, fs.UpdatedAt)
	}
	lkg := s.fleetStatsLKG()
	if lkg == nil {
		t.Fatal("fleetStatsLKG() = nil, want the trustworthy aggregate recorded")
	}
	if lkg.PRsMerged != 26 || lkg.Reporting != 2 || lkg.Eligible != 2 {
		t.Errorf("LKG = %+v, want PRsMerged 26 / Reporting 2 / Eligible 2", *lkg)
	}
}

// Once the coverage gate was removed, LIVE data always wins: a partial total is
// a real count of what the reporting hives did, so replacing it with a cached
// figure would substitute older numbers for current ones.
//
// This test previously asserted the opposite — that 1-of-4 coverage should swap
// the live total for the LKG and label it stale. That was a consequence of the
// gate, not a goal: it treated a real partial count as unusable. The LKG now
// covers only the case where NOTHING is reporting and there is nothing live to
// show.
func TestHandleFleetStats_LiveDataWinsOverLKG(t *testing.T) {
	s := fleetStatsLKGTestServer(t)
	collectedAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	s.registry.FleetStatsLKG = &FleetStatsSnapshot{
		ReposManaged: 42, PRsMerged: 26, PRsRejected: 3, CVEsClosed: 1,
		Hives: 18, Reporting: 12, Eligible: 18, CollectedAt: collectedAt,
	}
	// Live fleet: only 1 of 4 eligible hives reporting → 25%, below the bar.
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(),
			Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(2)},
		{ID: "h2", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(), Org: "a", Repos: []string{"r2"}},
		{ID: "h3", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(), Org: "a", Repos: []string{"r3"}},
		{ID: "h4", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(), Org: "a", Repos: []string{"r4"}},
	}

	fs := decodeFleetStats(t, s)
	if !fs.Trustworthy {
		t.Fatal("Trustworthy = false; a real count from 1 of 4 hives is publishable")
	}
	if fs.StaleData {
		t.Error("StaleData = true; live data was available and must not be replaced by the cache")
	}
	// Counts come from the LIVE hive, not the cache. The cached 26 is older; a
	// real count of 2 from the hives reporting right now is the current truth.
	if fs.PRsMerged != 2 {
		t.Errorf("PRsMerged = %d, want the live 2 (not the cached 26)", fs.PRsMerged)
	}
	if fs.ReposManaged != 4 {
		t.Errorf("ReposManaged = %d, want the live 4 (not the cached 42)", fs.ReposManaged)
	}
	// Coverage is reported alongside so the page can state "1 of 4 reporting".
	if fs.Reporting != 1 || fs.Eligible != 4 {
		t.Errorf("Reporting/Eligible = %d/%d, want live 1/4", fs.Reporting, fs.Eligible)
	}
	// AsOf is NOW, because these are live numbers, not a cached snapshot.
	if fs.AsOf == collectedAt.Format(time.RFC3339) {
		t.Errorf("AsOf = %q, the cached timestamp; live data must carry its own", fs.AsOf)
	}
	// And a publishable aggregate now REFRESHES the cache. Under the old gate
	// this could never happen below 50%, which is why the fallback stayed empty
	// on a fleet that never cleared the bar.
	if lkg := s.fleetStatsLKG(); lkg == nil || lkg.PRsMerged != 2 {
		t.Errorf("LKG was not refreshed from the live aggregate: %+v", lkg)
	}
}

// When NOTHING is reporting but eligible hives exist AND a last-known-good
// aggregate was stored, the handler serves the cached counts and labels them
// stale — the strip keeps a real (older) number rather than going blank on a
// fleet that has merely gone quiet. This exercises the LKG-serving branch that
// only fires at zero live coverage.
func TestHandleFleetStats_ZeroReportingServesStaleLKG(t *testing.T) {
	s := fleetStatsLKGTestServer(t)
	collectedAt := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	s.registry.FleetStatsLKG = &FleetStatsSnapshot{
		ReposManaged: 42, PRsMerged: 26, PRsRejected: 3, CVEsClosed: 1,
		Hives: 5, Reporting: 5, Eligible: 5, CollectedAt: collectedAt,
	}
	// Eligible (online, has repos) but NOT reporting (no counts at all), so
	// Reporting==0 while Eligible>0 — the exact state the LKG fallback covers.
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(), Org: "a", Repos: []string{"r1"}},
		{ID: "h2", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(), Org: "a", Repos: []string{"r2"}},
	}

	fs := decodeFleetStats(t, s)
	if fs.Trustworthy {
		t.Fatal("Trustworthy = true, want false when nothing is reporting")
	}
	if !fs.StaleData {
		t.Error("StaleData = false, want true when serving the cached aggregate")
	}
	// The served counts come from the cache, not the (empty) live aggregate.
	if fs.PRsMerged != 26 || fs.ReposManaged != 42 {
		t.Errorf("served PRsMerged/ReposManaged = %d/%d, want cached 26/42", fs.PRsMerged, fs.ReposManaged)
	}
	// AsOf reflects the cache's collection time, not now.
	if fs.AsOf != collectedAt.Format(time.RFC3339) {
		t.Errorf("AsOf = %q, want the cached collection time %q", fs.AsOf, collectedAt.Format(time.RFC3339))
	}
	// Live coverage figures are kept so the page can show recollection progress.
	if fs.Reporting != 0 || fs.Eligible != 2 {
		t.Errorf("Reporting/Eligible = %d/%d, want live 0/2", fs.Reporting, fs.Eligible)
	}
}

// Genuine first boot — never collected, nothing cached — is the only state
// that legitimately shows no total.
func TestHandleFleetStats_NoLKGShowsNothing(t *testing.T) {
	s := fleetStatsLKGTestServer(t)
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(), Org: "a", Repos: []string{"r1"}},
		{ID: "h2", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(), Org: "a", Repos: []string{"r2"}},
	}
	fs := decodeFleetStats(t, s)
	if fs.Trustworthy || fs.StaleData {
		t.Errorf("Trustworthy=%v StaleData=%v, want both false on first boot", fs.Trustworthy, fs.StaleData)
	}
	if fs.PRsMerged != 0 || fs.AsOf != "" {
		t.Errorf("PRsMerged=%d AsOf=%q, want 0 and empty with no cache", fs.PRsMerged, fs.AsOf)
	}
}

// The LKG must round-trip through the registry file, or a hub restart would
// blank the strip again — the durability half of the fix.
func TestFleetStatsLKG_PersistsAcrossReload(t *testing.T) {
	s := fleetStatsLKGTestServer(t)
	collectedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	s.registry.FleetStatsLKG = &FleetStatsSnapshot{
		PRsMerged: 26, Reporting: 12, Eligible: 18, CollectedAt: collectedAt,
	}
	if err := s.saveRegistryNow(); err != nil {
		t.Fatalf("saveRegistryNow: %v", err)
	}
	var reloaded Registry
	data, err := os.ReadFile(s.registryPath)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reloaded.FleetStatsLKG == nil {
		t.Fatal("FleetStatsLKG did not survive the registry round-trip")
	}
	if reloaded.FleetStatsLKG.PRsMerged != 26 {
		t.Errorf("PRsMerged = %d, want 26", reloaded.FleetStatsLKG.PRsMerged)
	}
	if !reloaded.FleetStatsLKG.CollectedAt.Equal(collectedAt) {
		t.Errorf("CollectedAt = %v, want %v", reloaded.FleetStatsLKG.CollectedAt, collectedAt)
	}
}

// ---------------------------------------------------------------------------
// Org dedupe of the org-scoped contribution counters.
//
// FleetStatsCollector counts AI-author PRs across a whole ORG, so every hive in
// that org reports the org's entire output as its own. Summing them multiplies
// one org's work by its hive count into a PUBLIC landing-page figure. These
// tests pin that a group contributes once, and — the positive controls — that
// dedupe does not simply collapse the whole fleet into one contribution.
// ---------------------------------------------------------------------------

// coHive builds a reporting hive in the given (org, ai-author) counting group.
// Repos must be unique per hive or the eligibility gate skips it; they do not
// affect the PR/CVE counters under test.
func coHive(id, org, author string, merged int) RegistryEntry {
	return RegistryEntry{
		ID: id, IsPublic: true, Online: true, Org: org, AIAuthor: author,
		Repos: []string{org + "/" + id}, PRsMerged90d: ptrInt(merged),
		FleetStatsCollectedAt: time.Now(),
	}
}

// TestFleetStatsCollapsesSameOrgCounts is the live bug: three hives in one org
// each reported the org's whole 3746 merged PRs and the public total showed
// 11238. It must show 3746.
func TestFleetStatsCollapsesSameOrgCounts(t *testing.T) {
	// The value measured on the live 62-hive fleet, reported identically by
	// three hives sharing one org.
	const sharedOrgMerged = 3746

	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		coHive("h1", "acme", "ai-bot", sharedOrgMerged),
		coHive("h2", "acme", "ai-bot", sharedOrgMerged),
		coHive("h3", "acme", "ai-bot", sharedOrgMerged),
	}

	got := s.computeFleetStats()
	if got.PRsMerged != sharedOrgMerged {
		t.Errorf("PRsMerged = %d, want %d (one org's output counted once, not %d times)",
			got.PRsMerged, sharedOrgMerged, len(s.registry.Hives))
	}
	// Positive control on the counterfactual: without dedupe this would be
	// 11238, so the assertion above is actually testing something.
	if got.PRsMerged == sharedOrgMerged*len(s.registry.Hives) {
		t.Errorf("PRsMerged = %d, which is exactly the tripled value — dedupe did not run",
			got.PRsMerged)
	}
	// Coverage counters stay per-hive: all three DID report, and collapsing
	// them would understate how much of the fleet is collecting successfully.
	if got.Reporting != 3 || got.Eligible != 3 {
		t.Errorf("Reporting/Eligible = %d/%d, want 3/3 (coverage is per-hive, not per-org)",
			got.Reporting, got.Eligible)
	}
}

// TestFleetStatsSumsDistinctOrgsIndependently is the positive control for the
// test above: a dedupe that collapsed EVERYTHING to one contribution would pass
// the same-org test and be badly wrong. Different orgs, and same org with
// different AI authors, are genuinely distinct work and must still add up.
func TestFleetStatsSumsDistinctOrgsIndependently(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		coHive("h1", "acme", "ai-bot", 100),
		coHive("h2", "globex", "ai-bot", 20),
		// Same ORG as h1 but a different AI author — two teams in one org
		// running different agents produce genuinely distinct output, so the
		// group key is (org, author) and these must not collapse together.
		coHive("h3", "acme", "other-bot", 3),
	}

	got := s.computeFleetStats()
	if want := 123; got.PRsMerged != want {
		t.Errorf("PRsMerged = %d, want %d (distinct (org, author) groups must sum independently)",
			got.PRsMerged, want)
	}
}

// TestFleetStatsGroupTakesMax pins that same-org hives whose collectors ran at
// slightly different moments collapse to the LARGEST count, not the first seen
// and not the smallest — over a fixed trailing window the freshest collect is
// the largest, so the most complete measurement should stand rather than
// collection order deciding the public number.
func TestFleetStatsGroupTakesMax(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		coHive("h1", "acme", "ai-bot", 3740),
		coHive("h2", "acme", "ai-bot", 3746),
		coHive("h3", "acme", "ai-bot", 3742),
	}

	got := s.computeFleetStats()
	if want := 3746; got.PRsMerged != want {
		t.Errorf("PRsMerged = %d, want %d (group must collapse to its max)", got.PRsMerged, want)
	}
}

// TestFleetStatsDedupeKeepsNilOutOfTotals guards the nil-vs-zero discipline the
// *int counters exist for. A hive that never computed a counter must stay
// invisible to it — dedupe must not materialize a group entry worth zero, and
// must not let a nil sibling suppress a real value.
func TestFleetStatsDedupeKeepsNilOutOfTotals(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		// Reports merged only; CVEs never computed.
		coHive("h1", "acme", "ai-bot", 50),
		// Same group, reports NOTHING for merged but does report CVEs. Its nil
		// must not become a participating zero that could win the max, and its
		// presence must not erase h1's 50.
		{ID: "h2", IsPublic: true, Online: true, Org: "acme", AIAuthor: "ai-bot",
			Repos: []string{"acme/r2"}, CVEsClosed: ptrInt(7),
			FleetStatsCollectedAt: time.Now()},
	}

	got := s.computeFleetStats()
	if got.PRsMerged != 50 {
		t.Errorf("PRsMerged = %d, want 50 (a nil sibling must not suppress a real value)", got.PRsMerged)
	}
	if got.CVEsClosed != 7 {
		t.Errorf("CVEsClosed = %d, want 7", got.CVEsClosed)
	}
	// Positive control: a group with NO reported value for a counter must
	// contribute nothing to it rather than a fabricated zero. PRsRejected90d is
	// nil on both hives here, so the total must be zero AND that zero must come
	// from absence, which the Reporting pair distinguishes.
	if got.PRsRejected != 0 {
		t.Errorf("PRsRejected = %d, want 0 (neither hive reported it)", got.PRsRejected)
	}
	if got.Reporting != 2 {
		t.Errorf("Reporting = %d, want 2 (both hives reported SOME counter)", got.Reporting)
	}
}

// TestFleetStatsStaleGroupStillExcluded pins that dedupe did not smuggle a
// stale count past the ageing rule. Staleness is evaluated PER HIVE before the
// group is formed, so a group is represented by its FRESHEST surviving members
// and a group with no fresh member contributes nothing at all.
func TestFleetStatsStaleGroupStillExcluded(t *testing.T) {
	stale := time.Now().Add(-2 * fleetStatsMaxAge)

	t.Run("wholly stale group contributes nothing", func(t *testing.T) {
		s := &HubServer{logger: slog.Default()}
		h1 := coHive("h1", "acme", "ai-bot", 3746)
		h1.FleetStatsCollectedAt = stale
		h2 := coHive("h2", "acme", "ai-bot", 3746)
		h2.FleetStatsCollectedAt = stale
		s.registry.Hives = []RegistryEntry{h1, h2}

		got := s.computeFleetStats()
		if got.PRsMerged != 0 {
			t.Errorf("PRsMerged = %d, want 0 (every member of the group is stale)", got.PRsMerged)
		}
		if got.Stale != 2 || got.Reporting != 0 {
			t.Errorf("Stale/Reporting = %d/%d, want 2/0", got.Stale, got.Reporting)
		}
	})

	// Positive control: one fresh member is enough to speak for the group, and
	// a stale sibling's LARGER count must not ride in on the group's max — that
	// is precisely how dedupe could have re-admitted aged-out data.
	t.Run("fresh member represents group and stale sibling does not inflate it", func(t *testing.T) {
		s := &HubServer{logger: slog.Default()}
		fresh := coHive("h1", "acme", "ai-bot", 10)
		aged := coHive("h2", "acme", "ai-bot", 999)
		aged.FleetStatsCollectedAt = stale
		s.registry.Hives = []RegistryEntry{fresh, aged}

		got := s.computeFleetStats()
		if got.PRsMerged != 10 {
			t.Errorf("PRsMerged = %d, want 10 (stale sibling must not win the group max)", got.PRsMerged)
		}
		if got.Stale != 1 || got.Reporting != 1 {
			t.Errorf("Stale/Reporting = %d/%d, want 1/1", got.Stale, got.Reporting)
		}
	})
}

// TestFleetStatsUngroupedHivesCountIndividually pins the safe default: a hive
// whose org or AI author is unknown cannot be SHOWN to duplicate anyone, so it
// counts on its own. Collapsing all unknowns into one bucket would silently
// delete real work from the public total.
func TestFleetStatsUngroupedHivesCountIndividually(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		// No AIAuthor -> ungrouped, even though the org matches.
		{ID: "h1", IsPublic: true, Online: true, Org: "acme", Repos: []string{"acme/r1"},
			PRsMerged90d: ptrInt(5), FleetStatsCollectedAt: time.Now()},
		{ID: "h2", IsPublic: true, Online: true, Org: "acme", Repos: []string{"acme/r2"},
			PRsMerged90d: ptrInt(7), FleetStatsCollectedAt: time.Now()},
	}

	got := s.computeFleetStats()
	if want := 12; got.PRsMerged != want {
		t.Errorf("PRsMerged = %d, want %d (unknown author means ungrouped, count each once)",
			got.PRsMerged, want)
	}
}

// TestFleetStatsDedupeAppliesToAllThreeCounters pins that the fix covers
// PRsRejected90d and CVEsClosed too, not just the counter the bug was spotted
// on. CVEsClosed has a different window (all-time, and a free-text "CVE-"
// search) but it is collected by the same org-scoped query, so it multiplies
// identically and is deduped identically.
func TestFleetStatsDedupeAppliesToAllThreeCounters(t *testing.T) {
	withAll := func(id string, merged, rejected, cves int) RegistryEntry {
		e := coHive(id, "acme", "ai-bot", merged)
		e.PRsRejected90d = ptrInt(rejected)
		e.CVEsClosed = ptrInt(cves)
		return e
	}
	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		withAll("h1", 100, 20, 5),
		withAll("h2", 100, 20, 5),
	}

	got := s.computeFleetStats()
	if got.PRsMerged != 100 || got.PRsRejected != 20 || got.CVEsClosed != 5 {
		t.Errorf("merged/rejected/cves = %d/%d/%d, want 100/20/5 (all three counters dedupe)",
			got.PRsMerged, got.PRsRejected, got.CVEsClosed)
	}
}
