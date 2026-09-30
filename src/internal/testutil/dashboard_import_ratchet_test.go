package testutil

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const dashboardImportPrefix = "github.com/hivecommons/hive/pkg/"

var (
	dashboardPackageDir      = filepath.Join("..", "..", "pkg", "dashboard")
	dashboardImportAllowlist = "dashboard_import_allowlist.txt"
)

// TestDashboardImportRatchet keeps pkg/dashboard's top-level package coupling
// from growing silently. It scans only non-test Go files in the pkg/dashboard
// package itself, collapses any github.com/hivecommons/hive/pkg/<x>/... import
// to github.com/hivecommons/hive/pkg/<x>, and compares that set with the
// allowlist checked in beside this test.
func TestDashboardImportRatchet(t *testing.T) {
	imports, filesScanned := dashboardTopLevelImports(t, dashboardPackageDir)
	if filesScanned == 0 {
		t.Fatalf("no non-test .go files found under %s — is the test running from src/internal/testutil?", dashboardPackageDir)
	}

	allowed := readDashboardImportAllowlist(t, dashboardImportAllowlist)
	added := sortedSetDifference(imports, allowed)
	stale := sortedSetDifference(allowed, imports)

	if len(added) > 0 || len(stale) > 0 {
		var b strings.Builder
		if len(added) > 0 {
			b.WriteString("pkg/dashboard gained dependencies not in internal/testutil/dashboard_import_allowlist.txt:\n")
			for _, path := range added {
				fmt.Fprintf(&b, "  - pkg/dashboard gained a new dependency on pkg/%s (%s) — route it through an existing seam or add it to the allowlist with justification in the PR.\n",
					strings.TrimPrefix(path, dashboardImportPrefix), path)
			}
		}
		if len(stale) > 0 {
			b.WriteString("internal/testutil/dashboard_import_allowlist.txt contains stale pkg/dashboard dependencies; remove:\n")
			for _, path := range stale {
				fmt.Fprintf(&b, "  - %s\n", path)
			}
		}
		t.Fatal(strings.TrimSpace(b.String()))
	}
}

func dashboardTopLevelImports(t *testing.T, dir string) (map[string]bool, int) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	imports := map[string]bool{}
	filesScanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		filesScanned++
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing imports in %s: %v", path, err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquoting import path %s in %s: %v", spec.Path.Value, path, err)
			}
			if top := dashboardTopLevelPackageImport(importPath); top != "" {
				imports[top] = true
			}
		}
	}
	return imports, filesScanned
}

func dashboardTopLevelPackageImport(importPath string) string {
	if !strings.HasPrefix(importPath, dashboardImportPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(importPath, dashboardImportPrefix)
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return ""
	}
	return dashboardImportPrefix + name
}

func readDashboardImportAllowlist(t *testing.T, path string) map[string]bool {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	allowed := map[string]bool{}
	var entries []string
	for i, raw := range strings.Split(string(data), "\n") {
		line := stripDashboardAllowlistComment(raw)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, dashboardImportPrefix) {
			t.Fatalf("%s:%d: allowlist entry %q must start with %s", path, i+1, line, dashboardImportPrefix)
		}
		if allowed[line] {
			t.Fatalf("%s:%d: duplicate allowlist entry %q", path, i+1, line)
		}
		allowed[line] = true
		entries = append(entries, line)
	}
	if !sort.StringsAreSorted(entries) {
		t.Fatalf("%s: allowlist entries must be sorted", path)
	}
	return allowed
}

func stripDashboardAllowlistComment(line string) string {
	line = strings.TrimSpace(line)
	for _, marker := range []string{"#", "//"} {
		if idx := strings.Index(line, marker); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
	}
	return line
}

func sortedSetDifference(left, right map[string]bool) []string {
	var diff []string
	for value := range left {
		if !right[value] {
			diff = append(diff, value)
		}
	}
	sort.Strings(diff)
	return diff
}
