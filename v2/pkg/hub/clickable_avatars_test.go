package hub

import (
	"regexp"
	"strings"
	"testing"
)

// Every user avatar in the hub dashboard is a link to that person's GitHub
// profile. The markup lives in the inline dashboardHTML JS, invisible to the Go
// compiler, so these tests pin the properties that would otherwise only surface
// as a broken, unsafe, or unreachable control in a browser.

// TestAvatarSitesUseSharedHelper asserts every avatar-rendering site goes
// through the shared helpers rather than hand-rolling an <img>. A site that
// builds its own markup is a site that will silently miss the anchor, the
// onerror fallback, or the escaping.
func TestAvatarSitesUseSharedHelper(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		{"inline per-hive faces", "return linkedAvatar(uname, INLINE_ACCESS_AVATAR_PX,"},
		{"status-dot hover panel rows", "linkedAvatar(a.username, PANEL_ACCESS_AVATAR_PX,"},
		{"per-hive pending request rows", "var avatar = linkedAvatar(pr.username, LIST_AVATAR_PX, pr.username, 'margin-right:6px');"},
		{"past requests table", "linkedAvatar(uname, PANEL_ACCESS_AVATAR_PX, uname, 'margin-right:6px')"},
		{"pending provision cards", "var avatar = linkedAvatar(pr.username, TABLE_AVATAR_PX, pr.username, 'margin-right:8px');"},
		{"admin users table", "var avatar = linkedAvatar(u.github_username, TABLE_AVATAR_PX,"},
		{"access modal pending requests", "var avatar = linkedAvatar(r.username, LIST_AVATAR_PX, r.username, 'margin-right:6px');"},
		{"access modal access list", "var avatar = linkedAvatar(u.username, LIST_AVATAR_PX,"},
		{"nav bar viewer avatar", "avatarProfileLink(data.login, String(data.login || '') + ' — ' + roleText,"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q — did an avatar site stop using the shared helper?", tc.snippet)
			}
		})
	}
}

// TestNoHandRolledAvatarImages asserts no site builds a github.com avatar <img>
// outside the shared helper. Exactly one occurrence of the ".png?size=" pattern
// is expected: the one inside avatarImg().
func TestNoHandRolledAvatarImages(t *testing.T) {
	const marker = ".png?size="
	if got := strings.Count(dashboardHTML, marker); got != 1 {
		t.Errorf("found %d occurrences of %q in dashboardHTML, want exactly 1 (only avatarImg may build an avatar URL)", got, marker)
	}
	// The other hand-rolled shape: 'https://github.com/' + <expr> + '.png'.
	if strings.Contains(dashboardHTML, `+ '.png"`) {
		t.Error("dashboardHTML still builds an avatar <img> src inline; use avatarImg()/linkedAvatar()")
	}
}

