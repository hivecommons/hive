package dashboard

// Tests for the spoke's remaining OIDC identity DISPLAY surfaces: the top-bar
// account chip (/api/role → header avatar menu) and the Settings → Audit Log
// actor column. Presentation only — the raw identity key keeps driving every
// permission decision, and each case asserts it survives unchanged alongside
// the optional resolved name.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// roleGet performs GET /api/role with the auth-proxy identity headers set.
func roleGet(t *testing.T, s *Server, user, role string) map[string]string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/role", nil)
	req.Header.Set("X-Hive-User", user)
	req.Header.Set("X-Hive-Role", role)
	rec := httptest.NewRecorder()
	s.handleRole(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/role = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return body
}

// TestRoleCarriesDisplayNameForOIDCIdentity asserts /api/role attaches
// display_name from the hub-delivered AuthorizedUserNames map for an opaque
// OIDC key, while user stays the raw key the UI must keep authorizing with.
func TestRoleCarriesDisplayNameForOIDCIdentity(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUserNames = map[string]string{
		"ibmid:5500087VJB": "Jane Doe",
	}
	body := roleGet(t, s, "ibmid:5500087VJB", "read")
	if body["user"] != "ibmid:5500087VJB" {
		t.Fatalf("user = %q, want the untouched raw key", body["user"])
	}
	if body["display_name"] != "Jane Doe" {
		t.Errorf(`display_name = %q, want "Jane Doe"`, body["display_name"])
	}
}

// TestRoleOmitsDisplayNameWhenUnknown asserts no display_name is emitted for
// a key the hub has no name for (or when the map was never delivered): absent
// means "render the raw key", never blank/undefined.
func TestRoleOmitsDisplayNameWhenUnknown(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUserNames = map[string]string{
		"ibmid:OTHERUSER": "Someone Else",
	}
	body := roleGet(t, s, "google:NONAMECLAIM", "read")
	if v, ok := body["display_name"]; ok && v != "" {
		t.Errorf("display_name = %q for an unknown key, want absent", v)
	}

	deps.Config.Dashboard.AuthorizedUserNames = nil
	body = roleGet(t, s, "clubanderson", "owner")
	if v, ok := body["display_name"]; ok && v != "" {
		t.Errorf("display_name = %q with a nil names map, want absent", v)
	}
	if body["user"] != "clubanderson" {
		t.Fatalf("user = %q, want unchanged", body["user"])
	}
}

// TestAuditLogDecoratesUserNameServeTimeOnly asserts handleAuditLog attaches
// user_name for OIDC actors with a hub-resolved name, leaves it absent
// otherwise, keeps the raw user on the wire, and never mutates the ring (the
// on-disk log and later reads must keep raw keys).
func TestAuditLogDecoratesUserNameServeTimeOnly(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUserNames = map[string]string{
		"ibmid:5500087VJB": "Jane Doe",
	}
	s.audit.Log("ibmid:5500087VJB", "test-action", "detail", "")
	s.audit.Log("clubanderson", "test-action-2", "detail", "")

	req := httptest.NewRequest(http.MethodGet, "/api/audit-log", nil)
	req.Header.Set("X-Hive-Role", "owner")
	rec := httptest.NewRecorder()
	s.handleAuditLog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/audit-log = %d, want 200", rec.Code)
	}
	var body struct {
		Entries []AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var oidc, gh *AuditEntry
	for i := range body.Entries {
		switch body.Entries[i].User {
		case "ibmid:5500087VJB":
			oidc = &body.Entries[i]
		case "clubanderson":
			gh = &body.Entries[i]
		}
	}
	if oidc == nil || gh == nil {
		t.Fatalf("audit entries missing: %+v", body.Entries)
	}
	if oidc.UserName != "Jane Doe" {
		t.Errorf(`OIDC entry user_name = %q, want "Jane Doe"`, oidc.UserName)
	}
	if gh.UserName != "" {
		t.Errorf("GitHub entry user_name = %q, want empty (key is already the label)", gh.UserName)
	}
	// Serve-time only: the ring itself must still hold raw, undecorated rows.
	for _, e := range s.audit.Recent(10) {
		if e.UserName != "" {
			t.Errorf("ring entry for %q was mutated with UserName %q — decoration must stay serve-time", e.User, e.UserName)
		}
	}
}

// TestHeaderChipAndAuditRowsRenderResolvedNames pins the index.html render
// sites: the header account chip must show display_name (with an initials
// avatar and no fabricated github.com profile/avatar for provider-prefixed
// keys), and the audit table must prefer user_name while keeping the raw key
// in the row tooltip.
func TestHeaderChipAndAuditRowsRenderResolvedNames(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// checkGHAuth: no github.com/<key>.png for OIDC keys, and the resolved
		// name rides along to showGHAuthState.
		"showGHAuthState(true, roleData.user, ghAvatar, roleData.role, roleData.display_name);",
		"var ghAvatar = roleData.user.indexOf(':') === -1",
		// showGHAuthState: display name in the menu, initials avatar fallback,
		// profile entry hidden for provider keys.
		"const shownName = displayName || username;",
		"img.src = initialsAvatarURI(shownName);",
		"if (username && !isProviderKey) {",
		"function initialsAvatarURI(name)",
		// Audit rows: resolved label + provider-aware avatar, raw key kept as
		// the cell tooltip.
		"var actorLabel = String(e.user_name || e.user || '');",
		"face = accessRowAvatar(actor, actorLabel, AUDIT_AVATAR_PX);",
		`title="' + esc(actor) + '">' + face + esc(actorLabel || '—')`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html missing expected snippet: %q", snippet)
		}
	}
}
