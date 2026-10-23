package hub

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/crux"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// TestSpokeProvidersWireCompat pins that the spoke's providers block and the
// per-agent backend-auth fields decode into the hub's payload unchanged.
func TestSpokeProvidersWireCompat(t *testing.T) {
	refreshable := false
	in := spoke.HeartbeatPayload{
		Agents: []spoke.AgentSummary{{
			Name: "scanner", Backend: "claude", Enabled: true,
			BackendAuthStatus: crux.BackendAuthTokenExpired, BackendAuthSince: "2026-10-01T08:00:00Z",
			BackendAuthRefreshable: &refreshable,
		}},
		Providers: []crux.ProviderStatus{{
			Provider: "claude", AuthState: crux.ProviderAuthNeedsLogin, AvailabilityState: crux.ProviderAvailUnknown,
			Since: "2026-10-01T08:00:00Z", Evidence: "1 of 1 enabled agent: token-expired×1",
			NextAction: "Log in to the provider once.", AffectedAgents: []string{"scanner"},
		}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out HeartbeatPayload
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Agents) != 1 || out.Agents[0].BackendAuthStatus != crux.BackendAuthTokenExpired ||
		out.Agents[0].BackendAuthRefreshable == nil || *out.Agents[0].BackendAuthRefreshable {
		t.Fatalf("agent backend auth = %+v", out.Agents)
	}
	got := sanitizeProviderStatuses(out.Providers)
	if len(got) != 1 {
		t.Fatalf("providers = %+v", got)
	}
	p := got[0]
	if p.Provider != "claude" || p.AuthState != crux.ProviderAuthNeedsLogin || p.AvailabilityState != crux.ProviderAvailUnknown ||
		p.Since != "2026-10-01T08:00:00Z" || p.Evidence != in.Providers[0].Evidence || p.NextAction != in.Providers[0].NextAction ||
		len(p.AffectedAgents) != 1 || p.AffectedAgents[0] != "scanner" {
		t.Fatalf("sanitized provider = %+v", p)
	}

	// An old spoke sends no providers key: the hub keeps nil (no signal).
	raw, err = json.Marshal(spoke.HeartbeatPayload{})
	if err != nil {
		t.Fatal(err)
	}
	out = HeartbeatPayload{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if got := sanitizeProviderStatuses(out.Providers); got != nil {
		t.Fatalf("old spoke must leave providers nil, got %+v", got)
	}
}

func TestSanitizeProviderStatusesClosesVocabulary(t *testing.T) {
	in := []crux.ProviderStatus{
		{Provider: "claude<script>", AuthState: "totally-fine", AvailabilityState: "bogus", Evidence: "<b>x</b>", Since: "not-a-time"},
		{Provider: "", AuthState: crux.ProviderAuthUsable},
	}
	for i := 0; i < maxProviderStatuses+5; i++ {
		in = append(in, crux.ProviderStatus{Provider: "p", AuthState: crux.ProviderAuthUsable, AvailabilityState: crux.ProviderAvailable})
	}
	got := sanitizeProviderStatuses(in)
	if len(got) != maxProviderStatuses {
		t.Fatalf("len = %d, want cap %d", len(got), maxProviderStatuses)
	}
	first := got[0]
	if first.Provider != "claudescript" {
		t.Errorf("provider = %q", first.Provider)
	}
	if first.AuthState != crux.ProviderAuthUnknown || first.AvailabilityState != crux.ProviderAvailUnknown {
		t.Errorf("unrecognised states must become unknown, got %q / %q", first.AuthState, first.AvailabilityState)
	}
	if first.Evidence != "bx/b" || first.Since != "" {
		t.Errorf("evidence/since not sanitized: %+v", first)
	}
	if got[1].Provider != "p" {
		t.Errorf("empty provider must be dropped, got %+v", got[1])
	}
}

func TestHeartbeatStoresProviders(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	body := `{
		"hive_id":"h1",
		"agents":[{"name":"scanner","state":"running","backend":"claude","enabled":true,
			"backendAuthStatus":"token-expired","backendAuthRefreshable":true}],
		"providers":[{"provider":"claude","auth_state":"refreshable","availability_state":"unknown",
			"evidence":"1 of 1 enabled agent: token-expired×1","affected_agents":["scanner"]}]
	}`
	rec := postHeartbeat(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID != "h1" {
			continue
		}
		if len(h.Providers) != 1 || h.Providers[0].AuthState != crux.ProviderAuthRefreshable || h.Providers[0].AffectedAgents[0] != "scanner" {
			t.Fatalf("stored providers = %+v", h.Providers)
		}
		if len(h.Agents) != 1 || h.Agents[0].BackendAuthRefreshable == nil || !*h.Agents[0].BackendAuthRefreshable {
			t.Fatalf("stored agent refreshable = %+v", h.Agents)
		}
		return
	}
	t.Fatal("hive h1 not in registry")
}
