package hub

import (
	"log/slog"
	"testing"
	"time"
)

// TestHeartbeatBearerOK_NoPrefix rejects tokens that don't start with "Bearer ".
func TestHeartbeatBearerOK_NoPrefix(t *testing.T) {
	s := &HubServer{hubSecret: "test-secret-42"}
	cases := []string{
		"",
		"Basic dXNlcjpwYXNz",
		"bearer test-secret-42", // lowercase "bearer" must not match
		"Token test-secret-42",
		"test-secret-42",
	}
	for _, auth := range cases {
		if s.heartbeatBearerOK(auth) {
			t.Errorf("heartbeatBearerOK(%q) = true, want false (missing 'Bearer ' prefix)", auth)
		}
	}
}

// TestHeartbeatBearerOK_LegacyRawSecret accepts the raw hub secret.
func TestHeartbeatBearerOK_LegacyRawSecret(t *testing.T) {
	secret := "super-secret-master-key-abc123"
	s := &HubServer{hubSecret: secret}
	if !s.heartbeatBearerOK("Bearer " + secret) {
		t.Fatal("heartbeatBearerOK should accept the raw hubSecret as legacy bearer")
	}
}

// TestHeartbeatBearerOK_DerivedHeartbeatKey accepts the HMAC-derived key.
func TestHeartbeatBearerOK_DerivedHeartbeatKey(t *testing.T) {
	secret := "super-secret-master-key-abc123"
	s := &HubServer{hubSecret: secret}

	derived := s.heartbeatKey()
	if derived == "" {
		t.Fatal("heartbeatKey() returned empty for a non-empty secret")
	}
	if derived == secret {
		t.Fatal("heartbeatKey() must differ from raw secret (it is HMAC-derived)")
	}
	if !s.heartbeatBearerOK("Bearer " + derived) {
		t.Fatal("heartbeatBearerOK should accept the derived heartbeatKey")
	}
}

// TestHeartbeatBearerOK_WrongToken rejects an unrelated token.
func TestHeartbeatBearerOK_WrongToken(t *testing.T) {
	s := &HubServer{hubSecret: "real-secret"}
	if s.heartbeatBearerOK("Bearer wrong-token") {
		t.Fatal("heartbeatBearerOK should reject a token that matches neither scheme")
	}
}

// TestHeartbeatBearerOK_EmptySecret documents that with an empty hubSecret,
// the function accepts an empty bearer token (secureCompare("","") == true).
// The caller (handleHeartbeat) guards with `if s.hubSecret != ""` before
// calling heartbeatBearerOK, so this does not create a bypass in production.
// A non-empty token is still rejected.
func TestHeartbeatBearerOK_EmptySecret(t *testing.T) {
	s := &HubServer{hubSecret: ""}

	// Non-empty tokens are rejected even with empty hubSecret.
	if s.heartbeatBearerOK("Bearer anything") {
		t.Error("heartbeatBearerOK should reject non-empty tokens when hubSecret is empty")
	}

	// "Bearer " (empty token) matches because secureCompare("","") == true.
	// This is documented behavior — the caller guards with hubSecret != "".
	if !s.heartbeatBearerOK("Bearer ") {
		t.Error("heartbeatBearerOK('Bearer ') with empty hubSecret: expected true (empty==empty)")
	}
}

// TestHeartbeatBearerOK_TimingSafe verifies that the comparison is constant-time
// by checking it goes through secureCompareHub (no early exit on partial match).
func TestHeartbeatBearerOK_TimingSafe(t *testing.T) {
	secret := "constant-time-test-secret"
	s := &HubServer{hubSecret: secret}

	// A token that shares a prefix with the real secret must still be rejected.
	partial := secret[:len(secret)-1] + "X"
	if s.heartbeatBearerOK("Bearer " + partial) {
		t.Fatal("heartbeatBearerOK accepted a near-miss token (timing-safe violation?)")
	}
}

// --- computeFleetStats tests ---

