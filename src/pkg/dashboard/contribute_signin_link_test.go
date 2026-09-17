package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// #7195: a visitor on a spoke arrives at /contribute anonymous — a session on
// the hub is not a session on the spoke's origin — and every signed-out prompt
// rendered "Sign in with GitHub" as bare <b> text. The page told them to do the
// one thing it gave them no way to do. These tests pin the prompt as a real
// control, because the previous version read correctly and was still unusable.

// The CTA helper must exist and must emit an anchor with a real href. A prompt
// that merely looks clickable is the bug being fixed, so assert the href too.
func TestContributeSignInCTAIsALink(t *testing.T) {
	body := renderContributePage(t)

	fn := jsFunc(t, body, "ccSignInCTA")
	for _, want := range []string{
		"<a ",
		`class="cc-signin-cta"`,
		`href="'+esc(ccSignInHref())+'"`, // the destination depends on the spoke's auth shape (#7453)
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("ccSignInCTA does not emit %q; it must be a real link, not styled text.\ngot:\n%s", want, fn)
		}
	}
	// On a self-hosted spoke the dashboard root IS the device-flow sign-in
	// page, so that is still where the link goes there.
	if href := jsFunc(t, body, "ccSignInHref"); !strings.Contains(href, "if(!hubProxied)return '/';") {
		t.Errorf("ccSignInHref no longer sends a self-hosted spoke's visitor to the dashboard root:\n%s", href)
	}
	if !strings.Contains(body, ".cc-signin-cta{") {
		t.Error("the .cc-signin-cta rule is missing, so the control would be indistinguishable from prose")
	}
}

// Each signed-out surface named in #7195 must route through the helper. Pinning
// the call sites is what stops one of them from silently regressing to <b>.
func TestEverySignedOutPromptUsesTheCTA(t *testing.T) {
	body := renderContributePage(t)

	for _, tc := range []struct{ fn, what string }{
		{"loadMeStanding", "the Rankings standing prompt"},
		{"renderMeSignIn", "the Profile tab prompt"},
		{"renderWork", "the Fleet work 'Mine' empty state"},
		{"ccRenderMineSignIn", "the Operations contribution-stats card"},
	} {
		src := jsFunc(t, body, tc.fn)
		if !strings.Contains(src, "ccSignInCTA(") {
			t.Errorf("%s (%s) does not use ccSignInCTA, so its prompt is not clickable:\n%s", tc.fn, tc.what, src)
		}
	}
}

// The specific regression: "Sign in" emphasised but inert. Bold is fine
// anywhere else on the page, so scope this to the sign-in wording only.
func TestNoInertBoldSignInPrompt(t *testing.T) {
	body := renderContributePage(t)

	inert := regexp.MustCompile(`<b>Sign in[^<]*</b>`)
	if m := inert.FindAllString(body, -1); len(m) > 0 {
		t.Errorf("signed-out prompts still render as inert bold text %q — use ccSignInCTA so the viewer can act on it", m)
	}
}

// The wording the earlier issues settled on must survive being wrapped in a
// link, so this fix cannot quietly undo #6945's or the Profile tab's phrasing.
func TestSignInPromptWordingSurvives(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		"Sign in with GitHub",
		"to see your own work here.",
		"to see your own contribution stats",
		"to see your personal contributor profile",
		"to see where you stand.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sign-in prompt wording %q was lost", want)
		}
	}
}
