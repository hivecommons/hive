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
		"repo-automerge-toggle",
		"repo-automerge-switch",
		"role=\"switch\"",
		"aria-checked=",
		"function toggleRepoAutoMerge(repo, enabled, btn)",
		"function maybeShowLevelAutoMergeActiveModal(data)",
		"Level 6 auto-merge is now active",
		"self_merge_sweep_active",
		"level_holds_pending",
		"level_changed_at",
		"hive:l6-automerge-info:",
		"PRs currently labelled <code>hold</code> stay held",
		"data-level-automerge-repo",
		"Switching to Level 6 turns auto-merge on for all active repositories.",
		"fetch('/api/repos/auto-merge'",
		"r.autoMerge !== false",
		"Requires Level 6",
		"data-action=\"toggleRepoAutoMerge\"",
		// #9070: enabling is owner-only in the UI, mirroring the server gate.
		"const canSetAutoMerge = autoMergeLevelOK && (autoMergeOn ? canPauseRepo : dashboardRoleAtLeast(window._hiveRole || 'read', 'owner'));",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("static dashboard missing %q", want)
		}
	}

	if strings.Contains(s, "toggleRepoAutoMerge") && (strings.Contains(s, "confirm(") || strings.Contains(s, "alert(")) {
		t.Fatal("repo auto-merge toggle must not use native browser dialogs")
	}
}

func TestTrustedAuthorAutoMergeSettingsStaticWiring(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static index: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"id=\"am-trusted-authors\"",
		"id=\"am-trusted-authors-enabled\"",
		"id=\"am-trusted-authors-require-role\"",
		"id=\"am-trusted-authors-require-github-permission\"",
		"id=\"am-trusted-authors-exclude-labels\"",
		"id=\"am-trusted-authors-repos\"",
		"function markDirtyTrustedAuthors()",
		"markDirty('auto-merge', 'trusted_authors'",
		"Fork PRs from non-members are blocked",
		"Leave every repo unselected to allow all watched repositories.",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("static dashboard missing %q", want)
		}
	}
	if strings.Contains(s, "am-trusted-authors") && (strings.Contains(s, "confirm(") || strings.Contains(s, "alert(")) {
		t.Fatal("trusted-author auto-merge controls must not use native browser dialogs")
	}
}
