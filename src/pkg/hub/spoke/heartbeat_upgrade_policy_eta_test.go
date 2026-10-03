package spoke

import (
	"encoding/json"
	"testing"
)

// #10256: the hub's next-update ETA decodes from the heartbeat response, and an
// older hub that does not send it leaves the field empty (unknown).
func TestHeartbeatUpgradePolicyDecodesNextUpdateAt(t *testing.T) {
	var resp HeartbeatResponse
	if err := json.Unmarshal([]byte(`{"ok":true,"upgrade_policy":{"hub_managed":true,"channel":"stable","target_resolved":true,"next_update_at":"2026-10-03T13:00:00Z"}}`), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.UpgradePolicy == nil || resp.UpgradePolicy.NextUpdateAt != "2026-10-03T13:00:00Z" {
		t.Fatalf("UpgradePolicy = %+v, want next_update_at decoded", resp.UpgradePolicy)
	}

	var old HeartbeatResponse
	if err := json.Unmarshal([]byte(`{"ok":true,"upgrade_policy":{"hub_managed":true,"channel":"stable","target_resolved":true}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.UpgradePolicy == nil || old.UpgradePolicy.NextUpdateAt != "" {
		t.Fatalf("UpgradePolicy = %+v, want empty NextUpdateAt from an older hub", old.UpgradePolicy)
	}
}
