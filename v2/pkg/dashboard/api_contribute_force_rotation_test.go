package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// registerContributor performs a plain (new-contributor) registration and
// returns the recorder so the caller can assert on it.
func registerContributor(t *testing.T, s *Server, username string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"github_username":"` + username + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	return w
}

// forceRegister issues a register force:true call. When caller is non-empty it
// is supplied via the hub-injected X-Hive-User identity header, which
// resolveViewerUsername (and thus resolveContributeCaller) honors server-side.
func forceRegister(t *testing.T, s *Server, username, caller string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"github_username":"` + username + `","force":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if caller != "" {
		req.Header.Set("X-Hive-User", caller)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	return w
}

// SECURITY (#2610): rotating an EXISTING contributor's token via
// register force:true must require the SAME authenticated caller-identity check
// that POST /api/contribute/reissue-token enforces. Unauthenticated force
// rotation is an auth bypass and must be rejected with 401.
func TestForceRegister_ExistingContributor_Unauthenticated_401(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// New contributor exists.
	if w := registerContributor(t, s, "existinguser"); w.Code != http.StatusOK {
		t.Fatalf("initial register: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// force:true with no caller identity must be refused.
	w := forceRegister(t, s, "existinguser", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated force rotation: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	// No new token must leak on the rejected path.
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["registration_token"] != "" {
		t.Errorf("rejected force rotation must not return a token, got %q", resp["registration_token"])
	}
}

// A caller who authenticates as a DIFFERENT user than the target contributor
// must not be able to rotate that contributor's token (403 — not the owner).
func TestForceRegister_ExistingContributor_WrongOwner_403(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	if w := registerContributor(t, s, "victimuser"); w.Code != http.StatusOK {
		t.Fatalf("initial register: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w := forceRegister(t, s, "victimuser", "attackeruser")
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-owner force rotation: expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// The owner, authenticated server-side, may rotate their own token via
// force:true. This preserves the intended recovery path.
func TestForceRegister_ExistingContributor_Owner_Succeeds(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	reg := registerContributor(t, s, "owneruser")
	if reg.Code != http.StatusOK {
		t.Fatalf("initial register: expected 200, got %d: %s", reg.Code, reg.Body.String())
	}
	var regResp map[string]string
	if err := json.Unmarshal(reg.Body.Bytes(), &regResp); err != nil {
		t.Fatalf("register response: %v", err)
	}
	origToken := regResp["registration_token"]

	w := forceRegister(t, s, "owneruser", "owneruser")
	if w.Code != http.StatusOK {
		t.Fatalf("owner force rotation: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("rotation response: %v", err)
	}
	if resp["registration_token"] == "" {
		t.Fatal("owner force rotation returned no new token")
	}
	if resp["registration_token"] == origToken {
		t.Error("owner force rotation returned the same token — rotation did not occur")
	}
}

// Initial registration of a NEW contributor stays open (no auth required) — the
// gate only applies to ROTATION of an existing token, not first registration.
func TestForceRegister_NewContributor_Unauthenticated_Succeeds(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// force:true for a user with no existing profile is a plain new
	// registration and must succeed without any caller identity.
	w := forceRegister(t, s, "brandnewuser", "")
	if w.Code != http.StatusOK {
		t.Fatalf("new-contributor registration: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("register response: %v", err)
	}
	if resp["registration_token"] == "" {
		t.Error("new-contributor registration returned no token")
	}
}
