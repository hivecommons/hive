package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestBackendDefersStartupKick pins WHICH backends get their bootstrap prompt
// delivered by deliverStartupKick versus embedded in the launch command.
//
// bob and pi are the fixes: without deferral, their bootstrap prompts are
// written to /tmp/.hive-bootstrap-<name>.txt and never read, leaving them idle.
//
// The other rows are regression guards. goose in particular MUST stay false —
// `goose run` needs the embedded --text prompt to stay interactive and exits on
// the ^C that readiness-gated delivery sends.
func TestBackendDefersStartupKick(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		want    bool
		why     string
	}{
		{"bob defers (the fix)", bobBackend, true,
			"bob is an interactive TUI with no embedded-prompt mechanism"},
		{"pi defers (the fix)", "pi", true,
			"pi is an interactive TUI with no embedded-prompt mechanism"},
		{"claude unchanged", "claude", true,
			"pre-existing deferred backend"},
		{"copilot unchanged", "copilot", true,
			"pre-existing deferred backend"},
		{"gemini unchanged", "gemini", true,
			"pre-existing deferred backend"},
		{"goose still embeds", "goose", false,
			"goose run needs --text and exits on the ^C a deferred kick sends"},
		{"codex still embeds", codexBackend, false,
			"not a deferred backend; must not change"},
		{"aider still embeds", "aider", false,
			"not a deferred backend; must not change"},
		{"unknown backend does not defer", "totally-made-up", false,
			"no verified readiness signal for unknown CLIs"},
		{"empty backend does not defer", "", false,
			"guard against a zero-value backend silently deferring"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := backendDefersStartupKick(tc.backend); got != tc.want {
				t.Errorf("backendDefersStartupKick(%q) = %v, want %v (%s)",
					tc.backend, got, tc.want, tc.why)
			}
		})
	}
}

// TestBobPaneMarkers pins the literals copied from the installed bobshell
// bundle (1.0.6, bundle/bob.js). They are vendor-defined, not ours: if a bob
// upgrade changes them, bob's readiness detection breaks silently (its startup
// kick is dropped after cliReadyTimeout) and this test is the tripwire.
func TestBobPaneMarkers(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"input placeholder (primary readiness signal)", bobInputPlaceholder,
			"Type your message or @path/to/file"},
		{"product marker (coarse CLI presence only)", bobProductMarker, "Bob-Shell"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("marker = %q, want %q", tc.got, tc.want)
			}
		})
	}

	// Both markers must be reachable through the shared CLI-presence check,
	// otherwise waitForCLIReadyForAgent can never see a booted bob.
	for _, marker := range []string{bobInputPlaceholder, bobProductMarker} {
		if !paneHasCLIMarker("some pane text " + marker + " trailing") {
			t.Errorf("paneHasCLIMarker must recognise bob marker %q", marker)
		}
	}
}

// TestPaneShowsInputPrompt covers the readiness gate that decides whether a
// startup kick is typed or dropped.
//
// The claude/copilot/gemini/goose rows assert the pre-existing markers still
// match EXACTLY as before — this is the test that proves adding bob did not
// change the other backends' readiness behaviour.
func TestPaneShowsInputPrompt(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want bool
	}{
		// --- bob: the new behaviour ---
		{"bob idle input box", "  " + bobInputPlaceholder, true},
		{"bob placeholder amid other output", "booting\n  " + bobInputPlaceholder + "\n", true},

		// --- pre-existing markers: must be unchanged ---
		{"claude/copilot/gemini arrow prompt", "some output\n❯ ", true},
		{"goose ready banner", "goose is ready", true},
		{"enter-to-send prompt", "> Enter to send", true},
		{"bare newline-gt-newline prompt", "text\n>\nmore", true},

		// --- negatives ---
		{"empty pane", "", false},
		{"bare bash prompt", "user@host:~$ ", false},
		{"bob product name alone is NOT input-ready", bobProductMarker, false},
		{"bob license dialog is NOT input-ready",
			bobProductMarker + " API Key Notice\nBy using the " + bobProductMarker + " API", false},
		{"unrelated prose", "the build finished successfully", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsInputPrompt(tc.pane); got != tc.want {
				t.Errorf("paneShowsInputPrompt(%q) = %v, want %v", tc.pane, got, tc.want)
			}
		})
	}
}

// TestBobConsentScreenNoFalsePositive guards the OTHER half of the readiness
// chain. paneShowsConsentScreen makes waitForInputPromptForAgent skip a pane,
// and its markers are Claude-specific ("Enter to confirm", "Bypass Permissions
// mode"). Verified against bobshell 1.0.6: the bundle contains none of them, so
// a bob pane must never be mistaken for a consent screen — if it were, bob's
// kick would be dropped exactly as it was before this fix.
func TestBobConsentScreenNoFalsePositive(t *testing.T) {
	tests := []struct {
		name string
		pane string
	}{
		{"bob idle prompt", "  " + bobInputPlaceholder},
		{"bob banner", bobProductMarker + " 1.0.6"},
		{"bob trust dialog text",
			"Trusting a folder allows " + bobProductMarker + " to read and perform auto-edits"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if paneShowsConsentScreen(tc.pane) {
				t.Errorf("paneShowsConsentScreen(%q) = true; a bob pane must never "+
					"be treated as a consent screen or its startup kick is dropped", tc.pane)
			}
		})
	}
}

