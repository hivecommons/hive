package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// setInviteTier writes a contributor profile at the given trust tier so the
// invite-mint gate can resolve the caller's tier from disk.
func setInviteTier(t *testing.T, username, tier string) {
	t.Helper()
	p := &ContributorProfile{
		GitHubUsername: username,
		ContributorID:  "c-" + username,
		TrustTier:      tier,
		RegisteredAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save profile: %v", err)
	}
}

func mintInvite(t *testing.T, s *Server, caller string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/invite", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	if caller != "" {
		// resolveViewerUsername honors the hub-injected identity header.
		req.Header.Set("X-Hive-User", caller)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	return w
}

// A trusted contributor can mint an invite link, and the returned token verifies
// back to the inviter.
func TestInviteMint_TrustedSucceeds(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	setInviteTier(t, "trustyperson", "trusted")

	w := mintInvite(t, s, "trustyperson")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	tok, _ := resp["invite"].(string)
	if tok == "" {
		t.Fatal("missing invite token")
	}
	url, _ := resp["invite_url"].(string)
	if url == "" {
		t.Fatal("missing invite_url")
	}
	if got := verifyInviteToken(tok, time.Now()); got != "trustyperson" {
		t.Fatalf("token did not verify back to inviter: got %q", got)
	}
}

// An advisor may also mint (advisor is the higher trusted tier).
func TestInviteMint_AdvisorSucceeds(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	setInviteTier(t, "sage", "advisor")

	if w := mintInvite(t, s, "sage"); w.Code != http.StatusOK {
		t.Fatalf("advisor expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// Newcomer and contributor tiers are refused server-side (403), regardless of UI.
func TestInviteMint_NonTrustedForbidden(t *testing.T) {
	for _, tier := range []string{"newcomer", "contributor"} {
		setupContributeEnv(t)
		s := NewServer(0, slog.Default())
		s.registerContributeRoutes()
		setInviteTier(t, "plain", tier)

		w := mintInvite(t, s, "plain")
		if w.Code != http.StatusForbidden {
			t.Fatalf("tier %q: expected 403, got %d: %s", tier, w.Code, w.Body.String())
		}
	}
}

// An anonymous caller (no resolvable identity) cannot mint.
func TestInviteMint_AnonymousUnauthorized(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	w := mintInvite(t, s, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// A caller with no contributor profile at all is refused (403), not treated as
// trusted.
func TestInviteMint_NoProfileForbidden(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	w := mintInvite(t, s, "ghost")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// Registering via a valid invite creates a NEWCOMER attributed to the inviter.
func TestInviteRegister_SetsInvitedByAndStaysNewcomer(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	token := mintInviteToken("mentor", time.Now())
	body := `{"github_username":"invitee1","invite":"` + token + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	p, err := loadContributorProfile("invitee1")
	if err != nil {
		t.Fatalf("load invitee: %v", err)
	}
	if p.InvitedBy != "mentor" {
		t.Fatalf("invited_by = %q, want %q", p.InvitedBy, "mentor")
	}
	if p.TrustTier != "newcomer" {
		t.Fatalf("invitee tier = %q, want newcomer (invite must not elevate)", p.TrustTier)
	}
}

// A normal (non-invited) registration is unaffected: no invited_by, newcomer.
func TestInviteRegister_NoInviteUnaffected(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"github_username":"solojoiner"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	p, err := loadContributorProfile("solojoiner")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.InvitedBy != "" {
		t.Fatalf("invited_by = %q, want empty for self-register", p.InvitedBy)
	}
	if p.TrustTier != "newcomer" {
		t.Fatalf("tier = %q, want newcomer", p.TrustTier)
	}
}

// A tampered/invalid invite token is silently ignored (no attribution, no error).
func TestInviteRegister_InvalidTokenIgnored(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"github_username":"invitee2","invite":"not.a.validtoken"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	p, err := loadContributorProfile("invitee2")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.InvitedBy != "" {
		t.Fatalf("invited_by = %q, want empty (invalid token must not attribute)", p.InvitedBy)
	}
}

// An expired invite token does not attribute.
func TestInviteToken_ExpiredRejected(t *testing.T) {
	setupContributeEnv(t)
	// Mint a token as if it were issued long enough ago that it is now expired.
	past := time.Now().Add(-inviteTokenTTL - time.Hour)
	tok := mintInviteToken("mentor", past)
	if got := verifyInviteToken(tok, time.Now()); got != "" {
		t.Fatalf("expired token verified to %q, want empty", got)
	}
}

// A self-invite (inviter == invitee) is not recorded as attribution.
func TestInviteRegister_SelfInviteIgnored(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	token := mintInviteToken("selfie", time.Now())
	body := `{"github_username":"selfie","invite":"` + token + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	p, err := loadContributorProfile("selfie")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.InvitedBy != "" {
		t.Fatalf("self-invite recorded invited_by=%q, want empty", p.InvitedBy)
	}
}
