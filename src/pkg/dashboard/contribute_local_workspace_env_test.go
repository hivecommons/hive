package dashboard

import (
	"strings"
	"testing"
)

// kubestellar/hive#6707 — in LOCAL mode the agent's tmux pane did not inherit
// HIVE_WORKSPACE_DIR, so the hub's task prompt, which tells the agent to clone
// into `$HIVE_WORKSPACE_DIR/<owner>/<repo>`, expanded to `/<owner>/<repo>` —
// filesystem root — and `gh repo fork --clone` died with
//
//	fatal: could not create leading directories of '/projectbluefin/utah':
//	Read-only file system
//
// after the fork had already been created on GitHub.
//
// The recipe DOES export the variable, and that export reaches the pane only
// when `tmux new-session` has to START a server. Run `just contribute-hive`
// from inside tmux — a normal way to run a long-lived contributor — and
// new-session JOINS the running server instead, so the pane inherits that
// server's environment, which predates the export. The reporter's diagnostic
// was conclusive: the relay process had HIVE_WORKSPACE_DIR and the codex
// process it launched did not, while codex HAD inherited an unrelated HIVE_*
// variable from an older shell in the same terminal — i.e. the pane's
// environment came from the long-lived server, not from the recipe.
//
// The Justfile already solved this shape for the build-cache variables, and the
// comment there states the reason in as many words ("rather than relying on
// `export` reaching the pane, because the agent is started through
// `tmux send-keys` and a tmux server that is already running does not
// necessarily carry this shell's environment into a new session").
// HIVE_WORKSPACE_DIR was simply not on the list.
//
// These tests read the Justfile and contributor-agent.sh rather than restating
// them, so neither launch site can regress quietly — the same technique, and
// the same rationale, as contribute_pane_cwd_test.go.

// launchLineFor returns the line in block that assembles the command typed into
// the pane (the AGENT_LAUNCH_CMD export for local mode).
func launchLineFor(t *testing.T, block, needle, label string) string {
	t.Helper()
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("%s: no line containing %q found in the launch block", label, needle)
	return ""
}

// TestLocalModeLaunchCarriesWorkspaceDirInline is the regression guard. The
// variable must travel ON the launch line as a `VAR=value` prefix, because that
// is the only mechanism in this path that does not depend on which tmux server
// the pane happened to be forked from.
func TestLocalModeLaunchCarriesWorkspaceDirInline(t *testing.T) {
	block := contributeHiveLaunchBlock(t)

	if !strings.Contains(block, "HIVE_WORKSPACE_DIR=$(printf %q \"$HIVE_WORKSPACE_DIR\")") {
		t.Error("local mode must put HIVE_WORKSPACE_DIR on the launch line as a VAR=value prefix " +
			"(shell-quoted, like the build-cache variables beside it). Exporting it into the recipe's " +
			"own shell does NOT reach the pane when `tmux new-session` joins an already-running server, " +
			"and the task prompt's $HIVE_WORKSPACE_DIR then expands to nothing — cloning to filesystem " +
			"root (#6707)")
	}

	// It must be part of the string that is actually typed into the pane, not
	// merely present somewhere in the block. BUILD_CACHE_ENV is what
	// AGENT_LAUNCH_CMD interpolates.
	launch := launchLineFor(t, block, "export AGENT_LAUNCH_CMD=", "local mode")
	if !strings.Contains(launch, "BUILD_CACHE_ENV") {
		t.Errorf("the launch command no longer interpolates BUILD_CACHE_ENV, so the inline environment "+
			"(HIVE_WORKSPACE_DIR and the build caches) never reaches the CLI: %s", strings.TrimSpace(launch))
	}
}

// TestLocalModePaneCwdIsNotMistakenForEnvironment pins the distinction the bug
// turned on. `tmux new-session -c <dir>` sets the pane's working DIRECTORY; it
// does not set the pane's ENVIRONMENT. Both appear in this block and they look
// interchangeable at a glance, which is how the gap survived.
func TestLocalModePaneCwdIsNotMistakenForEnvironment(t *testing.T) {
	block := contributeHiveLaunchBlock(t)

	newSession := launchLineFor(t, block, "tmux new-session", "local mode")
	if !strings.Contains(newSession, "-c ") {
		t.Errorf("expected new-session to still set the pane working directory: %s", strings.TrimSpace(newSession))
	}

	// The substantive property: ignoring comments, HIVE_WORKSPACE_DIR must be
	// used somewhere OTHER than the `-c` flag. Counting raw occurrences would
	// not do — the pre-fix block already contained two, one of them a comment
	// and the other the `-c` itself, so a count-based assertion could never
	// fail and would only look like coverage.
	usedAsAssignment := false
	usedAsPaneCwd := false
	for _, line := range strings.Split(block, "\n") {
		code := line
		if i := strings.Index(code, "#"); i >= 0 {
			code = code[:i]
		}
		if !strings.Contains(code, "HIVE_WORKSPACE_DIR") {
			continue
		}
		if strings.Contains(code, "tmux new-session") {
			usedAsPaneCwd = true
			continue
		}
		if strings.Contains(code, "HIVE_WORKSPACE_DIR=") {
			usedAsAssignment = true
		}
	}
	if !usedAsPaneCwd {
		t.Error("expected the pane working directory to still come from HIVE_WORKSPACE_DIR")
	}
	if !usedAsAssignment {
		t.Error("HIVE_WORKSPACE_DIR is used only to set the pane's working directory. `-c` sets the " +
			"pane's DIRECTORY, not its ENVIRONMENT — the agent still has no variable for the task " +
			"prompt to expand, which is exactly #6707. It must also be assigned onto the launch line.")
	}
}

// TestContainerModeStillExportsWorkspaceDir is the negative control and the
// reason the fix is local-mode-only. Container mode was never affected:
// contributor-agent.sh exports the variable and then starts that container's
// OWN tmux server in the same process, so the pane inherits it. If that ever
// changes — an entrypoint refactor that reuses an outer server — this fails and
// says why, rather than reproducing #6707 one mode over.
func TestContainerModeStillExportsWorkspaceDir(t *testing.T) {
	src := fileSource(t, "bin/contributor-agent.sh")
	if !strings.Contains(src, "export HIVE_WORKSPACE_DIR=") {
		t.Error("contributor-agent.sh must export HIVE_WORKSPACE_DIR; container mode's pane inherits it " +
			"because the entrypoint starts the container's own tmux server in this same process (#6707)")
	}
}

// TestTaskPromptStillReferencesTheWorkspaceVariable ties the two ends of the
// contract together. The hub emits `$HIVE_WORKSPACE_DIR` literally into
// agent-facing text, and every launch path has to honour that. #6707 suggested
// a more robust alternative — interpolate the RESOLVED path into the prompt so
// no shell in the chain has to carry the variable. That is not a drop-in: the
// hub never learns the contributor's workspace path (the relay does not declare
// it in the handshake), so it would need a protocol addition and belongs in its
// own change.
//
// Until then this test is the tripwire: if the prompt stops using the variable,
// the launch-line guards above are guarding a contract that no longer exists,
// and whoever makes that change should delete or rewrite them deliberately.
func TestTaskPromptStillReferencesTheWorkspaceVariable(t *testing.T) {
	src := fileSource(t, "src/pkg/dashboard/contribute_ws.go")
	if !strings.Contains(src, "$HIVE_WORKSPACE_DIR") {
		t.Skip("the task prompt no longer references $HIVE_WORKSPACE_DIR — if it now interpolates a " +
			"resolved absolute path (#6707 suggestion 3), the launch-line guards in this file describe " +
			"a contract that is gone and should be revisited")
	}
}
