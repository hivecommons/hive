package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/config"
)

func seedAssignableRoleConfig(deps *Dependencies) {
	deps.Config.Agents["scanner"] = config.AgentConfig{Role: "scanner", Enabled: true}
	deps.Config.Agents["quality"] = config.AgentConfig{Role: "quality", Enabled: true, LaneKeywords: []string{"quality"}}
	deps.Config.Agents["ci-maintainer"] = config.AgentConfig{Role: "ci-maintainer", Enabled: true}
	deps.Config.Hub.ContributeDelegatableRoles = []string{"scanner", "quality", "outreach", "ci-maintainer"}
}

func putAssignedRole(t *testing.T, s *Server, id, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/contributors/"+id+"/agent-role", strings.NewReader(`{"agent_role":"`+role+`"}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestContributorAgentRoleAssignmentValidationAndPersistence(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "trusted"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, deps := apiServer(t)
	seedAssignableRoleConfig(deps)

	deps.Config.Hub.ContributeDelegatableRoles = []string{"scanner", "quality", "outreach"}
	rec := putAssignedRole(t, s, "c-alice", "ci-maintainer")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("allow-list off got %d, want 400", rec.Code)
	}

	deps.Config.Hub.ContributeDelegatableRoles = []string{"scanner", "quality", "outreach", "ci-maintainer"}
	p := findContributor("c-alice")
	p.TrustTier = "contributor"
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save tier: %v", err)
	}
	rec = putAssignedRole(t, s, "c-alice", "ci-maintainer")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tier too low got %d, want 400", rec.Code)
	}

	p = findContributor("c-alice")
	p.TrustTier = "trusted"
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save trusted: %v", err)
	}
	rec = putAssignedRole(t, s, "c-alice", "supervisor")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("supervisor got %d, want 400", rec.Code)
	}

	// Issue #3011: this block used to assert the AUTO-GRANT — it expected 200
	// and a profile that had silently acquired the ci-maintainer grant
	// ("assignment did not persist with auto-grant"). That behaviour WAS the
	// vulnerability, so the assertions are INVERTED rather than deleted.
	// ci-maintainer is in roleClaimNeedsGrant, and alice holds no grant, so the
	// assignment must now be REFUSED and must write nothing.
	rec = putAssignedRole(t, s, "c-alice", "ci-maintainer")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("assign ci-maintainer without a grant got %d, want 400 — the auto-grant "+
			"bypass is back (issue #3011); body: %s", rec.Code, rec.Body.String())
	}
	p = findContributor("c-alice")
	if hasAgentRoleGrant(p, "ci-maintainer") {
		t.Fatalf("refused the assignment but still wrote the grant to the profile: %+v", p)
	}
	if p.AssignedAgentRole == "ci-maintainer" {
		t.Fatalf("refused the assignment but still assigned the role: %+v", p)
	}

	// The legitimate path: an owner grants the role explicitly, and only then
	// can it be assigned.
	grantReq := httptest.NewRequest(http.MethodPut, "/api/contributors/c-alice/agent-role-grants", strings.NewReader(`{"agent_role_grants":["ci-maintainer"]}`))
	grantReq.Header.Set("Content-Type", "application/json")
	markOwnerRequest(grantReq)
	grantRec := httptest.NewRecorder()
	s.mux.ServeHTTP(grantRec, grantReq)
	if grantRec.Code != http.StatusOK {
		t.Fatalf("explicit grant got %d, want 200: %s", grantRec.Code, grantRec.Body.String())
	}
	rec = putAssignedRole(t, s, "c-alice", "ci-maintainer")
	if rec.Code != http.StatusOK {
		t.Fatalf("assign ci-maintainer after an explicit grant got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	p = findContributor("c-alice")
	if p.AssignedAgentRole != "ci-maintainer" || !hasAgentRoleGrant(p, "ci-maintainer") {
		t.Fatalf("granted assignment did not persist: %+v", p)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/contributors/c-alice/agent-role-grants", strings.NewReader(`{"agent_role_grants":[]}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant replacement got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if p = findContributor("c-alice"); !hasAgentRoleGrant(p, "ci-maintainer") {
		t.Fatalf("active privileged assignment lost its required grant: %+v", p)
	}

	rec = putAssignedRole(t, s, "c-alice", "none")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear-to-none got %d, want 200", rec.Code)
	}
	p = findContributor("c-alice")
	if p.AssignedAgentRole != "none" {
		t.Fatalf("clear-to-none persisted %q, want none", p.AssignedAgentRole)
	}
}

