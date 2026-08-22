package linearagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testInstall() Install {
	return Install{
		ViewerID:           "viewer-1",
		OrganizationID:     "org-1",
		OrganizationName:   "Acme",
		OrganizationURLKey: "acme",
		Token:              Token{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(24 * time.Hour)},
		ConnectedAt:        time.Now(),
	}
}

func TestStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "linear-agent.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, ok := s.Get(); ok {
		t.Fatal("fresh store reports an install")
	}
	if err := s.Set(testInstall()); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Owner-only: the file holds a live token.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", fi.Mode().Perm())
	}
	// Atomic write: no .tmp litter.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file left behind: %v", err)
	}

	// A second store on the same path sees the install.
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	inst, ok := s2.Get()
	if !ok || inst.ViewerID != "viewer-1" || inst.OrganizationName != "Acme" || inst.Token.AccessToken != "at" {
		t.Fatalf("reloaded install = %+v ok=%v", inst, ok)
	}
}

func TestStore_UpdateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.json")
	s, _ := NewStore(path)
	if err := s.UpdateToken(Token{AccessToken: "x"}); err == nil {
		t.Fatal("UpdateToken on empty store must error")
	}
	if err := s.Set(testInstall()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.UpdateToken(Token{AccessToken: "at2", RefreshToken: "rt2"}); err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	s2, _ := NewStore(path)
	inst, _ := s2.Get()
	if inst.Token.AccessToken != "at2" || inst.Token.RefreshToken != "rt2" {
		t.Errorf("token = %+v", inst.Token)
	}
	if inst.ViewerID != "viewer-1" {
		t.Errorf("viewer id lost on token update: %+v", inst)
	}
}

func TestStore_Clear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.json")
	s, _ := NewStore(path)
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear on empty store: %v", err)
	}
	s.Set(testInstall())
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok := s.Get(); ok {
		t.Error("install survives Clear")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file survives Clear: %v", err)
	}
}

func TestStore_CorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := NewStore(path); err == nil {
		t.Fatal("corrupt store must surface, not read as not-installed")
	}
}

func TestStoredViewerID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.json")
	if got := StoredViewerID(path); got != "" {
		t.Errorf("missing file → %q, want empty", got)
	}
	os.WriteFile(path, []byte("nope"), 0o600)
	if got := StoredViewerID(path); got != "" {
		t.Errorf("corrupt file → %q, want empty", got)
	}
	s, _ := NewStore(filepath.Join(t.TempDir(), "real.json"))
	_ = s
	s2, _ := NewStore(path[:len(path)-len("l.json")] + "ok.json")
	s2.Set(testInstall())
	if got := StoredViewerID(s2.path); got != "viewer-1" {
		t.Errorf("StoredViewerID = %q, want viewer-1", got)
	}
}

func TestDefaultStorePath(t *testing.T) {
	t.Setenv("LINEAR_AGENT_STORE", "")
	if got := DefaultStorePath(); got != "/data/linear-agent.json" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("LINEAR_AGENT_STORE", "/tmp/x.json")
	if got := DefaultStorePath(); got != "/tmp/x.json" {
		t.Errorf("override = %q", got)
	}
}
