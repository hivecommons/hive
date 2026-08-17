package commands

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
)

func newKnowledgeCommand(env *commandEnv) *cobra.Command {
	knowledge := &cobra.Command{
		Use:     "knowledge",
		Aliases: []string{"wiki"},
		Short:   "Inspect and maintain Hive knowledge",
		Example: "  hivectl knowledge search \"user journey\"\n  hivectl knowledge list --layer project --output json",
	}
	for _, item := range []struct {
		name string
		path string
		help string
	}{
		{name: "health", path: "/api/knowledge/health", help: "Check knowledge engine health"},
		{name: "stats", path: "/api/knowledge/stats", help: "Show knowledge statistics"},
	} {
		item := item
		knowledge.AddCommand(&cobra.Command{
			Use:     item.name,
			Short:   item.help,
			Args:    argsNone(),
			Example: "  hivectl knowledge " + item.name + " --output json",
			RunE: func(cmd *cobra.Command, _ []string) error {
				return env.do(cmd, http.MethodGet, item.path, nil)
			},
		})
	}
	knowledge.AddCommand(knowledgeListCommand(env))
	knowledge.AddCommand(knowledgeSearchCommand(env))
	knowledge.AddCommand(knowledgeGetCommand(env))
	knowledge.AddCommand(knowledgeWriteCommand(env, "create"))
	knowledge.AddCommand(knowledgeWriteCommand(env, "update"))
	knowledge.AddCommand(knowledgeDeleteCommand(env))
	knowledge.AddCommand(knowledgeImportCommand(env))
	knowledge.AddCommand(knowledgeExportCommand(env))
	knowledge.AddCommand(knowledgeGraphCommand(env))
	return knowledge
}

func knowledgeListCommand(env *commandEnv) *cobra.Command {
	var layer, factType string
	command := &cobra.Command{
		Use:     "list",
		Short:   "List knowledge facts",
		Args:    argsNone(),
		Example: "  hivectl knowledge list --layer project --type regression --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if layer != "" && !validKnowledgeLayer(layer) {
				return &usageError{message: "--layer must be one of personal, project, org, or community"}
			}
			client, err := env.client()
			if err != nil {
				return err
			}
			path := "/api/knowledge"
			if layer != "" {
				path += "/" + url.PathEscape(layer)
			}
			query := url.Values{}
			if factType != "" {
				query.Set("type", factType)
			}
			result, err := client.Do(cmd.Context(), http.MethodGet, path, query, nil)
			if err != nil {
				return err
			}
			return env.print(cmd, result)
		},
	}
	command.Flags().StringVar(&layer, "layer", "", "knowledge layer such as project, org, personal, or community")
	command.Flags().StringVar(&factType, "type", "", "fact type filter")
	return command
}

func knowledgeSearchCommand(env *commandEnv) *cobra.Command {
	var tag, factType string
	var limit int
	command := &cobra.Command{
		Use:   "search [query]",
		Short: "Search knowledge facts by text or tag",
		Args:  argsMax(1),
		Example: `  hivectl knowledge search "user journey" --limit 20
  hivectl knowledge search --tag ux --type pattern`,
		RunE: func(cmd *cobra.Command, args []string) error {
			queryText := ""
			if len(args) == 1 {
				queryText = args[0]
			}
			if queryText == "" && tag == "" {
				return &usageError{message: "knowledge search requires a query or --tag"}
			}
			if limit < 0 {
				return &usageError{message: "--limit cannot be negative"}
			}
			query := url.Values{}
			if queryText != "" {
				query.Set("q", queryText)
			}
			if tag != "" {
				query.Set("tag", tag)
			}
			if factType != "" {
				query.Set("type", factType)
			}
			if limit > 0 {
				query.Set("limit", fmtInt(limit))
			}
			client, err := env.client()
			if err != nil {
				return err
			}
			result, err := client.Do(cmd.Context(), http.MethodGet, "/api/knowledge/search", query, nil)
			if err != nil {
				return err
			}
			return env.print(cmd, result)
		},
	}
	command.Flags().StringVar(&tag, "tag", "", "exact tag filter")
	command.Flags().StringVar(&factType, "type", "", "fact type filter")
	command.Flags().IntVar(&limit, "limit", 0, "maximum results; zero uses the server default")
	return command
}

func knowledgeGetCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "get <layer> <slug>",
		Short:   "Read one knowledge fact",
		Args:    argsExact(2),
		Example: "  hivectl knowledge get project create-project",
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.do(cmd, http.MethodGet, "/api/knowledge/"+url.PathEscape(args[0])+"/"+url.PathEscape(args[1]), nil)
		},
	}
}

