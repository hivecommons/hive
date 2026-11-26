package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// githubTokenPrompt is written to stderr before a hidden interactive read.
const githubTokenPrompt = "GitHub personal access token (input hidden): "

// Terminal hooks for the hidden token read. Tests replace them to simulate a
// TTY; production code uses the real terminal.
var (
	// tokenTerminalFd reports the file descriptor of r when r is a terminal.
	tokenTerminalFd = func(r io.Reader) (uintptr, bool) {
		file, ok := r.(*os.File)
		if !ok || !term.IsTerminal(file.Fd()) {
			return 0, false
		}
		return file.Fd(), true
	}
	// readHiddenLine reads one line from the terminal with echo turned off.
	readHiddenLine = term.ReadPassword
)

// newGitHubCommand groups the GitHub credential commands. Today that is PAT
// rotation (hivecommons/hive#19633): the scripted twin of the dashboard's
// Settings -> Forge App field, over the owner-only /api/github-token routes.
func newGitHubCommand(env *commandEnv) *cobra.Command {
	github := &cobra.Command{
		Use:   "github",
		Short: "Manage the hive's GitHub credential",
		Example: "  hivectl github token status\n" +
			"  hivectl github token set --stdin",
	}
	token := &cobra.Command{
		Use:   "token",
		Short: "Show or rotate the GitHub personal access token (owner only)",
		Example: "  hivectl github token status\n" +
			"  hivectl github token set",
	}
	token.AddCommand(&cobra.Command{
		Use:     "status",
		Short:   "Show the token's fingerprint, login and last verification (never the token)",
		Args:    argsNone(),
		Example: "  hivectl github token status --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return env.do(cmd, http.MethodGet, "/api/github-token", nil)
		},
	})
	token.AddCommand(githubTokenSetCommand(env))
	github.AddCommand(token)
	return github
}

// argsNoSecret rejects positional arguments without echoing them. cobra.NoArgs
// (behind argsNone) embeds args[0] in its error, which would print a token
// passed by mistake to stderr, CI logs and terminal recordings.
func argsNoSecret(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return &usageError{message: "the token is never accepted as an argument; pipe it with --stdin or run the command in a terminal to be prompted"}
	}
	return nil
}

// flagErrorNoSecret replaces pflag's parse errors for `github token set`.
// Those errors quote the offending value ("invalid argument \"ghp_...\" for
// --stdin", "unknown flag: --ghp_..."), and on this command any value may be
// the token typed in the wrong place, so none of it is repeated.
func flagErrorNoSecret(_ *cobra.Command, _ error) error {
	return &usageError{message: "invalid flags for `hivectl github token set` (the value is not repeated in case it is a token); the only command flag is --stdin"}
}

// githubTokenSetCommand reads the new token from stdin or a hidden prompt,
// never from argv (where it would show in the process list and shell
// history) and never from a flag value. The server verifies it before saving;
// a refused token leaves the current one in place and exits non-zero.
func githubTokenSetCommand(env *commandEnv) *cobra.Command {
	var stdin bool
	command := &cobra.Command{
		Use:   "set",
		Short: "Rotate the GitHub personal access token without a restart",
		Args:  argsNoSecret,
		Example: "  pass show hive/github-pat | hivectl github token set --stdin\n" +
			"  hivectl github token set    # prompts without echo",
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := readGitHubToken(env.in, cmd.ErrOrStderr(), stdin)
			if err != nil {
				return err
			}
			token := strings.TrimSpace(string(data))
			if token == "" {
				return &usageError{message: "the token input was empty"}
			}
			if strings.ContainsAny(token, " \t\r\n") {
				return &usageError{message: "the token input must be a single token with no whitespace inside"}
			}
			return scrubToken(sendGitHubToken(cmd, env, token), token)
		},
	}
	command.Flags().BoolVar(&stdin, "stdin", false, "read the new token from stdin (hidden when stdin is a terminal)")
	command.SetFlagErrorFunc(flagErrorNoSecret)
	return command
}

// readGitHubToken reads the token from a terminal with echo off, whether or
// not --stdin was given, so a typed token never appears on screen. A
// non-terminal stdin is read only with --stdin, so a script that forgot the
// flag fails instead of blocking.
func readGitHubToken(in io.Reader, prompt io.Writer, stdin bool) ([]byte, error) {
	if fd, ok := tokenTerminalFd(in); ok {
		_, _ = io.WriteString(prompt, githubTokenPrompt)
		data, err := readHiddenLine(fd)
		_, _ = io.WriteString(prompt, "\n")
		if err != nil {
			return nil, fmt.Errorf("read the token from the terminal: %w", err)
		}
		return data, nil
	}
	if !stdin {
		return nil, &usageError{message: "pipe the token with --stdin, or run the command in a terminal to be prompted"}
	}
	return readLimited(in, maxAPIInputBytes)
}

// sendGitHubToken PUTs the token. It refuses plain http to anything but
// loopback before building the request, and the client never follows a
// redirect on a request with a body, so the token goes to the configured
// server or nowhere.
func sendGitHubToken(cmd *cobra.Command, env *commandEnv, token string) error {
	client, err := env.client()
	if err != nil {
		return err
	}
	if err := client.RequireSecureTransport(); err != nil {
		return &usageError{message: err.Error()}
	}
	result, err := client.Do(cmd.Context(), http.MethodPut, "/api/github-token", nil, map[string]string{"token": token})
	if err != nil {
		return err
	}
	// The server never echoes the token. If a misbehaving proxy or build ever
	// does, refuse to print the response rather than put it on stdout.
	if encoded, err := json.Marshal(result); err != nil || strings.Contains(string(encoded), token) {
		return errors.New("the server response could not be shown safely; check `hivectl github token status`")
	}
	return env.print(cmd, result)
}

// tokenScrubbedError hides the token if any error text ever carries it, for
// example a server or proxy that quotes the request body back. It keeps the
// wrapped error for errors.As so the exit code is unchanged.
type tokenScrubbedError struct {
	err   error
	token string
}

func (e *tokenScrubbedError) Error() string {
	return strings.ReplaceAll(e.err.Error(), e.token, redactedToken)
}

func (e *tokenScrubbedError) Unwrap() error { return e.err }

func scrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return &tokenScrubbedError{err: err, token: token}
}
