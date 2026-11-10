package github

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/sentinel"
)

// SensitivePathsForRepo replaces the hive-wide sensitive-path list for one
// repository's pull requests (project.repo_policies[].sentinel_sensitive_paths),
// tighter or looser, and an explicit empty list matches nothing. On v6 tip the
// option does not exist and the scan always uses Evaluator.SensitivePaths, so
// the "override drops requirements" and "explicit empty" cases are flagged and
// the "override adds requirements" case is not.
func TestSweepSentinelPerRepoSensitivePaths(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hiveWide []string
		perRepo  func(string) []string
		flagged  bool
	}{
		{name: "no override uses the hive-wide defaults", flagged: true},
		{
			name:    "override drops requirements for this repo",
			perRepo: func(string) []string { return []string{"OWNERS"} },
		},
		{
			name:    "explicit empty override matches nothing",
			perRepo: func(string) []string { return []string{} },
		},
		{
			name:     "override adds requirements the hive-wide list leaves out",
			hiveWide: []string{"OWNERS"},
			perRepo:  func(string) []string { return []string{"OWNERS", "**/requirements*.txt"} },
			flagged:  true,
		},
		{
			name:     "another repo's override leaves this repo on the hive-wide list",
			hiveWide: []string{"OWNERS"},
			perRepo: func(repo string) []string {
				if repo == "o/other" {
					return []string{"**/requirements*.txt"}
				}
				return []string{"OWNERS"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &sentinelFixture{
				prs: []map[string]any{sentinelPR(5, "mallory", "req111")},
				files: map[int][]map[string]any{5: {{
					"filename": "requirements.txt", "status": "modified", "additions": 1, "deletions": 1,
					"patch": "@@ -1 +1 @@\n-requests==2.31.0\n+requests==2.32.3\n",
				}}},
			}
			srv := newSentinelServer(t, f)
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"o/r"})
			opts := defaultSentinelOpts(nil)
			opts.Evaluator.SensitivePaths = tc.hiveWide
			var asked []string
			if tc.perRepo != nil {
				opts.SensitivePathsForRepo = func(repo string) []string {
					asked = append(asked, repo)
					return tc.perRepo(repo)
				}
			}
			res, err := c.SweepSentinel(context.Background(), opts)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if tc.perRepo != nil && (len(asked) != 1 || asked[0] != "o/r") {
				t.Fatalf("SensitivePathsForRepo asked for %v, want [o/r]", asked)
			}
			if !tc.flagged {
				if len(res.Flagged) != 0 || len(f.labels[5]) != 0 {
					t.Fatalf("want no finding: res=%+v labels=%v comments=%v", res, f.labels, f.comments)
				}
				return
			}
			if len(res.Flagged) != 1 {
				t.Fatalf("want one flagged PR: res=%+v", res)
			}
			if rules := res.Flagged[0].Rules(); len(rules) != 1 || rules[0] != sentinel.RuleSensitivePath {
				t.Fatalf("rules=%v, want only sensitive_path", rules)
			}
		})
	}
}
