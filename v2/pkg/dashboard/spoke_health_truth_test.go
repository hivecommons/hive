package dashboard

// Issue #2465: the spoke dashboard showed a green "Health OK" header pill and
// green sidebar agent dots while the spoke's own deep health checks reported
// agents down. Two weak signals were rendered where truth exists: the pill
// ignored the deep checks, and the sidebar dot only knew the 'stopped' state,
// so 'failed' (crash-looping) agents fell through to the green 'idle' class.
//
// These tests pin the server-side contract (the status payload carries the
// deep checks) and the UI wiring (pill and dot read the strong signal).

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// deepHealthAgentsCheck digs the "agents" check out of a decoded deepHealth
// map. Returns nil if absent.
func deepHealthAgentsCheck(t *testing.T, deep map[string]any) map[string]any {
	t.Helper()
	checks, ok := deep["checks"].([]any)
	if !ok {
		t.Fatalf("deepHealth.checks is %T, want array", deep["checks"])
	}
	for _, c := range checks {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if m["name"] == "agents" {
			return m
		}
	}
	return nil
}

// The /api/status payload must carry the deep health checks — the same ones
// the heartbeat reports to the hub — so the header pill can render them. A
// down (registered but never launched ⇒ StateStopped) agent must appear as a
// failing "agents" check with its name in the detail.
func TestStatusPayloadCarriesDeepHealth(t *testing.T) {
	srv := newFullServer(t)
	srv.MarkReady()
	// Age readiness past the health grace period so the agents check judges
	// the fleet for real instead of skipping it as "just booted".
	srv.statusMu.Lock()
	srv.readyAt = time.Now().Add(-2 * healthGracePeriod)
	srv.statusMu.Unlock()

	srv.UpdateStatus(minimalPayload())

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)

	var got struct {
		DeepHealth map[string]any `json:"deepHealth"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decoding /api/status: %v", err)
	}
	if got.DeepHealth == nil {
		t.Fatal("/api/status payload has no deepHealth — the header pill has nothing truthful to render (#2465)")
	}
	if got.DeepHealth["status"] != "degraded" {
		t.Errorf("deepHealth.status = %v, want degraded (scanner is registered but not running)", got.DeepHealth["status"])
	}
	agents := deepHealthAgentsCheck(t, got.DeepHealth)
	if agents == nil {
		t.Fatal("deepHealth.checks has no 'agents' check")
	}
	if agents["status"] != "fail" {
		t.Errorf("agents check status = %v, want fail", agents["status"])
	}
	detail, _ := agents["detail"].(string)
	if !strings.Contains(detail, "1 down: scanner") {
		t.Errorf("agents check detail = %q, want it to name the down agent (\"1 down: scanner\")", detail)
	}
}

// The deep snapshot embedded on the FIRST beat is judged against the payload
// being installed, not the not-yet-assigned s.status — otherwise every boot
// flashed a false "ready: fail" (and a red pill) until the second full update.
func TestDeepHealthFirstBeatReadyPasses(t *testing.T) {
	srv := newFullServer(t)
	srv.MarkReady()

	// First UpdateStatus ever: s.status is still nil when the snapshot is computed.
	srv.UpdateStatus(minimalPayload())

	// Read the installed payload through the same JSON the frontend sees —
	// the in-memory map holds a typed check slice, so round-trip it.
	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)

	var got struct {
		DeepHealth map[string]any `json:"deepHealth"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decoding /api/status: %v", err)
	}
	if got.DeepHealth == nil {
		t.Fatal("first beat has no deepHealth")
	}
	checks, ok := got.DeepHealth["checks"].([]any)
	if !ok {
		t.Fatalf("deepHealth.checks is %T, want array", got.DeepHealth["checks"])
	}
	for _, c := range checks {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if m["name"] == "ready" && m["status"] != "pass" {
			t.Errorf("first-beat ready check = %v, want pass — the snapshot must be judged against the payload being installed", m["status"])
		}
	}
}

// The refactor onto healthSummaryFor must not weaken the public path: an
// un-ready server (no MarkReady, no status) still reports a failing "ready"
// check through HealthSummary, exactly as the heartbeat relies on.
func TestHealthSummaryNotReadyStillFails(t *testing.T) {
	srv := newFullServer(t)

	raw, err := json.Marshal(srv.HealthSummary())
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	checks, ok := m["checks"].([]any)
	if !ok {
		t.Fatalf("checks is %T, want array", m["checks"])
	}
	found := false
	for _, c := range checks {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["name"] == "ready" {
			found = true
			if cm["status"] != "fail" {
				t.Errorf("ready check on un-ready server = %v, want fail", cm["status"])
			}
		}
	}
	if !found {
		t.Error("HealthSummary has no ready check")
	}
}

// UI wiring: the header pill and sidebar agent dots must read the strong
// signals. Snippet assertions in the style of gh_app_banner_test.go — they
// pin the load-bearing lines of the inline script.
func TestSpokeHealthTruthUIWiring(t *testing.T) {
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading static/index.html: %v", err)
	}
	html := string(b)

	required := []struct {
		snippet string
		why     string
	}{
		{
			snippet: "data.deepHealth",
			why:     "the header pill must render the deep checks from the status payload, not just the repo-workflow health map",
		},
		{
			snippet: "'● Degraded'",
			why:     "any failing deep check must flip the pill to Degraded",
		},
		{
			snippet: "const isDown = a.state !== 'running' && !isPaused && !isOff && !isOnDemand;",
			why:     "the sidebar dot must treat every non-running, non-paused process state (stopped, failed, idle) as down",
		},
		{
			snippet: "const needsLoginDown = (isDown && needsLogin) || a.needsLogin === true;",
			why:     "a down agent with an unauthenticated CLI must get the distinct needs-login state, not plain down-red",
		},
		{
			snippet: ".oc-agent-dot.needs-login { background: transparent; border: 2px solid var(--amber); box-sizing: border-box; }",
			why:     "the sidebar needs-login dot must render amber (warning), not green or red",
		},
		{
			snippet: "needs login — CLI unauthenticated",
			why:     "the needs-login state must carry a 'needs login' note in the tooltip",
		},
	}
	for _, r := range required {
		if !strings.Contains(html, r.snippet) {
			t.Errorf("static/index.html is missing %q — %s (#2465)", r.snippet, r.why)
		}
	}

	// The old weak signal must be gone: matching only 'stopped' let 'failed'
	// (crash-looping) agents render the green idle dot.
	if strings.Contains(html, "const isStopped = a.state === 'stopped'") {
		t.Error("static/index.html still derives the sidebar dot from the 'stopped'-only check — 'failed' agents would render green again (#2465)")
	}
}
