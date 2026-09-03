package dashboard

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func setupWSTest(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", filepath.Join(tmpDir, "contributors"))
	t.Setenv("HIVE_FEDERATION_REGISTRY_PATH", filepath.Join(tmpDir, "federation", "registry.json"))
	redirectContributeWSDisk(t, filepath.Join(tmpDir, "ws-state"))
	oldLedgerPersistence := taskLedgerPersistenceEnabled
	taskLedgerPersistenceEnabled = false
	t.Cleanup(func() { taskLedgerPersistenceEnabled = oldLedgerPersistence })

	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	ts := httptest.NewServer(s.mux)
	return s, ts
}

func wsURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/contribute/ws"
}

func readMsg(t *testing.T, conn *websocket.Conn) WSMessage {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var msg WSMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("ws unmarshal: %v", err)
	}
	return msg
}

func TestWSAuthChallenge(t *testing.T) {
	_, ts := setupWSTest(t)
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	msg := readMsg(t, conn)
	if msg.Type != "auth_challenge" {
		t.Fatalf("expected auth_challenge, got %s", msg.Type)
	}
	if msg.Nonce == "" {
		t.Fatal("missing nonce")
	}
}

func TestWSAuthValid(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// Register a contributor via API
	body := `{"github_username":"ws-auth-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)
	token := reg["registration_token"]

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Read challenge
	challenge := readMsg(t, conn)
	if challenge.Type != "auth_challenge" {
		t.Fatalf("expected auth_challenge, got %s", challenge.Type)
	}

	// Send auth response
	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: token,
		CLIBackend:        "claude",
		Model:             "opus-4-6",
	})

	// Read auth_ok
	authOk := readMsg(t, conn)
	if authOk.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOk.Type, authOk.Reason)
	}
	if authOk.ContributorID == "" {
		t.Fatal("missing contributor_id")
	}
	if authOk.ConnectionID == "" {
		t.Fatal("missing connection_id for relay/hub close-log correlation")
	}
	if authOk.TrustTier != "newcomer" {
		t.Fatalf("expected newcomer, got %s", authOk.TrustTier)
	}

	// Verify active count
	if s.contributeHub.ActiveCount() != 1 {
		t.Fatalf("expected 1 active, got %d", s.contributeHub.ActiveCount())
	}
}

func TestWSAuthInvalidToken(t *testing.T) {
	_, ts := setupWSTest(t)
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge

	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: "bogus-token-that-does-not-exist",
	})

	msg := readMsg(t, conn)
	if msg.Type != "auth_failed" {
		t.Fatalf("expected auth_failed, got %s", msg.Type)
	}
}

func TestWSAuthRevoked(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// Register then revoke
	body := `{"github_username":"revoked-ws-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	// Revoke (owner role required — the mutation endpoint fails closed on a
	// missing role, see requireContributorWrite / C5 fix).
	revokeReq := httptest.NewRequest(http.MethodPost, "/api/contributors/"+reg["contributor_id"]+"/revoke", nil)
	revokeReq.Header.Set("X-Hive-Role", "owner")
	revokeReq.Header.Set(ownerRoleVerifiedHeader, "true")
	revokeW := httptest.NewRecorder()
	s.mux.ServeHTTP(revokeW, revokeReq)

	// Connect
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge

	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: reg["registration_token"],
	})

	msg := readMsg(t, conn)
	if msg.Type != "auth_failed" {
		t.Fatalf("expected auth_failed, got %s", msg.Type)
	}
	if !strings.Contains(strings.ToLower(msg.Reason), "revok") {
		t.Fatalf("expected revoked reason, got %s", msg.Reason)
	}
}

