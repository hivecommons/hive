package config

import (
	"os"
	"strings"
	"testing"
)

func TestConfigGoLineCountRatchet(t *testing.T) {
	const maxConfigGoLines = 200

	data, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	lines := strings.Count(string(data), "\n")
	if lines > maxConfigGoLines {
		t.Fatalf("config.go has %d lines, want <= %d; keep topical config code in split files", lines, maxConfigGoLines)
	}
}
