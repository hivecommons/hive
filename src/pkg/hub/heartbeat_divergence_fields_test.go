package hub

import (
	"net/http"
	"testing"
)

// storedAgents reaches the registry entry's Agents slice for a hive id.
func storedAgents(t *testing.T, s *HubServer, id string) []AgentSummary {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == id {
			return h.Agents
		}
	}
	t.Fatalf("hive %q not in registry", id)
	return nil
}

// The new fleet-divergence fields must survive handleHeartbeat: Backend is
// sanitized like an identifier, and the boolean signals pass through unchanged.
// A field that is silently dropped on receive is the classic wire-integration
// miss this test exists to catch.
func TestHeartbeatDivergenceFields_Stored(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	body := `{
		"hive_id":"h1",
		"agents":[
			{"name":"scanner","state":"running","backend":"claude",
			 "enabled":true,"expectedActive":true,
			 "canOpenIssue":true,"canOpenPR":true,"canMerge":true},
			{"name":"advisor","state":"running","backend":"copilot",
			 "enabled":true,"expectedActive":false,
			 "canOpenIssue":true,"canOpenPR":false,"canMerge":false}
		]
	}`
	rec := postHeartbeat(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	agents := storedAgents(t, s, "h1")
	if len(agents) != 2 {
		t.Fatalf("stored %d agents, want 2", len(agents))
	}
	byName := map[string]AgentSummary{}
	for _, a := range agents {
		byName[a.Name] = a
	}

	sc := byName["scanner"]
	if sc.Backend != "claude" {
		t.Errorf("scanner backend = %q, want claude", sc.Backend)
	}
	if !sc.Enabled || !sc.ExpectedActive || !sc.CanOpenIssue || !sc.CanOpenPR || !sc.CanMerge {
		t.Errorf("scanner divergence bools dropped: %+v", sc)
	}

	ad := byName["advisor"]
	if ad.ExpectedActive || ad.CanOpenPR || ad.CanMerge {
		t.Errorf("advisor false bools flipped: %+v", ad)
	}
	if !ad.CanOpenIssue || !ad.Enabled {
		t.Errorf("advisor true bools dropped: %+v", ad)
	}
}

// A malicious backend string must be sanitized like every other identifier
// field on the agent row (no angle brackets / injection survives).
func TestHeartbeatDivergenceFields_SanitizesBackend(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	body := `{"hive_id":"h1","agents":[
		{"name":"scanner","state":"running","backend":"clau<script>de"}
	]}`
	rec := postHeartbeat(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	agents := storedAgents(t, s, "h1")
	if len(agents) != 1 {
		t.Fatalf("stored %d agents, want 1", len(agents))
	}
	if got := agents[0].Backend; got == "clau<script>de" {
		t.Errorf("backend was not sanitized: %q", got)
	}
}
