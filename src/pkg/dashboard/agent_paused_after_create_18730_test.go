package dashboard

import (
	"net/http"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/governor"
)

// TestHandAgentPausedAfterCreateKeepsItsCard reproduces hivecommons/hive#18730.
// An operator created "sec-sync" from the dashboard and paused it. The agent
// then vanished from every dashboard view, yet creating "sec-sync" again was
// refused with "agent already exists".
//
// A hand-created agent is never in any ACMM pack roster, and the status
// builder hid every paused agent outside the pack as an inactive "ghost". The
// create handler makes the operator the owner of the agent's pause state, so
// the pack sweep never touches it and the card is the only way back to it: a
// paused operator-owned agent must keep its card, and must stay deletable so
// the name can be reused.
func TestHandAgentPausedAfterCreateKeepsItsCard(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()

	rec := doPost(s, "/api/agents", map[string]interface{}{
		"name":  "sec-sync",
		"agent": map[string]interface{}{"backend": "copilot", "enabled": true},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	created, ok := deps.Config.Agents["sec-sync"]
	if !ok {
		t.Fatal("sec-sync should be in config after creation")
	}
	if !created.PauseIsOperatorOwned() {
		t.Fatalf("a hand-created agent must be operator-owned for pause state, got PauseOwner=%q", created.PauseOwner)
	}

	// The hive runs an ACMM pack, and sec-sync is not in its roster.
	level := 1
	deps.Config.ACMMLevel = &level
	if allowed := acmmPackAllowedSet(deps.Config); allowed == nil || allowed["sec-sync"] {
		t.Fatalf("fixture: sec-sync must be outside the level-%d pack, allowed=%v", level, allowed)
	}

	// Paused immediately after creation: the runtime entry is paused, and the
	// pause has (or has not yet) been persisted to the config.
	for _, persisted := range []bool{false, true} {
		agentCfg := deps.Config.Agents["sec-sync"]
		agentCfg.Paused = persisted
		deps.Config.Agents["sec-sync"] = agentCfg

		statuses := map[string]*crux.Process{
			"sec-sync": {
				Name:          "sec-sync",
				Config:        agentCfg,
				State:         crux.StatePaused,
				Paused:        true,
				PausedTrigger: "dashboard-api",
				PausedReason:  "manual pause",
				OutputBuffer:  cruxtest.NewRingBuffer(10),
			},
		}
		agents, hidden := buildAgentsWithHidden(statuses, deps.Config, governor.State{Mode: governor.ModeIdle})
		if !frontendAgentsContain(agents, "sec-sync") {
			t.Errorf("persisted=%v: paused hand-created agent has no card: agents=%v hidden=%+v",
				persisted, agentNamesFromFrontend(agents), hidden)
		}
		for _, h := range hidden {
			if h.Name == "sec-sync" {
				t.Errorf("persisted=%v: paused hand-created agent reported hidden: %+v", persisted, h)
			}
		}

		// Same agent with no runtime entry at all (the manager has not
		// registered it yet, or dropped it): the config-only pass must
		// surface it too.
		agents, hidden = buildAgentsWithHidden(map[string]*crux.Process{}, deps.Config, governor.State{Mode: governor.ModeIdle})
		if persisted && !frontendAgentsContain(agents, "sec-sync") {
			t.Errorf("config-only: paused hand-created agent has no card: agents=%v hidden=%+v",
				agentNamesFromFrontend(agents), hidden)
		}
	}

	// It can still be deleted, which frees the name for a new create.
	rec = doDelete(s, "/api/agents/sec-sync", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = doPost(s, "/api/agents", map[string]interface{}{
		"name":  "sec-sync",
		"agent": map[string]interface{}{"backend": "copilot", "enabled": true},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-create after delete: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func frontendAgentsContain(agents []FrontendAgent, name string) bool {
	for _, a := range agents {
		if a.Name == name {
			return true
		}
	}
	return false
}
