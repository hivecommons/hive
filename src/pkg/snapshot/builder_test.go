package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/hivecommons/hive/pkg/dashboard"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))
}

func minimalStatus() *dashboard.StatusPayload {
	return &dashboard.StatusPayload{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Governor: dashboard.FrontendGovernor{
			Active: true,
			Mode:   "IDLE",
			Issues: 0,
			PRs:    0,
		},
		Agents:        []dashboard.FrontendAgent{},
		Repos:         []dashboard.FrontendRepo{},
		Health:        map[string]any{},
		Budget:        dashboard.FrontendBudget{},
		CadenceMatrix: []dashboard.FrontendCadence{},
		GHRateLimits:  map[string]any{},
		AgentMetrics:  map[string]any{},
		Hold:          dashboard.FrontendHold{Items: []any{}},
		IssueToMerge:  map[string]any{},
	}
}

func richStatus() *dashboard.StatusPayload {
	return &dashboard.StatusPayload{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Governor: dashboard.FrontendGovernor{
			Active: true,
			Mode:   "SURGE",
			Issues: 7,
			PRs:    3,
		},
		Agents: []dashboard.FrontendAgent{
			{Name: "scanner", State: "running", CLI: "anthropic", Model: "claude-sonnet-4-6"},
			{Name: "fixer", State: "idle", CLI: "anthropic", Model: "claude-opus-4-5"},
		},
		Repos:         []dashboard.FrontendRepo{},
		Health:        map[string]any{},
		Budget:        dashboard.FrontendBudget{},
		CadenceMatrix: []dashboard.FrontendCadence{},
		GHRateLimits:  map[string]any{},
		AgentMetrics:  map[string]any{},
		Hold:          dashboard.FrontendHold{Items: []any{}},
		IssueToMerge:  map[string]any{},
	}
}

func globJSON(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "status-*.json"))
	if err != nil {
		t.Fatalf("glob failed: %v", err)
	}
	return matches
}

func TestNewBuilder_FieldsStored(t *testing.T) {
	dir := t.TempDir()
	logger := discardLogger()
	b := NewBuilder(dir, logger)

	if b.outputDir != dir {
		t.Errorf("outputDir: got %q, want %q", b.outputDir, dir)
	}
	if b.logger != logger {
		t.Error("logger not stored correctly")
	}
}

func TestBuild_CreatesOutputDirIfNotExists(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "snapshots", "nested")

	b := NewBuilder(dir, discardLogger())
	if err := b.Build(minimalStatus()); err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("output dir does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("output path is not a directory")
	}
}

func TestBuild_WritesTimestampedStatusJSON(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	status := richStatus()
	if err := b.Build(status); err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	files := globJSON(t, dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 status-*.json, got %d: %v", len(files), files)
	}

	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("reading status file: %v", err)
	}

	var got dashboard.StatusPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshaling status JSON: %v", err)
	}

	if got.Governor.Mode != status.Governor.Mode {
		t.Errorf("Governor.Mode: got %q, want %q", got.Governor.Mode, status.Governor.Mode)
	}
	if got.Governor.Issues != status.Governor.Issues {
		t.Errorf("Issues: got %d, want %d", got.Governor.Issues, status.Governor.Issues)
	}
	if got.Governor.PRs != status.Governor.PRs {
		t.Errorf("PRs: got %d, want %d", got.Governor.PRs, status.Governor.PRs)
	}
}

func TestBuild_WritesLatestJSON(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	status := richStatus()
	if err := b.Build(status); err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	latestPath := filepath.Join(dir, "latest.json")
	if _, err := os.Stat(latestPath); err != nil {
		t.Fatalf("latest.json not found: %v", err)
	}
}

func TestBuild_LatestJSONMatchesTimestampedFile(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	if err := b.Build(richStatus()); err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	latestData, err := os.ReadFile(filepath.Join(dir, "latest.json"))
	if err != nil {
		t.Fatalf("reading latest.json: %v", err)
	}

	files := globJSON(t, dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 status-*.json, got %d", len(files))
	}

	tsData, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("reading timestamped file: %v", err)
	}

	if string(latestData) != string(tsData) {
		t.Error("latest.json content differs from timestamped status file")
	}
}

func TestBuild_IndexHTMLContainsGovernorMode(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	status := richStatus()
	if err := b.Build(status); err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}

	if !strings.Contains(string(html), "SURGE") {
		t.Errorf("index.html does not contain governor mode %q", "SURGE")
	}
}

