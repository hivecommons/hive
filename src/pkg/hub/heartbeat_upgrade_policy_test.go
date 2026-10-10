package hub

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"
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
	if got.ScheduleHour != 13 || got.ScheduleTimezone != "America/New_York" || got.ScheduleWeekday != "" {
		t.Errorf("schedule details = hour %d timezone %q weekday %q, want 13 America/New_York and no weekday", got.ScheduleHour, got.ScheduleTimezone, got.ScheduleWeekday)
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

// #10256: a stable-channel hive learns when the hub expects the next
// promotion into stable — the same soak deadline the hub card's eligible_at
// shows — and every case the rule cannot settle is omitted, never guessed.

func stablePolicyFor(t *testing.T, s *HubServer, imageRef, channel string) *HeartbeatUpgradePolicy {
	t.Helper()
	saas := &SaaSHive{AutoUpgrade: true, TrackedChannel: channel}
	payload := &HeartbeatPayload{HiveID: "h", GitBranch: "v5", GitHash: "0ba47d0", ImageRef: imageRef}
	got := s.heartbeatUpgradePolicy(payload, saas, false, false, "v5", "")
	if got == nil {
		t.Fatal("policy = nil")
	}
	return got
}

func TestHeartbeatUpgradePolicyNextUpdateAtForStableChannel(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	seedStablePromotionChannels(t)
	// newHubServerForTest redirects stablePromotionPath into t.TempDir(); a
	// bare &HubServer{} would leave saveStablePromotionState writing to /data.
	s := newHubServerForTest(t, withHubLogger(targetingLogger()))

	// Cold cache: the heartbeat never resolves channels itself.
	if got := stablePolicyFor(t, s, "ghcr.io/hivecommons/hive:stable", ReleaseChannelStable); got.NextUpdateAt != "" {
		t.Fatalf("NextUpdateAt = %q before channels resolved, want unknown", got.NextUpdateAt)
	}

	targets := getChannelTargets(getDisplaySHAs(), s.logger)
	want := stablePromotionEligibleAt(targetFor(targets, ReleaseChannelCandidate).CommittedAt)
	if want == "" {
		t.Fatal("seeded candidate has no commit date")
	}
	got := stablePolicyFor(t, s, "ghcr.io/hivecommons/hive:stable", ReleaseChannelStable)
	if got.NextUpdateAt != want || got.NextUpdateStatus != stableNextUpdateStatusQueued {
		t.Errorf("NextUpdate = %q/%q, want the stable eligible deadline %q/queued", got.NextUpdateAt, got.NextUpdateStatus, want)
	}
	if status := s.stablePromotionStatus(targets); status.EligibleAt == nil || *status.EligibleAt != got.NextUpdateAt {
		t.Errorf("NextUpdateAt %q disagrees with the hub card's eligible_at %v", got.NextUpdateAt, status.EligibleAt)
	}
	if _, err := time.Parse(time.RFC3339, got.NextUpdateAt); err != nil {
		t.Errorf("NextUpdateAt %q is not RFC3339: %v", got.NextUpdateAt, err)
	}

	if got := stablePolicyFor(t, s, "ghcr.io/hivecommons/hive:candidate", ReleaseChannelCandidate); got.NextUpdateAt != "" || got.NextUpdateStatus != "" {
		t.Errorf("candidate hive NextUpdate = %q/%q, want empty — candidate moves on every green build", got.NextUpdateAt, got.NextUpdateStatus)
	}

	if err := saveStablePromotionState(StablePromotionState{AutoPromote: false, UpdatedBy: hubAdminUsername}); err != nil {
		t.Fatal(err)
	}
	if got := stablePolicyFor(t, s, "ghcr.io/hivecommons/hive:stable", ReleaseChannelStable); got.NextUpdateAt != "" || got.NextUpdateStatus != stableNextUpdateStatusPaused {
		t.Errorf("NextUpdate = %q/%q while stable auto-promotion is paused, want empty/paused", got.NextUpdateAt, got.NextUpdateStatus)
	}
}

func TestNextScheduledAutoUpgradeAt(t *testing.T) {
	loc, err := autoUpgradeLocation()
	if err != nil {
		t.Fatal(err)
	}
	format := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	beforeDaily := time.Date(2026, 10, 5, 12, 30, 0, 0, loc)
	if got, want := nextScheduledAutoUpgradeAt(AutoUpgradeModeDaily, "", beforeDaily), format(time.Date(2026, 10, 5, 13, 0, 0, 0, loc)); got != want {
		t.Fatalf("daily before window = %q, want %q", got, want)
	}

	afterFired := time.Date(2026, 10, 5, 14, 20, 0, 0, loc)
	if got, want := nextScheduledAutoUpgradeAt(AutoUpgradeModeDaily, "2026-10-05", afterFired), format(time.Date(2026, 10, 6, 13, 0, 0, 0, loc)); got != want {
		t.Fatalf("daily after fired = %q, want %q", got, want)
	}
	if got, want := nextScheduledAutoUpgradeAt(AutoUpgradeModeDaily, "", afterFired), format(time.Date(2026, 10, 5, 13, 0, 0, 0, loc)); got != want {
		t.Fatalf("daily open window not fired = %q, want %q", got, want)
	}

	monday := time.Date(2026, 10, 5, 14, 20, 0, 0, loc)
	if got, want := nextScheduledAutoUpgradeAt(AutoUpgradeModeWeekly, "", monday), format(time.Date(2026, 10, 6, 13, 0, 0, 0, loc)); got != want {
		t.Fatalf("weekly before Tuesday window = %q, want %q", got, want)
	}

	wednesdayFired := time.Date(2026, 10, 7, 9, 0, 0, 0, loc)
	if got, want := nextScheduledAutoUpgradeAt(AutoUpgradeModeWeekly, "2026-10-06", wednesdayFired), format(time.Date(2026, 10, 13, 13, 0, 0, 0, loc)); got != want {
		t.Fatalf("weekly after this week fired = %q, want %q", got, want)
	}
	if got, want := nextScheduledAutoUpgradeAt(AutoUpgradeModeWeekly, "", wednesdayFired), format(time.Date(2026, 10, 6, 13, 0, 0, 0, loc)); got != want {
		t.Fatalf("weekly open window not fired = %q, want %q", got, want)
	}
}

func TestStableNextPromotionAtUnknownWhenNothingQueued(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	origGen, origRuns, origDigest := ghcrTagGeneration, stablePromotionFetchRuns, ghcrTagDigest
	defer func() {
		waitChannelTargetRefreshes(t)
		ghcrTagGeneration, stablePromotionFetchRuns, ghcrTagDigest = origGen, origRuns, origDigest
	}()
	resetStablePromotionCaches()
	t.Cleanup(resetStablePromotionCaches)

	builtAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	queued := []ChannelTarget{
		{Channel: ReleaseChannelStable, SHA: "0ba47d0", Digest: "sha256:stable"},
		{Channel: ReleaseChannelCandidate, SHA: "d5a638e", Digest: "sha256:candidate", CommittedAt: builtAt},
	}
	ghcrTagGeneration = func(_ string, tag string, _ *slog.Logger) int {
		switch tag {
		case ReleaseChannelStable:
			return 100
		case "d5a638e":
			return 101
		default:
			return 0
		}
	}
	ghcrTagDigest = func(_ string, tag string, _ *slog.Logger) string {
		if tag == "d5a638e" {
			return "sha256:candidate"
		}
		return ""
	}
	stablePromotionFetchRuns = func(*slog.Logger) []stablePromotionWorkflowRun {
		return []stablePromotionWorkflowRun{{RunNumber: 101, HeadSHA: "d5a638e", UpdatedAt: builtAt, Status: "completed", Conclusion: "success"}}
	}
	s := newHubServerForTest(t, withHubLogger(targetingLogger()))
	got, status := s.stableNextPromotion(queued)
	if want := stablePromotionEligibleAt(builtAt); got != want || status != stableNextUpdateStatusQueued {
		t.Fatalf("queued candidate: got %q/%q, want %q/queued", got, status, want)
	}

	stablePromotionFetchRuns = func(*slog.Logger) []stablePromotionWorkflowRun { return nil }
	resetStablePromotionCaches()
	for name, targets := range map[string][]ChannelTarget{
		"same digest": {
			{Channel: ReleaseChannelStable, SHA: "0ba47d0", Digest: "sha256:same"},
			{Channel: ReleaseChannelCandidate, SHA: "0ba47d0", Digest: "sha256:same", CommittedAt: builtAt},
		},
		"same commit": {
			{Channel: ReleaseChannelStable, SHA: "0ba47d0", Digest: "sha256:a"},
			{Channel: ReleaseChannelCandidate, SHA: "0ba47d0", Digest: "sha256:b", CommittedAt: builtAt},
		},
	} {
		if got, status := s.stableNextPromotion(targets); got != "" || status != stableNextUpdateStatusNone {
			t.Errorf("%s: got %q/%q, want empty/none", name, got, status)
		}
	}
	for name, targets := range map[string][]ChannelTarget{
		"stable unresolved": {
			{Channel: ReleaseChannelStable},
			{Channel: ReleaseChannelCandidate, SHA: "d5a638e", Digest: "sha256:candidate", CommittedAt: builtAt},
		},
		"candidate missing": {
			{Channel: ReleaseChannelStable, SHA: "0ba47d0", Digest: "sha256:stable"},
		},
	} {
		if got, status := s.stableNextPromotion(targets); got != "" || status != stableNextUpdateStatusUnknown {
			t.Errorf("%s: got %q/%q, want empty/unknown", name, got, status)
		}
	}
}

func TestHeartbeatUpgradePolicyNextUpdateAtWireShape(t *testing.T) {
	raw, err := json.Marshal(HeartbeatUpgradePolicy{Channel: ReleaseChannelStable, TargetResolved: true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["next_update_at"]; present {
		t.Errorf("next_update_at present when unknown: %s", raw)
	}
	raw, err = json.Marshal(HeartbeatUpgradePolicy{NextUpdateAt: "2026-10-03T13:00:00Z", NextUpdateStatus: stableNextUpdateStatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["next_update_at"]) != `"2026-10-03T13:00:00Z"` {
		t.Errorf("next_update_at = %s, want the RFC3339 ETA", m["next_update_at"])
	}
	if string(m["next_update_status"]) != `"queued"` {
		t.Errorf("next_update_status = %s, want queued", m["next_update_status"])
	}
}