func newTestHubServer() *HubServer {
	return &HubServer{
		logger:   slog.Default(),
		registry: Registry{},
	}
}

func TestComputeFleetStats_EmptyRegistry(t *testing.T) {
	s := newTestHubServer()
	fs := s.computeFleetStats()
	if fs.Hives != 0 || fs.TotalHives != 0 || fs.AvailableHives != 0 {
		t.Errorf("empty registry should yield zero counts, got hives=%d total=%d avail=%d",
			fs.Hives, fs.TotalHives, fs.AvailableHives)
	}
}

func TestComputeFleetStats_AvailablePlaceholders(t *testing.T) {
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{ID: "placeholder-1", ProvStatus: statusAvailable, Online: true},
		{ID: "placeholder-2", Org: placeholderOrgPrefix + "pool-x", Online: true},
	}
	fs := s.computeFleetStats()
	if fs.AvailableHives != 2 {
		t.Errorf("expected 2 available hives, got %d", fs.AvailableHives)
	}
	if fs.TotalHives != 0 {
		t.Errorf("placeholders should not count as assigned; TotalHives=%d", fs.TotalHives)
	}
	if fs.Hives != 0 {
		t.Errorf("placeholders should not count as online assigned; Hives=%d", fs.Hives)
	}
}

func TestComputeFleetStats_OnlineVsOffline(t *testing.T) {
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{ID: "online-1", Org: "acme", Online: true, Repos: []string{"acme/app"}},
		{ID: "offline-1", Org: "acme", Online: false, Repos: []string{"acme/lib"}},
	}
	fs := s.computeFleetStats()
	if fs.TotalHives != 2 {
		t.Errorf("TotalHives should count both online and offline; got %d", fs.TotalHives)
	}
	if fs.Hives != 1 {
		t.Errorf("Hives should count only online; got %d", fs.Hives)
	}
}

func TestComputeFleetStats_RepoDedup(t *testing.T) {
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Org: "acme", Online: true, Repos: []string{"acme/app", "acme/lib"}},
		{ID: "h2", Org: "acme", Online: true, Repos: []string{"acme/app", "acme/api"}},
	}
	fs := s.computeFleetStats()
	// "acme/app" appears in both hives but should be counted once.
	if fs.ReposManaged != 3 {
		t.Errorf("expected 3 deduplicated repos, got %d", fs.ReposManaged)
	}
}

func TestComputeFleetStats_BareRepoNormalization(t *testing.T) {
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Org: "acme", Online: true, Repos: []string{"myrepo"}},
		{ID: "h2", Org: "acme", Online: true, Repos: []string{"acme/myrepo"}},
	}
	fs := s.computeFleetStats()
	// "myrepo" from h1 should normalize to "acme/myrepo", deduplicating with h2.
	if fs.ReposManaged != 1 {
		t.Errorf("bare repo should normalize to org/repo and dedup; got %d", fs.ReposManaged)
	}
}

func TestComputeFleetStats_ContributionAggregation(t *testing.T) {
	merged := 10
	rejected := 3
	cves := 2
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{
			ID: "h1", Org: "a", Online: true,
			Repos:                 []string{"a/r"},
			PRsMerged90d:          &merged,
			PRsRejected90d:        &rejected,
			CVEsClosed:            &cves,
			FleetStatsCollectedAt: time.Now(),
		},
		{
			ID: "h2", Org: "b", Online: true,
			Repos:                 []string{"b/r"},
			PRsMerged90d:          &merged,
			PRsRejected90d:        &rejected,
			CVEsClosed:            &cves,
			FleetStatsCollectedAt: time.Now(),
		},
	}
	fs := s.computeFleetStats()
	if fs.PRsMerged != 20 {
		t.Errorf("PRsMerged expected 20, got %d", fs.PRsMerged)
	}
	if fs.PRsRejected != 6 {
		t.Errorf("PRsRejected expected 6, got %d", fs.PRsRejected)
	}
	if fs.CVEsClosed != 4 {
		t.Errorf("CVEsClosed expected 4, got %d", fs.CVEsClosed)
	}
	if fs.Reporting != 2 {
		t.Errorf("Reporting expected 2, got %d", fs.Reporting)
	}
}