func TestBuild_IndexHTMLContainsIssuePRCounts(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	status := richStatus()
	if err := b.Build(status); err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	content := string(html)

	if !strings.Contains(content, "7") {
		t.Error("index.html does not contain issue count 7")
	}
	if !strings.Contains(content, "3") {
		t.Error("index.html does not contain PR count 3")
	}
}

func TestBuild_IndexHTMLContainsAgentNamesAndStates(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	status := richStatus()
	if err := b.Build(status); err != nil {
		t.Fatalf("Build() error: %v", err)
	}

	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	content := string(html)

	for _, agent := range status.Agents {
		if !strings.Contains(content, agent.Name) {
			t.Errorf("index.html missing agent name %q", agent.Name)
		}
		if !strings.Contains(content, agent.State) {
			t.Errorf("index.html missing agent state %q for agent %q", agent.State, agent.Name)
		}
	}
}

func TestBuild_MultipleCalls_CreateMultipleTimestampedFiles(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	const calls = 3
	seen := map[string]struct{}{}

	for i := 0; i < calls; i++ {
		before := globJSON(t, dir)
		if err := b.Build(minimalStatus()); err != nil {
			t.Fatalf("Build() call %d error: %v", i, err)
		}
		after := globJSON(t, dir)

		beforeSet := map[string]struct{}{}
		for _, f := range before {
			beforeSet[f] = struct{}{}
		}
		for _, f := range after {
			if _, exists := beforeSet[f]; !exists {
				seen[f] = struct{}{}
			}
		}

		if i < calls-1 {
			time.Sleep(time.Second)
		}
	}

	if len(seen) < calls {
		t.Errorf("expected %d distinct timestamped files, got %d", calls, len(seen))
	}
}

func TestBuild_EmptyAgents_Succeeds(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	status := minimalStatus()
	status.Governor.Mode = "QUIET"

	if err := b.Build(status); err != nil {
		t.Fatalf("Build() with empty agents error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "latest.json")); err != nil {
		t.Error("latest.json missing for empty agents build")
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		t.Error("index.html missing for empty agents build")
	}
	if files := globJSON(t, dir); len(files) != 1 {
		t.Errorf("expected 1 status-*.json, got %d", len(files))
	}
}

func TestCleanup_RemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	oldFile := filepath.Join(dir, "status-old.json")
	if err := os.WriteFile(oldFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("writing old file: %v", err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatalf("setting mtime: %v", err)
	}

	maxAge := time.Hour
	if err := b.Cleanup(maxAge); err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("old file was not removed by Cleanup")
	}
}

func TestCleanup_PreservesLatestJSONAndIndexHTML(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	protected := []string{"latest.json", "index.html"}
	past := time.Now().Add(-2 * time.Hour)
	for _, name := range protected {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(`placeholder`), 0644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatalf("setting mtime on %s: %v", name, err)
		}
	}

	if err := b.Cleanup(time.Hour); err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}

	for _, name := range protected {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("protected file %q was removed by Cleanup", name)
		}
	}
}

func TestCleanup_PreservesRecentFiles(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	recentFile := filepath.Join(dir, "status-recent.json")
	if err := os.WriteFile(recentFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("writing recent file: %v", err)
	}

	if err := b.Cleanup(time.Hour); err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}

	if _, err := os.Stat(recentFile); err != nil {
		t.Error("recent file was incorrectly removed by Cleanup")
	}
}

func TestCleanup_EmptyDirectory_NoError(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	if err := b.Cleanup(time.Hour); err != nil {
		t.Fatalf("Cleanup() on empty dir error: %v", err)
	}
}

func TestCleanup_MixedFiles_RemovesOldKeepsRecent(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	past := time.Now().Add(-3 * time.Hour)
	maxAge := time.Hour

	for _, name := range []string{"status-old1.json", "status-old2.json"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(`{}`), 0644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatalf("setting mtime: %v", err)
		}
	}

	recentPath := filepath.Join(dir, "status-recent.json")
	if err := os.WriteFile(recentPath, []byte(`{}`), 0644); err != nil {
		t.Fatalf("writing recent file: %v", err)
	}

	if err := b.Cleanup(maxAge); err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}

	for _, name := range []string{"status-old1.json", "status-old2.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("old file %q was not removed", name)
		}
	}

	if _, err := os.Stat(recentPath); err != nil {
		t.Error("recent file was incorrectly removed")
	}
}

func TestCleanup_SkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	subdir := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}

	if err := b.Cleanup(time.Nanosecond); err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}

	if _, err := os.Stat(subdir); err != nil {
		t.Error("subdirectory was removed by Cleanup")
	}
}

