package codeindex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func checkout(t *testing.T, name string, files map[string]string) Repo {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init")
	for path, text := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	return Repo{Name: name, Root: root}
}

func TestSearchRegisteredReposAndExactIdentifiers(t *testing.T) {
	ctx := context.Background()
	a := checkout(t, "org/a", map[string]string{
		"main.go":              "package main\n// ConfigureWidget uses triage.prior_art and --max-findings\n",
		"docs/config guide.md": "# Guide\nConfigureWidget triage.prior_art --max-findings\n",
		"README.md":            "ConfigureWidget usage\n",
		"CHANGELOG.md":         "ConfigureWidget shipped\n",
	})
	b := checkout(t, "org/b", map[string]string{"lib.go": "ConfigureWidget triage.prior_art --max-findings\n"})
	idx, errs := Build(ctx, []Repo{b, a})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	matches, err := idx.Search(ctx, []string{"ConfigureWidget", "triage.prior_art", "--max-findings", "CONFIGUREWIDGET"}, 10)
	if err != nil || len(matches) != 5 {
		t.Fatalf("matches=%+v, err=%v", matches, err)
	}
	if matches[0].Repo != "org/a" || matches[0].Path != "docs/config guide.md" || matches[0].Line != 2 || !matches[0].Doc {
		t.Fatalf("unexpected strongest match: %+v", matches[0])
	}
	if len(matches[0].Terms) != 3 || !strings.Contains(matches[0].URL(), "/docs/config%20guide.md#L2") {
		t.Fatalf("terms or URL: %+v %s", matches[0], matches[0].URL())
	}
	if matches[2].Repo != "org/b" {
		t.Fatalf("second repo missing from strongest results: %+v", matches)
	}
	for _, term := range []string{"--max-finding", "triage.prior", "ConfigureWidge"} {
		got, err := idx.Search(ctx, []string{term}, 10)
		if err != nil || len(got) != 0 {
			t.Fatalf("partial identifier %q matched: %+v, %v", term, got, err)
		}
	}
	limited, _ := idx.Search(ctx, []string{"ConfigureWidget"}, 2)
	if len(limited) != 2 {
		t.Fatalf("limit: %+v", limited)
	}
	// Returned slices must not allow callers to mutate the shared snapshot.
	matches[0].Terms[0] = "corrupted"
	again, _ := idx.Search(ctx, []string{"ConfigureWidget", "triage.prior_art", "--max-findings"}, 10)
	if again[0].Terms[0] == "corrupted" {
		t.Fatal("search result aliases index state")
	}
}

