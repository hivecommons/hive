package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardInternalImportCountRatchet(t *testing.T) {
	const maxDashboardInternalImports = 31

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dashboard package: %v", err)
	}
	importRe := regexp.MustCompile(`"github\.com/kubestellar/hive/pkg/([^"]+)"`)
	imports := map[string]struct{}{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, match := range importRe.FindAllStringSubmatch(string(data), -1) {
			pkg := match[1]
			if strings.HasPrefix(pkg, "dashboard/") {
				imports[pkg] = struct{}{}
				continue
			}
			if i := strings.IndexByte(pkg, '/'); i >= 0 {
				pkg = pkg[:i]
			}
			imports[pkg] = struct{}{}
		}
	}
	if len(imports) > maxDashboardInternalImports {
		t.Fatalf("pkg/dashboard imports %d internal packages, want <= %d", len(imports), maxDashboardInternalImports)
	}
}
