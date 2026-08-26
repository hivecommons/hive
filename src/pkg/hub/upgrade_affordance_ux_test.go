package hub

// Upgrade STATE vs upgrade ACTION (issue #4097). The old single "queued" pill
// WAS the click-to-upgrade affordance and real owners could not find it
// ("that line that says 'queued' is the upgrade button — it's not
// straightforward"). These tests pin the split:
//   - queued (behind + auto-upgrade ON):  passive "Queued for auto-upgrade"
//     badge PLUS an explicit "Upgrade now" action beside it;
//   - behind + auto-upgrade OFF (owner):  an obvious "↑ Upgrade available →
//     <sha>" button with a hover state;
//   - behind, non-owner:                  passive "Update available" badge
//     with no handler;
//   - the facet filter and grouping labels use the same wording, while the
//     internal state constants ('upgrading'/'queued') stay unchanged so no
//     hiveUpgradeState consumer breaks.

import (
	"strings"
	"testing"
)

// The queued state renders a passive badge (state) and a separate labelled
// "Upgrade now" affordance (action). If they fuse back into one clickable
// pill, the confusion this split exists to fix comes straight back.
func TestQueuedStateSplitsBadgeFromUpgradeNowAction(t *testing.T) {
	body := funcBody(t, "var buildRow = function(")
	for _, want := range []string{
		"'Queued for auto-upgrade · 1pm ET' : 'Queued for auto-upgrade'",
		// The badge is affordance-free: styled by the shared state-badge
		// constant, with no role/onclick of its own.
		`'<span title="' + escAttr(queuedTitle) + '" style="' + UPGRADE_STATE_BADGE_STYLE + '">' + esc(queuedLabel) + '</span>'`,
		// The action is its own element, keyboard-operable, wired to the SAME
		// upgradeHive call the old pill triggered.
		">Upgrade now</span>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queued rendering is missing %q; the state badge and the "+
				"Upgrade now action must be separate elements", want)
		}
	}
	// The tooltip must name the target, not just say "queued".
	if !strings.Contains(body, "'Auto-upgrade will apply ' + branchLatest + ' (' + versionLabel(versionSel) + ')") {
		t.Error("the queued badge tooltip no longer names the target SHA and " +
			"branch/channel; 'queued for what?' becomes unanswerable again")
	}
}

// The manual path (auto-upgrade OFF) is a real button that names the action
// AND the target, with a hover state — not a quiet underlined word.
func TestManualUpgradeIsAnObviousButtonNamingTheTarget(t *testing.T) {
	body := funcBody(t, "var buildRow = function(")
	for _, want := range []string{
		"↑ Upgrade available → ' + esc(latestShort)",
		`style="' + UPGRADE_BTN_STYLE + '"`,
		"onmouseover=\"this.style.background=\\'' + UPGRADE_BTN_HOVER_BG + '\\'\"",
		"onmouseout=\"this.style.background=\\'' + UPGRADE_BTN_BG + '\\'\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the manual upgrade affordance is missing %q; it must read "+
				"as an actionable button, not a passive label", want)
		}
	}
}

// Non-owners see the state ("Update available") but never an action: they
// cannot trigger the upgrade, so a button would be a lie.
func TestNonOwnersGetPassiveUpdateAvailableBadge(t *testing.T) {
	body := funcBody(t, "var buildRow = function(")
	idx := strings.Index(body, ">Update available</span>")
	if idx < 0 {
		t.Fatal("non-owner rows no longer show the passive 'Update available' " +
			"badge; a behind hive reads as current to everyone but the owner")
	}
	// The badge's own <span> (the segment from its opening tag to the label)
	// must carry no click handler.
	open := strings.LastIndex(body[:idx], "<span")
	if open < 0 {
		t.Fatal("could not locate the Update available span opening tag")
	}
	if strings.Contains(body[open:idx], "onclick") {
		t.Error("the non-owner 'Update available' badge has an onclick handler; " +
			"it must be a passive state badge")
	}
}

// The named style constants exist (no magic inline styles) and the two
// visual vocabularies stay distinct: buttons look clickable, badges do not.
func TestUpgradeAffordanceStyleConstantsAreNamed(t *testing.T) {
	for _, decl := range []string{
		"var UPGRADE_BTN_BG = ",
		"var UPGRADE_BTN_HOVER_BG = ",
		"var UPGRADE_BTN_STYLE = ",
		"var UPGRADE_STATE_BADGE_STYLE = ",
	} {
		if !strings.Contains(dashboardHTML, decl) {
			t.Errorf("missing named style constant: %s (no magic values in this file)", decl)
		}
	}
	if !strings.Contains(dashboardHTML, "var UPGRADE_BTN_STYLE = 'cursor:pointer;") {
		t.Error("UPGRADE_BTN_STYLE lost its pointer cursor; the button no longer signals clickability")
	}
	if strings.Contains(dashboardHTML, "var UPGRADE_STATE_BADGE_STYLE = 'cursor:pointer") {
		t.Error("UPGRADE_STATE_BADGE_STYLE has a pointer cursor; a state badge must never look clickable")
	}
}

// Facet filter and group-by wording matches the row badge, while the internal
// state values stay 'upgrading'/'queued' so every hiveUpgradeState consumer
// (filter, facet counter, grouping) keeps working unchanged.
func TestUpgradeFilterLabelsMatchNewWordingWithoutRenamingStates(t *testing.T) {
	for _, want := range []string{
		"var UPGRADE_FILTER_UPGRADING = 'upgrading';",
		"var UPGRADE_FILTER_QUEUED = 'queued';",
		"{key: UPGRADE_FILTER_QUEUED, label: 'Queued for auto-upgrade'}",
		"var HIVE_UPGRADE_GROUP_QUEUED = 'Queued for auto-upgrade (not yet upgrading)';",
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("missing %q; the display labels must follow the new wording "+
				"while the state constants stay stable", want)
		}
	}
}
