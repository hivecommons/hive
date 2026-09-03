package commands

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// newLoginCommand implements `hivectl login` (#5651): run the dashboard's
// existing GitHub device flow from the terminal and cache the minted per-user
// session, so hives that only accept session auth — hub-hosted ones, and
// spokes with an authorized_users allowlist — no longer require copying a
// cookie out of a browser's devtools.
//
// The cached value is a credential equivalent to a logged-in browser session.
// It is written 0600 and NEVER printed: success reports the username and where
// the cache lives, nothing more.
func newLoginCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Log in to the hive with GitHub and cache the session",
		Long: "Log in to the hive with the GitHub device flow and cache the resulting\n" +
			"per-user session, so every hivectl subcommand and the TUI can use it.\n\n" +
			"This is how you authenticate to a hive that does not accept the shared\n" +
			"dashboard token: hub-hosted hives, and spokes with an authorized_users\n" +
			"allowlist. The login proves identity only — it requests no OAuth scope —\n" +
			"and your role is resolved by the hive on every request, live.\n\n" +
			"The session is cached per dashboard URL with owner-only permissions. An\n" +
			"explicitly exported " + hivectl.CookieEnv + " always takes precedence\n" +
			"over the cache.",
		Args:    argsNone(),
		Example: "  hivectl login\n  hivectl --server https://hive.example.com login",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := env.client()
			if err != nil {
				return err
			}
			store, err := hivectl.DefaultSessionStore()
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			result, err := client.DeviceLogin(cmd.Context(), hivectl.DeviceLoginOptions{
				Prompt: func(p hivectl.DeviceLoginPrompt) {
					fmt.Fprintf(out, "First copy your one-time code: %s\n", p.UserCode)
					fmt.Fprintf(out, "Then open: %s\n", p.VerificationURI)
					fmt.Fprintln(out, "Waiting for GitHub authorization (ctrl+c cancels)...")
				},
			})
			if err != nil {
				return err
			}

			if err := store.Save(env.options.server, hivectl.Session{
				Cookie:     result.Cookie,
				Username:   result.Username,
				ObtainedAt: time.Now(),
				ExpiresAt:  result.ExpiresAt,
			}); err != nil {
				return fmt.Errorf("the login succeeded but the session could not be cached: %w", err)
			}

			fmt.Fprintf(out, "Logged in as %s.\n", result.Username)
			fmt.Fprintf(out, "Session cached at %s (owner-only). hivectl commands and the TUI will use it\n"+
				"automatically until %s; run 'hivectl logout' to end it.\n",
				store.Path(), result.ExpiresAt.Format(time.RFC3339))
			return nil
		},
	}
}

// newLogoutCommand implements `hivectl logout`: end the session server-side
// via the existing endpoint, then remove the cached credential.
//
// The cache is cleared even when the server call fails. The local credential
// is the part only this command can remove — the server-side session expires
// on its own — so an unreachable hive must not leave a revoked-in-intent
// cookie sitting on disk. The server failure is reported, not swallowed.
func newLogoutCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "logout",
		Short:   "Log out of the hive and remove the cached session",
		Args:    argsNone(),
		Example: "  hivectl logout",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := hivectl.DefaultSessionStore()
			if err != nil {
				return err
			}
			sess, loadErr := store.Load(env.options.server)
			if loadErr != nil && !errors.Is(loadErr, hivectl.ErrSessionExpired) {
				return loadErr
			}
			envCookie := strings.TrimSpace(os.Getenv(hivectl.CookieEnv))
			if sess == nil && envCookie == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "No session to log out: nothing cached for %s and %s is not set.\n",
					env.options.server, hivectl.CookieEnv)
				return nil
			}

			// env.client() already attached the same env-first, cache-second
			// credential this command just resolved, so the logout request
			// carries the session it is ending. An EXPIRED cached session is
			// still presented — the server scopes logout to the request's own
			// cookie and clearing a stale one server-side is exactly right.
			client, err := env.client()
			if err != nil {
				return err
			}
			serverErr := client.Logout(cmd.Context())

			out := cmd.OutOrStdout()
			if sess != nil {
				if _, err := store.Delete(env.options.server); err != nil {
					return fmt.Errorf("remove cached session: %w", err)
				}
				fmt.Fprintf(out, "Removed the cached session for %s.\n", env.options.server)
			}
			if serverErr != nil {
				fmt.Fprintf(out, "Note: the hive could not confirm the logout (%v).\n"+
					"The local credential is gone; the server-side session expires on its own.\n", serverErr)
				return nil
			}
			fmt.Fprintln(out, "Logged out.")
			return nil
		},
	}
}
