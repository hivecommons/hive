package hub

import (
	"encoding/json"
	"testing"
)

// Spokes provisioned before #11432 rendered hub.cluster_id / HIVE_CLUSTER_ID
// never attach cluster_health, and on pull_only clusters the hub cannot
// re-render them. The hub delivers the cluster id it already records.

func TestHeartbeatClusterIDToDeliver(t *testing.T) {
	cases := []struct {
		name, saas, reported, want string
	}{
		{"spoke reports none", "vllm-d", "", "vllm-d"},
		{"spoke reports blank", "vllm-d", "  ", "vllm-d"},
		{"spoke already configured", "vllm-d", "vllm-d", ""},
		{"spoke configured differently is never contradicted", "vllm-d", "other", ""},
		{"no hub record", "", "", ""},
		{"record sanitized", " vllm-d\n", "", "vllm-d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := heartbeatClusterIDToDeliver(tc.saas, tc.reported); got != tc.want {
				t.Fatalf("heartbeatClusterIDToDeliver(%q, %q) = %q, want %q", tc.saas, tc.reported, got, tc.want)
			}
		})
	}
}

func decodeHeartbeatResponse(t *testing.T, s *HubServer, body string) HeartbeatResponse {
	t.Helper()
	rec := postHeartbeat(t, s, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp HeartbeatResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestHeartbeatDeliversClusterIDToSpokeReportingNone(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	const id = "hosted-available-vllmd-260731-nq4a"
	if err := saveSaaSHive(&SaaSHive{ID: id, Status: statusAvailable, ClusterID: "vllm-d"}); err != nil {
		t.Fatal(err)
	}

	resp := decodeHeartbeatResponse(t, s, `{"hive_id":"`+id+`","org":"available-vllmd"}`)
	if resp.ClusterID != "vllm-d" {
		t.Fatalf("cluster_id = %q, want the hub-recorded vllm-d for a spoke reporting none", resp.ClusterID)
	}

	resp = decodeHeartbeatResponse(t, s, `{"hive_id":"`+id+`","org":"available-vllmd","cluster_id":"vllm-d"}`)
	if resp.ClusterID != "" {
		t.Fatalf("cluster_id = %q re-sent to a spoke that already reports one", resp.ClusterID)
	}
}

func TestHeartbeatDoesNotDeliverDefaultClusterIDWithoutRecord(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	resp := decodeHeartbeatResponse(t, s, `{"hive_id":"byo-spoke","org":"acme"}`)
	if resp.ClusterID != "" {
		t.Fatalf("cluster_id = %q delivered to a hive the hub has no record of", resp.ClusterID)
	}
}
