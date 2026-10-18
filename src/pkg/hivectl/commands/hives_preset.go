package commands

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

const (
	presetStatusNotOnboarded = "not onboarded, skipped"
	presetStatusUnavailable  = "discovery unavailable, saved selection unchanged"
	presetStatusNotAdopted   = "not adopted from this preset, skipped (run hives preset adopt to review)"
)

// presetHiveReport is one hive's line of an adopt or refresh review.
type presetHiveReport struct {
	Preset          string   `json:"preset" yaml:"preset"`
	Hive            string   `json:"hive" yaml:"hive"`
	Profile         string   `json:"profile,omitempty" yaml:"profile,omitempty"`
	Status          string   `json:"status" yaml:"status"`
	Additions       []string `json:"additions,omitempty" yaml:"additions,omitempty"`
	PendingAdds     []string `json:"pending_additions,omitempty" yaml:"pending_additions,omitempty"`
	Removals        []string `json:"removals,omitempty" yaml:"removals,omitempty"`
	PendingRemovals []string `json:"pending_removals,omitempty" yaml:"pending_removals,omitempty"`
	Excluded        []string `json:"excluded,omitempty" yaml:"excluded,omitempty"`
	allCurrent      bool
}

func newHivesPresetCommand(env *commandEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use: "preset",
		Example: `  hivectl hives preset adopt https://bluefin.example/hive-preset.yml
  hivectl hives preset refresh`,
		Short: "Adopt or refresh a contribution preset into your repository selection",
		Long: "Review a project's credential-free contribution preset against your saved repository selection. " +
			"Recommendations become selected only after you confirm them; personal exclusions always win; " +
			"publisher removals are never applied silently; and a discovery failure never broadens scope.",
	}
	cmd.AddCommand(newHivesPresetAdoptCommand(env))
	cmd.AddCommand(newHivesPresetRefreshCommand(env))
	return cmd
}

