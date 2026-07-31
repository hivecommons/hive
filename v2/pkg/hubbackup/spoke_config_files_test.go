package hubbackup

import (
	"strings"
	"testing"
)

// The hub-side spoke capture must take BOTH config files.
//
// hive.yaml.dashboard is the PVC overlay and wins the boot-time merge, so it
// carries the hive's real configuration. hive.yaml.runtime is the post-merge
// snapshot, read by the disaster fallback and by the minority of spokes still
// running the ".bak wins" copy-config variant.
//
// Capturing only .bak — the previous behaviour — meant archiving the snapshot
// while omitting the file that wins, producing a restore that silently differs
// from the original.

// TestSpokeFilesIncludesOverlay pins the overlay into the captured set.
func TestSpokeFilesIncludesOverlay(t *testing.T) {
	var haveOverlay, haveBak bool
	for _, f := range spokeFiles {
		switch f {
		case "hive.yaml.dashboard":
			haveOverlay = true
		case "hive.yaml.runtime":
			haveBak = true
		}
	}
	if !haveOverlay {
		t.Error("spokeFiles omits hive.yaml.dashboard — the overlay wins the boot " +
			"merge, so a restore without it reverts the owner's configuration")
	}
	if !haveBak {
		t.Error("spokeFiles omits hive.yaml.runtime — it is the disaster-fallback source")
	}
}

// TestSpokeFilesIncludesLegacyRuntimeConfig covers the migration: the rename to
// hive.yaml.runtime is copy-forward, never a PVC rename, so a spoke that has
// not saved since upgrading still carries ONLY hive.yaml.bak. Dropping it from
// the captured set would silently lose that spoke's config on restore.
func TestSpokeFilesIncludesLegacyRuntimeConfig(t *testing.T) {
	var haveLegacy bool
	for _, f := range spokeFiles {
		if f == spokeRuntimeConfigFileLegacy {
			haveLegacy = true
		}
	}
	if !haveLegacy {
		t.Errorf("spokeFiles omits %s — spokes that have not saved since the "+
			"rename carry only that name, and would be captured with no config",
			spokeRuntimeConfigFileLegacy)
	}
}

// TestLegacyOnlySpokeIsRecoverable: a not-yet-migrated spoke has neither the
// overlay nor the new runtime name, only the legacy one. It must NOT be flagged
// unrecoverable — that would be a false alarm on every un-migrated hive.
func TestLegacyOnlySpokeIsRecoverable(t *testing.T) {
	files := map[string][]byte{spokeRuntimeConfigFileLegacy: []byte("project: acme")}
	if reason := spokeConfigUnrecoverable(files); reason != "" {
		t.Errorf("a spoke carrying only the legacy %s must be considered "+
			"recoverable during the migration; got %q",
			spokeRuntimeConfigFileLegacy, reason)
	}
}

// TestSpokeWithNoConfigAtAllIsFlagged is the other direction: with none of the
// three config files, the spoke genuinely cannot be rebuilt and must be flagged
// rather than accepted silently.
func TestSpokeWithNoConfigAtAllIsFlagged(t *testing.T) {
	files := map[string][]byte{"hive-id": []byte("hive-x")}
	reason := spokeConfigUnrecoverable(files)
	if reason == "" {
		t.Fatal("a spoke with no config file at all must be flagged, not accepted")
	}
	for _, want := range []string{"hive.yaml.dashboard", spokeRuntimeConfigFile, spokeRuntimeConfigFileLegacy} {
		if !strings.Contains(reason, want) {
			t.Errorf("the error should name %s so an operator knows what to look for; got %q", want, reason)
		}
	}
}

// TestSpokeFilesExcludesPlainHiveYaml: the PVC's hive.yaml is regenerated from
// seed + overlay on every boot and was observed stale on a live spoke.
func TestSpokeFilesExcludesPlainHiveYaml(t *testing.T) {
	for _, f := range spokeFiles {
		if f == "hive.yaml" {
			t.Fatal("spokeFiles includes hive.yaml, which is regenerated every boot " +
				"and was observed stale on a live spoke")
		}
	}
}

// TestSpokeRecoverableWithOverlayOnly: a spoke whose capture yielded the
// overlay but no .bak is still restorable, so it must not be flagged as
// unrecoverable. Requiring .bak specifically would fail backups for no reason.
func TestSpokeRecoverableWithOverlayOnly(t *testing.T) {
	files := map[string][]byte{"hive.yaml.dashboard": []byte("project:\n  org: acme\n")}
	if reason := spokeConfigUnrecoverable(files); reason != "" {
		t.Errorf("spoke with an overlay was marked unrecoverable: %q", reason)
	}
}

// TestSpokeRecoverableWithBakOnly: the converse. A spoke that has never had a
// dashboard save legitimately has no overlay yet, and its .bak is sufficient.
func TestSpokeRecoverableWithBakOnly(t *testing.T) {
	files := map[string][]byte{"hive.yaml.runtime": []byte("project:\n  org: acme\n")}
	if reason := spokeConfigUnrecoverable(files); reason != "" {
		t.Errorf("spoke with a .bak was marked unrecoverable: %q", reason)
	}
}

// TestSpokeUnrecoverableWithNeitherConfig: missing both is a genuine failure
// and must still be surfaced, or an unrestorable archive reports success.
func TestSpokeUnrecoverableWithNeitherConfig(t *testing.T) {
	files := map[string][]byte{"hive-id": []byte("hosted-x")}
	reason := spokeConfigUnrecoverable(files)
	if reason == "" {
		t.Fatal("a spoke with neither config file was not flagged as unrecoverable")
	}
	if !strings.Contains(reason, "hive.yaml.dashboard") || !strings.Contains(reason, "hive.yaml.runtime") {
		t.Errorf("error should name both missing files, got %q", reason)
	}
}
