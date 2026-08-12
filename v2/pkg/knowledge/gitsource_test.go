package knowledge

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateGitSourceURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "https", raw: "https://github.com/org/repo.git"},
		{name: "http", raw: "http://example.com/org/repo", wantErr: true},
		{name: "file scheme", raw: "file:///etc/passwd", wantErr: true},
		{name: "ssh scheme", raw: "ssh://github.com/org/repo.git", wantErr: true},
		{name: "git scheme", raw: "git://github.com/org/repo.git", wantErr: true},
		{name: "ftp scheme", raw: "ftp://example.com/org/repo.git", wantErr: true},
		{name: "ext helper", raw: "ext::sh -c 'id'", wantErr: true},
		{name: "scp like", raw: "git@github.com:o/r.git", wantErr: true},
		{name: "missing scheme", raw: "github.com/org/repo.git", wantErr: true},
		{name: "absolute local path", raw: "/data/hive.yaml", wantErr: true},
		{name: "invalid escape", raw: "https://example.com/%zz", wantErr: true},
		{name: "empty host", raw: "https:///org/repo.git", wantErr: true},
		{name: "leading dash", raw: "--upload-pack=/bin/sh", wantErr: true},
		{name: "transport helper separator", raw: "https://github.com/org/repo::helper", wantErr: true},
		{name: "transport helper separator in query", raw: "https://github.com/org/repo?ref=main::helper", wantErr: true},
		{name: "transport helper separator in userinfo", raw: "https://user::pass@github.com/org/repo", wantErr: true},
		// private/loopback SSRF guards
		{name: "loopback 127.0.0.1", raw: "http://127.0.0.1/repo.git", wantErr: true},
		{name: "localhost", raw: "https://localhost/repo.git", wantErr: true},
		{name: "RFC-1918 10.x", raw: "http://10.0.0.1/internal.git", wantErr: true},
		{name: "RFC-1918 192.168.x", raw: "http://192.168.1.1/private.git", wantErr: true},
		{name: "RFC-1918 172.16.x", raw: "http://172.16.0.1/repo.git", wantErr: true},
		{name: "link-local", raw: "http://169.254.169.254/latest/meta-data/", wantErr: true},
		{name: "IPv6 loopback", raw: "http://[::1]/repo.git", wantErr: true},
		{name: "all zeros", raw: "http://0.0.0.0/repo.git", wantErr: true},
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

func TestValidateGitSourceURL_HTTPRequiresPrivateOptIn(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	if err := ValidateGitSourceURL("http://git.internal.example/org/repo.git"); err != nil {
		t.Fatalf("explicit private-source opt-in should allow http git remotes, got %v", err)
	}
}

func TestValidateGitSourceURL_AllowsPublicLegacyIPv4Forms(t *testing.T) {
	// These forms are not net.ParseIP literals, so they reach the resolver.
	// Stub it so the test asserts the VALIDATOR's behaviour rather than whether
	// the machine running it happens to have working DNS.
	orig := gitSourceResolver
	gitSourceResolver = func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "134744072":
			return []string{"8.8.8.8"}, nil
		case "8.8.8":
			return []string{"8.8.0.8"}, nil
		}
		return nil, errors.New("unexpected host: " + host)
	}
	t.Cleanup(func() { gitSourceResolver = orig })

	tests := []string{
		"https://134744072/repo.git", // 8.8.8.8 as a single decimal integer.
		"https://8.8.8/repo.git",     // shortened dotted form: 8.8.0.8.
	}
	for _, raw := range tests {
		if err := ValidateGitSourceURL(raw); err != nil {
			t.Fatalf("expected public legacy IPv4 form %q to pass, got %v", raw, err)
		}
	}
}

// CORRECTION (audit F8). This case used to sit in the list above, commented
// "8.8.8.8 in dotted octal form" and asserted to PASS. That comment is wrong:
// Go's resolver — and glibc's, and Python's, all independently checked — read
// the leading zeros as DECIMAL, so 010.010.010.010 resolves to 10.10.10.10,
// which is RFC1918 PRIVATE. The old assertion therefore required the validator
// to accept an SSRF target, and it only "passed" because the literal-only check
// never resolved the name at all.
//
// Once the check resolves (F8), this form is correctly REJECTED. Pinning that
// here so the corrected behaviour cannot be mistaken for a regression and
// reverted.
func TestF8_DottedOctalFormIsPrivateAndRejected(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	orig := gitSourceResolver
	gitSourceResolver = func(_ context.Context, host string) ([]string, error) {
		if host == "010.010.010.010" {
			return []string{"10.10.10.10"}, nil
		}
		return nil, errors.New("unexpected host: " + host)
	}
	t.Cleanup(func() { gitSourceResolver = orig })

	if err := ValidateGitSourceURL("https://010.010.010.010/repo.git"); err == nil {
		t.Fatal("F8: 010.010.010.010 resolves to the RFC1918 address 10.10.10.10 and must be rejected as an SSRF target")
	}
}

func TestValidateGitSourceBranch(t *testing.T) {
	tests := []struct {
		name    string
		branch  string
		wantErr bool
	}{
		{name: "main", branch: "main"},
		{name: "release branch", branch: "release/v2"},
		{name: "empty", wantErr: true},
		{name: "flag-like", branch: "--upload-pack=/bin/sh", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGitSourceBranch(tc.branch)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateGitSourceBranch(%q) succeeded, want error", tc.branch)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateGitSourceBranch(%q) error = %v", tc.branch, err)
			}
		})
	}
}

func TestGitSourceEnvProtocolAllowlist(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	if got := gitSourceEnvValue(); got != "https" {
		t.Fatalf("default git env should allow only https, got %q", got)
	}

	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	if got := gitSourceEnvValue(); got != "https:http" {
		t.Fatalf("private-source opt-in should allow http for git subprocesses, got %q", got)
	}
}

func gitSourceEnvValue() string {
	const prefix = "GIT_ALLOW_PROTOCOL="
	value := ""
	for _, entry := range gitSourceEnv() {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
		}
	}
	return value
}

func TestGitSourceInitRejectsInvalidURL(t *testing.T) {
	gs := NewGitSource(GitSourceConfig{
		Name:  "bad",
		URL:   "ssh://github.com/org/repo.git",
		Layer: LayerProject,
	}, t.TempDir(), slog.Default())

	if err := gs.Init(context.Background()); err == nil {
		t.Fatal("expected invalid URL error")
	}
}

func TestGitSourceInitRejectsFlagLikeBranch(t *testing.T) {
	cloneDir := t.TempDir()
	initGitRepo(t, cloneDir)
	gs := NewGitSource(GitSourceConfig{
		Name:   "bad-branch",
		URL:    "https://github.com/org/repo.git",
		Branch: "--upload-pack=/bin/sh",
		Layer:  LayerProject,
	}, t.TempDir(), slog.Default())
	gs.cloneDir = cloneDir
	gs.indexDir = cloneDir

	if err := gs.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid branch") {
		t.Fatalf("expected invalid branch error before clone, got %v", err)
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
