package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunWritesThenChecks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	out := filepath.Join(dir, "index.html")
	writeTree(t, src, map[string]string{
		"manifest.txt": "# order matters\nhead.html\n\ncss/a.css\n",
		"head.html":    "<html>\n",
		"css/a.css":    "a{}\n",
	})

	var stderr bytes.Buffer
	if rc := run([]string{"-src", src, "-out", out}, &stderr); rc != 0 {
		t.Fatalf("write run exit %d: %s", rc, stderr.String())
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "<html>\na{}\n" {
		t.Fatalf("generated %q", got)
	}

	stderr.Reset()
	if rc := run([]string{"-src", src, "-out", out, "-check"}, &stderr); rc != 0 {
		t.Fatalf("check on fresh artifact exit %d: %s", rc, stderr.String())
	}

	if err := os.WriteFile(out, []byte("<html>\nb{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if rc := run([]string{"-src", src, "-out", out, "-check"}, &stderr); rc != 1 {
		t.Fatalf("check on stale artifact exit %d, want 1: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "make dashboard-gen") {
		t.Fatalf("stale message should point at make dashboard-gen: %s", stderr.String())
	}
}

func TestRunErrors(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	writeTree(t, src, map[string]string{
		"manifest.txt": "head.html\n",
		"head.html":    "<html>\n",
	})
	var stderr bytes.Buffer

	if rc := run([]string{"-src", filepath.Join(dir, "missing")}, &stderr); rc != 2 {
		t.Fatalf("missing source dir exit %d, want 2", rc)
	}
	if rc := run([]string{"-src", src, "-out", filepath.Join(dir, "nope", "index.html"), "-check"}, &stderr); rc != 2 {
		t.Fatalf("-check with unreadable -out exit %d, want 2", rc)
	}
	if rc := run([]string{"-src", src, "-out", filepath.Join(dir, "nope", "index.html")}, &stderr); rc != 2 {
		t.Fatalf("unwritable -out exit %d, want 2", rc)
	}
	if rc := run([]string{"-bogus"}, &stderr); rc != 2 {
		t.Fatalf("bad flag exit %d, want 2", rc)
	}
}
