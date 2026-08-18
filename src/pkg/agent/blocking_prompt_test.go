package agent

import "testing"

// codexUpdatePane is the real codex 0.146.0 update menu, verbatim. Note the
// PRE-SELECTED option ("›" marker) is "1. Update now" — the destructive one.
const codexUpdatePane = `  ✨ Update available! 0.146.0 -> 0.147.0
  Release notes: https://github.com/openai/codex/releases/latest
› 1. Update now (runs ` + "`npm install -g @openai/codex`" + `)
  2. Skip
  3. Skip until next version
  Press enter to continue`

const codexTrustPane = `> You are in /data/agents/architect
  Do you trust the contents of this directory? Working with untrusted contents
  comes with higher risk of prompt injection.
› 1. Yes, continue
  2. No, quit
  Press enter to continue`

const copilotTrustPane = `Confirm folder trust
  1. Yes
  2. Yes, and remember for future sessions
  3. No`

// TestBlockingPromptKey_CodexUpdateSkipsInsteadOfInstalling is the whole point
// of this mechanism.
//
// codex's update menu pre-selects "1. Update now", which runs
// `npm install -g @openai/codex` as the unprivileged agent UID. That fails with
// EACCES and kills the CLI — on EVERY launch, indefinitely. A bare Enter, or any
// heuristic that merely avoids options containing "no"/"exit", picks exactly
// that option. Only "3. Skip until next version" both unblocks startup and
// persists so the prompt does not return on the next launch.
func TestBlockingPromptKey_CodexUpdateSkipsInsteadOfInstalling(t *testing.T) {
	key, label, ok := blockingPromptKey("codex", codexUpdatePane)
	if !ok {
		t.Fatal("codex update prompt was not recognised; the agent would block on it until timeout")
	}
	if key == "1" {
		t.Fatal("answered \"1. Update now\" — this runs npm install -g as the agent UID and kills the CLI")
	}
	if key != "3" {
		t.Errorf("key = %q, want \"3\" (Skip until next version); %q does not persist across launches", key, key)
	}
	if label == "" {
		t.Error("label is empty; the audit log would not say which prompt was answered")
	}
}

func TestBlockingPromptKey_KnownPrompts(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		pane    string
		want    string
	}{
		{"codex directory trust", "codex", codexTrustPane, "1"},
		{"copilot folder trust", "copilot", copilotTrustPane, "2"},
		{"codex update", "codex", codexUpdatePane, "3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, _, ok := blockingPromptKey(c.backend, c.pane)
			if !ok {
				t.Fatalf("prompt not recognised")
			}
			if key != c.want {
				t.Errorf("key = %q, want %q", key, c.want)
			}
		})
	}
}

// TestBlockingPromptKey_IgnoresUnrelatedPanes: a prompt is only answered when
// positively identified. Firing a numbered keystroke at an arbitrary pane would
// type stray input into a working agent's prompt.
func TestBlockingPromptKey_IgnoresUnrelatedPanes(t *testing.T) {
	panes := []string{
		"",
		"? for shortcuts",
		"⏵⏵ bypass permissions on (shift+tab to cycle)",
		"› Implement {feature}\n  gpt-5.6-sol default · /data/agents/architect",
		// Superficially similar but NOT the update menu: no options rendered,
		// so there is nothing to answer.
		"Update available! 0.146.0 -> 0.147.0",
	}
	for _, p := range panes {
		if key, _, ok := blockingPromptKey("codex", p); ok {
			t.Errorf("pane %q was answered with %q; want no action", p, key)
		}
	}
}

