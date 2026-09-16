package dashboard

import (
	"strings"
	"testing"
)

// An agent can be dark for four independent reasons — it is disabled in config,
// an operator paused it, the governor has no cadence for it in the current
// mode, or its session is stopped. Three of those had a control; the first had
// none outside a buried config tab, and all four rendered as overlapping words
// in the same badge slot. These tests pin the two structural fixes: a master
// 0/1 switch for the enablement axis, and a cadence hold that routes to the
// cadence editor instead of impersonating a start button.

// TestPowerSwitchRendersAtEveryAgentSite pins the switch to the shared helper.
// The paused action learned this lesson the hard way (see
// TestPausedAgentCombinedStartResumePinned): when each render site spells its
// own affordance, they drift, and the drift is invisible until an operator
// clicks the wrong one. Card grid, detail panel and detail fast path must all
// build the switch here.
func TestPowerSwitchRendersAtEveryAgentSite(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "function agentPowerSwitchHtml(a) {") {
		t.Fatal("agentPowerSwitchHtml must exist as the single source of the 0/1 switch markup")
	}
	// One definition plus a call at each of the three render sites.
	if n := strings.Count(html, "agentPowerSwitchHtml(a"); n < 4 {
		t.Errorf("agentPowerSwitchHtml referenced %d times, want >= 4 (definition + card grid + detail + detail fast path)", n)
	}
	// The switch is only meaningful if it names which agent it acts on, and
	// setAgentEnabled finds every copy of a switch by that attribute to keep
	// them in sync after a click.
	if !strings.Contains(html, `data-action="toggleAgentEnabled" data-agent="${esc(a.name)}" data-enabled="${on ? 'true' : 'false'}"`) {
		t.Error("the switch must carry its agent and current state, or setAgentEnabled cannot target or invert it")
	}
	if !strings.Contains(html, "case 'toggleAgentEnabled': setAgentEnabled(el.dataset.agent, el.dataset.enabled !== 'true'); break;") {
		t.Error("the dispatcher must route toggleAgentEnabled to setAgentEnabled, inverting the rendered state")
	}
}

// TestPowerSwitchWritesOnlyTheEnabledField pins the partial update. The general
// config section also carries display name, description, launch command and
// model pins; a full-section PUT built from a stale client-side view would
// silently revert whatever an operator set elsewhere.
func TestPowerSwitchWritesOnlyTheEnabledField(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "JSON.stringify({ enabled: enable })") {
		t.Error("setAgentEnabled must PUT only `enabled`, so it cannot clobber the rest of the general section")
	}
	if !strings.Contains(html, "`/api/config/agent/${encodeURIComponent(agent)}/general`") {
		t.Error("setAgentEnabled must target the general config section, which owns the enabled flag")
	}
}

// TestCadenceHoldNeverRendersAsStart is the regression pin for a live
// inversion. The detail panel and its fast path rendered a cadence-held agent
// as '▶ start' wired to toggleAgent with data-paused="false". toggleAgent
// re-syncs the real pause flag and then picks `paused ? resume : pause`, so for
// an agent that was never paused the button labelled "start" POSTed
// /api/pause — it stopped the thing it offered to start, and the click
// succeeded, so nothing surfaced the contradiction.
func TestCadenceHoldNeverRendersAsStart(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "function cadenceOffAgentActionHtml(a) {") {
		t.Fatal("cadenceOffAgentActionHtml must exist as the single source of the cadence-hold action")
	}
	// A cadence hold has no start/stop endpoint — the only fix is to set an
	// interval — so every site routes to the Cadences tab.
	if !strings.Contains(html, `data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(a.name)}" data-tab="Cadences"`) {
		t.Error("the cadence-hold action must open the Cadences tab, the only place the hold can be cleared")
	}
	if n := strings.Count(html, "cadenceOffAgentActionHtml(a"); n < 4 {
		t.Errorf("cadenceOffAgentActionHtml referenced %d times, want >= 4 (definition + card grid + detail + detail fast path)", n)
	}
	// The exact shapes that made a cadence hold impersonate a start button.
	for _, banned := range []string{
		`>${isOff ? '▶ start' : '⏸ pause'}</button>`,
		`fastToggle.textContent = isOff ? '▶ start' : '⏸ pause';`,
	} {
		if strings.Contains(html, banned) {
			t.Errorf("a cadence-held agent must never render a start button wired to toggleAgent: found %q", banned)
		}
	}
}

