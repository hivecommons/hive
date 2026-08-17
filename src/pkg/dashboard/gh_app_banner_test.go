package dashboard

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// spokeIndexHTML loads the real spoke dashboard file the server serves.
func spokeIndexHTML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading static/index.html: %v", err)
	}
	return string(b)
}

// TestGitHubAppBannerHandlesOperatorSideStates locks the banner contract: the
// spoke dashboard must branch on the classified state and must not send a user
// to an install page when the failure is the operator's to fix.
func TestGitHubAppBannerHandlesOperatorSideStates(t *testing.T) {
	html := spokeIndexHTML(t)

	required := []struct {
		snippet string
		why     string
	}{
		{"ghAppIsOperatorSide", "the operator-side predicate must exist"},
		{"GH_APP_STATE_KEY_MISSING = 'key-missing'", "the key-missing wire token must match github.AppStateKeyMissing.String()"},
		{"GH_APP_STATE_KEY_INVALID = 'key-invalid'", "the key-invalid wire token must match github.AppStateKeyInvalid.String()"},
		{"data.githubAppState", "the banner must read the classified state from the status payload"},
		{"operator-issue", "operator-side failures need their own non-alarming styling"},
		{"no action needed from you", "the operator-side banner must tell the user they need do nothing"},
	}
	for _, r := range required {
		if !strings.Contains(html, r.snippet) {
			t.Errorf("static/index.html is missing %q — %s", r.snippet, r.why)
		}
	}
}

// TestGitHubAppBannerOperatorBranchOffersNoInstallLink is the specific
// regression for the reported bug: a user whose App is installed correctly but
// whose spoke holds the wrong key was shown "Install the GitHub App". The
// operator-side branch must hide the action link instead.
func TestGitHubAppBannerOperatorBranchOffersNoInstallLink(t *testing.T) {
	html := spokeIndexHTML(t)

	const branchStart = "if (isOperatorSide) {"
	i := strings.Index(html, branchStart)
	if i < 0 {
		t.Fatal("static/index.html has no operator-side banner branch")
	}
	// Bound the branch at its early return, which is where it ends.
	rest := html[i:]
	end := strings.Index(rest, "return;")
	if end < 0 {
		t.Fatal("operator-side branch does not return early")
	}
	branch := rest[:end]

	if !strings.Contains(branch, "oLink.removeAttribute('href')") {
		t.Error("the operator-side branch must remove the install link href")
	}
	// The forbidden strings are the exact misdirection the fix removes.
	for _, forbidden := range []string{"githubAppInstallURL", "Install GitHub App"} {
		if strings.Contains(branch, forbidden) {
			t.Errorf("the operator-side branch must not reference %q — the user cannot fix this by installing", forbidden)
		}
	}
	if !strings.Contains(branch, "hub administrator") && !strings.Contains(branch, "githubAppPermIssue") {
		t.Error("the operator-side branch must point the reader at the hub administrator")
	}
}

// TestWelcomeChecklistDoesNotBlameUserForOperatorFailure guards the journey
// nudge on the spoke: an operator-side block is not an unfinished user step.
func TestWelcomeChecklistDoesNotBlameUserForOperatorFailure(t *testing.T) {
	html := spokeIndexHTML(t)

	const marker = "if (ghAppIsOperatorSide(data.githubAppState)) return null;"
	if !strings.Contains(html, marker) {
		t.Error("the github-app welcome step must return null (unknown), not false, for an operator-side block")
	}
	if !strings.Contains(html, "installing the App again will not help") {
		t.Error("welcomeInstallGitHubApp must refuse to open an install page for an operator-side block")
	}
}

// TestGitHubAppStatusFieldIsExposed proves the classified state actually
// reaches the browser — the banner logic above is dead code without it.
func TestGitHubAppStatusFieldIsExposed(t *testing.T) {
	f, ok := reflect.TypeOf(StatusPayload{}).FieldByName("GitHubAppState")
	if !ok {
		t.Fatal("StatusPayload has no GitHubAppState field — the banner logic would be dead code")
	}
	// The JSON tag is the name the dashboard actually reads.
	if got := f.Tag.Get("json"); !strings.HasPrefix(got, "githubAppState") {
		t.Errorf("GitHubAppState json tag = %q, want it to serialise as githubAppState", got)
	}
}

