package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// The contributor-first lane keys (agents.<name>.contributor_first and
// contributor_first_grace, #16518) were hive.yaml-only, so on a hub-hosted
// spoke only the hub operator could switch a lane over. The agent's General
// tab now reads and writes both, refusing exactly what a load would refuse.

func generalSection(t *testing.T, s *Server, agent string) map[string]interface{} {
	t.Helper()
	rec := doGet(s, "/api/config/agent/"+agent)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	general, ok := decodeJSON(t, rec)["general"].(map[string]interface{})
	if !ok {
		t.Fatal("response has no general section")
	}
	return general
}

func TestAgentConfigGet_ExposesContributorFirst(t *testing.T) {
	s, deps := apiServer(t)
	cfg := deps.Config.Agents["scanner"]
	cfg.ContributorFirst = true
	cfg.ContributorFirstGrace = 15 * time.Minute
	deps.Config.Agents["scanner"] = cfg
	deps.Config.Agents["architect"] = config.AgentConfig{Backend: "claude", Enabled: true}

	g := generalSection(t, s, "scanner")
	if g["contributorFirst"] != true {
		t.Errorf("contributorFirst = %v, want true", g["contributorFirst"])
	}
	if g["contributorFirstGrace"] != float64(900) {
		t.Errorf("contributorFirstGrace = %v, want 900 seconds", g["contributorFirstGrace"])
	}
	if g["contributorFirstDelegatable"] != true {
		t.Errorf("scanner is a default delegatable role; contributorFirstDelegatable = %v", g["contributorFirstDelegatable"])
	}

	// Presence matters for the unset case too: the panel renders 0 as the
	// default placeholder and needs the delegatable flag to disable the toggle.
	g = generalSection(t, s, "architect")
	for key, want := range map[string]interface{}{"contributorFirst": false, "contributorFirstGrace": float64(0), "contributorFirstDelegatable": false} {
		v, present := g[key]
		if !present || v != want {
			t.Errorf("architect general.%s = %v (present %v), want %v", key, v, present, want)
		}
	}
}

func TestAgentConfigGeneral_SetsContributorFirstAndGrace(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"contributorFirst": true, "contributorFirstGrace": 300})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.Agents["scanner"]
	if !got.ContributorFirst || got.ContributorFirstGrace != 5*time.Minute {
		t.Fatalf("stored contributor_first=%v grace=%s, want true and 5m", got.ContributorFirst, got.ContributorFirstGrace)
	}
	overlay, err := os.ReadFile(filepath.Join(deps.Config.Data.AgentsDir, "scanner.yaml"))
	if err != nil {
		t.Fatalf("agent overlay not written: %v", err)
	}
	if !strings.Contains(string(overlay), "contributor_first: true") || !strings.Contains(string(overlay), "contributor_first_grace: 5m0s") {
		t.Errorf("agent overlay does not carry both keys:\n%s", overlay)
	}

	// GET round-trips what was saved.
	g := generalSection(t, s, "scanner")
	if g["contributorFirst"] != true || g["contributorFirstGrace"] != float64(300) {
		t.Errorf("GET after save: contributorFirst=%v grace=%v", g["contributorFirst"], g["contributorFirstGrace"])
	}

	// 0 clears the stored grace, so the governor falls back to the default.
	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{"contributorFirstGrace": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got = deps.Config.Agents["scanner"]
	if got.ContributorFirstGrace != 0 || got.ContributorFirstGraceOrDefault() != config.DefaultContributorFirstGrace {
		t.Errorf("grace 0 must clear to the default, got stored %s effective %s", got.ContributorFirstGrace, got.ContributorFirstGraceOrDefault())
	}
	if !got.ContributorFirst {
		t.Error("a save that only clears the grace must keep contributor_first")
	}
}

