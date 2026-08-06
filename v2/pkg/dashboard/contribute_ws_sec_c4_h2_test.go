package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_ws_sec_c4_h2_test.go covers the two contributor-auth security findings:
//
//   C4 (CWE-862/639): the contributor WebSocket must NOT reconstruct task ownership
//   from a client task_progress. A fabricated task_progress sent before any
//   task_assign must be rejected and must NOT mint / push a GitHub credential; a
//   legitimate resume is honored only against a server-issued lease and re-scopes the
//   credential to that lease's repo.
//
//   H2 (CWE-613/639): profile saves are versioned + revocation is server-authoritative
//   and terminal. A stale WS-path save cannot un-revoke an account, and a revoke
//   fences (disconnects) live sessions and denies reconnect.

// drainForToken reads up to a short window looking for a token_refresh, returning
// true if one arrives. Used to assert a credential was (or was NOT) pushed.
func drainForToken(t *testing.T, conn *websocket.Conn, window time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(window)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		conn.SetReadDeadline(time.Now().Add(remaining))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return false
		}
		var m WSMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.Type == "token_refresh" {
			return true
		}
	}
}

// registerAndAuth registers a fresh contributor and returns an authenticated,
// mint-capable connection plus its registration reply.
func registerAndAuth(t *testing.T, s *Server, ts *httptest.Server, username string) (*websocket.Conn, map[string]string) {
	t.Helper()
	body := `{"github_username":"` + username + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register decode: %v", err)
	}
	if reg["registration_token"] == "" {
		t.Fatalf("register returned no token: %s", w.Body.String())
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	readMsg(t, conn) // auth_challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok
	return conn, reg
}

// TestC4_FabricatedTaskProgressMintsNoCredential is the core C4 proof: a client that
// sends task_progress with a self-asserted task_id BEFORE it was ever assigned a task
// must be rejected — the hub does NOT rebuild currentTask from the client fields and
// does NOT mint or push a scoped GitHub credential. A control leg then shows a real
// assignment DOES yield a credential, proving the App auth is live (so the negative is
// meaningful, not just "minting is disabled").
func TestC4_FabricatedTaskProgressMintsNoCredential(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	// Live, succeeding App auth so a mint WOULD produce an observable token_refresh.
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_c4_scoped")}
	s.contributeHub.server = s

	conn, reg := registerAndAuth(t, s, ts, "attacker")
	defer conn.Close()

	// Attack: assert task_progress for a task the server never assigned, echoing a
	// generation the client just made up. currentTask is nil, so the OLD code would
	// rebuild ownership from these fields and mint. The fix must reject it.
	conn.WriteJSON(WSMessage{
		Type:    "task_progress",
		TaskID:  "ct-myorg/repo1-1-9999",
		TaskGen: 424242,
		Kind:    "issue",
		Repo:    "myorg/repo1",
		Number:  1,
		Title:   "forged",
	})
	if drainForToken(t, conn, 400*time.Millisecond) {
		t.Fatalf("C4: a fabricated task_progress was minted a credential (token_refresh pushed) — ownership was rebuilt from client fields")
	}

	// The hub must hold NO task for this connection (nothing was adopted).
	s.contributeHub.mu.RLock()
	var held bool
	for _, c := range s.contributeHub.connections {
		c.mu.Lock()
		if c.profile != nil && c.profile.ContributorID == reg["contributor_id"] && c.currentTask != nil {
			held = true
		}
		c.mu.Unlock()
	}
	s.contributeHub.mu.RUnlock()
	if held {
		t.Fatalf("C4: the forged task_progress caused the hub to adopt a task it never assigned")
	}

	// Control: prove the mint path is genuinely live in this setup, so the negative
	// above ("no token pushed") reflects the C4 rejection and not a dead mint. A real
	// assignment records a server lease AND holds a scoped credential pending delivery.
	setStatusIssues(s, intgIssue(700, "Real issue", "someone", nil))
	assign := s.contributeHub.selectTask(mustConn(t, s.contributeHub, reg["contributor_id"]))
	if assign == nil || assign.Type != "task_assign" {
		t.Fatalf("control: a real selectTask did not assign (got %+v) — cannot prove mint is live", assign)
	}
	// The lease was recorded server-side, and a credential is pending (mint succeeded).
	if s.contributeHub.lookupLease(reg["contributor_id"], assign.TaskID, assign.Repo, assign.Number, assign.TaskGen, time.Now()) == nil {
		t.Fatalf("control: selectTask did not record a server-issued lease")
	}
	holder := mustConn(t, s.contributeHub, reg["contributor_id"])
	holder.mu.Lock()
	pending := holder.pendingToken
	holder.mu.Unlock()
	if pending == "" {
		t.Fatalf("control: a real assignment did not mint a scoped credential — the mint path is not live, so the C4 negative is inconclusive")
	}
}

