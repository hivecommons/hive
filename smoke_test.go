package hub

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

// TestSmokeAlertsPayloadShape verifies the JSON the dashboard actually receives:
// key names must match what the JS reads (alerts/countsBySeverity/countsByType).
func TestSmokeAlertsPayloadShape(t *testing.T) {
	s := &HubServer{logger: slog.Default(), alerts: newAlertState()}
	e := MyHiveEntry{ProvStatus: "error", ProvError: "quota exceeded"}
	e.ID = "h1"
	e.Name = "org/h1"
	// An offline hive too, for a second type.
	e2 := MyHiveEntry{}
	e2.ID = "h2"
	e2.Name = "org/h2"
	e2.Online = false
	e2.LastHeartbeat = time.Now().Add(-3 * time.Hour).Format(time.RFC3339)

	summary := s.fleetAlerts([]MyHiveEntry{e, e2})
	b, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("payload: %s", b)

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"alerts", "countsBySeverity", "countsByType", "total"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("payload missing key %q: %s", k, b)
		}
	}
	arr, ok := raw["alerts"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("want 2 alerts, got %v", raw["alerts"])
	}
	first, _ := arr[0].(map[string]any)
	for _, k := range []string{"hiveId", "hiveName", "type", "severity", "reason", "firstSeen"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("alert missing key %q: %v", k, first)
		}
	}
}
