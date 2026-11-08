package climemory

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

func copyFixture(t *testing.T, name, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(fixture(t, name)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixedNow() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }

func TestDrainWritesDraftsAndDedupes(t *testing.T) {
	home := t.TempDir()
	memDir := filepath.Join(home, ".claude", "projects", "-data-agents-scanner-hivecommons-hive", "memory")
	copyFixture(t, "claude-MEMORY.md", filepath.Join(memDir, "MEMORY.md"))
	copyFixture(t, "claude-topic.md", filepath.Join(memDir, "build-pipeline.md"))
	codexDir := filepath.Join(home, ".codex-scanner", "memories")
	copyFixture(t, "codex-memory_summary.md", filepath.Join(codexDir, "memory_summary.md"))
	gemini := filepath.Join(home, ".gemini", "GEMINI.md")
	copyFixture(t, "gemini-GEMINI.md", gemini)

	vault := t.TempDir()
	d := &Drainer{Dir: vault, Now: fixedNow}
	req := Request{
		Agent: "scanner",
		Repos: []string{"hivecommons/hive", "hivecommons/crux"},
		Sources: []Source{
			{CLI: CLIClaude, Path: memDir},
			{CLI: CLICodex, Path: codexDir},
			{CLI: CLIGemini, Path: gemini},
		},
	}

	first := d.Drain(req)
	if len(first.Errors) != 0 {
		t.Fatalf("errors: %v", first.Errors)
	}
	// 5 (MEMORY.md) + 3 (topic) + 3 (codex) + 2 (gemini); the MEMORY.md
	// index bullet for build-pipeline.md carries its own summary and so is
	// a distinct fact from the topic file's paragraphs.
	if first.Drained != 13 || first.Dupes != 0 {
		t.Fatalf("first drain = %+v", first)
	}
	files, _ := filepath.Glob(filepath.Join(vault, filePrefix+"*.md"))
	if len(files) != 13 {
		t.Fatalf("vault has %d entries, want 13", len(files))
	}

	second := d.Drain(req)
	if second.Drained != 0 || second.Dupes != 13 || len(second.Errors) != 0 {
		t.Fatalf("second drain = %+v", second)
	}
	files, _ = filepath.Glob(filepath.Join(vault, filePrefix+"*.md"))
	if len(files) != 13 {
		t.Fatalf("second drain changed the vault: %d entries", len(files))
	}

	// Rewrapping the same fact in the memory file is still a duplicate.
	if err := os.WriteFile(gemini, []byte("## Gemini Added Memories\n- The hive dashboard\n  listens on port 8080.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third := d.Drain(Request{Agent: "scanner", Sources: []Source{{CLI: CLIGemini, Path: gemini}}})
	if third.Drained != 0 || third.Dupes != 1 {
		t.Fatalf("rewrapped fact drained again: %+v", third)
	}
}

func TestDrainedEntryFrontmatter(t *testing.T) {
	home := t.TempDir()
	memDir := filepath.Join(home, ".claude", "projects", "-data-agents-scanner-hivecommons-hive", "memory")
	copyFixture(t, "claude-topic.md", filepath.Join(memDir, "build-pipeline.md"))
	vault := t.TempDir()
	d := &Drainer{Dir: vault, Now: fixedNow}
	res := d.Drain(Request{Agent: "scanner", Repos: []string{"hivecommons/hive"}, Sources: []Source{{CLI: CLIClaude, Path: memDir}}})
	if res.Drained != 3 || len(res.Errors) != 0 {
		t.Fatalf("drain = %+v", res)
	}
	want := Fact{Heading: "Build pipeline", Body: "Use the failing job log to fix, then push again."}
	data, err := os.ReadFile(filepath.Join(vault, EntryFilename(Hash(want.Body))))
	if err != nil {
		t.Fatalf("entry not written under its hash name: %v", err)
	}
	got := string(data)
	for _, line := range []string{
		"title: Build pipeline: Use the failing job log to fix, then push again\n",
		"type: reference\n",
		"layer: personal\n",
		"state: draft\n",
		"source: cli-memory\n",
		"cli: claude\n",
		"agent: scanner\n",
		"repo: hivecommons/hive\n",
		"memhash: " + Hash(want.Body) + "\n",
		"tags: [cli-memory, cli:claude, memhash:" + Hash(want.Body) + ", agent:scanner, repo:hivecommons/hive]\n",
		"drained: 2026-11-07T12:00:00Z\n",
		"---\n\n" + want.Body + "\n",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("entry missing %q:\n%s", line, got)
		}
	}
}

// Drained entries are drafts, and only drafts: the vault parser must read
// them back as draft so the Primer's approved-only default never primes
// them, and nothing in the package can emit another state.
func TestDrainedEntriesAreNeverApproved(t *testing.T) {
	vault := t.TempDir()
	memDir := filepath.Join(t.TempDir(), "memory")
	copyFixture(t, "claude-MEMORY.md", filepath.Join(memDir, "MEMORY.md"))
	copyFixture(t, "codex-memory_summary.md", filepath.Join(memDir, "codex.md"))

	store, err := knowledge.NewFileStore(vault, "test", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	d := &Drainer{Dir: vault, Store: store, Now: fixedNow}
	res := d.Drain(Request{Agent: "scanner", Sources: []Source{{CLI: CLIClaude, Path: memDir}}})
	if res.Drained == 0 || len(res.Errors) != 0 {
		t.Fatalf("drain = %+v", res)
	}
	all := store.ListPages("")
	if len(all) != res.Drained {
		t.Fatalf("store indexed %d pages, drained %d", len(all), res.Drained)
	}
	for _, f := range all {
		if f.EffectiveState() != knowledge.StateDraft {
			t.Errorf("%s has state %q, want draft", f.Slug, f.EffectiveState())
		}
	}
	if primable := knowledge.FilterByLifecycle(all, nil); len(primable) != 0 {
		t.Fatalf("%d drained entries pass the approved-only filter: %+v", len(primable), primable)
	}

	// A human approving one keeps it out of the next drain (hash still
	// present) and never flips the others.
	if err := store.SetLifecycleState(all[0].Slug, knowledge.StateApproved); err != nil {
		t.Fatal(err)
	}
	again := d.Drain(Request{Agent: "scanner", Sources: []Source{{CLI: CLIClaude, Path: memDir}}})
	if again.Drained != 0 || again.Dupes != res.Drained {
		t.Fatalf("drain after approval = %+v", again)
	}
	approved := 0
	for _, f := range store.ListPages("") {
		if f.EffectiveState() == knowledge.StateApproved {
			approved++
		}
	}
	if approved != 1 {
		t.Fatalf("%d approved entries after one manual approval", approved)
	}
}

func TestDrainSkipsSymlinkedAndMissingStores(t *testing.T) {
	shared := t.TempDir()
	copyFixture(t, "gemini-GEMINI.md", filepath.Join(shared, "GEMINI.md"))
	home := t.TempDir()
	link := filepath.Join(home, "GEMINI.md")
	if err := os.Symlink(filepath.Join(shared, "GEMINI.md"), link); err != nil {
		t.Skip("symlinks unavailable")
	}
	d := &Drainer{Dir: t.TempDir(), Now: fixedNow}
	res := d.Drain(Request{Agent: "a", Sources: []Source{
		{CLI: CLIGemini, Path: link},
		{CLI: CLICodex, Path: filepath.Join(home, "does-not-exist")},
	}})
	if res.Drained != 0 || res.Dupes != 0 || len(res.Errors) != 0 {
		t.Fatalf("symlinked or missing store drained: %+v", res)
	}
}

func TestRepoFromClaudeSlug(t *testing.T) {
	repos := []string{"hivecommons/hive", "kubestellar/console.ui"}
	tests := []struct {
		path, want string
	}{
		{"/data/home/agents/s/.claude/projects/-data-agents-s-hivecommons-hive/memory", "hivecommons/hive"},
		{"/data/home/agents/s/.claude/projects/-data-agents-s-kubestellar-console-ui/memory", "kubestellar/console.ui"},
		{"/data/home/agents/s/.claude/projects/-data-agents-s/memory", ""},
		{"/data/home/.codex-s/memories", ""},
	}
	for _, tc := range tests {
		if got := repoFromClaudeSlug(tc.path, repos); got != tc.want {
			t.Errorf("repoFromClaudeSlug(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
