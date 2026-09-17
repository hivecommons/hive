package hub

import (
	"encoding/json"
	"testing"
)

// #7262: the heartbeat must carry the hub's upgrade posture so the spoke's
// dashboard measures "behind" against the commit the hub will actually roll it
// to, and renders the hub's schedule — not spoke-local defaults.

func TestHeartbeatUpgradePolicyNilForUnmanagedSpoke(t *testing.T) {
	s := &HubServer{logger: targetingLogger()}
	payload := &HeartbeatPayload{HiveID: "bare", GitBranch: "v4", GitHash: "abc1234"}
	if got := s.heartbeatUpgradePolicy(payload, nil, false, false, "v4", ""); got != nil {
		t.Fatalf("policy = %+v, want nil for a spoke with no SaaS record and auto_upgrade off (legacy wire format)", got)
	}
}

func TestHeartbeatUpgradePolicyHubManagedChannelSpoke(t *testing.T) {
	stubBranchHead(t, "v5", "5193426")
	stubChannelRevisions(t, map[string]string{"edge": "6a5b337"})
	s := &HubServer{logger: targetingLogger()}
	saas := &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: "", TrackedChannel: "edge"}
	payload := &HeartbeatPayload{HiveID: "h", GitBranch: "v5", GitHash: "ffe1e19", ImageRef: "ghcr.io/hivecommons/hive:edge", AutoUpgrade: true}

	// spokeManaged=false: the SaaS record with AutoUpgrade overrides the spoke flag.
	got := s.heartbeatUpgradePolicy(payload, saas, false, false, "v5", "6a5b337")
	if got == nil {
		t.Fatal("policy = nil, want a policy for a hub-managed hive")
	}
	if !got.HubManaged || got.SpokeManaged {
		t.Errorf("HubManaged=%v SpokeManaged=%v, want hub-managed only", got.HubManaged, got.SpokeManaged)
	}
	if got.Schedule != AutoUpgradeModeInstant {
		t.Errorf("Schedule = %q, want %q — an empty stored mode is instant, not unknown", got.Schedule, AutoUpgradeModeInstant)
	}
	if got.Branch != "v5" {
		t.Errorf("Branch = %q, want the spoke's own line v5 (never a hard-wired v4)", got.Branch)
	}
	if got.Channel != ReleaseChannelEdge {
		t.Errorf("Channel = %q, want %q", got.Channel, ReleaseChannelEdge)
	}
	if got.TargetSHA != "6a5b337" || !got.TargetResolved {
		t.Errorf("TargetSHA=%q Resolved=%v, want the channel revision 6a5b337 (branch head 5193426 is NOT reachable for a channel spoke)", got.TargetSHA, got.TargetResolved)
	}
	if got.ArmedTarget != "6a5b337" {
		t.Errorf("ArmedTarget = %q, want the armed hbTarget passed in", got.ArmedTarget)
	}
	if got.Paused {
		t.Error("Paused = true, want false")
	}
}

func TestHeartbeatUpgradePolicySpokeManagedBranchSpoke(t *testing.T) {
	stubBranchHead(t, "v4", "526ef71")
	s := &HubServer{logger: targetingLogger()}
	payload := &HeartbeatPayload{HiveID: "h", GitBranch: "v4", GitHash: "abc1234", ImageRef: "ghcr.io/hivecommons/hive:v4-latest", AutoUpgrade: true}

	got := s.heartbeatUpgradePolicy(payload, nil, true, true, "v4", "")
	if got == nil {
		t.Fatal("policy = nil, want a policy for a self-upgrading spoke")
	}
	if got.HubManaged || !got.SpokeManaged {
		t.Errorf("HubManaged=%v SpokeManaged=%v, want spoke-managed only", got.HubManaged, got.SpokeManaged)
	}
	if got.Schedule != AutoUpgradeModeInstant {
		t.Errorf("Schedule = %q, want instant for a self-upgrading spoke", got.Schedule)
	}
	if !got.Paused {
		t.Error("Paused = false, want true — the kill switch state must reach the spoke")
	}
	if got.TargetSHA != "526ef71" || !got.TargetResolved || got.Channel != "" {
		t.Errorf("target = %+v, want branch head 526ef71 for a branch-tracking spoke", got)
	}
}

