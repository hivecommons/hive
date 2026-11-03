package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// TestNavAvatarMenuOpensOnMobile guards two regressions that together made the
// account menu unreachable on phones: iOS Safari does not synthesise a click
// from a tap on a bare <img> that only has a delegated document handler, so
// the avatar must live inside a real <button>; and the phone topbar strip is
// overflow:hidden so the dropdown must leave the strip (position: fixed) in
// every media block that clips .oc-topbar-right.
func TestNavAvatarMenuOpensOnMobile(t *testing.T) {
	topbar := dashboardTopbarHTML(t)
	btnRE := regexp.MustCompile(`<button[^>]*id="oc-gh-avatar-btn"[^>]*>\s*<img[^>]*id="oc-gh-avatar"`)
	if !btnRE.MatchString(topbar) {
		t.Fatal("the topbar avatar <img> is not wrapped in the #oc-gh-avatar-btn <button>")
	}
	btnTag := regexp.MustCompile(`<button[^>]*id="oc-gh-avatar-btn"[^>]*>`).FindString(topbar)
	for _, want := range []string{`type="button"`, `data-action="toggleGHUserMenu"`, `aria-haspopup="menu"`, `aria-expanded="false"`, `aria-controls="oc-gh-user-menu"`, `aria-label="`} {
		if !strings.Contains(btnTag, want) {
			t.Errorf("avatar menu trigger is missing %s: %s", want, btnTag)
		}
	}
	imgTag := regexp.MustCompile(`<img[^>]*id="oc-gh-avatar"[^>]*>`).FindString(topbar)
	if strings.Contains(imgTag, `data-action=`) {
		t.Errorf("the avatar <img> must not carry its own data-action (the wrapping button does): %s", imgTag)
	}

	html := indexHTML(t)
	fixedRE := regexp.MustCompile(`#oc-gh-user-menu\s*\{\s*position:\s*fixed;`)
	clipRE := regexp.MustCompile(`\.oc-topbar-right\s*\{[^}]*overflow:\s*hidden;`)
	for _, media := range []string{
		`@media (max-width: 768px) {`,
		`@media (max-width: 1024px) and (max-height: 500px) and (orientation: landscape) and (pointer: coarse) {`,
	} {
		// The same media query opens several blocks across the stylesheet;
		// every block that clips .oc-topbar-right must also pin the menu.
		clipping := 0
		for rest := html; ; {
			start := strings.Index(rest, media)
			if start < 0 {
				break
			}
			block := mediaBlockBody(rest[start:])
			rest = rest[start+len(media):]
			if !clipRE.MatchString(block) {
				continue
			}
			clipping++
			if !fixedRE.MatchString(block) {
				t.Errorf("a %s block clips .oc-topbar-right but does not pin #oc-gh-user-menu with position: fixed", media)
			}
		}
		if clipping == 0 {
			t.Fatalf("no %s block clips .oc-topbar-right any more; update this test if the layout changed", media)
		}
	}

	for _, want := range []string{
		"function hiveSyncGHUserMenuTrigger()",
		"btn.setAttribute('aria-expanded', menu.hidden ? 'false' : 'true');",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q: aria-expanded on the avatar button is not kept in sync", want)
		}
	}
}

// mediaBlockBody returns the body of the @media block that starts at the
// beginning of s, matching braces so nested rules are included.
func mediaBlockBody(s string) string {
	open := strings.Index(s, "{")
	if open < 0 {
		return ""
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[open+1 : i]
			}
		}
	}
	return s[open+1:]
}
