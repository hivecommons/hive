package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// /api/version — what the SPA's version menu actually reads — carries the
// same derived upgrade state as /api/status plus the hub's attempt history,
// so the spoke dashboard can show "draining · 1 agent in turn · 1m left" and
// the missed-window list without a second fetch (#14950).
func TestVersionCarriesUpgradeProgressAndHistory(t *testing.T) {
	origMarker, origOutcome := upgradeMarkerPath, upgradeOutcomePath
	dir := t.TempDir()
	upgradeMarkerPath = filepath.Join(dir, "upgrade-requested")
	upgradeOutcomePath = filepath.Join(dir, "last-upgrade-outcome")
	t.Cleanup(func() { upgradeMarkerPath, upgradeOutcomePath = origMarker, origOutcome })

	s := &Server{status: &StatusPayload{Agents: []FrontendAgent{{Name: "reviewer"}, {Name: "scanner"}}}, startedAt: time.Now()}
	started := time.Date(2026, 10, 26, 17, 0, 0, 0, time.UTC)
	s.SetHubUpgradePolicy(&spoke.HeartbeatUpgradePolicy{
		HubManaged:   true,
		NextWindowAt: "2026-10-29T17:00:00Z",
		UpgradeHistory: []spoke.UpgradeAttempt{
			{Target: "abc1234", Trigger: spoke.UpgradeTriggerWindow, StartedAt: started, EndedAt: started, Outcome: spoke.UpgradeOutcomeMissed, Reason: "drain lapsed"},
		},
	})
	s.SetUpgradeDrainSource(func(now time.Time) *spoke.UpgradeDrainReport {
		return &spoke.UpgradeDrainReport{Target: "abc1234", RemainingS: 60, InFlight: []string{"reviewer"}, Turns: []spoke.UpgradeTurn{{Agent: "reviewer", ElapsedS: 2280}}}
	})

	rec := httptest.NewRecorder()
	s.handleVersion(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		UpgradeProgress *spoke.UpgradeProgress `json:"upgradeProgress"`
		UpgradeHistory  []spoke.UpgradeAttempt `json:"upgradeHistory"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	p := body.UpgradeProgress
	if p == nil || p.State != spoke.UpgradeStateDraining || p.DrainRemainingS != 60 || p.AgentsTotal != 2 {
		t.Fatalf("upgradeProgress = %+v, want draining/60s/2 agents", p)
	}
	if len(p.AgentsInTurn) != 1 || p.AgentsInTurn[0].Agent != "reviewer" || p.AgentsInTurn[0].ElapsedS != 2280 {
		t.Fatalf("agentsInTurn = %+v", p.AgentsInTurn)
	}
	if len(body.UpgradeHistory) != 1 || body.UpgradeHistory[0].Outcome != spoke.UpgradeOutcomeDelayed || body.UpgradeHistory[0].Reason != "drain lapsed" {
		t.Fatalf("upgradeHistory = %+v", body.UpgradeHistory)
	}

	// The spoke knows the rollout landed before the hub's next heartbeat.
	s.SetUpgradeDrainSource(nil)
	outcome, err := json.Marshal(upgradeOutcome{TargetSHA: "abc1234", CompletedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upgradeOutcomePath, outcome, 0o600); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleVersion(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	body.UpgradeHistory = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.UpgradeProgress == nil || body.UpgradeProgress.State != spoke.UpgradeStateDone || len(body.UpgradeHistory) != 0 {
		t.Fatalf("successful upgrade still shows a delay: %+v", body)
	}
	if err := os.Remove(upgradeOutcomePath); err != nil {
		t.Fatal(err)
	}

	// No policy, no drain, nothing on disk: the keys are absent rather than
	// null so older SPA builds see exactly the payload they did before.
	s2 := &Server{status: &StatusPayload{}, startedAt: time.Now()}
	rec = httptest.NewRecorder()
	s2.handleVersion(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["upgradeHistory"]; ok {
		t.Errorf("upgradeHistory present without a policy: %s", raw["upgradeHistory"])
	}
}

// The SPA renders the state pill and history list from those fields, with
// a label for every state the server can emit, and never through a native
// dialog or an inline style.
func TestIndexRendersUpgradeProgressPillAndHistory(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function versionUpgradeStatePillHTML(",
		"function versionUpgradeHistoryHTML(",
		"function versionAgentsInTurnText(",
		"v.upgradeProgress",
		"v.upgradeHistory",
		"st.drainRemainingS",
		"st.agentsInTurn",
		".oc-version-state-pill {",
		".oc-version-state-pill.is-draining",
		".oc-version-state-pill.is-delayed",
		".oc-version-state-pill.is-missed",
		".oc-version-history {",
		"row('Progress', statePill",
		">Upgrade history</span>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	labels := jsFunc(t, html, "versionStateLabels")
	for _, state := range []string{"scheduled", "delayed", "draining", "pulling", "rolling", "verifying", "done", "missed"} {
		if !strings.Contains(labels, state+":") {
			t.Errorf("versionStateLabels lacks %q", state)
		}
	}
	fnStart := strings.Index(html, "function versionUpgradeHistoryHTML(")
	fn := html[fnStart : fnStart+strings.Index(html[fnStart:], "\n    }\n")]
	if strings.Contains(fn, "style=") {
		t.Errorf("versionUpgradeHistoryHTML must use the .oc-version-history classes, not inline styles:\n%s", fn)
	}
}
