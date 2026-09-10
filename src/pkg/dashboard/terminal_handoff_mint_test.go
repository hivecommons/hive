package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/terminalassert"
)

// Tests for the POST /api/terminal/handoff mint endpoint
// (handleCreateTerminalHandoff) and the shared writeTerminalRoleForbidden
// refusal writer. These pin the endpoint's three gates — role, signing-key
// precondition, and hive identity — and the success contract: a one-shot code
// bound to the requesting user and role, plus a terminal assertion cookie.

// mintHandoff drives the real route on the server mux, mirroring how the
// hub-authenticated proxy path reaches the handler (identity headers already
// resolved by the outer middleware).
func mintHandoff(s *Server, user, role string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", terminalHandoffPath, nil)
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestTerminalHandoffMint_ReadRoleForbidden(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha")

	rec := mintHandoff(s, "carol", config.RoleRead)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-role handoff mint = %d, want 403", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Fatalf("403 body is not {\"error\":...} JSON: %q", rec.Body.String())
	}
	if assertionCookie(rec.Result()) != nil {
		t.Fatal("forbidden mint still set a terminal assertion cookie")
	}
	s.terminalHandoffMu.Lock()
	n := len(s.terminalHandoffs)
	s.terminalHandoffMu.Unlock()
	if n != 0 {
		t.Fatalf("forbidden mint stored %d handoff codes, want 0", n)
	}
}

// TestTerminalHandoffMint_NoHubLanesFallsBackAndSucceeds mirrors
// TestHandleCreateTerminalHandoff_NoHubLanesFallsBackAndSucceeds against the
// mint endpoint directly: the #6489 fix means a standalone spoke with neither
// hub lane configured mints via terminalassert's lane-3 persisted fallback key
// instead of 503ing.
func TestTerminalHandoffMint_NoHubLanesFallsBackAndSucceeds(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha")
	// Empty both lanes of hub.TerminalSigningKey: the injected per-hive key and
	// the master secret the self-derive lane needs.
	t.Setenv(hub.EnvTerminalKey, "")
	t.Setenv("HIVE_HUB_SECRET", "")
	t.Setenv(terminalassert.EnvFallbackKeyDir, t.TempDir())

	rec := mintHandoff(s, "alice", config.RoleOwner)

	if rec.Code != http.StatusOK {
		t.Fatalf("keyless handoff mint = %d, want 200 (fallback key should apply, body %q)", rec.Code, rec.Body.String())
	}
	if assertionCookie(rec.Result()) == nil {
		t.Fatal("successful mint under the fallback key did not set a terminal assertion cookie")
	}
}

func TestTerminalHandoffMint_NoHiveID503(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha")
	s.deps.Config.HiveID = ""

	rec := mintHandoff(s, "alice", config.RoleOwner)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("hive-id-less handoff mint = %d, want 503", rec.Code)
	}
}

func TestTerminalHandoffMint_OwnerSuccessCodeIsOneShot(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha")

	rec := mintHandoff(s, "alice", config.RoleOwner)

	if rec.Code != http.StatusOK {
		t.Fatalf("owner handoff mint = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("mint response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if resp["code"] == "" {
		t.Fatal("mint response carries no handoff code")
	}
	if resp["expires_in"] != "60" {
		t.Fatalf("expires_in = %q, want 60", resp["expires_in"])
	}
	if assertionCookie(rec.Result()) == nil {
		t.Fatal("successful mint did not set the terminal assertion cookie")
	}

	user, role, ok := s.redeemTerminalHandoff(resp["code"])
	if !ok || user != "alice" || role != config.RoleOwner {
		t.Fatalf("redeem = (%q, %q, %v), want (alice, %s, true)", user, role, ok, config.RoleOwner)
	}
	if _, _, ok := s.redeemTerminalHandoff(resp["code"]); ok {
		t.Fatal("handoff code redeemed twice — must be one-shot")
	}
}

func TestTerminalHandoffMint_ReadWriteRoleAllowed(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha")

	rec := mintHandoff(s, "bob", config.RoleReadWrite)

	if rec.Code != http.StatusOK {
		t.Fatalf("read-write handoff mint = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("mint response is not JSON: %v", err)
	}
	user, role, ok := s.redeemTerminalHandoff(resp["code"])
	if !ok || user != "bob" || role != config.RoleReadWrite {
		t.Fatalf("redeem = (%q, %q, %v), want (bob, %s, true)", user, role, ok, config.RoleReadWrite)
	}
}

// An unproxied deployment reaches the handler with no identity headers at all:
// the role defaults to owner (local operator) and the user records as "local".
func TestTerminalHandoffMint_NoHeadersDefaultsToLocalOwner(t *testing.T) {
	s := newRenewServer(t, "hosted-alpha")

	rec := mintHandoff(s, "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("headerless handoff mint = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("mint response is not JSON: %v", err)
	}
	user, role, ok := s.redeemTerminalHandoff(resp["code"])
	if !ok || user != "local" || role != config.RoleOwner {
		t.Fatalf("redeem = (%q, %q, %v), want (local, %s, true)", user, role, ok, config.RoleOwner)
	}
}

func TestWriteTerminalRoleForbidden_JSONBodyOnAPIPath(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/terminal/handoff", nil)

	writeTerminalRoleForbidden(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	// NOTE: the handler sets Content-Type: application/json, but http.Error
	// unconditionally overwrites it with text/plain — API clients must parse
	// the body shape, not trust the header. Pinned here as current behavior;
	// tracked separately as a content-type mismatch finding.
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Fatalf("API refusal body is not {\"error\":...} JSON: %q", rec.Body.String())
	}
}

func TestWriteTerminalRoleForbidden_PlainTextOnTerminalPath(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/terminal/", nil)

	writeTerminalRoleForbidden(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if strings.Contains(rec.Body.String(), "{") {
		t.Fatalf("browser-facing refusal looks like JSON: %q", rec.Body.String())
	}
}
