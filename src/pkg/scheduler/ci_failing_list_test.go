package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// buildCIFailingList renders the "CI failing" section of a kick from the
// metrics snapshot at ciFailingPath. These tests pin the three contracts the
// kick template depends on: absent/unreadable file → "(none)\n", malformed or
// empty payload → "(none)\n", and a populated payload → one formatted line per
// PR carrying number, repo, author, head SHA, and title.

func newCIFailingScheduler(t *testing.T) *Scheduler {
	t.Helper()
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console"}},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func overrideCIFailingPath(t *testing.T) string {
	t.Helper()
	orig := ciFailingPath
	ciFailingPath = filepath.Join(t.TempDir(), "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = orig })
	return ciFailingPath
}

func TestBuildCIFailingList_MissingFile(t *testing.T) {
	s := newCIFailingScheduler(t)
	overrideCIFailingPath(t) // path exists in no filesystem yet

	if got := s.buildCIFailingList(); got != "(none)\n" {
		t.Fatalf("missing snapshot should render %q, got %q", "(none)\n", got)
	}
}

func TestBuildCIFailingList_MalformedJSON(t *testing.T) {
	s := newCIFailingScheduler(t)
	path := overrideCIFailingPath(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := s.buildCIFailingList(); got != "(none)\n" {
		t.Fatalf("malformed snapshot should render %q, got %q", "(none)\n", got)
	}
}

func TestBuildCIFailingList_EmptyItems(t *testing.T) {
	s := newCIFailingScheduler(t)
	path := overrideCIFailingPath(t)
	if err := os.WriteFile(path, []byte(`{"ci_failing":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := s.buildCIFailingList(); got != "(none)\n" {
		t.Fatalf("empty snapshot should render %q, got %q", "(none)\n", got)
	}
}

func TestBuildCIFailingList_FormatsEachPR(t *testing.T) {
	s := newCIFailingScheduler(t)
	path := overrideCIFailingPath(t)
	const fixture = `{"ci_failing":[
		{"number":101,"repo":"test-org/console","title":"fix retry loop","author":"alice","head_sha":"abc1234"},
		{"number":202,"repo":"test-org/api","title":"tighten auth","author":"bob","head_sha":"def5678"}
	]}`
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	got := s.buildCIFailingList()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("rendered %d lines, want 2: %q", len(lines), got)
	}
	want := []string{
		"  #101 test-org/console by @alice (sha:abc1234) — fix retry loop",
		"  #202 test-org/api by @bob (sha:def5678) — tighten auth",
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("line %d = %q, want %q", i, lines[i], w)
		}
	}
}
