package policies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuideFlowLensPreservesAdvisoryBoundary(t *testing.T) {
	for _, name := range []string{"guide.md", "guide-advisory.md", "guide-issues.md", "guide-holdgated.md", "guide-full.md"} {
		for _, root := range []string{"defaults", filepath.Join("..", "..", "policies")} {
			t.Run(filepath.Join(root, name), func(t *testing.T) {
				body, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				for _, required := range []string{"## Surge-coach lens (flow health)", "HIVE_FLOW: clogged", "HIVE_FLOW: normal", "HIVE_FLOW: unknown", "This lens grants no extra write authority", "Never merge, edit thresholds/cadences", "reap only your own guide beads", "counts, denominators, age ranges, links", "clogged_by=", "held on purpose"} {
					if !strings.Contains(string(body), required) {
						t.Errorf("missing %q", required)
					}
				}
			})
		}
	}
}
