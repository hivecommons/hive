package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewBotPriority(t *testing.T) {
	bodies := []string{"![P0 Badge](url)", "![P1 Badge](url)", "![P2 Badge](url)", "![P3 Badge](url)", "no badge", "P2: plain text", "![P4 Badge](url)", "![P1 Badge](url) ![P3 Badge](url)"}
	for _, threshold := range []string{"", "P0", "P1", "P2", "P3", " p1 ", "typo"} {
		r := ReviewBotsConfig{MinPriority: threshold}
		limit := -1
		switch threshold {
		case "P0":
			limit = 0
		case "P1", " p1 ":
			limit = 1
		case "P2":
			limit = 2
		case "P3":
			limit = 3
		}
		for i, body := range bodies {
			want := limit < 0 || i >= 4 || i <= limit
			if i == 7 {
				want = limit < 0 || limit >= 1
			}
			if got := r.IncludesPriority(body); got != want {
				t.Errorf("%q %q: got %v want %v", threshold, body, got, want)
			}
		}
	}
}

func TestReviewBotPriorityConfigFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project.yaml")
	if err := os.WriteFile(path, []byte("classification:\n  review_bots:\n    logins: [Copilot]\n    min_priority: P1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	got, err := cfg.EffectiveReviewBots(path)
	if err != nil || got.MinPriority != "P1" {
		t.Fatalf("project fallback: %+v %v", got, err)
	}
	cfg.Classification.ReviewBots = ReviewBotsConfig{Logins: []string{"Copilot"}, MinPriority: "P0"}
	got, err = cfg.EffectiveReviewBots(path)
	if err != nil || got.MinPriority != "P0" {
		t.Fatalf("hive override: %+v %v", got, err)
	}
}
