package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllocateUIDs_Alphabetical(t *testing.T) {
	u := NewUIDMap()
	u.AllocateUIDs([]string{"quality", "scanner", "ci-maintainer", "guide", "supervisor"})

	expected := map[string]int{
		"ci-maintainer": 2001,
		"guide":         2002,
		"quality":       2003,
		"scanner":       2004,
		"supervisor":    2005,
	}
	for name, want := range expected {
		got := u.Agents[name]
		if got != want {
			t.Errorf("agent %s: got UID %d, want %d", name, got, want)
		}
	}
}

func TestAllocateUIDs_Idempotent(t *testing.T) {
	u := NewUIDMap()
	u.AllocateUIDs([]string{"scanner", "quality"})
	first := u.Agents["quality"]
	u.AllocateUIDs([]string{"scanner", "quality"})
	second := u.Agents["quality"]
	if first != second {
		t.Errorf("not idempotent: first=%d, second=%d", first, second)
	}
}

func TestAllocateUID_Dynamic(t *testing.T) {
	u := NewUIDMap()
	u.AllocateUIDs([]string{"quality", "scanner"})
	maxBefore := 0
	for _, uid := range u.Agents {
		if uid > maxBefore {
			maxBefore = uid
		}
	}

	uid := u.AllocateUID("newcomer")
	if uid != maxBefore+1 {
		t.Errorf("dynamic UID: got %d, want %d", uid, maxBefore+1)
	}

	uid2 := u.AllocateUID("newcomer")
	if uid2 != uid {
		t.Errorf("duplicate allocation: got %d, want %d", uid2, uid)
	}
}

func TestLookupByUID(t *testing.T) {
	u := NewUIDMap()
	u.AllocateUIDs([]string{"quality", "scanner"})

	name := u.LookupByUID(u.Agents["quality"])
	if name != "quality" {
		t.Errorf("lookup by UID: got %q, want %q", name, "quality")
	}

	name = u.LookupByUID(99999)
	if name != "" {
		t.Errorf("lookup unknown UID: got %q, want empty", name)
	}
}

func TestLookupByName(t *testing.T) {
	u := NewUIDMap()
	u.AllocateUIDs([]string{"guide"})

	uid := u.LookupByName("guide")
	if uid != baseAgentUID {
		t.Errorf("lookup by name: got %d, want %d", uid, baseAgentUID)
	}

	uid = u.LookupByName("nonexistent")
	if uid != 0 {
		t.Errorf("lookup unknown name: got %d, want 0", uid)
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "uid-map.json")

	u := NewUIDMap()
	u.AllocateUIDs([]string{"quality", "scanner"})
	u.IptablesActive = true

	if err := u.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadUIDMap(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.ProxyUID != u.ProxyUID {
		t.Errorf("ProxyUID: got %d, want %d", loaded.ProxyUID, u.ProxyUID)
	}
	if !loaded.IptablesActive {
		t.Error("IptablesActive should be true")
	}
	for name, want := range u.Agents {
		got := loaded.Agents[name]
		if got != want {
			t.Errorf("loaded agent %s: got %d, want %d", name, got, want)
		}
	}
}

func TestLoadUIDMap_NotFound(t *testing.T) {
	_, err := LoadUIDMap("/nonexistent/uid-map.json")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestLoadUIDMap_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not json"), 0o644)

	_, err := LoadUIDMap(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}
