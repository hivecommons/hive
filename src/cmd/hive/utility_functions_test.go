package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestLoginCommandForBackend(t *testing.T) {
	cases := []struct {
		backend string
		want    string
	}{
		{"claude", "Run: claude login"},
		{"copilot", "Run: copilot auth login"},
		{"gemini", "Run: gemini auth login"},
		{"goose", "Run: goose auth login"},
		{"unknown-backend", "Run the login command for unknown-backend"},
		{"", "Run the login command for "},
	}
	for _, tc := range cases {
		got := loginCommandForBackend(tc.backend)
		if got != tc.want {
			t.Errorf("loginCommandForBackend(%q) = %q, want %q", tc.backend, got, tc.want)
		}
	}
}

func TestActionableIssueRefPinsGitHubAndWorksourceIdentity(t *testing.T) {
	cases := []struct {
		name  string
		issue github.Issue
		want  string
	}{
		{"github", github.Issue{Repo: "hivecommons/hive", Number: 42, ExternalID: "42"}, "hivecommons/hive#42"},
		{"linear", github.Issue{Repo: "hivecommons/hive", SourceType: "linear", ExternalID: "ENG-7"}, "hivecommons/hive!ENG-7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := actionableIssueRef(tc.issue); got != tc.want {
				t.Fatalf("actionableIssueRef() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseEndpointList(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"single endpoint", "http://localhost:8080", []string{"http://localhost:8080"}},
		{"multiple endpoints", "http://a:8080, http://b:8080", []string{"http://a:8080", "http://b:8080"}},
		{"trims spaces", "  http://a , http://b  ", []string{"http://a", "http://b"}},
		{"empty string returns empty", "", []string{}},
		{"all-commas returns empty", ",,,", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseEndpointList(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("parseEndpointList(%q) len = %d, want %d", tc.raw, len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseEndpointList(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSameStringSlice(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both nil", nil, nil, true},
		{"both empty", []string{}, []string{}, true},
		{"equal", []string{"a", "b"}, []string{"a", "b"}, true},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"different content", []string{"a", "b"}, []string{"a", "c"}, false},
		{"order matters", []string{"a", "b"}, []string{"b", "a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameStringSlice(tc.a, tc.b); got != tc.want {
				t.Errorf("sameStringSlice(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestParseColorInt(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"#3498db", 0x3498db},
		{"3498db", 0x3498db},
		{"#000000", 0},
		{"#ffffff", 0xffffff},
		{"", 0x95a5a6}, // default grey
	}
	for _, tc := range cases {
		got := parseColorInt(tc.input)
		if got != tc.want {
			t.Errorf("parseColorInt(%q) = 0x%x, want 0x%x", tc.input, got, tc.want)
		}
	}
}

func TestStringFromMap(t *testing.T) {
	m := map[string]interface{}{
		"name":   "hive",
		"count":  42,
		"nested": map[string]string{"x": "y"},
	}
	if got := stringFromMap(m, "name"); got != "hive" {
		t.Errorf("stringFromMap key=name got %q, want %q", got, "hive")
	}
	if got := stringFromMap(m, "count"); got != "" {
		t.Errorf("stringFromMap key=count (int) got %q, want empty", got)
	}
	if got := stringFromMap(m, "missing"); got != "" {
		t.Errorf("stringFromMap key=missing got %q, want empty", got)
	}
	if got := stringFromMap(nil, "any"); got != "" {
		t.Errorf("stringFromMap nil map got %q, want empty", got)
	}
}

func TestConfidenceToFloat(t *testing.T) {
	cases := []struct {
		input string
		want  float64
	}{
		{"high", 0.9},
		{"medium", 0.7},
		{"low", 0.4},
		{"unknown", 0.5},
		{"", 0.5},
	}
	for _, tc := range cases {
		got := confidenceToFloat(tc.input)
		if got != tc.want {
			t.Errorf("confidenceToFloat(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestInferACMMLevel(t *testing.T) {
	t.Run("explicit level", func(t *testing.T) {
		level := 3
		cfg := &config.Config{ACMMLevel: &level}
		if got := inferACMMLevel(cfg); got != 3 {
			t.Errorf("inferACMMLevel with explicit 3 = %d, want 3", got)
		}
	})
	t.Run("nil defaults to 1", func(t *testing.T) {
		cfg := &config.Config{ACMMLevel: nil}
		if got := inferACMMLevel(cfg); got != 1 {
			t.Errorf("inferACMMLevel with nil = %d, want 1", got)
		}
	})
}

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}
	for _, tc := range cases {
		got := parseLogLevel(tc.input)
		if got != tc.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestMergeableJSON(t *testing.T) {
	cases := []struct {
		input github.Mergeable
		want  string
	}{
		{github.MergeableUnknown, "unknown"},
		{github.MergeableYes, "yes"},
		{github.MergeableNo, "no"},
	}
	for _, tc := range cases {
		got := mergeableJSON(tc.input)
		if got != tc.want {
			t.Errorf("mergeableJSON(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	atomicWrite(path, []byte("hello world"))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("atomicWrite failed to create file: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("atomicWrite content = %q, want %q", string(data), "hello world")
	}

	// Overwrite existing
	atomicWrite(path, []byte("updated"))
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("atomicWrite overwrite failed: %v", err)
	}
	if string(data) != "updated" {
		t.Errorf("atomicWrite overwrite content = %q, want %q", string(data), "updated")
	}
}

func TestEnvOrDefault(t *testing.T) {
	key := "HIVE_TEST_ENV_OR_DEFAULT_XYZZY"
	os.Unsetenv(key)

	if got := envOrDefault(key, "fallback"); got != "fallback" {
		t.Errorf("envOrDefault unset = %q, want %q", got, "fallback")
	}

	os.Setenv(key, "actual")
	defer os.Unsetenv(key)

	if got := envOrDefault(key, "fallback"); got != "actual" {
		t.Errorf("envOrDefault set = %q, want %q", got, "actual")
	}
}
