package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

func newHivesReposCommand(env *commandEnv) *cobra.Command {
	var selectMode, include, exclude, clear, toggle, forge, host string
	var reset, yes, deselectCurrent, follow bool
	cmd := &cobra.Command{
		Use: "repos <hive>", Short: "View or edit a hive's repository selection",
		Args: cobra.ExactArgs(1),
		Long: "View saved repository consent, or update it through profiles.yml and the existing relay reload path. Empty means idle, not unrestricted. Scope enforcement requires a compatible Hive and relay.",
		RunE: func(cmd *cobra.Command, args []string) error {
			writes := 0
			for _, flag := range []string{"select", "include", "exclude", "clear", "toggle-exclude", "reset", "deselect-current", "follow-new-repos"} {
				if cmd.Flags().Changed(flag) {
					writes++
				}
			}
			if writes > 1 {
				return fmt.Errorf("choose only one of --select, --include, --exclude, --clear, --toggle-exclude, --reset, --deselect-current, or --follow-new-repos")
			}
			if reset && !yes {
				return fmt.Errorf("--reset restores unrestricted participation; pass --yes to confirm")
			}
			if cmd.Flags().Changed("reset") && !reset {
				return fmt.Errorf("use --reset --yes to restore unrestricted participation")
			}
			if cmd.Flags().Changed("deselect-current") && !deselectCurrent {
				return fmt.Errorf("use --deselect-current to clear current membership while retaining follow policy")
			}
			if !validOutput(env.options.output) {
				return fmt.Errorf("invalid output format %q", env.options.output)
			}
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			set, err := loadProfiles(cmd, deps, false)
			if err != nil {
				return err
			}
			p, _ := set.Find(args[0])
			if p == nil {
				return unknownHiveError(args[0], set)
			}
			if writes == 0 {
				return env.printRepoSelection(cmd, set, p)
			}
			switch {
			case cmd.Flags().Changed("select"):
				var repos []string
				switch selectMode {
				case "all-current":
					repos, err = discoverSelectionRepositories(cmd, p.Hub, env)
					if err != nil {
						return err
					}
				case "none":
				default:
					return fmt.Errorf("--select must be all-current or none")
				}
				err = set.SetSelectionMode(p.Name, selectMode, repos)
			case cmd.Flags().Changed("include"):
				var id string
				id, err = hivectl.RepositoryInput(include, forge, host)
				if err == nil {
					err = set.IncludeRepo(p.Name, id)
				}
			case cmd.Flags().Changed("exclude"):
				var id string
				id, err = hivectl.RepositoryInput(exclude, forge, host)
				if err == nil {
					err = set.ExcludeRepo(p.Name, id)
				}
			case cmd.Flags().Changed("toggle-exclude"):
				var id string
				id, err = hivectl.RepositoryInput(toggle, forge, host)
				if err == nil {
					err = set.ToggleRepoExclusion(p.Name, id)
				}
			case cmd.Flags().Changed("clear"):
				var id string
				id, err = hivectl.RepositoryInput(clear, forge, host)
				if err == nil {
					err = set.ClearRepoOverride(p.Name, id)
				}
			case cmd.Flags().Changed("follow-new-repos"):
				_, err = set.SetFollowNewRepositories(p.Name, follow)
			case deselectCurrent:
				err = set.DeselectCurrent(p.Name)
			case reset:
				err = set.ClearSelection(p.Name)
			}
			if err != nil {
				return err
			}
			if err = commit(deps, set); err != nil {
				return err
			}
			if err := reloadHivesAfterEdit(cmd, deps); err != nil {
				return err
			}
			return env.printRepoSelection(cmd, set, p)
		},
	}
	cmd.Flags().StringVar(&selectMode, "select", "", "all-current (snapshot eligible repositories) or none (select nothing and stop following)")
	cmd.Flags().StringVar(&include, "include", "", "personally include owner/name or a canonical repository ID; exclusions still win")
	cmd.Flags().StringVar(&exclude, "exclude", "", "personally exclude owner/name or a canonical repository ID")
	cmd.Flags().StringVar(&toggle, "toggle-exclude", "", "toggle a personal exclusion (same as x in the Hives overlay)")
	cmd.Flags().StringVar(&clear, "clear", "", "clear personal inclusion and exclusion for a repository")
	cmd.Flags().StringVar(&forge, "forge", "github", "forge for owner/name: github, gitlab, or gitea")
	cmd.Flags().StringVar(&host, "host", "github.com", "forge host for owner/name")
	cmd.Flags().BoolVar(&deselectCurrent, "deselect-current", false, "clear current membership and personal inclusions, retaining exclusions and follow policy")
	cmd.Flags().BoolVar(&follow, "follow-new-repos", false, "follow additions on supported refresh; use =false to stop following without changing membership")
	cmd.Flags().BoolVar(&reset, "reset", false, "drop the selection and restore legacy unrestricted participation")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm unrestricted reset")
	return cmd
}

