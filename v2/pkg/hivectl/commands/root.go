package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/kubestellar/hive/v2/pkg/hivectl"
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
	noColor  bool
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
	root.PersistentFlags().StringVar(&options.server, "server", "http://127.0.0.1:3001", "Hive dashboard API base URL")
	root.PersistentFlags().StringVar(&options.tokenEnv, "token-env", "HIVE_DASHBOARD_TOKEN", "environment variable containing the dashboard token")
	root.PersistentFlags().DurationVar(&options.timeout, "timeout", 30*time.Second, "request or stream timeout")
	root.PersistentFlags().StringVarP(&options.output, "output", "o", "table", "output format: table, json, yaml, or jsonl")
	root.PersistentFlags().BoolVar(&options.noColor, "no-color", false, "disable color output")

	root.AddCommand(newSystemCommand(env))
	root.AddCommand(newAgentCommand(env))
	root.AddCommand(newKnowledgeCommand(env))
	root.AddCommand(newBeadCommand(env))
	root.AddCommand(newGovernorCommand(env))
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
	return hivectl.NewClient(e.options.server, token, e.options.timeout)
}

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
