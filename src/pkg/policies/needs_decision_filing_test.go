package policies

import (
	"bytes"
	"io/fs"
	"path"
	"strings"
	"testing"
)

// Every shipped policy that files issues must tell the agent to park an issue
// that asks the maintainer to choose, using the relay's --needs-decision flag
// rather than naming the label (hivecommons/hive#11215). The wording is the
// same in every lane so no lane's parking depends on its own phrasing.
func TestIssueFilingPoliciesParkDecisionIssues(t *testing.T) {
	t.Parallel()

	rule := []byte("**Park an issue that needs the maintainer's call.** If the issue body asks the\n" +
		"maintainer to choose between options, or to approve before work can start, add\n" +
		"`--needs-decision` to the issue-create command below")

	entries, err := fs.ReadDir(DefaultPolicies, "defaults")
	if err != nil {
		t.Fatalf("read embedded defaults: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := DefaultPolicies.ReadFile(path.Join("defaults", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if !bytes.Contains(data, []byte("\ngh issue create")) && !bytes.Contains(data, []byte("hive-open-issue")) {
			continue
		}
		checked++
		if !bytes.Contains(data, rule) {
			t.Errorf("%s files issues but lacks the --needs-decision parking rule", e.Name())
		}
	}
	if checked == 0 {
		t.Fatal("no issue-filing policies found; the guard would pass vacuously")
	}
}