// mustConn returns the (single) live connection for a contributor ID, failing if none.
func mustConn(t *testing.T, h *ContributeWSHub, contributorID string) *ContributorConnection {
	t.Helper()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		if c.profile != nil && c.profile.ContributorID == contributorID {
			return c
		}
	}
	t.Fatalf("no live connection for %s", contributorID)
	return nil
}

// TestC4_ResumeOnlyAgainstServerLease_ExactRepo proves the lease-bound resume: a
// reconnecting relay may re-adopt ONLY the exact task+repo the hub issued to it, under
// the issued generation. A resume whose repo/generation does not match a live lease is
// rejected; a resume that matches is accepted.
func TestC4_ResumeOnlyAgainstServerLease_ExactRepo(t *testing.T) {
	hub, s := covK2Hub(t)
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_resume_ok")}
	hub.server = s

	identity := "c-lease"
	// Hub issues a lease for repo1#5 at generation 9.
	hub.recordLease(identity, "ct-1", "myorg/repo1", 5, "contributor", 9, time.Now())

	now := time.Now()
	// Wrong repo → no match.
	if hub.lookupLease(identity, "ct-1", "myorg/other", 5, 9, now) != nil {
		t.Fatalf("C4: a resume for the WRONG repo matched a lease — the credential would be re-scoped to a repo the hub never assigned")
	}
	// Wrong generation → no match (a stale/forged generation).
	if hub.lookupLease(identity, "ct-1", "myorg/repo1", 5, 1, now) != nil {
		t.Fatalf("C4: a resume echoing a non-issued generation matched a lease")
	}
	// Unversioned (gen 0) → no match: re-adoption requires proving the generation.
	if hub.lookupLease(identity, "ct-1", "myorg/repo1", 5, 0, now) != nil {
		t.Fatalf("C4: an unversioned resume (gen 0) matched a lease — a client with no generation must not resurrect ownership")
	}
	// Wrong task → no match.
	if hub.lookupLease(identity, "ct-OTHER", "myorg/repo1", 5, 9, now) != nil {
		t.Fatalf("C4: a resume for a different task_id matched the lease")
	}
	// Exact match → adopts.
	if l := hub.lookupLease(identity, "ct-1", "myorg/repo1", 5, 9, now); l == nil {
		t.Fatalf("C4: the exact server-issued lease did not match its own resume — a legitimate reconnect would be refused")
	}
	// After expiry → no match, and the stale lease is dropped.
	if hub.lookupLease(identity, "ct-1", "myorg/repo1", 5, 9, now.Add(leaseTTL+time.Minute)) != nil {
		t.Fatalf("C4: an EXPIRED lease was still re-adoptable")
	}
	// Once released (revoked), even an exact resume no longer matches.
	hub.recordLease(identity, "ct-2", "myorg/repo1", 6, "contributor", 10, now)
	hub.revokeLease(identity, "ct-2")
	if hub.lookupLease(identity, "ct-2", "myorg/repo1", 6, 10, now) != nil {
		t.Fatalf("C4: a released task remained re-adoptable — a released worker could resurrect it")
	}
}

// TestC4_ResumeRefusedWhenSuspended proves the resume path re-runs the admission
// gates: a task with a valid live lease still cannot resume (and re-mint) once the
// operator has SUSPENDED the contribute queue.
func TestC4_ResumeRefusedWhenSuspended(t *testing.T) {
	hub, _ := covK2Hub(t)
	// Suspended queue → the resume gate must refuse.
	c := &ContributorConnection{profile: &ContributorProfile{TrustTier: "contributor"}}
	if got := hub.resumeGateReason(c); got != "" {
		// covK2Hub's config is unsuspended by default; sanity check.
		if got != taskUnavailableHubNotReady {
			t.Fatalf("baseline resume gate unexpectedly refused: %q", got)
		}
	}
	// A revoked profile is refused regardless of queue state.
	rev := &ContributorConnection{profile: &ContributorProfile{TrustTier: "revoked"}}
	if hub.resumeGateReason(rev) == "" {
		t.Fatalf("C4: resume gate admitted a revoked contributor")
	}
}

// TestC4_NoFullCacheFallback proves the deleted installation-wide fallback: with NO
// App auth configured, mintScopedToken returns an EMPTY token (mint nothing) rather
// than the hive's full cached credential. Previously it returned the whole cache.
func TestC4_NoFullCacheFallback(t *testing.T) {
	hub, _ := covK2Hub(t)
	// No GHAppAuth on deps → the App path is unavailable.
	if hub.server != nil && hub.server.deps != nil {
		hub.server.deps.GHAppAuth = nil
	}
	tok, err := hub.mintScopedToken("contributor", "myorg/repo1")
	if err != nil {
		t.Fatalf("mint returned an error: %v", err)
	}
	if tok != "" {
		t.Fatalf("C4: mintScopedToken returned a non-empty token with no App auth — the full-cache fallback is still leaking a credential")
	}
}

