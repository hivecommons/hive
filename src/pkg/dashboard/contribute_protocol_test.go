package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_protocol_test.go covers the ADDITIVE, backward-compatible
// contributor-protocol extensions from kubestellar/hive#2547 (capability
// DECLARE half) and #2567 (protocol version + server capability set + surface
// identifier). Nothing here exercises routing/gating — those are deliberately
// out of scope (the deferred Gate half of #2547).

// registerWSUser registers a contributor and returns its registration token +
// contributor id, mirroring the flow in contribute_ws_test.go.
func registerWSUser(t *testing.T, s *Server, username string) (token, contributorID string) {
	t.Helper()
	body := `{"github_username":"` + username + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register unmarshal: %v", err)
	}
	return reg["registration_token"], reg["contributor_id"]
}

// TestWS_AuthOKCarriesVersionAndCapabilities asserts #2567: the server's auth_ok
// advertises the protocol version and its capability set so a client can learn
// what the deployed hub supports without probing.
func TestWS_AuthOKCarriesVersionAndCapabilities(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, _ := registerWSUser(t, s, "ver-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude"})
	authOK := readMsg(t, conn)

	if authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}
	if authOK.ProtocolVersion != contributorProtocolVersion {
		t.Fatalf("auth_ok protocol_version: want %q, got %q", contributorProtocolVersion, authOK.ProtocolVersion)
	}
	if len(authOK.ServerCapabilities) == 0 {
		t.Fatal("auth_ok must advertise server_capabilities")
	}
	// The advertised set must include the well-known capability tokens.
	want := map[string]bool{
		capTokenRefresh: false, capTaskUnavailableReasons: false,
		capPromptPreview: false, capCapabilityDeclare: false,
	}
	for _, c := range authOK.ServerCapabilities {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for c, seen := range want {
		if !seen {
			t.Errorf("auth_ok server_capabilities missing %q", c)
		}
	}
}

// TestWS_ClientDeclaredCapabilitiesStoredAndSurfaced asserts the #2547 declare
// half: a client that DECLARES capabilities has them stored and surfaced
// read-only on the fleet snapshot (FleetClanker), consistent with how
// cli_backend/model/role are surfaced. No routing/gating is asserted.
func TestWS_ClientDeclaredCapabilitiesStoredAndSurfaced(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, cid := registerWSUser(t, s, "caps-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: token,
		CLIBackend:        "claude",
		Capabilities: &ContributorCapabilities{
			ContainerRuntime:     "podman",
			OS:                   "linux",
			Arch:                 "arm64",
			AgentCLIVersion:      "1.2.3",
			RelayProtocolVersion: "1.1",
			CredentialType:       "app",
		},
	})
	if authOK := readMsg(t, conn); authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}

	// Give the hub a moment to register the connection, then read the fleet snapshot.
	deadline := time.Now().Add(2 * time.Second)
	var fc *FleetClanker
	for time.Now().Before(deadline) {
		snap := s.contributeHub.FleetSnapshot()
		for i := range snap.Clankers {
			if snap.Clankers[i].ContributorID == cid {
				fc = &snap.Clankers[i]
				break
			}
		}
		if fc != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fc == nil {
		t.Fatal("clanker not found in fleet snapshot")
	}
	if fc.Capabilities == nil {
		t.Fatal("declared capabilities were not surfaced on FleetClanker")
	}
	if fc.Capabilities.ContainerRuntime != "podman" ||
		fc.Capabilities.OS != "linux" ||
		fc.Capabilities.Arch != "arm64" ||
		fc.Capabilities.AgentCLIVersion != "1.2.3" ||
		fc.Capabilities.RelayProtocolVersion != "1.1" ||
		fc.Capabilities.CredentialType != "app" {
		t.Fatalf("surfaced capabilities mismatch: %+v", fc.Capabilities)
	}
}

