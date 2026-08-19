package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Regression tests for #4134: "Failed to save budget: owner access required"
// shown to the hive OWNER. The dashboard UI authenticates with the shared
// dashboard token (Authorization: Bearer <token> from localStorage, or via the
// local gateway which injects X-Hive-Internal), but the F14 owner-provenance
// hardening only set the server-side owner verification marker for per-user
// sessions and proof-verified hub identities — so on every token-secured
// spoke the operator's own budget save was denied by requireOwnerRole.
//
// These tests run PUT /api/config/governor/budget through the FULL middleware
// stack (authenticate → roleEnforcement → mux), exactly as a browser request
// arrives.

const budget4134Token = "shared-secret-token-4134"

// seedBudget4134 stores a known-good budget so a partial update validates.
func seedBudget4134(s *Server) {
	s.deps.Config.Governor.Budget.TotalTokens = 1000
	s.deps.Config.Governor.Budget.PeriodDays = 7
	s.deps.Config.Governor.Budget.CriticalPct = 90
}

func putBudget4134(s *Server, decorate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/budget",
		strings.NewReader(`{"totalTokens":50000}`))
	req.Header.Set("Content-Type", "application/json")
	if decorate != nil {
		decorate(req)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// Owner success — bearer shared token (how the dashboard UI authenticates on
// a token-secured spoke reached directly).
func TestBudgetSave4134_OwnerViaBearerToken(t *testing.T) {
	s := newFullServer(t)
	s.authToken = budget4134Token
	seedBudget4134(s)

	w := putBudget4134(s, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+s.authToken)
	})
	if w.Code != http.StatusOK {
		t.Fatalf("owner budget save via bearer token = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if got := s.deps.Config.Governor.Budget.TotalTokens; got != 50000 {
		t.Fatalf("totalTokens = %d, want 50000 (save did not persist)", got)
	}
}

// Owner success — gateway path (local proxy strips client identity headers
// and injects X-Hive-Internal with the shared token).
func TestBudgetSave4134_OwnerViaInternalToken(t *testing.T) {
	s := newFullServer(t)
	s.authToken = budget4134Token
	seedBudget4134(s)

	w := putBudget4134(s, func(r *http.Request) {
		r.Header.Set("X-Hive-Internal", s.authToken)
	})
	if w.Code != http.StatusOK {
		t.Fatalf("owner budget save via internal token = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if got := s.deps.Config.Governor.Budget.TotalTokens; got != 50000 {
		t.Fatalf("totalTokens = %d, want 50000 (save did not persist)", got)
	}
}

// Owner success — verified owner session (device-flow login).
func TestBudgetSave4134_OwnerViaSession(t *testing.T) {
	s := newDirectRouteServer(t, "owneruser")
	seedBudget4134(s)
	sid := s.createUserSession("owneruser", "owner")

	w := putBudget4134(s, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	})
	if w.Code != http.StatusOK {
		t.Fatalf("owner budget save via session = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

// Non-owner denial — a read-role session must still be blocked.
func TestBudgetSave4134_ReadSessionDenied(t *testing.T) {
	s := newDirectRouteServer(t, "readuser")
	seedBudget4134(s)
	sid := s.createUserSession("readuser", "read")

	w := putBudget4134(s, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("read-role budget save = %d, want 403; body=%q", w.Code, w.Body.String())
	}
	if got := s.deps.Config.Governor.Budget.TotalTokens; got != 1000 {
		t.Fatalf("totalTokens = %d, want 1000 (denied save must not persist)", got)
	}
}

// Non-owner denial — a non-owner (read-write) session lacks the owner gate.
func TestBudgetSave4134_NonOwnerSessionDenied(t *testing.T) {
	s := newDirectRouteServer(t, "rwuser")
	seedBudget4134(s)
	sid := s.createUserSession("rwuser", "read-write")

	w := putBudget4134(s, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("read-write budget save = %d, want 403; body=%q", w.Code, w.Body.String())
	}
}

// Spoof denial — owner identity headers without any credential are stripped
// and rejected; the fix must not reopen header spoofing.
func TestBudgetSave4134_SpoofedOwnerHeadersDenied(t *testing.T) {
	s := newFullServer(t)
	s.authToken = budget4134Token
	seedBudget4134(s)

	w := putBudget4134(s, func(r *http.Request) {
		r.Header.Set("X-Hive-User", "attacker")
		r.Header.Set("X-Hive-Role", "owner")
		r.Header.Set(ownerRoleVerifiedHeader, "true")
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed owner headers budget save = %d, want 401; body=%q", w.Code, w.Body.String())
	}
	if got := s.deps.Config.Governor.Budget.TotalTokens; got != 1000 {
		t.Fatalf("totalTokens = %d, want 1000 (spoofed save must not persist)", got)
	}
}

// Wrong-token denial — possession of the secret, not its mere mention, is the
// owner credential.
func TestBudgetSave4134_WrongBearerTokenDenied(t *testing.T) {
	s := newFullServer(t)
	s.authToken = budget4134Token
	seedBudget4134(s)

	w := putBudget4134(s, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer not-the-token")
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer token budget save = %d, want 401; body=%q", w.Code, w.Body.String())
	}
}
