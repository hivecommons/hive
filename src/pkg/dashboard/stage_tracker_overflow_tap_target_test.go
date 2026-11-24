package dashboard

import (
	"strings"
	"testing"
)

// stageTrackerPhoneTier is the phone / coarse-pointer media query the
// activity monitor's touch rules live under (STAGE_TRACKER_SHEET_QUERY).
const stageTrackerPhoneTier = "@media (max-width: 640px), ((pointer: coarse) and (max-width: 820px)) {"

// phoneTierBlocks returns the body of every phone-tier media block in html.
func phoneTierBlocks(html string) []string {
	var out []string
	for rest := html; ; {
		i := strings.Index(rest, stageTrackerPhoneTier)
		if i < 0 {
			return out
		}
		rest = rest[i+len(stageTrackerPhoneTier):]
		end := strings.Index(rest, "\n    }\n")
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
	}
}

// TestStageTrackerOverflowButtonsTapTarget pins the phone tap targets of the
// dashboard's "…" / ⋯ overflow buttons. The human gate's … segment pads its
// hit area out with a ::before slop, but the pill's overflow: hidden clipped
// that slop, so on a phone the … took taps on a 44×16 strip only. On the
// phone tier the pill no longer clips, the slop reaches below the one-line
// pill (keeping the #17161 card height), the … sits 8px clear of the verb
// before it, the row ⋯ kebab sits 8px clear of the select box, the decision
// popover's "More ⋯" is tap-sized, and the sessions ⋯ meets the
// coarse-pointer floor. Desktop rules are unchanged.
func TestStageTrackerOverflowButtonsTapTarget(t *testing.T) {
	html := indexHTML(t)
	blocks := phoneTierBlocks(html)
	if len(blocks) == 0 {
		t.Fatalf("index.html has no %q block", stageTrackerPhoneTier)
	}
	inPhoneTier := func(rule string) bool {
		for _, b := range blocks {
			if strings.Contains(b, rule) {
				return true
			}
		}
		return false
	}
	for _, rule := range []string{
		// The gate pill no longer clips its segments' hit slop.
		"#activity-monitor-section .ham-human-gate { display: inline-flex; overflow: visible;",
		// Every gate segment, the … included, is at least tap-target wide and
		// sits above the strip line that paints after it.
		"#activity-monitor-section .ham-human-gate > button.ham-gate-status { position: relative; z-index: 1; display: inline-flex; align-items: center; justify-content: center; min-width: var(--tap-target);",
		// The slop makes each segment tap-target tall: --sp-1 above the pill
		// (clear of the title link), the rest below it.
		`#activity-monitor-section .ham-human-gate .ham-gate-more::before, #activity-monitor-section .ham-human-gate > button.ham-gate-status::before { content: ""; position: absolute; left: 0; right: 0; top: calc(-1 * var(--sp-1)); bottom: calc(100% + var(--sp-1) - var(--tap-target)); }`,
		// The … keeps 8px between itself and the verb before it.
		"#activity-monitor-section .ham-human-gate .ham-gate-more-wrap { display: inline-flex; align-items: stretch; margin-left: var(--sp-4); }",
		// Focus stays visible on the … and the kebab.
		"#activity-monitor-section .ham-human-gate .ham-gate-more:focus-visible, #activity-monitor-section .ham-kebab:focus-visible { outline: calc(var(--line-width) * 2) solid var(--accent);",
		// The row ⋯ kebab: tap-target square, 8px from the select box, and the
		// head keeps 8px clear of the tools.
		"#activity-monitor-section .ham-sel, #activity-monitor-section .ham-kebab { width: var(--tap-target); height: var(--tap-target); min-width: var(--tap-target); min-height: var(--tap-target);",
		"flex-direction: row; align-items: flex-start; gap: var(--sp-4); z-index: 1; }",
		"padding-right: calc(2 * var(--tap-target) + var(--sp-4) + var(--sp-4));",
		// The decision popover's "More ⋯" and its sibling verbs.
		".ham-decision-acts { gap: var(--sp-4); }",
		".ham-decision-act { min-height: var(--tap-target-touch); min-width: var(--tap-target-touch);",
		// The section remaps --tap-target to the 44px touch floor.
		"#activity-monitor-section { --tap-target: var(--tap-target-touch); }",
	} {
		if !inPhoneTier(rule) {
			t.Errorf("phone tier is missing overflow-button tap-target rule %q", rule)
		}
	}
	// The phone tier must not clip the gate again: overflow: hidden on the
	// pill is what shrank the … to its 16px painted line.
	for _, b := range blocks {
		if strings.Contains(b, "#activity-monitor-section .ham-human-gate { display: inline-flex; overflow: hidden") {
			t.Errorf("phone-tier gate pill must not clip its segments' hit slop")
		}
	}
	// The touch floor itself.
	if !strings.Contains(html, "--tap-target-touch: 44px;") {
		t.Errorf("--tap-target-touch must remain 44px, the coarse-pointer tap floor")
	}
	// The sessions ⋯ summary meets the floor on any coarse pointer.
	if !strings.Contains(html, "@media (pointer: coarse) { .sessions-menu > summary { display: inline-flex; align-items: center; justify-content: center; box-sizing: border-box; min-width: var(--tap-target-touch); min-height: var(--tap-target-touch);") {
		t.Errorf("sessions ⋯ menu summary is missing its coarse-pointer tap-target rule")
	}
	// Desktop density: the base (non-phone) gate pill still clips and the
	// kebab keeps its compact gutter size.
	if !strings.Contains(html, ".ham-human-gate { display: inline-flex; align-items: stretch; max-width: 100%; overflow: hidden;") {
		t.Errorf("desktop gate pill rule changed; the phone fix must stay inside the phone tier")
	}
	if !strings.Contains(html, ".ham-sel, .ham-kebab { appearance: none; -webkit-appearance: none; margin: 0; padding: 0; width: var(--ham-tool-size); height: var(--ham-tool-size);") {
		t.Errorf("desktop kebab rule changed; the phone fix must stay inside the phone tier")
	}
}
