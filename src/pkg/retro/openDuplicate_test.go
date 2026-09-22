package retro

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

func TestOpenDuplicateNormalizesTitleAndFileSet(t *testing.T) {
	advisory := newStore(t, "advisory-files")
	l := &Lane{advisoryStore: advisory}
	created, err := advisory.Create("Config parser drops explicit false", beads.TypeAdvisory, beads.PriorityMedium, "retro", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := advisory.SetMetadata(created.ID, "file_set", "src/config.go,src/config_test.go"); err != nil {
		t.Fatal(err)
	}
	if !l.openDuplicate("  config   parser DROPS explicit false ", []string{"src/config_test.go", "src/config.go"}) {
		t.Fatal("normalized title plus identical file set must count as duplicate")
	}
	if l.openDuplicate("config parser drops explicit false", []string{"src/other.go"}) {
		t.Fatal("same title with different file set must not count as duplicate")
	}
}
