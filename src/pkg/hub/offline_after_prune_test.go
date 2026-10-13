package hub

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tests for #11644: an assigned hive whose registry entry was pruned after
// staleRemoveAge must keep its offline alert and say "offline since …" on its
// row, using the went_offline time persisted in its timeline.

func TestEvaluateAlerts_OfflineSinceAfterPrune(t *testing.T) {
	tests := []struct {
		name          string
		online        bool
		lastHeartbeat string
		offlineSince  string
		want          bool
	}{
		{
			name:         "pruned hive offline past the threshold alerts",
			offlineSince: rfc3339(fixedNow.Add(-30 * time.Hour)),
			want:         true,
		},
		{
			name:         "pruned hive offline under the threshold does not alert",
			offlineSince: rfc3339(fixedNow.Add(-alertOfflineThreshold + time.Second)),
			want:         false,
		},
		{
			name:         "unparseable offlineSince does not alert",
			offlineSince: "not-a-timestamp",
			want:         false,
		},
		{
			name:         "no heartbeat and no offlineSince does not alert",
			offlineSince: "",
			want:         false,
		},
		{
			name:         "online hive never alerts",
			online:       true,
			offlineSince: rfc3339(fixedNow.Add(-30 * time.Hour)),
			want:         false,
		},
		{
			name:          "a live heartbeat wins over offlineSince",
			lastHeartbeat: rfc3339(fixedNow.Add(-time.Minute)),
			offlineSince:  rfc3339(fixedNow.Add(-30 * time.Hour)),
			want:          false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := baseHive("h1")
			h.Online = tc.online
			h.LastHeartbeat = tc.lastHeartbeat
			h.OfflineSince = tc.offlineSince
			got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
			a, ok := findAlert(got.Alerts, "h1", AlertTypeOffline)
			if ok != tc.want {
				t.Fatalf("offline alert fired = %v, want %v (alerts: %+v)", ok, tc.want, got.Alerts)
			}
			if !ok {
				return
			}
			if a.Severity != AlertSeverityCritical {
				t.Errorf("severity = %q, want %q", a.Severity, AlertSeverityCritical)
			}
			if !strings.Contains(a.Reason, "Offline since "+tc.offlineSince) || !strings.Contains(a.Reason, "30h0m0s") {
				t.Errorf("reason = %q, want it to name the offline-since time and duration", a.Reason)
			}
		})
	}
}

func TestAlertHiveFromEntryCarriesOfflineSince(t *testing.T) {
	ts := rfc3339(fixedNow.Add(-30 * time.Hour))
	got := alertHiveFromEntry(MyHiveEntry{RegistryEntry: RegistryEntry{ID: "h1"}, OfflineSince: ts})
	if got.OfflineSince != ts {
		t.Errorf("OfflineSince = %q, want %q", got.OfflineSince, ts)
	}
}

