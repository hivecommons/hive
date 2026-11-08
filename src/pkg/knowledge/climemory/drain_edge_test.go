package climemory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDrainRejectsMissingTarget(t *testing.T) {
	var nilDrainer *Drainer
	if res := nilDrainer.Drain(Request{}); len(res.Errors) != 1 || res.Drained != 0 {
		t.Fatalf("nil drainer: %+v", res)
	}
	if res := (&Drainer{}).Drain(Request{}); len(res.Errors) != 1 {
		t.Fatalf("empty Dir: %+v", res)
	}
	// Dir cannot be created because a regular file sits on its path.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := (&Drainer{Dir: filepath.Join(blocker, "vault")}).Drain(Request{})
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "creating") {
		t.Fatalf("unmkdirable Dir: %+v", res)
	}
}

func TestDrainCapsFactsPerDrain(t *testing.T) {
	home := t.TempDir()
	var sb strings.Builder
	for i := 0; i < maxFactsPerDrain+5; i++ {
		fmt.Fprintf(&sb, "- distinct remembered fact number %d\n", i)
	}
	mem := filepath.Join(home, "memory_summary.md")
	if err := os.WriteFile(mem, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Drainer{Dir: t.TempDir(), Now: fixedNow}
	res := d.Drain(Request{Agent: "a", Sources: []Source{{CLI: CLICodex, Path: home}}})
	if res.Drained != maxFactsPerDrain || res.Skipped != 5 || len(res.Errors) != 0 {
		t.Fatalf("cap not applied: drained=%d skipped=%d errs=%v", res.Drained, res.Skipped, res.Errors)
	}
	// The second drain picks up the five the cap left behind.
	res = d.Drain(Request{Agent: "a", Sources: []Source{{CLI: CLICodex, Path: home}}})
	if res.Drained != 5 || res.Dupes != maxFactsPerDrain || res.Skipped != 0 {
		t.Fatalf("second pass: drained=%d dupes=%d skipped=%d", res.Drained, res.Dupes, res.Skipped)
	}
}

func TestDrainReportsUnreadableSourceAndWriteFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission bits are not enforced for root")
	}
	home := t.TempDir()
	mem := filepath.Join(home, "notes.md")
	if err := os.WriteFile(mem, []byte("- a fact\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	d := &Drainer{Dir: t.TempDir(), Now: fixedNow}
	res := d.Drain(Request{Sources: []Source{{CLI: CLICodex, Path: mem}}})
	if res.Drained != 0 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "reading") {
		t.Fatalf("unreadable source: %+v", res)
	}

	// A read-only vault makes the atomic write fail per fact.
	ro := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "ok.md"), []byte("- another fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	res = (&Drainer{Dir: ro, Now: fixedNow}).Drain(Request{Sources: []Source{{CLI: CLICodex, Path: filepath.Join(home, "ok.md")}}})
	if res.Drained != 0 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "writing") {
		t.Fatalf("read-only vault: %+v", res)
	}

	// An unlistable vault fails before any source is read.
	if err := os.Chmod(ro, 0o000); err != nil {
		t.Fatal(err)
	}
	res = (&Drainer{Dir: ro}).Drain(Request{})
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "listing") {
		t.Fatalf("unlistable vault: %+v", res)
	}
}

func TestMemoryFilesFilters(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.md", "- a\n")
	write("sub/nested.md", "- b\n")
	write("draft.tmp.md", "- temp\n")
	write("notes.txt", "not markdown\n")
	write(".hidden/secret.md", "- hidden\n")
	write(".dotfile.md", "- hidden file\n")
	write("huge.md", strings.Repeat("x", maxMemoryFileBytes+1))

	files, err := memoryFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "keep.md"), filepath.Join(root, "sub", "nested.md")}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("memoryFiles = %v, want %v", files, want)
	}

	// Single-file sources: oversized or non-markdown yield nothing.
	for _, rel := range []string{"huge.md", "notes.txt"} {
		if got, err := memoryFiles(filepath.Join(root, rel)); err != nil || len(got) != 0 {
			t.Fatalf("memoryFiles(%s) = %v, %v", rel, got, err)
		}
	}
	if got, err := memoryFiles(filepath.Join(root, "keep.md")); err != nil || len(got) != 1 {
		t.Fatalf("memoryFiles(keep.md) = %v, %v", got, err)
	}

	// A path whose parent is a regular file is a real stat error, not ENOENT.
	if _, err := memoryFiles(filepath.Join(root, "keep.md", "child")); err == nil {
		t.Fatal("expected a stat error for a path under a regular file")
	}
}

