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
			// not idle inventory. A placeholder is detected by its "hosted-available-"
			// ID prefix OR "available-" org prefix, NOT by an empty Org (an unclaimed
			// slot keeps a leftover pool org). Here: 2 assigned (1 up), 3 available.
			name: "placeholders count as available, not assigned",
			hives: []RegistryEntry{
				{ID: "h1", IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(3)},
				{ID: "h2", IsPublic: false, Online: false, Org: "b", Repos: []string{"r2"}, PRsMerged90d: ptrInt(1)},
				{ID: "hosted-available-oke-01-placeholder-aa01", Online: true, Org: "available-pool"}, // both markers, online
				{ID: "hosted-available-oke-02-placeholder-aa02", Online: false, Org: ""},              // ID marker only, offline
				{ID: "regular-id", Online: true, Org: "available-pool"},                               // org marker only, online
			},
			want: FleetStats{ReposManaged: 1, PRsMerged: 3, Hives: 1, TotalHives: 2, AvailableHives: 3, Reporting: 1, Eligible: 1},
		},
		{
			// THE BUG: an unclaimed slot keeps a leftover REAL pool org (live
			// example id="hosted-available-oke-01-placeholder-bb95",
			// org="TradingAsBuddies"). The old Org=="" check missed it entirely —
			// counting it as assigned and inflating hives-up/total. The
			// "hosted-available-" ID prefix still marks it available.
			name: "placeholder with leftover real org is still available",
			hives: []RegistryEntry{
				{ID: "h1", IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(9)},
				{ID: "hosted-available-oke-01-placeholder-bb95", Online: true, Org: "TradingAsBuddies", Repos: []string{"leftover/repo"}},
			},
			want: FleetStats{ReposManaged: 1, PRsMerged: 9, Hives: 1, TotalHives: 1, AvailableHives: 1, Reporting: 1, Eligible: 1},
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
	old := registryPath
	registryPath = t.TempDir() + "/reg.json"
	t.Cleanup(func() { registryPath = old })
	return &HubServer{logger: slog.Default()}
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
	data, err := os.ReadFile(registryPath)
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
