package cline

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageCumulativeFinalAndFailure(t *testing.T) {
	root := t.TempDir()
	record := usageFile(root, "turn-test", "openrouter/anthropic/test", "coder")
	stream := `{"type":"agent_event","event":{"type":"usage","inputTokens":2,"outputTokens":1,"totalInputTokens":9,"totalOutputTokens":3,"totalCacheReadTokens":4,"totalCacheWriteTokens":5}}` + "\n"
	stream += `{"type":"run_result","finishReason":"completed","text":"done","usage":{"inputTokens":9,"outputTokens":3,"cacheReadTokens":4,"cacheWriteTokens":5}}` + "\n"
	if err := renderUsage(strings.NewReader(stream), &bytes.Buffer{}, record); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "usage"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("duplicate usage: %v %v", entries, err)
	}
	path := filepath.Join(root, "usage", "turn-test.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var row struct {
		Input   int    `json:"input_tokens"`
		Output  int    `json:"output_tokens"`
		Read    int    `json:"cache_read"`
		Write   int    `json:"cache_creation"`
		Agent   string `json:"agent"`
		Backend string `json:"backend"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	if row.Input != 9 || row.Output != 3 || row.Read != 4 || row.Write != 5 ||
		row.Agent != "coder" || row.Backend != "cline" || row.Model != "openrouter/anthropic/test" {
		t.Fatalf("wrong normalized usage: %+v", row)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
	failed := strings.Split(stream, "\n")[0] + "\n" + `{"type":"error","message":"secret"}`
	if err := renderUsage(strings.NewReader(failed), &bytes.Buffer{}, record); err == nil {
		t.Fatal("failed run accepted")
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "secret") || !strings.Contains(string(data), `"input_tokens":9`) {
		t.Fatalf("failed turn lost sanitized spend: %s", data)
	}
}

func TestUsageRetentionAndErrors(t *testing.T) {
	root := t.TempDir()
	record := usageFile(root, "turn-old", "provider/model", "coder")
	if err := record(json.RawMessage(`{"inputTokens":1}`)); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "usage", "turn-old.jsonl")
	past := time.Now().Add(-usageRetention - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := usageFile(root, "turn-new", "provider/model", "coder")(json.RawMessage(`{"inputTokens":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old usage retained: %v", err)
	}
	if err := record(json.RawMessage("null")); err != nil {
		t.Fatal(err)
	}
	if err := record(json.RawMessage("{")); err == nil {
		t.Fatal("malformed usage accepted")
	}
	for _, stream := range []string{
		`{"type":"run_result","finishReason":"completed","usage":3}`,
		`{"type":"agent_event","event":{"type":"usage","inputTokens":"invalid"}}`,
	} {
		if err := renderUsage(strings.NewReader(stream), &bytes.Buffer{}, record); err == nil {
			t.Fatal("usage schema drift silently dropped spend", stream)
		}
	}
	blocked := filepath.Join(root, "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := usageFile(blocked, "turn-test", "provider/model", "coder")(json.RawMessage("{}")); err == nil {
		t.Fatal("usage IO failure ignored")
	}
}
