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
	"github.com/hivecommons/hive/pkg/outcomebench"
)

// benchmarksExportNow is overridable in tests.
var benchmarksExportNow = func() time.Time { return time.Now().UTC() }

var benchmarksHashRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func newBenchmarksCommand(env *commandEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "benchmarks",
		Short: "Offline helpers for the public benchmarks leaderboard",
		Long:  "Work with the records published at https://hivecommons.dev/benchmarks/ without a running hive.",
	}
	cmd.AddCommand(newBenchmarksExportCommand(env))
	return cmd
}

func newBenchmarksExportCommand(_ *commandEnv) *cobra.Command {
	var root, hiveID, hiveHash, since, until, out, byMonth string
	var publicRepos []string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Convert local evidence bundles into benchmark schema records, offline",
		Long: "Read the schema v1 evidence bundles under --root and convert them with the same converter and record schema as\n" +
			"GET /api/benchmarks/export, so an owner can publish by hand. No network and no hive session.\n\n" +
			"The hive is identified only by its salted hash: --hive-id (hashed locally) or --hive-hash (the 16 hex shown in\n" +
			"Settings > Benchmarks). One is required. Records are anonymized by default: task.repo and source.org appear only\n" +
			"for repos named with --public-repo owner/name; otherwise every repo is anonymous. No login, PR number, SHA,\n" +
			"prompt, finding text or path is ever written. Cost stays \"unknown\" because there is no launch-cost store offline.\n\n" +
			"--since and --until bound the evidence time of a record (RFC3339, YYYY-MM-DD, or for --since a Go duration such\n" +
			"as 720h); --since is exclusive and --until is exclusive. Output is a JSON array on stdout or --out. --by-month DIR\n" +
			"instead writes DIR/<hash>/<YYYY-MM>.json (the data/fleet/ layout of hivecommons/benchmarks), one file per month of\n" +
			"the evidence time.",
		Example: "  hivectl benchmarks export --hive-hash 0123456789abcdef --out records.json\n" +
			"  hivectl benchmarks export --hive-id my-hive --public-repo hivecommons/hive --since 2026-10-01\n" +
			"  hivectl benchmarks export --hive-hash 0123456789abcdef --by-month ./benchmarks/data/fleet",
		Args: argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			hiveID, hiveHash = strings.TrimSpace(hiveID), strings.ToLower(strings.TrimSpace(hiveHash))
			if (hiveID == "") == (hiveHash == "") {
				return &usageError{message: "exactly one of --hive-id or --hive-hash is required"}
			}
			if hiveHash != "" && !benchmarksHashRe.MatchString(hiveHash) {
				return &usageError{message: fmt.Sprintf("invalid --hive-hash %q; expected 16 lowercase hex characters", hiveHash)}
			}
			if out != "" && byMonth != "" {
				return &usageError{message: "--out and --by-month are mutually exclusive"}
			}
			now := benchmarksExportNow()
			sinceT, err := parseBenchmarksTime("--since", since, now, true)
			if err != nil {
				return &usageError{message: err.Error()}
			}
			untilT, err := parseBenchmarksTime("--until", until, now, false)
			if err != nil {
				return &usageError{message: err.Error()}
			}
			public := map[string]bool{}
			for _, r := range publicRepos {
				r = strings.TrimSpace(r)
				if strings.Count(r, "/") != 1 || strings.HasPrefix(r, "/") || strings.HasSuffix(r, "/") {
					return &usageError{message: fmt.Sprintf("invalid --public-repo %q; expected owner/name", r)}
				}
				public[strings.ToLower(r)] = true
			}

			bundles, unreadable, err := outcomebench.Load(root)
			if err != nil {
				return err
			}
			if unreadable > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %d unreadable or non-v1 bundle(s) under %s\n", unreadable, root)
			}
			opts := benchmarks.Options{
				HiveID:         hiveID,
				HiveHash:       hiveHash,
				HiveVersion:    "unknown",
				Since:          sinceT,
				Now:            now,
				RepoConsent:    true,
				RepoPublic:     func(repo string) bool { return public[strings.ToLower(repo)] },
				SandboxRuntime: nil,
				CostLookup:     nil,
			}
			all := benchmarks.Convert(bundles, opts)
			recs := make([]benchmarks.Record, 0, len(all))
			for _, r := range all {
				if !untilT.IsZero() && !r.UpdatedAt.Before(untilT) {
					continue
				}
				recs = append(recs, r)
			}

			if byMonth != "" {
				paths, err := writeBenchmarksByMonth(byMonth, recs)
				if err != nil {
					return err
				}
				for _, p := range paths {
					fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", p)
				}
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
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", out, err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d record(s) to %s\n", len(recs), out)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", ghpkg.ReviewEvidenceRoot, "evidence root holding <org>/<repo>/<pr>/<sha>.json bundles")
	cmd.Flags().StringVar(&hiveID, "hive-id", "", "hive id, hashed locally into the published hive hash")
	cmd.Flags().StringVar(&hiveHash, "hive-hash", "", "published hive hash (16 hex, Settings > Benchmarks)")
	cmd.Flags().StringArrayVar(&publicRepos, "public-repo", nil, "owner/name of a public repo whose name may appear in records (repeatable); all others stay anonymous")
	cmd.Flags().StringVar(&since, "since", "", "only records with evidence newer than this RFC3339 time, YYYY-MM-DD date or Go duration (e.g. 720h)")
	cmd.Flags().StringVar(&until, "until", "", "only records with evidence older than this RFC3339 time or YYYY-MM-DD date")
	cmd.Flags().StringVar(&out, "out", "", "write the JSON array to this file instead of stdout (\"-\" is stdout)")
	cmd.Flags().StringVar(&byMonth, "by-month", "", "write DIR/<hash>/<YYYY-MM>.json files instead of one array")
	return cmd
}

// parseBenchmarksTime accepts RFC3339 and YYYY-MM-DD, and a Go duration back
// from now when allowDuration. Empty means unbounded.
func parseBenchmarksTime(flag, s string, now time.Time, allowDuration bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if allowDuration {
		if d, err := time.ParseDuration(s); err == nil && d >= 0 {
			return now.Add(-d), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid %s %q; expected an RFC3339 time or YYYY-MM-DD date", flag, s)
}

func marshalBenchmarkRecords(recs []benchmarks.Record) ([]byte, error) {
	if recs == nil {
		recs = []benchmarks.Record{}
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// writeBenchmarksByMonth writes dir/<hash>/<YYYY-MM>.json, grouping records
// by the UTC month of their evidence time. Returns the paths written.
func writeBenchmarksByMonth(dir string, recs []benchmarks.Record) ([]string, error) {
	groups := map[string][]benchmarks.Record{}
	for _, r := range recs {
		m := r.UpdatedAt.UTC().Format("2006-01")
		groups[m] = append(groups[m], r)
	}
	months := make([]string, 0, len(groups))
	for m := range groups {
		months = append(months, m)
	}
	sort.Strings(months)
	var paths []string
	for _, m := range months {
		hash := groups[m][0].Source.HiveID
		target := filepath.Join(dir, hash, m+".json")
		data, err := marshalBenchmarkRecords(groups[m])
		if err != nil {
			return paths, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return paths, fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return paths, fmt.Errorf("writing %s: %w", target, err)
		}
		paths = append(paths, target)
	}
	return paths, nil
}
