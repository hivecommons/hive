package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the workflow's actual package-selection pipeline with a synthetic
// go list result: future subpackages must reach the coverage gate even when
// they have no tests. Only the two function-sharded packages may be omitted.
func TestCoveragePartitionIncludesSubpackages(t *testing.T) {
	workflow := readRepoFile(t, ".github", "workflows", "v2-tests.yml")
	start := strings.Index(workflow, "go list ./pkg/... ./cmd/... ./internal/... ./crux/... \\\n")
	if start < 0 {
		t.Fatal("package-list pipeline not found")
	}
	const endMarker = "sort > rest-pkgs.txt"
	end := strings.Index(workflow[start:], endMarker)
	if end < 0 {
		t.Fatal("package-list output not found")
	}
	pipeline := workflow[start : start+end+len(endMarker)]
	const module = "github.com/hivecommons/hive/"
	packages := []string{
		"pkg/hub", "pkg/hub/spoke", "pkg/hub/future", "pkg/hub/future/nested",
		"pkg/hubbackup", "crux", "crux/agent", "crux/agent/future", "cmd/hive", "internal/testutil",
	}
	var input strings.Builder
	for _, pkg := range packages {
		input.WriteString(module + pkg + "\n")
	}
	// Replace only go list, leaving every filter and the sort from CI intact.
	cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", "go() { cat; }\n"+pipeline)
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader(input.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package selection: %v\n%s", err, output)
	}
	selected, err := os.ReadFile(filepath.Join(cmd.Dir, "rest-pkgs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	for _, pkg := range []string{
		"cmd/hive", "crux", "crux/agent/future", "internal/testutil", "pkg/hub/future",
		"pkg/hub/future/nested", "pkg/hub/spoke", "pkg/hubbackup",
	} {
		want += module + pkg + "\n"
	}
	if string(selected) != want {
		t.Fatalf("rest packages must include every package except pkg/hub and crux/agent\ngot:\n%s\nwant:\n%s", selected, want)
	}
}