func TestWSPingPong(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// Register
	body := `{"github_username":"ping-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok

	// Send ping
	conn.WriteJSON(WSMessage{Type: "ping", Seq: 42})
	pong := readMsg(t, conn)
	if pong.Type != "pong" {
		t.Fatalf("expected pong, got %s", pong.Type)
	}
	if pong.Seq != 42 {
		t.Fatalf("expected seq 42, got %d", pong.Seq)
	}
}

func TestWSReady(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"ready-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok

	// Send ready. With no status snapshot the hub now replies with an explicit
	// task_unavailable/hub_not_ready negative-ack instead of silence (#2546).
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	na := readMsg(t, conn)
	if na.Type != "task_unavailable" || na.Reason != taskUnavailableHubNotReady {
		t.Fatalf("expected task_unavailable/%s after ready with no status, got type=%s reason=%s",
			taskUnavailableHubNotReady, na.Type, na.Reason)
	}

	// Send ping to verify connection is still alive
	conn.WriteJSON(WSMessage{Type: "ping", Seq: 99})
	pong := readMsg(t, conn)
	if pong.Type != "pong" {
		t.Fatalf("expected pong after ready, got %s", pong.Type)
	}
}

func TestWSDisconnectCleansUp(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"disconnect-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	readMsg(t, conn)
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn)

	if s.contributeHub.ActiveCount() != 1 {
		t.Fatalf("expected 1 active before disconnect, got %d", s.contributeHub.ActiveCount())
	}

	conn.Close()
	time.Sleep(100 * time.Millisecond)

	if s.contributeHub.ActiveCount() != 0 {
		t.Fatalf("expected 0 active after disconnect, got %d", s.contributeHub.ActiveCount())
	}
}

func TestWSTaskCompleteUnassigned(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"task-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn)
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn)

	// Send task_complete for a task that was never assigned — should be ignored
	conn.WriteJSON(WSMessage{
		Type:    "task_complete",
		TaskID:  "FAKE-NEVER-ASSIGNED",
		Result:  "pr_created",
		Summary: "Fake completion",
	})

	time.Sleep(50 * time.Millisecond)
	p := findContributor(reg["contributor_id"])
	if p == nil {
		t.Fatal("contributor not found")
	}
	if p.TasksCompleted != 0 {
		t.Fatalf("expected 0 completed (unassigned task ignored), got %d", p.TasksCompleted)
	}
}

func TestWSAuthWithRole(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// Register a contributor
	body := `{"github_username":"role-auth-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)
	token := reg["registration_token"]

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge

	// Send auth response with role
	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: token,
		CLIBackend:        "claude",
		Model:             "opus-4-6",
		Role:              "scanner",
	})

	authOk := readMsg(t, conn)
	if authOk.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOk.Type, authOk.Reason)
	}
	if authOk.Role != "scanner" {
		t.Fatalf("expected role 'scanner' in auth_ok, got %q", authOk.Role)
	}

	// Verify the connection has the role stored
	conns := s.contributeHub.ActiveConnections()
	if len(conns) != 1 {
		t.Fatalf("expected 1 active connection, got %d", len(conns))
	}
	if conns[0].role != "scanner" {
		t.Fatalf("expected connection role 'scanner', got %q", conns[0].role)
	}
}

func TestWSAuthWithoutRole(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"norole-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge

	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: reg["registration_token"],
		CLIBackend:        "claude",
		Model:             "opus-4-6",
	})

	authOk := readMsg(t, conn)
	if authOk.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s", authOk.Type)
	}
	if authOk.Role != "" {
		t.Fatalf("expected empty role for task-driven mode, got %q", authOk.Role)
	}
}

func TestWSRoleBreakdown(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// Register three contributors
	usernames := []string{"role-breakdown-1", "role-breakdown-2", "role-breakdown-3"}
	tokens := make([]string, len(usernames))
	for i, u := range usernames {
		body := `{"github_username":"` + u + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		var reg map[string]string
		json.Unmarshal(w.Body.Bytes(), &reg)
		tokens[i] = reg["registration_token"]
	}

	// Connect: user1 as scanner, user2 as reviewer, user3 as task-driven (no role)
	roles := []string{"scanner", "reviewer", ""}
	conns := make([]*websocket.Conn, len(usernames))
	for i, tok := range tokens {
		c, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer c.Close()
		conns[i] = c

		readMsg(t, c) // challenge
		msg := WSMessage{
			Type:              "auth_response",
			RegistrationToken: tok,
			CLIBackend:        "claude",
			Model:             "opus-4-6",
		}
		if roles[i] != "" {
			msg.Role = roles[i]
		}
		c.WriteJSON(msg)
		readMsg(t, c) // auth_ok
	}

	// Check role breakdown
	breakdown := s.contributeHub.RoleBreakdown()
	if breakdown["scanner"] != 1 {
		t.Fatalf("expected 1 scanner, got %d", breakdown["scanner"])
	}
	if breakdown["reviewer"] != 1 {
		t.Fatalf("expected 1 reviewer, got %d", breakdown["reviewer"])
	}
	if breakdown["task-driven"] != 1 {
		t.Fatalf("expected 1 task-driven, got %d", breakdown["task-driven"])
	}

	// Check pool status includes role breakdown
	poolStatus := s.BuildContributorPoolStatus()
	if poolStatus.Active != 3 {
		t.Fatalf("expected 3 active, got %d", poolStatus.Active)
	}
	if poolStatus.ByRole == nil {
		t.Fatal("expected non-nil ByRole in pool status")
	}
	if poolStatus.ByRole["scanner"] != 1 {
		t.Fatalf("pool status: expected 1 scanner, got %d", poolStatus.ByRole["scanner"])
	}
}