// TestWS_OmittedCapabilitiesUnaffected asserts backward compatibility: a client
// that declares NO capabilities (an existing/unversioned relay) authenticates
// exactly as before and is surfaced with nil capabilities — nothing changes.
func TestWS_OmittedCapabilitiesUnaffected(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, cid := registerWSUser(t, s, "legacy-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	// No Capabilities / ProtocolVersion — exactly today's wire.
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude"})
	authOK := readMsg(t, conn)
	if authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}

	deadline := time.Now().Add(2 * time.Second)
	var fc *FleetClanker
	for time.Now().Before(deadline) {
		snap := s.contributeHub.FleetSnapshot()
		for i := range snap.Clankers {
			if snap.Clankers[i].ContributorID == cid {
				fc = &snap.Clankers[i]
				break
			}
		}
		if fc != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fc == nil {
		t.Fatal("clanker not found in fleet snapshot")
	}
	if fc.Capabilities != nil {
		t.Fatalf("a client that declared nothing must surface nil capabilities, got %+v", fc.Capabilities)
	}
}

// TestWS_DeclaredCapabilitiesInFleetAPIPayload asserts the #2547 declare half is
// visible where an operator actually reads it: the /api/contribute/fleet HTTP JSON
// payload (what the Operations tab hydrates from), not just the in-process
// FleetSnapshot(). A connected client that declared capabilities has them present
// under clankers[].capabilities in the served JSON.
func TestWS_DeclaredCapabilitiesInFleetAPIPayload(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, cid := registerWSUser(t, s, "fleetapi-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: token,
		CLIBackend:        "claude",
		Capabilities: &ContributorCapabilities{
			ContainerRuntime: "podman",
			OS:               "linux",
			CredentialType:   "pat",
		},
	})
	if authOK := readMsg(t, conn); authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}

	// Poll the real HTTP endpoint until the connection is registered, then assert
	// the served JSON carries the declared capabilities under this clanker.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("fleet endpoint: got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Clankers []FleetClanker `json:"clankers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid fleet JSON: %v", err)
		}
		for i := range resp.Clankers {
			if resp.Clankers[i].ContributorID != cid {
				continue
			}
			caps := resp.Clankers[i].Capabilities
			if caps == nil {
				t.Fatal("declared capabilities missing from /api/contribute/fleet payload")
			}
			if caps.ContainerRuntime != "podman" || caps.OS != "linux" || caps.CredentialType != "pat" {
				t.Fatalf("fleet payload capabilities mismatch: %+v", caps)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("clanker not found in /api/contribute/fleet payload")
}

// TestWS_ProtocolVersionOnlyClientFoldsIn asserts a version-only client (sends a
// top-level protocol_version but no capabilities object) still has its protocol
// version stored + surfaced, so an intentionally-degrading newer client is
// recorded. Additive.
func TestWS_ProtocolVersionOnlyClientFoldsIn(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, cid := registerWSUser(t, s, "veronly-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude", ProtocolVersion: "9.9"})
	if authOK := readMsg(t, conn); authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
	}

	deadline := time.Now().Add(2 * time.Second)
	var fc *FleetClanker
	for time.Now().Before(deadline) {
		snap := s.contributeHub.FleetSnapshot()
		for i := range snap.Clankers {
			if snap.Clankers[i].ContributorID == cid {
				fc = &snap.Clankers[i]
				break
			}
		}
		if fc != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fc == nil || fc.Capabilities == nil {
		t.Fatal("version-only client should surface capabilities carrying the protocol version")
	}
	if fc.Capabilities.RelayProtocolVersion != "9.9" {
		t.Fatalf("relay_protocol_version: want 9.9, got %q", fc.Capabilities.RelayProtocolVersion)
	}
}

