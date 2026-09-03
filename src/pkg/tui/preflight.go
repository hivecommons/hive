package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// preflightTimeout bounds the credential probe.
//
// It is deliberately shorter than the client's own 5s request timeout: this
// runs BEFORE the operator has a frame to look at, so every millisecond of it
// is a terminal sitting blank after they typed the command. Two seconds is long
// enough for a loopback dashboard and a hub across the internet, and short
// enough that a hive which accepts the connection and then says nothing does
// not read as a hung command.
const preflightTimeout = 2 * time.Second

// unauthorizedHelp is what an operator sees instead of a dashboard they cannot
// read.
//
// It names both credentials because the dashboard accepts either and which one
// this hive wants is a property of how it was deployed, which the TUI cannot
// see from out here. Naming only the token — the historical default — would
// send an operator of a hub-hosted or direct-route hive to regenerate a secret
// that endpoint will never accept no matter how correct it is.
const unauthorizedHelp = "the dashboard rejected these credentials (401)\n\n" +
	"Set one of:\n" +
	"  " + client.TokenEnv + "   shared dashboard token — self-hosted hives\n" +
	"  " + client.CookieEnv + "  session cookie header, e.g. \"hive_session=…\" —\n" +
	"                          hub-hosted hives and spokes with an authorized_users\n" +
	"                          allowlist, which do not accept the shared token\n\n" +
	"See src/docs/hivectl.md, \"Credentials\"."

// preflight refuses to start the TUI when the dashboard has already said the
// credentials are no good, and gets out of the way in every other case.
//
// ONLY 401 IS FATAL, and the asymmetry is the design. A 401 is a standing
// answer: it will be the same on the next tick and on every endpoint, the TUI
// has no keybinding that can fix it, and starting anyway produces four panes
// reading "waiting for data" forever — a screen that says the hive is silent
// when what happened is that the operator was not let in. Anything else is
// either transient or partial: a dashboard that is merely down comes back and
// the panes fill (the poll loop is built for exactly that), and a 403 means a
// working credential whose role is too narrow for some reads, which leaves most
// of the TUI useful and is already rendered per-pane.
//
// It follows that a hive which is DOWN at startup still opens the TUI, as it
// always has. That is not an oversight to fix later: refusing on an unreachable
// dashboard would make the TUI unopenable during the exact window an operator
// most wants it open.
func preflight(ctx context.Context, api *client.Client) error {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	if err := api.CheckCredentials(ctx); client.IsUnauthorized(err) {
		return fmt.Errorf("%s", unauthorizedHelp)
	}
	return nil
}
