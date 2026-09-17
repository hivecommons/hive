package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub"
)

// #7262: every version surface on the spoke must be measured against the
// commit the hub will actually roll it to — delivered on the heartbeat as
// UpgradePolicy — and never against a hard-wired stable branch. A v5 spoke
// measured against v4 reported "35 behind" while its real distance was 28,
// and rendered "Automatic updates are turned off" for a hub-managed hive.

func TestResolveUpgradeTargetPrecedence(t *testing.T) {
	// No policy: the build's own branch tip, never a constant "v4".
	got := resolveUpgradeTarget(nil, "v5", "5193426abcdef")
	if got.Source != upgradeTargetSourceBranch || got.Branch != "v5" || got.Short != "5193426" || !got.Resolved || got.ManagedBy != "" {
		t.Errorf("no-policy target = %+v, want branch v5 tip 5193426", got)
	}

	// Hub-managed channel spoke: the channel revision wins over the branch tip.
	p := &hub.HeartbeatUpgradePolicy{HubManaged: true, Branch: "v5", Channel: "edge", TargetSHA: "6a5b337", TargetResolved: true, Schedule: "instant"}
	got = resolveUpgradeTarget(p, "v5", "5193426abcdef")
	if got.Source != upgradeTargetSourceHub || got.Short != "6a5b337" || got.Channel != "edge" || got.ManagedBy != autoUpdateManagedByHub {
		t.Errorf("hub target = %+v, want channel revision 6a5b337 managed by hub", got)
	}

	// Unresolved channel: no SHA, Resolved=false — the UI must say unknown.
	p = &hub.HeartbeatUpgradePolicy{HubManaged: true, Branch: "v4", Channel: "stable", TargetResolved: false}
	got = resolveUpgradeTarget(p, "v4", "526ef71abcdef")
	if got.Resolved || got.SHA != "" {
		t.Errorf("unresolved target = %+v, want no SHA and Resolved=false (branch tip must never stand in for a channel)", got)
	}

	// Spoke-managed branch spoke whose hub has no verified image yet: the
	// branch tip is the same answer the hub would give.
	p = &hub.HeartbeatUpgradePolicy{SpokeManaged: true, Branch: "v4", TargetResolved: true, Paused: true}
	got = resolveUpgradeTarget(p, "v4", "526ef71abcdef")
	if got.Short != "526ef71" || got.ManagedBy != autoUpdateManagedBySpoke || !got.Paused {
		t.Errorf("spoke-managed target = %+v, want branch tip, managed by spoke, paused", got)
	}
}