// TestSetGitHubAppStateRoundTrips covers the setter/getter and the clearing
// contract: turning the requirement off must also clear the classified state,
// or a stale operator-side token would suppress future genuine nudges.
func TestSetGitHubAppStateRoundTrips(t *testing.T) {
	s := &Server{}
	s.SetGitHubAppRequired(true)
	s.SetGitHubAppState("key-invalid")
	if got := s.GetGitHubAppState(); got != "key-invalid" {
		t.Fatalf("GetGitHubAppState() = %q, want key-invalid", got)
	}
	s.SetGitHubAppRequired(false)
	if got := s.GetGitHubAppState(); got != "" {
		t.Errorf("GetGitHubAppState() = %q after clearing the requirement, want empty", got)
	}
}

// TestGitHubAppBannerStartTagIsClosed guards a whole class of silent HTML
// corruption that no JS syntax check can catch.
//
// The banner's opening <div> shipped for seven weeks with no ">" at all:
//
//	<div id="gh-app-install-banner" class="gh-app-install-banner" hidden
//	  <span style="font-size:1.5rem">&#9888;&#65039;</span>
//
// Per the HTML5 tokenizer, "<" is a legal attribute-name character, so the
// parser stayed in the div's start tag and consumed "<span" as an attribute
// NAME and "font-size:1.5rem" as the value of a "style" attribute it then
// hung on the DIV. The ">" that was meant to close the <span> closed the
// <div> instead. Net effect: the ⚠️ glyph was swallowed into the tag and
// never rendered, and the banner div acquired a bogus inline
// style="font-size:1.5rem" that fights the cssText the show/hide code
// assigns. node --check sees none of this — the script bodies are untouched.
//
// Assert on the parsed shape rather than the literal text so a future
// reformat of the attribute list cannot silently retire the guard.
func TestGitHubAppBannerStartTagIsClosed(t *testing.T) {
	html := spokeIndexHTML(t)

	const bannerID = `id="gh-app-install-banner"`
	start := strings.Index(html, bannerID)
	if start < 0 {
		t.Fatalf("static/index.html no longer contains %s", bannerID)
	}
	// Walk back to the "<" that opens this element, then forward to the first
	// ">" — everything between them is the start tag as the browser sees it.
	open := strings.LastIndex(html[:start], "<")
	if open < 0 {
		t.Fatalf("could not find the opening < for the banner div")
	}
	end := strings.Index(html[open:], ">")
	if end < 0 {
		t.Fatalf("banner div start tag is never terminated by >")
	}
	tag := html[open : open+end+1]

	// A well-formed start tag cannot contain another "<": if it does, the
	// tokenizer has swallowed the next element into this tag's attributes.
	if strings.Contains(tag[1:], "<") {
		t.Errorf("banner div start tag swallowed a following element — missing '>':\n%s", tag)
	}
	// The warning glyph must be real text content, not tag innards.
	afterTag := html[open+end+1:]
	if !strings.HasPrefix(strings.TrimSpace(afterTag), "<span") {
		t.Errorf("expected the warning <span> to follow the banner div start tag, got: %.80s", strings.TrimSpace(afterTag))
	}
}

// TestInstallIdHintPopoverHasViewportCollisionHandling is the regression
// guard for the reported bug: the install-ID help popover (".gh-idhint-pop")
// defaults to `bottom: 100%`, anchoring itself above the "?" button. When the
// GitHub-App-install banner sits near the top of the viewport there isn't
// enough room above the anchor and the popover overflowed off the top of the
// browser window, clipping its lead text. The fix adds JS collision handling
// that measures the anchor via getBoundingClientRect() and flips the popover
// below the anchor (via the .gh-idhint-pop-below class) when it doesn't fit
// above, plus a horizontal clamp for left/right edges. This test locks that
// the flip/clamp machinery — not just the popover markup — is present.
func TestInstallIdHintPopoverHasViewportCollisionHandling(t *testing.T) {
	html := spokeIndexHTML(t)

	required := []struct {
		snippet string
		why     string
	}{
		{"gh-idhint-pop-below", "CSS needs a flip class the JS can toggle to render the popover below its anchor"},
		{"function positionInstallIdHint", "the collision-handling function must exist"},
		{"getBoundingClientRect", "positioning must measure the real anchor/popover geometry, not assume it fits"},
		{"classList.toggle('gh-idhint-pop-below'", "the function must flip the popover by toggling the CSS class"},
		{"pop.style.left", "the function must also clamp horizontally so it can't run off the left/right edge"},
	}
	for _, r := range required {
		if !strings.Contains(html, r.snippet) {
			t.Errorf("static/index.html is missing %q — %s", r.snippet, r.why)
		}
	}

	// The click/keyboard toggle must trigger positioning when opening the
	// popover — a flip/clamp function that exists but is never called on
	// show would leave the original clipping bug in place.
	i := strings.Index(html, "function toggleInstallIdHint(event)")
	if i < 0 {
		t.Fatal("static/index.html has no toggleInstallIdHint function")
	}
	end := strings.Index(html[i:], "\n    }")
	if end < 0 {
		t.Fatal("could not find the end of toggleInstallIdHint")
	}
	body := html[i : i+end]
	if !strings.Contains(body, "positionInstallIdHint") {
		t.Error("toggleInstallIdHint does not call positionInstallIdHint — opening the popover via click/keyboard would skip collision handling")
	}

	// The popover is also shown by pure CSS on hover/focus-within with no
	// click involved, so hover/focus triggers must run the same positioning
	// logic or hover users would still see the clipped popover.
	if !strings.Contains(html, "mouseenter") || !strings.Contains(html, "focusin") {
		t.Error("expected hover (mouseenter) and keyboard-focus (focusin) listeners that also trigger positionInstallIdHint")
	}
}

