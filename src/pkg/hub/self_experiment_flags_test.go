package hub

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/nous"
)

func TestHeartbeatSelfExperimentAssignmentAndRevert(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	assignment := &nous.SelfFlagAssignment{ExperimentID: "exp-1", Flag: "review-v2", Enabled: true}
	if err := s.SetSelfExperimentFlag("hive-a", assignment); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"hive_id":"hive-a","org":"acme"}`,
		`{"hive_id":"hive-a","org":"acme","self_experiments_opt_in":false}`,
		`{"hive_id":"hive-b","org":"acme","self_experiments_opt_in":true}`,
	} {
		if resp := decodeHeartbeatResponse(t, s, body); resp.SelfExperimentFlag != nil {
			t.Fatalf("assignment leaked to non-opted-in or unassigned hive: %s", body)
		}
	}
	const optedIn = `{"hive_id":"hive-a","org":"acme","self_experiments_opt_in":true}`
	resp := decodeHeartbeatResponse(t, s, optedIn)
	if resp.SelfExperimentFlag == nil || *resp.SelfExperimentFlag != *assignment {
		t.Fatalf("assignment not delivered: %+v", resp.SelfExperimentFlag)
	}
	// Pin both sides of the wire: a hub assignment must decode on the spoke.
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var received spoke.HeartbeatResponse
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatal(err)
	}
	if received.SelfExperimentFlag == nil || *received.SelfExperimentFlag != *assignment {
		t.Fatal("hub/spoke assignment wire mismatch")
	}
	assignment.Enabled = false
	if !s.selfExperimentFlagForHeartbeat("hive-a", true).Enabled {
		t.Fatal("assignment stored by pointer, not by value")
	}
	if err := s.SetSelfExperimentFlag("hive-a", assignment); err != nil {
		t.Fatal(err)
	}
	resp = decodeHeartbeatResponse(t, s, optedIn)
	if resp.SelfExperimentFlag == nil || resp.SelfExperimentFlag.Enabled {
		t.Fatal("next heartbeat did not deliver explicit revert")
	}
	if err := s.SetSelfExperimentFlag("hive-a", nil); err != nil {
		t.Fatal(err)
	}
	if resp = decodeHeartbeatResponse(t, s, optedIn); resp.SelfExperimentFlag != nil {
		t.Fatal("withdrawn flag still delivered")
	}
}

func TestSetSelfExperimentFlagValidation(t *testing.T) {
	s := &HubServer{}
	if err := s.SetSelfExperimentFlag("", nil); err == nil {
		t.Fatal("accepted empty hive ID")
	}
	if err := s.SetSelfExperimentFlag("hive-a", &nous.SelfFlagAssignment{Enabled: true}); err == nil {
		t.Fatal("accepted invalid assignment")
	}
	if got := s.selfExperimentFlagForHeartbeat("hive-a", true); got != nil {
		t.Fatalf("invalid assignment became deliverable: %+v", got)
	}
}
