package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

func techdocs(t *testing.T) *workflowmodel.Model {
	t.Helper()
	b, err := os.ReadFile("../testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func TestEmptyStoreListsBuiltin(t *testing.T) {
	s := Open(t.TempDir())
	got, err := s.List()
	if err != nil || len(got) != 1 || got[0].Name != hivedefault.Name || !got[0].ReadOnly || !got[0].Compiled || got[0].Provenance != workflowmodel.ProvenanceBuiltIn {
		t.Fatalf("%v %+v", err, got)
	}
	m, e, err := s.Get(hivedefault.Name, "")
	if err != nil || m == nil || e.Hash != hivedefault.Profile().Hash {
		t.Fatalf("%v %+v", err, e)
	}
	if _, _, err := s.Get(hivedefault.Name, "9"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	p, err := s.Profile(hivedefault.Name, hivedefault.Version)
	if err != nil || p.Name != hivedefault.Name {
		t.Fatal(err)
	}
	if _, err := s.Save(hivedefault.Generate(), "andy", t0); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	if err := s.Delete(hivedefault.Name, "1"); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
}

func TestSaveGetListDelete(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	m := techdocs(t)
	e, err := s.Save(m, "andy", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Compiled || e.Provenance != workflowmodel.ProvenanceLocal || e.SavedBy != "andy" || !e.SavedAt.Equal(t0) || e.States != 9 || e.Columns != 5 || e.Key() != "cncf-techdocs@1" {
		t.Fatalf("%+v", e)
	}
	if _, err := os.Stat(filepath.Join(root, Subdir, "cncf-techdocs", "1.json")); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 2 || list[1].Key() != "cncf-techdocs@1" {
		t.Fatalf("%v %+v", err, list)
	}
	got, ge, err := s.Get("cncf-techdocs", "1")
	if err != nil || got.Hash() != m.Hash() || ge.SavedBy != "andy" {
		t.Fatalf("%v %+v", err, ge)
	}
	if p, err := s.Profile("cncf-techdocs", "1"); err != nil || p.Mode != workflowmodel.DerivationRules {
		t.Fatal(err)
	}

	// second version, then Versions is newest first
	m2 := techdocs(t)
	m2.Spec.Meta.Version = "2"
	if _, err := s.Save(m2, "nate", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	vs, err := s.Versions("cncf-techdocs")
	if err != nil || len(vs) != 2 || vs[0].Version != "2" {
		t.Fatalf("%v %+v", err, vs)
	}
	if _, err := s.Versions("nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	if err := s.Delete("cncf-techdocs", "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("cncf-techdocs", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, _, err := s.Get("cncf-techdocs", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	list, _ = s.List()
	if len(list) != 2 || list[1].Version != "2" {
		t.Fatalf("%+v", list)
	}
	if err := s.Delete("cncf-techdocs", "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, Subdir, "cncf-techdocs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty model dir should be removed: %v", err)
	}
}

func TestDraftsSaveButDoNotProfile(t *testing.T) {
	s := Open(t.TempDir())
	m := techdocs(t)
	m.Spec.Gates[0].ReleasedBy = ""
	e, err := s.Save(m, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if e.Compiled || len(e.Diagnostics) == 0 {
		t.Fatalf("%+v", e)
	}
	if _, err := s.Profile("cncf-techdocs", "1"); !errors.Is(err, ErrNotCompiled) {
		t.Fatal(err)
	}
	// an invalid shape is refused outright
	m.Spec.Initial = "ghost"
	if _, err := s.Save(m, "", t0); err == nil {
		t.Fatal("invalid model saved")
	}
}

func TestKeysAreChecked(t *testing.T) {
	s := Open(t.TempDir())
	m := techdocs(t)
	m.Metadata.Name = "../etc"
	if _, err := s.Save(m, "", t0); !errors.Is(err, ErrBadName) {
		t.Fatal(err)
	}
	m = techdocs(t)
	m.Spec.Meta.Version = "1/../x"
	if _, err := s.Save(m, "", t0); !errors.Is(err, ErrBadVersion) {
		t.Fatal(err)
	}
	if _, _, err := s.Get("Bad Name", "1"); !errors.Is(err, ErrBadName) {
		t.Fatal(err)
	}
	if err := s.Delete("ok", "bad version"); !errors.Is(err, ErrBadVersion) {
		t.Fatal(err)
	}
	if _, err := s.Save(nil, "", t0); err == nil {
		t.Fatal("nil saved")
	}
}

func TestResaveKeepsReservedFields(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	m := techdocs(t)
	if _, err := s.Save(m, "", t0); err != nil {
		t.Fatal(err)
	}
	idx, _ := s.readIndex()
	idx[0].Ratings = json.RawMessage(`{"stars":5}`)
	if err := s.writeIndex(idx); err != nil {
		t.Fatal(err)
	}
	m.Spec.Meta.Description = "edited"
	e, err := s.Save(m, "", t0.Add(time.Minute))
	if err != nil || e.Description != "edited" {
		t.Fatalf("%v %+v", err, e)
	}
	var r struct{ Stars int }
	if err := json.Unmarshal(e.Ratings, &r); err != nil || r.Stars != 5 {
		t.Fatalf("ratings %s: %v", e.Ratings, err)
	}
}

func TestRebuild(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	if _, err := s.Save(techdocs(t), "", t0); err != nil {
		t.Fatal(err)
	}
	// corrupt the index, drop a stray file and a misfiled model
	if err := os.WriteFile(filepath.Join(root, Subdir, IndexFile), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("corrupt index read")
	}
	_ = os.WriteFile(filepath.Join(root, Subdir, "cncf-techdocs", "notes.txt"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, Subdir, "cncf-techdocs", "7.json"), []byte(`{"metadata":{"name":"other"}}`), 0o644)
	_ = os.WriteFile(filepath.Join(root, Subdir, "cncf-techdocs", "8.json"), []byte(`not json`), 0o644)
	idx, err := s.Rebuild(t0)
	if err != nil || len(idx) != 1 || idx[0].Key() != "cncf-techdocs@1" {
		t.Fatalf("%v %+v", err, idx)
	}
	// an index entry missing for a model on disk still resolves on Get
	if err := os.WriteFile(filepath.Join(root, Subdir, IndexFile), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, e, err := s.Get("cncf-techdocs", "1")
	if err != nil || !e.Compiled || e.SavedBy != "" {
		t.Fatalf("%v %+v", err, e)
	}

	empty := Open(t.TempDir())
	if idx, err := empty.Rebuild(t0); err != nil || len(idx) != 0 {
		t.Fatalf("%v %+v", err, idx)
	}
}

func TestUnreadableModelAndIndex(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	if _, err := s.Save(techdocs(t), "", t0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.modelPath("cncf-techdocs", "1"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get("cncf-techdocs", "1"); err == nil {
		t.Fatal("truncated model read")
	}
	if _, err := s.Profile("cncf-techdocs", "1"); err == nil {
		t.Fatal("truncated model profiled")
	}
	if err := os.WriteFile(filepath.Join(root, Subdir, IndexFile), []byte("["), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Versions("cncf-techdocs"); err == nil {
		t.Fatal("corrupt index listed")
	}
	if _, err := s.Save(techdocs(t), "", t0); err == nil {
		t.Fatal("saved over a corrupt index")
	}
	if err := s.Delete("cncf-techdocs", "1"); err == nil {
		t.Fatal("deleted over a corrupt index")
	}
	if vs, err := Open(t.TempDir()).Versions(hivedefault.Name); err != nil || len(vs) != 1 {
		t.Fatalf("%v %+v", err, vs)
	}
}

func TestRoot(t *testing.T) {
	t.Setenv(EnvRoot, "")
	if Root("") != DefaultRoot || Root("/x") != "/x" {
		t.Fatal("root resolution")
	}
	t.Setenv(EnvRoot, "/env")
	if Root("/x") != "/env" {
		t.Fatal("env must win")
	}
}

func TestNextVersion(t *testing.T) {
	s := Open(t.TempDir())
	m := techdocs(t)
	if v, err := s.NextVersion(m.Metadata.Name); err != nil || v != "1" {
		t.Fatalf("empty: %q %v", v, err)
	}
	if _, err := s.NextVersion("Bad Name"); !errors.Is(err, ErrBadName) {
		t.Fatal(err)
	}
	for _, v := range []string{"1", "3", "2.0-rc1"} {
		m.Spec.Meta.Version = v
		if _, err := s.Save(m, "andy", t0); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := s.NextVersion(m.Metadata.Name); err != nil || v != "4" {
		t.Fatalf("after 1,3,2.0-rc1: %q %v", v, err)
	}
	if v, err := s.NextVersion(hivedefault.Name); err != nil || v != "2" {
		t.Fatalf("builtin: %q %v", v, err)
	}
}

func TestProvisionedLabelsRoundTrip(t *testing.T) {
	s := Open(t.TempDir())
	if rec, err := s.LoadProvisioned("Acme/Widget"); err != nil || rec != nil {
		t.Fatalf("missing record: %v %v", rec, err)
	}
	if err := s.DeleteProvisioned("acme/widget"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	in := workflowmodel.ProvisionedLabels{Repo: "Acme/Widget", Name: "techdocs", Version: "1", Hash: "h", Labels: []string{"docs/010-requested"}, UpdatedAt: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	if err := s.SaveProvisioned(in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir(), ProvisionedSubdir, "acme__widget.json")); err != nil {
		t.Fatal(err)
	}
	rec, err := s.LoadProvisioned("acme/widget")
	if err != nil || rec == nil || rec.Repo != "acme/widget" || rec.Name != "techdocs" || rec.Hash != "h" || len(rec.Labels) != 1 || !rec.UpdatedAt.Equal(in.UpdatedAt) {
		t.Fatalf("round trip: %+v %v", rec, err)
	}
	if err := s.SaveProvisioned(workflowmodel.ProvisionedLabels{Repo: "acme/widget"}); err != nil {
		t.Fatal(err)
	}
	if rec, _ = s.LoadProvisioned("acme/widget"); rec == nil || rec.Labels == nil || len(rec.Labels) != 0 {
		t.Fatalf("empty labels must marshal as []: %+v", rec)
	}
	if err := s.DeleteProvisioned("acme/widget"); err != nil {
		t.Fatal(err)
	}
	if rec, _ = s.LoadProvisioned("acme/widget"); rec != nil {
		t.Fatalf("still present: %+v", rec)
	}
	for _, bad := range []string{"", "widget", "/widget", "acme/"} {
		if _, err := s.LoadProvisioned(bad); err == nil {
			t.Errorf("LoadProvisioned(%q) accepted", bad)
		}
		if err := s.SaveProvisioned(workflowmodel.ProvisionedLabels{Repo: bad}); err == nil {
			t.Errorf("SaveProvisioned(%q) accepted", bad)
		}
		if err := s.DeleteProvisioned(bad); err == nil {
			t.Errorf("DeleteProvisioned(%q) accepted", bad)
		}
	}
	if err := os.WriteFile(filepath.Join(s.dir(), ProvisionedSubdir, "acme__broken.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadProvisioned("acme/broken"); err == nil {
		t.Fatal("corrupt record accepted")
	}
}