func TestHeartbeatUpgradePolicyDailyScheduleAndUnresolvedChannel(t *testing.T) {
	stubBranchHead(t, "v4", "526ef71")
	stubChannelRevisions(t, map[string]string{}) // stable does not resolve
	s := &HubServer{logger: targetingLogger()}
	saas := &SaaSHive{AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeDaily, TrackedChannel: "stable"}
	payload := &HeartbeatPayload{HiveID: "h", GitBranch: "v4", GitHash: "abc1234", ImageRef: "ghcr.io/hivecommons/hive:stable"}

	got := s.heartbeatUpgradePolicy(payload, saas, false, false, "v4", "")
	if got == nil {
		t.Fatal("policy = nil")
	}
	if got.Schedule != AutoUpgradeModeDaily {
		t.Errorf("Schedule = %q, want daily", got.Schedule)
	}
	if got.TargetResolved || got.TargetSHA != "" {
		t.Errorf("TargetResolved=%v TargetSHA=%q, want unresolved with no SHA — the spoke must render unknown, not the v4 tip", got.TargetResolved, got.TargetSHA)
	}
	if got.Channel != ReleaseChannelStable {
		t.Errorf("Channel = %q, want stable so the spoke can name it", got.Channel)
	}
}

// A SaaS record WITHOUT auto_upgrade and a spoke that does not self-upgrade
// still gets a policy (the hub has an opinion: nobody upgrades it) so the
// spoke can say so instead of "turned off for this hive" in the local voice.
func TestHeartbeatUpgradePolicySaaSRecordNobodyManages(t *testing.T) {
	stubBranchHead(t, "v4", "526ef71")
	s := &HubServer{logger: targetingLogger()}
	saas := &SaaSHive{AutoUpgrade: false}
	payload := &HeartbeatPayload{HiveID: "h", GitBranch: "v4", GitHash: "abc1234", ImageRef: "ghcr.io/hivecommons/hive:v4-latest"}

	got := s.heartbeatUpgradePolicy(payload, saas, false, false, "v4", "")
	if got == nil {
		t.Fatal("policy = nil, want a policy whenever a SaaS record exists")
	}
	if got.HubManaged || got.SpokeManaged {
		t.Errorf("HubManaged=%v SpokeManaged=%v, want neither", got.HubManaged, got.SpokeManaged)
	}
	if got.Schedule != "" {
		t.Errorf("Schedule = %q, want empty when nobody manages upgrades", got.Schedule)
	}
}

// The wire field is omitted entirely for a nil policy so older spokes see an
// unchanged response, and present (never null) when populated.
func TestHeartbeatResponseUpgradePolicyWireShape(t *testing.T) {
	raw, err := json.Marshal(HeartbeatResponse{OK: true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["upgrade_policy"]; present {
		t.Errorf("upgrade_policy present on a response without a policy: %s", raw)
	}

	raw, err = json.Marshal(HeartbeatResponse{OK: true, UpgradePolicy: &HeartbeatUpgradePolicy{HubManaged: true, Schedule: AutoUpgradeModeWeekly, TargetSHA: "abc1234", TargetResolved: true}})
	if err != nil {
		t.Fatal(err)
	}
	var round HeartbeatResponse
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	if round.UpgradePolicy == nil || !round.UpgradePolicy.HubManaged || round.UpgradePolicy.Schedule != AutoUpgradeModeWeekly || round.UpgradePolicy.TargetSHA != "abc1234" {
		t.Errorf("round-trip lost the policy: %+v", round.UpgradePolicy)
	}
}
