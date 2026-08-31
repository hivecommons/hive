package dashboard

import (
	"strings"
	"testing"
)

// Model auto-heal picks the model it will PIN an agent to. Before #5338 it
// took available[0] — the first entry of whatever catalog discovery returned —
// on the unstated assumption that catalog membership proves the backend's CLI
// can run the model. It does not.
//
// For claude the catalog comes from api.anthropic.com/v1/models, which
// enumerates what the ACCOUNT may call over the HTTP API. That is a superset of
// what the bundled `claude` CLI can drive, and it is ordered newest-first, so a
// model the CLI lists but cannot build a request for lands exactly in the slot
// auto-heal assigned from. The reporting hive had its only write-capable agent
// pinned that way; every model call died inside the CLI's request builder, the
// agent produced nothing for days, and each boot re-applied the same model.
//
// These tests pin the guard at the selection point.

// TestHealTargetIsVettedNotFirstAvailable is the core assertion: the heal
// target must come from healTargetFor, never straight from available[0].
func TestHealTargetIsVettedNotFirstAvailable(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "const first = healTargetFor(backend, available);") {
		t.Error("auto-heal no longer routes its heal target through healTargetFor — it would pin agents to available[0], a model the backend's CLI may advertise but cannot run (#5338)")
	}
	if strings.Contains(html, "const first = available[0];") {
		t.Error("auto-heal still assigns available[0] directly as the heal target (#5338)")
	}
}

// TestClaudeHealTargetsArePreferredAliases pins WHICH targets are vetted for
// claude. The bare aliases resolve through the claude CLI's own current-model
// mapping, so the CLI can only ever land on something it can actually drive —
// unlike a concrete dated id lifted from the API catalog.
func TestClaudeHealTargetsArePreferredAliases(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "const MODEL_HEAL_PREFERRED_TARGETS = {") {
		t.Fatal("MODEL_HEAL_PREFERRED_TARGETS is gone — auto-heal has no vetted target set (#5338)")
	}
	if !strings.Contains(html, "claude: ['sonnet', 'opus', 'haiku'],") {
		t.Error("claude's vetted heal targets are not the bare CLI aliases — a concrete catalog id can be advertised by the CLI yet fail on every call (#5338)")
	}
}

// TestVettedHealTargetsAreAlwaysServed is the cross-file invariant that makes
// the claude targets usable: the aliases must be in every served claude list,
// live-discovered or static fallback, or healTargetFor would find no target and
// permanently suppress claude heals.
func TestVettedHealTargetsAreAlwaysServed(t *testing.T) {
	for _, alias := range []string{"opus", "sonnet", "haiku"} {
		if !contains(claudeAlwaysIncludeModels, alias) {
			t.Errorf("claudeAlwaysIncludeModels is missing %q, a vetted heal target — auto-heal for claude would never find a target and stop healing entirely (#5338)", alias)
		}
	}
}

// TestNoHealTargetSuppressesTheSwitch covers the fail-safe. When no vetted
// target is available the heal must be SKIPPED, not fired with an empty model:
// leaving a model that is merely missing from one catalog sample is strictly
// better than pinning an agent to one its CLI cannot run.
func TestNoHealTargetSuppressesTheSwitch(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "if (!first) {") {
		t.Error("auto-heal does not bail out when healTargetFor finds no vetted target — it would call switchModel with an empty model (#5338)")
	}
	// The bail-out must precede _confirmAbsent, or a heal that cannot fire
	// still burns the once-per-disappearance guard and the absence clock,
	// silently disqualifying the later heal that COULD have succeeded.
	bail := strings.Index(html, "no vetted heal target is available — keeping selection")
	confirm := strings.Index(html, "if (!_confirmAbsent(key, a.model)) return;")
	if bail < 0 || confirm < 0 {
		t.Fatal("could not locate the auto-heal bail-out and confirmation guards (#5338)")
	}
	if bail > confirm {
		t.Error("the no-target bail-out runs AFTER _confirmAbsent — a suppressed heal would consume the absence clock and the once-per-disappearance guard (#5338)")
	}
}

// TestHealTargetFallbackKeepsOtherBackendsUnchanged pins the blast radius.
// Backends with no entry in MODEL_HEAL_PREFERRED_TARGETS must keep the previous
// first-available behavior — this change narrows claude's target selection, it
// does not re-tune heal targets fleet-wide.
func TestHealTargetFallbackKeepsOtherBackendsUnchanged(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "if (!preferred) return available[0] || '';") {
		t.Error("healTargetFor no longer falls back to available[0] for unlisted backends — this change must not alter heal behavior for backends it did not vet (#5338)")
	}
}

// TestAutoHealStillExemptsAutoSentinels guards the load-bearing exemption this
// change sits next to. copilot's and bob's `auto` are never concrete ids and
// never in a catalog, so healing them would rewrite a valid selection — and for
// bob specifically, any concrete id makes every prompt die with "Cannot read
// properties of undefined (reading 'maxTokens')".
func TestAutoHealStillExemptsAutoSentinels(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "if (isAutoModelSentinel(backend, a.model)) return;") {
		t.Error("auto-heal lost its AUTO_MODEL_SENTINELS exemption — it would rewrite copilot/bob 'auto' to a concrete discovered id (bob crashes on any concrete model)")
	}
	if !strings.Contains(html, "const AUTO_MODEL_SENTINELS = { copilot: ['auto'], bob: ['auto'] };") {
		t.Error("AUTO_MODEL_SENTINELS no longer exempts both copilot and bob")
	}
}

// TestBobStaysSingleOptionAndModelless re-pins bob's contract from the model
// layer this change touches: exactly one offered value, and no --model on the
// launch path at all.
func TestBobStaysSingleOptionAndModelless(t *testing.T) {
	if len(bobStaticModels) != 1 || bobStaticModels[0] != bobAutoModel {
		t.Errorf("bobStaticModels = %v, want exactly [%q] — offering bob a concrete model breaks every prompt", bobStaticModels, bobAutoModel)
	}
	html := indexHTML(t)
	if !strings.Contains(html, "const SINGLE_OPTION_BACKENDS = ['bob'];") {
		t.Error("bob is no longer a single-option backend — the dropdown could offer a model bob cannot honor")
	}
}
