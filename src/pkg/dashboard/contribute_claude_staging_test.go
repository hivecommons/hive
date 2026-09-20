package dashboard

import (
	"strings"
	"testing"
)

// contribute_claude_staging_test.go pins hivecommons/hive#7836.
//
// Container mode stages the contributor's Claude Code state into an ephemeral
// directory and mounts THAT at /home/dev/.claude (the H6 / CWE-668 boundary).
// It used to stage the whole of ~/.claude with `cp -a`. That directory is not
// just credentials: it holds every Claude Code transcript the contributor has
// ever had on the machine, the full prompt history, the paste cache, file
// history, plans, per-project memory and the private CLAUDE.md — 137 MB on the
// reporting host, for a container that needs one ~1 KB file. The agent inside
// runs third-party repositories' test suites for real (#4918), so any test
// fixture could read all of it.
//
// The recipe now stages an allowlist, file by file: .credentials.json (the
// OAuth token, exactly what the K8s path materializes for itself) and
// settings.json (the contributor's own Claude Code configuration). These tests
// pin the source; src/deploy/test_contributor_claude_staging.sh runs the real
// recipe against a populated fake ~/.claude and asserts what the container
// actually receives.

// contributeHiveClaudeStagingHelper returns the body of stage_claude_home, the
// allowlist helper the claude case stages through.
func contributeHiveClaudeStagingHelper(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	start := strings.Index(src, "stage_claude_home() {")
	if start < 0 {
		t.Fatal("the stage_claude_home helper was not found: contribute-hive has no allowlist for ~/.claude (#7836)")
	}
	end := strings.Index(src[start:], "\n      }")
	if end < 0 {
		t.Fatal("the end of the stage_claude_home helper was not found")
	}
	return src[start : start+end]
}

// TestClaudeStagingIsAnAllowlistNotTheDirectory is the invariant: the claude
// case must never hand the container ~/.claude as a directory. It fails on the
// pre-#7836 recipe, where stage_copy did exactly that.
func TestClaudeStagingIsAnAllowlistNotTheDirectory(t *testing.T) {
	block := contributeHiveClaudeStagingBlock(t)

	if strings.Contains(block, `stage_copy "${HOME}/.claude"`) {
		t.Error("the claude case stages the whole ~/.claude with stage_copy: every transcript, " +
			"the prompt history, paste cache, memory and CLAUDE.md reach the sandbox (#7836)")
	}
	if !strings.Contains(block, `stage_claude_home "${HOME}/.claude" "${CLI_STAGE}/.claude"`) {
		t.Error("the claude case must stage ~/.claude through stage_claude_home, the allowlist helper")
	}
	// The boundary itself is unchanged: the container still gets the staged
	// copy, never the host directory.
	if !strings.Contains(block, `-v ${CLI_STAGE}/.claude:/home/dev/.claude${VOLSUF}`) {
		t.Error("the staged copy must still be what is mounted at /home/dev/.claude")
	}
	if strings.Contains(block, `-v ${HOME}/.claude:`) {
		t.Error("the claude case bind-mounts the host's real ~/.claude; H6 requires the ephemeral staging copy")
	}
}

// TestClaudeStagingAllowlistIsCredentialAndSettingsOnly pins the list itself.
// Widening it is a security decision, not a convenience: each entry is a file
// a third-party test suite can read. Change this test deliberately, in the
// same PR, with the reason.
func TestClaudeStagingAllowlistIsCredentialAndSettingsOnly(t *testing.T) {
	src := justfileSource(t)
	const decl = `CLAUDE_STAGE_ALLOWLIST="`
	start := strings.Index(src, decl)
	if start < 0 {
		t.Fatal("CLAUDE_STAGE_ALLOWLIST is not declared: the claude staging allowlist has no single source of truth (#7836)")
	}
	rest := src[start+len(decl):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatal("CLAUDE_STAGE_ALLOWLIST's closing quote was not found")
	}
	got := strings.Fields(rest[:end])
	want := []string{".credentials.json", "settings.json"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("CLAUDE_STAGE_ALLOWLIST = %v, want %v: the container needs the OAuth token and the "+
			"contributor's own settings, nothing else in ~/.claude (#7836)", got, want)
	}
}

// TestClaudeStagingCopiesFilesNeverTrees guards the shape of the copy. An
// allowlist is only as narrow as its copy command: `cp -a` on an entry that
// turns out to be a directory (or a symlink to one) would drag a tree back in.
func TestClaudeStagingCopiesFilesNeverTrees(t *testing.T) {
	helper := contributeHiveClaudeStagingHelper(t)

	for _, tree := range []string{"cp -a", "cp -r", "cp -R", "rsync"} {
		if strings.Contains(helper, tree) {
			t.Errorf("stage_claude_home uses %q: an allowlist entry that is a directory would be staged whole", tree)
		}
	}
	if !strings.Contains(helper, `[ -f "${src}/${f}" ]`) {
		t.Error("stage_claude_home must copy regular files only (-f), so a directory named like an allowed file is skipped")
	}
	// Claude Code writes .credentials.json 0600 and the K8s path materializes
	// it 0600 (#5103); the staged copy must not loosen that.
	if !strings.Contains(helper, "cp -p") {
		t.Error("stage_claude_home must preserve the credential's mode (cp -p)")
	}
}

// TestClaudeStagingIsReportedToTheOperator: the exposure used to be silent —
// 137 MB copied per launch with nothing on screen. The helper names what it
// staged so an operator can see the boundary rather than trust it.
func TestClaudeStagingIsReportedToTheOperator(t *testing.T) {
	helper := contributeHiveClaudeStagingHelper(t)

	if !strings.Contains(helper, `echo "Staged:`) {
		t.Error("stage_claude_home must print what it staged, so the exposure is visible at launch rather than silent (#7836)")
	}
}
