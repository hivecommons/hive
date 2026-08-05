package dashboard

import (
	"strings"
	"testing"
)

// #2548 — branded client entry points (onboarding, additive). These tests pin the
// rendered /contribute page: per-client visual identity, first-class ordering for
// Claude/Copilot/Pi/Goose/LiteLLM/OpenRouter (they lead the tile grid; the visible
// "First-class" badge itself was removed per operator request — peer:true is now
// an ORDERING signal only, not a rendered pill), a documented-only "Open in"
// deep-link labeled as onboarding-not-contribution, and a full copy-pasteable
// customizable prompt — all without breaking the existing CLI/Mode/Runtime/OS
// selectors or the copy-block. renderContributePage lives in
// contribute_devlog_rail_test.go.

// TestBrandedEntryPoints_IdentityAndParity asserts the find-by-sight tile grid, the
// per-client inline SVG emblems (CSP-safe, no external images), and first-class
// ORDERING for Claude/Copilot/Pi/Goose/LiteLLM/OpenRouter are all present — and
// that the visible "First-class" pill is gone (removed per operator request; the
// tiles themselves — emblem + name + vendor subtitle — stay).
func TestBrandedEntryPoints_IdentityAndParity(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`id="client-tiles"`,   // the tile grid container
		`class="client-tiles`, // its styling hook
		`class="client-tile`,  // per-client tile
		`ct-emblem`,           // the inline emblem slot
		`var CLIENTS=`,        // the client metadata table
		`var EMB=`,            // the inline SVG emblem table
		`buildTiles`,          // tile renderer
	} {
		if !strings.Contains(body, want) {
			t.Errorf("branded picker missing %q", want)
		}
	}

	// The visible "First-class" pill/badge must be gone — operator asked for it
	// to be removed from the tiles entirely.
	if strings.Contains(body, "First-class") {
		t.Error("the \"First-class\" badge must be removed from the client tiles")
	}
	if strings.Contains(body, "ct-parity") {
		t.Error("the now-unused .ct-parity badge class must be removed, not left dangling")
	}

	// Ordering: each of the SIX first-class clients must still be marked
	// peer:true in the CLIENTS table so it leads the grid (tileOrder() puts
	// peers first) — this is an ordering signal only now, not a rendered badge.
	for _, peer := range []string{
		`claude:{name:'Claude Code'`, `copilot:{name:'GitHub Copilot'`, `pi:{name:'Pi'`,
		`goose:{name:'Goose'`, `litellm:{name:'LiteLLM'`, `openrouter:{name:'OpenRouter'`,
	} {
		if !strings.Contains(body, peer) {
			t.Errorf("missing first-class client entry %q", peer)
		}
	}
	if strings.Count(body, "peer:true") < 6 {
		t.Errorf("expected >=6 peer:true (Claude/Copilot/Pi/Goose/LiteLLM/OpenRouter lead the grid), got %d", strings.Count(body, "peer:true"))
	}

	// Emblems must be inline SVG (CSP-safe) — no external <img> smuggled into a tile.
	if !strings.Contains(body, `<svg viewBox="0 0 24 24"`) {
		t.Error("expected inline SVG emblems for client identity")
	}
	if strings.Contains(body, `ct-emblem"><img`) {
		t.Error("client emblem must be inline SVG, not an external image")
	}
}

// TestBrandedEntryPoints_DeepLinkLabeledOnboarding asserts the "Open in" affordance
// exists ONLY with a real documented scheme (Claude's claude:// desktop deep link)
// and is labeled unambiguously as onboarding/setup help — NOT as a contribution path.
func TestBrandedEntryPoints_DeepLinkLabeledOnboarding(t *testing.T) {
	body := renderContributePage(t)

	// The affordance markup + the only documented scheme we ship (Claude desktop).
	for _, want := range []string{
		`id="openin-row"`,
		`id="openin-link"`,
		`claude://claude.ai/new?q=`, // the documented scheme, not an invented one
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deep-link affordance missing %q", want)
		}
	}

	// It MUST be labeled onboarding-not-contribution: the copy has to say it opens a
	// chat in the vendor's app and does NOT connect the tool to this hive.
	if !strings.Contains(body, "onboarding help") {
		t.Error("deep-link must be labeled as onboarding help")
	}
	if !strings.Contains(body, "does NOT connect your tool to this hive") {
		t.Error("deep-link must state it does NOT connect the tool to the hive (not a contribution path)")
	}

	// Honesty: no invented vendor deep-link schemes for tools that don't document one.
	for _, forbidden := range []string{"goose://", "copilot://", "pi://"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("invented vendor deep-link scheme present: %q", forbidden)
		}
	}
}

// TestBrandedEntryPoints_CustomizablePrompt asserts the full, copy-pasteable prompt
// lives in an editable block (a <textarea>, read/edit) with its own copy button —
// NOT compressed into a URL — and is prefilled per client.
func TestBrandedEntryPoints_CustomizablePrompt(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`class="prompt-block"`,
		`id="prompt-text"`, // the editable textarea
		`<textarea id="prompt-text"`,
		`id="prompt-copy"`,      // its own copy affordance
		`defaultPromptFor`,      // per-client prompt generator
		`just contribute-setup`, // the real setup steps live in the prompt text
	} {
		if !strings.Contains(body, want) {
			t.Errorf("customizable prompt missing %q", want)
		}
	}
}

// TestBrandedEntryPoints_ExistingSelectorsIntact guards against regression: the
// branded additions must NOT break the OS/CLI/Mode/Runtime selectors or the shell
// copy block the onboarding flow already depends on.
func TestBrandedEntryPoints_ExistingSelectorsIntact(t *testing.T) {
	body := renderContributePage(t)
	for _, want := range []string{
		`id="os-select"`,
		`id="cli-select"`,
		`id="mode-select"`,
		`id="runtime-select"`,
		`id="copy-cmds"`,
		`id="copy-btn"`,
		`just contribute-hive`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("existing onboarding control regressed/missing: %q", want)
		}
	}
}