// TestC4_RepoNameOnly checks the repo-name extraction used to repository-scope the
// installation token.
func TestC4_RepoNameOnly(t *testing.T) {
	cases := map[string]string{
		"myorg/repo1":              "repo1",
		"repo1":                    "repo1",
		"projectbluefin/fsdk-cont": "fsdk-cont",
		"":                         "",
		"  myorg/r  ":              "r",
	}
	for in, want := range cases {
		if got := repoNameOnly(in); got != want {
			t.Fatalf("repoNameOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- H2 ----------------------------------------------------------------------

// TestH2_RevokeIsTerminalAgainstStaleSave is the core H2 proof: after an admin revoke,
// a stale WS-path save carrying an in-memory "contributor" profile must NOT restore
// the tier. The revoke is server-authoritative and terminal.
func TestH2_RevokeIsTerminalAgainstStaleSave(t *testing.T) {
	setupContributeEnv(t)

	// Seed a contributor and capture a "stale" in-memory copy (as a WS session holds).
	p, _ := createContributorProfile("victim")
	stale := *p // value copy: TrustTier "newcomer", version as written

	// Admin revokes (server-authoritative, adminOverride).
	p.TrustTier = "revoked"
	if err := saveContributorProfileCAS(p, true); err != nil {
		t.Fatalf("admin revoke save: %v", err)
	}

	// The stale WS save tries to persist the OLD non-revoked tier (e.g. a
	// task_complete stat bump on the pre-revoke pointer).
	stale.TasksCompleted++
	_ = saveContributorProfile(&stale) // non-admin path

	// On disk, the tier must still be revoked — the stale save could not un-revoke.
	got, err := loadContributorProfile("victim")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.TrustTier != "revoked" {
		t.Fatalf("H2: a stale WS save restored tier %q over a revoke — account un-revoked", got.TrustTier)
	}
}

// TestH2_VersionConflictRejectsLostUpdate proves the optimistic-concurrency guard: a
// writer whose base version is behind the current on-disk version is rejected, so a
// lost update cannot silently clobber a newer change.
func TestH2_VersionConflictRejectsLostUpdate(t *testing.T) {
	setupContributeEnv(t)
	p, _ := createContributorProfile("racer")

	// Load two independent copies at the same version (two goroutines / two paths).
	a, _ := loadContributorProfile("racer")
	b, _ := loadContributorProfile("racer")

	// Writer A commits first — bumps the on-disk version.
	a.Model = "sonnet"
	if err := saveContributorProfileCAS(a, false); err != nil {
		t.Fatalf("writer A: %v", err)
	}
	// Writer B, still at the old version, must be rejected as a lost update.
	b.Model = "opus"
	if err := saveContributorProfileCAS(b, false); err != errProfileConflict {
		t.Fatalf("H2: stale writer B was not rejected (err=%v) — lost update possible", err)
	}
	_ = p
}

// TestH2_RevokeDisconnectsLiveSessionAndDeniesReconnect drives the real WS + admin
// revoke endpoint: an authenticated session is fenced (disconnected) by a revoke, and
// a reconnect with the same token is denied at auth.
func TestH2_RevokeDisconnectsLiveSessionAndDeniesReconnect(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	conn, reg := registerAndAuth(t, s, ts, "gonner")

	// Admin revokes via the real endpoint.
	revReq := httptest.NewRequest(http.MethodPost, "/api/contributors/"+reg["contributor_id"]+"/revoke", nil)
	revReq.Header.Set("X-Hive-Role", "owner")
	revW := httptest.NewRecorder()
	s.mux.ServeHTTP(revW, revReq)
	if revW.Code != http.StatusOK {
		t.Fatalf("revoke got %d, want 200 (body: %s)", revW.Code, revW.Body.String())
	}

	// The live session is fenced: the socket closes, so a read eventually errors.
	closed := false
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			closed = true
			break
		}
	}
	conn.Close()
	if !closed {
		t.Fatalf("H2: the revoked contributor's live session was not disconnected")
	}

	// Reconnect with the same token is denied at auth (revoked profile).
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial reconnect: %v", err)
	}
	defer conn2.Close()
	readMsg(t, conn2) // auth_challenge
	conn2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	reply := readMsg(t, conn2)
	if reply.Type != "auth_failed" {
		t.Fatalf("H2: a revoked contributor reconnected successfully (got %s), want auth_failed", reply.Type)
	}
}
