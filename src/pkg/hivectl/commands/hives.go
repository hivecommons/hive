package commands

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/hivectl/profiles"
	"github.com/spf13/cobra"
)

// hivesOptions carries the flags shared by the hives subcommands. dir is
// overridable for tests; empty means the contributor config directory
// (~/.config/hive).
type hivesOptions struct {
	dir string
}

func newHivesCommand(env *commandEnv) *cobra.Command {
	opts := &hivesOptions{}
	cmd := &cobra.Command{
		Use:   "hives",
		Short: "Manage named hive profiles (the hives this machine contributes to)",
		Long: `Manage named hive profiles (hivecommons/hive#8097).

Profiles live in ~/.config/hive/profiles.yml; contributor.env is regenerated
from them as a read-only projection for the relay. The first 'hives' command
run against an old positional contributor.env migrates it automatically.`,
		Example: `  hivectl hives list
  hivectl hives add myhive --hub wss://hive.example.dev/contribute --username octocat
  hivectl hives use myhive
  hivectl hives remove myhive
  hivectl hives rename myhive work`,
	}
	cmd.PersistentFlags().StringVar(&opts.dir, "config-dir", "", "profiles directory (default ~/.config/hive)")
	cmd.AddCommand(newHivesListCommand(env, opts))
	cmd.AddCommand(newHivesAddCommand(env, opts))
	cmd.AddCommand(newHivesUseCommand(env, opts))
	cmd.AddCommand(newHivesRemoveCommand(env, opts))
	cmd.AddCommand(newHivesRenameCommand(env, opts))
	return cmd
}

func (o *hivesOptions) resolveDir() (string, error) {
	if o.dir != "" {
		return o.dir, nil
	}
	return profiles.DefaultDir()
}

func (o *hivesOptions) load() (string, *profiles.File, error) {
	dir, err := o.resolveDir()
	if err != nil {
		return "", nil, err
	}
	f, err := profiles.LoadOrMigrate(dir)
	if err != nil {
		return "", nil, err
	}
	return dir, f, nil
}

// hiveRow is the list view of one profile. The registration token is a live
// credential and is deliberately not part of any output format.
type hiveRow struct {
	Name          string `json:"name" yaml:"name"`
	Hub           string `json:"hub" yaml:"hub"`
	ContributorID string `json:"contributor_id" yaml:"contributor_id"`
	Session       string `json:"session,omitempty" yaml:"session,omitempty"`
	Backend       string `json:"backend,omitempty" yaml:"backend,omitempty"`
	Active        bool   `json:"active" yaml:"active"`
	AddedAt       string `json:"added_at,omitempty" yaml:"added_at,omitempty"`
}

func newHivesListCommand(env *commandEnv, opts *hivesOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List saved hive profiles with the active one marked",
		Args:  argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validOutput(env.options.output) {
				return &usageError{message: fmt.Sprintf("invalid --output %q; expected table, json, yaml, or jsonl", env.options.output)}
			}
			_, f, err := opts.load()
			if err != nil {
				return err
			}
			active := f.ActiveProfile()
			rows := make([]any, 0, len(f.Profiles))
			for i := range f.Profiles {
				p := &f.Profiles[i]
				rows = append(rows, hiveRow{
					Name:          p.Name,
					Hub:           p.Hub,
					ContributorID: p.ContributorID,
					Session:       p.Session,
					Backend:       p.Backend,
					Active:        p == active,
					AddedAt:       p.AddedAt,
				})
			}
			if len(rows) == 0 && env.options.output == "table" {
				return env.printNoProfiles(cmd)
			}
			return env.print(cmd, rows)
		},
	}
}

func (e *commandEnv) printNoProfiles(cmd *cobra.Command) error {
	_, err := fmt.Fprintln(cmd.OutOrStdout(), "No hive profiles saved. Add one with 'hivectl hives add <name> --hub <url>' or run 'just contribute-setup'.")
	return err
}

