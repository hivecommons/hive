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

// Every scanner policy that can comment through the relay must tell the agent
// to park an existing issue whose remaining step is owner-only, with the
// command that actually works: the label relay refuses needs-human and
// needs-decision from agents (hivecommons/hive#12329).
func TestScannerPoliciesParkOwnerOnlyHandOffs(t *testing.T) {
	t.Parallel()

	rule := []byte("**Park an existing issue when only the owner can finish it.**")
	command := []byte("`hive-open-issue comment --repo <owner/repo> <number> --body-file <file> --needs-decision`")

	entries, err := fs.ReadDir(DefaultPolicies, "defaults")
	if err != nil {
		t.Fatalf("read embedded defaults: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "scanner-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := DefaultPolicies.ReadFile(path.Join("defaults", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		checked++
		if !bytes.Contains(data, []byte("hive-open-issue")) {
			// Advisory scanners cannot comment; they record the step instead.
			if !bytes.Contains(data, []byte("**Owner-only steps.**")) {
				t.Errorf("%s has no owner-only rule", e.Name())
			}
			continue
		}
		if !bytes.Contains(data, rule) || !bytes.Contains(data, command) {
			t.Errorf("%s lacks the owner-only park rule naming %s", e.Name(), command)
		}
	}
	if checked == 0 {
		t.Fatal("no scanner policies found; the guard would pass vacuously")
	}
}
