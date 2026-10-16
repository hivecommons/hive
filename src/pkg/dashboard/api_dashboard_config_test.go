package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDashboardSettingsAPIRoundTripAndMasksToken(t *testing.T) {
	s := govServer(t)
	s.deps.Config.Dashboard.AuthToken = "old-token"
	rec := doOwnerGet(s, "/api/config/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !json.Valid([]byte(body)) || body == "" || strings.Contains(body, "old-token") {
		t.Fatalf("GET leaked token or invalid JSON: %s", body)
	}
	put := doPut(s, "/api/config/dashboard", map[string]any{
		"port":                  8080,
		"agent_poll_interval_s": 7,
		"experiment_lab":        true,
		"auth_token":            "new-token",
		"authorized_users":      []string{"alice:owner", "bob:read"},
		"authorized_user_names": map[string]string{"alice": "Alice A."},
		"hub_proxied":           true,
		"issue_bands": map[string]any{
			"waiting_labels": []string{"waiting"},
			"done_labels":    []string{"done"},
			"stale_days":     14,
		},
	})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", put.Code, put.Body.String())
	}
	d := s.deps.Config.Dashboard
	if d.Port != 8080 || d.AgentPollIntervalS != 7 || !d.ExperimentLab || d.AuthToken != "new-token" || !d.HubProxied {
		t.Fatalf("dashboard scalar config not updated: %+v", d)
	}
	if len(d.AuthorizedUsers) != 2 || d.AuthorizedUserNames["alice"] != "Alice A." {
		t.Fatalf("access config not updated: %+v", d)
	}
	if d.IssueBands.StaleDays != 14 || len(d.IssueBands.WaitingLabels) != 1 || len(d.IssueBands.DoneLabels) != 1 {
		t.Fatalf("issue bands not updated: %+v", d.IssueBands)
	}
}

func TestDashboardSettingsAPIRejectsInvalidNumbers(t *testing.T) {
	s := govServer(t)
	s.deps.Config.Dashboard.Port = 3001
	for _, body := range []map[string]any{
		{"port": 70000},
		{"agent_poll_interval_s": 0},
		{"port": 8080, "issue_bands": map[string]any{"stale_days": -1}},
	} {
		rec := doPut(s, "/api/config/dashboard", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %#v => %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
	if s.deps.Config.Dashboard.Port != 3001 {
		t.Fatalf("invalid PUT mutated dashboard.port to %d", s.deps.Config.Dashboard.Port)
	}
}