func reloadHivesAfterEdit(cmd *cobra.Command, deps *hivesDeps) error {
	result, err := signalHivesRelay(cmd, deps)
	if err != nil {
		return fmt.Errorf("profiles saved, but relay reload failed: %w", err)
	}
	if result.Running {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Profiles saved; running relay signaled (%s). In-flight work finishes on its original hive.\n", result.Target)
	} else {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Profiles saved; no running relay found, applies on next start.")
	}
	return nil
}

// Discovery is public. Never attach the saved registration token or turn an
// error/unsupported endpoint into an empty snapshot or a wildcard.
func discoverSelectionRepositories(cmd *cobra.Command, hub string, env *commandEnv) ([]string, error) {
	base, err := hivectl.HubHTTPBase(hub)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, strings.TrimRight(base, "/")+"/api/contribute/repositories", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: env.options.timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("repository discovery unavailable; selection unchanged: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("repository discovery returned HTTP %d; selection unchanged", response.StatusCode)
	}
	var payload struct {
		Repositories *[]struct {
			ID string `json:"id"`
		} `json:"repositories"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("invalid repository discovery; selection unchanged: %w", err)
	}
	if payload.Repositories == nil {
		return nil, fmt.Errorf("repository discovery omitted repositories; selection unchanged")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("invalid trailing repository discovery data; selection unchanged")
	}
	repos := make([]string, 0, len(*payload.Repositories))
	for _, repo := range *payload.Repositories {
		id, err := hivectl.NormalizeRepositoryID(repo.ID)
		if err != nil {
			return nil, err
		}
		repos = append(repos, id)
	}
	return repos, nil
}

type repoSelectionRow struct {
	ID       string `json:"id" yaml:"id"`
	Source   string `json:"source" yaml:"source"`
	Selected bool   `json:"selected" yaml:"selected"`
}

func (e *commandEnv) printRepoSelection(cmd *cobra.Command, set *hivectl.ProfileSet, p *hivectl.Profile) error {
	rows := []repoSelectionRow{}
	if p.Selection != nil {
		// Include excluded recommendations in the view so contributors can undo them.
		sources := map[string]string{}
		base := p.Selection.Repositories
		source := "preset"
		if len(p.Selection.Sources) > 0 {
			source += ":" + strings.Join(p.Selection.Sources, ",")
		} else {
			source = "reviewed"
		}
		if p.Selection.Mode == hivectl.SelectionAllCurrent {
			base = p.Selection.MembershipSnapshot
			source = "snapshot"
		}
		for _, id := range base {
			canonical, _ := hivectl.NormalizeRepositoryID(id)
			sources[canonical] = source
		}
		for _, id := range p.Selection.Include {
			canonical, _ := hivectl.NormalizeRepositoryID(id)
			sources[canonical] = "personal"
		}
		// Match projection's cross-profile exclusion precedence.
		excluded := map[string]bool{}
		for _, profile := range set.Profiles {
			if profile.Selection != nil {
				for _, id := range profile.Selection.Exclude {
					canonical, _ := hivectl.NormalizeRepositoryID(id)
					excluded[canonical] = true
				}
			}
		}
		for _, id := range p.Selection.Exclude {
			canonical, _ := hivectl.NormalizeRepositoryID(id)
			sources[canonical] = "personal exclusion"
		}
		for id, source := range sources {
			rows = append(rows, repoSelectionRow{ID: id, Source: source, Selected: !excluded[id]})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if e.options.output != "table" {
		return e.print(cmd, struct {
			Hive         string             `json:"hive" yaml:"hive"`
			Legacy       bool               `json:"legacy_unrestricted" yaml:"legacy_unrestricted"`
			Follow       bool               `json:"follow_new_repositories" yaml:"follow_new_repositories"`
			Repositories []repoSelectionRow `json:"repositories" yaml:"repositories"`
		}{p.Name, p.Selection == nil, p.Selection != nil && p.Selection.FollowNewRepositories, rows})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", p.Name, set.SelectionSummary(p.Name))
	if p.Selection == nil {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Legacy unrestricted participation; use --select all-current to review a snapshot.")
		return nil
	}
	if set.SelectionSummary(p.Name) == "none" {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No repositories selected; a scope-capable relay idles for this hive.")
	}
	if p.Selection.FollowNewRepositories {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Follow future repositories: requested (saved snapshot remains authoritative until a supported refresh).")
	}
	for _, row := range rows {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\tselected=%t\t%s\n", row.ID, row.Selected, row.Source)
	}
	return nil
}