func TestWSRoleBasedReady(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"role-ready-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: reg["registration_token"],
		CLIBackend:        "claude",
		Model:             "opus-4-6",
		Role:              "ci-maintainer",
	})
	readMsg(t, conn) // auth_ok

	// Send ready. With no status snapshot the hub replies with an explicit
	// task_unavailable/hub_not_ready negative-ack (#2546) rather than silence;
	// role-based contributors still get no task_assign.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	na := readMsg(t, conn)
	if na.Type != "task_unavailable" || na.Reason != taskUnavailableHubNotReady {
		t.Fatalf("expected task_unavailable/%s after role-based ready, got type=%s reason=%s",
			taskUnavailableHubNotReady, na.Type, na.Reason)
	}

	// Verify connection is still alive
	conn.WriteJSON(WSMessage{Type: "ping", Seq: 99})
	pong := readMsg(t, conn)
	if pong.Type != "pong" {
		t.Fatalf("expected pong after role-based ready, got %s", pong.Type)
	}
}

func TestContributorProfilePreferredRole(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", tmpDir)

	profile, _ := createContributorProfile("role-pref-user")
	profile.PreferredRole = "scanner"
	if err := saveContributorProfile(profile); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadContributorProfile("role-pref-user")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.PreferredRole != "scanner" {
		t.Fatalf("expected preferred_role 'scanner', got %q", loaded.PreferredRole)
	}
}

// Auto-promotion grants contents:write and pulls:write, so it must not fire on
// completions that never reported a pull request. Completion is self-reported:
// an agent that goes idle without shipping anything still sends task_complete.
func TestWSAutoPromotionRequiresReportedPR(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// #2565: PR-backed promotion now requires server-side verification. Install a
	// GitHub mock answering the pulls lookup with a PR in acme/widgets authored by
	// "promote-user", matching the completions this test reports, so the verified
	// path can legitimately grant credit + promotion.
	s.deps = verifyDepsFor(t, "acme", "widgets", "promote-user")

	body := `{"github_username":"promote-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn)
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn)

	// complete drives one task through the hub as if it had been assigned,
	// reporting a PR only when prURL is non-empty.
	complete := func(taskID, prURL string, wantCompleted, wantWithPR int, wantTier string) {
		t.Helper()
		s.contributeHub.mu.RLock()
		var c *ContributorConnection
		for _, candidate := range s.contributeHub.connections {
			c = candidate
		}
		s.contributeHub.mu.RUnlock()
		if c == nil {
			t.Fatal("no live contributor connection")
		}
		c.mu.Lock()
		c.currentTask = &WSTaskAssign{TaskID: taskID, Kind: "issue", Repo: "acme/widgets", Number: 1}
		c.mu.Unlock()

		if err := conn.WriteJSON(WSMessage{Type: "task_complete", TaskID: taskID, Result: "done", PRURL: prURL}); err != nil {
			t.Fatalf("write task_complete: %v", err)
		}
		waitFor(t, func() bool {
			p := findContributor(reg["contributor_id"])
			if p == nil {
				return false
			}
			if p.TasksCompleted < wantCompleted || p.TasksWithPR < wantWithPR {
				return false
			}
			return wantTier == "" || p.TrustTier == wantTier
		}, fmt.Sprintf("completion %s to be recorded", taskID))
	}

	for i := 0; i < contributorAutoPromoteAt; i++ {
		complete(fmt.Sprintf("no-pr-%d", i), "", i+1, 0, "newcomer")
	}

	p := findContributor(reg["contributor_id"])
	if p == nil {
		t.Fatal("contributor not found")
	}
	if p.TasksCompleted != contributorAutoPromoteAt {
		t.Fatalf("expected %d completions recorded, got %d", contributorAutoPromoteAt, p.TasksCompleted)
	}
	if p.TasksWithPR != 0 {
		t.Fatalf("expected 0 completions with a PR, got %d", p.TasksWithPR)
	}
	if p.TrustTier != "newcomer" {
		t.Fatalf("promoted to %q on completions that reported no PR", p.TrustTier)
	}

	for i := 0; i < contributorAutoPromoteAt; i++ {
		complete(fmt.Sprintf("with-pr-%d", i), "https://github.com/acme/widgets/pull/7", contributorAutoPromoteAt+i+1, i+1, "")
	}

	p = findContributor(reg["contributor_id"])
	if p.TasksWithPR != contributorAutoPromoteAt {
		t.Fatalf("expected %d completions with a PR, got %d", contributorAutoPromoteAt, p.TasksWithPR)
	}
	if p.TrustTier != "contributor" {
		t.Fatalf("expected promotion to contributor after %d PR-backed completions, got %q", contributorAutoPromoteAt, p.TrustTier)
	}
}
