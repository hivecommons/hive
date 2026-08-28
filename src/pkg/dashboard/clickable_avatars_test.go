package dashboard

import (
	"strings"
	"testing"
)

// indexHTML returns the spoke dashboard's single-page markup from the embedded
// FS. The avatar markup is JS inside that file, invisible to the Go compiler, so
// these tests are the only automated check that the faces stay linked, safe and
// layout-neutral.
func indexHTML(t *testing.T) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	return string(b)
}

// TestAvatarSitesUseSharedHelper asserts each avatar-rendering site goes through
// the shared helper rather than hand-rolling an <img>. A site with its own
// markup is a site that will silently miss the anchor, the onerror fallback, or
// the escaping.
func TestAvatarSitesUseSharedHelper(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"contributor cards", "const avatarHtml = linkedAvatar(c.github_username, CONTRIBUTOR_AVATAR_PX,"},
		{"leaderboard rows", "? linkedAvatar(e.github_username, CONTRIBUTOR_AVATAR_PX, `@${e.github_username}`, e.avatar_url)"},
		// The access allowlist renders non-GitHub identities (ibmid:, google:,
		// microsoft:) too, and those have no github.com profile — a linked
		// avatar there produced a 404 that also pointed at whatever account
		// occupies that URL shape. It now goes through accessRowAvatar, which
		// DELEGATES to linkedAvatar for GitHub keys and renders an escaped
		// initials tile otherwise. The guard's intent is unchanged: no site
		// hand-rolls an <img>. TestAccessRowAvatarDelegates below pins the
		// delegation so this indirection cannot become a bypass.
		{"access tab allowlist", "accessRowAvatar(u.username, u.display_name || '', ACCESS_LIST_AVATAR_PX)"},
		// The audit log's GitHub-shaped actors still go through linkedAvatar;
		// provider-prefixed OIDC keys route through accessRowAvatar (initials
		// tile, no fabricated github.com link) — same split as the access tab.
		{"audit log actor", "face = linkedAvatar(actor, AUDIT_AVATAR_PX, actor, '', 'margin-right:4px');"},
		{"audit log OIDC actor", "face = accessRowAvatar(actor, actorLabel, AUDIT_AVATAR_PX);"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(html, tc.snippet) {
				t.Errorf("index.html is missing %q — did an avatar site stop using the shared helper?", tc.snippet)
			}
		})
	}
}

// TestAccessRowAvatarDelegates pins that accessRowAvatar still routes GitHub
// keys through the shared linkedAvatar helper, and escapes everything on the
// non-GitHub path. Without this, swapping the access list off linkedAvatar
// (see TestAvatarSitesUseSharedHelper) would be a way to bypass the shared
// helper's anchor, onerror fallback and escaping rather than an indirection
// through it.
func TestAccessRowAvatarDelegates(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "function accessRowAvatar(key, displayName, px)") {
		t.Fatal("accessRowAvatar is missing — the access list has no avatar helper")
	}
	// GitHub keys must still go through the shared helper, not a local <img>.
	if !strings.Contains(html, "if (provider === 'github') return linkedAvatar(key, px, title, '', 'margin-right:6px');") {
		t.Error("accessRowAvatar no longer delegates GitHub keys to linkedAvatar")
	}
	// A display name is attacker-influenced in a way an opaque subject is not,
	// so both the tooltip and the initial must be escaped.
	if !strings.Contains(html, `'<span title="' + esc(title) + '"`) {
		t.Error("accessRowAvatar must escape its title — display names are user-controlled")
	}
	if !strings.Contains(html, `var initial = esc((displayName || key || '?')`) {
		t.Error("accessRowAvatar must escape the initial it renders")
	}
}

// TestAvatarHelpersPresent asserts the shared helpers and their named sizes
// exist. A merge that drops one leaves faces unlinked or reintroduces a magic
// number, and nothing else in the build would notice.
func TestAvatarHelpersPresent(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function ghProfileURL(username)",
		"function avatarProfileLink(username, label, avatarHTML)",
		"function avatarImg(src, px, extraStyle)",
		"function linkedAvatar(username, px, label, avatarURL, extraStyle)",
		"const CONTRIBUTOR_AVATAR_PX = 28;",
		"const ACCESS_LIST_AVATAR_PX = 20;",
		"const AUDIT_AVATAR_PX = 16;",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
}

// TestAvatarAnchorSafety asserts the profile anchor opens a new tab without
// leaking the opener, and carries an accessible name so the link's purpose is
// clear without seeing the picture.
func TestAvatarAnchorSafety(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"`<a href=\"${esc(ghProfileURL(uname))}\" target=\"_blank\" rel=\"noopener noreferrer\" ` +",
		"`title=\"${name}\" aria-label=\"${name}\" ` +",
		"`style=\"display:inline-block;line-height:0;text-decoration:none\">${avatarHTML}</a>`",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("avatarProfileLink is missing %q", snippet)
		}
	}
}

