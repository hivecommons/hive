package dashboard

import (
	"strings"
	"testing"
)

// findAuditEntry returns the first recent audit entry matching the given
// action, or nil if none was recorded.
func findAuditEntry(s *Server, action string) *AuditEntry {
	for _, e := range s.audit.Recent(auditMaxEntries) {
		if e.Action == action {
			return &e
		}
	}
	return nil
}

// TestLoginAudit_SuccessCarriesUsername verifies that the audit emission used
// on a successful device-flow login records a "login" entry whose actor is the
// GitHub username, so the dashboard Audit Log shows WHO logged in. This mirrors
// the exact s.audit.Log(...) call made by handleGHUserAuthPoll on success.
func TestLoginAudit_SuccessCarriesUsername(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson")

	// Same call the success path makes.
	s.audit.Log("clubanderson", "login", auditDetail("method", "github device flow", "role", "owner"), "")

	e := findAuditEntry(s, "login")
	if e == nil {
		t.Fatal("successful login must produce a 'login' audit entry")
	}
	if e.User != "clubanderson" {
		t.Fatalf("login audit actor must be the GitHub username, got %q", e.User)
	}
	if !strings.Contains(e.Detail, "github device flow") {
		t.Fatalf("login audit detail must record the method, got %q", e.Detail)
	}
}

// TestLoginAudit_DeniedCarriesUsername verifies that the audit emission used on
// a rejected login (authenticated with GitHub but not on the hive allowlist)
// records a "login_denied" entry whose actor is the attempted username, so the
// owner can see WHO tried and was rejected.
func TestLoginAudit_DeniedCarriesUsername(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson")

	// Same call the rejection path makes for an unauthorized user.
	s.audit.Log("attacker", "login_denied", auditDetail("reason", "not authorized for this hive"), "")

	e := findAuditEntry(s, "login_denied")
	if e == nil {
		t.Fatal("rejected login must produce a 'login_denied' audit entry")
	}
	if e.User != "attacker" {
		t.Fatalf("login_denied audit actor must be the attempted username, got %q", e.User)
	}
	if !strings.Contains(e.Detail, "not authorized") {
		t.Fatalf("login_denied audit detail must record the reason, got %q", e.Detail)
	}
}

// TestLoginAudit_EntriesReachRecent proves both login-audit actions surface via
// Recent(), which is exactly what handleAuditLog serves to the dashboard Audit
// Log panel — so the new entries render there like every other audit entry.
func TestLoginAudit_EntriesReachRecent(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson")
	s.audit.Log("attacker", "login_denied", auditDetail("reason", "not authorized for this hive"), "")
	s.audit.Log("clubanderson", "login", auditDetail("method", "github device flow", "role", "owner"), "")

	var sawLogin, sawDenied bool
	for _, e := range s.audit.Recent(auditMaxEntries) {
		switch e.Action {
		case "login":
			sawLogin = true
		case "login_denied":
			sawDenied = true
		}
	}
	if !sawLogin || !sawDenied {
		t.Fatalf("both login audit entries must reach Recent(): login=%v denied=%v", sawLogin, sawDenied)
	}
}
