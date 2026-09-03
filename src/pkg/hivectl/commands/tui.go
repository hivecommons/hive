package commands

import (
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/tui"
	tuiclient "github.com/hivecommons/hive/pkg/tui/client"
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
			exportCachedSessionForTUI()
			return tui.Run()
		},
	}
}

// exportCachedSessionForTUI hands a session cached by `hivectl login` (#5651)
// to the TUI through HIVE_DASHBOARD_COOKIE — the TUI's own session lane
// (#5645/#5649) — by setting the variable in this process when the operator
// has not.
//
// WHY THE ENV VAR AND NOT A PARAMETER. `hivectl tui` deliberately takes no
// flags and builds its client from environment variables alone (the epic's
// fixed Data source decision, documented above). Feeding the cache through the
// variable the TUI already reads keeps that contract intact and keeps this
// package out of pkg/tui's construction: precedence stays exactly the
// documented one — an exported HIVE_DASHBOARD_COOKIE always wins, the cache
// fills in only when the operator exported nothing.
//
// The cache is keyed by the URL the TUI will actually dial — HIVE_DASHBOARD_URL
// or its default — NOT hivectl's --server flag, which the TUI ignores by
// design. SessionKey folds the two defaults' localhost/127.0.0.1 spelling
// difference. Every failure here (no cache, unreadable cache, no entry)
// degrades to today's behaviour: the TUI starts with whatever credentials the
// environment carries, and its own preflight explains a rejection.
func exportCachedSessionForTUI() {
	if strings.TrimSpace(os.Getenv(hivectl.CookieEnv)) != "" {
		return
	}
	store, err := hivectl.DefaultSessionStore()
	if err != nil {
		return
	}
	base := strings.TrimSpace(os.Getenv(tuiclient.BaseURLEnv))
	if base == "" {
		base = tuiclient.DefaultBaseURL
	}
	// An expired session is still exported (Load returns it alongside
	// ErrSessionExpired): the server re-validates every request, so a stale
	// cookie costs nothing, and on a token-auth hive the token lane still
	// works regardless.
	sess, _ := store.Load(base)
	if sess == nil {
		return
	}
	_ = os.Setenv(hivectl.CookieEnv, sess.Cookie)
}
