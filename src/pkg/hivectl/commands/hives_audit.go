package commands

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// relayAuditView is the --json shape of `hivectl hives audit`.
type relayAuditView struct {
	Available bool                       `json:"available" yaml:"available"`
	Path      string                     `json:"path" yaml:"path"`
	Retention string                     `json:"retention" yaml:"retention"`
	Rows      []hivectl.RelayAuditRecord `json:"rows" yaml:"rows"`
}

func newHivesAuditCommand(env *commandEnv) *cobra.Command {
	var (
		since, task, level string
		asJSON, clear      bool
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show the relay's local audit record (lifecycle, commands, reasons)",
		Args:  argsNone(),
		Long: "Read the contributor relay's local leveled audit stream (~/.config/hive/relay-activity.jsonl): " +
			"task lifecycle events, GitHub-touching gh/git commands and, at the reasoning level, the assistant " +
			"turn that preceded each command. What is recorded per hive is set with " +
			"`hivectl hives set <name> --audit-level`. Nothing is uploaded. --level shows rows at or below a level; " +
			"--clear --level reasoning removes only the stored reasons and keeps every row.",
		Example: `  hivectl hives audit --since 24h
  hivectl hives audit --task 1234 --level commands
  hivectl hives audit --json
  hivectl hives audit --clear --level reasoning`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format := env.options.output
			if asJSON {
				format = "json"
			}
			if !validOutput(format) {
				return fmt.Errorf("invalid output format %q", format)
			}
			level = strings.ToLower(strings.TrimSpace(level))
			if level != "" {
				if err := hivectl.ValidateAuditLevel(level); err != nil {
					return err
				}
			}
			deps, err := env.hivesDeps()
			if err != nil {
				return err
			}
			if clear {
				if level != hivectl.AuditLevelReasoning {
					return errors.New("--clear requires --level reasoning (use `hivectl hives activity --clear` to delete all local history)")
				}
				n, err := deps.store.ClearRelayAuditReasons()
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Cleared reasons from %d command row(s) in %s; rows and other fields kept.\n", n, deps.store.RelayActivityPath())
				return nil
			}
			filter := hivectl.RelayAuditFilter{Task: strings.TrimSpace(task), Level: level}
			if since != "" {
				t, err := parseAuditSince(since, time.Now())
				if err != nil {
					return err
				}
				filter.Since = t
			}
			view := relayAuditView{Path: deps.store.RelayActivityPath(), Retention: hivectl.RelayActivityRetention(), Rows: []hivectl.RelayAuditRecord{}}
			records, skipped, err := deps.store.LoadRelayAudit()
			if errors.Is(err, hivectl.ErrRelayActivityUnavailable) {
				if format != "table" {
					return printer{format: format, out: cmd.OutOrStdout()}.print(view)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Audit: unavailable (retention: %s)\n", view.Retention)
				return nil
			}
			if err != nil {
				return err
			}
			if skipped > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Skipped %d unreadable line(s) in %s.\n", skipped, view.Path)
			}
			view.Available = true
			view.Rows = hivectl.FilterRelayAudit(records, filter)
			if format != "table" {
				return printer{format: format, out: cmd.OutOrStdout()}.print(view)
			}
			out := cmd.OutOrStdout()
			if len(view.Rows) == 0 {
				_, _ = fmt.Fprintln(out, "Audit: no matching rows")
				return nil
			}
			for _, r := range view.Rows {
				_, _ = fmt.Fprintln(out, formatAuditRow(r))
				if r.Reason != "" {
					_, _ = fmt.Fprintf(out, "    reason (%s): %s\n", r.ReasonSource, oneLine(r.Reason))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only rows at or after this time: a duration (24h, 7d) or RFC 3339 timestamp")
	cmd.Flags().StringVar(&task, "task", "", "only rows for this task id")
	cmd.Flags().StringVar(&level, "level", "", "only rows at or below this level: "+strings.Join(hivectl.AuditLevels, "|"))
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON (same as -o json)")
	cmd.Flags().BoolVar(&clear, "clear", false, "with --level reasoning: remove stored reasons, keeping every row")
	return cmd
}

// parseAuditSince accepts a Go duration, a whole number of days ("7d") or an
// RFC 3339 timestamp.
func parseAuditSince(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--since %q must be a duration (24h, 7d) or an RFC 3339 timestamp", raw)
}

func formatAuditRow(r hivectl.RelayAuditRecord) string {
	parts := []string{r.TS.UTC().Format(time.RFC3339), r.Level, "task=" + r.TaskID}
	switch {
	case r.IsTerminal():
		parts = append(parts, "outcome="+r.Outcome)
	case r.IsCommand():
		if r.Cmd != "" {
			parts = append(parts, oneLine(r.Cmd))
		} else {
			parts = append(parts, r.Op)
		}
		if r.Exit != nil {
			parts = append(parts, "exit="+strconv.Itoa(*r.Exit))
		}
		if r.Remote != "" {
			parts = append(parts, "remote="+r.Remote)
		}
		if r.Refspec != "" {
			parts = append(parts, "refspec="+r.Refspec)
		}
	default:
		parts = append(parts, r.Event)
		if r.Repository != "" {
			parts = append(parts, r.Repository)
		}
		if r.URL != "" {
			parts = append(parts, r.URL)
		}
		if r.Backend != "" && !r.IsTerminal() {
			parts = append(parts, "backend="+r.Backend)
		}
		if r.Stash != "" {
			parts = append(parts, "stash="+r.Stash)
		}
		if r.TokenGen > 0 {
			parts = append(parts, "token_gen="+strconv.Itoa(r.TokenGen))
		}
		if r.Capture != "" {
			parts = append(parts, "capture="+r.Capture)
		}
		if r.Reason != "" {
			parts = append(parts, "reason="+oneLine(r.Reason))
		}
	}
	return strings.Join(parts, "\t")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
