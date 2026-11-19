package curated

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestLoadSkipsBadFiles(t *testing.T) {
	good, err := files.ReadFile("cncf-techdocs-assessment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{
		"a-bad.yaml":   {Data: []byte("a: [")},
		"b-good.yaml":  {Data: good},
		"c-wrong.yaml": {Data: []byte("apiVersion: workflow.hivecommons.dev/v1alpha1\nkind: WorkflowModel\nspec: {meta: {version: v1, provenance: ingested}}\n")},
		"ignored.json": {Data: []byte("{")},
	}
	entries, errs := load(fsys)
	if len(entries) != 1 || entries[0].Path != "b-good.yaml" {
		t.Fatalf("entries = %+v", entries)
	}
	if len(errs) != 2 {
		t.Fatalf("errs = %v", errs)
	}
}

// unreadableFS globs one file whose read fails.
type unreadableFS struct{ fstest.MapFS }

func (u unreadableFS) Open(name string) (fs.File, error) {
	if name == "x.yaml" {
		return nil, errors.New("boom")
	}
	return u.MapFS.Open(name)
}

func TestLoadReadError(t *testing.T) {
	fsys := unreadableFS{fstest.MapFS{"x.yaml": {Data: []byte("a: 1")}}}
	entries, errs := load(fsys)
	if len(entries) != 0 || len(errs) != 1 {
		t.Fatalf("entries=%v errs=%v", entries, errs)
	}
}