func knowledgeWriteCommand(env *commandEnv, operation string) *cobra.Command {
	var file string
	var stdin bool
	use := operation
	short := "Create a knowledge fact from JSON or YAML"
	args := argsNone()
	if operation == "update" {
		use = "update <layer> <slug>"
		short = "Update a knowledge fact from JSON or YAML"
		args = argsExact(2)
	}
	command := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  args,
		Example: "  hivectl knowledge " + operation + " --file fact.yaml\n" +
			"  cat fact.yaml | hivectl knowledge " + operation + " --stdin",
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := readStructuredInput(env.in, file, stdin)
			if err != nil {
				return err
			}
			path := "/api/knowledge/create"
			method := http.MethodPost
			if operation == "update" {
				path = "/api/knowledge/" + url.PathEscape(args[0]) + "/" + url.PathEscape(args[1])
				method = http.MethodPut
			}
			return env.do(cmd, method, path, body)
		},
	}
	command.Flags().StringVarP(&file, "file", "f", "", "JSON or YAML request file")
	command.Flags().BoolVar(&stdin, "stdin", false, "read JSON or YAML request from stdin")
	return command
}

func knowledgeDeleteCommand(env *commandEnv) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:     "delete <layer> <slug>",
		Short:   "Delete a knowledge fact",
		Args:    argsExact(2),
		Example: "  hivectl knowledge delete project create-project --yes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireYes(yes, "hivectl knowledge delete "+args[0]+" "+args[1]+" --yes"); err != nil {
				return err
			}
			return env.do(cmd, http.MethodDelete, "/api/knowledge/"+url.PathEscape(args[0])+"/"+url.PathEscape(args[1]), nil)
		},
	}
	command.Flags().BoolVar(&yes, "yes", false, "confirm deletion without an interactive prompt")
	return command
}

func knowledgeImportCommand(env *commandEnv) *cobra.Command {
	var file, format, layer string
	var stdin bool
	command := &cobra.Command{
		Use:     "import",
		Short:   "Import Markdown or structured knowledge content",
		Args:    argsNone(),
		Example: "  hivectl knowledge import --file ux-wiki.md --format markdown --layer project",
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := readInput(env.in, file, stdin)
			if err != nil {
				return err
			}
			return env.do(cmd, http.MethodPost, "/api/knowledge/import", map[string]any{
				"content": string(data),
				"format":  format,
				"layer":   layer,
			})
		},
	}
	command.Flags().StringVarP(&file, "file", "f", "", "content file")
	command.Flags().BoolVar(&stdin, "stdin", false, "read content from stdin")
	command.Flags().StringVar(&format, "format", "markdown", "server import format")
	command.Flags().StringVar(&layer, "layer", "project", "target knowledge layer")
	return command
}

func knowledgeExportCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "export",
		Short:   "Export all knowledge as Markdown",
		Args:    argsNone(),
		Example: "  hivectl knowledge export > hive-knowledge.md",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := env.client()
			if err != nil {
				return err
			}
			data, _, err := client.Raw(cmd.Context(), http.MethodGet, "/api/knowledge/export", nil, nil)
			if err != nil {
				return err
			}
			if env.options.output == "table" {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			return env.print(cmd, string(data))
		},
	}
}

func knowledgeGraphCommand(env *commandEnv) *cobra.Command {
	var root string
	var depth int
	command := &cobra.Command{
		Use:     "graph",
		Short:   "Read the knowledge graph",
		Args:    argsNone(),
		Example: "  hivectl knowledge graph --root create-project --depth 3 --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if depth < 1 {
				return &usageError{message: "--depth must be greater than zero"}
			}
			query := url.Values{"depth": {fmtInt(depth)}}
			if root != "" {
				query.Set("root", root)
			}
			client, err := env.client()
			if err != nil {
				return err
			}
			result, err := client.Do(cmd.Context(), http.MethodGet, "/api/knowledge/graph", query, nil)
			if err != nil {
				return err
			}
			return env.print(cmd, result)
		},
	}
	command.Flags().StringVar(&root, "root", "", "root fact slug")
	command.Flags().IntVar(&depth, "depth", 2, "graph traversal depth")
	return command
}

func fmtInt(value int) string {
	return strconv.Itoa(value)
}

// validKnowledgeLayer guards --layer so a value like "search" or "stats" cannot
// be routed to a reserved /api/knowledge/<verb> endpoint instead of a layer.
func validKnowledgeLayer(layer string) bool {
	switch layer {
	case "personal", "project", "org", "community":
		return true
	default:
		return false
	}
}
