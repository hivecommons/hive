package hub

import (
	"strings"
	"testing"
)

// The inline per-hive access avatars live in the inline dashboardHTML JS, so
// the Go compiler cannot see them at all. These tests pin the invariants that
// are cheap to break in a rebase (many agents edit saas.go concurrently) and
// that would otherwise only show up as a broken or unsafe row in a browser.

// TestInlineAccessAvatarHelpersPresent asserts the helpers and their named
// budgets exist. A merge that drops one would render rows with no faces, or
// reintroduce a magic number, and nothing else in the build would notice.
func TestInlineAccessAvatarHelpersPresent(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		{"viewer-excluding selector", "function otherHiveMembers(h)"},
		{"single face renderer", "function inlineAccessAvatar(a)"},
		{"inline summary renderer", "function hiveAccessAvatars(h)"},
		{"shared role colour helper", "function accessRoleColor(role)"},
		{"named face cap", "var INLINE_ACCESS_AVATAR_MAX"},
		{"named rendered size", "var INLINE_ACCESS_AVATAR_PX"},
		{"named HiDPI fetch scale", "var AVATAR_HIDPI_SCALE"},
		{"shared profile url helper", "function ghProfileURL(username)"},
		{"shared profile anchor helper", "function avatarProfileLink(username, label, avatarHTML)"},
		{"shared linked-avatar helper", "function linkedAvatar(username, px, label, extraStyle)"},
		{"named role colour map", "var ACCESS_ROLE_COLORS"},
		{"named role colour fallback", "var ACCESS_ROLE_COLOR_DEFAULT"},
		{"viewer identity state", "var _currentUser"},
		// The avatars are only useful if they are actually placed in the row.
		{"wired into the name cell", "var accessFaces = hiveAccessAvatars(h);"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q", tc.snippet)
			}
		})
	}
}

// TestCurrentUserCapturedFromAuth asserts the signed-in login is captured from
// the /api/auth/user payload and lowercased. Without the lowercasing the viewer
// would still see their own face whenever the roster and the auth payload
// disagree on casing, which GitHub allows.
func TestCurrentUserCapturedFromAuth(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		{"login read from auth payload", "_currentUser = String(data.login || '').toLowerCase();"},
		{"compared case-insensitively", "uname.toLowerCase() === _currentUser"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q", tc.snippet)
			}
		})
	}
}

// TestInlineAccessAvatarEscaping asserts the username — which is user-controlled
// and reaches both a URL path segment and a quoted attribute value — goes
// through the right escaper for each context. esc() escapes only & < >, so a
// login containing a double quote would close the attribute and everything
// after it would parse as markup.
func TestInlineAccessAvatarEscaping(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		// URL path segment. The inline faces now render through the shared
		// avatar helpers, so the escaping lives there — assert it at the source.
		{"profile url encodes the username", `return 'https://github.com/' + encodeURIComponent(String(username || ''));`},
		{"avatar url built from the encoded profile url", `'<img src="' + escAttr(ghProfileURL(username)) + '.png?size='`},
		// Quoted attribute values.
		{"anchor href uses escAttr", `'<a href="' + escAttr(ghProfileURL(uname)) + '" target="_blank" rel="noopener noreferrer" '`},
		// title now folds in a live line via a `title` var; both title and
		// aria-label still go through escAttr.
		{"anchor title uses escAttr", `'title="' + escAttr(title) + '" aria-label="' + escAttr(label || uname) + '" '`},
		// The inline face still carries the role in its tooltip, now built by the
		// shared accessAvatarTitle helper (username — role, plus contact metadata).
		{"inline face labels username and role via title helper", `? linkedAvatar(uname, INLINE_ACCESS_AVATAR_PX, accessAvatarTitle(a), extraStyle)`},
		{"access title helper carries username and role", `var lines = [uname + (role ? ' — ' + role : '')];`},
		{"overflow chip title uses escAttr", `'<span title="' + escAttr(hiddenNames.join(', ')) + '" '`},
		{"aria-label uses escAttr", `'<span class="hive-access-faces" aria-label="' + escAttr(label) + '" '`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing escaping %q", tc.snippet)
			}
		})
	}
}

// TestInlineAccessAvatarDegradesCleanly asserts the cases where the summary must
// render nothing at all: an empty container would still occupy its margin and
// gap and make those rows sit wider than their neighbours.
func TestInlineAccessAvatarDegradesCleanly(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		// Placeholder (unassigned) rows have no meaningful membership, and rows
		// the caller does not own carry no access list at all.
		{"placeholder and missing-hive guard", "if (!h || isPlaceholderHive(h)) return [];"},
		{"nil-safe access iteration", "var list = (h.access || []);"},
		{"blank usernames dropped", "if (!uname) continue;"},
		{"no members renders nothing", "if (!members.length) return '';"},
		{"cap applied before render", "var shown = members.slice(0, INLINE_ACCESS_AVATAR_MAX);"},
		{"overflow counted from the full list", "var overflow = members.length - shown.length;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q", tc.snippet)
			}
		})
	}
}

