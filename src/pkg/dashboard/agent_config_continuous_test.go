package dashboard

import (
	"net/http"
	"testing"
	"time"
)

func TestAgentConfigContinuousRoundTrip(t *testing.T) {
	s, deps := apiServer(t)

	cfg := deps.Config.Agents["scanner"]
	cfg.Continuous = true
	cfg.ContinuousCooldown = 2 * time.Minute
	cfg.ContinuousBudgetPct = 75
	deps.Config.Agents["scanner"] = cfg

	rec := doGet(s, "/api/config/agent/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
	}
	general := decodeJSON(t, rec)["general"].(map[string]interface{})
	if general["continuous"] != true {
		t.Fatalf("general.continuous = %v, want true", general["continuous"])
	}
	if general["continuousCooldown"] != float64(120) {
		t.Fatalf("general.continuousCooldown = %v, want 120", general["continuousCooldown"])
	}
	if general["continuousBudgetPct"] != float64(75) {
		t.Fatalf("general.continuousBudgetPct = %v, want 75", general["continuousBudgetPct"])
	}

	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{"continuous": false, "continuousCooldown": 90, "continuousBudgetPct": 70})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.Agents["scanner"]
	if got.Continuous {
		t.Fatal("continuous should be cleared by general save")
	}
	if got.ContinuousCooldown != 90*time.Second {
		t.Fatalf("continuous cooldown = %v, want 90s", got.ContinuousCooldown)
	}
	if got.ContinuousBudgetPct != 70 {
		t.Fatalf("continuous budget pct = %d, want 70", got.ContinuousBudgetPct)
	}
}

func TestAgentConfigContinuousCooldownValidation(t *testing.T) {
	s, _ := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"continuousCooldown": 0})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{"continuousBudgetPct": 101})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
