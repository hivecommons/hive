package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// An OIDC user has no github.com account. Their auth label is a display name
// or an email, so a profile link built from it points at a stranger, or at
// github.com/Ada%20Lovelace.

func authUserPayload(t *testing.T, s *HubServer, user string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://hive.hivecommons.dev/api/auth/user", nil)
	req.AddCookie(testAuthCookie(user))
	rec := httptest.NewRecorder()
	s.handleAuthUser(rec, req)
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("%s: decoding auth payload: %v (body %q)", user, err, rec.Body.String())
	}
	return payload
}

func mkOIDCUser(t *testing.T, id, displayName, avatarURL string) {
	t.Helper()
	u := ensureSaaSUser(id)
	u.DisplayName = displayName
	u.AvatarURL = avatarURL
	if err := saveSaaSUser(u); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
}

func TestAuthUserPayloadNamesIdentityProvider(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUser(t, "octocat")
	mkOIDCUser(t, "custom:4f2a9c", "Ada Lovelace", "")

	for _, tc := range []struct{ user, login, provider string }{
		{"octocat", "octocat", "github"},
		{"custom:4f2a9c", "Ada Lovelace", "custom"},
	} {
		payload := authUserPayload(t, s, tc.user)
		if payload["login"] != tc.login {
			t.Errorf("%s: login = %v, want %q", tc.user, payload["login"], tc.login)
		}
		if payload["provider"] != tc.provider {
			t.Errorf("%s: provider = %v, want %q", tc.user, payload["provider"], tc.provider)
		}
	}

	if got := impersonatedAuthUser(t, s, "custom:4f2a9c")["provider"]; got != "custom" {
		t.Errorf("impersonated provider = %v, want the target's provider custom", got)
	}
}

// esc, escAttr and jsArg are stand-ins. The shipped esc needs a DOM, and
// jsFunc cannot cut out escAttr or jsArg: it reads the quote in their regex
// literals as the start of a string.
func avatarHarness(t *testing.T) string {
	t.Helper()
	parts := []string{
		"const assert = require('node:assert/strict');",
		"function esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }",
		"function escAttr(s) { return esc(s).replace(/\"/g, '&quot;').replace(/'/g, '&#39;'); }",
		`function jsArg(s) { return esc("'" + String(s == null ? '' : s).replace(/\\/g, '\\\\').replace(/'/g, "\\'") + "'"); }`,
		"var NAV_AVATAR_PX = 28;",
	}
	for _, name := range []string{"AVATAR_HIDPI_SCALE", "AVATAR_FALLBACK_HUES", "_liveHiveUsers"} {
		parts = append(parts, countryJSDecl(t, name))
	}
	for _, name := range []string{
		"ghProfileURL", "avatarProfileLink", "avatarInitials", "avatarFallbackHue",
		"avatarInitialsSVG", "isUserLive", "avatarImg", "linkedAvatar",
		"userDisplayLabel", "userAvatar", "identityProviderFromKey",
		"navAvatarHTML", "accessFace",
	} {
		parts = append(parts, jsFunc(t, name))
	}
	return strings.Join(parts, "\n")
}

func runNode(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node assertions failed: %v\n%s", err, out)
	}
}

func TestDashboardLinksOnlyGitHubIdentitiesToGitHub(t *testing.T) {
	runNode(t, avatarHarness(t)+`
let nav = navAvatarHTML({login: 'octocat', provider: 'github', avatar_url: 'https://github.com/octocat.png'}, 'octocat — User');
assert.match(nav, /<a href="https:\/\/github\.com\/octocat"/, 'GitHub viewer keeps the profile link');

nav = navAvatarHTML({login: 'Ada Lovelace', provider: 'custom', avatar_url: 'https://idp.example/ada.png'}, 'Ada Lovelace — User');
assert.doesNotMatch(nav, /<a /, 'OIDC viewer gets no anchor');
assert.doesNotMatch(nav, /github\.com/, 'OIDC viewer gets nothing from github.com');
assert.match(nav, /src="https:\/\/idp\.example\/ada\.png"/, 'OIDC viewer keeps the stored avatar');

let face = accessFace({username: 'octocat', provider: 'github'}, 18, 'octocat — owner', '');
assert.match(face, /<a href="https:\/\/github\.com\/octocat"/, 'GitHub member keeps the profile link');
assert.match(face, /src="https:\/\/github\.com\/octocat\.png/, 'GitHub member keeps the github.com avatar');

for (const a of [
  {username: 'custom:4f2a9c', provider: 'custom', display_label: 'Ada Lovelace', avatar_url: 'https://idp.example/ada.png'},
  {username: 'custom:4f2a9c', display_label: 'Ada Lovelace'},
]) {
  face = accessFace(a, 18, 'Ada Lovelace — owner', '');
  assert.doesNotMatch(face, /<a /, 'OIDC member gets no anchor');
  assert.doesNotMatch(face, /github\.com/, 'OIDC member gets nothing from github.com');
}
`)
}

func TestMyHivesLinksOnlyGitHubIdentitiesToGitHub(t *testing.T) {
	b, err := os.ReadFile("static/my-hives.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	parts := []string{
		"const assert = require('node:assert/strict');",
		"function esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\"/g, '&quot;').replace(/'/g, '&#39;'); }",
		"function escAttr(s) { return esc(s); }",
	}
	for _, name := range []string{"AVATAR_HIDPI_SCALE", "INLINE_ACCESS_AVATAR_PX", "ACCESS_ROLE_COLORS", "ACCESS_ROLE_COLOR_DEFAULT", "AVATAR_FALLBACK_HUES", "_liveHiveUsers"} {
		m := regexp.MustCompile(`(?m)^var ` + regexp.QuoteMeta(name) + ` = .*;`).FindString(page)
		if m == "" {
			t.Fatalf("declaration of %s not found in my-hives.html", name)
		}
		parts = append(parts, m)
	}
	for _, name := range []string{
		"jsArg", "accessRoleColor", "ghProfileURL", "avatarInitials",
		"avatarFallbackHue", "avatarInitialsSVG", "isUserLive", "avatarProfileLink",
		"avatarImg", "linkedAvatar", "fmtHours", "fmtUserTS", "relTime",
		"accessAvatarTitle", "inlineAccessAvatar", "navProfileURL",
	} {
		parts = append(parts, jsFuncIn(t, page, "", name))
	}
	runNode(t, strings.Join(parts, "\n")+`
assert.equal(navProfileURL({login: 'octocat', provider: 'github'}), 'https://github.com/octocat');
assert.equal(navProfileURL({login: 'Ada Lovelace', provider: 'custom'}), '', 'OIDC viewer gets no GitHub profile link');

let face = inlineAccessAvatar({username: 'octocat', provider: 'github', role: 'owner'});
assert.match(face, /<a href="https:\/\/github\.com\/octocat"/, 'GitHub member keeps the profile link');

face = inlineAccessAvatar({username: 'custom:4f2a9c', provider: 'custom', role: 'owner', display_label: 'Ada Lovelace', avatar_url: 'https://idp.example/ada.png'});
assert.doesNotMatch(face, /<a /, 'OIDC member gets no anchor');
assert.doesNotMatch(face, /github\.com/, 'OIDC member gets nothing from github.com');
assert.match(face, /src="https:\/\/idp\.example\/ada\.png"/, 'OIDC member keeps the stored avatar');
`)
}
