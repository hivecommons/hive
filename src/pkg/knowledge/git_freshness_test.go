package knowledge

import (
	"context"
	"log/slog"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freshnessGit(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func freshnessCommit(t *testing.T, dir, date, file, body string) {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	freshnessGit(t, dir, nil, "add", "--", file)
	freshnessGit(t, dir, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", file)
}

func assertFreshness(t *testing.T, store *FileStore, slug, want string) {
	t.Helper()
	fact, err := store.ReadPage(slug)
	if err != nil {
		t.Fatal(err)
	}
	if want == "" {
		if !fact.Updated.IsZero() {
			t.Fatalf("%s: updated = %v, want unknown", slug, fact.Updated)
		}
		return
	}
	expected, err := time.Parse(time.RFC3339, want)
	if err != nil {
		t.Fatal(err)
	}
	if !fact.Updated.Equal(expected) {
		t.Fatalf("%s: updated = %v, want %v", slug, fact.Updated, expected)
	}
}

func TestGitSourceFreshnessCloneAndUpdates(t *testing.T) {
	remote := t.TempDir()
	freshnessGit(t, remote, nil, "init")
	freshnessCommit(t, remote, "2020-01-02T03:04:05Z", "docs/old.md", "---\nstatus: approved\nimported: 2099-01-01T00:00:00Z\n---\n# Old")
	freshnessCommit(t, remote, "2021-02-03T04:05:06Z", "docs/new.md", "# New")
	clone := filepath.Join(t.TempDir(), "clone")
	freshnessGit(t, "", nil, "clone", "--", remote, clone)
	store, err := newGitSourceFileStore(filepath.Join(clone, "docs"), clone, "docs", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	assertFreshness(t, store, "old", "2020-01-02T03:04:05Z")
	old, err := store.ReadPage("old")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(old.ConfidenceReason, "freshness >365d") {
		t.Fatalf("confidence must use the file commit, not imported timestamp: %q", old.ConfidenceReason)
	}
	assertFreshness(t, store, "new", "2021-02-03T04:05:06Z")

	freshnessCommit(t, remote, "2022-03-04T05:06:07Z", "docs/new.md", "# New updated")
	freshnessGit(t, clone, nil, "pull", "--ff-only")
	store.Reindex()
	assertFreshness(t, store, "old", "2020-01-02T03:04:05Z")
	assertFreshness(t, store, "new", "2022-03-04T05:06:07Z")

	freshnessCommit(t, remote, "2019-01-01T00:00:00Z", "docs/new.md", "# Clock skew")
	freshnessGit(t, clone, nil, "pull", "--ff-only")
	store.Reindex()
	assertFreshness(t, store, "new", "2019-01-01T00:00:00Z")

	if err := os.WriteFile(filepath.Join(clone, "docs", "untracked.md"), []byte("# Untracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	store.Reindex()
	assertFreshness(t, store, "untracked", "")
}

func TestGitSourceFreshnessShallowAndBrokenHistory(t *testing.T) {
	remote := t.TempDir()
	freshnessGit(t, remote, nil, "init")
	freshnessCommit(t, remote, "2020-01-02T03:04:05Z", "old.md", "# Old")
	freshnessCommit(t, remote, "2021-02-03T04:05:06Z", "new.md", "# New")
	clone := filepath.Join(t.TempDir(), "clone")
	freshnessGit(t, "", nil, "clone", "--depth=1", "--", "file://"+remote, clone)
	store, err := newGitSourceFileStore(clone, clone, "docs", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	assertFreshness(t, store, "old", "")
	if err := gitSourceEnsureFullHistory(context.Background(), clone, "file://"+remote, "main"); err == nil {
		t.Fatal("unvalidated history fetch unexpectedly succeeded")
	}
	freshnessGit(t, clone, nil, "fetch", "--unshallow")
	store.Reindex()
	assertFreshness(t, store, "old", "2020-01-02T03:04:05Z")
	if err := os.RemoveAll(filepath.Join(clone, ".git")); err != nil {
		t.Fatal(err)
	}
	store.Reindex()
	assertFreshness(t, store, "old", "")
}

func TestGitSourceSyncFailureInterval(t *testing.T) {
	gs := NewGitSource(GitSourceConfig{Name: "test"}, t.TempDir(), slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gs.Sync(ctx); err == nil {
		t.Fatal("canceled sync succeeded")
	}
	if !gs.Info().FailureSince.IsZero() {
		t.Fatal("cancellation must not start a failure interval")
	}
	if err := gs.Sync(context.Background()); err == nil {
		t.Fatal("missing clone sync succeeded")
	}
	first := gs.Info().FailureSince
	if first.IsZero() {
		t.Fatal("failure interval was not recorded")
	}
	_ = gs.Sync(context.Background())
	if !gs.Info().FailureSince.Equal(first) {
		t.Fatal("repeated failure reset the interval")
	}
	_ = gs.Sync(ctx)
	if !gs.Info().FailureSince.Equal(first) {
		t.Fatal("canceled sync changed the failure interval")
	}
}

func TestGitSourceInitAndSyncWithCompleteHistory(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	freshnessGit(t, "", nil, "init", "--bare", remote)
	freshnessGit(t, remote, nil, "symbolic-ref", "HEAD", "refs/heads/main")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	freshnessGit(t, work, nil, "init")
	freshnessCommit(t, work, "2020-01-02T03:04:05Z", "docs/old.md", "# Old")
	freshnessCommit(t, work, "2021-02-03T04:05:06Z", "docs/new.md", "# New")
	freshnessGit(t, work, nil, "push", remote, "HEAD:refs/heads/main")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(&cgi.Handler{
		Path: git,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	defer server.Close()
	url := server.URL + "/remote.git"
	gs := NewGitSource(GitSourceConfig{Name: "docs", URL: url, Branch: "main", Subpath: "docs"}, t.TempDir(), slog.Default())
	if err := gs.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertFreshness(t, gs.Store(), "old", "2020-01-02T03:04:05Z")
	assertFreshness(t, gs.Store(), "new", "2021-02-03T04:05:06Z")

	freshnessCommit(t, work, "2022-03-04T05:06:07Z", "docs/new.md", "# Updated")
	freshnessGit(t, work, nil, "push", remote, "HEAD:refs/heads/main")
	if err := gs.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertFreshness(t, gs.Store(), "old", "2020-01-02T03:04:05Z")
	assertFreshness(t, gs.Store(), "new", "2022-03-04T05:06:07Z")

	freshnessGit(t, gs.cloneDir, nil, "config", "remote.origin.url", server.URL+"/missing.git")
	if err := gs.Sync(context.Background()); err == nil || gs.Info().FailureSince.IsZero() {
		t.Fatal("failed sync did not start a failure interval")
	}
	freshnessGit(t, gs.cloneDir, nil, "config", "remote.origin.url", url)
	if err := gs.Sync(context.Background()); err != nil || !gs.Info().FailureSince.IsZero() {
		t.Fatalf("successful sync did not clear failure interval: %v", err)
	}

	shallow := filepath.Join(t.TempDir(), "shallow")
	freshnessGit(t, "", nil, "clone", "--depth=1", "--", "file://"+remote, shallow)
	freshnessGit(t, shallow, nil, "config", "remote.origin.url", url)
	reused := &GitSource{
		config:   GitSourceConfig{Name: "docs", URL: url, Branch: "main", Subpath: "docs"},
		cloneDir: shallow,
		indexDir: filepath.Join(shallow, "docs"),
		logger:   slog.Default(),
	}
	if err := reused.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertFreshness(t, reused.Store(), "old", "2020-01-02T03:04:05Z")
}
