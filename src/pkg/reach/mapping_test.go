package reach

import (
	"math"
	"reflect"
	"testing"
)

// TestMapFile pins the D3 mapping rules of #3994 exactly: src/pkg/<name>/**
// → <name>, src/cmd/hive/** → main, src/proxy/** → proxy, everything else →
// the unattributable bucket — plus the pre-#3996 v2/... legacy alias of each
// rule, because merged-PR file lists are immutable history.
func TestMapFile(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"src/pkg/governor/eval.go", "governor"},
		{"src/pkg/hub/saas.go", "hub"},
		{"src/pkg/tracing/tracing.go", "tracing"},
		{"src/pkg/reach/deeply/nested/file.go", "reach"},
		{"src/cmd/hive/main.go", ComponentMain},
		{"src/cmd/hive/sub/dir/file.go", ComponentMain},
		{"src/proxy/mitm.go", ComponentProxy},
		{"src/proxy/certs/ca.go", ComponentProxy},
		// Legacy pre-#3996 spellings: PRs merged before the v2/→src/ rename
		// report these paths from the GitHub API forever.
		{"v2/pkg/governor/eval.go", "governor"},
		{"v2/pkg/hub/saas.go", "hub"},
		{"v2/pkg/reach/deeply/nested/file.go", "reach"},
		{"v2/cmd/hive/main.go", ComponentMain},
		{"v2/cmd/hive/sub/dir/file.go", ComponentMain},
		{"v2/proxy/mitm.go", ComponentProxy},
		{"v2/proxy/certs/ca.go", ComponentProxy},
		// Unattributable: workflows, deploy, docs, scripts, bin, misc.
		{".github/workflows/ci.yml", ComponentUnattributable},
		{"deploy/hub/deployment.yaml", ComponentUnattributable},
		{"src/docs/design/pr-reach-telemetry.md", ComponentUnattributable},
		{"v2/docs/design/pr-reach-telemetry.md", ComponentUnattributable},
		{"scripts/release.sh", ComponentUnattributable},
		{"bin/agent-launch.sh", ComponentUnattributable},
		{"README.md", ComponentUnattributable},
		{"Dockerfile", ComponentUnattributable},
		// A file directly under src/pkg/ (or legacy v2/pkg/) belongs to no
		// package.
		{"src/pkg/orphan.go", ComponentUnattributable},
		{"v2/pkg/orphan.go", ComponentUnattributable},
		// Prefix traps: names that merely start like the mapped prefixes.
		{"src/pkgnot/file.go", ComponentUnattributable},
		{"src/cmd/hivectl/main.go", ComponentUnattributable},
		{"src/proxyfoo/file.go", ComponentUnattributable},
		{"v2/pkgnot/file.go", ComponentUnattributable},
		{"v2/cmd/hivectl/main.go", ComponentUnattributable},
		{"v2/proxyfoo/file.go", ComponentUnattributable},
		{"other/src/pkg/hub/saas.go", ComponentUnattributable},
		{"other/v2/pkg/hub/saas.go", ComponentUnattributable},
	}
	for _, tc := range cases {
		if got := MapFile(tc.path); got != tc.want {
			t.Errorf("MapFile(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestAttribute covers de-duplication, sorting, the explicit unattributable
// share, and the coverage fraction — the D3 requirement that the
// unattributable share is never silently dropped.
func TestAttribute(t *testing.T) {
	files := []string{
		"src/pkg/hub/saas.go",
		"src/pkg/hub/heartbeat.go", // duplicate component
		"src/pkg/tracing/tracing.go",
		"src/cmd/hive/main.go",
		".github/workflows/ci.yml",
		"deploy/hub/deployment.yaml",
	}
	attr := Attribute(files)

	if want := []string{"hub", ComponentMain, "tracing"}; !reflect.DeepEqual(attr.Components, want) {
		t.Errorf("Components = %v, want %v", attr.Components, want)
	}
	if want := []string{".github/workflows/ci.yml", "deploy/hub/deployment.yaml"}; !reflect.DeepEqual(attr.UnattributableFiles, want) {
		t.Errorf("UnattributableFiles = %v, want %v", attr.UnattributableFiles, want)
	}
	if attr.FilesTotal != 6 || attr.FilesAttributed != 4 {
		t.Errorf("FilesTotal=%d FilesAttributed=%d, want 6 and 4", attr.FilesTotal, attr.FilesAttributed)
	}
	if want := 4.0 / 6.0; math.Abs(attr.Coverage-want) > 1e-9 {
		t.Errorf("Coverage = %v, want %v", attr.Coverage, want)
	}
}

// TestAttributeFullyUnattributable: a docs-only PR must report zero coverage
// with every file accounted for — not an empty result.
func TestAttributeFullyUnattributable(t *testing.T) {
	attr := Attribute([]string{"README.md", "docs/guide.md"})
	if len(attr.Components) != 0 {
		t.Errorf("Components = %v, want none", attr.Components)
	}
	if len(attr.UnattributableFiles) != 2 {
		t.Errorf("UnattributableFiles = %v, want both files", attr.UnattributableFiles)
	}
	if attr.Coverage != 0 {
		t.Errorf("Coverage = %v, want 0", attr.Coverage)
	}
}

// TestAttributeEmpty: no changed files → zero coverage, no divide-by-zero.
func TestAttributeEmpty(t *testing.T) {
	attr := Attribute(nil)
	if attr.Coverage != 0 || attr.FilesTotal != 0 || attr.FilesAttributed != 0 {
		t.Errorf("Attribute(nil) = %+v, want zero-valued counts", attr)
	}
}
