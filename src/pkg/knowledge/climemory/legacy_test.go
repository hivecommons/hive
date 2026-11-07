package climemory

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyClaudeSources(t *testing.T) {
	if got, err := LegacyClaudeSources(filepath.Join(t.TempDir(), "missing")); err != nil || got != nil {
		t.Fatalf("missing tree = %v, %v; want none", got, err)
	}
	notDir := filepath.Join(t.TempDir(), "projects")
	writeFile(t, notDir, "not a directory")
	if _, err := LegacyClaudeSources(notDir); err == nil {
		t.Fatal("an unreadable projects tree must be reported, not treated as empty")
	}

	projects := t.TempDir()
	writeFile(t, filepath.Join(projects, "-data-agents-scanner", "memory", "MEMORY.md"), "- a\n")
	writeFile(t, filepath.Join(projects, "-data-agents-developer", "memory", "MEMORY.md"), "- b\n")
	writeFile(t, filepath.Join(projects, "-data-agents-nomem", "transcript.jsonl"), "{}\n")
	writeFile(t, filepath.Join(projects, "stray.md"), "- c\n")
	outside := filepath.Join(t.TempDir(), "memory")
	writeFile(t, filepath.Join(outside, "MEMORY.md"), "- d\n")
	if err := os.MkdirAll(filepath.Join(projects, "-data-agents-linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(projects, "-data-agents-linked", "memory")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(projects, "-data-agents-linkedproject")); err != nil {
		t.Fatal(err)
	}

	got, err := LegacyClaudeSources(projects)
	if err != nil {
		t.Fatal(err)
	}
	want := []Source{
		{CLI: CLIClaude, Path: filepath.Join(projects, "-data-agents-developer", "memory")},
		{CLI: CLIClaude, Path: filepath.Join(projects, "-data-agents-scanner", "memory")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %v, want %v (symlinked stores and dirs without memory skipped)", got, want)
	}
}

// TestTruncateDrainedOnlyEmptiesFullyStoredFiles proves the --truncate
// contract of the legacy drain: a file is emptied only when every fact in it
// is in the vault, so a fact the vault lacks is never lost.
func TestTruncateDrainedOnlyEmptiesFullyStoredFiles(t *testing.T) {
	projects := t.TempDir()
	stored := filepath.Join(projects, "-data-agents-scanner", "memory")
	pending := filepath.Join(projects, "-data-agents-developer", "memory")
	writeFile(t, filepath.Join(stored, "MEMORY.md"), "- Run go test with -run to narrow a failure.\n- The dashboard listens on port 8080.\n")
	writeFile(t, filepath.Join(stored, "empty.md"), "")
	writeFile(t, filepath.Join(pending, "MEMORY.md"), "- Never force-push a shared branch.\n")

	vault := t.TempDir()
	d := &Drainer{Dir: vault, Now: fixedNow}
	res := d.Drain(Request{Agent: LegacyAgent, Sources: []Source{{CLI: CLIClaude, Path: stored}}})
	if res.Drained != 2 || len(res.Errors) != 0 {
		t.Fatalf("drain = %+v", res)
	}
	entries, _ := filepath.Glob(filepath.Join(vault, filePrefix+"*.md"))
	for _, e := range entries {
		b, _ := os.ReadFile(e)
		if !strings.Contains(string(b), "agent:"+LegacyAgent) || !strings.Contains(string(b), "state: draft") {
			t.Fatalf("legacy entry %s lacks agent:%s or state draft:\n%s", e, LegacyAgent, b)
		}
	}

	sources := []Source{{CLI: CLIClaude, Path: stored}, {CLI: CLIClaude, Path: pending}}
	truncated, kept, err := d.TruncateDrained(sources)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(stored, "MEMORY.md")}; !reflect.DeepEqual(truncated, want) {
		t.Fatalf("truncated = %v, want %v", truncated, want)
	}
	if want := []string{filepath.Join(pending, "MEMORY.md")}; !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept = %v, want %v", kept, want)
	}
	if b, _ := os.ReadFile(filepath.Join(stored, "MEMORY.md")); len(b) != 0 {
		t.Fatalf("stored file not emptied: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(pending, "MEMORY.md")); !strings.Contains(string(b), "force-push") {
		t.Fatalf("file with an unstored fact was touched: %q", b)
	}

	if _, _, err := (&Drainer{}).TruncateDrained(sources); err == nil {
		t.Fatal("TruncateDrained without a vault dir must fail")
	}
	if _, _, err := (&Drainer{Dir: filepath.Join(t.TempDir(), "missing")}).TruncateDrained(sources); err == nil {
		t.Fatal("TruncateDrained with an unreadable vault must fail before touching any file")
	}
	if b, _ := os.ReadFile(filepath.Join(pending, "MEMORY.md")); len(b) == 0 {
		t.Fatal("a failed TruncateDrained emptied a memory file")
	}
}
