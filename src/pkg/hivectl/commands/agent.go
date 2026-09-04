package commands

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hivecommons/hive/pkg/skillreg"
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
		Args:    argsNone(),
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
			Args:    argsExact(1),
			Example: "  hivectl agent " + action + " ux-discovery",
			RunE: func(cmd *cobra.Command, args []string) error {
				return env.do(cmd, http.MethodPost, "/api/"+action+"/"+url.PathEscape(args[0]), map[string]any{})
			},
		})
	}
	agent.AddCommand(agentLogsCommand(env))
	agent.AddCommand(agentDeleteCommand(env))
	agent.AddCommand(agentPromptCommand(env))
	agent.AddCommand(agentSpecsCommand(env))
	agent.AddCommand(agentModelSetCommand(env))
	agent.AddCommand(agentBackendSetCommand(env))
	agent.AddCommand(agentPipelineSetCommand(env))
	return agent
}

func agentSpecsCommand(env *commandEnv) *cobra.Command {
	dir := "/data/skills"
	specs := &cobra.Command{
		Use:     "specs",
		Short:   "List and search reusable agent specs and skills",
		Example: "  hivectl agent specs list --dir /data/skills\n  hivectl agent specs search go --limit 10",
	}
	specs.PersistentFlags().StringVar(&dir, "dir", dir, "skill registry directory")
	specs.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List registered reusable skills",
		Args:    argsNone(),
		Example: "  hivectl agent specs list --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, err := loadLocalSkillRegistry(dir)
			if err != nil {
				return err
			}
			return env.print(cmd, map[string]any{"items": skillViews(reg.List())})
		},
	})
	var limit int
	search := &cobra.Command{
		Use:     "search [query]",
		Short:   "Search registered reusable skills",
		Args:    argsMax(1),
		Example: "  hivectl agent specs search testing --limit 10 --output json",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return &usageError{message: "--limit cannot be negative"}
			}
			reg, err := loadLocalSkillRegistry(dir)
			if err != nil {
				return err
			}
			term := ""
			if len(args) == 1 {
				term = args[0]
			}
			items := skillViews(reg.Search(term))
			if limit > 0 && len(items) > limit {
				items = items[:limit]
			}
			return env.print(cmd, map[string]any{"items": items})
		},
	}
	search.Flags().IntVar(&limit, "limit", 0, "maximum results; zero returns all matches")
	specs.AddCommand(search)
	return specs
}

