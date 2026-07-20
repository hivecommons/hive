package commands

import (
	"context"
	"errors"
	"net/http"

	"github.com/spf13/cobra"
)

func newSystemCommand(env *commandEnv) *cobra.Command {
	system := &cobra.Command{
		Use:     "system",
		Short:   "Inspect Hive runtime state",
		Example: "  hivectl system status --output json\n  hivectl system events --follow --timeout 2m --output jsonl",
	}
	for _, item := range []struct {
		name string
		path string
		help string
	}{
		{name: "health", path: "/api/health", help: "Check the lightweight health endpoint"},
		{name: "status", path: "/api/status", help: "Show Hive runtime and agent status"},
		{name: "version", path: "/api/version", help: "Show the running Hive version"},
		{name: "snapshot", path: "/api/snapshot", help: "Fetch the public Hive snapshot"},
	} {
		item := item
		system.AddCommand(&cobra.Command{
			Use:     item.name,
			Short:   item.help,
			Args:    argsNone(),
			Example: "  hivectl system " + item.name + " --output json",
			RunE: func(cmd *cobra.Command, _ []string) error {
				return env.do(cmd, http.MethodGet, item.path, nil)
			},
		})
	}

	events := &cobra.Command{
		Use:     "events",
		Short:   "Stream Hive server-sent events as JSONL",
		Args:    argsNone(),
		Example: "  hivectl system events --follow --timeout 2m --output jsonl",
	}
	follow := false
	events.Flags().BoolVar(&follow, "follow", false, "confirm that the command should wait for streaming events")
	events.RunE = func(cmd *cobra.Command, _ []string) error {
		if !follow {
			return &usageError{message: "system events is a stream; pass --follow and set an appropriate --timeout\nExample: hivectl system events --follow --timeout 2m --output jsonl"}
		}
		if env.options.output != "jsonl" {
			return &usageError{message: "system events requires --output jsonl"}
		}
		client, err := env.client()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), env.options.timeout)
		defer cancel()
		err = client.StreamSSE(ctx, "/api/events", nil, cmd.OutOrStdout())
		// --timeout is an intentional end condition for streams, not a connection failure.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	system.AddCommand(events)
	return system
}
