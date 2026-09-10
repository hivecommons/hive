package dashboard

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// Security finding #6557: the agent-control mutations below reached
// AgentMgr/config writes for ANY authenticated non-read role — including
// handleKick, which types an operator-supplied prompt (up to 10,000 chars)
// directly into the agent's CLI session. A hub-authorized read-write
// contributor could switch an agent's backend/model, restart it, clear its
// restart counter, (un)pin its cli/model choice, or inject an arbitrary
// prompt, while sibling handlers of the same class (handlePause,
// handleResume, handleEffortSet, handleBreakerEngage/Release) were already
// owner-gated. Fixed by adding the same requireOwnerRole(w, r) guard these
// seven handlers were missing.

// agentControl6557Handlers names each patched handler and the path/method the
// dashboard actually uses to reach it (see routes registered in api.go).
func agentControl6557Handlers(srv *Server) []struct {
	name    string
	method  string
	path    string
	handler func(http.ResponseWriter, *http.Request)
} {
	return []struct {
		name    string
		method  string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"kick", http.MethodPost, "/api/kick/scanner", srv.handleKick},
		{"switch", http.MethodPost, "/api/switch/scanner/claude", srv.handleSwitch},
		{"model set", http.MethodPost, "/api/model/scanner/sonnet", srv.handleModelSet},
		{"restart", http.MethodPost, "/api/restart/scanner", srv.handleRestart},
		{"reset restarts", http.MethodPost, "/api/reset-restarts/scanner", srv.handleResetRestarts},
		{"pin", http.MethodPost, "/api/pin/scanner/cli", srv.handlePin},
		{"unpin", http.MethodPost, "/api/unpin/scanner/cli", srv.handleUnpin},
	}
}

// TestAgentControlHandlersRejectNonOwnerRoles is the direct 6557 regression:
// each of the seven previously-ungated handlers must reject a read-write
// contributor with 403, exactly like handlePause/handleEffortSet already do.
// Uses a bare *Server{} (no deps) — requireOwnerRole must return before any
// handler touches s.deps, the same assumption
// TestV4OwnerOnlyHandlerGapsRejectUnverifiedOwners relies on.
func TestAgentControlHandlersRejectNonOwnerRoles(t *testing.T) {
	srv := &Server{}

	roles := []string{"read-write", "merger", "owner"} // "owner" here is proofless — no ownerRoleVerifiedHeader.
	for _, tc := range agentControl6557Handlers(srv) {
		for _, role := range roles {
			t.Run(tc.name+"/"+role, func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("handler panicked for unverified request: %v; want status %d", r, http.StatusForbidden)
					}
				}()
				req := httptest.NewRequest(tc.method, tc.path, nil)
				req.Header.Set("X-Hive-Role", role)
				req.SetPathValue("agent", "scanner")
				req.SetPathValue("backend", "claude")
				req.SetPathValue("model", "sonnet")
				req.SetPathValue("dimension", "cli")
				w := httptest.NewRecorder()

				tc.handler(w, req)

				if w.Code != http.StatusForbidden {
					t.Fatalf("%s: role %q status = %d, want %d (owner access required)", tc.name, role, w.Code, http.StatusForbidden)
				}
			})
		}
	}
}

// TestAgentControlHandlersRejectMissingRole covers the no-header case
// (mirrors TestNousMutationHandlersRejectMissingRole): a request that never
// went through authenticate at all must still be denied, not just one that
// carries an explicit non-owner role.
func TestAgentControlHandlersRejectMissingRole(t *testing.T) {
	srv := &Server{}
	for _, tc := range agentControl6557Handlers(srv) {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.SetPathValue("agent", "scanner")
			req.SetPathValue("backend", "claude")
			req.SetPathValue("model", "sonnet")
			req.SetPathValue("dimension", "cli")
			w := httptest.NewRecorder()

			tc.handler(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("%s: missing-role status = %d, want %d", tc.name, w.Code, http.StatusForbidden)
			}
		})
	}
}

// TestAgentControlHandlersPassOwnerGateForVerifiedOwner is the positive
// control: a verified owner (role header + the server-set
// ownerRoleVerifiedHeader marker requireOwnerRole demands) must clear the
// gate and reach the handler's own logic, not be rejected at the door. Using
// an unknown agent lets each handler run past requireOwnerRole and fail on
// its own agent-not-found path (400/404) instead of asserting full mutation
// side effects, matching how budget_owner_live_role_4299_test.go and
// coverage_boost4_test.go probe the same marker.
func TestAgentControlHandlersPassOwnerGateForVerifiedOwner(t *testing.T) {
	srv := newFullServer(t)

	for _, tc := range agentControl6557Handlers(srv) {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("X-Hive-Role", "owner")
			req.Header.Set(ownerRoleVerifiedHeader, "true")
			req.SetPathValue("agent", "does-not-exist")
			req.SetPathValue("backend", "claude")
			req.SetPathValue("model", "sonnet")
			req.SetPathValue("dimension", "cli")
			w := httptest.NewRecorder()

			tc.handler(w, req)

			if w.Code == http.StatusForbidden {
				t.Fatalf("%s: verified owner status = %d, want the gate to pass (any non-403 outcome)", tc.name, w.Code)
			}
		})
	}
}

// TestAgentControl6557OwnerGateCountFloor is the blunt count-floor check from
// f16_owner_gate_test.go: it survives a handler rename that a name-based test
// would miss, and catches a sync merge dropping a gate even if nothing else
// in the diff looks suspicious.
func TestAgentControl6557OwnerGateCountFloor(t *testing.T) {
	body := f16ReadSource(t, "api.go")
	got := regexp.MustCompile(`requireOwnerRole\(w, r\)`).FindAllString(body, -1)
	// api.go already carries requireOwnerRole gates for handleEffortSet,
	// handleBreakerEngage and handleBreakerRelease (3) prior to this fix; this
	// fix adds exactly 7 more (kick, switch, model, restart, reset-restarts,
	// pin, unpin).
	const want = 3 + 7
	if len(got) < want {
		t.Errorf("api.go has %d requireOwnerRole gates, want at least %d — a gate was removed "+
			"(audit F14/F16 regressed exactly this way, via a sync merge)", len(got), want)
	}
}
