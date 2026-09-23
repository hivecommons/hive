package hub

import (
	"encoding/json"
	"testing"
)

func TestDashboardHostSupplementalCases(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{name: "host without scheme is not a URL host", rawURL: "hive.example.com", want: ""},
		{name: "whitespace only is empty", rawURL: "   ", want: ""},
		{name: "strips user info and port", rawURL: "https://operator@HIVE.EXAMPLE.COM:9443/dashboard", want: "hive.example.com"},
		{name: "ipv6 literal host", rawURL: "http://[2001:db8::1]:8080/api", want: "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dashboardHost(tt.rawURL); got != tt.want {
				t.Fatalf("dashboardHost(%q) = %q, want %q", tt.rawURL, got, tt.want)
			}
		})
	}
}

func TestHeartbeatPayloadRunsRoundTripOptional(t *testing.T) {
	raw, err := json.Marshal(HeartbeatPayload{HiveID: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" || !json.Valid(raw) {
		t.Fatalf("invalid json: %s", raw)
	}
	var legacy HeartbeatPayload
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Runs != nil {
		t.Fatalf("legacy runs = %+v, want nil", legacy.Runs)
	}

	active, waiting := 3, 2
	oldest := int64(7200)
	completed := "2026-09-22T10:00:00Z"
	withRuns := HeartbeatPayload{HiveID: "v6", Runs: &RunsSummary{
		Active: &active, WaitingOnHuman: &waiting, OldestWaitSeconds: &oldest, LastStageCompletedAt: &completed,
	}}
	raw, err = json.Marshal(withRuns)
	if err != nil {
		t.Fatal(err)
	}
	var got HeartbeatPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Runs == nil || got.Runs.Active == nil || *got.Runs.Active != active ||
		got.Runs.WaitingOnHuman == nil || *got.Runs.WaitingOnHuman != waiting ||
		got.Runs.OldestWaitSeconds == nil || *got.Runs.OldestWaitSeconds != oldest ||
		got.Runs.LastStageCompletedAt == nil || *got.Runs.LastStageCompletedAt != completed {
		t.Fatalf("runs round trip = %+v", got.Runs)
	}
}
