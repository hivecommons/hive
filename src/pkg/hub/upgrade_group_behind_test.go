package hub

import (
	"strings"
	"testing"
)

// hivecommons/hive#12466: the Upgrade-state group-by showed an
// "Up to date (6)" header while every row underneath read "107–161 behind".
// The ” state (no rollout in flight, nothing queued) covers both genuinely
// current hives and hives far behind with auto-upgrade off; the header must
// not call the latter up to date.
func TestUpgradeGroupLabelSplitsBehindFromUpToDate(t *testing.T) {
	src := strings.Join([]string{
		jsFunc(t, "hiveUpgradeGroupLabel"),
		jsFunc(t, "_upgradeGroupOrder"),
		"var HIVE_UPGRADE_GROUP_QUEUED = 'Queued for auto-upgrade (not yet upgrading)';",
		"var HIVE_UPGRADE_GROUP_UPGRADING = 'Upgrading';",
		"var HIVE_UPGRADE_GROUP_BEHIND = 'Behind target (no upgrade pending)';",
		"var HIVE_UPGRADE_GROUP_UPTODATE = 'Up to date';",
		"var UPGRADE_GROUP_UNKNOWN_ORDER = Number.MAX_SAFE_INTEGER;",
		"var current = {id:'a', gitBranch:'v5', gitHash:'aaaaaaa', commitsBehindTarget: 0};",
		"var behindOff = {id:'b', gitBranch:'v5', gitHash:'bbbbbbb', commitsBehindTarget: 161, autoUpgrade: false};",
		"var behindStr = {id:'c', gitBranch:'v5', gitHash:'ccccccc', commitsBehindTarget: '107'};",
		"var queued = {id:'d', gitBranch:'v5', gitHash:'ddddddd', commitsBehindTarget: 17, autoUpgrade: true, upgradeTarget: 'eeeeeee'};",
		"var upgrading = {id:'e', gitBranch:'v5', gitHash:'eeeeeee', commitsBehindTarget: 3, behindTargetSHA: 'fffffff', upgrading: true};",
		"var labels = [current, behindOff, behindStr, queued, upgrading].map(hiveUpgradeGroupLabel);",
		"var sorted = labels.slice().sort(function(a, b) { return _upgradeGroupOrder(a) - _upgradeGroupOrder(b); });",
		"result = {labels: labels, sorted: sorted, unknownLast: _upgradeGroupOrder('bogus') > _upgradeGroupOrder(HIVE_UPGRADE_GROUP_UPTODATE)};",
	}, "\n")
	var got struct {
		Labels      []string `json:"labels"`
		Sorted      []string `json:"sorted"`
		UnknownLast bool     `json:"unknownLast"`
	}
	runUpgradeJS(t, src, &got)

	want := []string{
		"Up to date",
		"Behind target (no upgrade pending)",
		"Behind target (no upgrade pending)",
		"Queued for auto-upgrade (not yet upgrading)",
		"Upgrading",
	}
	if strings.Join(got.Labels, "|") != strings.Join(want, "|") {
		t.Fatalf("labels = %q, want %q", got.Labels, want)
	}
	wantSorted := []string{
		"Queued for auto-upgrade (not yet upgrading)",
		"Upgrading",
		"Behind target (no upgrade pending)",
		"Behind target (no upgrade pending)",
		"Up to date",
	}
	if strings.Join(got.Sorted, "|") != strings.Join(wantSorted, "|") {
		t.Fatalf("sorted = %q, want %q", got.Sorted, wantSorted)
	}
	if !got.UnknownLast {
		t.Fatal("an unexpected label must still sort after Up to date")
	}
}

// The grouping dimension must route through the named function so the
// label under test is the label the page renders.
func TestUpgradeGroupDimensionUsesNamedLabeler(t *testing.T) {
	if !strings.Contains(dashboardHTML, "return hiveUpgradeGroupLabel(h);") {
		t.Fatal("HIVE_GROUP_DIMENSIONS upgrade-state of() no longer calls hiveUpgradeGroupLabel")
	}
}
