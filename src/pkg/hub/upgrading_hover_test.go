package hub

import (
	"strings"
	"testing"
)

// TestUpgradingStatusDotUsesHealthHover guards the dashboard regression where
// an upgrading hive bypassed healthBadge(), replacing the rich status/access
// hover panel with a bare animated blue dot title.
func TestUpgradingStatusDotUsesHealthHover(t *testing.T) {
	if strings.Contains(dashboardHTML, `? '<span class="online-dot upgrading" title="Upgrading \u2014 a rollout is in progress"></span>'`) {
		t.Fatal("upgrading hives still bypass healthBadge and lose the rich hover panel")
	}
	for _, snippet := range []string{
		`var isUpgrading = h.upgrading || _upgradingHives[h.id];`,
		`var dot = h.upgrading`,
		`? healthBadge(h)`,
		`? '<span class="online-dot upgrading" style="margin-right:0"></span>'`,
		`isUpgrading ? 'Upgrading — rollout in progress'`,
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Errorf("dashboardHTML missing upgrading hover snippet %q", snippet)
		}
	}
}
