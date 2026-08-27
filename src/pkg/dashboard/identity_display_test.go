package dashboard

// Tests for the OIDC identity DISPLAY fix on the spoke: Settings → Security →
// Access must show a human name for non-GitHub identities (IBMid, Google,
// Microsoft) when the hub has resolved one, and must always keep the raw
// identity key discoverable and fall back to it cleanly when no name is
// known. This is presentation only — AuthorizedUsers (the allowlist/auth-key
// list AuthorizedRole matches against) is never touched by any of this.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAuthorizedUsersListCarriesDisplayName asserts handleAuthorizedUsersList
// attaches display_name from Dashboard.AuthorizedUserNames when the hub sent
// one for that raw key, and omits it (never emits an empty/undefined name)
// for a key the hub has no name for.
func TestAuthorizedUsersListCarriesDisplayName(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUsers = []string{
		"owner1:owner",
		"ibmid:5500087VJB:read",
		"google:NONAMECLAIM:read",
	}
	deps.Config.Dashboard.AuthorizedUserNames = map[string]string{
		"ibmid:5500087VJB": "Jane Doe",
		// Deliberately no entry for owner1 or the google key: both must fall
		// back to their raw key rather than rendering blank.
	}

	rec := doGet(s, "/api/config/authorized-users")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var body struct {
		Users []struct {
			Username    string `json:"username"`
			Role        string `json:"role"`
			DisplayName string `json:"display_name"`
		} `json:"users"`
		Enforced bool `json:"enforced"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	byUsername := map[string]string{}
	for _, u := range body.Users {
		byUsername[u.Username] = u.DisplayName
		// The identity key itself must be the untouched raw allowlist entry —
		// this endpoint must never rewrite Username to the friendly name.
	}

	if got := byUsername["ibmid:5500087VJB"]; got != "Jane Doe" {
		t.Errorf(`ibmid user display_name = %q, want "Jane Doe"`, got)
	}
	if got, ok := byUsername["owner1"]; ok && got != "" {
		t.Errorf("owner1 (no known name) display_name = %q, want empty/absent", got)
	}
	if got, ok := byUsername["google:NONAMECLAIM"]; ok && got != "" {
		t.Errorf("google user with no known name display_name = %q, want empty/absent", got)
	}
	if _, ok := byUsername["ibmid:5500087VJB"]; !ok {
		t.Fatal("ibmid user missing from the authorized-users response entirely")
	}
}

// TestAuthorizedUsersListNilNamesMapFallsBackCleanly asserts a hub that has
// never sent AuthorizedUserNames (nil map, e.g. an older hub or one with no
// SaaS record for this hive) still renders every row from its raw key — the
// pre-existing behavior — rather than erroring or producing empty rows.
func TestAuthorizedUsersListNilNamesMapFallsBackCleanly(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUsers = []string{"owner1:owner", "microsoft:AAAABBBB:read"}
	deps.Config.Dashboard.AuthorizedUserNames = nil

	rec := doGet(s, "/api/config/authorized-users")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var body struct {
		Users []struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Users) != 2 {
		t.Fatalf("users = %d, want 2", len(body.Users))
	}
	for _, u := range body.Users {
		if u.DisplayName != "" {
			t.Errorf("%s: display_name = %q, want empty (no names map delivered)", u.Username, u.DisplayName)
		}
		if u.Username == "" {
			t.Error("a row lost its raw identity key")
		}
	}
}

// TestAccessRowRenderingUsesDisplayNameAndKeepsRawKeyDiscoverable pins the
// Access tab's row markup: it must prefer display_name over the raw key,
// classify the provider icon without assuming github, avoid linking a
// non-GitHub key to a bogus github.com profile, and always keep the raw key
// reachable (a title attribute and, when a friendly name is shown, a muted
// secondary line) — never an empty/blank row.
func TestAccessRowRenderingUsesDisplayNameAndKeepsRawKeyDiscoverable(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"var hasName = !!(u.display_name && u.display_name !== u.username);",
		"var primary = hasName ? u.display_name : u.username;",
		"function identityKeyProvider(key)",
		"function accessRowAvatar(key, displayName, px)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Access tab row rendering missing expected snippet: %q", snippet)
		}
	}
	// A non-GitHub identity must not get the plain linkedAvatar (which always
	// points at github.com/<key>.png — wrong host for an ibmid/google/ms
	// subject and a dead link).
	if !strings.Contains(html, "if (provider === 'github') return linkedAvatar(key, px, title, '', 'margin-right:6px');") {
		t.Error("accessRowAvatar must special-case github vs non-github rather than always linking to a github.com profile")
	}
}
