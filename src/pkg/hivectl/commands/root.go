package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

const (
	ExitSuccess    = 0
	ExitFailure    = 1
	ExitUsage      = 2
	ExitAuth       = 3
	ExitConnection = 4
	ExitAPI        = 5
)

type rootOptions struct {
	server   string
	tokenEnv string
	timeout  time.Duration
	output   string
}

type commandEnv struct {
	options *rootOptions
	in      io.Reader
}

type usageError struct {
	message string
}

func (e *usageError) Error() string { return e.message }

func NewRootCommand(in io.Reader, out, errOut io.Writer) *cobra.Command {
	options := &rootOptions{}
	env := &commandEnv{options: options, in: in}
	root := &cobra.Command{
		Use:           "hivectl",
		Short:         "Non-interactive client for the Hive dashboard API",
		SilenceErrors: true,
		SilenceUsage:  true,
		Example: `  hivectl system status --output json
  hivectl agent list
  cat agent.yaml | hivectl agent import --stdin --preview`,
	}
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	// Flag parsing failures are user mistakes; surface them with the usage exit code.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{message: err.Error()}
	})
	root.PersistentFlags().StringVar(&options.server, "server", "http://127.0.0.1:3001", "Hive dashboard API base URL")
	root.PersistentFlags().StringVar(&options.tokenEnv, "token-env", "HIVE_DASHBOARD_TOKEN", "environment variable containing the dashboard token")
	root.PersistentFlags().DurationVar(&options.timeout, "timeout", 30*time.Second, "request or stream timeout")
	root.PersistentFlags().StringVarP(&options.output, "output", "o", "table", "output format: table, json, yaml, or jsonl")

	root.AddCommand(newSystemCommand(env))
	root.AddCommand(newAgentCommand(env))
	root.AddCommand(newKnowledgeCommand(env))
	root.AddCommand(newBeadCommand(env))
	root.AddCommand(newGovernorCommand(env))
	root.AddCommand(newObserveCommand(env))
	root.AddCommand(newEnrollCommand(env))
	root.AddCommand(newTUICommand(env))
	root.AddCommand(newLoginCommand(env))
	root.AddCommand(newLogoutCommand(env))
	return root
}

func (e *commandEnv) client() (*hivectl.Client, error) {
	if !validOutput(e.options.output) {
		return nil, &usageError{message: fmt.Sprintf("invalid --output %q; expected table, json, yaml, or jsonl", e.options.output)}
	}
	token := ""
	if e.options.tokenEnv != "" {
		token = os.Getenv(e.options.tokenEnv)
	}
	client, err := hivectl.NewClient(e.options.server, token, e.options.timeout)
	if err != nil {
		// Invalid --server or --timeout are usage mistakes, not runtime failures.
		return nil, &usageError{message: err.Error()}
	}
	e.attachSession(client)
	return client, nil
}

// attachSession wires the per-user session lane (#5651) onto every client the
// commands build: HIVE_DASHBOARD_COOKIE when exported, else the session
// `hivectl login` cached for this --server.
//
// The env var wins UNCONDITIONALLY over the cache — an operator who exported a
// credential did so deliberately, and a cache able to override it would make
// the exported value silently inert. The token lane is untouched either way:
// which lane a hive honours depends on how it was deployed, so both are
// presented and the server picks the one it implements.
//
// A cache that cannot be read degrades to "no cached session" HERE — a broken
// cache file must not brick every hivectl command, including the `hivectl
// login` that would fix it — and the failure is surfaced where it can be acted
// on: Load's error names the file and the remedy, and login/logout return it.
func (e *commandEnv) attachSession(client *hivectl.Client) {
	if cookie := strings.TrimSpace(os.Getenv(hivectl.CookieEnv)); cookie != "" {
		client.SetSessionCookie(cookie)
		return
	}
	store, err := hivectl.DefaultSessionStore()
	if err != nil {
		return
	}
	sess, err := store.Load(e.options.server)
	if sess == nil {
		return
	}
	// An expired session is still PRESENTED — the server re-validates every
	// request, so a stale cookie costs nothing and clock skew means "expired
	// here" is not proof of "expired there". What changes is the advice on a
	// 401: a cached session that stops working means "log in again", not
	// "your token is wrong", and without this hint the operator gets a bare
	// 401 that reads like the latter.
	client.SetSessionCookie(sess.Cookie)
	if errors.Is(err, hivectl.ErrSessionExpired) {
		client.SetLoginHint("Your cached hive session has expired — run 'hivectl login' to obtain a new one.")
		return
	}
	client.SetLoginHint("Your cached hive session was rejected — it may have been revoked or expired; run 'hivectl login' to obtain a new one.")
}

// wrapArgs adapts a cobra positional-args validator so violations report the
// usage exit code (2) instead of the generic failure code.
func wrapArgs(validator cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validator(cmd, args); err != nil {
			return &usageError{message: err.Error()}
		}
		return nil
	}
}

func argsNone() cobra.PositionalArgs       { return wrapArgs(cobra.NoArgs) }
func argsExact(n int) cobra.PositionalArgs { return wrapArgs(cobra.ExactArgs(n)) }
func argsMax(n int) cobra.PositionalArgs   { return wrapArgs(cobra.MaximumNArgs(n)) }

func (e *commandEnv) print(cmd *cobra.Command, value any) error {
	return printer{format: e.options.output, out: cmd.OutOrStdout()}.print(value)
}

func (e *commandEnv) do(cmd *cobra.Command, method, path string, body any) error {
	client, err := e.client()
	if err != nil {
		return err
	}
	result, err := client.Do(cmd.Context(), method, path, nil, body)
	if err != nil {
		return err
	}
	return e.print(cmd, result)
}

func validOutput(value string) bool {
	switch value {
	case "table", "json", "yaml", "jsonl":
		return true
	default:
		return false
	}
}

func ExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	var usage *usageError
	if errors.As(err, &usage) {
		return ExitUsage
	}
	var tooLarge *hivectl.RequestTooLargeError
	if errors.As(err, &tooLarge) {
		return ExitUsage
	}
	var apiErr *hivectl.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden {
			return ExitAuth
		}
		return ExitAPI
	}
	var connectionErr *hivectl.ConnectionError
	if errors.As(err, &connectionErr) || errors.Is(err, context.DeadlineExceeded) {
		return ExitConnection
	}
	return ExitFailure
}

func requireYes(yes bool, example string) error {
	if yes {
		return nil
	}
	return &usageError{message: "refusing destructive operation without --yes\nExample: " + example}
}