// TestBobInputHandlerSettleDelay pins the bob-only settle window and asserts it
// is genuinely distinct from the generic tmux pacing constants. bob's Ink
// reconciler paints the input box before attaching its stdin handler, so text
// typed too early is painted but never reaches component state.
func TestBobInputHandlerSettleDelay(t *testing.T) {
	if bobInputHandlerSettleDelay <= 0 {
		t.Fatalf("bobInputHandlerSettleDelay must be positive, got %v", bobInputHandlerSettleDelay)
	}
	// It must not silently collapse into a value that "happens to work" and is
	// really just tmux keystroke pacing.
	for _, other := range []struct {
		name string
		d    time.Duration
	}{
		{"textToEnterDelay", textToEnterDelay},
		{"staleCheckDelay", staleCheckDelay},
	} {
		if bobInputHandlerSettleDelay == other.d {
			t.Errorf("bobInputHandlerSettleDelay (%v) must be a deliberate, distinct "+
				"value, not a reuse of %s (%v)", bobInputHandlerSettleDelay, other.name, other.d)
		}
	}
	// The settle window must fit comfortably inside the gate that precedes it,
	// or a slow bob would blow the overall startup-kick budget.
	if bobInputHandlerSettleDelay >= inputPromptTimeout {
		t.Errorf("bobInputHandlerSettleDelay (%v) must be far below inputPromptTimeout (%v)",
			bobInputHandlerSettleDelay, inputPromptTimeout)
	}
}

// TestBobStartupKickNotWrittenToDeadTempFile documents that bob no longer takes
// the write-a-file-and-never-read-it path. Only backends that actually consume
// the file (goose, via --text "$(cat ...)") should be writing it.
//
// Asserted through the launch-command builder: bob's command must contain no
// reference to the bootstrap temp file and no embedded prompt argument.
func TestBobStartupKickNotWrittenToDeadTempFile(t *testing.T) {
	cmd := bobLaunchCmd("bob")
	for _, forbidden := range []string{".hive-bootstrap", "--text", "$(cat", "-p ", "--prompt"} {
		if strings.Contains(cmd, forbidden) {
			t.Errorf("bobLaunchCmd() = %q must not contain %q: bob's prompt is delivered "+
				"by deliverStartupKick, not embedded or read from a temp file", cmd, forbidden)
		}
	}
}

// TestDeliverStartupKick_BobReceivesKick is the end-to-end proof that the
// prompt actually reaches bob, rather than merely that a function was called.
//
// It drives the real deliverStartupKick against a real tmux pane rendering
// bob's real idle placeholder, and asserts the kick was delivered: LastKick
// set, the exact prompt text recorded, and history appended. Before this fix
// the pane would never satisfy waitForCLIReadyForAgent (bob renders none of
// the pre-existing markers) and the kick would be dropped.
func TestDeliverStartupKick_BobReceivesKick(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	const session = "hive-bobkick1"
	newRawTmuxSession(t, session)

	m := NewManager(map[string]config.AgentConfig{
		session: {Backend: bobBackend},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents[session]
	m.mu.RUnlock()
	if agent == nil {
		t.Fatalf("agent %q not registered", session)
	}
	agent.tmuxSession = session
	agent.State = StateRunning
	agent.launchGen = 1

	// Render EXACTLY what a booted, idle bob shows — no "❯", no goose banner.
	paneInject(t, session, bobInputPlaceholder)
	// Don't start the readiness clock until tmux has actually painted the
	// placeholder: on a loaded runner the render can outlast paneInject's fixed
	// sleep AND the TestMain-shrunk cliReadyTimeout, dropping the kick and
	// flaking this test for reasons unrelated to the delivery path under test.
	requirePaneShows(t, session, bobInputPlaceholder)

	const prompt = "bootstrap: go find work"
	m.deliverStartupKick(agent, prompt, 1)

	if agent.LastKick == nil {
		t.Fatal("bob must receive its startup kick; LastKick is nil " +
			"(the prompt was dropped, which is the bug this change fixes)")
	}
	if agent.LastKickMessage != prompt {
		t.Errorf("LastKickMessage = %q, want %q", agent.LastKickMessage, prompt)
	}
	if len(agent.KickHistory) == 0 {
		t.Error("bob's startup kick must be recorded in KickHistory")
	}
}

// TestDeliverStartupKick_PausedAgentNotKicked covers the paused-agent case.
//
// An agent restored as paused never reaches launchInTmux at all (Start returns
// early with State=StatePaused), so no startup kick is ever spawned. This test
// pins the second line of defence: even if a kick goroutine is somehow in
// flight, deliverStartupKick's state check must refuse to deliver to an agent
// that is not StateRunning.
func TestDeliverStartupKick_PausedAgentNotKicked(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}

	tests := []struct {
		name    string
		session string
		backend string
	}{
		{"paused bob is not kicked", "hive-bobpause1", bobBackend},
		{"paused claude is not kicked", "hive-bobpause2", "claude"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newRawTmuxSession(t, tc.session)
			name := tc.session
			m := NewManager(map[string]config.AgentConfig{
				name: {Backend: tc.backend},
			}, discardLogger(), ProjectContext{})

			m.mu.RLock()
			agent := m.agents[name]
			m.mu.RUnlock()
			if agent == nil {
				t.Fatalf("agent %q not registered", name)
			}
			agent.tmuxSession = tc.session
			agent.Paused = true
			agent.State = StatePaused
			agent.launchGen = 1

			// Render a ready prompt so the readiness gates pass fast and the
			// state check — not a timeout — is what stops delivery.
			paneInject(t, tc.session, "goose is ready")

			m.deliverStartupKick(agent, "bootstrap prompt", 1)

			if agent.LastKick != nil {
				t.Error("a paused agent must not receive a startup kick")
			}
			if len(agent.KickHistory) != 0 {
				t.Errorf("a paused agent must record no kick history, got %d entries",
					len(agent.KickHistory))
			}
		})
	}
}
