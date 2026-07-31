package spokebackup

import (
	"strings"
	"testing"
)

// The overlay (/data/hive.yaml.dashboard) is the layer that wins the boot-time
// merge, so a backup omitting it captures a post-merge snapshot while missing
// the file that actually carries the hive's configuration. A restore from such
// an archive produces a differently-configured spoke, with no error at backup
// time and no symptom until the owner notices their settings reverted.
//
// Pre-existing tests cover .bak. These cover the overlay, and the relationship
// between the two.

// TestOverlayIsCaptured is the regression. Before this change, spokeFiles and
// includedRootFiles listed only hive.yaml.bak.
func TestOverlayIsCaptured(t *testing.T) {
	files, _, _ := buildTestBackup(t)

	got, ok := files["spoke/hive.yaml.dashboard"]
	if !ok {
		t.Fatal("hive.yaml.dashboard missing from archive — the overlay wins the " +
			"boot merge, so omitting it makes the backup unrestorable")
	}
	if !strings.Contains(got, "wins-the-merge") {
		t.Errorf("overlay content wrong: %q", got)
	}
}

// TestBakStillCapturedAlongsideOverlay guards the other direction: .bak serves
// the disaster fallback (ConfigMap missing/empty) and is the boot source on
// the variant-A/B hives, so adding the overlay must not displace it.
func TestBakStillCapturedAlongsideOverlay(t *testing.T) {
	files, _, _ := buildTestBackup(t)

	bak, ok := files["spoke/hive.yaml.bak"]
	if !ok {
		t.Fatal("hive.yaml.bak missing from archive — it is the disaster-fallback " +
			"source and the boot source on variant-A/B hives")
	}
	if !strings.Contains(bak, "post-merge-snapshot") {
		t.Errorf(".bak content wrong: %q", bak)
	}
}

// TestOverlayAndBakAreBothCapturedWhenTheyDiffer reflects the live case: on
// projectbluefin-knuckle the two files differ (14609 B vs 14547 B) because the
// overlay is written secret-free while .bak retains the merged auth_token.
// Capturing only one loses information the other holds.
func TestOverlayAndBakAreBothCapturedWhenTheyDiffer(t *testing.T) {
	files, _, _ := buildTestBackup(t)

	overlay, okOverlay := files["spoke/hive.yaml.dashboard"]
	bak, okBak := files["spoke/hive.yaml.bak"]
	if !okOverlay || !okBak {
		t.Fatalf("both config files must be captured; overlay=%v bak=%v", okOverlay, okBak)
	}
	if overlay == bak {
		t.Fatal("fixture is not exercising the divergent case; the two files are equal")
	}
}

// TestPlainHiveYamlStillExcluded: the PVC's hive.yaml is regenerated from seed
// + overlay on every boot and was observed stale on a production spoke.
// Capturing it would invite restoring the wrong file.
func TestPlainHiveYamlStillExcluded(t *testing.T) {
	files, _, _ := buildTestBackup(t)

	if _, ok := files["spoke/hive.yaml"]; ok {
		t.Error("hive.yaml was captured; it is regenerated on every boot and was " +
			"observed stale on a live spoke")
	}
}
