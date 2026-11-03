package commands

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// newHivesSetCommand changes per-hive local settings that are not credentials
// or rank. Today that is the relay's local audit level (#13456).
func newHivesSetCommand(env *commandEnv) *cobra.Command {
	var auditLevel string
	cmd := &cobra.Command{
		Use:   "set <name> --audit-level <" + strings.Join(hivectl.AuditLevels, "|") + ">",
		Short: "Change a saved hive's local settings (audit level)",
		Long: "Set what this machine's relay records locally for tasks from one hive. --audit-level is one of " +
			strings.Join(hivectl.AuditLevels, ", ") + " (default " + hivectl.DefaultAuditLevel + "); each level " +
			"includes the ones before it. The record stays on this machine (see `hivectl hives audit`); a running " +
			"relay applies the new level from its next task.",
		Example: "  hivectl hives set acme --audit-level commands",
		Args:    argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("audit-level") {
				return errors.New("nothing to set: pass --audit-level")
			}
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			set, err := loadProfiles(cmd, deps, false)
			if err != nil {
				return err
			}
			profile, err := set.SetAuditLevel(args[0], strings.ToLower(strings.TrimSpace(auditLevel)))
			if err != nil {
				return err
			}
			if err := commit(deps, set); err != nil {
				return err
			}
			result, err := signalHivesRelay(cmd, deps)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Hive %s audit level set to %s (local only)\n", profile.Name, profile.AuditLevel)
			if result.Running {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  running relay signaled; applies from its next task")
			} else {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  no running relay found; applies on next start")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&auditLevel, "audit-level", "", "local audit level: "+strings.Join(hivectl.AuditLevels, "|"))
	return cmd
}