// TestInstallIdInputIsCrossBrowserHardened is the regression guard for the
// user report that entering the installation ID and clicking "Set ID" worked
// instantly in Safari but "did not stick" in Firefox on Linux. Since the same
// backend serves both (Safari proves the API works), the cause is a Firefox
// client-side input/event/DOM difference. This locks the defensive hardening:
//
//   - The Set-ID button is type="button". A <button> with no type defaults to
//     type=submit; if the banner were ever wrapped in a form (or Firefox
//     treated an ancestor as one) the click would trigger an implicit submit
//     that resets the page and clears the field before the handler ran.
//   - The input has autocomplete="off" so Firefox form history cannot
//     offer/refill/clear the value.
//   - Enter in the input also submits, so entry works even if the Set-ID
//     button is obscured (e.g. the "?" popover) or its click is flaky.
//   - renderGitHubAppBanner (which runs on every poll) preserves a value the
//     user is mid-typing, so a background re-render can never wipe it.
func TestInstallIdInputIsCrossBrowserHardened(t *testing.T) {
	html := spokeIndexHTML(t)

	// Isolate the Set-ID button's start tag and require an explicit
	// type="button" — the exact fix for the implicit-submit reset.
	const setBtnID = `id="gh-app-set-id-btn"`
	bi := strings.Index(html, setBtnID)
	if bi < 0 {
		t.Fatalf("static/index.html no longer contains %s", setBtnID)
	}
	btnOpen := strings.LastIndex(html[:bi], "<button")
	if btnOpen < 0 {
		t.Fatalf("could not find the opening <button for the Set-ID control")
	}
	btnEnd := strings.Index(html[btnOpen:], ">")
	btnTag := html[btnOpen : btnOpen+btnEnd+1]
	if !strings.Contains(btnTag, `type="button"`) {
		t.Errorf("Set-ID button must be type=\"button\" (a default <button> is type=submit and an implicit form submit resets the field in Firefox before submitGitHubInstallationID runs):\n%s", btnTag)
	}
	// The save button must LOOK like a button: it inherits the banner's solid
	// primary .gh-app-btn recipe. An inline transparent override once made it
	// read as static text — operators typed the ID and never clicked (user
	// report: "set id does not look like a button").
	if strings.Contains(btnTag, "background:transparent") {
		t.Errorf("the save-installation-ID button must not be a transparent ghost — it is the submit affordance:\n%s", btnTag)
	}
	// And its label must say what it does with what: "Set ID" told users
	// neither the verb's object nor that pressing it was required.
	btnClose := strings.Index(html[btnOpen:], "</button>")
	if btnLabel := html[btnOpen+btnEnd+1 : btnOpen+btnClose]; !strings.Contains(btnLabel, "Save Installation ID") {
		t.Errorf("save button label = %q, want %q", btnLabel, "Save Installation ID")
	}

	// Isolate the install-ID input's start tag: it must disable autocomplete
	// and wire an Enter-key submit.
	const inputID = `id="gh-app-install-id-input"`
	ii := strings.Index(html, inputID)
	if ii < 0 {
		t.Fatalf("static/index.html no longer contains %s", inputID)
	}
	inOpen := strings.LastIndex(html[:ii], "<input")
	inEnd := strings.Index(html[inOpen:], ">")
	inTag := html[inOpen : inOpen+inEnd+1]
	if !strings.Contains(inTag, `autocomplete="off"`) {
		t.Errorf("install-ID input must set autocomplete=\"off\" so Firefox form history can't clear/refill the value:\n%s", inTag)
	}
	if !strings.Contains(inTag, "submitGitHubInstallationID()") || !strings.Contains(inTag, "Enter") {
		t.Errorf("install-ID input must submit on Enter so entry works even if the Set-ID button is obscured or its click is flaky in Firefox:\n%s", inTag)
	}

	// The per-poll banner render must not clobber a value the user is typing.
	ri := strings.Index(html, "function renderGitHubAppBanner(data)")
	if ri < 0 {
		t.Fatal("static/index.html has no renderGitHubAppBanner function")
	}
	end := strings.Index(html[ri:], "\n    }")
	body := html[ri : ri+end]
	if !strings.Contains(body, "priorIdValue") {
		t.Error("renderGitHubAppBanner must snapshot/restore the install-ID input value so a background poll re-render can't wipe what the user typed (the Firefox \"doesn't stick\" symptom)")
	}
}

