package commands

import (
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

func newGovernorCommand(env *commandEnv) *cobra.Command {
	governor := &cobra.Command{
		Use:     "governor",
		Short:   "Inspect and update Hive governor configuration",
		Example: "  hivectl governor get --output yaml\n  hivectl governor thresholds-set --file thresholds.yaml",
	}
	governor.AddCommand(&cobra.Command{
		Use:     "get",
		Short:   "Show effective governor configuration",
		Example: "  hivectl governor get --output yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return env.do(cmd, http.MethodGet, "/api/config/governor", nil)
		},
	})
	governor.AddCommand(governorFileCommand(env, "thresholds-set", "/api/config/governor/thresholds", "Update governor queue thresholds"))
	governor.AddCommand(governorFileCommand(env, "budget-set", "/api/config/governor/budget", "Update governor token budget"))
	governor.AddCommand(governorFileCommand(env, "repos-set", "/api/config/governor/repos", "Update governed repositories"))
	governor.AddCommand(governorAgentAddCommand(env))
	governor.AddCommand(governorAgentRemoveCommand(env))
	return governor
}

func governorFileCommand(env *commandEnv, name, path, short string) *cobra.Command {
	var file string
	var stdin bool
	command := &cobra.Command{
		Use:   name,
		Short: short,
		Example: "  hivectl governor " + name + " --file config.yaml\n" +
			"  cat config.json | hivectl governor " + name + " --stdin",
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := readStructuredInput(env.in, file, stdin)
			if err != nil {
				return err
			}
			return env.do(cmd, http.MethodPut, path, body)
		},
	}
	command.Flags().StringVarP(&file, "file", "f", "", "JSON or YAML request file")
	command.Flags().BoolVar(&stdin, "stdin", false, "read JSON or YAML request from stdin")
	return command
}

func governorAgentAddCommand(env *commandEnv) *cobra.Command {
	var backend, model string
	command := &cobra.Command{
		Use:     "agent-add <name>",
		Short:   "Add a basic agent to governor configuration",
		Args:    cobra.ExactArgs(1),
		Example: "  hivectl governor agent-add ux-discovery --backend copilot --model claude-sonnet-4-6",
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.do(cmd, http.MethodPost, "/api/config/governor/agents", map[string]any{
				"name":    args[0],
				"backend": backend,
				"model":   model,
			})
		},
	}
	command.Flags().StringVar(&backend, "backend", "claude", "agent backend")
	command.Flags().StringVar(&model, "model", "", "agent model")
	return command
}

func governorAgentRemoveCommand(env *commandEnv) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:     "agent-remove <name>",
		Short:   "Remove an agent from governor configuration",
		Args:    cobra.ExactArgs(1),
		Example: "  hivectl governor agent-remove ux-discovery --yes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireYes(yes, "hivectl governor agent-remove "+args[0]+" --yes"); err != nil {
				return err
			}
			return env.do(cmd, http.MethodDelete, "/api/config/governor/agents/"+url.PathEscape(args[0]), nil)
		},
	}
	command.Flags().BoolVar(&yes, "yes", false, "confirm removal without an interactive prompt")
	return command
}