// TestAvatarAnchorSafety asserts the profile anchor carries the attributes that
// make a new-tab link safe and legible: rel="noopener noreferrer" so the opened
// tab cannot reach back through window.opener, and an accessible name so the
// link's purpose is clear without seeing the picture.
func TestAvatarAnchorSafety(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		{"opens in a new tab", `target="_blank"`},
		{"blocks opener and referrer", `rel="noopener noreferrer"`},
		{"names the link for assistive tech", `aria-label="' + escAttr(label || uname) + '"`},
		{"native tooltip names the user too", `title="' + escAttr(label || uname) + '"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("avatarProfileLink is missing %q", tc.snippet)
			}
		})
	}
}

// TestAvatarLinkDegradesWithoutUsername asserts a record with no username
// renders the bare avatar rather than an anchor to https://github.com/ — a link
// to a profile that cannot exist is worse than no link.
func TestAvatarLinkDegradesWithoutUsername(t *testing.T) {
	if !strings.Contains(dashboardHTML, "if (!uname) return avatarHTML;") {
		t.Error("avatarProfileLink no longer returns the unwrapped avatar for a blank username")
	}
}

// TestAvatarOnErrorFallbackKept asserts the 404-avatar fallback survived the
// move into an anchor. A broken-image glyph inside a link reads as a dead
// control, which is worse than the gap it replaced.
func TestAvatarOnErrorFallbackKept(t *testing.T) {
	if !strings.Contains(dashboardHTML, `onerror="this.style.visibility=\'hidden\'"`) {
		t.Error("avatarImg no longer hides a broken avatar image")
	}
}

// TestAvatarAnchorDoesNotDisturbRowHeight asserts the anchor is laid out so it
// cannot add a text baseline's worth of height to a dense table row. A bare
// inline anchor inherits its parent's line-height and would shift rows.
func TestAvatarAnchorDoesNotDisturbRowHeight(t *testing.T) {
	for _, snippet := range []string{"display:inline-block", "line-height:0"} {
		if !strings.Contains(dashboardHTML, "style=\"display:inline-block;line-height:0;text-decoration:none\"") {
			t.Fatalf("avatarProfileLink is missing the layout-neutral anchor style (looking for %q)", snippet)
		}
	}
}

// TestProfileLinksAlwaysGithubCom asserts profile links are built from
// github.com and never from a per-record github_host. The account that signs in
// to the hub is a github.com account even when the ORG it works on lives on a
// GitHub Enterprise host; only REPO links take their origin from the record.
func TestProfileLinksAlwaysGithubCom(t *testing.T) {
	if !strings.Contains(dashboardHTML, `return 'https://github.com/' + encodeURIComponent(String(username || ''));`) {
		t.Error("ghProfileURL no longer builds a github.com profile URL from an encoded username")
	}
	// ghRepoURL is the one that must consult github_host; assert the split is
	// still real so a future edit cannot quietly merge the two.
	if !strings.Contains(dashboardHTML, "function ghRepoURL(") {
		t.Error("ghRepoURL is gone; repo links must still be built from the record's github_host")
	}
	if strings.Contains(dashboardHTML, "ghProfileURL(username, host)") {
		t.Error("ghProfileURL took on a host argument; profile links must stay on github.com")
	}
}

// TestRedundantProfileAffordancesRemoved asserts the separate link affordances
// that pointed at the profile the avatar now links to are gone: the admin Users
// table's ↗ glyph and the Past Requests table's second anchor on the username.
func TestRedundantProfileAffordancesRemoved(t *testing.T) {
	if strings.Contains(dashboardHTML, `title="GitHub profile" style="color:var(--muted);font-size:0.65rem;margin-right:4px">↗</a>`) {
		t.Error("the admin Users table still renders the ↗ profile glyph; the avatar replaces it")
	}
	if strings.Contains(dashboardHTML, `'<a href="https://github.com/' + encodeURIComponent(uname) + '" target="_blank" rel="noopener noreferrer" style="color:inherit">' + esc(uname) + '</a>'`) {
		t.Error("the Past Requests table still links the username separately; the avatar replaces it")
	}
	// The assign-flow anchor on the username must SURVIVE: it goes somewhere
	// else (the assign modal), so it is not redundant with the profile link.
	if !strings.Contains(dashboardHTML, `onclick="openAssignForUser(\'`) {
		t.Error("the admin Users table lost its assign-flow link, which is a different destination")
	}
}

// TestAvatarSizesAreNamedConstants asserts no avatar site hardcodes a pixel
// size at the call site.
func TestAvatarSizesAreNamedConstants(t *testing.T) {
	for _, decl := range []string{
		"var PANEL_ACCESS_AVATAR_PX = 18;",
		"var LIST_AVATAR_PX = 20;",
		"var TABLE_AVATAR_PX = 24;",
		"var AVATAR_HIDPI_SCALE = 2;",
	} {
		if !strings.Contains(dashboardHTML, decl) {
			t.Errorf("dashboardHTML is missing the named size constant %q", decl)
		}
	}
	// No call site may pass a bare number as the size argument.
	numericSize := regexp.MustCompile(`linkedAvatar\([^,]+,\s*\d`)
	if m := numericSize.FindString(dashboardHTML); m != "" {
		t.Errorf("a linkedAvatar call passes a magic pixel size: %q", m)
	}
}
