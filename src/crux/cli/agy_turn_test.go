package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/config"
)

// `crux agy-turn` is the pane-side subprocess a `crux serve` manager types
// into headless agy panes (it spawns its own executable). It dispatches to
// agent.RunAgyTurn, runs locally even when CRUX_SERVER points at a server,
// and is refused over the control socket.
func TestAgyTurnSubcommand(t *testing.T) {
	h := newHarness(t)
	if code := h.run(agent.AgyTurnSubcommand, "--help"); code != ExitOK {
		t.Fatalf("agy-turn --help: code %d, stderr %q", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "-prompt-file") {
		t.Fatalf("agy-turn --help did not print agy-turn's flags:\n%s", h.stderr)
	}

	h = newHarness(t)
	if code := h.run(agent.AgyTurnSubcommand, "--no-such-flag"); code != 2 {
		t.Fatalf("agy-turn bad flag: code %d, stderr %q", code, h.stderr)
	}

	// A CRUX_SERVER in the environment must not send the subprocess over
	// the API: nothing listens there for it, and the pane would hang.
	t.Setenv(config.ServerEnvVar, "127.0.0.1:1")
	h = newHarness(t)
	if code := h.run(agent.AgyTurnSubcommand, "--help"); code != ExitOK {
		t.Fatalf("agy-turn with CRUX_SERVER: code %d, stderr %q", code, h.stderr)
	}

	h = newHarness(t)
	if code := h.env.dispatch(context.Background(), globalOptions{}, agent.AgyTurnSubcommand, nil, true); code != ExitUsage ||
		!strings.Contains(h.stderr.String(), "not available over the control socket") {
		t.Fatalf("agy-turn over the control socket: code %d, stderr %q", code, h.stderr)
	}
}
