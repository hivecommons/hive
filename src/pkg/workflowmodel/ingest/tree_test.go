package ingest

import (
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
