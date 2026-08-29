package dashboard

import (
	"strings"
	"testing"
)

// #2548 — branded client entry points (onboarding, additive). These tests pin the
// rendered /contribute page: per-client visual identity, first-class ordering for
// Claude/Codex/Copilot/Pi/Goose/LiteLLM/OpenRouter (they lead the tile grid; the visible
// "First-class" badge itself was removed per operator request — peer:true is now
// an ORDERING signal only, not a rendered pill), a documented-only "Open in"
// deep-link labeled as onboarding-not-contribution, and a full copy-pasteable
// customizable prompt — all without breaking the existing CLI/Mode/Runtime/OS
// selectors or the copy-block. renderContributePage lives in
// contribute_devlog_rail_test.go.

// TestBrandedEntryPoints_IdentityAndParity asserts the find-by-sight tile grid, the
// per-client inline SVG emblems (CSP-safe, no external images), and first-class
// ORDERING for Claude/Codex/Copilot/Pi/Goose/LiteLLM/OpenRouter are all present — and
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

	// Ordering: each of the SEVEN first-class clients must still be marked
	// peer:true in the CLIENTS table so it leads the grid (tileOrder() puts
	// peers first) — this is an ordering signal only now, not a rendered badge.
	for _, peer := range []string{
		`claude:{name:'Claude Code'`, `codex:{name:'Codex'`, `copilot:{name:'GitHub Copilot'`,
		`pi:{name:'Pi'`, `goose:{name:'Goose'`, `litellm:{name:'LiteLLM'`,
		`openrouter:{name:'OpenRouter'`,
	} {
		if !strings.Contains(body, peer) {
			t.Errorf("missing first-class client entry %q", peer)
		}
	}
	if strings.Count(body, "peer:true") < 7 {
		t.Errorf("expected >=7 peer:true (Claude/Codex/Copilot/Pi/Goose/LiteLLM/OpenRouter lead the grid), got %d", strings.Count(body, "peer:true"))
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
	for _, forbidden := range []string{"goose://", "copilot://", "pi://", "codex://"} {
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

// TestContributeCodexOnboardingSurface pins Codex as a first-class picker entry.
// Every layer below the page already supported it — `just contribute-setup codex`
// preflights the CLI + auth, the relay drives `codex exec` headlessly and passes
// AGENT_REASONING_EFFORT as -c model_reasoning_effort, K8S_HEADLESS_BACKENDS
// lists it, and Dockerfile.contributor pins @openai/codex — but the CLI select
// and the tile grid omitted it, so contributors had no path to pick it.
func TestContributeCodexOnboardingSurface(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		// The CLI select option, with the vendor's own install command.
		`<option value="codex"`,
		`npm i -g @openai/codex`,
		// Auth is Codex's device login or an API key — the same two modes the
		// Justfile preflight accepts.
		`codex login --device-auth`,
		// --model is honored (codex is not in the relay's NO_MODEL_FLAG_BACKENDS).
		`data-model-flag="--model" data-default-model="" data-env="# Optional: Codex reasoning effort`,
		// Tile metadata + emblem so it renders in the grid like its peers.
		`codex:{name:'Codex',tag:'OpenAI',peer:true}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("codex onboarding surface missing %q", want)
		}
	}

	// Codex must lead the grid next to Claude Code, not trail the non-peer
	// backends: peer:true plus an earlier position in the select (tileOrder
	// preserves select order within the peer group).
	iClaude := strings.Index(body, `<option value="claude"`)
	iCodex := strings.Index(body, `<option value="codex"`)
	iOther := strings.Index(body, `<option value="other"`)
	if iClaude < 0 || iCodex < 0 || iOther < 0 {
		t.Fatalf("missing select options: claude=%d codex=%d other=%d", iClaude, iCodex, iOther)
	}
	if iClaude >= iCodex || iCodex >= iOther {
		t.Errorf("codex must sit next to Claude Code in the CLI select: claude=%d codex=%d other=%d", iClaude, iCodex, iOther)
	}
}

// TestContributeAgySurface pins agy (Google's Antigravity CLI) as a picker
// entry honest about its real, narrower limit (#5048). agy is no longer
// forced to Host: src/Dockerfile.contributor now installs the binary, so
// Container is offerable like any other backend. What actually still applies
// to agy is that it has no OS-level sandbox of its own — Container mode is
// the only mode with any host boundary, and Local mode refuses to launch it
// without the explicit HIVE_AGY_DANGEROUSLY_RUN_UNCONFINED=1 escape hatch
// (#5024, untouched by this change). The page says exactly that, rather than
// the two inaccurate/unverified reasons ("no binary in the image" and "no
// inheritable credential") the old copy gave for forcing Host.
func TestContributeAgySurface(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		// The CLI select option, with the vendor's install path.
		`<option value="agy"`,
		`brew install --cask antigravity-cli`,
		// agy needs --effort whenever a model is set, or it ignores the model.
		`AGENT_REASONING_EFFORT=low`,
		// Tile metadata + emblem, tagged so the "no sandbox" constraint is
		// visible before a contributor commits to it.
		`agy:{name:'Antigravity',tag:'Google (unconfined)'}`,
		// agy's own confinement note (no OS sandbox; Container is the only
		// mode with a boundary) — separate from the generic host-only note.
		`id="agy-confinement-note"`,
		`no OS-level sandbox of its own`,
		`interactive Google OAuth flow`,
		`HIVE_AGY_DANGEROUSLY_RUN_UNCONFINED`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agy onboarding surface missing %q", want)
		}
	}

	// agy must NOT force Host anymore — "other" is the only entry left with
	// no image at all.
	if strings.Contains(body, `HOST_ONLY_BACKENDS=['other','agy']`) {
		t.Error("agy must not be in HOST_ONLY_BACKENDS — the contributor image now ships the agy binary (#5048)")
	}
	if !strings.Contains(body, `var HOST_ONLY_BACKENDS=['other']`) {
		t.Error("HOST_ONLY_BACKENDS must still cover 'other' — the entry that has no image by definition")
	}

	// The host-only fallback (now scoped to "other") must still switch the
	// visible mode selector itself, not just a local variable, or the
	// generated commands and the UI disagree.
	if !strings.Contains(body, `if(isHostOnly(cli)&&mode!=='host'){modeSel.value='host';mode='host';}`) {
		t.Error("host-only backends must flip the mode selector itself, not only the local mode")
	}

	// agy must NOT be advertised for Kubernetes: a pod cannot complete its
	// OAuth even once, so the k8s capability map stays without it (see
	// TestContributeK8sHeadlessCapability for the full enumeration).
	if strings.Contains(body, "K8S_HEADLESS_BACKENDS={claude:1,litellm:1,copilot:1,codex:1,watsonx:1,goose:1,agy:1") {
		t.Error("agy must stay out of K8S_HEADLESS_BACKENDS — a pod cannot complete agy's sign-in")
	}
}
