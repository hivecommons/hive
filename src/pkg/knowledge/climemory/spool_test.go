package climemory

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func writeMemory(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readEntry(t *testing.T, vault, body string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vault, EntryFilename(Hash(body))))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A second agent draining a fact already in the vault adds its agent: tag
// to the one entry instead of creating another, and never resets the state
// an operator set: a deprecated fact stays deprecated.
func TestDrainDedupAddsAgentTagAndNeverRevives(t *testing.T) {
	vault := t.TempDir()
	d := &Drainer{Dir: vault, Now: fixedNow}
	const fact = "The dashboard listens on port 8080."
	scannerMem := filepath.Join(t.TempDir(), "memories", "notes.md")
	writeMemory(t, scannerMem, "- "+fact+"\n")
	if res := d.Drain(Request{Agent: "scanner", Sources: []Source{{CLI: CLICodex, Path: scannerMem}}}); res.Drained != 1 || len(res.Errors) != 0 {
		t.Fatalf("first drain = %+v", res)
	}

	// The operator discards it.
	store, err := knowledge.NewFileStore(vault, "test", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	pages := store.ListPages("")
	if len(pages) != 1 {
		t.Fatalf("vault pages = %d", len(pages))
	}
	if err := store.SetLifecycleState(pages[0].Slug, knowledge.StateDeprecated); err != nil {
		t.Fatal(err)
	}

	geminiMem := filepath.Join(t.TempDir(), ".gemini", "GEMINI.md")
	writeMemory(t, geminiMem, "Operator context.\n\n## Gemini Added Memories\n- "+fact+"\n")
	for i := range 2 { // the second pass must be a no-op
		res := d.Drain(Request{Agent: "reviewer", Sources: []Source{{CLI: CLIGemini, Path: geminiMem}}})
		if res.Drained != 0 || res.Dupes != 1 || len(res.Errors) != 0 {
			t.Fatalf("pass %d: %+v", i, res)
		}
	}
	entries, _ := filepath.Glob(filepath.Join(vault, "*.md"))
	if len(entries) != 1 {
		t.Fatalf("vault has %d entries, want the one fact", len(entries))
	}
	entry := readEntry(t, vault, fact)
	if strings.Count(entry, "agent:reviewer") != 1 || !strings.Contains(entry, "agent:scanner") {
		t.Fatalf("tags not merged once:\n%s", entry)
	}
	store.Reindex()
	got := store.ListPages("")
	if len(got) != 1 || got[0].EffectiveState() != knowledge.StateDeprecated {
		t.Fatalf("discarded fact revived: %+v", got)
	}
	if len(knowledge.FilterByLifecycle(got, nil)) != 0 {
		t.Fatal("deprecated fact passes the approved-only filter after a re-drain")
	}
}

func TestAddAgentTag(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, in, want string
		changed        bool
		wantErr        bool
	}{
		{"appends", "---\ntitle: x\ntags: [cli-memory, agent:a]\n---\n\nbody\n", "---\ntitle: x\ntags: [cli-memory, agent:a, agent:b]\n---\n\nbody\n", true, false},
		{"already tagged", "---\ntags: [agent:b]\n---\nbody\n", "---\ntags: [agent:b]\n---\nbody\n", false, false},
		{"no tags line", "---\ntitle: x\n---\nbody\n", "---\ntitle: x\ntags: [agent:b]\n---\nbody\n", true, false},
		{"no frontmatter", "body only\n", "body only\n", false, false},
		{"block list", "---\ntags:\n  - a\n---\nbody\n", "---\ntags:\n  - a\n---\nbody\n", false, true},
	}
	for _, tc := range cases {
		p := filepath.Join(dir, tc.name+".md")
		writeMemory(t, p, tc.in)
		changed, err := addAgentTag(p, "b")
		if changed != tc.changed || (err != nil) != tc.wantErr {
			t.Errorf("%s: changed=%v err=%v", tc.name, changed, err)
		}
		if data, _ := os.ReadFile(p); string(data) != tc.want {
			t.Errorf("%s: file =\n%s\nwant\n%s", tc.name, data, tc.want)
		}
	}
	if changed, err := addAgentTag(filepath.Join(dir, "appends.md"), "../x"); changed || err != nil {
		t.Errorf("unsafe agent name: changed=%v err=%v", changed, err)
	}
}