// TestBackendHasBlockingPrompts_GateMatchesTable is the guard for the bug that
// made this whole mechanism inert: the watcher was gated on
// `backend == "copilot"`, so codex agents were never rescued from their update
// menu no matter how well the menu was understood. Deriving the gate from the
// table means a prompt added for a new backend enables the watcher for it.
func TestBackendHasBlockingPrompts_GateMatchesTable(t *testing.T) {
	for _, p := range blockingPrompts {
		if !backendHasBlockingPrompts(p.backend) {
			t.Errorf("backend %q has a blocking prompt but the watcher gate excludes it", p.backend)
		}
	}
	if !backendHasBlockingPrompts("codex") {
		t.Error("codex must run the watcher: its update menu kills the CLI on every launch")
	}
	if backendHasBlockingPrompts("claude") {
		t.Error("claude has no table entries; the watcher should not be started for it")
	}
}

// TestBlockingPromptKey_BackendScoped: a prompt must only be answered for the
// backend that renders it, so a stray keystroke is never typed into another
// CLI's pane that happens to contain similar words.
func TestBlockingPromptKey_BackendScoped(t *testing.T) {
	if _, _, ok := blockingPromptKey("claude", codexUpdatePane); ok {
		t.Error("codex update prompt matched while running claude")
	}
	if _, _, ok := blockingPromptKey("codex", copilotTrustPane); ok {
		t.Error("copilot trust prompt matched while running codex")
	}
}

// TestBlockingPromptKey_IgnoresScrollback is the guard for a bug seen live:
// captureTmuxPaneForAgent returns scrollback, so a dead CLI's update menu
// stayed in history and the watcher typed "3" into the bash shell that replaced
// it, on every poll, indefinitely. Only the tail of the pane may be matched.
func TestBlockingPromptKey_IgnoresScrollback(t *testing.T) {
	deadCLIPane := codexUpdatePane + `
npm error The operation was rejected by your operating system.
Error: ` + "`npm install -g @openai/codex`" + ` failed with status exit status: 243
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$
hive-ci-maintainer@pod:/data/agents/ci-maintainer$`
	if key, _, ok := blockingPromptKey("codex", deadCLIPane); ok {
		t.Errorf("answered %q against scrollback of a dead CLI; keystrokes would go to the shell", key)
	}
	// The same prompt, still live at the bottom, must still be answered.
	if _, _, ok := blockingPromptKey("codex", "some earlier output\n"+codexUpdatePane); !ok {
		t.Error("a live prompt at the tail was not recognised")
	}
}

const agyTrustPane = `      ▄▀▀▄        Antigravity CLI 1.1.13
     ▀▀▀▀▀▀       user@example.com (Google AI Pro)
   Accessing workspace:
   /data/agents/guide
   Do you trust the contents of this project?
   Antigravity CLI requires permission to read, edit, and execute files here.
 > Yes, I trust this folder
   No, exit
   ↑/↓ Navigate · enter Confirm`

// TestBlockingPromptKey_AgyProjectTrust: agy blocks startup on a project-trust
// menu whose affirmative option is ALREADY selected, so the answer is a bare
// Enter and the key must be empty — typing a digit there is stray input, not a
// selection. Without this entry an agent rotated onto agy hangs at startup,
// which matters now that automated rotation can move agents to Google.
func TestBlockingPromptKey_AgyProjectTrust(t *testing.T) {
	key, label, ok := blockingPromptKey("agy", agyTrustPane)
	if !ok {
		t.Fatal("agy project-trust prompt not recognised; a rotated agent would hang on it")
	}
	if key != "" {
		t.Errorf("key = %q, want \"\" (Enter alone; the affirmative option is preselected)", key)
	}
	if label == "" {
		t.Error("label is empty; the audit log would not say which prompt was answered")
	}
	if !backendHasBlockingPrompts("agy") {
		t.Error("agy has a table entry but the watcher gate excludes it")
	}
}

// The agy pane must not be answered while running another backend.
func TestBlockingPromptKey_AgyScoped(t *testing.T) {
	for _, b := range []string{"codex", "copilot", "claude"} {
		if _, _, ok := blockingPromptKey(b, agyTrustPane); ok {
			t.Errorf("agy trust prompt matched while running %q", b)
		}
	}
}
