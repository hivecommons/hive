package commands

import (
	"fmt"
	"math"
	"os"
	"strings"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/outcomebench"
	"github.com/spf13/cobra"
)

// `hivectl evidence benchmark` (hivecommons/hive#14577): a Markdown report
// comparing outcomes of Hive-mediated PRs with unmediated ones, built by
// pkg/outcomebench from the same folded bundles `evidence export` reads.
// Local filesystem only, and reporting only.

func newEvidenceBenchmarkCommand(_ *commandEnv) *cobra.Command {
	var root, since, until, repo, out string
	var spend float64
	cmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Render the mediated-vs-unmediated outcome benchmark report as Markdown",
		Long: "Fold the schema v1 evidence bundles under --root per PR (as `evidence export` does) and compare PRs Hive\n" +
			"mediated (at least one Hive reviewer verdict or posted review) with unmediated ones: revert rate, change-failure\n" +
			"rate (reverts + issue reopens), CI failure at the merged head, Hive changes_requested verdict rate (mediated only),\n" +
			"median time to merge and, with --fleet-spend-usd, cost per merged mediated PR. control_window on/off is a\n" +
			"secondary cut when bundles carry it. Rates carry 95% Wilson intervals; any cell with n < 10 reads \"not reported\".\n" +
			"The report ends with a methodology section and the metrics schema v1 cannot answer.\n\n" +
			"--since and --until take an RFC3339 timestamp or a Go duration back from now and window PRs on open time\n" +
			"(since <= opened_at < until). Use absolute timestamps for a committed report: the output depends only on the\n" +
			"bundles and the flags, so the same root and window reproduce the same bytes.\n" +
			"Reads the local filesystem only; no network, no hive session, and nothing here affects merge eligibility.",
		Example: "  hivectl evidence benchmark --since 2026-10-01T00:00:00Z --until 2026-11-01T00:00:00Z \\\n" +
			"      --out src/docs/benchmarks/outcomes-2026-10.md\n" +
			"  hivectl evidence benchmark --since 720h --repo hivecommons/hive --fleet-spend-usd 412.50\n" +
			"  src/scripts/outcome-benchmark.sh 2026-10       # the same, for one calendar month",
		Args: argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			now := evidenceExportNow()
			from, err := parseEvidenceSince(since, now)
			if err != nil {
				return &usageError{message: err.Error()}
			}
			to, err := parseEvidenceSince(until, now)
			if err != nil {
				return &usageError{message: strings.Replace(err.Error(), "--since", "--until", 1)}
			}
			if !from.IsZero() && !to.IsZero() && !to.After(from) {
				return &usageError{message: "--until must be after --since"}
			}
			repo = strings.TrimSpace(repo)
			if repo != "" && strings.Count(repo, "/") != 1 {
				return &usageError{message: fmt.Sprintf("invalid --repo %q; expected owner/name", repo)}
			}
			opts := outcomebench.Options{Root: root, Repo: repo, Since: from, Until: to}
			if cmd.Flags().Changed("fleet-spend-usd") {
				if spend < 0 || math.IsNaN(spend) || math.IsInf(spend, 0) {
					return &usageError{message: fmt.Sprintf("invalid --fleet-spend-usd %v; expected a non-negative amount", spend)}
				}
				opts.FleetSpendUSD = &spend
			}

			bundles, unreadable, err := loadEvidenceBundles(root)
			if err != nil {
				return err
			}
			if unreadable > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %d unreadable or non-v1 bundle(s) under %s\n", unreadable, root)
			}
			report := outcomebench.Build(bundles, unreadable, opts)
			md := outcomebench.Render(report)
			if out == "" || out == "-" {
				_, err = fmt.Fprint(cmd.OutOrStdout(), md)
				return err
			}
			if err := os.WriteFile(out, []byte(md), 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", out, err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote benchmark over %d PR(s) to %s\n", report.PRs, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", ghpkg.ReviewEvidenceRoot, "evidence root holding <org>/<repo>/<pr>/<sha>.json bundles")
	cmd.Flags().StringVar(&since, "since", "", "only PRs opened at or after this RFC3339 time or within this Go duration (e.g. 720h)")
	cmd.Flags().StringVar(&until, "until", "", "only PRs opened before this RFC3339 time or Go duration back from now")
	cmd.Flags().StringVar(&repo, "repo", "", "only this owner/name repository (case-insensitive)")
	cmd.Flags().StringVar(&out, "out", "", "write the Markdown report to this file instead of stdout (\"-\" is stdout)")
	cmd.Flags().Float64Var(&spend, "fleet-spend-usd", 0, "fleet estimated spend over the window in USD; enables cost per merged mediated PR")
	return cmd
}