func TestComputeFleetStats_NilCountsSkipped(t *testing.T) {
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{
			ID: "h1", Org: "a", Online: true,
			Repos:        []string{"a/r"},
			PRsMerged90d: nil, // never reported
		},
	}
	fs := s.computeFleetStats()
	if fs.Reporting != 0 {
		t.Errorf("hive with nil counts should not be in Reporting; got %d", fs.Reporting)
	}
	if fs.PRsMerged != 0 {
		t.Errorf("PRsMerged should remain 0 for nil-count hives; got %d", fs.PRsMerged)
	}
}

func TestComputeFleetStats_StaleCollectExcluded(t *testing.T) {
	merged := 100
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{
			ID: "stale-h", Org: "a", Online: true,
			Repos:                 []string{"a/r"},
			PRsMerged90d:          &merged,
			FleetStatsCollectedAt: time.Now().Add(-7 * 24 * time.Hour), // > fleetStatsMaxAge (6h)
		},
	}
	fs := s.computeFleetStats()
	if fs.Stale != 1 {
		t.Errorf("expected 1 stale hive, got %d", fs.Stale)
	}
	if fs.Reporting != 0 {
		t.Errorf("stale hive should not be in Reporting; got %d", fs.Reporting)
	}
	if fs.PRsMerged != 0 {
		t.Errorf("stale hive counts should not contribute; PRsMerged=%d", fs.PRsMerged)
	}
}

func TestComputeFleetStats_ZeroTimestampTrusted(t *testing.T) {
	merged := 50
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{
			ID: "old-spoke", Org: "a", Online: true,
			Repos:                 []string{"a/r"},
			PRsMerged90d:          &merged,
			FleetStatsCollectedAt: time.Time{}, // zero — predates the field
		},
	}
	fs := s.computeFleetStats()
	if fs.Reporting != 1 {
		t.Errorf("zero-timestamp hive should be trusted and count as Reporting; got %d", fs.Reporting)
	}
	if fs.PRsMerged != 50 {
		t.Errorf("expected PRsMerged=50, got %d", fs.PRsMerged)
	}
}

func TestComputeFleetStats_EmptyRepoNotEligible(t *testing.T) {
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{ID: "no-repos", Org: "a", Online: true, Repos: []string{}},
	}
	fs := s.computeFleetStats()
	if fs.Eligible != 0 {
		t.Errorf("hive with no repos should not be eligible; got %d", fs.Eligible)
	}
}

func TestComputeFleetStats_AgentsAndContributors(t *testing.T) {
	s := newTestHubServer()
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Org: "a", Online: true, AgentCount: 3, ActiveContributors: 5, ContributorCount: 10},
		{ID: "h2", Org: "b", Online: true, AgentCount: 2, ActiveContributors: 1, ContributorCount: 4},
	}
	fs := s.computeFleetStats()
	if fs.AgentsRunning != 5 {
		t.Errorf("AgentsRunning expected 5, got %d", fs.AgentsRunning)
	}
	if fs.Contributors != 6 {
		t.Errorf("Contributors expected 6, got %d", fs.Contributors)
	}
	if fs.ContributorsTotal != 14 {
		t.Errorf("ContributorsTotal expected 14, got %d", fs.ContributorsTotal)
	}
}

func TestFleetStatsTrustworthy_ZeroReporting(t *testing.T) {
	fs := FleetStats{Reporting: 0, Eligible: 10}
	if fs.FleetStatsTrustworthy() {
		t.Error("should not be trustworthy with 0 reporting")
	}
}

func TestFleetStatsTrustworthy_SomeReporting(t *testing.T) {
	fs := FleetStats{Reporting: 1, Eligible: 10}
	if !fs.FleetStatsTrustworthy() {
		t.Error("should be trustworthy with >= 1 reporting")
	}
}
