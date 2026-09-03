package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Issue #3011 (High, authorization bypass / privilege escalation).
// PUT /api/contributors/{id}/agent-role had TWO independent defects:
//
//  1. It was gated on requireContributorWrite, which admits any caller whose
//     role is not "read" — so a read-write contributor could assign agent
//     roles. Its sibling .../agent-role-grants was moved to requireOwnerRole
//     by F16; this half was left behind, so the issue was half-fixed and still
//     live on v4.
//
//  2. Independent of the gate: the handler PRE-POPULATED its probe profile
//     with the very grant it was about to check, so roleClaimAllowed's
//     "requires an operator grant" branch passed trivially, and the grant was
//     then written to the REAL profile. That auto-granted the privileged roles
//     (sec-check, architect, ci-maintainer) as a side effect of assigning them.
//
// Fixing only the gate would leave an owner able to trip the bypass by
// accident and leave it one refactor from reachable, so both are fixed and
// both are pinned here.

// seed3011Contributor writes a trusted contributor holding NO agent-role
// grants, which is the precondition the bypass exploited.
func seed3011Contributor(t *testing.T) {
	t.Helper()
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "alice",
		ContributorID:  "c-alice",
		TrustTier:      "trusted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func put3011AgentRole(s *Server, id, body string, mark func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/contributors/"+id+"/agent-role", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if mark != nil {
		mark(req)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// --- DEFECT 1: THE ROLE GATE -------------------------------------------------

// TestIssue3011AgentRoleRejectsNonOwner is the negative half of the gate.
func TestIssue3011AgentRoleRejectsNonOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark func(*http.Request)
	}{
		{"no role headers at all", nil},
		{"read-write session", func(r *http.Request) { r.Header.Set("X-Hive-Role", "read-write") }},
		{"merger session", func(r *http.Request) { r.Header.Set("X-Hive-Role", "merger") }},
		// A client-supplied owner role WITHOUT the server-only verification
		// marker must not pass; a test sending only X-Hive-Role: owner would
		// otherwise pass for the wrong reason.
		{"owner role header without the verified marker", func(r *http.Request) {
			r.Header.Set("X-Hive-Role", "owner")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupContributeEnv(t)
			seed3011Contributor(t)
			s, deps := apiServer(t)
			seedAssignableRoleConfig(deps)

			rec := put3011AgentRole(s, "c-alice", `{"agent_role":"scanner"}`, tc.mark)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("PUT agent-role as %s got %d, want 403 — role assignment is owner-only (issue #3011)",
					tc.name, rec.Code)
			}
			if p := findContributor("c-alice"); p != nil && normalizeAgentRole(p.AssignedAgentRole) != "" {
				t.Fatalf("refused with 403 but the role was still assigned: %+v", p)
			}
		})
	}
}

// TestIssue3011AgentRoleAcceptsOwner is the positive control for the gate:
// without it, a gate that rejected everything would satisfy the test above
// while breaking the ops tab's role-assignment control outright.
func TestIssue3011AgentRoleAcceptsOwner(t *testing.T) {
	setupContributeEnv(t)
	seed3011Contributor(t)
	s, deps := apiServer(t)
	seedAssignableRoleConfig(deps)

	rec := put3011AgentRole(s, "c-alice", `{"agent_role":"scanner"}`, markOwnerRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner PUT agent-role got %d, want 200 — the gate is over-broad and owners can "+
			"no longer assign agent roles; body=%s", rec.Code, rec.Body.String())
	}
	p := findContributor("c-alice")
	if p == nil || p.AssignedAgentRole != "scanner" {
		t.Fatalf("owner assignment did not persist: %+v", p)
	}
}

// --- DEFECT 2: THE AUTO-GRANT BYPASS -----------------------------------------

// TestIssue3011NoAutoGrantOnAssign is the core of the second defect. Every role
// in roleClaimNeedsGrant must be REFUSED for a target holding no grant, and —
// critically — nothing may be written to the persisted profile. The original
// bug wrote the grant as a side effect, so asserting the HTTP status alone
// would not have caught it.
//
// Note the caller here is a fully-authorized OWNER: this defect is independent
// of the gate, which is exactly why fixing the gate alone is insufficient.
func TestIssue3011NoAutoGrantOnAssign(t *testing.T) {
	for _, role := range []string{"sec-check", "architect", "ci-maintainer"} {
		t.Run(role, func(t *testing.T) {
			setupContributeEnv(t)
			seed3011Contributor(t)
			s, deps := apiServer(t)
			seedAssignableRoleConfig(deps)
			// Make every privileged role delegatable and enabled, so the ONLY
			// thing standing between the caller and the role is the grant.
			// Otherwise this test could pass for the wrong reason — refused by
			// the delegatable-roles check rather than by the grant check.
			deps.Config.Agents[role] = agentConfigFor(role)
			deps.Config.Hub.ContributeDelegatableRoles = []string{
				"scanner", "quality", "outreach", "ci-maintainer", "sec-check", "architect",
			}

			rec := put3011AgentRole(s, "c-alice", `{"agent_role":"`+role+`"}`, markOwnerRequest)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("assigning %q to a contributor with NO grant got %d, want 400 — "+
					"the auto-grant bypass is back (issue #3011); body=%s", role, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "grant") {
				t.Errorf("refusal should name the missing grant; body=%s", rec.Body.String())
			}

			// The persisted profile must be untouched in BOTH respects.
			p := findContributor("c-alice")
			if p == nil {
				t.Fatalf("contributor vanished")
			}
			if hasAgentRoleGrant(p, role) {
				t.Fatalf("handler refused the assignment but STILL auto-granted %q to the "+
					"persisted profile — this is the #3011 escalation: %+v", role, p)
			}
			if normalizeAgentRole(p.AssignedAgentRole) == role {
				t.Fatalf("handler refused the assignment but STILL assigned %q: %+v", role, p)
			}
		})
	}
}

