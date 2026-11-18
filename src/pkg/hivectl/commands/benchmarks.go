package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// `hivectl benchmarks export` (hivecommons/hive#18990, hivecommons/benchmarks#1
// scope item 3, RFC #17694): the offline twin of GET /api/benchmarks/export.
// It runs the same converter (pkg/evidence/benchmarks.Convert) over the local
// evidence tree, so the records carry the same fields and the same privacy
// rules; only the inputs that need a live hive differ (see the command help).

const (
	// benchmarksHashLen is the length of a published hive hash in hex digits.
	benchmarksHashLen = 16
	// benchmarksDateLayout is the YYYY-MM-DD form --since and --until accept,
	// matching the HTTP exporter's ?since=.
	benchmarksDateLayout = "2006-01-02"
	// benchmarksMonthLayout names the month files under <hash>/.
	benchmarksMonthLayout = "2006-01"
	// benchmarksMonthDirPerm and benchmarksMonthFilePerm match the hub's
	// month store.
	benchmarksMonthDirPerm  = 0o755
	benchmarksMonthFilePerm = 0o644
)

var benchmarksHashRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// benchmarksNow is overridable in tests; it stamps published_at and anchors
// duration forms of --since and --until.
var benchmarksNow = func() time.Time { return time.Now().UTC() }

func newBenchmarksCommand(env *commandEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "benchmarks",
		Short:   "Benchmark records for hivecommons.dev/benchmarks: export them offline from local evidence",
		Long:    "Work with the per-PR benchmark records Hive publishes to https://hivecommons.dev/benchmarks/ (docs/benchmarks.md).\nexport reads the local evidence tree only; it does not contact a hive or the leaderboard.",
		Example: "  hivectl benchmarks export --hive-hash 0123456789abcdef --out records.json\n  hivectl benchmarks export --hive-id my-hive --by-month ./data/fleet --public-repo my-org/my-repo",
	}
	cmd.AddCommand(newBenchmarksExportCommand(env))
	return cmd
}