func TestBuildAutoUpdateStatusHubPolicyOverridesLocalFlag(t *testing.T) {
	zero := 0
	three := 3

	// Local config says off; the hub says it manages upgrades instantly.
	// The hub is the authority.
	st := buildAutoUpdateStatus(autoUpdateInputs{
		Enabled: false, Period: "",
		Policy:        &hub.HeartbeatUpgradePolicy{HubManaged: true, Schedule: "instant", Branch: "v5", Channel: "edge", TargetSHA: "6a5b337", TargetResolved: true},
		TargetBranch:  "v5",
		TargetChannel: "edge",
		TargetCommit:  "6a5b337",
		CurrentCommit: "6a5b337",
		CommitsBehind: &zero,
	})
	if !st.Enabled || st.ManagedBy != autoUpdateManagedByHub || st.PolicySource != upgradeTargetSourceHub {
		t.Errorf("Enabled=%v ManagedBy=%q PolicySource=%q, want enabled/hub/hub", st.Enabled, st.ManagedBy, st.PolicySource)
	}
	if st.Period != hub.AutoUpgradeModeInstant {
		t.Errorf("Period = %q, want instant from the hub (local mode was empty)", st.Period)
	}
	if st.State != autoUpdateStateUpToDate || !st.Healthy {
		t.Errorf("state=%q healthy=%v, want up_to_date/healthy", st.State, st.Healthy)
	}
	if !strings.Contains(st.Detail, ":edge channel") {
		t.Errorf("Detail = %q, want it to name the channel", st.Detail)
	}

	// Paused fleet-wide: deliberate, so healthy, but never "up to date" and
	// the behind count is still surfaced.
	st = buildAutoUpdateStatus(autoUpdateInputs{
		Policy:        &hub.HeartbeatUpgradePolicy{HubManaged: true, Schedule: "daily", Paused: true, TargetResolved: true},
		TargetBranch:  "v4",
		CommitsBehind: &three,
	})
	if st.State != autoUpdateStatePaused || !st.Healthy || !st.Paused {
		t.Errorf("paused: state=%q healthy=%v paused=%v", st.State, st.Healthy, st.Paused)
	}
	if !strings.Contains(st.Detail, "3 commit(s) behind") {
		t.Errorf("paused Detail = %q, want the behind count", st.Detail)
	}

	// Hub says nobody manages it: disabled, in the hub's voice.
	st = buildAutoUpdateStatus(autoUpdateInputs{
		Enabled: true, // stale local flag must not win
		Policy:  &hub.HeartbeatUpgradePolicy{TargetResolved: true},
	})
	if st.Enabled || st.State != autoUpdateStateDisabled || st.ManagedBy != "" {
		t.Errorf("nobody-manages: %+v", st)
	}
	if !strings.Contains(st.Detail, "hub does not apply upgrades") {
		t.Errorf("Detail = %q, want the hub-sourced wording", st.Detail)
	}

	// Unresolved channel: unknown, never healthy, and says WHY.
	st = buildAutoUpdateStatus(autoUpdateInputs{
		Policy: &hub.HeartbeatUpgradePolicy{HubManaged: true, Channel: "stable", TargetResolved: false},
	})
	if st.State != autoUpdateStateUnknown || st.Healthy {
		t.Errorf("unresolved: state=%q healthy=%v", st.State, st.Healthy)
	}
	if !strings.Contains(st.Detail, "could not resolve stable") {
		t.Errorf("Detail = %q, want the channel named", st.Detail)
	}

	// No policy at all (older hub / unmanaged): the local flag still rules and
	// PolicySource says so.
	st = buildAutoUpdateStatus(autoUpdateInputs{Enabled: true, Period: "weekly", CommitsBehind: &zero})
	if st.PolicySource != "local" || st.ManagedBy != autoUpdateManagedBySpoke || st.Period != hub.AutoUpgradeModeWeekly {
		t.Errorf("local fallback: %+v", st)
	}
}