// TestIssue3011GrantedRoleStillAssignable is the positive control for the
// bypass fix: a target who legitimately HAS the grant must still be
// assignable, so "deny every privileged role" cannot pass the test above.
func TestIssue3011GrantedRoleStillAssignable(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername:  "alice",
		ContributorID:   "c-alice",
		TrustTier:       "trusted",
		AgentRoleGrants: []string{"ci-maintainer"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, deps := apiServer(t)
	seedAssignableRoleConfig(deps)

	rec := put3011AgentRole(s, "c-alice", `{"agent_role":"ci-maintainer"}`, markOwnerRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("assigning a role the contributor HOLDS A GRANT for got %d, want 200 — the "+
			"fix over-corrected and explicit operator grants no longer work; body=%s",
			rec.Code, rec.Body.String())
	}
	p := findContributor("c-alice")
	if p == nil || p.AssignedAgentRole != "ci-maintainer" {
		t.Fatalf("granted assignment did not persist: %+v", p)
	}
}

// TestIssue3011UnprivilegedRolesNeedNoGrant is the second positive control: the
// ordinary roles are NOT in roleClaimNeedsGrant and must stay assignable
// without any grant, so the grant check cannot be over-applied.
func TestIssue3011UnprivilegedRolesNeedNoGrant(t *testing.T) {
	for _, role := range []string{"scanner", "quality"} {
		t.Run(role, func(t *testing.T) {
			setupContributeEnv(t)
			seed3011Contributor(t)
			s, deps := apiServer(t)
			seedAssignableRoleConfig(deps)

			rec := put3011AgentRole(s, "c-alice", `{"agent_role":"`+role+`"}`, markOwnerRequest)
			if rec.Code != http.StatusOK {
				t.Fatalf("assigning unprivileged role %q got %d, want 200 — the grant check is "+
					"over-applied; body=%s", role, rec.Code, rec.Body.String())
			}
			if p := findContributor("c-alice"); p == nil || p.AssignedAgentRole != role {
				t.Fatalf("assignment did not persist: %+v", p)
			}
		})
	}
}

// --- SOURCE-ASSERTING INVARIANT ----------------------------------------------

// TestIssue3011AgentRoleHandlersAreOwnerGated pins BOTH endpoints at the source
// level. The grants half was already fixed by F16 and regressing it would
// re-open this issue's other half, so both are asserted together.
func TestIssue3011AgentRoleHandlersAreOwnerGated(t *testing.T) {
	src := f16ReadSource(t, "api_contribute.go")
	for _, name := range []string{
		"handleContributorAgentRole",
		"handleContributorAgentRoleGrants",
	} {
		body := f16HandlerBody(t, src, name)
		if !strings.Contains(body, "requireOwnerRole(w, r)") {
			t.Errorf("%s has no requireOwnerRole gate — a read-write contributor can assign or "+
				"grant privileged agent roles (issue #3011)", name)
		}
		if strings.Contains(body, "s.requireContributorWrite(w, r)") {
			t.Errorf("%s gates on requireContributorWrite, which admits RoleReadWrite; agent-role "+
				"administration is owner-only (issue #3011)", name)
		}
	}
}

// TestIssue3011ProbeIsNotPreSeededWithTheGrant asserts the bypass cannot come
// back by shape. The defect was a single append onto a COPY of the profile
// immediately before the check that reads it; a behavioural test catches
// today's version, this catches a re-introduction that a refactor makes
// reachable again.
func TestIssue3011ProbeIsNotPreSeededWithTheGrant(t *testing.T) {
	body := f16HandlerBody(t, f16ReadSource(t, "api_contribute.go"), "handleContributorAgentRole")

	if strings.Contains(body, "probeProfile") {
		t.Error("handleContributorAgentRole reconstructs a probeProfile — the #3011 bypass " +
			"pre-populated a copied profile with the grant it was about to check, making " +
			"roleClaimAllowed tautological. Probe with the real profile instead.")
	}
	// The handler must never WRITE a grant. Issuance belongs to
	// handleContributorAgentRoleGrants alone.
	if strings.Contains(body, "AgentRoleGrants = append") {
		t.Error("handleContributorAgentRole appends to AgentRoleGrants — this handler must never " +
			"issue a grant; that is handleContributorAgentRoleGrants' job (issue #3011)")
	}
}

// TestIssue3011GrantIssuanceStaysWithTheGrantsHandler is the positive control
// for the source assertion above: the grants handler is the one place that IS
// supposed to write grants, so "no handler may ever write a grant" cannot be
// the reading of the rule.
func TestIssue3011GrantIssuanceStaysWithTheGrantsHandler(t *testing.T) {
	body := f16HandlerBody(t, f16ReadSource(t, "api_contribute.go"), "handleContributorAgentRoleGrants")
	if !strings.Contains(body, "p.AgentRoleGrants = grants") {
		t.Error("handleContributorAgentRoleGrants no longer writes grants — grant issuance must " +
			"still exist as an explicit owner action, otherwise privileged roles become " +
			"unassignable entirely")
	}
}

// agentConfigFor builds an enabled AgentConfig for a role, so a test can make a
// privileged role fully available and isolate the grant check.
func agentConfigFor(role string) config.AgentConfig {
	return config.AgentConfig{Role: role, Enabled: true}
}
