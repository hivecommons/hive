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
			want: FleetStats{ReposManaged: 3, PRsMerged: 15, PRsRejected: 3, CVEsClosed: 4, Hives: 2, Reporting: 2, Eligible: 2},
		},
		{
			name: "skips private and offline hives",
			hives: []RegistryEntry{
				{IsPublic: false, Online: true, Org: "a", Repos: []string{"r1"}, PRsMerged90d: ptrInt(99)},
				{IsPublic: true, Online: false, Org: "a", Repos: []string{"r2"}, PRsMerged90d: ptrInt(99)},
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r3"}, PRsMerged90d: ptrInt(7)},
			},
			want: FleetStats{ReposManaged: 1, PRsMerged: 7, Hives: 1, Reporting: 1, Eligible: 1},
		},
		{
			name: "nil counts are not aggregated as zero",
			hives: []RegistryEntry{
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r1"}}, // all nil
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"r2"}, PRsMerged90d: ptrInt(4)},
			},
			want: FleetStats{ReposManaged: 2, PRsMerged: 4, Hives: 2, Reporting: 1, Eligible: 2},
		},
		{
			name: "empty repo strings are skipped",
			hives: []RegistryEntry{
				{IsPublic: true, Online: true, Org: "a", Repos: []string{"", "r1", ""},
					PRsMerged90d: ptrInt(2)},
			},
			want: FleetStats{ReposManaged: 1, PRsMerged: 2, Hives: 1, Reporting: 1, Eligible: 1},
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
func TestFleetStatsTrustworthy(t *testing.T) {
	tests := []struct {
		name      string
		reporting int
		eligible  int
		want      bool
	}{
		{"the reported regression: 2 of 50", 2, 50, false},
		{"nothing eligible", 0, 0, false},
		{"eligible but none reporting", 0, 12, false},
		{"exactly at the threshold", 5, 10, true},
		{"just under the threshold", 4, 10, false},
		{"whole fleet reporting", 50, 50, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := FleetStats{Reporting: tt.reporting, Eligible: tt.eligible}
			if got := fs.FleetStatsTrustworthy(); got != tt.want {
				t.Errorf("Trustworthy(%d/%d) = %v, want %v",
					tt.reporting, tt.eligible, got, tt.want)
			}
		})
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

// The regression this whole change exists for: once coverage collapses, the
// page must still get NUMBERS (from the cache) plus an honest stale label —
// not a blank strip.
func TestHandleFleetStats_DegradedServesLabelledLKG(t *testing.T) {
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
	if fs.Trustworthy {
		t.Fatal("Trustworthy = true, want false (1 of 4 reporting)")
	}
	if !fs.StaleData {
		t.Error("StaleData = false, want true when serving the cache")
	}
	// Counts come from the cache, NOT from the 1 live hive (which would be 2).
	if fs.PRsMerged != 26 {
		t.Errorf("PRsMerged = %d, want 26 from LKG (not the degraded live 2)", fs.PRsMerged)
	}
	if fs.ReposManaged != 42 {
		t.Errorf("ReposManaged = %d, want 42 from LKG", fs.ReposManaged)
	}
	// Coverage figures stay LIVE so the page can show recollection progress.
	if fs.Reporting != 1 || fs.Eligible != 4 {
		t.Errorf("Reporting/Eligible = %d/%d, want live 1/4", fs.Reporting, fs.Eligible)
	}
	if fs.AsOf != collectedAt.Format(time.RFC3339) {
		t.Errorf("AsOf = %q, want the LKG collection time %q", fs.AsOf, collectedAt.Format(time.RFC3339))
	}
	// A degraded aggregate must never overwrite the cache.
	if lkg := s.fleetStatsLKG(); lkg == nil || lkg.PRsMerged != 26 {
		t.Errorf("degraded aggregate overwrote the LKG: %+v", lkg)
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
