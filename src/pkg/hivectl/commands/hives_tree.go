package commands

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

func newHivesTreeCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use: "tree", Short: "Inspect saved contribution consent without changing local state",
		Long: "Show a credential-free contribution tree from saved profiles. Does not migrate legacy credentials, discover membership, register, commit, or signal the relay. Missing discovery, readiness and activity remain unknown. Hub discovery integration is pending; saved subscriptions appear under Direct Hives.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validOutput(env.options.output) {
				return fmt.Errorf("invalid output format %q", env.options.output)
			}
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			// Load, not LoadOrMigrate: even opening a legacy installation is read-only.
			set, err := deps.store.Load()
			if err == nil && set == nil {
				return fmt.Errorf("no saved profiles; tree does not migrate contributor.env (use hives list to review legacy setup)")
			}
			if err != nil {
				return err
			}
			tree, err := hivectl.BuildContributionTree(set, nil, nil)
			if err != nil {
				return err
			}
			if env.options.output != "table" {
				return env.print(cmd, tree)
			}
			byID := map[string]hivectl.TreeSubscription{}
			for _, s := range tree.Subscriptions {
				byID[s.ID] = s
			}
			for _, h := range tree.Hubs {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s — %s\n", h.Name, treeSelectionLabel(h.Selection))
				for _, id := range h.Subscriptions {
					s := byID[id]
					shared := ""
					if s.SharedAcrossHubs > 1 {
						shared = fmt.Sprintf("; Shared across %d hubs", s.SharedAcrossHubs)
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s (%s) — %s; follow=%t; readiness=%s; activity=%s%s\n", s.ID, s.HiveID, treeSelectionLabel(s.Selection), s.Follow, s.Readiness, s.Activity, shared)
					for _, r := range s.Repositories {
						mark := "[ ]"
						if r.Selected {
							mark = "[x]"
						}
						badge := ""
						if len(r.ExcludedBy) > 0 {
							badge = fmt.Sprintf(" Excluded by %v", r.ExcludedBy)
						}
						if r.Unreviewed {
							badge += " Unreviewed"
						}
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "    %s %s%s; offered=%s\n", mark, r.ID, badge, r.Offered)
					}
				}
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Saved consent only — membership incomplete; relay readiness and activity unavailable. Hub integration pending.")
			return nil
		},
	}
}
func treeSelectionLabel(s hivectl.TreeSelection) string {
	if s.Legacy {
		return "Unrestricted — legacy present"
	}
	label := s.State
	if s.Repositories == 0 {
		label = "Off — no saved repositories"
	}
	if s.MembershipIncomplete {
		label += "; Membership incomplete"
	}
	return fmt.Sprintf("%s (%d/%d selected; %d excluded; %d unreviewed)", label, s.Selected, s.Repositories, s.Excluded, s.Unreviewed)
}
