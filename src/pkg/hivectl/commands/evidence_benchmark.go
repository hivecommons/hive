package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/outcomebench"
)

// evidenceBenchmarkNow is overridable in tests.
var evidenceBenchmarkNow = func() time.Time { return time.Now().UTC() }

// benchmarkReadmeDocDir is the benchmark directory as the repository README
// links to it.
const benchmarkReadmeDocDir = "src/docs/benchmarks"

func newEvidenceBenchmarkCommand(_ *commandEnv) *cobra.Command {
	var root, month, now, spendPath, hubPath, storiesPath, redTeamPath, outDir, readme, source, fromJSON string
	var readmeMinBase int
	cmd := &cobra.Command{
		Use:   "benchmark",
		Short: "Compare mediated and unmediated merged PRs from stored evidence and write the outcomes report",
		Long: "Fold the schema v1 evidence bundles under --root per PR and compare the pull requests Hive's review swarm\n" +
			"touched (at least one verdict or posted review) with those merged without it: revert rate, change-failure rate,\n" +
			"median time-to-merge, CI failure at merged head, changes-requested-before-merge, and cost per merged PR when a\n" +
			"saved /api/cost response is given with --spend. Every rate carries its n and a 95% Wilson interval; n < 10 is\n" +
			"printed as \"not reported\". No telemetry is added and nothing contacts a hive.\n\n" +
			"Writes <out-dir>/outcomes-YYYY-MM.md, outcomes-YYYY-MM.json, assets/outcomes-YYYY-MM-{bars,trend}.svg and the\n" +
			"plain-language README cards assets/outcomes-YYYY-MM-card-<name>-{dark,light}.svg (one number per card, no\n" +
			"statistics; cards whose base is under --readme-min-base are not written). --stories supplies curated real PRs\n" +
			"for the story card; --red-team the prompt-injection eval summary. --readme rewrites the <!-- outcomes:start --> … <!-- outcomes:end --> block of that README\n" +
			"from the same report, so the README never drifts from the committed report (a test asserts they match).",
		Example: "  hivectl evidence benchmark --root ./evidence --month 2026-10 --spend cost.json --out-dir src/docs/benchmarks --readme README.md\n" +
			"  hivectl evidence benchmark --root /data/evidence --month 2026-10 --out-dir /tmp/bench\n" +
			"  hivectl evidence benchmark --from-json src/docs/benchmarks/outcomes-2026-10.json --out-dir src/docs/benchmarks --readme README.md",
		Args: argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fromJSON != "" {
				if cmd.Flags().Changed("root") || cmd.Flags().Changed("month") || spendPath != "" || hubPath != "" || storiesPath != "" || redTeamPath != "" {
					return &usageError{message: "--from-json re-renders a saved report and takes no evidence inputs"}
				}
				rep, err := loadBenchmarkReport(fromJSON)
				if err != nil {
					return err
				}
				return emitBenchmark(cmd, rep, outDir, readme)
			}
			var opts outcomebench.Options
			var err error
			if strings.TrimSpace(month) != "" {
				if opts.Since, opts.Until, err = outcomebench.ParseWindow(month); err != nil {
					return &usageError{message: err.Error()}
				}
			}
			opts.Now = evidenceBenchmarkNow()
			if strings.TrimSpace(now) != "" {
				if opts.Now, err = time.Parse(time.RFC3339, strings.TrimSpace(now)); err != nil {
					return &usageError{message: fmt.Sprintf("invalid --now %q; expected RFC3339", now)}
				}
			}
			if spendPath != "" {
				if opts.Spend, err = outcomebench.LoadSpend(spendPath); err != nil {
					return err
				}
			}
			if hubPath != "" {
				if opts.Hub, err = outcomebench.LoadHub(hubPath); err != nil {
					return err
				}
			}
			if storiesPath != "" {
				if opts.Stories, err = outcomebench.LoadStories(storiesPath); err != nil {
					return err
				}
			}
			if redTeamPath != "" {
				if opts.RedTeam, err = outcomebench.LoadRedTeam(redTeamPath); err != nil {
					return err
				}
			}
			opts.ReadmeMinBase = readmeMinBase
			opts.Source = source
			if opts.Source == "" {
				opts.Source = "evidence bundles under " + root
			}
			bundles, unreadable, err := outcomebench.Load(root)
			if err != nil {
				return err
			}
			if unreadable > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %d unreadable or non-v1 bundle(s) under %s\n", unreadable, root)
			}
			return emitBenchmark(cmd, outcomebench.Compute(bundles, unreadable, opts), outDir, readme)
		},
	}
	cmd.Flags().StringVar(&fromJSON, "from-json", "", "re-render markdown, assets and the README block from a saved outcomes-YYYY-MM.json instead of recomputing from evidence")
	cmd.Flags().StringVar(&root, "root", ghpkg.ReviewEvidenceRoot, "evidence root holding <org>/<repo>/<pr>/<sha>.json bundles")
	cmd.Flags().StringVar(&month, "month", "", "only PRs merged in this YYYY-MM month (default: all)")
	cmd.Flags().StringVar(&now, "now", "", "observation time (RFC3339) for the maturation window; default now")
	cmd.Flags().StringVar(&spendPath, "spend", "", "saved GET /api/cost JSON for cost per merged PR")
	cmd.Flags().StringVar(&hubPath, "hub", "", "saved GET /api/hub/evidence JSON for fleet context")
	cmd.Flags().StringVar(&storiesPath, "stories", "", "saved JSON list of real PRs for the README story card")
	cmd.Flags().StringVar(&redTeamPath, "red-team", "", "saved JSON summary of the prompt-injection eval (docs/ioscan-red-team.md) for the security card")
	cmd.Flags().IntVar(&readmeMinBase, "readme-min-base", outcomebench.DefaultReadmeMinBase, "smallest count a README card may be built on")
	cmd.Flags().StringVar(&outDir, "out-dir", "docs/benchmarks", "directory for the report, JSON and assets/")
	cmd.Flags().StringVar(&readme, "readme", "", "README whose outcomes block to rewrite from this report")
	cmd.Flags().StringVar(&source, "source", "", "how the report names its source (default: the --root path)")
	return cmd
}

