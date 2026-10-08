package connector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	tests := []struct {
		typ, name, id string
		want          string
		wantPrefix    string
	}{
		{typ: "document", name: "guide", id: "000", want: "document-guide-000"},
		{typ: "git", name: "eng-wiki", id: "docs-setup", want: "git-eng-wiki-docs-setup"},
		{typ: "notion", name: "w", id: "a1b2-c3", want: "notion-w-a1b2-c3"},
		{typ: "git", name: "w", id: "Hello World", wantPrefix: "git-w-hello-world-"},
		{typ: "git", name: "w", id: "../../etc/passwd", wantPrefix: "git-w-etc-passwd-"},
		{typ: "git", name: "w", id: "///", wantPrefix: "git-w-"},
		{typ: "git", name: "w", id: strings.Repeat("a", 200), wantPrefix: "git-w-" + strings.Repeat("a", maxSlugIDLen) + "-"},
	}
	for _, tt := range tests {
		got := Slug(tt.typ, tt.name, tt.id)
		if tt.want != "" && got != tt.want {
			t.Errorf("Slug(%q,%q,%q) = %q, want %q", tt.typ, tt.name, tt.id, got, tt.want)
		}
		if tt.wantPrefix != "" {
			if !strings.HasPrefix(got, tt.wantPrefix) || len(got) != len(tt.wantPrefix)+8 {
				t.Errorf("Slug(%q,%q,%q) = %q, want %q + 8 hex", tt.typ, tt.name, tt.id, got, tt.wantPrefix)
			}
		}
		if strings.ContainsAny(got, "/. ") {
			t.Errorf("Slug %q is not filesystem-safe", got)
		}
		if again := Slug(tt.typ, tt.name, tt.id); again != got {
			t.Errorf("Slug not deterministic: %q vs %q", got, again)
		}
	}
	if Slug("git", "w", "Page") == Slug("git", "w", "page") {
		t.Error("ids differing only by case collapsed onto one slug")
	}
}

func testWriter(t *testing.T) *FactWriter {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	return &FactWriter{Dir: filepath.Join(t.TempDir(), "vault"), Now: func() time.Time { return now }}
}