type skillView struct {
	Name        string   `json:"name" yaml:"name"`
	Version     string   `json:"version" yaml:"version"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Source      string   `json:"source" yaml:"source"`
	Tags        []string `json:"tags,omitempty" yaml:"tags,omitempty"`
}

func loadLocalSkillRegistry(dir string) (*skillreg.Registry, error) {
	reg := skillreg.NewRegistry()
	if _, err := reg.Load(dir, nil); err != nil {
		return nil, err
	}
	return reg, nil
}

func skillViews(skills []skillreg.Skill) []skillView {
	out := make([]skillView, 0, len(skills))
	for _, skill := range skills {
		out = append(out, skillView{
			Name:        skill.Name,
			Version:     skill.Version,
			Description: skill.Description,
			Source:      string(skill.Source),
			Tags:        skill.Tags,
		})
	}
	return out
}

func agentGetCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "get <name>",
		Short:   "Show effective agent configuration and status",
		Args:    argsExact(1),
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
		Args:  argsNone(),
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
		Args:    argsExact(1),
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
			// An exported AgentDefinition is portable YAML; a key/value table would
			// silently corrupt it, so table output emits the raw document verbatim.
			if env.options.output == "table" || env.options.output == "yaml" {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.TrimRight(yamlText, "\n"))
				return err
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
		Args:  argsExact(1),
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
		Args:    argsExact(1),
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

func agentPromptCommand(env *commandEnv) *cobra.Command {
	prompt := &cobra.Command{
		Use:     "prompt",
		Short:   "Read or update an agent kick prompt template",
		Example: "  hivectl agent prompt get quality --raw\n  hivectl agent prompt set quality --file quality.md",
	}

	var raw bool
	get := &cobra.Command{
		Use:     "get <name>",
		Short:   "Show the effective kick prompt template",
		Args:    argsExact(1),
		Example: "  hivectl agent prompt get quality --raw --output json",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := env.client()
			if err != nil {
				return err
			}
			var query url.Values
			if raw {
				query = url.Values{"raw": {"1"}}
			}
			result, err := client.Do(cmd.Context(), http.MethodGet, "/api/config/agent/"+url.PathEscape(args[0])+"/prompt", query, nil)
			if err != nil {
				return err
			}
			return env.print(cmd, result)
		},
	}
	get.Flags().BoolVar(&raw, "raw", false, "return the template without variable substitution")

	var file string
	var stdin bool
	set := &cobra.Command{
		Use:     "set <name>",
		Short:   "Replace the kick prompt template from a file or stdin",
		Args:    argsExact(1),
		Example: "  hivectl agent prompt set quality --file quality.md\n  cat quality.md | hivectl agent prompt set quality --stdin",
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readInput(env.in, file, stdin)
			if err != nil {
				return err
			}
			return env.do(cmd, http.MethodPut, "/api/config/agent/"+url.PathEscape(args[0])+"/prompt", map[string]any{"template": string(data)})
		},
	}
	set.Flags().StringVarP(&file, "file", "f", "", "prompt template file")
	set.Flags().BoolVar(&stdin, "stdin", false, "read the prompt template from stdin")

	prompt.AddCommand(get, set)
	return prompt
}

func agentModelSetCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "model-set <name> <model>",
		Short:   "Persist an agent's model in configuration",
		Args:    argsExact(2),
		Example: "  hivectl agent model-set quality claude-sonnet-4-6",
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.do(cmd, http.MethodPut, "/api/config/agent/"+url.PathEscape(args[0])+"/models", map[string]any{"model": args[1]})
		},
	}
}

func agentBackendSetCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "backend-set <name> <backend>",
		Short:   "Persist an agent's backend in configuration",
		Args:    argsExact(2),
		Example: "  hivectl agent backend-set quality claude",
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.do(cmd, http.MethodPut, "/api/config/agent/"+url.PathEscape(args[0])+"/models", map[string]any{"backend": args[1]})
		},
	}
}

func agentPipelineSetCommand(env *commandEnv) *cobra.Command {
	var file string
	var stdin bool
	command := &cobra.Command{
		Use:   "pipeline-set <name>",
		Short: "Set an agent's pipeline step toggles from JSON or YAML",
		Args:  argsExact(1),
		Example: `  hivectl agent pipeline-set quality --file pipeline.yaml
  cat pipeline.yaml | hivectl agent pipeline-set quality --stdin`,
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := readStructuredInput(env.in, file, stdin)
			if err != nil {
				return err
			}
			steps, ok := body.(map[string]any)
			if !ok {
				return &usageError{message: "pipeline input must be a map of step=true|false"}
			}
			for key, value := range steps {
				if _, ok := value.(bool); !ok {
					return &usageError{message: fmt.Sprintf("pipeline step %q must be a boolean", key)}
				}
			}
			return env.do(cmd, http.MethodPut, "/api/config/agent/"+url.PathEscape(args[0])+"/pipeline", body)
		},
	}
	command.Flags().StringVarP(&file, "file", "f", "", "pipeline toggles file (JSON or YAML)")
	command.Flags().BoolVar(&stdin, "stdin", false, "read pipeline toggles from stdin")
	return command
}

func agentDeleteCommand(env *commandEnv) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:     "delete <name>",
		Short:   "Delete a managed agent",
		Args:    argsExact(1),
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