// TestDisabledAgentOffersOnlyThePowerSwitch pins the gate. canToggle only ever
// excluded the supervisor, so a disabled agent still rendered a green '⏸ pause'
// — offering a temporary operator hold on an agent the governor already refuses
// to schedule. The click succeeded and changed nothing observable.
func TestDisabledAgentOffersOnlyThePowerSwitch(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "const toggleBtn = (canToggle && !agentIsDisabled(a)) ?") {
		t.Error("the card grid must suppress the scheduling toggle for a disabled agent")
	}
	if !strings.Contains(html, "${agentIsDisabled(a)\n            ? ''") {
		t.Error("the detail panel must suppress the scheduling toggle for a disabled agent")
	}
}

// TestOffIsNamedByMode pins the vocabulary fix. A bare "off" reads as master
// power, so an operator seeing it went looking for an on/off control and found
// none — the agent was enabled and unpaused the whole time, and the only lever
// was a per-mode interval. "no cadence in <mode>" names what is actually
// missing and leaves "disabled" free to mean the cross-mode enablement axis
// that the 0/1 switch controls.
func TestOffIsNamedByMode(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "function agentCadenceOffLabel() {") {
		t.Fatal("agentCadenceOffLabel must exist as the single source of the cadence-hold wording")
	}
	if !strings.Contains(html, "return mode ? `no cadence in ${mode}` : 'no cadence in this mode';") {
		t.Error("the cadence hold must name the governor mode holding the agent")
	}
	// `enabled` is a CROSS-MODE flag. Borrowing the word "disabled" for a
	// per-mode schedule gap re-creates the ambiguity this whole change removes,
	// so the cadence label must never say it.
	if strings.Contains(html, "`disabled in ${mode}`") {
		t.Error("the cadence label must not reuse \"disabled\" — that word belongs to the cross-mode enabled flag")
	}
	// The card's state line, the detail header and the interval/next-kick
	// fields all rendered a bare 'off'. The remaining `isOff ? 'off'` sites are
	// CSS CLASS ternaries (dotCls, cls) — those must keep the class name, so
	// this pins the four DISPLAY sites by their value expressions rather than
	// banning the substring outright.
	for _, banned := range []string{
		"isOff ? 'off' : esc(a.cadence || '—')",
		"isOff ? 'off' : esc(a.nextKick || '—')",
		"isOff ? 'off' : a.cadence}",
		"isOff ? 'off' : (a.nextKick || '—')",
		"isOff ? 'off' : isRunning ? 'working' : 'idle'",
	} {
		if strings.Contains(html, banned) {
			t.Errorf("a bare 'off' display label is ambiguous with the enablement axis — name the mode instead: found %q", banned)
		}
	}
	// The card's state line keeps a CSS class of 'off' (styling) while its TEXT
	// names the mode — the class and the label are deliberately different here.
	if !strings.Contains(html, "isOff ? agentCadenceOffLabel() : a.busy}") {
		t.Error("the card state line must render the mode-named label, not a bare 'off'")
	}
}

// TestCadenceDialogCarriesThePowerSwitch pins the switch into the cadence
// editor. An operator who disabled an agent and later came here to work out why
// it never kicks had no way to see that from the cadence grid — every mode
// showed a healthy interval.
func TestCadenceDialogCarriesThePowerSwitch(t *testing.T) {
	html := indexHTML(t)
	idx := strings.Index(html, "function renderAgentCadences(c) {")
	if idx < 0 {
		t.Fatal("renderAgentCadences not found")
	}
	end := strings.Index(html[idx:], "function renderAgentPipeline")
	if end < 0 {
		t.Fatal("could not bound renderAgentCadences")
	}
	body := html[idx : idx+end]
	if !strings.Contains(body, "pwr-switch") {
		t.Error("the cadence dialog must show the master power switch beside the per-mode intervals")
	}
	// Inside the dialog the switch commits through the dirty bag on Save,
	// unlike the card's immediate action — same markup, dialog semantics.
	if !strings.Contains(body, `data-action="toggleConfigSwitch" data-section="general" data-key="enabled"`) {
		t.Error("the dialog switch must save through the config dirty bag, not fire immediately")
	}
}

// TestPowerSwitchHasAccessibleSemantics — the switch is a span, not an input,
// so the role and state have to be declared explicitly or it is invisible to
// assistive tech and unreadable to anyone who cannot see the lever position.
func TestPowerSwitchHasAccessibleSemantics(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, `role="switch" aria-checked="${on ? 'true' : 'false'}"`) {
		t.Error("the card switch must expose role=switch and its checked state")
	}
	if !strings.Contains(html, ".pwr-switch.on .pwr-lever {") {
		t.Error("the lever's on-position styling must exist, or 0 and 1 look identical")
	}
	// Default (classless) is the 0 position so the same markup works with
	// toggleConfigSwitch, which toggles exactly the 'on' class.
	if !strings.Contains(html, "position: absolute; left: 2px; right: 2px; top: 16px;") {
		t.Error("the lever must default to the 0 position so toggleConfigSwitch can drive it with the 'on' class alone")
	}
}
