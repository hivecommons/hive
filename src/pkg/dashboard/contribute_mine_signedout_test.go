package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// #6937: the Operations tab's "Your contribution" card is identity-dependent,
// and it used to answer an anonymous viewer by staying hidden. Nothing on the
// page then said the panel existed, so /contribute looked complete and gave a
// signed-out visitor no hint that signing in would reveal anything — while the
// Profile tab, on the same page and for the same condition, already rendered a
// .me-signin prompt.
//
// The server side of that distinction was never the problem: /api/contribute/me
// already answers 401 for anonymous and, since #12111, a 200 with
// has_profile:false for signed-in-without-a-profile (it was a 403, which a
// hosted spoke's Ingress intercepted); TestContributeMeAnonymousRejected /
// TestContributeMeNoProfileIsAnEmptyState pin it. What changed here is the
// client, so these tests pin the client wiring in the same strings.Contains
// style the other Operations-page tests use.

// TestMineCardRendersSignedOutPrompt proves ccLoadMine branches on the two
// identity statuses instead of returning early on every non-2xx, and that each
// branch has a renderer that reveals the card.
func TestMineCardRendersSignedOutPrompt(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		"r.status===401",               // anonymous is branched on, not swallowed
		"ccRenderMineSignIn()",         // ...and routed to the sign-in prompt
		"r.status===403",               // signed in, no profile — pre-#12111 spokes
		"d.has_profile===false",        // ...and the 200 shape newer spokes answer
		"ccRenderMineNoProfile(",       // ...both routed to its own wording
		"function ccRenderMineSignIn(", // the renderers themselves exist
		"function ccRenderMineNoProfile(",
		"function ccRenderMineError(",
		"function ccRenderMineMessage(",
		`class="me-signin"`, // the Profile tab's class, reused rather than reinvented
		"Sign in with GitHub",
		"connect a contributor agent to get started",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered contribute page missing signed-out contribution-card marker %q", want)
		}
	}
}

// TestMineCardMessageRevealsTheCard proves the message path actually UNHIDES
// #cc-mine-card. The card is declared style="display:none", so a renderer that
// only wrote innerHTML would leave the regression exactly as it was.
//
// jsFunc (acmm_per_repo_ui_test.go) narrows each assertion to ONE function
// body; against the whole 8000-line document every marker below matches
// somewhere.
func TestMineCardMessageRevealsTheCard(t *testing.T) {
	body := renderContributePage(t)

	fn := jsFunc(t, body, "ccRenderMineMessage")
	for _, want := range []string{
		"card.style.display=''", // the reveal
		"is-message",            // the layout swap away from the tile grid
		"cc-mine-tier",          // stale tier chip cleared with the tiles it labelled
		"cc-mine-note",          // ...and the PR footnote about numbers not on screen
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("ccRenderMineMessage missing %q; body was:\n%s", want, fn)
		}
	}

	// Signing in mid-session must restore the tile grid, or the four tiles stack
	// in one 94px column under the message layout.
	if paint := jsFunc(t, body, "ccRenderMine"); !strings.Contains(paint, "classList.remove('is-message')") {
		t.Errorf("ccRenderMine does not undo the message layout; body was:\n%s", paint)
	}

	// The CSS that layout swap depends on has to ship with it.
	if !strings.Contains(body, ".cc-mine.is-message{display:block}") {
		t.Error("rendered contribute page missing the .cc-mine.is-message layout rule")
	}
}

// TestMineCardLoadFaultIsNotSilentAbsence is the distinction renderMeError draws
// on the Profile tab: a broken load must not render the same as "you have no
// numbers". Both the rejected-promise path and an unreadable 200 report it.
func TestMineCardLoadFaultIsNotSilentAbsence(t *testing.T) {
	body := renderContributePage(t)

	loader := jsFunc(t, body, "ccLoadMine")
	if strings.Count(loader, "ccRenderMineError()") < 2 {
		t.Errorf("ccLoadMine should report BOTH a transport fault and an unusable payload as errors; body was:\n%s", loader)
	}
	if !strings.Contains(jsFunc(t, body, "ccRenderMineError"), "This is a bug") {
		t.Error("ccRenderMineError does not say the failure is a bug rather than a missing account")
	}
}

// TestContributeMeStillGatesAnonymous restates the server contract these client
// branches are written against: the statuses they switch on are the real ones.
func TestContributeMeStillGatesAnonymous(t *testing.T) {
	s := meStatsServer(t)
	if rec := getAs(s, "/api/contribute/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/contribute/me = %d, want 401", rec.Code)
	}
	// Profile-less is NOT a gate: it is a readable 200 (#12111), so the only
	// non-2xx the client has to branch on for identity is the 401.
	if rec := getAs(s, "/api/contribute/me", "stranger"); rec.Code != http.StatusOK {
		t.Fatalf("profile-less GET /api/contribute/me = %d, want 200", rec.Code)
	}
}
