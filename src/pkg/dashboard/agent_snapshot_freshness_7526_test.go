package dashboard

import (
	"encoding/json"
	"testing"
)

// #7526: /api/status serves a cached snapshot that used to be refreshed only by
// the eval cycle, which enumerates every configured repo against the GitHub API
// first. On a rate-limited spoke that cycle runs far past its nominal interval,
// so agents the manager started minutes ago still read state=stopped and the
// dashboard paints the whole fleet red. The fast agent-only tick must refresh
// the agent block of the served snapshot.
func TestBroadcastAgentStatusRefreshesServedSnapshot(t *testing.T) {
	s := newTestServer()

	stale := minimalPayload()
	stale.Agents = []FrontendAgent{
		{Name: "reviewer", State: "stopped", Busy: "idle"},
		{Name: "scanner", State: "stopped", Busy: "idle"},
	}
	s.UpdateStatus(stale)

	// The 10s agent tick: manager reports both agents running.
	s.BroadcastAgentStatus(&AgentStatusPayload{
		Agents: []FrontendAgent{
			{Name: "reviewer", State: "running", Busy: "working", Session: "hive-reviewer"},
			{Name: "scanner", State: "running", Busy: "idle", Session: "hive-scanner"},
		},
		ConfiguredAgents: []FrontendConfiguredAgent{{Name: "reviewer"}, {Name: "scanner"}},
	})

	rec := doGet(s, "/api/status")
	if rec.Code != 200 {
		t.Fatalf("GET /api/status = %d, want 200", rec.Code)
	}
	var got StatusPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(got.Agents))
	}
	for _, a := range got.Agents {
		if a.State != "running" {
			t.Errorf("agent %s state = %q, want %q — /api/status is serving "+
				"eval-cycle-stale liveness (#7526)", a.Name, a.State, "running")
		}
	}
	if len(got.ConfiguredAgents) != 2 {
		t.Errorf("configuredAgents = %d, want 2", len(got.ConfiguredAgents))
	}
}

// The refresh must be copy-on-write: a payload already handed to a reader (as
// handleStatus does, marshalling outside statusMu) must not be mutated.
func TestRefreshAgentSnapshotIsCopyOnWrite(t *testing.T) {
	s := newTestServer()
	base := minimalPayload()
	base.Agents = []FrontendAgent{{Name: "reviewer", State: "stopped"}}
	s.UpdateStatus(base)

	s.statusMu.RLock()
	held := s.status
	s.statusMu.RUnlock()

	s.RefreshAgentSnapshot(&AgentStatusPayload{
		Agents: []FrontendAgent{{Name: "reviewer", State: "running"}},
	})

	if held.Agents[0].State != "stopped" {
		t.Errorf("previously loaded snapshot was mutated in place: state = %q", held.Agents[0].State)
	}
	s.statusMu.RLock()
	current := s.status
	s.statusMu.RUnlock()
	if current == held {
		t.Fatal("snapshot pointer unchanged, want copy-on-write replacement")
	}
	if current.Agents[0].State != "running" {
		t.Errorf("current snapshot state = %q, want running", current.Agents[0].State)
	}
	if current.HiveID != base.HiveID || current.Governor.Mode != base.Governor.Mode {
		t.Error("non-agent fields lost by the agent-block patch")
	}
}

// A nil cached snapshot (pre-first-eval boot) must not panic or fabricate one:
// handleStatus's "initializing" response stays intact.
func TestRefreshAgentSnapshotNoopsBeforeFirstStatus(t *testing.T) {
	s := newTestServer()
	s.RefreshAgentSnapshot(&AgentStatusPayload{Agents: []FrontendAgent{{Name: "reviewer", State: "running"}}})
	s.RefreshAgentSnapshot(nil)

	s.statusMu.RLock()
	got := s.status
	s.statusMu.RUnlock()
	if got != nil {
		t.Fatalf("status = %#v, want nil before the first full snapshot", got)
	}
}