// TestWS_DeclaredCapabilitiesDoNotAlterTrust is the load-bearing guarantee of the
// #2547 declare half: a self-declared capability map is ADDITIVE and is NEVER a
// trust signal. A client can claim anything (a privileged-looking credential_type,
// any runtime), and the hub must grant EXACTLY the trust tier and permissions it
// would grant a client that declared nothing — the auth_ok tier/permissions derive
// solely from the persisted profile (TrustTier), never from a client-supplied
// capability field. This asserts the declaration does not elevate (or change at
// all) the permission decision.
func TestWS_DeclaredCapabilitiesDoNotAlterTrust(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// authOnce connects a freshly-registered newcomer, optionally declaring rich
	// capabilities, and returns the trust tier + permissions the hub granted.
	authOnce := func(username string, caps *ContributorCapabilities) (tier string, perms []string) {
		t.Helper()
		token, _ := registerWSUser(t, s, username)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		readMsg(t, conn) // challenge
		conn.WriteJSON(WSMessage{
			Type:              "auth_response",
			RegistrationToken: token,
			CLIBackend:        "claude",
			Capabilities:      caps,
		})
		authOK := readMsg(t, conn)
		if authOK.Type != "auth_ok" {
			t.Fatalf("expected auth_ok, got %s: %s", authOK.Type, authOK.Reason)
		}
		return authOK.TrustTier, authOK.Permissions
	}

	// Baseline: a client that declares nothing (an unversioned relay).
	baseTier, basePerms := authOnce("notrust-plain", nil)

	// A client that declares a rich, deliberately privileged-LOOKING posture —
	// "app" credential type, a container runtime, etc. None of this may buy trust.
	richTier, richPerms := authOnce("notrust-rich", &ContributorCapabilities{
		ContainerRuntime:     "docker",
		OS:                   "linux",
		Arch:                 "amd64",
		AgentCLIVersion:      "9.9.9",
		RelayProtocolVersion: "1.1",
		CredentialType:       "app",
	})

	if richTier != baseTier {
		t.Fatalf("declared capabilities altered trust tier: declared=%q, undeclared=%q", richTier, baseTier)
	}
	if strings.Join(richPerms, ",") != strings.Join(basePerms, ",") {
		t.Fatalf("declared capabilities altered permissions: declared=%v, undeclared=%v", richPerms, basePerms)
	}
	// Both must be the unprivileged newcomer default — declaring did not elevate.
	if baseTier != "newcomer" {
		t.Fatalf("expected a freshly-registered contributor to be newcomer, got %q", baseTier)
	}
}

// TestWS_UnknownMessageTypeIgnored asserts forward compatibility: an unknown
// message type is silently ignored (dropped by the read-loop's default), so a
// newer peer can introduce new message types without breaking an older peer.
func TestWS_UnknownMessageTypeIgnored(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	token, _ := registerWSUser(t, s, "future-user")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude"})
	if authOK := readMsg(t, conn); authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s", authOK.Type)
	}

	// Send a message type this server has never heard of. It must be ignored, not
	// close the connection: a subsequent legitimate "ready" must still be served.
	conn.WriteJSON(map[string]any{"type": "some_future_message_v99", "foo": "bar"})
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 99})

	// The connection is still alive if the hub still counts us active.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.contributeHub.ActiveCount() == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("connection dropped after an unknown message type — must be ignored (forward-compatible)")
}

// TestContributeStatus_CarriesSurfaceAndVersion asserts #2567 on the status
// response: it now carries a surface discriminator plus the api/protocol version
// (and a served SHA), while keeping every pre-existing field unchanged
// (backward-compatible). Default (not hub-proxied) reports "hub".
func TestContributeStatus_CarriesSurfaceAndVersion(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/status", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// Pre-existing fields unchanged (backward-compatible).
	if resp["hub"] != "online" {
		t.Errorf("expected hub=online, got %v", resp["hub"])
	}
	// New additive discriminators.
	if resp["surface"] != surfaceHub {
		t.Errorf("expected surface=%q, got %v", surfaceHub, resp["surface"])
	}
	if resp["api_version"] != contributorProtocolVersion {
		t.Errorf("expected api_version=%q, got %v", contributorProtocolVersion, resp["api_version"])
	}
	if _, ok := resp["served_sha"]; !ok {
		t.Error("expected served_sha field to be present")
	}
	if got := w.Header().Get("X-Hive-Contribute-Protocol"); got != contributorProtocolVersion {
		t.Errorf("expected X-Hive-Contribute-Protocol=%q, got %q", contributorProtocolVersion, got)
	}
}