// TestInlineAvatarsCarryNoCustomPanel is the counterpart to
// TestSingleHoverPanelInvariant, from the other direction. That test asserts the
// status dot's custom panel carries no native title; this one asserts the inline
// faces — which DO carry a native title — never grow a custom hover panel. An
// element with both draws the browser tooltip on top of the panel, the overlap
// bug that was already fixed once.
func TestInlineAvatarsCarryNoCustomPanel(t *testing.T) {
	const marker = "function hiveAccessAvatars(h) {"
	idx := strings.Index(dashboardHTML, marker)
	if idx < 0 {
		t.Fatalf("hiveAccessAvatars not found in dashboardHTML")
	}
	// Scan the two functions that build the inline markup: the summary and the
	// single-face renderer that precedes it.
	start := strings.Index(dashboardHTML, "function inlineAccessAvatar(a) {")
	if start < 0 || start > idx {
		t.Fatalf("inlineAccessAvatar not found before hiveAccessAvatars")
	}
	rest := dashboardHTML[start:]
	end := strings.Index(rest, "\n    }\n\n    /* ---- Config drift")
	if end < 0 {
		// Fall back to the end of hiveAccessAvatars' body.
		end = strings.Index(rest[idx-start:], "\n    }")
		if end < 0 {
			t.Fatalf("could not find the end of the inline avatar helpers")
		}
		end += idx - start
	}
	body := rest[:end]
	// The class the status dot's panel is looked up by. If it ever appears in
	// the inline avatar markup, the faces have grown a second custom panel.
	for _, forbidden := range []string{"hive-access-pop", "hive-access-wrap"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("inline access avatars reference %q; they must carry a native title only, "+
				"never a custom hover panel (that is healthBadge's job). Markup:\n%s", forbidden, body)
		}
	}
}

// TestHoverPanelReusesSharedRoleColor asserts the status-dot panel and the
// inline faces derive role colours from the SAME helper. Two hardcoded palettes
// would drift and a face would eventually read as a different role than its own
// line in the panel.
func TestHoverPanelReusesSharedRoleColor(t *testing.T) {
	if !strings.Contains(dashboardHTML, "var rc = accessRoleColor(a.role);") {
		t.Error("healthBadge's access rows no longer use the shared accessRoleColor helper")
	}
	if !strings.Contains(dashboardHTML, "'border:1px solid ' + accessRoleColor(role) + ';") {
		t.Error("inline access avatars no longer use the shared accessRoleColor helper")
	}
}

// TestHiveTableColumnCountUnchanged asserts the inline avatars did NOT add a
// column. They live inside the existing name cell precisely so the header, the
// section-header colspan and the pending-expand colspan all stay correct; a
// stale count leaves the table visibly ragged.
//
// 12 = the columns left after the 15-to-9 fold (PROV, DRIFT, ACMM+JOURNEY→Maturity,
// ISSUES+PRS+CONTRIB→Activity), on top of the earlier Public→Location and
// AI-Author→Repos folds. The inline access avatars still add NO column — they live
// inside the existing name cell, so the header, section-header colspan and
// pending-expand colspan all stay correct.
// TestHiveTableColumnCountsAgree is the authority on this number: it counts the
// emitted <th> and <td> cells rather than trusting a literal, so update it
// first and follow it here.
func TestHiveTableColumnCountUnchanged(t *testing.T) {
	for _, snippet := range []string{
		"var TOTAL_COLUMNS = 13;",
		"var TOTAL_COLUMNS_HEADER = 13;",
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Errorf("dashboardHTML is missing %q — did the hive table gain or lose a column?", snippet)
		}
	}
}

// TestNonGitHubAvatarsNeverLinkToGitHub pins the reason the avatar sites stopped
// calling linkedAvatar unconditionally (see TestAvatarSitesUseSharedHelper).
//
// A key like "ibmid:5500087VJB" is not a github.com login. Building an avatar
// URL from it yields a 404 AND a profile link to whatever account happens to
// occupy that URL shape — attributing a stranger's face and identity to an
// internal user. Each site now branches on provider: GitHub keys keep the
// linked github.com avatar, everything else renders the provider-stored avatar
// or initials derived from the DISPLAY LABEL, never from the opaque subject.
func TestNonGitHubAvatarsNeverLinkToGitHub(t *testing.T) {
	for _, snippet := range []string{
		// Inline per-hive faces branch before linking.
		"var provider = a.provider || identityProviderFromKey(uname);",
		// The non-GitHub arm seeds initials from the display label, not the key.
		"userAvatar({display_name: a.display_label, avatar_url: a.avatar_url, github_username: uname},",
		// The admin table makes the same distinction.
		"var isGitHubUser = String(u.provider || 'github').toLowerCase() === 'github';",
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Errorf("dashboardHTML is missing %q — a non-GitHub identity may be linking to github.com", snippet)
		}
	}
}
