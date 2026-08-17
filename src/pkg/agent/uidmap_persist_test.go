package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// unwritableDirMode denies write and traverse to everyone but the owner's read
// bit, so creating anything beneath it fails with EACCES.
const unwritableDirMode os.FileMode = 0o500

// TestUIDMapSaveLoadRoundTrip pins the persistence contract: a saved map reads
// back with the same agent→UID assignments, so agent identities survive a
// restart. A UID that changed across a restart would break every
// file-ownership check that keys off it.
func TestUIDMapSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "uid-map.json")
	u := &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{
		"quality": testUIDBase,
		"scanner": testUIDBase + 1,
	}}

	if err := u.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadUIDMap(path)
	if err != nil {
		t.Fatalf("LoadUIDMap: %v", err)
	}
	if loaded.BaseUID != u.BaseUID {
		t.Errorf("BaseUID = %d, want %d", loaded.BaseUID, u.BaseUID)
	}
	for name, want := range u.Agents {
		if got := loaded.LookupByName(name); got != want {
			t.Errorf("LookupByName(%q) = %d, want %d", name, got, want)
		}
	}
	if got := loaded.LookupByUID(testUIDBase + 1); got != "scanner" {
		t.Errorf("LookupByUID(%d) = %q, want %q", testUIDBase+1, got, "scanner")
	}
}

// TestUIDMapSaveCreatesParentDir checks Save creates the intermediate directory
// rather than failing when the map is written before its directory exists.
func TestUIDMapSaveCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c", "uid-map.json")
	u := &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

	if err := u.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("uid map was not written: %v", err)
	}
}

// TestUIDMapSaveLeavesNoTempFile checks the atomic tmp+rename really renames:
// a leftover .tmp beside the map would accumulate on every save.
func TestUIDMapSaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uid-map.json")
	u := &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

	if err := u.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a .tmp file survived the save (err=%v), want it renamed away", err)
	}
}

// TestUIDMapSaveReportsWriteFailure checks an unwritable destination surfaces
// as an error rather than silently losing the map — a dropped UID map would
// re-assign every agent a new UID on the next boot.
func TestUIDMapSaveReportsWriteFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, unwritableDirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, unwritableDirMode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	u := &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}
	err := u.Save(filepath.Join(dir, "uid-map.json"))

	if err == nil {
		t.Fatal("Save() = nil, want an error when the destination is unwritable")
	}
}

// TestLoadUIDMapErrors covers the two failure modes callers must distinguish
// from an empty map: a missing file and unparseable contents.
func TestLoadUIDMapErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadUIDMap(filepath.Join(dir, "absent.json")); err == nil {
			t.Error("LoadUIDMap() = nil error for a missing file, want an error")
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := LoadUIDMap(path); err == nil {
			t.Error("LoadUIDMap() = nil error for malformed json, want an error")
		}
	})
}

// TestLoadUIDMapInitialisesNilAgents checks a map persisted without any agents
// comes back with a usable (non-nil) map, so the first assignment does not
// panic on a nil-map write.
func TestLoadUIDMapInitialisesNilAgents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uid-map.json")
	if err := os.WriteFile(path, []byte(`{"BaseUID":5000}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := LoadUIDMap(path)
	if err != nil {
		t.Fatalf("LoadUIDMap: %v", err)
	}
	if loaded.Agents == nil {
		t.Fatal("Agents map is nil, want it initialised")
	}
	// Must be usable immediately.
	if got := loaded.LookupByName("nobody"); got != 0 {
		t.Errorf("LookupByName on an empty map = %d, want 0", got)
	}
}

// TestNearestExistingDir pins the helper that locates which directory's
// permissions actually govern a recursive mkdir: it must walk up past every
// not-yet-created segment and stop at the first ancestor that exists.
func TestNearestExistingDir(t *testing.T) {
	root := t.TempDir()

	t.Run("path itself exists", func(t *testing.T) {
		if got := nearestExistingDir(root); got != root {
			t.Errorf("nearestExistingDir(%q) = %q, want the path itself", root, got)
		}
	})

	t.Run("walks up past missing segments", func(t *testing.T) {
		missing := filepath.Join(root, "not", "yet", "created")
		if got := nearestExistingDir(missing); got != root {
			t.Errorf("nearestExistingDir(%q) = %q, want %q", missing, got, root)
		}
	})

	t.Run("one missing segment", func(t *testing.T) {
		missing := filepath.Join(root, "child")
		if got := nearestExistingDir(missing); got != root {
			t.Errorf("nearestExistingDir(%q) = %q, want %q", missing, got, root)
		}
	})
}
