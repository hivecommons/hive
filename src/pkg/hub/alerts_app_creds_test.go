package hub

// Tests for the app-creds-undelivered alert (#4316): a claimed, online hive
// reporting an OPERATOR-SIDE GitHub App credential state must actively alert
// the hub operator, because every owner-facing surface deliberately stands
// down for these states — kelly-headwaters sat Degraded on key-missing for 8
// days with zero signal reaching the only actor who could fix it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// appCredsHive is baseHive plus the App-credential fields under test.
func appCredsHive(id, state string) alertHive {
	h := baseHive(id)
	h.GitHubAppRequired = true
	h.GitHubAppState = state
	h.ClusterID = "oke-frankfurt-1"
	return h
}

func TestEvaluateAlerts_AppCredsUndeliveredRule(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*alertHive)
		want   bool
	}{
		{name: "key-missing on a claimed online hive fires", mutate: func(h *alertHive) {}, want: true},
		{name: "key-invalid fires", mutate: func(h *alertHive) { h.GitHubAppState = appStateKeyInvalidToken }, want: true},
		{name: "no-app-assigned fires", mutate: func(h *alertHive) { h.GitHubAppState = appStateNoAppAssignedToken }, want: true},
		{name: "state token survives surrounding whitespace", mutate: func(h *alertHive) { h.GitHubAppState = "  key-missing  " }, want: true},
		{name: "an owner-side state does not fire", mutate: func(h *alertHive) { h.GitHubAppState = "not-installed" }, want: false},
		{name: "an empty state does not fire", mutate: func(h *alertHive) { h.GitHubAppState = "" }, want: false},
		{name: "a hive that never asked for App auth does not fire", mutate: func(h *alertHive) { h.GitHubAppRequired = false }, want: false},
		{name: "a placeholder does not fire", mutate: func(h *alertHive) { h.IsPlaceholder = true }, want: false},
		{name: "an offline stranded hive still fires", mutate: func(h *alertHive) { h.Online = false }, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := appCredsHive("h1", appStateKeyMissingToken)
			tc.mutate(&h)
			summary := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
			a, got := findAlert(summary.Alerts, "h1", AlertTypeAppCredsUndelivered)
			if got != tc.want {
				t.Fatalf("alert fired = %v, want %v", got, tc.want)
			}
			if got && a.Severity != AlertSeverityCritical {
				t.Errorf("severity = %q, want %q — the hive provably cannot work", a.Severity, AlertSeverityCritical)
			}
		})
	}
}

// TestAppCredsUndelivered_FiresAlongsideOfflineForAStrandedOfflineHive pins
// the deliberate deviation from #4316's wording ("claimed, ONLINE hive"),
// adopted from Danathar's #4322: an offline keyless hive is worst off — still
// keyless AND no longer heartbeating — and the offline alert alone cannot say
// WHY the hive never worked. The two alerts answer different questions and an
// operator needs both.
func TestAppCredsUndelivered_FiresAlongsideOfflineForAStrandedOfflineHive(t *testing.T) {
	h := appCredsHive("h1", appStateKeyMissingToken)
	h.Online = false
	h.LastHeartbeat = rfc3339(fixedNow.Add(-2 * alertOfflineThreshold))

	summary := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
	if !hasAlert(summary.Alerts, "h1", AlertTypeAppCredsUndelivered) {
		t.Error("an offline keyless hive still needs its key uploaded; being offline must not suppress the credential alert")
	}
	if !hasAlert(summary.Alerts, "h1", AlertTypeOffline) {
		t.Error("the offline rule must still fire independently — the two answer different questions")
	}
}

