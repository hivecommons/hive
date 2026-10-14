package dashboard

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestGovernorFeatures_AutonomyPolicyPut covers the autonomy.* Settings
// controls (hivecommons/hive#11855): values persist, bad values are rejected
// before anything is mutated, and the Features tab renders every key.
func TestGovernorFeatures_AutonomyPolicyPut(t *testing.T) {
	s := covApiServer(t)

	for _, bad := range []map[string]any{
		{"autonomyPromoteAfter": -1},
		{"autonomyCooldownDays": -2},
		{"autonomyMaxLevel": 9},
		{"autonomyDemoteOn": "sometimes", "autonomyPromoteAfter": 5},
		{"autonomyMaxLevel": 0, "autonomyDemoteOn": " ROLLBACKS "},
	} {
		if rec := doPut(s, "/api/config/governor/features", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %v = %d, want 400", bad, rec.Code)
		}
	}
	if s.deps.Config.Autonomy.PromoteAfter != 0 {
		t.Fatal("a rejected PUT changed the config")
	}

	rec := doPut(s, "/api/config/governor/features", map[string]any{
		"autonomyPromoteAfter": 5, "autonomyCooldownDays": 14, "autonomyMaxLevel": 4, "autonomyDemoteOn": "Either",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d - %s", rec.Code, rec.Body.String())
	}
	a := s.deps.Config.Autonomy
	if a.PromoteAfter != 5 || a.CooldownDays != 14 || a.MaxLevel != 4 || a.DemoteOn != "either" {
		t.Fatalf("autonomy not persisted: %+v", a)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"autonomyDemoteOn": " Rework "}); rec.Code != http.StatusOK {
		t.Fatalf("PUT mixed-case demote_on = %d - %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Autonomy.DemoteOn; got != "rework" {
		t.Fatalf("demote_on not canonicalised: %q", got)
	}

	allow, err := os.ReadFile("settings_parity_allowlist.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(allow), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "autonomy.") {
			t.Errorf("settings_parity_allowlist.txt still allow-lists %q; the control exists now", strings.TrimSpace(line))
		}
	}

	html, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"autonomyAutoPromote", "autonomyAutoDemote", "autonomyPromoteAfter", "autonomyCooldownDays", "autonomyMaxLevel", "autonomyDemoteOn"} {
		if !strings.Contains(string(html), `data-key="`+key+`"`) && !strings.Contains(string(html), `data-arg1="`+key+`"`) {
			t.Errorf("Settings UI has no control for %s", key)
		}
	}
}