func TestOwnerAssignmentPrecedenceAndReconnect(t *testing.T) {
	setupContributeEnv(t)
	token := "tok-" + randomHex(8)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername:    "alice",
		ContributorID:     "c-alice",
		RegistrationToken: sha256Hex(token),
		TrustTier:         "contributor",
		AssignedAgentRole: "scanner",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, deps := apiServer(t)
	seedAssignableRoleConfig(deps)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	dial := func(role string) (*websocket.Conn, WSMessage) {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if msg := readMsg(t, conn); msg.Type != "auth_challenge" {
			t.Fatalf("challenge = %+v", msg)
		}
		if err := conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude", Model: "sonnet", Role: role}); err != nil {
			t.Fatalf("auth write: %v", err)
		}
		return conn, readMsg(t, conn)
	}

	conn, auth := dial("quality")
	if auth.Type != "auth_ok" || auth.Role != "scanner" {
		t.Fatalf("owner assignment should beat client claim, got %+v", auth)
	}
	snap := s.contributeHub.FleetSnapshot()
	if len(snap.Clankers) != 1 || snap.Clankers[0].Role != "scanner" || snap.Clankers[0].ClientRole != "quality" || !strings.Contains(snap.Clankers[0].RoleMismatch, "client requested quality") {
		t.Fatalf("fleet mismatch fields not surfaced: %+v", snap.Clankers)
	}
	conn.Close()

	conn2, auth2 := dial("")
	defer conn2.Close()
	if auth2.Type != "auth_ok" || auth2.Role != "scanner" {
		t.Fatalf("persisted owner assignment not used on reconnect: %+v", auth2)
	}
}

func TestAssignedRoleAppliesNextTaskNotCurrent(t *testing.T) {
	hub, _ := roleTestHub(t)
	setStatusIssues(hub.server,
		map[string]any{"number": float64(1), "title": "ordinary bug", "author": "bob"},
		map[string]any{"number": float64(2), "title": "quality regression", "author": "bob", "labels": []any{"quality"}, "lane": "quality"},
	)
	conn := &ContributorConnection{
		profile:      &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		role:         "scanner",
		assignedRole: "scanner",
		currentTask:  &WSTaskAssign{TaskID: "old", Role: "scanner", Repo: "myorg/repo1", Number: 1, Title: "ordinary bug"},
		lastPong:     time.Now(),
	}
	hub.mu.Lock()
	hub.connections["c"] = conn
	hub.mu.Unlock()

	hub.SetAssignedAgentRole("c-alice", "quality", nil)
	if conn.currentTask == nil || conn.currentTask.Role != "scanner" {
		t.Fatalf("in-flight task was rewritten: %+v", conn.currentTask)
	}
	conn.mu.Lock()
	conn.currentTask = nil
	conn.mu.Unlock()
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" || msg.Role != "quality" || msg.Number != 2 {
		b, _ := json.Marshal(msg)
		t.Fatalf("next task did not use new role: %s", b)
	}
}

func TestAgentRoleGrantUpdateSyncsLiveConnection(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "trusted", AgentRoleGrants: []string{"ci-maintainer"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, deps := apiServer(t)
	seedAssignableRoleConfig(deps)
	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "trusted", AgentRoleGrants: []string{"ci-maintainer"}},
		role:     "ci-maintainer",
		lastPong: time.Now(),
	}
	s.contributeHub.mu.Lock()
	s.contributeHub.connections["live"] = conn
	s.contributeHub.mu.Unlock()

	req := httptest.NewRequest(http.MethodPut, "/api/contributors/c-alice/agent-role-grants", strings.NewReader(`{"agent_role_grants":[]}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant replacement got %d: %s", rec.Code, rec.Body.String())
	}
	if hasAgentRoleGrant(conn.profile, "ci-maintainer") {
		t.Fatalf("live connection kept removed privileged grant: %+v", conn.profile.AgentRoleGrants)
	}
	if ok, _ := s.contributeHub.roleClaimAllowed(conn, "ci-maintainer"); ok {
		t.Fatal("live connection can still claim removed privileged role")
	}
}