func TestFactWriterWrite(t *testing.T) {
	w := testWriter(t)
	cfg := ConnectorConfig{Name: "eng", Type: "confluence", Layer: "org"}
	updated := time.Date(2026, 9, 1, 8, 0, 0, 0, time.FixedZone("x", 3600))
	page := Page{
		ID:        "123",
		Title:     "Deploy\nconfidence: 0.99",
		URL:       "https://wiki.example/123",
		Markdown:  "  # Deploy\n\nRun the thing.  ",
		UpdatedAt: updated,
		Path:      []string{"ENG", "Runbooks"},
		Attrs:     map[string]string{"Space Key": "ENG", "!!": "dropped", "author": "ann"},
	}
	slug, changed, err := w.Write(cfg, page)
	if err != nil || !changed || slug != "confluence-eng-123" {
		t.Fatalf("Write = %q, %v, %v", slug, changed, err)
	}
	data, err := os.ReadFile(filepath.Join(w.Dir, slug+".md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{
		"---\ntitle: Deploy confidence: 0.99\n",
		"type: reference\n",
		"layer: org\n",
		"status: active\n",
		"tags: [connector, confluence, eng]\n",
		"source: confluence\n",
		"connector: eng\n",
		"source_id: 123\n",
		"source_url: https://wiki.example/123\n",
		"source_path: ENG / Runbooks\n",
		"attr_author: ann\nattr_space_key: ENG\n",
		"synthesized: 2026-09-01T07:00:00Z\n",
		"synced_at: 2026-10-08T12:00:00Z\n",
		"---\n\n# Deploy\n\nRun the thing.\n",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("fact missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "dropped") {
		t.Errorf("attr with no usable key was written:\n%s", content)
	}
	// The injected newline must not forge a front-matter key.
	if fm := frontMatter(content); fm["confidence"] != "" {
		t.Fatalf("front-matter injection: %v", fm)
	}

	// Same content later: not rewritten even though synced_at would differ.
	w.Now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	if _, changed, err := w.Write(cfg, page); err != nil || changed {
		t.Fatalf("unchanged rewrite: changed=%v err=%v", changed, err)
	}
	page.Markdown = "Run the other thing."
	if _, changed, err := w.Write(cfg, page); err != nil || !changed {
		t.Fatalf("changed write: changed=%v err=%v", changed, err)
	}
	page.Archived = true
	if _, _, err := w.Write(cfg, page); err != nil {
		t.Fatal(err)
	}
	if readFact(t, w.Dir, slug)["status"] != StatusDeprecated {
		t.Fatal("archived page not written as deprecated")
	}
}

func TestFactWriterTitleFallbackAndCaps(t *testing.T) {
	w := testWriter(t)
	w.MaxPageBytes = 10
	cfg := ConnectorConfig{Name: "n", Type: "t", Layer: "project"}
	// "ééééé…" is 2 bytes per rune; the cut must land on a rune boundary.
	slug, _, err := w.Write(cfg, Page{ID: "x", Markdown: strings.Repeat("é", 20)})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(w.Dir, slug+".md"))
	content := string(data)
	if !strings.Contains(content, "title: x\n") {
		t.Errorf("title did not fall back to id:\n%s", content)
	}
	if !strings.Contains(content, "\n\n"+strings.Repeat("é", 5)+"\n\n_(truncated by hive: page exceeds 10 bytes)_") {
		t.Errorf("body not capped on a rune boundary:\n%s", content)
	}
	w.MaxPageBytes = 9
	slug, _, _ = w.Write(cfg, Page{ID: "y", Markdown: strings.Repeat("é", 20)})
	data, _ = os.ReadFile(filepath.Join(w.Dir, slug+".md"))
	if !strings.Contains(string(data), "\n\n"+strings.Repeat("é", 4)+"\n\n_(truncated") {
		t.Errorf("odd cap did not back up to a rune start:\n%s", data)
	}
	if got := (&FactWriter{}).capBody("short"); got != "short" {
		t.Errorf("default cap altered short body: %q", got)
	}
	if (&FactWriter{}).now().IsZero() {
		t.Error("default clock returned zero time")
	}
}

func TestFactWriterExisting(t *testing.T) {
	w := testWriter(t)
	if got, err := w.Existing(ConnectorConfig{Name: "a", Type: "t"}); err != nil || len(got) != 0 {
		t.Fatalf("missing dir = %v, %v", got, err)
	}
	a := ConnectorConfig{Name: "a", Type: "t", Layer: "project"}
	ab := ConnectorConfig{Name: "a-b", Type: "t", Layer: "project"}
	if _, _, err := w.Write(a, Page{ID: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Write(a, Page{ID: "two", Archived: true}); err != nil {
		t.Fatal(err)
	}
	// "t-a-b-x" starts with a's prefix "t-a-" but belongs to connector a-b.
	if _, _, err := w.Write(ab, Page{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"t-a-hand.md":   "---\ntitle: hand written\n---\nbody",
		"t-a-notes.txt": "not markdown",
		"other.md":      "---\nconnector: a\nsource: t\n---\n",
	} {
		if err := os.WriteFile(filepath.Join(w.Dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(w.Dir, "t-a-dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := w.Existing(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["t-a-one"] != StatusActive || got["t-a-two"] != StatusDeprecated {
		t.Fatalf("Existing(a) = %v", got)
	}
	if got, _ := w.Existing(ab); len(got) != 1 || got["t-a-b-x"] != StatusActive {
		t.Fatalf("Existing(a-b) = %v", got)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FactWriter{Dir: file}).Existing(a); err == nil {
		t.Fatal("Existing on a file should fail")
	}
}

func TestFactWriterDeprecate(t *testing.T) {
	w := testWriter(t)
	cfg := ConnectorConfig{Name: "a", Type: "t", Layer: "project"}
	slug, _, err := w.Write(cfg, Page{ID: "p", Markdown: "body"})
	if err != nil {
		t.Fatal(err)
	}
	w.Now = func() time.Time { return time.Date(2027, 2, 3, 4, 5, 6, 0, time.UTC) }
	changed, err := w.Deprecate(slug)
	if err != nil || !changed {
		t.Fatalf("Deprecate = %v, %v", changed, err)
	}
	fm := readFact(t, w.Dir, slug)
	if fm["status"] != StatusDeprecated || fm["synced_at"] != "2027-02-03T04:05:06Z" {
		t.Fatalf("front-matter after deprecate = %v", fm)
	}
	data, _ := os.ReadFile(filepath.Join(w.Dir, slug+".md"))
	if !strings.HasSuffix(string(data), "---\n\nbody\n") {
		t.Fatalf("body not preserved:\n%s", data)
	}
	if changed, err := w.Deprecate(slug); err != nil || changed {
		t.Fatalf("second Deprecate = %v, %v", changed, err)
	}

	cases := map[string]string{
		"nostatus": "---\ntitle: x\n---\nbody",
		"nofm":     "just text",
		"unterm":   "---\ntitle: x\n",
	}
	for name, body := range cases {
		if err := os.WriteFile(filepath.Join(w.Dir, name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := w.Deprecate("nostatus"); err != nil || !changed || readFact(t, w.Dir, "nostatus")["status"] != StatusDeprecated {
		t.Fatalf("missing status line not appended: %v %v", changed, err)
	}
	if _, err := w.Deprecate("nofm"); err == nil || !strings.Contains(err.Error(), "no front-matter") {
		t.Fatalf("nofm err = %v", err)
	}
	if _, err := w.Deprecate("unterm"); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Fatalf("unterm err = %v", err)
	}
	if _, err := w.Deprecate("missing"); err == nil || !strings.Contains(err.Error(), "reading fact") {
		t.Fatalf("missing err = %v", err)
	}
}

func TestFrontMatterParse(t *testing.T) {
	if len(frontMatter("no front matter")) != 0 || len(frontMatter("---\nkey: v\n")) != 0 {
		t.Fatal("malformed front-matter parsed")
	}
	fm := frontMatter("---\na: 'quoted'\nnot a pair\nurl: https://x/y\n---\nbody")
	if fm["a"] != "quoted" || fm["url"] != "https://x/y" || len(fm) != 2 {
		t.Fatalf("frontMatter = %v", fm)
	}
}

func TestWriteAtomicErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(blocker, "x.md"), "x"); err == nil || !strings.Contains(err.Error(), "creating vault dir") {
		t.Fatalf("mkdir err = %v", err)
	}
	tmpDir := filepath.Join(dir, "a.md.tmp")
	if err := os.Mkdir(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(dir, "a.md"), "x"); err == nil || !strings.Contains(err.Error(), "writing fact") {
		t.Fatalf("write err = %v", err)
	}
	target := filepath.Join(dir, "b.md")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(target, "x"); err == nil || !strings.Contains(err.Error(), "renaming fact") {
		t.Fatalf("rename err = %v", err)
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file left behind after failed rename")
	}
	// A fact write into a vault path that cannot be created surfaces the error.
	w := &FactWriter{Dir: filepath.Join(blocker, "vault")}
	if _, _, err := w.Write(ConnectorConfig{Name: "a", Type: "t"}, Page{ID: "p"}); err == nil {
		t.Fatal("Write into an impossible dir succeeded")
	}
}

func TestSortedAttrs(t *testing.T) {
	got := sortedAttrs(map[string]string{"a b": "first", "a-b": "second", "Zed": "z", "--": "dropped"})
	if len(got) != 2 || got[0] != [2]string{"a_b", "first"} || got[1] != [2]string{"zed", "z"} {
		t.Fatalf("sortedAttrs = %v", got)
	}
	if len(sortedAttrs(nil)) != 0 {
		t.Fatal("nil attrs produced pairs")
	}
}