func TestHashFromFrontmatter(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name, body, want string
	}{
		{"quoted", "---\ntitle: x\nmemhash: \"abc\"\n---\nbody", "abc"},
		{"single-quoted", "---\nmemhash: 'def'\n---\n", "def"},
		{"no frontmatter", "# just a page\nmemhash: nope\n", ""},
		{"key after closing fence", "---\ntitle: x\n---\nmemhash: nope\n", ""},
		{"unterminated without key", "---\ntitle: x\n", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		if got := hashFromFrontmatter(write(tc.name+".md", tc.body)); got != tc.want {
			t.Errorf("%s: hashFromFrontmatter = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := hashFromFrontmatter(filepath.Join(dir, "missing.md")); got != "" {
		t.Errorf("missing file: %q", got)
	}

	// existingHashes ignores subdirectories and non-markdown files.
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("stray.txt", "---\nmemhash: zzz\n---\n")
	seen, err := existingHashes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if seen["abc"] == "" || seen["def"] == "" || seen["zzz"] != "" || seen["nope"] != "" {
		t.Fatalf("existingHashes = %v", seen)
	}
}

func TestEntryFilenameAndList(t *testing.T) {
	long := strings.Repeat("a", 64)
	if got := EntryFilename(long); got != filePrefix+strings.Repeat("a", hashFilenameHex)+".md" {
		t.Fatalf("EntryFilename(long) = %q", got)
	}
	if got := EntryFilename("short"); got != filePrefix+"short.md" {
		t.Fatalf("EntryFilename(short) = %q", got)
	}
	if got := list([]string{"a,b", " ", "c\nd", ""}); got != "a b, c d" {
		t.Fatalf("list = %q", got)
	}
	if got := scalar("line one\r\nline  two"); got != "line one line two" {
		t.Fatalf("scalar = %q", got)
	}
}

func TestRenderEntryOmitsEmptyProvenance(t *testing.T) {
	entry := renderEntry(Fact{Body: "a fact"}, "h", entryMeta{cli: CLICodex, path: "/p", layer: "personal", at: fixedNow()})
	for _, forbidden := range []string{"\nagent:", "\nrepo:", "agent:", "repo:"} {
		if strings.Contains(entry, forbidden) {
			t.Fatalf("entry carries %q without a value:\n%s", forbidden, entry)
		}
	}
	if !strings.Contains(entry, "state: draft\n") || !strings.HasSuffix(entry, "a fact\n") {
		t.Fatalf("entry shape:\n%s", entry)
	}
}

func TestParseMarkdownStructure(t *testing.T) {
	src := strings.Join([]string{
		"# Project",
		"",
		"Intro paragraph",
		"spanning lines.",
		"",
		"---",
		"",
		"- parent item:",
		"  - nested one",
		"  - nested two",
		"  continuation of parent",
		"* second item",
		"1. numbered item",
		"",
		"Fenced:",
		"```sh",
		"",
		"echo  spaced",
		"```",
		"",
		"[index](topic.md)",
		"",
		"[summary](other.md) — kept summary",
		"",
	}, "\n")
	facts := ParseClaudeMemory(src)
	bodies := make([]string, 0, len(facts))
	for _, f := range facts {
		bodies = append(bodies, f.Body)
		if f.Heading != "Project" {
			t.Errorf("heading = %q for %q", f.Heading, f.Body)
		}
	}
	want := []string{
		"Intro paragraph spanning lines.",
		"parent item; nested one; nested two continuation of parent",
		"second item",
		"numbered item",
		"Fenced:\n```sh\n\necho  spaced\n```",
		"kept summary",
	}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("facts:\n%s\nwant:\n%s", strings.Join(bodies, "\n"), strings.Join(want, "\n"))
	}
	// Codex parsing keeps index-style links as ordinary facts.
	if got := ParseCodexMemories("[index](topic.md)\n"); len(got) != 1 || got[0].Body != "[index](topic.md)" {
		t.Fatalf("codex index link: %+v", got)
	}
	if got := ParseFor("unknown-cli", "- x\n"); len(got) != 1 {
		t.Fatalf("ParseFor fallback: %+v", got)
	}
}

func TestFactTitleEdges(t *testing.T) {
	tests := []struct {
		name string
		fact Fact
		want string
	}{
		{"question ends sentence", Fact{Body: "Why is CI slow? Because of shards."}, "Why is CI slow"},
		{"exclamation ends sentence", Fact{Body: "Never force push! Ever."}, "Never force push"},
		{"trailing period trimmed", Fact{Body: "Single sentence."}, "Single sentence"},
		{"markdown lead stripped", Fact{Body: "**bold** start"}, "bold** start"},
		{"heading match is case-insensitive", Fact{Heading: "build", Body: "Build with just"}, "Build with just"},
		{"long word boundary fallback", Fact{Body: strings.Repeat("x", maxTitleRunes+10)}, strings.Repeat("x", maxTitleRunes) + "…"},
		{"cut on exact space", Fact{Body: strings.Repeat("y", maxTitleRunes) + " tail"}, strings.Repeat("y", maxTitleRunes) + "…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fact.Title(); got != tc.want {
				t.Errorf("Title() = %q, want %q", got, tc.want)
			}
		})
	}
}
