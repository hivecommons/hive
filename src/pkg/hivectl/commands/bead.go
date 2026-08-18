package commands

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func newBeadCommand(env *commandEnv) *cobra.Command {
	bead := &cobra.Command{
		Use:     "bead",
		Aliases: []string{"beads"},
		Short:   "List, create, and reset Hive beads",
		Example: "  hivectl bead list --agent quality\n  hivectl bead create --agent quality --title \"Review finding\"",
	}
	bead.AddCommand(beadListCommand(env))
	bead.AddCommand(beadCreateCommand(env))
	bead.AddCommand(beadResetCommand(env))
	return bead
}

func beadListCommand(env *commandEnv) *cobra.Command {
	var agent string
	command := &cobra.Command{
		Use:     "list",
		Short:   "List beads for all agents or one agent",
		Args:    argsNone(),
		Example: "  hivectl bead list --agent quality --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := "/api/beads"
			if agent != "" {
				path += "/" + url.PathEscape(agent)
			}
			return env.do(cmd, http.MethodGet, path, nil)
		},
	}
	command.Flags().StringVar(&agent, "agent", "", "limit results to one agent")
	return command
}

func beadCreateCommand(env *commandEnv) *cobra.Command {
	var agent, title, beadType, externalRef string
	var priority int
	var metadata []string
	command := &cobra.Command{
		Use:   "create",
		Short: "Create a bead in an agent store",
		Args:  argsNone(),
		Example: `  hivectl bead create --agent quality --title "Review UX journey" --type advisory --priority 2
  hivectl bead create --agent quality --title "Visual regression" --external-ref visual-hive://repo/finding`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if agent == "" || strings.TrimSpace(title) == "" {
				return &usageError{message: "bead create requires --agent and --title"}
			}
			if priority < 0 || priority > 4 {
				return &usageError{message: "--priority must be between 0 and 4"}
			}
			validTypes := map[string]bool{
				"bug": true, "feature": true, "task": true, "epic": true,
				"chore": true, "decision": true, "advisory": true,
			}
			if !validTypes[beadType] {
				return &usageError{message: "--type must be one of bug, feature, task, epic, chore, decision, or advisory"}
			}
			if len(metadata) > 50 {
				return &usageError{message: "--metadata may be repeated at most 50 times"}
			}
			parsedMetadata := map[string]string{}
			for _, item := range metadata {
				key, value, ok := strings.Cut(item, "=")
				if !ok || strings.TrimSpace(key) == "" {
					return &usageError{message: "--metadata values must use key=value"}
				}
				key = strings.TrimSpace(key)
				if len(key) > 100 || len(value) > 1000 {
					return &usageError{message: "metadata keys must be at most 100 characters and values at most 1000 characters"}
				}
				parsedMetadata[key] = value
			}
			return env.do(cmd, http.MethodPost, "/api/beads/"+url.PathEscape(agent), map[string]any{
				"title":        title,
				"type":         beadType,
				"priority":     priority,
				"external_ref": externalRef,
				"metadata":     parsedMetadata,
			})
		},
	}
	command.Flags().StringVar(&agent, "agent", "", "target agent bead store")
	command.Flags().StringVar(&title, "title", "", "bead title")
	command.Flags().StringVar(&beadType, "type", "advisory", "bead type")
	command.Flags().IntVar(&priority, "priority", 2, "priority from 0 (critical) to 4")
	command.Flags().StringVar(&externalRef, "external-ref", "", "stable external deduplication reference")
	command.Flags().StringSliceVar(&metadata, "metadata", nil, "metadata key=value; may be repeated")
	return command
}

func beadResetCommand(env *commandEnv) *cobra.Command {
	var agent, reason string
	var yes bool
	command := &cobra.Command{
		Use:     "reset",
		Short:   "Close all open beads globally or for one agent",
		Args:    argsNone(),
		Example: "  hivectl bead reset --agent ux-discovery --reason \"replace stale scan\" --yes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			example := "hivectl bead reset --yes"
			if agent != "" {
				example = "hivectl bead reset --agent " + agent + " --yes"
			}
			if err := requireYes(yes, example); err != nil {
				return err
			}
			path := "/api/beads/reset"
			if agent != "" {
				path += "/" + url.PathEscape(agent)
			}
			return env.do(cmd, http.MethodPost, path, map[string]any{"reason": reason})
		},
	}
	command.Flags().StringVar(&agent, "agent", "", "reset only one agent bead store")
	command.Flags().StringVar(&reason, "reason", "manual reset via hivectl", "audit reason")
	command.Flags().BoolVar(&yes, "yes", false, "confirm reset without an interactive prompt")
	return command
}
