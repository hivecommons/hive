package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardInternalImportCountRatchet(t *testing.T) {
	// 41 = the v5 baseline of 34 plus pkg/fleetreport and pkg/hiveadvisor,
	// which the v4 fleet self-report and hive-owner-advice features import,
	// plus pkg/scheduler for the kick_template provenance type the
	// SchedulerControl seam now carries (hivecommons/hive#7390), plus
	// pkg/review for the perspective-set config API that lets the hive
	// choose its review perspectives (hivecommons/hive#7717), plus
	// pkg/ioscan for the design-gate plan handlers (RFC #7993), plus
	// pkg/celtrigger for run-stage handoff events (hivecommons/hive#8298), plus
	// pkg/retro for autonomy signal facts and automatic ACMM decisions (#8364).
	const maxDashboardInternalImports = 41

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dashboard package: %v", err)
	}
	importRe := regexp.MustCompile(`"github\.com/hivecommons/hive/pkg/([^"]+)"`)
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