// ── #2547 DECLARE-half guard: declarations must NEVER influence selection ─────
//
// The accepted design split on kubestellar/hive#2547 is DECLARE only: a client
// may describe its execution environment, and the hub records and DISPLAYS it —
// but acting on a declaration when choosing what to assign (the ROUTE half) is
// explicitly undecided and NOT implemented. The tests below pin that invariant
// the same way the F14 owner-gate tests do — behaviourally (selection output is
// identical with and without a declaration) AND at source level (the selection
// code cannot even see the field) — because the failure mode to defend against
// is a future change, or a sync merge, quietly wiring the self-reported posture
// into dispatch. A declaration is unverified self-description; the moment it
// influences assignment it becomes a value the client controls steering the
// hub, which #2547 records as an explicit non-goal until ROUTE is decided.

// richDeclaration returns a fully-populated, privileged-LOOKING capability
// declaration. Nothing in it may buy — or cost — an assignment.
func richDeclaration() *ContributorCapabilities {
	return &ContributorCapabilities{
		ContainerRuntime:     "docker",
		OS:                   "linux",
		Arch:                 "amd64",
		AgentCLIVersion:      "9.9.9",
		RelayProtocolVersion: "1.1",
		CredentialType:       "app",
	}
}

// TestSelectTask_DeclaredCapabilitiesDoNotAffectSelection asserts the ROUTE
// half is genuinely unimplemented: contributors identical in every input
// selectTask may legitimately read (tier, role, username, label interests) but
// differing in what they declared are offered exactly the same work item, and a
// single contributor's pick does not move when it starts declaring mid-session.
func TestSelectTask_DeclaredCapabilitiesDoNotAffectSelection(t *testing.T) {
	hub, s := covK2Hub(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name: "repo1",
			Full: "myorg/repo1",
			ActionableIssues: []any{
				map[string]any{
					"number": float64(10),
					"title":  "Issue A (head of scan order)",
					"url":    "https://github.com/myorg/repo1/issues/10",
					"author": "someone",
				},
				map[string]any{
					"number": float64(20),
					"title":  "Issue B",
					"url":    "https://github.com/myorg/repo1/issues/20",
					"author": "someone",
				},
			},
		}},
	}
	s.statusMu.Unlock()

	mkConn := func(user string, caps *ContributorCapabilities) *ContributorConnection {
		return &ContributorConnection{
			profile:      &ContributorProfile{GitHubUsername: user, ContributorID: "c-" + user, TrustTier: "contributor"},
			lastPong:     time.Now(),
			capabilities: caps,
		}
	}

	declared := hub.selectTask(mkConn("cap-rich", richDeclaration()))
	undeclared := hub.selectTask(mkConn("cap-none", nil))
	for name, msg := range map[string]*WSMessage{"declared": declared, "undeclared": undeclared} {
		if msg == nil || msg.Type != "task_assign" {
			t.Fatalf("%s: expected a task_assign, got %+v", name, msg)
		}
	}
	if declared.Repo != undeclared.Repo || declared.Number != undeclared.Number {
		t.Fatalf("declaring capabilities changed the selection: declared=%s#%d, undeclared=%s#%d",
			declared.Repo, declared.Number, undeclared.Repo, undeclared.Number)
	}

	// Same contributor, before vs after declaring: the pick must not move. The
	// held task is released the way the task_failed/complete handlers do (clear
	// currentTask) WITHOUT recording a failure or completion, so the queue state
	// seen by the second pass is identical to the first.
	conn := mkConn("cap-flip", nil)
	before := hub.selectTask(conn)
	conn.mu.Lock()
	conn.currentTask = nil
	conn.mu.Unlock()
	conn.capabilities = richDeclaration()
	after := hub.selectTask(conn)
	if before == nil || after == nil || before.Type != "task_assign" || after.Type != "task_assign" {
		t.Fatalf("expected task_assign both times, got before=%+v, after=%+v", before, after)
	}
	if before.Repo != after.Repo || before.Number != after.Number {
		t.Fatalf("declaring capabilities mid-session changed the selection: before=%s#%d, after=%s#%d",
			before.Repo, before.Number, after.Repo, after.Number)
	}
}

