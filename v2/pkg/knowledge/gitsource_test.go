package knowledge

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateGitSourceURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "https", raw: "https://github.com/org/repo.git"},
		{name: "http", raw: "http://example.com/org/repo"},
		{name: "file scheme", raw: "file:///etc/passwd", wantErr: true},
		{name: "ssh scheme", raw: "ssh://github.com/org/repo.git", wantErr: true},
		{name: "git scheme", raw: "git://github.com/org/repo.git", wantErr: true},
		{name: "ext helper", raw: "ext::sh -c 'id'", wantErr: true},
		{name: "scp like", raw: "git@github.com:o/r.git", wantErr: true},
		{name: "empty host", raw: "https:///org/repo.git", wantErr: true},
		{name: "leading dash", raw: "--upload-pack=/bin/sh", wantErr: true},
		{name: "transport helper separator", raw: "https://github.com/org/repo::helper", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGitSourceURL(tc.raw)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateGitSourceURL(%q) succeeded, want error", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateGitSourceURL(%q) error = %v", tc.raw, err)
			}
		})
	}
}

func TestGitSourceSlug(t *testing.T) {
	tests := []struct {
		url     string
		subpath string
		want    string
	}{
		{
			url:  "https://github.com/projectbluefin/dakota",
			want: "github.com_projectbluefin_dakota",
		},
		{
			url:     "https://github.com/projectbluefin/dakota",
			subpath: "docs/skills",
			want:    "github.com_projectbluefin_dakota__docs_skills",
		},
		{
			url:  "https://github.com/org/repo.git",
			want: "github.com_org_repo",
		},
	}

	for _, tc := range tests {
		got := gitSourceSlug(tc.url, tc.subpath)
		if got != tc.want {
			t.Errorf("gitSourceSlug(%q, %q) = %q, want %q", tc.url, tc.subpath, got, tc.want)
		}
	}
}

func TestNewGitSource_Defaults(t *testing.T) {
	config := GitSourceConfig{
		Name:  "test",
		URL:   "https://github.com/org/repo",
		Layer: LayerProject,
	}

	gs := NewGitSource(config, "/tmp/test-knowledge", slog.Default())

	if gs.Config().Branch != "main" {
		t.Errorf("default branch should be 'main', got %q", gs.Config().Branch)
	}
	if gs.Ready() {
		t.Error("should not be ready before Init")
	}
	if gs.Store() != nil {
		t.Error("store should be nil before Init")
	}
}

func TestNewGitSource_WithSubpath(t *testing.T) {
	config := GitSourceConfig{
		Name:    "skills",
		URL:     "https://github.com/org/repo",
		Subpath: "docs/skills",
		Layer:   LayerProject,
	}

	gs := NewGitSource(config, "/tmp/test-knowledge", slog.Default())

	expectedClone := "/tmp/test-knowledge/git-sources/github.com_org_repo__docs_skills"
	if gs.CloneDir() != expectedClone {
		t.Errorf("clone dir = %q, want %q", gs.CloneDir(), expectedClone)
	}
}

func TestGitSource_InitWithLocalDir(t *testing.T) {
	tmpDir := t.TempDir()
	docsDir := filepath.Join(tmpDir, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write some test markdown files
	files := map[string]string{
		"getting-started.md": "# Getting Started\n\nThis is a guide to getting started.\n\n#setup #onboarding",
		"debugging.md":       "# Debugging\n\nHow to debug common issues.\n\n#debug #troubleshooting",
		"ci.md":              "# CI Pipeline\n\nContinuous integration setup.\n\n#ci #automation",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(docsDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Init a git repo so isGitRepo returns true
	initGitRepo(t, tmpDir)

	config := GitSourceConfig{
		Name:    "test-docs",
		URL:     "file://" + tmpDir,
		Subpath: "docs",
		Layer:   LayerProject,
	}

	// Since it's already cloned (we made the dir), we manually set up the source
	// to test the FileStore indexing path
	logger := slog.Default()
	gs := &GitSource{
		config:   config,
		cloneDir: tmpDir,
		indexDir: docsDir,
		logger:   logger,
	}

	store, err := NewFileStore(docsDir, "test-docs", logger)
	if err != nil {
		t.Fatalf("creating FileStore: %v", err)
	}
	gs.store = store
	gs.ready = true

	if !gs.Ready() {
		t.Fatal("should be ready after manual setup")
	}
	if gs.Store() == nil {
		t.Fatal("store should not be nil")
	}

	stats := gs.Store().Stats()
	if stats.TotalPages != 3 {
		t.Errorf("expected 3 pages, got %d", stats.TotalPages)
	}

	// Search should work with Fisher-Rao scoring
	results := gs.Store().Search("debugging troubleshooting", 10)
	if len(results) == 0 {
		t.Error("expected search results for 'debugging troubleshooting'")
	}
	if results[0].Title != "Debugging" {
		t.Errorf("expected 'Debugging' as top result, got %q", results[0].Title)
	}

	// Info should reflect the state
	info := gs.Info()
	if info.Pages != 3 {
		t.Errorf("info.Pages = %d, want 3", info.Pages)
	}
	if info.Layer != LayerProject {
		t.Errorf("info.Layer = %s, want project", info.Layer)
	}
}

func TestGitSource_Info(t *testing.T) {
	config := GitSourceConfig{
		Name:    "dakota-skills",
		URL:     "https://github.com/projectbluefin/dakota",
		Branch:  "main",
		Subpath: "docs/skills",
		Layer:   LayerProject,
	}

	gs := NewGitSource(config, "/tmp/test-base", slog.Default())
	info := gs.Info()

	if info.Name != "dakota-skills" {
		t.Errorf("name = %q, want 'dakota-skills'", info.Name)
	}
	if info.Ready {
		t.Error("should not be ready before Init")
	}
	if info.Pages != 0 {
		t.Errorf("pages should be 0 before Init, got %d", info.Pages)
	}
}

// TestGitSource_CloneDakota is an integration test that clones the real dakota
// repo with sparse checkout. Skip in CI or when network is unavailable.
func TestGitSource_CloneDakota(t *testing.T) {
	if os.Getenv("HIVE_INTEGRATION_TESTS") == "" {
		t.Skip("set HIVE_INTEGRATION_TESTS=1 to run integration tests")
	}

	tmpDir := t.TempDir()
	config := GitSourceConfig{
		Name:    "dakota-skills",
		URL:     "https://github.com/projectbluefin/dakota",
		Branch:  "main",
		Subpath: "docs/skills",
		Layer:   LayerProject,
	}

	gs := NewGitSource(config, tmpDir, slog.Default())
	ctx := context.Background()

	if err := gs.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if !gs.Ready() {
		t.Fatal("should be ready after Init")
	}

	stats := gs.Store().Stats()
	if stats.TotalPages < 10 {
		t.Errorf("expected at least 10 skills docs, got %d", stats.TotalPages)
	}

	results := gs.Store().Search("packaging rust", 5)
	if len(results) == 0 {
		t.Error("expected search results for 'packaging rust'")
	}

	t.Logf("dakota-skills: %d pages indexed, top result for 'packaging rust': %s",
		stats.TotalPages, results[0].Title)
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("creating .git dir: %v", err)
	}
}