// loadBenchmarkReport reads a committed outcomes-YYYY-MM.json.
func loadBenchmarkReport(path string) (outcomebench.Report, error) {
	var rep outcomebench.Report
	data, err := os.ReadFile(path)
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		return rep, fmt.Errorf("parsing %s: %w", path, err)
	}
	return rep, nil
}

// emitBenchmark writes the report's outputs and, if asked, the README block.
func emitBenchmark(cmd *cobra.Command, rep outcomebench.Report, outDir, readme string) error {
	paths, err := writeBenchmark(rep, outDir)
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", p)
	}
	if readme != "" {
		data, err := os.ReadFile(readme)
		if err != nil {
			return fmt.Errorf("reading %s: %w", readme, err)
		}
		updated, err := outcomebench.UpdateReadme(string(data), rep.ReadmeBlock(benchmarkReadmeDocDir))
		if err != nil {
			return fmt.Errorf("%s: %w", readme, err)
		}
		if err := os.WriteFile(readme, []byte(updated), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", readme, err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "updated outcomes block in %s\n", readme)
	}
	fmt.Fprint(cmd.OutOrStdout(), benchmarkSummary(rep))
	return nil
}

// writeBenchmark writes the markdown, JSON, SVG and PNG outputs and returns
// their paths.
func writeBenchmark(rep outcomebench.Report, outDir string) ([]string, error) {
	if err := os.MkdirAll(filepath.Join(outDir, "assets"), 0o755); err != nil {
		return nil, err
	}
	js, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, err
	}
	base := filepath.Join(outDir, "outcomes-"+rep.Month())
	files := map[string][]byte{
		base + ".md":   []byte(rep.Markdown()),
		base + ".json": append(js, '\n'),
		filepath.Join(outDir, "assets", rep.AssetBase()+"-bars.svg"):  []byte(rep.BarChartSVG()),
		filepath.Join(outDir, "assets", rep.AssetBase()+"-trend.svg"): []byte(rep.TrendSVG()),
	}
	order := []string{base + ".md", base + ".json", filepath.Join(outDir, "assets", rep.AssetBase()+"-bars.svg"), filepath.Join(outDir, "assets", rep.AssetBase()+"-trend.svg")}
	cards := rep.Cards()
	pngs, err := rep.CardPNGs()
	if err != nil {
		return nil, err
	}
	for _, name := range outcomebench.CardNames() {
		for _, theme := range []string{"dark", "light"} {
			file := rep.CardFile(name, theme)
			if svg, ok := cards[file]; ok {
				p := filepath.Join(outDir, "assets", file)
				files[p] = []byte(svg)
				order = append(order, p)
			}
			if png, ok := pngs[rep.CardPNGFile(name, theme)]; ok {
				p := filepath.Join(outDir, "assets", rep.CardPNGFile(name, theme))
				files[p] = png
				order = append(order, p)
			}
		}
	}
	paths := make([]string, 0, len(files))
	for _, p := range order {
		if err := os.WriteFile(p, files[p], 0o644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", p, err)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func benchmarkSummary(rep outcomebench.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "outcomes %s: mediated %d merged, unmediated %d merged, %d PRs / %d repos / %d verdicts\n",
		rep.Month(), rep.Mediated.Merged, rep.Unmediated.Merged, rep.PRs, len(rep.Repos), rep.Verdicts)
	for _, f := range rep.Findings() {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	return b.String()
}
