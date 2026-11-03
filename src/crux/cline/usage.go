package cline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const UsageRelativeDir = ".local/state/crux/cline/usage"
const usageRetention = 30 * 24 * time.Hour

// usageFile writes the same normalized session shape Hive's existing token
// collector reads. One cumulative record per turn avoids double counting final
// results after streaming usage updates, and records spend even on failed turns.
func usageFile(parent, turn, model, agent string) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		if len(raw) == 0 || string(raw) == "null" {
			return nil
		}
		var usage struct {
			Input       int64  `json:"inputTokens"`
			Output      int64  `json:"outputTokens"`
			Read        int64  `json:"cacheReadTokens"`
			Write       int64  `json:"cacheWriteTokens"`
			TotalInput  *int64 `json:"totalInputTokens"`
			TotalOutput *int64 `json:"totalOutputTokens"`
			TotalRead   *int64 `json:"totalCacheReadTokens"`
			TotalWrite  *int64 `json:"totalCacheWriteTokens"`
		}
		if err := json.Unmarshal(raw, &usage); err != nil {
			return err
		}
		if usage.TotalInput != nil {
			usage.Input = *usage.TotalInput
		}
		if usage.TotalOutput != nil {
			usage.Output = *usage.TotalOutput
		}
		if usage.TotalRead != nil {
			usage.Read = *usage.TotalRead
		}
		if usage.TotalWrite != nil {
			usage.Write = *usage.TotalWrite
		}
		record := struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			Agent     string `json:"agent"`
			Backend   string `json:"backend"`
			Model     string `json:"model"`
			Timestamp string `json:"timestamp"`
			Input     int64  `json:"input_tokens"`
			Output    int64  `json:"output_tokens"`
			Read      int64  `json:"cache_read"`
			Write     int64  `json:"cache_creation"`
		}{"usage", "assistant", agent, "cline", model, time.Now().UTC().Format(time.RFC3339Nano),
			usage.Input, usage.Output, usage.Read, usage.Write}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		dir := filepath.Join(parent, "usage")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		path := filepath.Join(dir, turn+".jsonl")
		if err := os.WriteFile(path+".writing", append(encoded, '\n'), 0o600); err != nil {
			return err
		}
		if err := os.Rename(path+".writing", path); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "turn-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > usageRetention {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
		return nil
	}
}
