package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
