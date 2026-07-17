package commands

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func newAgentCommand(env *commandEnv) *cobra.Command {
	agent := &cobra.Command{
		Use:     "agent",
		Short:   "Manage and operate Hive agents",
		Example: "  hivectl agent list\n  hivectl agent import --file agent.yaml --preview\n  hivectl agent kick quality",
	}
	agent.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List configured agents",
		Example: "  hivectl agent list --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return env.do(cmd, http.MethodGet, "/api/agents", nil)
		},
	})
	agent.AddCommand(agentGetCommand(env))
	agent.AddCommand(agentImportCommand(env))
	agent.AddCommand(agentExportCommand(env))
	agent.AddCommand(agentKickCommand(env))
	for _, action := range []string{"pause", "resume", "restart"} {
		action := action
		agent.AddCommand(&cobra.Command{
			Use:     action + " <name>",
			Short:   strings.ToUpper(action[:1]) + action[1:] + " an agent",
			Args:    cobra.ExactArgs(1),
			Example: "  hivectl agent " + action + " ux-discovery",
			RunE: func(cmd *cobra.Command, args []string) error {
				return env.do(cmd, http.MethodPost, "/api/"+action+"/"+url.PathEscape(args[0]), map[string]any{})
			},
		})
	}
	agent.AddCommand(agentLogsCommand(env))
	agent.AddCommand(agentDeleteCommand(env))
	return agent
}

func agentGetCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "get <name>",
		Short:   "Show effective agent configuration and status",
		Args:    cobra.ExactArgs(1),
		Example: "  hivectl agent get ux-discovery --output yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.do(cmd, http.MethodGet, "/api/config/agent/"+url.PathEscape(args[0]), nil)
		},
	}
}

func agentImportCommand(env *commandEnv) *cobra.Command {
	var file, sourceURL string
	var stdin, preview bool
	command := &cobra.Command{
		Use:   "import",
		Short: "Import or preview a portable AgentDefinition YAML",
		Example: `  hivectl agent import --file ux-discovery.yaml --preview
  cat ux-discovery.yaml | hivectl agent import --stdin`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body := map[string]any{"preview": preview}
			if sourceURL != "" {
				if file != "" || stdin {
					return &usageError{message: "--url cannot be combined with --file or --stdin"}
				}
				body["source"] = "url"
				body["url"] = sourceURL
			} else {
				data, err := readInput(env.in, file, stdin)
				if err != nil {
					return err
				}
				body["source"] = "paste"
				body["content"] = string(data)
			}
			return env.do(cmd, http.MethodPost, "/api/agents/import", body)
		},
	}
	command.Flags().StringVarP(&file, "file", "f", "", "AgentDefinition YAML file")
	command.Flags().BoolVar(&stdin, "stdin", false, "read AgentDefinition YAML from stdin")
	command.Flags().StringVar(&sourceURL, "url", "", "fetch AgentDefinition YAML from a URL")
	command.Flags().BoolVar(&preview, "preview", false, "parse and validate without importing")
	return command
}

func agentExportCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "export <name>",
		Short:   "Export a portable AgentDefinition",
		Args:    cobra.ExactArgs(1),
		Example: "  hivectl agent export ux-discovery --output yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := env.client()
			if err != nil {
				return err
			}
			result, err := client.Do(cmd.Context(), http.MethodGet, "/api/config/agent/"+url.PathEscape(args[0])+"/export", nil, nil)
			if err != nil {
				return err
			}
			response, ok := result.(map[string]any)
			if !ok {
				return fmt.Errorf("unexpected agent export response")
			}
			yamlText, ok := response["yaml"].(string)
			if !ok || strings.TrimSpace(yamlText) == "" {
				return fmt.Errorf("agent export response did not include YAML")
			}
			value, err := decodeStructured([]byte(yamlText))
			if err != nil {
				return err
			}
			return env.print(cmd, value)
		},
	}
}

func agentKickCommand(env *commandEnv) *cobra.Command {
	var prompt, promptFile string
	var stdin bool
	command := &cobra.Command{
		Use:   "kick <name>",
		Short: "Kick an agent with an optional prompt",
		Args:  cobra.ExactArgs(1),
		Example: `  hivectl agent kick ux-discovery
  hivectl agent kick ux-discovery --prompt "Scan changed UX surfaces"
  cat prompt.md | hivectl agent kick ux-discovery --stdin`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if prompt != "" && (promptFile != "" || stdin) {
				return &usageError{message: "use only one of --prompt, --prompt-file, or --stdin"}
			}
			if promptFile != "" || stdin {
				data, err := readInput(env.in, promptFile, stdin)
				if err != nil {
					return err
				}
				prompt = string(data)
			}
			return env.do(cmd, http.MethodPost, "/api/kick/"+url.PathEscape(args[0]), map[string]any{"prompt": prompt})
		},
	}
	command.Flags().StringVar(&prompt, "prompt", "", "kick prompt text")
	command.Flags().StringVar(&promptFile, "prompt-file", "", "read kick prompt from a file")
	command.Flags().BoolVar(&stdin, "stdin", false, "read kick prompt from stdin")
	return command
}

func agentLogsCommand(env *commandEnv) *cobra.Command {
	var lines int
	var source string
	command := &cobra.Command{
		Use:     "logs <name>",
		Short:   "Read a snapshot of recent agent output",
		Args:    cobra.ExactArgs(1),
		Example: "  hivectl agent logs ux-discovery --lines 200 --output json",
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 1 || lines > 1000 {
				return &usageError{message: "--lines must be between 1 and 1000"}
			}
			if source != "" && source != "buffer" {
				return &usageError{message: "--source must be empty or buffer"}
			}
			client, err := env.client()
			if err != nil {
				return err
			}
			query := url.Values{"lines": {fmt.Sprint(lines)}}
			if source != "" {
				query.Set("source", source)
			}
			result, err := client.Do(cmd.Context(), http.MethodGet, "/api/pane/"+url.PathEscape(args[0]), query, nil)
			if err != nil {
				return err
			}
			return env.print(cmd, result)
		},
	}
	command.Flags().IntVar(&lines, "lines", 100, "number of lines to fetch (1-1000)")
	command.Flags().StringVar(&source, "source", "", "output source; use buffer for the tmux buffer")
	return command
}

func agentDeleteCommand(env *commandEnv) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:     "delete <name>",
		Short:   "Delete a managed agent",
		Args:    cobra.ExactArgs(1),
		Example: "  hivectl agent delete ux-discovery --yes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireYes(yes, "hivectl agent delete "+args[0]+" --yes"); err != nil {
				return err
			}
			return env.do(cmd, http.MethodDelete, "/api/agents/"+url.PathEscape(args[0]), nil)
		},
	}
	command.Flags().BoolVar(&yes, "yes", false, "confirm deletion without an interactive prompt")
	return command
}