// selectionFuncBody returns the source of the named ContributeWSHub method,
// from its func line to the first column-0 closing brace — the same extraction
// the F14 owner-gate invariant tests use on api_contribute.go.
func selectionFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "func (h *ContributeWSHub) "+name+"(")
	if i < 0 {
		t.Fatalf("%s not found in contribute_ws.go — it was renamed or removed; update this test deliberately, do not delete the case", name)
	}
	j := strings.Index(src[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("could not find the end of %s", name)
	}
	return src[i : i+j]
}

// TestSelectionPathsDoNotReadDeclaredCapabilities is the source-level half of
// the guard: the assignment/selection code paths must not even MENTION the
// declared capability map. A behavioural test alone could pass while a routing
// read hides behind config that is off in tests; a merge that wires the field
// into selectTask trips this immediately (the exact way the F14 owner-gate
// regression was caught).
func TestSelectionPathsDoNotReadDeclaredCapabilities(t *testing.T) {
	raw, err := os.ReadFile("contribute_ws.go")
	if err != nil {
		t.Fatalf("read contribute_ws.go: %v", err)
	}
	src := string(raw)

	// Positive control (a): the declared-capability plumbing genuinely lives in
	// this file, so a read inside the selection bodies below would be visible to
	// the scan. If storage moved, re-point the test rather than deleting it.
	if !strings.Contains(src, "capabilities *ContributorCapabilities") {
		t.Fatal("contribute_ws.go no longer stores declared capabilities — the DECLARE-half storage moved; update this test deliberately")
	}

	for _, name := range []string{"selectTask", "RequeueContributorTask"} {
		body := selectionFuncBody(t, src, name)
		// Positive control (b): the extraction really captured the selection body.
		if name == "selectTask" && !strings.Contains(body, "candidates") {
			t.Fatal("extracted selectTask body has no candidate collection — extraction is wrong; fix the test")
		}
		if strings.Contains(strings.ToLower(body), "capabilit") {
			t.Errorf("%s references the client-declared capability map — ROUTE is intentionally "+
				"NOT implemented (hivecommons/hive#2547 DECLARE/ROUTE split). If routing has now been "+
				"decided and recorded by a maintainer, update this test in that same PR; otherwise "+
				"remove the read.", name)
		}
	}
}

// TestOpsPage_DeclaredCapabilitiesLabeledSelfReported asserts the fleet view
// keeps the self-reported framing visible at the point of use (#2547 risk
// section: a capability map that reads like a verified inventory is worse than
// honest ignorance). The rendered Operations page must carry the renderer, the
// explicit "declares:" prefix, and the advisory-only labeling — and the
// renderer must actually be wired into the clanker row render.
func TestOpsPage_DeclaredCapabilitiesLabeledSelfReported(t *testing.T) {
	body := renderContributePage(t)
	for _, want := range []string{
		"function capabilityLine",        // the renderer exists
		"declares:",                      // explicit self-report prefix on the row
		"clanker-declares",               // stable hook class for the line
		"Self-declared by the client",    // labeling at the point of use
		"never routes, gates, or trusts", // the no-routing / not-a-trust-signal statement
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ops page missing %q — declared capabilities must stay labeled self-reported (#2547)", want)
		}
	}
	if !strings.Contains(body, "capabilityLine(c.capabilities)") {
		t.Error("renderClankers no longer renders capabilityLine(c.capabilities) — the declared posture is stored but not displayed")
	}
}