// cssRule extracts the body of the first CSS rule whose selector line starts
// with the given prefix, so tests can assert on one rule without matching
// stray occurrences of a declaration elsewhere in the stylesheet.
func cssRule(t *testing.T, html, selector string) string {
	t.Helper()
	i := strings.Index(html, selector+" {")
	if i < 0 {
		t.Fatalf("static/index.html has no %q CSS rule", selector)
	}
	end := strings.Index(html[i:], "}")
	if end < 0 {
		t.Fatalf("could not find the end of the %q CSS rule", selector)
	}
	return html[i : i+end]
}

// TestInstallIdHintURLTailVisibleAtRest is the regression guard for the
// follow-up report on the install-ID hint's example URL (post-#2710): the URL
// row was preserved in a horizontally-scrollable column, but the column
// rested scrolled to the START of the URL, so the /settings/installations/<id>
// tail — the entire point of the hint — was scrolled out of view at rest and
// the right-aligned "↑↑↑ this number" arrow row pointed at the truncation
// ("…organizations/open-") instead of at the id. Because the org name is
// dynamic, a separate character-aligned arrow row can never reliably line up
// under the digits, so the fix is twofold:
//
//  1. END-anchor the scroll column (direction: rtl on .gh-idhint-urlcol, with
//     the URL text restored to direction: ltr as an atomic inline-block) so
//     the resting view shows "…/settings/installations/<id>".
//  2. Delete the arrow row entirely and attach the "this number" callout to
//     the seg-id span itself via CSS ::after — intrinsic to the span, so it
//     tracks the digits at every scroll position and org-name length.
func TestInstallIdHintURLTailVisibleAtRest(t *testing.T) {
	html := spokeIndexHTML(t)

	// 1. End-anchored scroll column.
	col := cssRule(t, html, ".gh-idhint-urlcol")
	if !strings.Contains(col, "direction: rtl") {
		t.Error(".gh-idhint-urlcol is not end-anchored (direction: rtl) — at rest the column shows the URL's start and hides the /installations/<id> tail")
	}
	text := cssRule(t, html, ".gh-idhint-urltext")
	if !strings.Contains(text, "direction: ltr") {
		t.Error(".gh-idhint-urltext must restore direction: ltr inside the rtl scroll column or the URL renders right-to-left")
	}
	if !strings.Contains(text, "inline-block") {
		t.Error(".gh-idhint-urltext must be an atomic inline-block: a block child of the rtl column overflows to the unreachable side and breaks the end-anchor")
	}

	// 2. The callout is attached to the id segment itself, not a separate
	// aligned row.
	seg := cssRule(t, html, ".gh-idhint-url .seg-id")
	if !strings.Contains(seg, "position: relative") {
		t.Error(".seg-id must be position: relative so its ::after callout anchors to the digits")
	}
	after := cssRule(t, html, ".gh-idhint-url .seg-id::after")
	if !strings.Contains(after, "this number") {
		t.Error(".seg-id::after must carry the \"this number\" label so the callout moves with the digits")
	}
	if !strings.Contains(after, "position: absolute") {
		t.Error(".seg-id::after must be absolutely positioned off the span — any column- or character-based alignment cannot track a dynamic-width URL")
	}

	// The old arrow row must be gone: it could never align under a dynamic
	// URL and its right-aligned arrows pointed at whatever happened to be at
	// the visible column's right edge.
	if strings.Contains(html, "gh-idhint-caret") {
		t.Error("the gh-idhint-caret arrow row is back — the callout must be the seg-id ::after label, arrows in a separate row cannot line up under a dynamic-width URL")
	}
}
