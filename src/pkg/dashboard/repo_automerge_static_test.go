package dashboard

import (
	"os"
	"strings"
	"testing"
)

func TestRepoAutoMergeStaticWiring(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static index: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"repo-automerge-off-pill",
		"repo-automerge-btn",
		"function toggleRepoAutoMerge(repo, enabled, btn)",
		"function maybeShowLevelAutoMergeActiveModal(data)",
		"Level 6 auto-merge is now active",
		"self_merge_sweep_active",
		"level_holds_pending",
		"level_changed_at",
		"hive:l6-automerge-info:",
		"PRs currently labelled <code>hold</code> stay held",
		"data-level-automerge-repo",
		"Switch off any repository that should not participate in L6 auto-merge.",
		"fetch('/api/repos/auto-merge'",
		"r.autoMerge !== false",
		"data-action=\"toggleRepoAutoMerge\"",
		// #9070: enabling is owner-only in the UI, mirroring the server gate.
		"const canSetAutoMerge = autoMergeOn ? canPauseRepo : dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("static dashboard missing %q", want)
		}
	}
	if strings.Contains(s, "toggleRepoAutoMerge") && (strings.Contains(s, "window.confirm") || strings.Contains(s, "window.alert")) {
		t.Fatal("repo auto-merge toggle must not use native browser dialogs")
	}
}
