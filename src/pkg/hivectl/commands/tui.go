package commands

import (
	"github.com/hivecommons/hive/pkg/tui"
	"github.com/spf13/cobra"
)

// newTUICommand registers the full-screen terminal dashboard (#4907) on the
// existing hivectl root.
//
// WHY hivectl AND NOT THE `hive` BINARY. The epic asks for a subcommand of the
// existing binary "if the CLI entrypoint has subcommand structure". Of the two
// candidates in this module, only this one does: `hive` (cmd/hive) is the spoke
// DAEMON — it parses a single -config flag, takes a process-wide singleton
// flock, and then runs the agent manager, dashboard and heartbeat for the
// lifetime of the container. It has no subcommand dispatch to hang `tui` off,
// and bolting an interactive foreground program onto the daemon's startup path
// would mean an operator's TUI session competing with the flock that exists to
// stop a second hive process. hivectl is already the operator-facing cobra CLI
// for this API.
//
// It is also where the TUI's plumbing already lives: the persistent --server
// flag defaults to the same :3001 Node proxy the epic names as the TUI's data
// source, and --token-env defaults to HIVE_DASHBOARD_TOKEN, the token the epic
// names too. Later tasks in the epic that need a configured client get those
// for free here, rather than re-deriving them in a standalone main.
func newTUICommand(_ *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the full-screen terminal dashboard",
		Long: "Open the full-screen terminal dashboard: a keyboard-driven view of the\n" +
			"agent fleet, governor, token spend and event feed, over the same dashboard\n" +
			"API that hivectl's non-interactive subcommands use.\n\n" +
			"Requires a terminal. Press q or ctrl+c to exit.",
		Args:    argsNone(),
		Example: "  hivectl tui",
		RunE: func(_ *cobra.Command, _ []string) error {
			return tui.Run()
		},
	}
}
