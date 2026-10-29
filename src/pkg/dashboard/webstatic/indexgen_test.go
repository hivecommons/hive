package webstatic

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// TestIndexHTMLIsGenerated pins the rule that static/index.html is an
// artifact: it must equal the byte-exact concatenation of static-src/ per
// manifest.txt. A failure means someone edited the artifact directly or
// changed a source without running `make dashboard-gen`.
func TestIndexHTMLIsGenerated(t *testing.T) {
	want, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ConcatIndex(os.DirFS("../static-src"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, want) {
		return
	}
	line, col := firstDiff(got, want)
	t.Fatalf("static/index.html differs from the concatenation of static-src/ (first difference at line %d, byte %d).\n"+
		"Edit the per-area sources in static-src/, then run `make dashboard-gen` (go generate ./pkg/dashboard/webstatic/) and commit both.", line, col)
}

func firstDiff(a, b []byte) (line, col int) {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	prefix := a[:i]
	return bytes.Count(prefix, []byte("\n")) + 1, i - (bytes.LastIndexByte(prefix, '\n') + 1)
}

func TestIndexManifestEveryFileListedAndUsed(t *testing.T) {
	data, err := os.ReadFile("../static-src/" + IndexManifestName)
	if err != nil {
		t.Fatal(err)
	}
	files, err := ParseIndexManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, f := range files {
		if listed[f] {
			t.Errorf("%s listed twice in manifest", f)
		}
		listed[f] = true
	}
	src := os.DirFS("../static-src")
	var onDisk []string
	for _, dir := range []string{"css", "js", "html", "glue", "."} {
		entries, err := os.ReadDir("../static-src/" + dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || e.Name() == IndexManifestName {
				continue
			}
			name := e.Name()
			if dir != "." {
				name = dir + "/" + name
			}
			onDisk = append(onDisk, name)
		}
	}
	for _, name := range onDisk {
		if !listed[name] {
			t.Errorf("static-src/%s exists but is not in manifest.txt (it would be silently dropped from index.html)", name)
		}
	}
	for _, name := range files {
		if _, err := src.Open(name); err != nil {
			t.Errorf("manifest lists %s: %v", name, err)
		}
	}
}

func TestConcatIndexIsByteExact(t *testing.T) {
	fsys := fstest.MapFS{
		IndexManifestName: {Data: []byte("# comment\nhead.html\n\ncss/a.css\njs/b.js\n")},
		"head.html":       {Data: []byte("<html>\n  <style>\n")},
		"css/a.css":       {Data: []byte("a{b:c}")}, // no trailing newline: must not get one
		"js/b.js":         {Data: []byte("\n</style></html>\n")},
	}
	got, err := ConcatIndex(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if want := "<html>\n  <style>\na{b:c}\n</style></html>\n"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := ConcatIndex(fstest.MapFS{IndexManifestName: {Data: []byte("missing.css\n")}}); err == nil || !strings.Contains(err.Error(), "missing.css") {
		t.Fatalf("expected missing-file error, got %v", err)
	}
	if _, err := ParseIndexManifest([]byte("../escape.css\n")); err == nil {
		t.Fatal("expected rejection of path escaping the source root")
	}
}
