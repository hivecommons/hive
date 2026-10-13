package dashboard

import (
	"errors"
	"net/http"
	"testing"
)

// The Strategy Lab panel configuration must survive a restart (#11694):
// every mutating /api/nous call hands Mode, Scope and Config to
// PersistPanel, and a fresh NousState re-applies that snapshot at boot.
func TestNousPanelConfigPersistsAcrossRestart(t *testing.T) {
	var saved []NousPanelSnapshot

	s, deps := apiServer(t)
	deps.Nous = &NousState{
		Mode:         "observe",
		Scope:        "governor",
		Status:       map[string]interface{}{},
		PersistPanel: func(snap NousPanelSnapshot) error { saved = append(saved, snap); return nil },
	}

	for _, step := range []struct {
		path string
		body map[string]interface{}
	}{
		{"/api/nous/mode", map[string]interface{}{"mode": "suggest"}},
		{"/api/nous/scope", map[string]interface{}{"scope": "both"}},
		{"/api/nous/config/goals", map[string]interface{}{"governor": "reduce MTTR"}},
		{"/api/nous/config/schedule", map[string]interface{}{"cooldown_hours": 2}},
	} {
		if rec := doPut(s, step.path, step.body); rec.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d: %s", step.path, rec.Code, rec.Body.String())
		}
	}

	if len(saved) != 4 {
		t.Fatalf("PersistPanel called %d times, want one per mutating PUT", len(saved))
	}
	pc := saved[len(saved)-1]
	if pc.Mode != "suggest" || pc.Scope != "both" {
		t.Fatalf("persisted = %+v, want suggest/both", pc)
	}
	if goals, _ := pc.Config["goals"].(map[string]interface{}); goals["governor"] != "reduce MTTR" {
		t.Fatalf("persisted goals = %+v", pc.Config["goals"])
	}
	if _, has := pc.Config["schedule"]; !has {
		t.Fatalf("persisted config lacks schedule: %+v", pc.Config)
	}

	// "Restart": a defaults-only state picks the persisted values back up.
	fresh := &NousState{Mode: "observe", Scope: "governor", Status: map[string]interface{}{"mode": "observe", "scope": "governor"}}
	fresh.ApplyPanelConfig(pc)
	if fresh.Mode != "suggest" || fresh.Scope != "both" {
		t.Fatalf("after restart mode/scope = %q/%q", fresh.Mode, fresh.Scope)
	}
	if fresh.Status["mode"] != "suggest" || fresh.Status["scope"] != "both" {
		t.Fatalf("status mirrors not refreshed: %+v", fresh.Status)
	}
	if goals, _ := fresh.Config["goals"].(map[string]interface{}); goals["governor"] != "reduce MTTR" {
		t.Fatalf("after restart goals = %+v", fresh.Config)
	}
}

func TestNousApplyPanelConfigKeepsDefaultsForEmptyFields(t *testing.T) {
	ns := &NousState{Mode: "observe", Scope: "governor", Config: map[string]interface{}{"keep": true}}
	ns.ApplyPanelConfig(NousPanelSnapshot{})
	if ns.Mode != "observe" || ns.Scope != "governor" {
		t.Fatalf("empty persisted fields must not clear defaults: %q/%q", ns.Mode, ns.Scope)
	}
	if ns.Config["keep"] != true {
		t.Fatalf("nil persisted Config must not replace the in-memory map: %+v", ns.Config)
	}
	// Nil Status is tolerated (bare NousState in tests).
	(&NousState{}).ApplyPanelConfig(NousPanelSnapshot{Mode: "suggest"})
}

func TestNousPersistPanelConfigNoopWithoutSeam(t *testing.T) {
	var nilState *NousState
	if err := nilState.PersistPanelConfig(); err != nil {
		t.Fatalf("nil state: %v", err)
	}
	if err := (&NousState{Mode: "observe"}).PersistPanelConfig(); err != nil {
		t.Fatalf("no seam: %v", err)
	}
}

// The snapshot handed to PersistPanel must be a copy: a later PUT mutating
// the live Config map must not reach into an in-flight write.
func TestNousPersistPanelConfigCopiesConfig(t *testing.T) {
	var got NousPanelSnapshot
	ns := &NousState{Config: map[string]interface{}{"goals": "a"}, PersistPanel: func(s NousPanelSnapshot) error { got = s; return nil }}
	if err := ns.PersistPanelConfig(); err != nil {
		t.Fatal(err)
	}
	ns.Config["goals"] = "b"
	if got.Config["goals"] != "a" {
		t.Fatalf("snapshot aliases live map: %+v", got.Config)
	}
}

func TestNousPersistFailureDoesNotFailRequest(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{Mode: "observe", PersistPanel: func(NousPanelSnapshot) error { return errors.New("disk full") }}
	if rec := doPut(s, "/api/nous/mode", map[string]interface{}{"mode": "suggest"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite persist failure", rec.Code)
	}
	if deps.Nous.Mode != "suggest" {
		t.Fatalf("mode = %q", deps.Nous.Mode)
	}
	// Nil deps/Nous short-circuits.
	(&Server{}).persistNousPanel()
}