func newHivesAddCommand(env *commandEnv, opts *hivesOptions) *cobra.Command {
	var hub, username, session, backend string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Register with a hub and save it as a named profile",
		Long: `Register with a hub (the registration half of contribute-setup) and save the
credential as a named profile. Registration is keyed on your GitHub username
and issues a token the hub can never reprint; the profile file holds it at
mode 0600.`,
		Example: `  hivectl hives add myhive --hub wss://hive.example.dev/contribute --username octocat
  hivectl hives add second-session --hub wss://hive.example.dev/contribute --username octocat --session goose`,
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHivesAdd(cmd, env, opts, args[0], hub, username, session, backend)
		},
	}
	cmd.Flags().StringVar(&hub, "hub", "", "hub contribute URL (wss://... or https://...)")
	cmd.Flags().StringVar(&username, "username", "", "GitHub username to register (default: the profile file's saved username)")
	cmd.Flags().StringVar(&session, "session", "", "optional HIVE_SESSION label so a second relay against the same hive gets its own slot")
	cmd.Flags().StringVar(&backend, "backend", "", "optional AGENT_BACKEND default for this profile")
	return cmd
}

func runHivesAdd(cmd *cobra.Command, env *commandEnv, opts *hivesOptions, name, hub, username, session, backend string) error {
	if strings.TrimSpace(hub) == "" {
		return &usageError{message: "--hub is required"}
	}
	if !profiles.ValidName(name) {
		return &usageError{message: fmt.Sprintf("invalid profile name %q (letters, digits, '.', '_' and '-' only)", name)}
	}
	dir, f, err := opts.load()
	if err != nil {
		return err
	}
	if username == "" {
		username = f.Username
	}
	if username == "" {
		username = strings.TrimSpace(os.Getenv("CONTRIBUTOR_USERNAME"))
	}
	if username == "" {
		return &usageError{message: "--username is required the first time (no saved username to reuse)"}
	}
	reg, err := profiles.Register(cmd.Context(), &http.Client{Timeout: env.options.timeout}, hub, username)
	if err != nil {
		return err
	}
	if f.Username == "" {
		f.Username = username
	}
	if err := f.Add(profiles.Profile{
		Name:              name,
		Hub:               profiles.HubWSURL(hub),
		ContributorID:     reg.ContributorID,
		RegistrationToken: reg.RegistrationToken,
		Session:           session,
		Backend:           backend,
	}); err != nil {
		return err
	}
	if len(f.Profiles) == 1 {
		f.Active = name
	}
	if err := profiles.Save(dir, f); err != nil {
		return err
	}
	if err := profiles.Project(dir, f); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Registered %s on %s as profile %q (%s).\n", username, hub, name, reg.ContributorID)
	return err
}

func newHivesUseCommand(env *commandEnv, opts *hivesOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Make a profile the active hive",
		Long: `Make a profile the active hive. The projection lists the active hive first,
which is where the relay starts soliciting; a relay that is already running
picks the change up on its next restart (a live switch is phase 2 of #8097).`,
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, f, err := opts.load()
			if err != nil {
				return err
			}
			if err := f.Use(args[0]); err != nil {
				return err
			}
			if err := profiles.Save(dir, f); err != nil {
				return err
			}
			if err := profiles.Project(dir, f); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Active hive is now %q (%s). Restart the relay to switch.\n", args[0], f.ActiveProfile().Hub)
			return err
		},
	}
}

func newHivesRemoveCommand(env *commandEnv, opts *hivesOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a saved profile",
		Long: `Remove a saved profile. The registration token it held is discarded locally;
the hub-side identity is untouched, and the hub can never reprint the token,
so removing a profile you still need means re-running registration.`,
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, f, err := opts.load()
			if err != nil {
				return err
			}
			if err := f.Remove(args[0]); err != nil {
				return err
			}
			if err := profiles.Save(dir, f); err != nil {
				return err
			}
			if len(f.Profiles) > 0 {
				if err := profiles.Project(dir, f); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed profile %q.\n", args[0])
			return err
		},
	}
}

func newHivesRenameCommand(env *commandEnv, opts *hivesOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a saved profile",
		Args:  argsExact(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, f, err := opts.load()
			if err != nil {
				return err
			}
			if err := f.Rename(args[0], args[1]); err != nil {
				return err
			}
			if err := profiles.Save(dir, f); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Renamed profile %q to %q.\n", args[0], args[1])
			return err
		},
	}
}
