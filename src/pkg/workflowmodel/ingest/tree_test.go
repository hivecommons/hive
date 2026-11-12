package ingest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestReadTreeSelectsCIFiles(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".github/workflows/ci.yml", "on: push\njobs: {}\n")
	write(".github/workflows/nested/no.yml", "on: push\n")
	write(".github/dependabot.yml", "version: 2\n")
	write(".tekton/pr.yaml", "apiVersion: tekton.dev/v1\nkind: PipelineRun\n")
	write("ci/tekton/build.yaml", "kind: Task\n")
	write("deploy/release-pipeline.yaml", "apiVersion: tekton.dev/v1\nkind: Pipeline\n")
	write("deploy/other-pipeline.yaml", "apiVersion: apps/v1\nkind: Deployment\n")
	write("charts/values.yaml", "replicas: 1\n")
	write("vendor/x/.github/workflows/v.yml", "on: push\n")
	write("testdata/pipeline.yaml", "apiVersion: tekton.dev/v1\n")
	write("README.md", "hi\n")

	files, err := ReadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for p := range files {
		got = append(got, p)
	}
	sort.Strings(got)
	want := ".github/workflows/ci.yml .tekton/pr.yaml ci/tekton/build.yaml deploy/release-pipeline.yaml"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v", got)
	}
}

func TestReadTreeFeedsLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "workflows", "ci.yml"), []byte("name: CI\non: pull_request\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := ReadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	m := Lifecycle(scopeFor("acme/x"), files)
	if len(m.Spec.Pipelines) != 1 {
		t.Fatalf("pipelines %d", len(m.Spec.Pipelines))
	}
}

func TestReadTarballStreamsSelection(t *testing.T) {
	type entry struct {
		name string
		body []byte
		flag byte
	}
	big := bytes.Repeat([]byte("x"), 3*MaxTarballFileBytes)
	entries := []entry{
		{name: "pax_global_header", flag: tar.TypeXGlobalHeader},
		{name: "acme-widgets-abc123/", flag: tar.TypeDir},
		{name: "acme-widgets-abc123/.github/workflows/ci.yml", body: []byte("on: push\njobs: {}\n"), flag: tar.TypeReg},
		{name: "acme-widgets-abc123/.tekton/pr.yaml", body: []byte("apiVersion: tekton.dev/v1\nkind: PipelineRun\n"), flag: tar.TypeReg},
		{name: "acme-widgets-abc123/deploy/release-pipeline.yaml", body: []byte("apiVersion: tekton.dev/v1\nkind: Pipeline\n"), flag: tar.TypeReg},
		{name: "acme-widgets-abc123/deploy/other-pipeline.yaml", body: []byte("kind: Deployment\n"), flag: tar.TypeReg},
		{name: "acme-widgets-abc123/vendor/x/.github/workflows/v.yml", body: []byte("on: push\n"), flag: tar.TypeReg},
		// Source blobs far past any per-file cap stream through untouched.
		{name: "acme-widgets-abc123/third_party/blob.bin", body: big, flag: tar.TypeReg},
		{name: "acme-widgets-abc123/assets/huge.tar", body: big, flag: tar.TypeReg},
		// A matched path over the per-file cap is skipped, not fatal.
		{name: "acme-widgets-abc123/.github/workflows/huge.yml", body: big, flag: tar.TypeReg},
		{name: "acme-widgets-abc123/README.md", body: []byte("hi\n"), flag: tar.TypeReg},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.flag, Mode: 0o644, Size: int64(len(e.body))}
		if e.flag == tar.TypeXGlobalHeader {
			hdr = &tar.Header{Name: e.name, Typeflag: e.flag, PAXRecords: map[string]string{"comment": "abc123"}}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := ReadTarball(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for p := range files {
		got = append(got, p)
	}
	sort.Strings(got)
	want := ".github/workflows/ci.yml .tekton/pr.yaml deploy/release-pipeline.yaml"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v", got)
	}
	if string(files[".tekton/pr.yaml"]) != "apiVersion: tekton.dev/v1\nkind: PipelineRun\n" {
		t.Fatalf("content: %q", files[".tekton/pr.yaml"])
	}

	if _, err := ReadTarball(strings.NewReader("not gzip")); err == nil {
		t.Fatal("expected gzip error")
	}
}