// End-to-end through the real handler: once the heartbeat has delivered a
// policy, /api/version measures commitsBehind against the policy's TargetSHA
// (the channel revision), not the branch tip, and reports the hub's schedule.
func TestHandleVersionUsesHubUpgradePolicyTarget(t *testing.T) {
	origHash, origShort := versionHash, versionShort
	versionHash, versionShort = "ffe1e19aaaa", "ffe1e19"
	t.Cleanup(func() { versionHash, versionShort = origHash, origShort })
	orig := upgradeMarkerPath
	upgradeMarkerPath = filepath.Join(t.TempDir(), "no-such-marker")
	t.Cleanup(func() { upgradeMarkerPath = orig })

	// Both the branch tip and the channel revision have images; only the
	// channel revision may be compared against.
	ghcrCacheMu.Lock()
	ghcrCacheResult["5193426"] = true
	ghcrCacheExpiry["5193426"] = time.Now().Add(time.Hour)
	ghcrCacheResult["6a5b337"] = true
	ghcrCacheExpiry["6a5b337"] = time.Now().Add(time.Hour)
	ghcrCacheMu.Unlock()
	t.Cleanup(func() {
		ghcrCacheMu.Lock()
		for _, k := range []string{"5193426", "6a5b337"} {
			delete(ghcrCacheResult, k)
			delete(ghcrCacheExpiry, k)
		}
		ghcrCacheMu.Unlock()
	})

	branchCompares, channelCompares := 0, 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/hivecommons/hive/git/ref/heads/v4", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref":    "refs/heads/v4",
			"object": map[string]any{"sha": "5193426000000000000000000000000000000000"},
		})
	})
	mux.HandleFunc("/repos/hivecommons/hive/commits/5193426000000000000000000000000000000000", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"message": "tip"}})
	})
	mux.HandleFunc("/repos/hivecommons/hive/compare/ffe1e19...5193426", func(w http.ResponseWriter, r *http.Request) {
		branchCompares++
		_ = json.NewEncoder(w).Encode(map[string]any{"ahead_by": 35})
	})
	mux.HandleFunc("/repos/hivecommons/hive/compare/ffe1e19...6a5b337", func(w http.ResponseWriter, r *http.Request) {
		channelCompares++
		_ = json.NewEncoder(w).Encode(map[string]any{"ahead_by": 28})
	})
	ghSrv := httptest.NewServer(mux)
	t.Cleanup(ghSrv.Close)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.Config.Hub.AutoUpgrade = false // local flag says off; the hub disagrees
	deps.GHClient = ghpkg.NewClientForTest(ghSrv.URL, "myorg", []string{"repo1"}, logger)
	s.RegisterAPI(deps)

	s.SetHubUpgradePolicy(&hub.HeartbeatUpgradePolicy{
		HubManaged: true, Schedule: "daily", Branch: "v4", Channel: "edge",
		TargetSHA: "6a5b337", TargetResolved: true,
	})

	rec := doGet(s, "/api/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["commitsBehind"] != float64(28) {
		t.Fatalf("commitsBehind = %v, want 28 (distance to the channel revision), not 35 (distance to the branch tip)", body["commitsBehind"])
	}
	if branchCompares != 0 || channelCompares != 1 {
		t.Fatalf("compares: branch=%d channel=%d, want 0/1", branchCompares, channelCompares)
	}
	if body["stableV4Short"] != "6a5b337" {
		t.Errorf("stableV4Short (legacy target key) = %v, want the policy target 6a5b337", body["stableV4Short"])
	}
	target, _ := body["target"].(map[string]any)
	if target["source"] != upgradeTargetSourceHub || target["channel"] != "edge" || target["managedBy"] != "hub" {
		t.Errorf("target = %v, want source=hub channel=edge managedBy=hub", target)
	}
	au, _ := body["autoUpdate"].(map[string]any)
	if au["enabled"] != true || au["managedBy"] != "hub" || au["period"] != "daily" || au["policySource"] != "hub" {
		t.Errorf("autoUpdate = %v, want enabled/hub/daily/hub despite the local flag being off", au)
	}
	if au["state"] != autoUpdateStateBehind || au["targetChannel"] != "edge" {
		t.Errorf("autoUpdate state=%v targetChannel=%v, want behind/edge", au["state"], au["targetChannel"])
	}
}

// A recorded spoke-side success must not keep claiming "running the target
// image" after the hub rolled the pod to a newer commit via a floating tag.
func TestBuildUpgradeAttemptStatusSupersededByLaterRoll(t *testing.T) {
	outcome := &upgradeOutcome{
		TargetSHA:   "06cccf4",
		RequestedAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 9, 16, 12, 3, 0, 0, time.UTC),
	}
	current := buildUpgradeAttemptStatus(outcome, nil, "06cccf4abcdef")
	if current.Superseded || !strings.Contains(current.Detail, "running the target image") {
		t.Errorf("still-current outcome: %+v", current)
	}

	moved := buildUpgradeAttemptStatus(outcome, nil, "ffe1e19abcdef")
	if moved.State != upgradeAttemptSucceeded {
		t.Errorf("State = %q, want succeeded (that attempt did land)", moved.State)
	}
	if !moved.Superseded || moved.RunningCommit != "ffe1e19" {
		t.Errorf("Superseded=%v RunningCommit=%q, want true/ffe1e19", moved.Superseded, moved.RunningCommit)
	}
	if strings.Contains(moved.Detail, "running the target image") || !strings.Contains(moved.Detail, "since moved to ffe1e19") {
		t.Errorf("Detail = %q, must say the hive has since moved", moved.Detail)
	}
}

func TestIndexHTMLHubTabReflectsUpgradePolicy(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`id="hub-autoupdate-toggle"`,
		`id="hub-autoupdate-schedule"`,
		"function applyAutoUpdatePolicyToHubTab(au)",
		"applyAutoUpdatePolicyToHubTab(au);",
		"function upgradeTargetLabel(v)",
		"case 'paused':",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html missing %q — the Hub tab no longer reflects the hub's upgrade policy (#7262)", snippet)
		}
	}
	if strings.Contains(html, "Stable v4 tip: ${") || strings.Contains(html, "stable v4 tip ${") {
		t.Error("index.html still measures the top-bar badge against a hard-wired v4 tip (#7262)")
	}
}