func newHivesPresetAdoptCommand(env *commandEnv) *cobra.Command {
	var hive string
	var yes, follow bool
	cmd := &cobra.Command{
		Use: "adopt <url-or-file>",
		Example: `  hivectl hives preset adopt https://bluefin.example/hive-preset.yml
  hivectl hives preset adopt ./preset.yml --hive acme --follow-new-repos --yes`,
		Short: "Review a preset and save its recommendations for your onboarded hives",
		Args:  cobra.ExactArgs(1),
		Long: "Loads the preset, matches each recommended Hive to an existing profile by origin (a Hive without a " +
			"profile is reported as not onboarded and skipped; no credentials are created), shows the additions " +
			"against your saved selection and, once confirmed, records them with the preset's provenance. " +
			"all-current snapshots the Hive's currently eligible repositories; it does not follow future ones " +
			"unless --follow-new-repos is given.",
		RunE: func(cmd *cobra.Command, args []string) error {
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
			if hive != "" {
				if p, _ := set.Find(hive); p == nil {
					return unknownHiveError(hive, set)
				}
			}
			source := presetSource(args[0])
			preset, err := hivectl.LoadPreset(cmd.Context(), source)
			if err != nil {
				return err
			}
			id := hivectl.PresetReviewID(preset.Name)
			revision, err := hivectl.PresetRevision(preset)
			if err != nil {
				return err
			}
			reports := env.reviewPreset(cmd, set, preset, id, hive, false)
			pending, reviewable := false, 0
			var unavailable []string
			for i := range reports {
				r := &reports[i]
				if r.Status == presetStatusUnavailable {
					unavailable = append(unavailable, r.Profile)
				}
				p, _ := set.Find(r.Profile)
				if r.Status != "" || p == nil {
					continue
				}
				reviewable++
				// Adopt never drops saved repositories; they are shown as kept.
				r.PendingRemovals, r.Removals = r.Removals, nil
				if len(r.Additions) > 0 || !p.Selection.HasPresetSource(id) {
					pending = true
				}
				if cmd.Flags().Changed("follow-new-repos") && (p.Selection == nil || p.Selection.FollowNewRepositories != follow) {
					pending = true
				}
			}
			if prior := set.FindPreset(id); !pending && prior != nil && prior.Source == source && prior.ReviewedRevision == revision {
				return env.printPresetReview(cmd, reports, fmt.Sprintf("Preset %q is already adopted at revision %s; nothing to change.", id, revision))
			}
			if err := env.printPresetReview(cmd, reports, ""); err != nil {
				return err
			}
			if reviewable == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No onboarded hive could be reviewed; nothing was saved.")
				return presetUnavailableError(unavailable)
			}
			if !yes {
				reader := bufio.NewReader(cmd.InOrStdin())
				confirmed, err := confirmPreset(cmd, reader, fmt.Sprintf("Adopt preset %q (revision %s) for the hives above?", id, revision))
				if err != nil {
					return err
				}
				if !confirmed {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Saved selection left unchanged.")
					return nil
				}
			}
			changed := false
			for _, r := range reports {
				if r.Status != "" {
					continue
				}
				ok, err := set.ApplyPreset(r.Profile, id, r.allCurrent, r.Additions, nil)
				if err != nil {
					return err
				}
				changed = changed || ok
				if cmd.Flags().Changed("follow-new-repos") {
					ok, err = set.SetFollowNewRepositories(r.Profile, follow)
					if err != nil {
						return err
					}
					changed = changed || ok
				}
			}
			if set.RecordPresetReview(id, source, revision, deps.now().UTC().Truncate(time.Second)) {
				changed = true
			}
			if !changed {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Nothing to change.")
				return presetUnavailableError(unavailable)
			}
			if err := commit(deps, set); err != nil {
				return err
			}
			if err := reloadHivesAfterEdit(cmd, deps); err != nil {
				return err
			}
			return presetUnavailableError(unavailable)
		},
	}
	cmd.Flags().StringVar(&hive, "hive", "", "only review the preset's recommendation for this hive profile")
	cmd.Flags().BoolVar(&yes, "yes", false, "adopt the reviewed additions without prompting")
	cmd.Flags().BoolVar(&follow, "follow-new-repos", false, "also include future additions at the adopted hives on refresh (off by default; exclusions still win)")
	return cmd
}