func TestExclusionsAndSnapshotRefresh(t *testing.T) {
	ctx := context.Background()
	repo := checkout(t, "org/repo", map[string]string{
		"main.go":             "UniqueIdentifier",
		"binary.dat":          "UniqueIdentifier\x00",
		"invalid.dat":         "UniqueIdentifier\xff",
		"nested/source.go":    "UniqueIdentifier",
		"vendor/lib.go":       "UniqueIdentifier",
		"node_modules/lib.js": "UniqueIdentifier",
		"large.txt":           strings.Repeat("x", maxFileBytes+1) + "UniqueIdentifier",
		"link.go":             "UniqueIdentifier",
	})
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("UniqueIdentifier"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo.Root, "link.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo.Root, "link.go")); err != nil {
		t.Fatal(err)
	}
	// A regular tracked leaf under a replaced parent must not escape either.
	if err := os.RemoveAll(filepath.Join(repo.Root, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(outside), "source.go"), []byte("UniqueIdentifier"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(repo.Root, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "untracked.go"), []byte("UniqueIdentifier"), 0644); err != nil {
		t.Fatal(err)
	}
	idx, errs := Build(ctx, []Repo{repo})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	got, _ := idx.Search(ctx, []string{"UniqueIdentifier"}, 20)
	if len(got) != 1 || got[0].Path != "main.go" {
		t.Fatalf("excluded content indexed: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "main.go"), []byte("ReplacementIdentifier"), 0644); err != nil {
		t.Fatal(err)
	}
	old, _ := idx.Search(ctx, []string{"UniqueIdentifier"}, 20)
	if !reflect.DeepEqual(got, old) {
		t.Fatal("disk edit changed immutable snapshot")
	}
	fresh, errs := Build(ctx, []Repo{repo})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	updated, _ := fresh.Search(ctx, []string{"ReplacementIdentifier"}, 20)
	if len(updated) != 1 {
		t.Fatalf("refresh failed: %+v", updated)
	}
}

func TestPartialCoverageAndCancellation(t *testing.T) {
	ctx := context.Background()
	repo := checkout(t, "org/available", map[string]string{"main.go": "ExistingFeature"})
	idx, errs := Build(ctx, []Repo{{Name: "org/missing", Root: t.TempDir()}, repo})
	if len(errs) != 1 || errs["org/missing"] == nil {
		t.Fatalf("missing checkout not reported: %v", errs)
	}
	got, err := idx.Search(ctx, []string{"ExistingFeature"}, 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("available repo lost: %+v, %v", got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if snapshot, errs := Build(canceled, []Repo{repo}); snapshot != nil || len(errs) == 0 {
		t.Fatalf("canceled build published snapshot: %+v, %v", snapshot, errs)
	}
	if _, err := idx.Search(canceled, []string{"ExistingFeature"}, 5); err != context.Canceled {
		t.Fatalf("canceled search: %v", err)
	}
	if got, err := idx.Search(ctx, nil, 0); len(got) != 0 || err != nil {
		t.Fatalf("empty query: %+v, %v", got, err)
	}
}

// cancelAfter reports context.Canceled once Err has been called n times.
type cancelAfter struct {
	context.Context
	n int
}

func (c *cancelAfter) Err() error {
	if c.n--; c.n < 0 {
		return context.Canceled
	}
	return nil
}

func TestBuildFileErrorPaths(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		skip    bool
		corrupt func(t *testing.T, root string)
	}{
		{"tracked file deleted", false, func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "bad.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{"tracked file unreadable", runtime.GOOS == "windows" || os.Geteuid() == 0, func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, "bad.go"), 0); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skip {
				t.Skip("cannot make a file unreadable here")
			}
			repo := checkout(t, "org/repo", map[string]string{"bad.go": "BadIdentifier", "good.go": "GoodIdentifier"})
			tt.corrupt(t, repo.Root)
			idx, errs := Build(ctx, []Repo{repo})
			if errs["org/repo"] == nil || !strings.Contains(errs["org/repo"].Error(), "bad.go") {
				t.Fatalf("per-file error not reported: %v", errs)
			}
			if got, _ := idx.Search(ctx, []string{"GoodIdentifier"}, 5); len(got) != 1 {
				t.Fatalf("healthy file lost: %+v", got)
			}
			if got, _ := idx.Search(ctx, []string{"BadIdentifier"}, 5); len(got) != 0 {
				t.Fatalf("unreadable file indexed: %+v", got)
			}
		})
	}
}

func TestBuildTextBudgetExceeded(t *testing.T) {
	old := maxRepoBytes
	maxRepoBytes = 10
	t.Cleanup(func() { maxRepoBytes = old })
	repo := checkout(t, "org/repo", map[string]string{"a.txt": "0123456789ab"})
	if _, errs := Build(context.Background(), []Repo{repo}); errs["org/repo"] == nil || !strings.Contains(errs["org/repo"].Error(), "budget") {
		t.Fatalf("budget error missing: %v", errs)
	}
}

func TestBuildCanceledMidWalk(t *testing.T) {
	repo := checkout(t, "org/repo", map[string]string{"a.go": "Alpha", "b.go": "Beta"})
	snapshot, errs := Build(&cancelAfter{Context: context.Background(), n: 2}, []Repo{repo})
	if snapshot != nil || errs["org/repo"] != context.Canceled {
		t.Fatalf("mid-walk cancel: %+v, %v", snapshot, errs)
	}
}

func TestSearchNilIndexAndCanceledTerms(t *testing.T) {
	ctx := context.Background()
	var nilIdx *Index
	if got, err := nilIdx.Search(ctx, []string{"x"}, 5); got != nil || err != nil {
		t.Fatalf("nil index: %+v, %v", got, err)
	}
	repo := checkout(t, "org/repo", map[string]string{"a.go": "Alpha"})
	idx, _ := Build(ctx, []Repo{repo})
	if _, err := idx.Search(&cancelAfter{Context: ctx, n: 1}, []string{"alpha", "beta"}, 5); err != context.Canceled {
		t.Fatalf("cancel during terms: %v", err)
	}
	if _, err := idx.Search(&cancelAfter{Context: ctx, n: 1}, []string{"alpha"}, 5); err != context.Canceled {
		t.Fatalf("cancel during lines: %v", err)
	}
}

func TestLongLineTruncatesOnRuneBoundary(t *testing.T) {
	ctx := context.Background()
	repo := checkout(t, "org/repo", map[string]string{"a.txt": "Marker " + strings.Repeat("é", 3000)})
	idx, _ := Build(ctx, []Repo{repo})
	got, _ := idx.Search(ctx, []string{"marker"}, 5)
	if len(got) != 1 || !utf8.ValidString(got[0].Text) || len(got[0].Text) > maxLineBytes {
		t.Fatalf("truncation: %+v", got)
	}
}
