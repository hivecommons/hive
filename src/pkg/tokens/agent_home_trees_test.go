package tokens

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Since hivecommons/hive#17031 every per-UID agent's ~/.claude and ~/.bob are
// REAL directories under <home>/agents/<name>, not bridges into the shared
// home, so the dashboard's claude_sessions_dir / bob_sessions_dir scans must
// also walk the per-agent trees or every new session goes uncounted.

func TestClaudeSessionTrees_DerivesPerAgentHomes(t *testing.T) {
	home := t.TempDir()
	shared := filepath.Join(home, ".claude", "projects")
	for _, a := range []string{"scanner", "reviewer", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(home, "agents", a, ".claude", "projects"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	// A per-agent home still carrying the legacy whole-directory bridge
	// resolves to the shared tree and must not be scanned a second time
	// (and under the wrong owner); a home with no .claude must not appear.
	if err := os.MkdirAll(filepath.Join(home, "agents", "legacy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".claude"), filepath.Join(home, "agents", "legacy", ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "agents", "inference-only"), 0o755); err != nil {
		t.Fatal(err)
	}

	trees := ClaudeSessionTrees(shared)
	if len(trees) != 3 {
		t.Fatalf("trees = %+v, want shared + reviewer + scanner", trees)
	}
	if trees[0].Dir != shared || trees[0].Agent != "" {
		t.Errorf("trees[0] = %+v, want shared dir with no agent", trees[0])
	}
	if trees[1].Agent != "reviewer" || trees[2].Agent != "scanner" {
		t.Errorf("per-agent trees = %+v", trees[1:])
	}
	if trees[2].Dir != filepath.Join(home, "agents", "scanner", ".claude", "projects") {
		t.Errorf("scanner dir = %q", trees[2].Dir)
	}
}

func TestClaudeSessionTrees_NonCanonicalDirIsSharedOnly(t *testing.T) {
	if got := ClaudeSessionTrees(""); got != nil {
		t.Errorf("empty dir → %+v, want nil", got)
	}
	got := ClaudeSessionTrees("/some/custom/projects")
	if len(got) != 1 || got[0].Dir != "/some/custom/projects" {
		t.Errorf("custom dir → %+v", got)
	}
}

func TestBobSessionTrees_DerivesPerAgentHomes(t *testing.T) {
	home := t.TempDir()
	shared := filepath.Join(home, ".bob")
	for _, a := range []string{"quality", "scanner"} {
		if err := os.MkdirAll(filepath.Join(home, "agents", a, ".bob"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	trees := BobSessionTrees(shared)
	if len(trees) != 3 || trees[0].Dir != shared || trees[1].Agent != "quality" || trees[2].Agent != "scanner" {
		t.Fatalf("trees = %+v", trees)
	}
}

// A Claude session in a per-agent tree is attributed to that agent even when
// its project directory name encodes no agent work dir (a worktree cwd), and
// the dashboard sees sessions from BOTH the shared and per-agent trees.
func TestCollectorScansPerAgentClaudeHomes(t *testing.T) {
	metricsDir := t.TempDir()
	home := t.TempDir()
	shared := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	line := func(in, out int64) string {
		return `{"type":"human","timestamp":"` + now + `","message":{"text":"please continue"}}` + "\n" +
			`{"type":"assistant","timestamp":"` + now + `","message":{"model":"claude-opus-5.5","usage":{"input_tokens":` +
			strconv.FormatInt(in, 10) + `,"output_tokens":` + strconv.FormatInt(out, 10) + `}}}` + "\n"
	}
	write := func(projectsDir, proj, session, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(projectsDir, proj), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projectsDir, proj, session+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Legacy shared tree: agent from the project-dir path hint.
	write(shared, "-data-agents-coder", "c1", line(100, 10))
	// Per-agent reviewer tree, worktree cwd: no path hint and no keyword, so
	// the home it sits in owns it (the shared scan would have said "unknown").
	write(filepath.Join(home, "agents", "reviewer", ".claude", "projects"), "-home-wt-pr-1", "r1", line(200, 20))
	// Per-agent scanner tree, canonical cwd: path hint and owner agree.
	write(filepath.Join(home, "agents", "scanner", ".claude", "projects"), "-data-agents-scanner", "s1", line(300, 30))

	c := NewCollector(metricsDir, testLogger())
	c.SetPersistPath(filepath.Join(metricsDir, "token-summary.json"))
	c.SetClaudeSessionsDir(shared)
	c.scan()

	summary := c.Summary()
	if summary == nil {
		t.Fatal("nil summary")
	}
	want := map[string]int64{"coder": 110, "reviewer": 220, "scanner": 330}
	for agent, tokens := range want {
		if got := summary.ByAgent[agent]; got != tokens {
			t.Errorf("ByAgent[%s] = %d, want %d (ByAgent=%v)", agent, got, tokens, summary.ByAgent)
		}
	}
}

// A bob recording in a per-agent ~/.bob whose project hash is not in that
// tree's trustedFolders.json is attributed to the tree owner, not "bob", and
// its error streak is reported under that agent.
func TestCollectorScansPerAgentBobHomes(t *testing.T) {
	metricsDir := t.TempDir()
	home := t.TempDir()
	shared := filepath.Join(home, ".bob")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	base := now.Add(-2 * time.Hour)
	qualityHome := filepath.Join(home, "agents", "quality", ".bob")
	if err := os.MkdirAll(qualityHome, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBobChat(t, qualityHome, "/home/wt/pr-7", "s1", map[string]any{
		"sessionId":   "s1",
		"startTime":   ts(base, 0),
		"lastUpdated": ts(base, 90),
		"messages": []map[string]any{
			{"type": "user", "timestamp": ts(base, 0), "content": "kick 1"},
			{"type": "bob-shell", "timestamp": ts(base, 1), "model": "premium",
				"tokens": map[string]int64{"input": 100, "output": 20}},
			{"type": "user", "timestamp": ts(base, 30), "content": "kick 2"},
			{"type": "bob-shell", "timestamp": ts(base, 31), "model": "premium",
				"tokens": map[string]int64{"input": 0, "output": 0}},
		},
	})

	c := NewCollector(metricsDir, testLogger())
	c.SetPersistPath(filepath.Join(metricsDir, "token-summary.json"))
	c.SetBobSessionsDir(shared)
	c.scan()

	summary := c.Summary()
	if summary == nil {
		t.Fatal("nil summary")
	}
	if got := summary.ByAgent["quality"]; got != 120 {
		t.Errorf("ByAgent[quality] = %d, want 120 (ByAgent=%v)", got, summary.ByAgent)
	}
	if _, leaked := summary.ByAgent[bobUnknownAgent]; leaked {
		t.Errorf("per-agent recording fell back to %q: %v", bobUnknownAgent, summary.ByAgent)
	}
	if streaks := c.AgentErrorStreaks(); streaks["quality"] != 1 {
		t.Errorf("AgentErrorStreaks = %v, want quality:1", streaks)
	}
}