func newHivesPresetRefreshCommand(env *commandEnv) *cobra.Command {
	var hive string
	var yes, acceptRemovals bool
	cmd := &cobra.Command{
		Use: "refresh",
		Example: `  hivectl hives preset refresh
  hivectl hives preset refresh --hive acme --yes
  hivectl hives preset refresh --accept-removals`,
		Short: "Re-fetch adopted presets and review what changed",
		Args:  cobra.NoArgs,
		Long: "Re-fetches each recorded preset source and diffs it against your saved selection. Additions are " +
			"applied automatically only at hives that follow new repositories; elsewhere they need confirmation " +
			"(--yes). Removals are shown and kept unless you confirm them (--accept-removals). A fetch or discovery " +
			"failure preserves the saved selection and is reported as discovery unavailable.",
		RunE: func(cmd *cobra.Command, args []string) error {
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
			if hive != "" {
				if p, _ := set.Find(hive); p == nil {
					return unknownHiveError(hive, set)
				}
			}
			if len(set.Presets) == 0 {
				return errors.New("no adopted presets; run hivectl hives preset adopt <url-or-file> first")
			}
			reader := bufio.NewReader(cmd.InOrStdin())
			changed := false
			var unavailable []string
			var all []presetHiveReport
			for _, recorded := range append([]hivectl.PresetProvenance{}, set.Presets...) {
				preset, err := hivectl.LoadPreset(cmd.Context(), recorded.Source)
				if err != nil {
					unavailable = append(unavailable, recorded.ID)
					report := presetHiveReport{Preset: recorded.ID, Hive: recorded.Source, Status: "preset unavailable, saved selection unchanged: " + err.Error()}
					all = append(all, report)
					if env.options.output == "table" {
						_ = env.printPresetReview(cmd, []presetHiveReport{report}, "")
					}
					continue
				}
				revision, err := hivectl.PresetRevision(preset)
				if err != nil {
					return err
				}
				reports := env.reviewPreset(cmd, set, preset, recorded.ID, hive, true)
				complete := hive == ""
				for i := range reports {
					r := &reports[i]
					if r.Status == presetStatusUnavailable {
						unavailable = append(unavailable, r.Profile)
						complete = false
					}
					if r.Status != "" {
						continue
					}
					p, _ := set.Find(r.Profile)
					if !p.Selection.FollowNewRepositories {
						r.PendingAdds, r.Additions = r.Additions, nil
					}
					r.PendingRemovals = r.Removals
					r.Removals = nil
				}
				if env.options.output == "table" {
					if err := env.printPresetReview(cmd, reports, ""); err != nil {
						return err
					}
				}
				for i := range reports {
					r := &reports[i]
					if r.Status != "" {
						continue
					}
					if len(r.PendingAdds) > 0 {
						ok := yes
						if !ok {
							ok, err = confirmPreset(cmd, reader, fmt.Sprintf("Add %d recommended repositories to %q?", len(r.PendingAdds), r.Profile))
							if err != nil {
								return err
							}
						}
						if ok {
							r.Additions, r.PendingAdds = append(r.Additions, r.PendingAdds...), nil
						}
					}
					if len(r.PendingRemovals) > 0 {
						ok := acceptRemovals
						if !ok && !yes {
							ok, err = confirmPreset(cmd, reader, fmt.Sprintf("Remove %d repositories no longer recommended from %q?", len(r.PendingRemovals), r.Profile))
							if err != nil {
								return err
							}
						}
						if ok {
							r.Removals, r.PendingRemovals = r.PendingRemovals, nil
						}
					}
					if len(r.PendingAdds) > 0 || len(r.PendingRemovals) > 0 {
						complete = false
					}
					ok, err := set.ApplyPreset(r.Profile, recorded.ID, r.allCurrent, r.Additions, r.Removals)
					if err != nil {
						return err
					}
					changed = changed || ok
				}
				all = append(all, reports...)
				// The reviewed revision only advances once every hive was reviewed
				// and nothing is left pending, so the next refresh still shows it.
				if complete && set.RecordPresetReview(recorded.ID, recorded.Source, revision, deps.now().UTC().Truncate(time.Second)) {
					changed = true
				}
			}
			if env.options.output != "table" {
				if err := env.print(cmd, all); err != nil {
					return err
				}
			}
			if changed {
				if err := commit(deps, set); err != nil {
					return err
				}
				if err := reloadHivesAfterEdit(cmd, deps); err != nil {
					return err
				}
			} else {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Saved selection unchanged.")
			}
			return presetUnavailableError(unavailable)
		},
	}
	cmd.Flags().StringVar(&hive, "hive", "", "only refresh this hive profile")
	cmd.Flags().BoolVar(&yes, "yes", false, "accept new recommendations without prompting (removals still need --accept-removals)")
	cmd.Flags().BoolVar(&acceptRemovals, "accept-removals", false, "drop saved repositories the preset no longer recommends")
	return cmd
}

// presetSource keeps URLs as given and makes a local path absolute, so a
// later refresh re-reads the same file from any working directory.
func presetSource(source string) string {
	if strings.Contains(source, "://") {
		return source
	}
	if abs, err := filepath.Abs(source); err == nil {
		return abs
	}
	return source
}

