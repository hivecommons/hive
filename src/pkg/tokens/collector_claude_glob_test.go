package tokens

// TestCollector_ScanClaudeGlob covers the per-agent CLAUDE_CONFIG_DIR
// transcript roots (kubestellar/hive#4596): transcripts no longer all land in
// the one shared /data/home/.claude/projects, so the collector scans a glob of
// per-agent roots in addition to the legacy dir. The glob is evaluated at scan
// time, so an agent added while the hive is running is picked up on the next
// pass with no restart.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollector_ScanClaudeGlob(t *testing.T) {
	root := t.TempDir()

	// Two per-agent config dirs with one transcript each, plus one non-matching
	// sibling dir that must NOT be scanned.
	writeTranscript := func(cfgDir, session string) {
		projects := filepath.Join(root, cfgDir, "projects", "-data-agents-x")
		if err := os.MkdirAll(projects, 0o755); err != nil {
			t.Fatal(err)
		}
		content := `{"type":"human","timestamp":"2025-01-01T00:00:00Z","message":{"text":"triage bugs"}}
{"type":"assistant","timestamp":"2025-01-01T00:01:00Z","message":{"model":"sonnet","usage":{"input_tokens":200,"output_tokens":100}}}
`
		if err := os.WriteFile(filepath.Join(projects, session+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct session file names: MergeAggregates dedupes by session ID
	// (derived from the file name), which is what guarantees a transcript can
	// never be double-counted even if it shows up under two scanned roots.
	writeTranscript(".claude-scanner", "sess-aaa")
	writeTranscript(".claude-quality", "sess-bbb")
	writeTranscript("unrelated", "sess-ccc") // outside the glob

	c := NewCollector(t.TempDir(), testLogger())
	c.SetPersistPath(filepath.Join(t.TempDir(), "snap.json"))
	c.SetClaudeSessionsGlob(filepath.Join(root, ".claude-*", "projects"))
	c.scan()

	summary := c.Summary()
	if summary == nil {
		t.Fatal("nil summary")
	}
	if summary.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2 (both per-agent roots, not the unrelated dir)", summary.SessionCount)
	}
	if summary.TotalTokens != 600 {
		t.Errorf("TotalTokens = %d, want 600", summary.TotalTokens)
	}

	// An unset glob must scan nothing extra — the field is opt-in.
	c2 := NewCollector(t.TempDir(), testLogger())
	c2.SetPersistPath(filepath.Join(t.TempDir(), "snap.json"))
	c2.scan()
	if s2 := c2.Summary(); s2 != nil && s2.SessionCount != 0 {
		t.Errorf("no-glob SessionCount = %d, want 0", s2.SessionCount)
	}
}
