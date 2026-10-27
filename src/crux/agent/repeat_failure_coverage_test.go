package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// TestRepeatFailureCoverageListsEveryBackend is C13/C-AC11: the code table has
// exactly one row for every config.CLIBackends and config.InferenceBackends
// entry, so removing a backend from it fails here.
func TestRepeatFailureCoverageListsEveryBackend(t *testing.T) {
	rows := map[string]RepeatFailureCoverage{}
	for _, row := range RepeatFailureCoverageTable() {
		if _, dup := rows[row.Backend]; dup {
			t.Errorf("backend %q has more than one coverage row", row.Backend)
		}
		rows[row.Backend] = row
	}
	want := append(append([]string(nil), config.CLIBackends...), config.InferenceBackends...)
	for _, backend := range want {
		if _, ok := rows[backend]; !ok {
			t.Errorf("repeat-failure coverage table is missing backend %q", backend)
		}
	}
	for backend := range rows {
		if !slices.Contains(want, backend) {
			t.Errorf("coverage row %q is neither a config.CLIBackends nor a config.InferenceBackends entry", backend)
		}
	}
}

// TestRepeatFailureCoverageRowsAreHonest is C4/C5: a covered row carries a
// route, committed harness evidence with the CLI's --version and a
// shell-activity signal (or "safeguard unavailable"); a row that is not
// covered carries a reason and a follow-up issue.
func TestRepeatFailureCoverageRowsAreHonest(t *testing.T) {
	for _, row := range RepeatFailureCoverageTable() {
		if row.FollowUp < 0 {
			t.Errorf("%s: negative follow-up issue", row.Backend)
		}
		if strings.TrimSpace(row.Route) == "" {
			t.Errorf("%s: route is empty", row.Backend)
		}
		if strings.Contains(row.Route+row.Reason+row.Evidence+row.ShellActivity, "|") {
			t.Errorf("%s: a cell contains '|', which breaks the docs table", row.Backend)
		}
		if !row.Covered {
			if strings.TrimSpace(row.Reason) == "" || row.FollowUp == 0 {
				t.Errorf("%s: not covered needs a reason and a follow-up issue (C5)", row.Backend)
			}
			if row.Evidence != "" || row.EvidencePath != "" || row.ShellActivity != "" {
				t.Errorf("%s: not covered must not carry evidence or a safeguard signal", row.Backend)
			}
			if RepeatFailureBackendCovered(row.Backend) {
				t.Errorf("%s: RepeatFailureBackendCovered disagrees with the table", row.Backend)
			}
			continue
		}
		if row.Reason != "" {
			t.Errorf("%s: a covered row must not carry a not-covered reason", row.Backend)
		}
		if !strings.Contains(row.Evidence, "--version") {
			t.Errorf("%s: covered evidence must include the CLI's --version output (C4)", row.Backend)
		}
		if row.EvidencePath == "" {
			t.Errorf("%s: covered without committed real-run harness evidence (C4)", row.Backend)
		} else if _, err := os.Stat(filepath.Join("..", "..", row.EvidencePath)); err != nil {
			t.Errorf("%s: evidence %s is not committed: %v", row.Backend, row.EvidencePath, err)
		}
		if strings.TrimSpace(row.ShellActivity) == "" {
			t.Errorf("%s: covered row must name its shell-activity signal or %q (C12)", row.Backend, repeatFailureSafeguardUnavailable)
		}
		if !RepeatFailureBackendCovered(row.Backend) {
			t.Errorf("%s: RepeatFailureBackendCovered disagrees with the table", row.Backend)
		}
	}
	if RepeatFailureBackendCovered("no-such-backend") {
		t.Error("an unknown backend must not be covered")
	}
}

// TestRepeatFailureCoverageDocsMatchCode is C13/C-AC11: the repeated-failure
// table in docs/backend-support-tiers.md is exactly the rendered code table,
// so a row changed only in the docs fails here.
func TestRepeatFailureCoverageDocsMatchCode(t *testing.T) {
	raw, err := os.ReadFile("../../docs/backend-support-tiers.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	const heading = "## Repeated-failure check"
	start := strings.Index(doc, heading)
	if start < 0 {
		t.Fatalf("backend-support-tiers.md has no %q section", heading)
	}
	section := doc[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	var got []string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "| `") {
			got = append(got, line)
		}
	}
	want := RepeatFailureCoverageMarkdownRows()
	if !slices.Equal(got, want) {
		t.Errorf("docs repeated-failure table differs from the code table (pkg/agent/repeat_failure_coverage.go).\nwant rows:\n%s\n\ngot rows:\n%s",
			strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}
