package commands

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// `hivectl hives hub` tracks hubs and shows the Hives they advertise. It is
// discovery only: it never registers with a Hive, writes a token, or changes
// any profile's membership.

type hubListRow struct {
	Name   string `json:"name" yaml:"name"`
	URL    string `json:"url" yaml:"url"`
	Follow string `json:"follow" yaml:"follow"`
}

type hubHiveRow struct {
	hivectl.HubHive `yaml:",inline"`
	Onboarded       bool   `json:"onboarded" yaml:"onboarded"`
	Profile         string `json:"profile,omitempty" yaml:"profile,omitempty"`
}

// hubDiscoveryClient sends no credentials; DiscoverHubHives disables redirects.
func hubDiscoveryClient(env *commandEnv) *http.Client {
	return &http.Client{Timeout: env.options.timeout}
}

func newHivesHubCommand(env *commandEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use: "hub",
		Example: `  hivectl hives hub add commons --url https://hub.example.org
  hivectl hives hub adopt commons --follow current
  hivectl hives hub follow commons --mode current-and-future-repos
  hivectl hives hub refresh commons
  hivectl hives hub list -o json
  hivectl hives hub show commons
  hivectl hives hub remove commons`,
		Short: "Track, enroll from, and refresh hubs",
		Long: "Track a hub in profiles.yml (hubs:), review and enroll the Hives it advertises, and " +
			"refresh that review later. Discovery never sends credentials; each new Hive registration " +
			"uses the same unauthenticated registration path as 'hivectl hives add'.",
	}
	cmd.AddCommand(newHivesHubAddCommand(env))
	cmd.AddCommand(newHivesHubAdoptCommand(env))
	cmd.AddCommand(newHivesHubFollowCommand(env))
	cmd.AddCommand(newHivesHubListCommand(env))
	cmd.AddCommand(newHivesHubRefreshCommand(env))
	cmd.AddCommand(newHivesHubShowCommand(env))
	cmd.AddCommand(newHivesHubRemoveCommand(env))
	return cmd
}

func newHivesHubAddCommand(env *commandEnv) *cobra.Command {
	var hubURL string
	cmd := &cobra.Command{
		Use:     "add <name> --url <hub-url>",
		Example: `  hivectl hives hub add commons --url https://hub.example.org`,
		Short:   "Track a hub for discovery (no enrollment)",
		Args:    argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if hubURL == "" {
				return &usageError{message: "--url is required"}
			}
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			set, err := loadProfiles(cmd, deps, true)
			if err != nil {
				return err
			}
			if err := set.AddHub(hivectl.HubEntry{Name: args[0], URL: hubURL, Follow: hivectl.HubFollowOff}); err != nil {
				return err
			}
			if err := commit(deps, set); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Tracking hub %q (%s). Run 'hivectl hives hub show %s' to see its Hives.\n", args[0], hubURL, args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&hubURL, "url", "", "hub URL whose GET /api/hives lists its Hives")
	return cmd
}

func newHivesHubListCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Example: `  hivectl hives hub list
  hivectl hives hub list -o yaml`,
		Short: "List tracked hubs",
		Args:  argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validOutput(env.options.output) {
				return fmt.Errorf("invalid output format %q", env.options.output)
			}
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			set, err := loadProfiles(cmd, deps, true)
			if err != nil {
				return err
			}
			rows := make([]hubListRow, 0, len(set.Hubs))
			for _, h := range set.Hubs {
				rows = append(rows, hubListRow{Name: h.Name, URL: h.URL, Follow: h.EffectiveFollow()})
			}
			if env.options.output != "table" {
				return env.print(cmd, rows)
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No hubs tracked. Add one with 'hivectl hives hub add <name> --url <hub-url>'.")
				return nil
			}
			for _, r := range rows {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tfollow=%s\n", r.Name, r.URL, r.Follow)
			}
			return nil
		},
	}
}

func newHivesHubShowCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "show <name>",
		Example: `  hivectl hives hub show commons -o json`,
		Short:   "Discover a tracked hub's Hives and mark which are onboarded",
		Args:    argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validOutput(env.options.output) {
				return fmt.Errorf("invalid output format %q", env.options.output)
			}
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			set, err := loadProfiles(cmd, deps, true)
			if err != nil {
				return err
			}
			hub, _ := set.FindHub(args[0])
			if hub == nil {
				return unknownHubError(args[0], set)
			}
			hives, err := hivectl.DiscoverHubHives(cmd.Context(), hubDiscoveryClient(env), hub.URL)
			if err != nil {
				return fmt.Errorf("cannot list Hives of hub %q: %w", hub.Name, err)
			}
			rows := hubHiveRows(set, hives)
			if env.options.output != "table" {
				return env.print(cmd, rows)
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "%s (%s): %d Hive(s) advertised, follow=%s\n", hub.Name, hub.URL, len(rows), hub.EffectiveFollow())
			for _, r := range rows {
				status := "not onboarded"
				if r.Onboarded {
					status = "onboarded as " + r.Profile
				}
				label := r.ProjectName
				if r.Org != "" {
					label = r.Org + "/" + label
				}
				_, _ = fmt.Fprintf(out, "%s\t%s\t%s\n", r.ID, label, status)
			}
			return nil
		},
	}
}

func newHivesHubRemoveCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Example: `  hivectl hives hub remove commons`,
		Short:   "Stop tracking a hub (onboarded Hive profiles are kept)",
		Args:    argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			set, err := loadProfiles(cmd, deps, true)
			if err != nil {
				return err
			}
			if _, i := set.FindHub(args[0]); i < 0 {
				return unknownHubError(args[0], set)
			}
			removed, err := set.RemoveHub(args[0])
			if err != nil {
				return err
			}
			if err := commit(deps, set); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Stopped tracking hub %q. Hive profiles are unchanged.\n", removed.Name)
			return nil
		},
	}
}

// hubHiveRows marks each advertised Hive as onboarded when a saved profile has
// the same normalized origin.
func hubHiveRows(set *hivectl.ProfileSet, hives []hivectl.HubHive) []hubHiveRow {
	profiles := map[string]string{}
	for _, p := range set.Profiles {
		id, err := hivectl.NormalizeHiveID(p.Hub)
		if err != nil {
			continue
		}
		if _, ok := profiles[id]; !ok {
			profiles[id] = p.Name
		}
	}
	rows := make([]hubHiveRow, 0, len(hives))
	for _, h := range hives {
		name, ok := profiles[h.ID]
		rows = append(rows, hubHiveRow{HubHive: h, Onboarded: ok, Profile: name})
	}
	return rows
}

func unknownHubError(name string, set *hivectl.ProfileSet) error {
	if len(set.Hubs) == 0 {
		return fmt.Errorf("%w: %q (no hubs are tracked)", hivectl.ErrHubNotFound, name)
	}
	known := make([]string, 0, len(set.Hubs))
	for _, h := range set.Hubs {
		known = append(known, h.Name)
	}
	return fmt.Errorf("%w: %q (tracked: %s)", hivectl.ErrHubNotFound, name, strings.Join(known, ", "))
}