// TestAvatarLinkDegradesWithoutUsername asserts a record with no username
// renders the bare avatar rather than an anchor to https://github.com/.
func TestAvatarLinkDegradesWithoutUsername(t *testing.T) {
	if !strings.Contains(indexHTML(t), "if (!uname) return avatarHTML;") {
		t.Error("avatarProfileLink no longer returns the unwrapped avatar for a blank username")
	}
}

// TestAvatarOnErrorFallbackKept asserts the 404-avatar fallback survived the
// move into an anchor: a broken-image glyph inside a link reads as a dead
// control.
func TestAvatarOnErrorFallbackKept(t *testing.T) {
	if !strings.Contains(indexHTML(t), `data-error-action="gh19"`) {
		t.Error("avatarImg no longer hides a broken avatar image")
	}
}

// TestProfileLinksAlwaysGithubCom asserts profile links are built from
// github.com and never from a per-hive Enterprise host. The account that
// authenticated is a github.com account even on a hive whose org lives on GHE.
func TestProfileLinksAlwaysGithubCom(t *testing.T) {
	if !strings.Contains(indexHTML(t), "return `https://github.com/${encodeURIComponent(String(username || ''))}`;") {
		t.Error("ghProfileURL no longer builds a github.com profile URL from an encoded username")
	}
}

// TestRedundantProfileAffordancesRemoved asserts the second anchors that pointed
// at the profile the avatar now links to are gone — the contributor card's
// @username link and the leaderboard row's @username link — while the links that
// go SOMEWHERE ELSE (task issue links) survive.
func TestRedundantProfileAffordancesRemoved(t *testing.T) {
	html := indexHTML(t)
	for _, gone := range []string{
		"<a href=\"${ghLink}\" target=\"_blank\" rel=\"noopener\" style=\"color:var(--blue);text-decoration:none;font-weight:600\">@${esc(c.github_username)}</a>",
		"<a href=\"${ghLink}\" target=\"_blank\" rel=\"noopener\" style=\"color:var(--blue);text-decoration:none\">@${esc(e.github_username)}</a>",
	} {
		if strings.Contains(html, gone) {
			t.Errorf("a redundant @username profile anchor survives; the avatar replaces it: %q", gone)
		}
	}
	// Task links point at an issue, not a profile — they must NOT be removed.
	if !strings.Contains(html, "https://github.com/${esc(t.repo)}/issues/${t.number}") {
		t.Error("the contributor card's task issue link was removed; it is a different destination")
	}
}

// TestAnonymousAuditActorIsNotLinked asserts the audit log's placeholder face
// for an entry with no actor is rendered UNLINKED. 'ghost' is a real github.com
// account; linking the placeholder would send the reader to a stranger's
// profile and imply they performed the action.
func TestAnonymousAuditActorIsNotLinked(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "face = avatarImg(ghProfileURL('ghost') + '.png', AUDIT_AVATAR_PX, 'margin-right:4px');") {
		t.Error("the audit log's actorless placeholder face is no longer rendered unlinked")
	}
	if strings.Contains(html, "linkedAvatar(e.user||'ghost'") {
		t.Error("the audit log links its 'ghost' placeholder to a real github.com account")
	}
}

// TestNavAvatarKeepsItsMenu asserts the top-bar avatar still opens the user menu
// rather than being repurposed into a profile link — the menu is a different
// destination — and that the profile is reachable from a menu entry instead.
func TestNavAvatarKeepsItsMenu(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, `id="oc-gh-avatar"`) || !strings.Contains(html, `data-action="toggleGHUserMenu"`) {
		t.Error("the top-bar avatar no longer opens the user menu")
	}
	if !strings.Contains(html, `id="oc-gh-menu-profile"`) {
		t.Error("the user menu has no GitHub profile entry")
	}
	if !strings.Contains(html, "profEl.href = ghProfileURL(username);") {
		t.Error("the user menu's profile entry is not pointed at the signed-in account")
	}
}

// TestNoLiteralBodyTagInComments guards the snapshot builder, which locates the
// document body with indexOf("<body>"). A literal opening body tag inside any
// comment or string in this file corrupts that build.
func TestNoLiteralBodyTagInComments(t *testing.T) {
	const openBody = "<" + "body>"
	if got := strings.Count(indexHTML(t), openBody); got != 1 {
		t.Errorf("found %d occurrences of the opening body tag, want exactly 1 (the real one) — "+
			"a copy inside a comment or string corrupts the snapshot builder", got)
	}
}