func newBenchmarksExportCommand(_ *commandEnv) *cobra.Command {
	var root, hiveID, hiveHash, hiveVersion, runtime, since, until, out, byMonth string
	var publicRepos []string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Convert local evidence bundles into anonymized benchmark records (offline GET /api/benchmarks/export)",
		Long: "Convert the schema v1 evidence bundles under --root (<root>/<org>/<repo>/<pr>/<sha>.json) into the records\n" +
			"the leaderboard publishes: one author record and one reviewer record per PR, validating against\n" +
			"hivecommons/benchmarks schema/result.schema.json. It runs the same converter as the spoke's\n" +
			"GET /api/benchmarks/export, so an owner can publish by hand from a copy of /data/evidence.\n\n" +
			"The hive is identified only by its salted hash. Pass --hive-id (hashed locally, never written out) or\n" +
			"--hive-hash (the 16 hex digits Settings > Benchmarks and /api/status show); exactly one is required.\n\n" +
			"Privacy is the HTTP exporter's, anonymized by default: a record never carries a login, PR number, commit SHA,\n" +
			"prompt, finding text, file path or code. task.repo and source.org appear only for repositories you list with\n" +
			"--public-repo owner/name (repeatable); offline there is no GitHub client to look visibility up, so list\n" +
			"only repositories that are public. Every other repository stays anonymous.\n\n" +
			"Differences from the HTTP exporter: bundle signatures are not verified (like `evidence export`); cost.basis is\n" +
			"\"unknown\" because the launch-cost store lives on the running hive; sandbox_runtime comes from the bundle\n" +
			"stamp and falls back to --sandbox-runtime for bundles that predate the stamp; hive_version is --hive-version.\n\n" +
			"--since and --until take an RFC3339 time, YYYY-MM-DD, or (since/until back from now) a Go duration such as\n" +
			"72h, and window on the evidence bundle's update time, as the hourly push does. Output is a JSON array sorted by\n" +
			"record id, on stdout or --out. --by-month DIR instead writes DIR/<hash>/<YYYY-MM>.json (the data/fleet layout\n" +
			"of hivecommons/benchmarks), merging by record id into any file already there, so the files can be committed\n" +
			"as a pull request to that repository.",
		Example: "  hivectl benchmarks export --root ./evidence --hive-hash 0123456789abcdef --out records.json\n" +
			"  hivectl benchmarks export --hive-id my-hive --since 2026-11-01 --by-month ./data/fleet\n" +
			"  hivectl benchmarks export --hive-id my-hive --public-repo my-org/app --public-repo my-org/docs",
		Args: argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			hiveID = strings.TrimSpace(hiveID)
			hiveHash = strings.ToLower(strings.TrimSpace(hiveHash))
			switch {
			case hiveID != "" && hiveHash != "":
				return &usageError{message: "--hive-id and --hive-hash are mutually exclusive"}
			case hiveID == "" && hiveHash == "":
				return &usageError{message: "one of --hive-id or --hive-hash is required"}
			case hiveHash != "" && !benchmarksHashRe.MatchString(hiveHash):
				return &usageError{message: fmt.Sprintf("invalid --hive-hash %q; expected %d lowercase hex digits", hiveHash, benchmarksHashLen)}
			}
			if byMonth != "" && out != "" {
				return &usageError{message: "--by-month and --out are mutually exclusive"}
			}
			now := benchmarksNow()
			var sinceT, untilT time.Time
			var err error
			if sinceT, err = parseBenchmarksTime("--since", since, now); err != nil {
				return &usageError{message: err.Error()}
			}
			if untilT, err = parseBenchmarksTime("--until", until, now); err != nil {
				return &usageError{message: err.Error()}
			}
			if !sinceT.IsZero() && !untilT.IsZero() && !untilT.After(sinceT) {
				return &usageError{message: "--until must be after --since"}
			}
			public := map[string]bool{}
			for _, r := range publicRepos {
				r = strings.TrimSpace(r)
				if strings.Count(r, "/") != 1 || strings.HasPrefix(r, "/") || strings.HasSuffix(r, "/") {
					return &usageError{message: fmt.Sprintf("invalid --public-repo %q; expected owner/name", r)}
				}
				public[strings.ToLower(r)] = true
			}

			bundles, unreadable, err := readEvidenceBundles(root)
			if err != nil {
				return err
			}
			if unreadable > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %d unreadable or non-v1 bundle(s) under %s\n", unreadable, root)
			}
			recs := benchmarks.Convert(bundles, benchmarks.Options{
				HiveID:         hiveID,
				HiveHash:       hiveHash,
				HiveVersion:    hiveVersion,
				Since:          sinceT,
				Now:            now,
				RepoConsent:    len(public) > 0,
				RepoPublic:     func(repo string) bool { return public[strings.ToLower(repo)] },
				SandboxRuntime: func(string) string { return runtime },
			})
			if !untilT.IsZero() {
				kept := recs[:0]
				for _, r := range recs {
					if !r.UpdatedAt.After(untilT) {
						kept = append(kept, r)
					}
				}
				recs = kept
			}

			if byMonth != "" {
				paths, err := writeBenchmarkMonths(byMonth, recs)
				if err != nil {
					return err
				}
				for _, p := range paths {
					fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", p)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "exported %d record(s) from %d bundle(s) into %d file(s)\n", len(recs), len(bundles), len(paths))
				return nil
			}
			data, err := marshalBenchmarkRecords(recs)
			if err != nil {
				return err
			}
			if out == "" || out == "-" {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(out, data, benchmarksMonthFilePerm); err != nil {
				return fmt.Errorf("writing %s: %w", out, err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d record(s) from %d bundle(s) to %s\n", len(recs), len(bundles), out)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", ghpkg.ReviewEvidenceRoot, "evidence root holding <org>/<repo>/<pr>/<sha>.json bundles")
	cmd.Flags().StringVar(&hiveID, "hive-id", "", "this hive's id (hive_id in hive.yaml); hashed locally, never written out")
	cmd.Flags().StringVar(&hiveHash, "hive-hash", "", "this hive's published hash (16 hex digits, from Settings > Benchmarks); alternative to --hive-id")
	cmd.Flags().StringVar(&hiveVersion, "hive-version", "", "source.hive_version to stamp (default \"unknown\")")
	cmd.Flags().StringVar(&runtime, "sandbox-runtime", benchmarks.SandboxRuntimeNone, "sandbox_runtime for bundles that predate the runtime stamp")
	cmd.Flags().StringSliceVar(&publicRepos, "public-repo", nil, "owner/name of a public repository that may be named in records (repeatable); all others stay anonymous")
	cmd.Flags().StringVar(&since, "since", "", "only bundles updated after this RFC3339 time, YYYY-MM-DD, or within this Go duration (e.g. 72h)")
	cmd.Flags().StringVar(&until, "until", "", "only bundles updated at or before this RFC3339 time, YYYY-MM-DD, or Go duration back from now")
	cmd.Flags().StringVar(&out, "out", "", "write the JSON array to this file instead of stdout (\"-\" is stdout)")
	cmd.Flags().StringVar(&byMonth, "by-month", "", "write DIR/<hash>/<YYYY-MM>.json month files (data/fleet layout), merging by record id, instead of one array")
	return cmd
}

// parseBenchmarksTime accepts RFC3339, YYYY-MM-DD, or a Go duration back from
// now. Empty means unset.
func parseBenchmarksTime(flag, s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(benchmarksDateLayout, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("invalid %s %q; expected an RFC3339 time, YYYY-MM-DD, or a Go duration such as 72h", flag, s)
	}
	return now.Add(-d), nil
}

// marshalBenchmarkRecords renders recs as one JSON array plus a newline; an
// empty set is [] rather than null.
func marshalBenchmarkRecords(recs []benchmarks.Record) ([]byte, error) {
	if recs == nil {
		recs = []benchmarks.Record{}
	}
	data, err := json.Marshal(recs)
	if err != nil {
		return nil, fmt.Errorf("encoding records: %w", err)
	}
	return append(data, '\n'), nil
}

// benchmarkMonthOf files a record under the month it merged, else opened,
// else was published: the rule the hub's month store applies.
func benchmarkMonthOf(r benchmarks.Record, now time.Time) string {
	for _, ts := range []string{r.Outcome.MergedAt, r.Outcome.OpenedAt, r.PublishedAt} {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t.UTC().Format(benchmarksMonthLayout)
		}
	}
	return now.UTC().Format(benchmarksMonthLayout)
}

// writeBenchmarkMonths writes recs to dir/<hash>/<YYYY-MM>.json, merging by
// record id into a file already present (new records win), and returns the
// paths written in sorted order. An existing file that is not a JSON array of
// records is an error: it is never overwritten.
func writeBenchmarkMonths(dir string, recs []benchmarks.Record) ([]string, error) {
	type key struct{ hash, month string }
	groups := map[key][]benchmarks.Record{}
	now := benchmarksNow()
	for _, r := range recs {
		k := key{hash: r.Source.HiveID, month: benchmarkMonthOf(r, now)}
		groups[k] = append(groups[k], r)
	}
	var paths []string
	for k, add := range groups {
		path := filepath.Join(dir, k.hash, k.month+".json")
		byID := map[string]benchmarks.Record{}
		if data, err := os.ReadFile(path); err == nil {
			var cur []benchmarks.Record
			if err := json.Unmarshal(data, &cur); err != nil {
				return nil, fmt.Errorf("%s exists but is not a JSON array of records: %w", path, err)
			}
			for _, r := range cur {
				byID[r.ID] = r
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		for _, r := range add {
			byID[r.ID] = r
		}
		merged := make([]benchmarks.Record, 0, len(byID))
		for _, r := range byID {
			merged = append(merged, r)
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
		data, err := marshalBenchmarkRecords(merged)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), benchmarksMonthDirPerm); err != nil {
			return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, data, benchmarksMonthFilePerm); err != nil {
			return nil, fmt.Errorf("writing %s: %w", path, err)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}