// TestAppCredsUndeliveredReason_CarriesTheRemedy locks the operator-facing
// content: every variant must name the exact PUT endpoint (the remedy was
// undocumented, which is half of how #4316 stayed silent), and the key-missing
// text must say whether the hub even holds a key for the cluster.
func TestAppCredsUndeliveredReason_CarriesTheRemedy(t *testing.T) {
	const remedy = "PUT /api/saas/admin/cluster-app-keys/oke-frankfurt-1"

	tests := []struct {
		name    string
		state   string
		hasKey  bool
		want    []string
		notWant []string
	}{
		{
			name:  "key-missing with no hub key says upload",
			state: appStateKeyMissingToken,
			want:  []string{remedy, "holds no key for oke-frankfurt-1", "only an operator can fix this"},
		},
		{
			name:    "key-missing with a hub key says delivery is stuck, not upload-a-key",
			state:   appStateKeyMissingToken,
			hasKey:  true,
			want:    []string{remedy, "even though the hub holds a key"},
			notWant: []string{"holds no key"},
		},
		{
			name:  "key-invalid says replace",
			state: appStateKeyInvalidToken,
			want:  []string{remedy, "does not match the App", "replace the key"},
		},
		{
			name:  "no-app-assigned says assign",
			state: appStateNoAppAssignedToken,
			want:  []string{remedy, "placeholder app_id", "assign the cluster's App"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := appCredsHive("h1", tc.state)
			h.ClusterHasKey = tc.hasKey
			reason := appCredsUndeliveredReason(h)
			for _, w := range tc.want {
				if !strings.Contains(reason, w) {
					t.Errorf("reason %q does not contain %q", reason, w)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(reason, nw) {
					t.Errorf("reason %q must not contain %q", reason, nw)
				}
			}
		})
	}
}

// TestAppCredsUndeliveredReason_UnknownClusterStillNamesTheEndpoint covers a
// spoke too old (or too wedged) to report its cluster: the remedy must still
// name the endpoint, with the path placeholder the operator has to fill in.
func TestAppCredsUndeliveredReason_UnknownClusterStillNamesTheEndpoint(t *testing.T) {
	h := appCredsHive("h1", appStateKeyMissingToken)
	h.ClusterID = ""
	reason := appCredsUndeliveredReason(h)
	if !strings.Contains(reason, "PUT /api/saas/admin/cluster-app-keys/{clusterID}") {
		t.Errorf("reason %q must still name the upload endpoint", reason)
	}
	if !strings.Contains(reason, "its cluster") {
		t.Errorf("reason %q must not render a blank cluster name", reason)
	}
}

// TestAppCredsUndelivered_DedupesToOneAlertPerHive is the dedupe guarantee:
// re-evaluating the same stranded hive produces exactly one alert with a
// STABLE FirstSeen, so the panel shows one row whose age keeps growing — never
// a fresh-looking alert every poll (8 days of degradation must read as 8 days).
func TestAppCredsUndelivered_DedupesToOneAlertPerHive(t *testing.T) {
	state := newAlertState()
	h := appCredsHive("h1", appStateKeyMissingToken)

	first := evaluateAlerts(state, []alertHive{h}, nil, fixedNow)
	later := evaluateAlerts(state, []alertHive{h}, nil, fixedNow.Add(8*24*time.Hour))

	count := 0
	for _, a := range later.Alerts {
		if a.HiveID == "h1" && a.Type == AlertTypeAppCredsUndelivered {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("alert count = %d, want exactly 1", count)
	}
	a1, _ := findAlert(first.Alerts, "h1", AlertTypeAppCredsUndelivered)
	a2, _ := findAlert(later.Alerts, "h1", AlertTypeAppCredsUndelivered)
	if !a1.FirstSeen.Equal(a2.FirstSeen) {
		t.Errorf("FirstSeen moved from %v to %v while the condition held", a1.FirstSeen, a2.FirstSeen)
	}
}

// TestAppCredsUndelivered_ClearsWhenTheKeyLands is the self-heal guarantee: the
// moment the spoke stops reporting an operator-side state (the key was
// delivered and works), the alert disappears — and its acknowledgement is
// forgotten, so a RE-occurrence alerts again rather than staying silenced.
func TestAppCredsUndelivered_ClearsWhenTheKeyLands(t *testing.T) {
	state := newAlertState()
	stranded := appCredsHive("h1", appStateKeyMissingToken)

	summary := evaluateAlerts(state, []alertHive{stranded}, nil, fixedNow)
	if !hasAlert(summary.Alerts, "h1", AlertTypeAppCredsUndelivered) {
		t.Fatal("precondition: alert must fire while stranded")
	}
	if !state.setAck("h1", AlertTypeAppCredsUndelivered, "op", fixedNow) {
		t.Fatal("precondition: ack must succeed on a live condition")
	}

	// The key lands: the spoke reports App auth healthy.
	healed := appCredsHive("h1", "")
	healed.GitHubAppRequired = true
	summary = evaluateAlerts(state, []alertHive{healed}, nil, fixedNow.Add(time.Minute))
	if hasAlert(summary.Alerts, "h1", AlertTypeAppCredsUndelivered) {
		t.Fatal("alert must clear once the spoke reports a healthy state")
	}

	// It breaks again (key rotated to a wrong one): the old ack must not
	// silence the new occurrence.
	broken := appCredsHive("h1", appStateKeyInvalidToken)
	summary = evaluateAlerts(state, []alertHive{broken}, nil, fixedNow.Add(2*time.Minute))
	a, ok := findAlert(summary.Alerts, "h1", AlertTypeAppCredsUndelivered)
	if !ok {
		t.Fatal("alert must re-fire on a re-occurrence")
	}
	if a.Acknowledged {
		t.Error("a cleared condition's ack must not silence its re-occurrence")
	}
}

// TestFleetAlerts_StampsClusterHasKey verifies the hub-observed half of the
// reason: fleetAlerts consults the per-cluster PEM store so key-missing reads
// "the hub holds no key" only when that is actually true on disk.
func TestFleetAlerts_StampsClusterHasKey(t *testing.T) {
	dir := withTempAppKeyDir(t)
	if err := os.WriteFile(filepath.Join(dir, "with-key.pem"),
		[]byte("-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newTestAlertServer()
	mk := func(id, clusterID string) MyHiveEntry {
		var e MyHiveEntry
		e.ID = id
		e.Name = "org/" + id
		e.Online = true
		e.LastHeartbeat = rfc3339(time.Now())
		e.GitHubAppRequired = true
		e.GitHubAppState = appStateKeyMissingToken
		e.ClusterID = clusterID
		return e
	}

	summary := s.fleetAlerts([]MyHiveEntry{mk("h1", "with-key"), mk("h2", "no-key")})

	withKey, ok := findAlert(summary.Alerts, "h1", AlertTypeAppCredsUndelivered)
	if !ok {
		t.Fatal("h1 must alert")
	}
	if !strings.Contains(withKey.Reason, "even though the hub holds a key") {
		t.Errorf("h1 reason %q must reflect the stored key", withKey.Reason)
	}
	noKey, ok := findAlert(summary.Alerts, "h2", AlertTypeAppCredsUndelivered)
	if !ok {
		t.Fatal("h2 must alert")
	}
	if !strings.Contains(noKey.Reason, "holds no key for no-key") {
		t.Errorf("h2 reason %q must say the hub holds no key", noKey.Reason)
	}
}

// TestAppCredsUndeliveredAlertHasADashboardLabel mirrors the failed-upgrade
// guard: without a label the chip renders the raw key 'app-creds-undelivered'.
func TestAppCredsUndeliveredAlertHasADashboardLabel(t *testing.T) {
	if !strings.Contains(dashboardHTML, "'"+AlertTypeAppCredsUndelivered+"': 'App credentials undelivered'") {
		t.Errorf("ALERT_TYPE_LABELS has no entry for %q", AlertTypeAppCredsUndelivered)
	}
}
