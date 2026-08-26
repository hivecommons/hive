package commands

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
)

const maxTrendHours = 720

func newObserveCommand(env *commandEnv) *cobra.Command {
	observe := &cobra.Command{
		Use:     "observe",
		Short:   "Inspect Hive token, cost, audit, and history data",
		Example: "  hivectl observe tokens --output json\n  hivectl observe audit\n  hivectl observe trends --range week",
	}

	for _, item := range []struct {
		name string
		path string
		help string
	}{
		{name: "tokens", path: "/api/tokens", help: "Show token usage summary"},
		{name: "audit", path: "/api/audit", help: "Show the recent audit log"},
		{name: "history", path: "/api/history", help: "Show governor evaluation history"},
		{name: "timeline", path: "/api/timeline", help: "Show the kick and mode timeline"},
	} {
		item := item
		observe.AddCommand(&cobra.Command{
			Use:     item.name,
			Short:   item.help,
			Args:    argsNone(),
			Example: "  hivectl observe " + item.name + " --output json",
			RunE: func(cmd *cobra.Command, _ []string) error {
				return env.do(cmd, http.MethodGet, item.path, nil)
			},
		})
	}

	observe.AddCommand(observeTrendsCommand(env))
	return observe
}

func observeTrendsCommand(env *commandEnv) *cobra.Command {
	var rangeParam string
	var hours int
	command := &cobra.Command{
		Use:     "trends",
		Short:   "Show governor and token trends over a time window",
		Args:    argsNone(),
		Example: "  hivectl observe trends --range week\n  hivectl observe trends --hours 12 --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if rangeParam != "" && rangeParam != "day" && rangeParam != "week" {
				return &usageError{message: "--range must be day or week"}
			}
			if hours != 0 && (hours < 1 || hours > maxTrendHours) {
				return &usageError{message: fmt.Sprintf("--hours must be between 1 and %d", maxTrendHours)}
			}
			if rangeParam != "" && hours > 0 {
				return &usageError{message: "use only one of --range or --hours"}
			}
			query := url.Values{}
			if rangeParam != "" {
				query.Set("range", rangeParam)
			}
			if hours > 0 {
				query.Set("hours", strconv.Itoa(hours))
			}
			client, err := env.client()
			if err != nil {
				return err
			}
			result, err := client.Do(cmd.Context(), http.MethodGet, "/api/trends", query, nil)
			if err != nil {
				return err
			}
			return env.print(cmd, result)
		},
	}
	command.Flags().StringVar(&rangeParam, "range", "", "preset window: day or week")
	command.Flags().IntVar(&hours, "hours", 0, fmt.Sprintf("custom window in hours (1-%d)", maxTrendHours))
	return command
}
