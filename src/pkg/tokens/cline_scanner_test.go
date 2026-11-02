package tokens

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/crux/cline"
)

func TestClineSharedAndPerAgentUsage(t *testing.T) {
	root := t.TempDir()
	metrics := t.TempDir()
	for _, home := range []string{root, filepath.Join(root, "agents", "coder")} {
		dir := filepath.Join(home, cline.UsageRelativeDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, "turn-one.jsonl", `{"type":"usage","role":"assistant","agent":"coder","backend":"cline","model":"openrouter/anthropic/test","input_tokens":10,"output_tokens":5,"cache_read":2,"cache_creation":3,"timestamp":"2026-11-02T00:00:00Z"}`)
	}
	c := NewCollector(metrics, nil)
	c.SetPersistPath(filepath.Join(root, "summary.json"))
	c.SetClineHomesRoot(root)
	c.scan()
	summary := c.Summary()
	if summary.SessionCount != 2 || summary.TotalTokens != 40 || summary.ByAgent["coder"] != 40 {
		t.Fatalf("Cline usage missing/duplicated: %+v", summary)
	}
	for _, session := range summary.Sessions {
		if session.Backend != "cline" || session.UsageTotal() != session.TotalTokens {
			t.Fatalf("wrong provenance/timeline: %+v", session)
		}
	}
}