func TestTimelineOfflineSince(t *testing.T) {
	const (
		t1 = "2026-10-11T18:24:00Z"
		t2 = "2026-10-12T09:00:00Z"
	)
	tests := []struct {
		name   string
		events []TimelineEvent
		want   string
	}{
		{name: "no events", want: ""},
		{
			name:   "went offline",
			events: []TimelineEvent{{TS: t1, Kind: TimelineWentOffline}},
			want:   t1,
		},
		{
			name: "came back online after going offline",
			events: []TimelineEvent{
				{TS: t1, Kind: TimelineWentOffline},
				{TS: t2, Kind: TimelineCameOnline},
			},
			want: "",
		},
		{
			name: "newest went_offline wins, unrelated events skipped",
			events: []TimelineEvent{
				{TS: t1, Kind: TimelineWentOffline},
				{TS: t1, Kind: TimelineCameOnline},
				{TS: t2, Kind: TimelineWentOffline},
				{TS: t2, Kind: TimelineHealthChanged},
			},
			want: t2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTimelineStore()
			ts.byID["h1"] = tc.events
			if got := ts.offlineSince("h1"); got != tc.want {
				t.Errorf("offlineSince = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrunedOfflineSince(t *testing.T) {
	const went = "2026-10-11T18:24:00Z"
	s := &HubServer{timeline: newTimelineStore()}
	s.timeline.byID["hosted-x"] = []TimelineEvent{{TS: went, Kind: TimelineWentOffline}}

	assigned := MyHiveEntry{
		RegistryEntry: RegistryEntry{ID: "hosted-x", Org: "acme"},
		ProvStatus:    statusAssigned,
	}
	tests := []struct {
		name   string
		server *HubServer
		mutate func(*MyHiveEntry)
		want   string
	}{
		{name: "assigned and pruned", server: s, want: went},
		{name: "running and pruned", server: s, mutate: func(e *MyHiveEntry) { e.ProvStatus = "running" }, want: went},
		{name: "nil timeline", server: &HubServer{}, want: ""},
		{name: "online", server: s, mutate: func(e *MyHiveEntry) { e.Online = true }, want: ""},
		{name: "still has a heartbeat", server: s, mutate: func(e *MyHiveEntry) { e.LastHeartbeat = went }, want: ""},
		{name: "provisioning", server: s, mutate: func(e *MyHiveEntry) { e.ProvStatus = "provisioning" }, want: ""},
		{
			name:   "unassigned placeholder",
			server: s,
			mutate: func(e *MyHiveEntry) { e.ProvStatus = "running"; e.Org = placeholderOrgPrefix + "x" },
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := assigned
			if tc.mutate != nil {
				tc.mutate(&e)
			}
			if got := tc.server.prunedOfflineSince(e); got != tc.want {
				t.Errorf("prunedOfflineSince = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMyHivesKeepsOfflineAlertAfterPrune is the end-to-end shape of the bug:
// an assigned hive with a meta record but NO registry entry must still come
// back from /api/saas/my-hives with offlineSince set and an offline alert.
func TestMyHivesKeepsOfflineAlertAfterPrune(t *testing.T) {
	const owner = "pruned-offline-owner"
	const hiveID = "hosted-pruned-11644"
	const token = "ghp_pruned_offline"

	dir := t.TempDir()
	origUsers, origHives, origTimeline := saasUsersDir, saasHivesDir, timelineDir
	saasUsersDir = dir + "/users"
	saasHivesDir = dir + "/hives"
	timelineDir = dir + "/timeline"
	t.Cleanup(func() { saasUsersDir, saasHivesDir, timelineDir = origUsers, origHives, origTimeline })

	cleanup := helperSetupAuthUser(t, token, owner)
	defer cleanup()

	if err := saveSaaSHive(&SaaSHive{
		ID:          hiveID,
		Owner:       owner,
		Org:         "acme",
		PrimaryRepo: "widget",
		Repos:       []string{"acme/widget"},
		Status:      statusAssigned,
	}); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}

	s := newHubServerForTest(t)
	went := time.Now().Add(-30 * time.Hour).UTC().Format(time.RFC3339)
	s.timeline.mu.Lock()
	s.timeline.byID[hiveID] = []TimelineEvent{{TS: went, Kind: TimelineWentOffline, Detail: "no heartbeat"}}
	s.timeline.mu.Unlock()

	req := httptest.NewRequest("GET", "/api/saas/my-hives", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.handleMyHives(rec, req)
	if rec.Code != 200 {
		t.Fatalf("handleMyHives = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Hives  []MyHiveEntry `json:"hives"`
		Alerts AlertSummary  `json:"alerts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got *MyHiveEntry
	for i := range resp.Hives {
		if resp.Hives[i].ID == hiveID {
			got = &resp.Hives[i]
		}
	}
	if got == nil {
		t.Fatalf("hive %s missing from my-hives response", hiveID)
	}
	if got.OfflineSince != went {
		t.Errorf("offlineSince = %q, want %q", got.OfflineSince, went)
	}
	if !hasAlert(resp.Alerts.Alerts, hiveID, AlertTypeOffline) {
		t.Errorf("no offline alert for pruned assigned hive; alerts: %+v", resp.Alerts.Alerts)
	}
}

// TestDashboardRendersOfflineSince pins the card text for a pruned-but-assigned
// hive: the status dot tooltip and the Version cell say "offline since …"
// rather than "no heartbeat yet".
func TestDashboardRendersOfflineSince(t *testing.T) {
	for _, part := range []string{
		`else if (h.offlineSince) parts.push('offline since: ' + fmtUserTS(h.offlineSince));`,
		`if (!h.lastHeartbeat && h.offlineSince) {`,
		`vmLabel = 'offline since ' + fmtUserTS(h.offlineSince);`,
	} {
		if !strings.Contains(dashboardHTML, part) {
			t.Errorf("dashboard is missing the offline-since rendering %q", part)
		}
	}
}