// When the vault cannot be written, every memory file is copied into the
// agent's spool, so the clear_on_kick wipe that follows loses nothing;
// failing again replaces the copy rather than adding one. The next drain
// that can write stores the spooled facts with their original provenance
// and empties the spool.
func TestDrainSpoolsOnVaultFailureAndRetries(t *testing.T) {
	home := t.TempDir()
	memDir := filepath.Join(home, ".claude", "projects", "-data-agents-scanner-hivecommons-hive", "memory")
	writeMemory(t, filepath.Join(memDir, "MEMORY.md"), "- Always read the failing job log.\n- Sign every commit.\n")
	spool := t.TempDir()
	blocker := filepath.Join(t.TempDir(), "file")
	writeMemory(t, blocker, "x")
	req := Request{Agent: "scanner", Repos: []string{"hivecommons/hive"}, Sources: []Source{{CLI: CLIClaude, Path: memDir}}}

	broken := &Drainer{Dir: filepath.Join(blocker, "vault"), SpoolDir: spool, Now: fixedNow}
	for i := range 2 {
		res := broken.Drain(req)
		if res.Drained != 0 || res.Spooled != 1 || len(res.Errors) != 1 {
			t.Fatalf("failed drain %d = %+v", i, res)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(spool, "scanner")); len(entries) != 1 {
		t.Fatalf("spool entries after two failures = %d, want 1", len(entries))
	}
	// clear_on_kick wipes the store.
	if err := os.RemoveAll(memDir); err != nil {
		t.Fatal(err)
	}

	vault := t.TempDir()
	d := &Drainer{Dir: vault, SpoolDir: spool, Now: fixedNow}
	res := d.Drain(req)
	if res.Drained != 2 || res.Spooled != 0 || len(res.Errors) != 0 {
		t.Fatalf("retry drain = %+v", res)
	}
	entry := readEntry(t, vault, "Always read the failing job log.")
	for _, want := range []string{"state: draft", "repo: hivecommons/hive", "source_path: " + filepath.Join(memDir, "MEMORY.md")} {
		if !strings.Contains(entry, want) {
			t.Errorf("spooled entry lost %q:\n%s", want, entry)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(spool, "scanner")); len(left) != 0 {
		t.Fatalf("spool not emptied after a complete retry: %d entries", len(left))
	}
	if res := d.Drain(req); res.Drained != 0 || res.Dupes != 0 || len(res.Errors) != 0 {
		t.Fatalf("drain after retry = %+v", res)
	}
}

// A file whose facts only partly reach the vault (here: the per-drain cap)
// is spooled and completed from the spool on the next drain.
func TestDrainSpoolsPartialFile(t *testing.T) {
	mem := filepath.Join(t.TempDir(), "memories", "notes.md")
	var sb strings.Builder
	for i := range maxFactsPerDrain + 3 {
		fmt.Fprintf(&sb, "- distinct remembered fact number %d\n", i)
	}
	writeMemory(t, mem, sb.String())
	spool := t.TempDir()
	d := &Drainer{Dir: t.TempDir(), SpoolDir: spool, Now: fixedNow}
	req := Request{Agent: "scanner", Sources: []Source{{CLI: CLICodex, Path: mem}}}

	res := d.Drain(req)
	if res.Drained != maxFactsPerDrain || res.Skipped != 3 || res.Spooled != 1 {
		t.Fatalf("capped drain = %+v", res)
	}
	entries, _ := os.ReadDir(filepath.Join(spool, "scanner"))
	if len(entries) != 1 {
		t.Fatalf("spool entries = %d, want 1", len(entries))
	}

	// The store is wiped; the next drain completes the file from the spool.
	if err := os.Remove(mem); err != nil {
		t.Fatal(err)
	}
	res = d.Drain(req)
	if res.Drained != 3 || res.Dupes != maxFactsPerDrain || res.Spooled != 0 || len(res.Errors) != 0 {
		t.Fatalf("spool retry = %+v", res)
	}
	if entries, _ := os.ReadDir(filepath.Join(spool, "scanner")); len(entries) != 0 {
		t.Fatalf("spool not emptied: %d", len(entries))
	}
}

// A failed drain without a spool, or for an agent whose name is unsafe as a
// path, writes nothing outside the vault.
func TestDrainSpoolNeedsSafeAgentAndDir(t *testing.T) {
	mem := filepath.Join(t.TempDir(), "notes.md")
	writeMemory(t, mem, "- a fact\n")
	blocker := filepath.Join(t.TempDir(), "file")
	writeMemory(t, blocker, "x")
	spool := t.TempDir()
	for _, agent := range []string{"", "../escape", ".hidden"} {
		d := &Drainer{Dir: filepath.Join(blocker, "vault"), SpoolDir: spool}
		if res := d.Drain(Request{Agent: agent, Sources: []Source{{CLI: CLICodex, Path: mem}}}); res.Spooled != 0 {
			t.Errorf("agent %q spooled: %+v", agent, res)
		}
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("spool written for unsafe agents: %d entries", len(entries))
	}
}
