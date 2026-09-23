package retro

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/findingidentity"
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

func TestOpenDuplicateUsesFindingIdentityBeforeFallback(t *testing.T) {
	advisory := newStore(t, "advisory-finding-key")
	l := &Lane{advisoryStore: advisory}
	existing := Finding{
		Title:         "Retro: guard missing",
		SubjectDigest: "sha256:abc",
		Predicate:     "hive.audit.missing-guard/v1",
		Location:      "src/guard.go:12",
	}
	created, err := advisory.Create(existing.Title, beads.TypeAdvisory, beads.PriorityMedium, "retro", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := advisory.SetMetadata(created.ID, findingidentity.MetaKey, retroFindingKey(existing)); err != nil {
		t.Fatal(err)
	}
	sameFinding := Finding{
		Title:         "Retro: guard missing, line moved",
		SubjectDigest: "sha256:abc",
		Predicate:     "hive.audit.missing-guard/v1",
		Location:      "src/guard.go:99",
	}
	if !l.openDuplicateFinding(sameFinding) {
		t.Fatal("same subject/predicate/normalized location must count as duplicate")
	}
	differentPredicate := sameFinding
	differentPredicate.Title = existing.Title
	differentPredicate.Predicate = "hive.audit.bad-retry/v1"
	if l.openDuplicateFinding(differentPredicate) {
		t.Fatal("same title with different predicate must not use substring fallback")
	}
}
