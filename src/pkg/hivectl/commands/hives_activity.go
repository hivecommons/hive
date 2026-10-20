package commands

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

func newHivesActivityCommand(env *commandEnv) *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "activity",
		Short: "Show what this machine's relay has done, from local history",
		Args:  argsNone(),
		Long: "Summarize the contributor relay's local activity store (~/.config/hive/relay-activity.jsonl): " +
			"per-repository completed tasks, outcomes and last activity. Counts cover retained history (" +
			hivectl.RelayActivityRetention() + "), not lifetime totals. Completed means the local task finished, " +
			"not that a PR was approved or merged. --clear deletes the local history only; hive profiles, " +
			"consent and repository selection are unchanged.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validOutput(env.options.output) {
				return fmt.Errorf("invalid output format %q", env.options.output)
			}
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			if clear {
				if err := deps.store.ClearRelayActivity(); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Cleared local relay activity history (%s); hive profiles and consent unchanged.\n", deps.store.RelayActivityPath())
				return nil
			}
			records, skipped, err := deps.store.LoadRelayActivity()
			if errors.Is(err, hivectl.ErrRelayActivityUnavailable) {
				if env.options.output != "table" {
					return env.print(cmd, hivectl.RelayActivityRollup{
						Scope:        "retained history",
						Retention:    hivectl.RelayActivityRetention(),
						Outcomes:     map[string]int{},
						Repositories: []hivectl.RepositoryActivity{},
					})
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Activity: unavailable (retention: %s)\n", hivectl.RelayActivityRetention())
				return nil
			}
			if err != nil {
				return err
			}
			if skipped > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Skipped %d unreadable line(s) in %s.\n", skipped, deps.store.RelayActivityPath())
			}
			rollup := hivectl.RollupRelayActivity(records)
			if env.options.output != "table" {
				return env.print(cmd, rollup)
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Activity (%s: %s): %d completed of %d attempts%s\n",
				rollup.Scope, rollup.Retention, rollup.Completed, rollup.Attempts, lastActivitySuffix(rollup.LastActivity))
			for _, repo := range rollup.Repositories {
				_, _ = fmt.Fprintf(out, "%s\tcompleted=%d\t%s\tlast=%s\n",
					repo.Repository, repo.Completed, formatActivityOutcomes(repo.Outcomes), repo.LastActivity.UTC().Format(time.RFC3339))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "delete local activity history (does not change profiles or consent)")
	return cmd
}

func lastActivitySuffix(last time.Time) string {
	if last.IsZero() {
		return ""
	}
	return ", last " + last.UTC().Format(time.RFC3339)
}

func formatActivityOutcomes(outcomes map[string]int) string {
	keys := make([]string, 0, len(outcomes))
	for k := range outcomes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, outcomes[k]))
	}
	return strings.Join(parts, " ")
}