// The save updates the governor in place (UpdateAgents), so a lane switched
// on from the panel is withheld on the next evaluation once a relay covers
// the role — no restart.
func TestAgentConfigGeneral_ContributorFirstReachesGovernor(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.SetContributorCoverage(map[string]governor.ContributorCoverage{"scanner": {Contributors: []string{"alice"}}})
	deps.Governor.Evaluate(0, 0, 0, 0)
	if len(deps.Governor.GetState().CoveredLanes) != 0 {
		t.Fatal("without the flag a covered role must not be reported as a covered lane")
	}

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"contributorFirst": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	deps.Governor.SetContributorCoverage(map[string]governor.ContributorCoverage{"scanner": {Contributors: []string{"alice"}}})
	deps.Governor.Evaluate(0, 0, 0, 0)
	cov, ok := deps.Governor.GetState().CoveredLanes["scanner"]
	if !ok || len(cov.Contributors) != 1 || cov.Contributors[0] != "alice" {
		t.Fatalf("CoveredLanes = %+v, want scanner covered by alice after the save", deps.Governor.GetState().CoveredLanes)
	}
}

// A refusal answers 400 with the load's own message and changes nothing, in
// memory or on disk.
func TestAgentConfigGeneral_RefusesWhatTheLoadRefuses(t *testing.T) {
	cases := []struct {
		name  string
		agent string
		body  map[string]any
	}{
		{"role not delegatable", "architect", map[string]any{"contributorFirst": true}},
		{"supervisor", "supervisor", map[string]any{"contributorFirst": true}},
		{"negative grace", "scanner", map[string]any{"contributorFirstGrace": -60}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, deps := apiServer(t)
			deps.Config.Data.AgentsDir = t.TempDir()
			if _, ok := deps.Config.Agents[tc.agent]; !ok {
				deps.Config.Agents[tc.agent] = config.AgentConfig{Backend: "claude", Enabled: true}
			}
			before := deps.Config.Agents[tc.agent]

			body := map[string]any{"displayName": "Renamed"}
			for k, v := range tc.body {
				body[k] = v
			}
			rec := doPut(s, "/api/config/agent/"+tc.agent+"/general", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}

			attempted := before
			if v, ok := tc.body["contributorFirst"].(bool); ok {
				attempted.ContributorFirst = v
			}
			if v, ok := tc.body["contributorFirstGrace"].(int); ok {
				attempted.ContributorFirstGrace = time.Duration(v) * time.Second
			}
			want := config.ValidateContributorFirstAgent(tc.agent, attempted, deps.Config.Hub)
			if want == nil {
				t.Fatal("test case must be one the load refuses")
			}
			if msg := decodeJSON(t, rec)["error"]; msg != want.Error() {
				t.Errorf("error = %q, want the load's message %q", msg, want.Error())
			}

			after := deps.Config.Agents[tc.agent]
			if after.ContributorFirst != before.ContributorFirst || after.ContributorFirstGrace != before.ContributorFirstGrace || after.DisplayName != before.DisplayName {
				t.Errorf("a refused save changed the agent: before %+v after %+v", before, after)
			}
			if entries, _ := os.ReadDir(deps.Config.Data.AgentsDir); len(entries) != 0 {
				t.Errorf("a refused save wrote %d file(s) to the agents dir", len(entries))
			}
		})
	}
}

func TestAgentConfigGeneral_OmittedContributorFirstIsPreserved(t *testing.T) {
	s, deps := apiServer(t)
	cfg := deps.Config.Agents["scanner"]
	cfg.ContributorFirst = true
	cfg.ContributorFirstGrace = 20 * time.Minute
	deps.Config.Agents["scanner"] = cfg

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"displayName": "Scanner"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.Agents["scanner"]
	if !got.ContributorFirst || got.ContributorFirstGrace != 20*time.Minute {
		t.Errorf("a save that omits both keys must keep them, got contributor_first=%v grace=%s", got.ContributorFirst, got.ContributorFirstGrace)
	}
}