// reviewPreset resolves every preset Hive to an existing profile and diffs its
// recommendation. It never creates profiles or credentials. With
// adoptedOnly, hives whose selection did not come from this preset are skipped.
func (e *commandEnv) reviewPreset(cmd *cobra.Command, set *hivectl.ProfileSet, preset *hivectl.Preset, id, only string, adoptedOnly bool) []presetHiveReport {
	reports := make([]presetHiveReport, 0, len(preset.Hives))
	for _, hive := range preset.Hives {
		report := presetHiveReport{Preset: id, Hive: hive.URL, allCurrent: hive.Repositories.AllCurrent}
		p, err := set.FindHive(hive.URL)
		switch {
		case err != nil:
			report.Status = "skipped: " + err.Error()
		case p == nil:
			report.Status = presetStatusNotOnboarded
		}
		if p != nil {
			report.Profile = p.Name
			if only != "" && !strings.EqualFold(p.Name, only) {
				continue
			}
		} else if only != "" {
			continue
		}
		if report.Status == "" && adoptedOnly && !p.Selection.HasPresetSource(id) {
			report.Status = presetStatusNotAdopted
		}
		if report.Status != "" {
			reports = append(reports, report)
			continue
		}
		var recommended []string
		if hive.Repositories.AllCurrent {
			recommended, err = discoverSelectionRepositories(cmd, p.Hub, e)
			if err != nil {
				report.Status = presetStatusUnavailable
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", p.Name, err)
				reports = append(reports, report)
				continue
			}
		} else {
			recommended, err = hivectl.PresetHiveRepositories(hive)
			if err != nil {
				report.Status = "skipped: " + err.Error()
				reports = append(reports, report)
				continue
			}
		}
		diff, err := set.DiffPreset(p.Name, recommended)
		if err != nil {
			report.Status = "skipped: " + err.Error()
			reports = append(reports, report)
			continue
		}
		report.Additions, report.Removals, report.Excluded = diff.Additions, diff.Removals, diff.Excluded
		reports = append(reports, report)
	}
	if len(preset.Hubs) > 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Preset %q also recommends %d hub(s); hub membership is not adopted here. Onboard their Hives with hivectl hives add.\n", id, len(preset.Hubs))
	}
	return reports
}

func (e *commandEnv) printPresetReview(cmd *cobra.Command, reports []presetHiveReport, note string) error {
	if e.options.output != "table" {
		return e.print(cmd, reports)
	}
	out := cmd.OutOrStdout()
	for _, r := range reports {
		name := r.Hive
		if r.Profile != "" {
			name = r.Profile + " (" + r.Hive + ")"
		}
		if r.Status != "" {
			_, _ = fmt.Fprintf(out, "%s: %s\n", name, r.Status)
			continue
		}
		if len(r.Additions)+len(r.PendingAdds)+len(r.Removals)+len(r.PendingRemovals)+len(r.Excluded) == 0 {
			_, _ = fmt.Fprintf(out, "%s: up to date\n", name)
			continue
		}
		_, _ = fmt.Fprintf(out, "%s:\n", name)
		for _, group := range []struct {
			prefix string
			ids    []string
		}{
			{"+ ", r.Additions},
			{"+? (review) ", r.PendingAdds},
			{"- ", r.Removals},
			{"-? (kept unless confirmed) ", r.PendingRemovals},
			{"x (personally excluded) ", r.Excluded},
		} {
			for _, id := range group.ids {
				_, _ = fmt.Fprintf(out, "  %s%s\n", group.prefix, id)
			}
		}
	}
	if note != "" {
		_, _ = fmt.Fprintln(out, note)
	}
	return nil
}

// presetUnavailableError keeps a partial outage visible to scripts without
// discarding the reviewed changes that were saved for reachable hives.
func presetUnavailableError(unavailable []string) error {
	if len(unavailable) == 0 {
		return nil
	}
	return fmt.Errorf("discovery unavailable for %s; saved selection preserved", strings.Join(unavailable, ", "))
}

func confirmPreset(cmd *cobra.Command, reader *bufio.Reader, question string) (bool, error) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", question)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
