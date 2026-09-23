package extwork

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "extwork")
	s := NewFileStore(dir)
	adm := testAdmission()
	adm.EngineIncarnation = "inc-1"
	if _, ok, err := s.Load(adm.AssignmentID); ok || err != nil {
		t.Fatalf("empty Load = %v %v", ok, err)
	}
	if err := s.Persist(adm); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load(adm.AssignmentID)
	if err != nil || !ok || got.EngineIncarnation != "inc-1" || got.ExecutionKey() != adm.ExecutionKey() {
		t.Fatalf("Load = %+v %v %v", got, ok, err)
	}
	info, err := os.Stat(filepath.Join(dir, adm.AssignmentID+admissionFileSuffix))
	if err != nil || info.Mode().Perm() != fileStoreFilePerm {
		t.Fatalf("record perm = %v %v", info, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("stray temp files: %d entries", len(entries))
	}
	if _, ok, _ := s.LoadReceipt(adm.AssignmentID); ok {
		t.Fatal("receipt present before save")
	}
	if err := s.SaveReceipt(adm.AssignmentID, []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if raw, ok, err := s.LoadReceipt(adm.AssignmentID); err != nil || !ok || string(raw) != `{"x":1}` {
		t.Fatalf("LoadReceipt = %q %v %v", raw, ok, err)
	}
	// Corrupt record is reported, not silently treated as absent.
	if err := os.WriteFile(filepath.Join(dir, adm.AssignmentID+admissionFileSuffix), []byte("{"), fileStoreFilePerm); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load(adm.AssignmentID); err == nil {
		t.Fatal("corrupt record loaded without error")
	}
	// Invalid admissions and unsafe ids are refused everywhere.
	bad := adm
	bad.WorkKey = ""
	if err := s.Persist(bad); !errors.Is(err, ErrInvalidAdmission) {
		t.Fatalf("Persist invalid = %v", err)
	}
	for _, id := range []string{"", "../escape", ".hidden", "a/b", "sp ace"} {
		if _, _, err := s.Load(id); !errors.Is(err, ErrBadAssignmentID) {
			t.Errorf("Load(%q) = %v", id, err)
		}
		if err := s.SaveReceipt(id, nil); !errors.Is(err, ErrBadAssignmentID) {
			t.Errorf("SaveReceipt(%q) = %v", id, err)
		}
		if _, _, err := s.LoadReceipt(id); !errors.Is(err, ErrBadAssignmentID) {
			t.Errorf("LoadReceipt(%q) = %v", id, err)
		}
		unsafe := adm
		unsafe.AssignmentID = id
		if id != "" {
			if err := s.Persist(unsafe); !errors.Is(err, ErrBadAssignmentID) {
				t.Errorf("Persist(%q) = %v", id, err)
			}
		}
	}
	// Unwritable directory surfaces the error.
	blocked := NewFileStore(filepath.Join(t.TempDir(), "file-not-dir"))
	if err := os.WriteFile(blocked.dir, []byte("x"), fileStoreFilePerm); err != nil {
		t.Fatal(err)
	}
	if err := blocked.Persist(adm); err == nil {
		t.Fatal("write into a file path succeeded")
	}
	if _, _, err := blocked.Load(adm.AssignmentID); err == nil {
		t.Fatal("read through a file path succeeded")
	}
}