func TestBuildIndexHTML_ContainsTimestamp(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	const ts = "2026-05-11T12-00-00Z"
	status := &dashboard.StatusPayload{
		Governor: dashboard.FrontendGovernor{Mode: "BUSY", Issues: 2, PRs: 1},
		Agents:   []dashboard.FrontendAgent{},
	}
	indexPath := filepath.Join(dir, "index.html")
	if err := b.buildIndexHTML(indexPath, status, ts); err != nil {
		t.Fatalf("buildIndexHTML() error: %v", err)
	}

	html, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	if !strings.Contains(string(html), ts) {
		t.Errorf("index.html does not contain timestamp %q", ts)
	}
}

func TestBuildIndexHTML_AllGovernorModes(t *testing.T) {
	modes := []string{"SURGE", "BUSY", "QUIET", "IDLE"}

	for _, mode := range modes {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			b := NewBuilder(dir, discardLogger())

			status := &dashboard.StatusPayload{
				Governor: dashboard.FrontendGovernor{Mode: mode},
				Agents:   []dashboard.FrontendAgent{},
			}
			indexPath := filepath.Join(dir, "index.html")
			if err := b.buildIndexHTML(indexPath, status, "ts"); err != nil {
				t.Fatalf("buildIndexHTML() error: %v", err)
			}

			html, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatalf("reading index.html: %v", err)
			}
			if !strings.Contains(string(html), mode) {
				t.Errorf("index.html does not contain mode %q", mode)
			}
		})
	}
}

func TestBuild_ErrorWhenOutputDirIsFile(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatalf("writing blocker file: %v", err)
	}
	b := NewBuilder(filepath.Join(blocker, "subdir"), discardLogger())
	if err := b.Build(minimalStatus()); err == nil {
		t.Error("expected error when outputDir cannot be created, got nil")
	}
}

func TestBuild_ErrorWhenStatusFileNotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write to read-only dirs; skipping")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0755)

	b := NewBuilder(dir, discardLogger())
	if err := b.Build(minimalStatus()); err == nil {
		t.Error("expected error writing status file to read-only dir, got nil")
	}
}

func TestBuild_ErrorWhenLatestJSONIsDir(t *testing.T) {
	dir := t.TempDir()

	blocker := filepath.Join(dir, "latest.json")
	if err := os.MkdirAll(blocker, 0755); err != nil {
		t.Fatalf("creating blocker dir: %v", err)
	}

	b := NewBuilder(dir, discardLogger())
	err := b.Build(minimalStatus())
	if err == nil {
		t.Error("expected error when latest.json is a directory, got nil")
	}
}

func TestBuild_ErrorWhenIndexHTMLIsDir(t *testing.T) {
	dir := t.TempDir()

	blocker := filepath.Join(dir, "index.html")
	if err := os.MkdirAll(blocker, 0755); err != nil {
		t.Fatalf("creating blocker dir: %v", err)
	}

	b := NewBuilder(dir, discardLogger())
	err := b.Build(minimalStatus())
	if err == nil {
		t.Error("expected error when index.html is a directory, got nil")
	}
}

func TestCleanup_ErrorOnUnreadableDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read any dir; skipping")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0755)

	b := NewBuilder(dir, discardLogger())
	if err := b.Cleanup(time.Hour); err == nil {
		t.Error("expected error when ReadDir fails on unreadable dir, got nil")
	}
}

func TestBuild_SecondCall_UpdatesLatestJSON(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, discardLogger())

	s1 := minimalStatus()
	s1.Governor.Issues = 1
	if err := b.Build(s1); err != nil {
		t.Fatalf("first Build() error: %v", err)
	}

	time.Sleep(time.Second)

	s2 := minimalStatus()
	s2.Governor.Mode = "SURGE"
	s2.Governor.Issues = 99
	if err := b.Build(s2); err != nil {
		t.Fatalf("second Build() error: %v", err)
	}

	latestData, err := os.ReadFile(filepath.Join(dir, "latest.json"))
	if err != nil {
		t.Fatalf("reading latest.json: %v", err)
	}

	var got dashboard.StatusPayload
	if err := json.Unmarshal(latestData, &got); err != nil {
		t.Fatalf("unmarshaling latest.json: %v", err)
	}

	if got.Governor.Issues != 99 {
		t.Errorf("latest.json Issues: got %d, want 99", got.Governor.Issues)
	}
}
