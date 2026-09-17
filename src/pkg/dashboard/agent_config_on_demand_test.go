package dashboard

import (
	"net/http"
	"testing"
)

// Regression for hivecommons/hive#7446: on_demand is a real per-agent config
// field, but the settings dialog only ever RENDERED it — as a read-only "on
// demand" badge on the agent card — and never offered a control. An operator
// could see that an agent was on-demand and had no way to take it off, because
// GET /api/config/agent/{name} did not return the field and
// PUT /api/config/agent/{name}/general silently dropped it.
//
// The field matters beyond scheduling: reviewCapableAgents (pkg/review/dispatch.go)
// skips any agent with OnDemand set, so a reviewer left on-demand can never be
// dispatched — the hive reports reviewCapableAgents: 0 and no PR is ever
// reviewed. Being unable to clear the flag from the UI made that unrecoverable
// without hand-editing hive.yaml.

func TestAgentConfigGet_ExposesOnDemand(t *testing.T) {
	s, deps := apiServer(t)

	cfg := deps.Config.Agents["scanner"]
	cfg.OnDemand = true
	deps.Config.Agents["scanner"] = cfg

	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	general, ok := resp["general"].(map[string]interface{})
	if !ok {
		t.Fatalf("response has no general section: %v", resp)
	}
	// Presence is the point: a missing key renders as an unchecked switch, which
	// would show "not on demand" for an agent that is.
	v, present := general["onDemand"]
	if !present {
		t.Fatalf("general.onDemand must be present so the dialog can render the control: %v", general)
	}
	if v != true {
		t.Errorf("general.onDemand = %v, want true", v)
	}
}

func TestAgentConfigGeneral_ClearsAndSetsOnDemand(t *testing.T) {
	s, deps := apiServer(t)

	cfg := deps.Config.Agents["scanner"]
	cfg.OnDemand = true
	deps.Config.Agents["scanner"] = cfg

	// Clearing it is the case that was impossible from the UI.
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"onDemand": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.Agents["scanner"].OnDemand {
		t.Fatal("on_demand must be cleared by the general save; the agent stays unschedulable otherwise")
	}

	// And it must still be settable, so the control is not one-way.
	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{"onDemand": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !deps.Config.Agents["scanner"].OnDemand {
		t.Fatal("on_demand must be settable back to true")
	}
}

// A general save that does not mention onDemand must leave it alone. The
// dialog PUTs only dirty keys, so an operator editing an unrelated field on an
// on-demand agent must not silently un-demand it.
func TestAgentConfigGeneral_OmittedOnDemandIsPreserved(t *testing.T) {
	s, deps := apiServer(t)

	cfg := deps.Config.Agents["scanner"]
	cfg.OnDemand = true
	deps.Config.Agents["scanner"] = cfg

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"displayName": "Scanner"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !deps.Config.Agents["scanner"].OnDemand {
		t.Error("a save that omits onDemand must not clear it")
	}
}
