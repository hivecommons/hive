package policies

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"testing"
)

// Every ci-maintainer variant carries the merge-hotspot guidance so a
// `refactor: … merge hotspot` issue is handled as a mechanical split and the
// lane knows where the report lives.
func TestCIMaintainerPoliciesCarryMergeHotspotGuidance(t *testing.T) {
	t.Parallel()
	names := []string{"ci-maintainer.md", "ci-maintainer-full.md", "ci-maintainer-holdgated.md", "ci-maintainer-advisory.md"}
	required := [][]byte{
		[]byte("## Merge Hotspots"),
		[]byte("GET /api/hotspots"),
		[]byte("hivectl governor hotspots"),
		[]byte("<!-- hive:hotspot file=<path> -->"),
		[]byte("mechanical split"),
		[]byte("Do not splinter"),
	}
	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			embedded, err := DefaultPolicies.ReadFile(path.Join("defaults", name))
			if err != nil {
				t.Fatalf("read embedded policy: %v", err)
			}
			source, err := os.ReadFile(filepath.Join("..", "..", "policies", name))
			if err != nil {
				t.Fatalf("read source policy: %v", err)
			}
			if !bytes.Equal(embedded, source) {
				t.Errorf("embedded and source copies of %s differ", name)
			}
			for _, marker := range required {
				if !bytes.Contains(embedded, marker) {
					t.Errorf("%s is missing merge-hotspot guidance %q", name, marker)
				}
			}
		})
	}
}
