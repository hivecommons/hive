package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureRepo creates a bare repository with two commits and returns its
// file:// URL and both SHAs (oldest first).
func fixtureRepo(t *testing.T) (repo string, shas []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	work := filepath.Join(t.TempDir(), "work")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	for i, name := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", name)
		git("commit", "-q", "-m", "c"+string(rune('0'+i)))
		shas = append(shas, git("rev-parse", "HEAD"))
	}
	bare := filepath.Join(t.TempDir(), "fixture.git")
	git("clone", "-q", "--bare", work, bare)
	return "file://" + bare, shas
}

func newTestService(t *testing.T, repo string, edit func(*Config)) *Service {
	t.Helper()
	cfg := Config{Dir: filepath.Join(t.TempDir(), "checkouts"), AllowedRepos: []string{"file://" + filepath.Dir(strings.TrimPrefix(repo, "file://"))}}
	if edit != nil {
		edit(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestCreateListDeleteRoundTrip(t *testing.T) {
	repo, shas := fixtureRepo(t)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	svc := newTestService(t, repo, func(c *Config) { c.Now = func() time.Time { return now } })
	ctx := context.Background()

	rec, err := svc.Create(ctx, Request{Repo: repo, Commit: shas[0], Owner: "nous-42", TTL: 72 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(rec.ID) || rec.Commit != shas[0] || rec.Owner != "nous-42" || !rec.Branches || rec.MaxBytes != DefaultMaxBytes || rec.Bytes <= 0 {
		t.Fatalf("record = %+v", rec)
	}
	if rec.TTL != "72h0m0s" || rec.ExpiresAt == nil || !rec.ExpiresAt.Equal(now.Add(72*time.Hour)) || !rec.CreatedAt.Equal(now) {
		t.Fatalf("ttl/expiry = %q %v %v", rec.TTL, rec.ExpiresAt, rec.CreatedAt)
	}
	if _, err := os.Stat(filepath.Join(rec.Path, "one.txt")); err != nil {
		t.Fatalf("worktree content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rec.Path, "two.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree is not pinned to the first commit: %v", err)
	}
	// Records are under Dir and the index has the create.
	if _, err := os.Stat(filepath.Join(svc.cfg.Dir, rec.ID+".json")); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(rec.ID)
	if err != nil || got.ID != rec.ID || got.Path != rec.Path || got.Source != rec.Source {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	// A second checkout for the same (repo, owner) reuses the clone; the
	// other commit is reachable without a fetch.
	second, err := svc.Create(ctx, Request{Repo: repo, Commit: shas[1], Owner: "nous-42"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Source != rec.Source || second.Path == rec.Path {
		t.Fatalf("second = %+v, first = %+v", second, rec)
	}
	// Another owner gets its own clone.
	other, err := svc.Create(ctx, Request{Repo: repo, Commit: shas[1], Owner: "spotlights"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Source == rec.Source {
		t.Fatal("owners must not share a clone")
	}

	all, err := svc.List("")
	if err != nil || len(all) != 3 {
		t.Fatalf("List all = %d %v", len(all), err)
	}
	mine, err := svc.List("nous-42")
	if err != nil || len(mine) != 2 {
		t.Fatalf("List owner = %d %v", len(mine), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].ID > all[i].ID {
			t.Fatal("List is not ordered by id")
		}
	}

	if err := svc.Delete(ctx, rec.ID, "operator:operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rec.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree still present: %v", err)
	}
	if _, err := svc.Get(rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}
	if err := svc.Delete(ctx, rec.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}

	index, err := os.ReadFile(filepath.Join(svc.cfg.Dir, IndexName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(index)), "\n")
	if len(lines) != 4 {
		t.Fatalf("index has %d lines: %s", len(lines), index)
	}
	var last indexEntry
	if err := json.Unmarshal([]byte(lines[3]), &last); err != nil {
		t.Fatal(err)
	}
	if last.Op != "delete" || last.ID != rec.ID || last.By != "operator:operator" || last.Owner != "nous-42" {
		t.Fatalf("last index entry = %+v", last)
	}
}

func TestCreateRefusals(t *testing.T) {
	repo, shas := fixtureRepo(t)
	svc := newTestService(t, repo, nil)
	ctx := context.Background()
	ok := Request{Repo: repo, Commit: shas[0], Owner: "o"}
	cases := []struct {
		name string
		edit func(*Request)
		want error
		msg  string
	}{
		{"short sha", func(r *Request) { r.Commit = shas[0][:12] }, ErrBadRequest, "full 40-hex SHA"},
		{"branch", func(r *Request) { r.Commit = "main" }, ErrBadRequest, "not a tag or branch"},
		{"tag-like", func(r *Request) { r.Commit = "v1.0.0" }, ErrBadRequest, "full 40-hex SHA"},
		{"missing owner", func(r *Request) { r.Owner = "" }, ErrBadRequest, "owner"},
		{"bad owner", func(r *Request) { r.Owner = "Has Spaces" }, ErrBadRequest, "owner"},
		{"long owner", func(r *Request) { r.Owner = strings.Repeat("a", MaxOwnerLen+1) }, ErrBadRequest, "owner"},
		{"negative cap", func(r *Request) { r.MaxBytes = -1 }, ErrBadRequest, "max_bytes"},
		{"negative ttl", func(r *Request) { r.TTL = -time.Second }, ErrBadRequest, "ttl"},
		{"ssh url", func(r *Request) { r.Repo = "git@github.com:org/name.git" }, ErrBadRequest, "https:// or file://"},
		{"http url", func(r *Request) { r.Repo = "http://github.com/org/name.git" }, ErrBadRequest, "https:// or file://"},
		{"credentials in url", func(r *Request) { r.Repo = "https://user:tok@github.com/org/name.git" }, ErrBadRequest, "credentials"},
		{"query in url", func(r *Request) { r.Repo = "https://github.com/org/name.git?x=1" }, ErrBadRequest, "query"},
		{"no host", func(r *Request) { r.Repo = "https:///name.git" }, ErrBadRequest, "no host"},
		{"off allowlist", func(r *Request) { r.Repo = "https://github.com/org/name.git" }, ErrForbidden, "matches none of the allowed prefixes"},
		{"unknown commit", func(r *Request) { r.Commit = strings.Repeat("0", 40) }, ErrBadRequest, "could not be fetched"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := ok
			tc.edit(&req)
			_, err := svc.Create(ctx, req)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("Create = %v, want %v containing %q", err, tc.want, tc.msg)
			}
		})
	}
	// The forbidden error names the rule set that denied it, and an empty
	// allowlist denies everything with its own message.
	if _, err := svc.Allowed("https://evil.example/x.git"); err == nil || !strings.Contains(err.Error(), svc.cfg.AllowedRepos[0]) {
		t.Fatalf("Allowed off-list = %v", err)
	}
	if rule, err := svc.Allowed(repo); err != nil || rule != svc.cfg.AllowedRepos[0] {
		t.Fatalf("Allowed on-list = %q %v", rule, err)
	}
	empty := newTestService(t, repo, func(c *Config) { c.AllowedRepos = nil })
	if _, err := empty.Create(ctx, ok); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "allowlist is empty") {
		t.Fatalf("empty allowlist = %v", err)
	}
	// Nothing was written for refused requests.
	if list, err := svc.List(""); err != nil || len(list) != 0 {
		t.Fatalf("records after refusals = %v %v", list, err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("New without dir = %v", err)
	}
	if _, err := New(Config{Dir: t.TempDir(), AllowedRepos: []string{"github.com/org/"}}); !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "allowlist entry") {
		t.Fatalf("New with bare prefix = %v", err)
	}
	svc, err := New(Config{Dir: t.TempDir(), AllowedRepos: []string{" https://github.com/org/ "}})
	if err != nil || svc.cfg.AllowedRepos[0] != "https://github.com/org/" || svc.cfg.MaxCheckouts != DefaultMaxCheckouts {
		t.Fatalf("New = %+v %v", svc, err)
	}
	if svc.InUse("chk_000000000000") {
		t.Fatal("nil InUse hook must report not in use")
	}
	if DefaultDir("/data/agents") != "/data/agents/checkouts" {
		t.Fatal(DefaultDir("/data/agents"))
	}
	for id, want := range map[string]bool{"chk_0123456789ab": true, "chk_0123456789aB": true, "chk_0123456789": false, "run_0123456789ab": false, "chk_../../etc/pw": false, "": false} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) = %v", id, !want)
		}
	}
}

func TestSizeCapIsPostHocAndRefuses(t *testing.T) {
	repo, shas := fixtureRepo(t)
	svc := newTestService(t, repo, nil)
	ctx := context.Background()
	_, err := svc.Create(ctx, Request{Repo: repo, Commit: shas[0], Owner: "o", MaxBytes: 1})
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "cap is 1") || !strings.Contains(err.Error(), "after cloning") {
		t.Fatalf("over cap = %v", err)
	}
	// The over-cap worktree is gone and nothing was recorded; the shared
	// clone stays for the next request.
	if list, _ := svc.List(""); len(list) != 0 {
		t.Fatalf("records after refusal: %v", list)
	}
	if entries, _ := os.ReadDir(filepath.Join(svc.cfg.Dir, "runs")); len(entries) != 0 {
		for _, e := range entries {
			if sub, _ := os.ReadDir(filepath.Join(svc.cfg.Dir, "runs", e.Name())); len(sub) != 0 {
				t.Fatalf("worktree left behind: %v", sub)
			}
		}
	}

	// A cap that cannot be measured is refused as unsupported, not skipped.
	broken := newTestService(t, repo, func(c *Config) {
		c.DiskUsage = func(string) (int64, error) { return 0, errors.New("no du") }
	})
	if _, err := broken.Create(ctx, Request{Repo: repo, Commit: shas[0], Owner: "o"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unmeasurable = %v", err)
	}
	// Measuring only the worktree failing is the same refusal.
	calls := 0
	broken = newTestService(t, repo, func(c *Config) {
		c.DiskUsage = func(path string) (int64, error) {
			calls++
			if calls == 2 {
				return 0, errors.New("no du")
			}
			return 1, nil
		}
	})
	if _, err := broken.Create(ctx, Request{Repo: repo, Commit: shas[0], Owner: "o"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unmeasurable worktree = %v", err)
	}
	if _, err := diskUsage(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("diskUsage of a missing path must fail")
	}
}

func TestDeleteOwnershipAndInUse(t *testing.T) {
	repo, shas := fixtureRepo(t)
	inUse := map[string]bool{}
	svc := newTestService(t, repo, func(c *Config) { c.InUse = func(id string) bool { return inUse[id] } })
	ctx := context.Background()
	rec, err := svc.Create(ctx, Request{Repo: repo, Commit: shas[0], Owner: "atlas"})
	if err != nil {
		t.Fatal(err)
	}
	if !CanDelete(rec, "atlas", false) || CanDelete(rec, "beacon", false) || !CanDelete(rec, "beacon", true) || CanDelete(rec, "", false) {
		t.Fatal("ownership rule")
	}
	inUse[rec.ID] = true
	if err := svc.Delete(ctx, rec.ID, "project:atlas"); !errors.Is(err, ErrInUse) || !svc.InUse(rec.ID) {
		t.Fatalf("delete in use = %v", err)
	}
	inUse[rec.ID] = false
	if err := svc.Delete(ctx, rec.ID, "project:atlas"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get("../escape"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get with a path = %v", err)
	}
	if err := svc.Delete(ctx, "chk_000000000000", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete unknown = %v", err)
	}
}

func TestGitFailuresAreTyped(t *testing.T) {
	repo, shas := fixtureRepo(t)
	ctx := context.Background()
	real := execRunner
	// Clone fails: typed ErrGit and the half-made source removed.
	failClone := newTestService(t, repo, func(c *Config) {
		c.Git = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			if args[0] == "clone" {
				return []byte("fatal: nope"), errors.New("exit 128")
			}
			return real(ctx, dir, name, args...)
		}
	})
	_, err := failClone.Create(ctx, Request{Repo: repo, Commit: shas[0], Owner: "o"})
	if !errors.Is(err, ErrGit) || !strings.Contains(err.Error(), "fatal: nope") {
		t.Fatalf("clone failure = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(failClone.cfg.Dir, sourceDirName)); len(entries) != 0 {
		t.Fatalf("source left behind: %v", entries)
	}

	// Worktree add fails.
	failTree := newTestService(t, repo, func(c *Config) {
		c.Git = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			if args[0] == "worktree" {
				return []byte("boom"), errors.New("exit 1")
			}
			return real(ctx, dir, name, args...)
		}
	})
	if _, err := failTree.Create(ctx, Request{Repo: repo, Commit: shas[0], Owner: "o"}); !errors.Is(err, ErrGit) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("worktree failure = %v", err)
	}

	// HEAD disagrees with the pin: 502-class mismatch naming both SHAs.
	mismatch := newTestService(t, repo, func(c *Config) {
		c.Git = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			if args[0] == "rev-parse" {
				return []byte(shas[1] + "\n"), nil
			}
			return real(ctx, dir, name, args...)
		}
	})
	_, err = mismatch.Create(ctx, Request{Repo: repo, Commit: shas[0], Owner: "o"})
	if !errors.Is(err, ErrMismatch) || !strings.Contains(err.Error(), shas[0]) || !strings.Contains(err.Error(), shas[1]) {
		t.Fatalf("mismatch = %v", err)
	}

	// A fetch that "succeeds" without delivering the commit is still a
	// bad request, not a checkout at the wrong commit.
	phantom := newTestService(t, repo, func(c *Config) {
		c.Git = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			if args[0] == "fetch" {
				return nil, nil
			}
			return real(ctx, dir, name, args...)
		}
	})
	if _, err := phantom.Create(ctx, Request{Repo: repo, Commit: strings.Repeat("a", 40), Owner: "o"}); !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "is not in") {
		t.Fatalf("phantom fetch = %v", err)
	}

	// Single-branch clones are requested when branches is false.
	var cloneArgs []string
	single := newTestService(t, repo, func(c *Config) {
		c.Git = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			if args[0] == "clone" {
				cloneArgs = args
			}
			return real(ctx, dir, name, args...)
		}
	})
	no := false
	rec, err := single.Create(ctx, Request{Repo: repo, Commit: shas[1], Owner: "o", Branches: &no})
	if err != nil || rec.Branches {
		t.Fatalf("single branch = %+v %v", rec, err)
	}
	if !contains(cloneArgs, "--single-branch") {
		t.Fatalf("clone args = %v", cloneArgs)
	}

	// Removing a worktree whose git state is gone falls back to deleting
	// the directory; a runner that fails on remove is reported.
	failRemove := newTestService(t, repo, func(c *Config) {
		c.Git = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
			if args[0] == "worktree" && args[1] == "remove" {
				return nil, errors.New("locked")
			}
			return real(ctx, dir, name, args...)
		}
	})
	rec, err = failRemove.Create(ctx, Request{Repo: repo, Commit: shas[0], Owner: "o"})
	if err != nil {
		t.Fatal(err)
	}
	if err := failRemove.Delete(ctx, rec.ID, "x"); err != nil {
		t.Fatalf("delete with failing git remove should fall back to os.RemoveAll: %v", err)
	}
	if _, err := os.Stat(rec.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worktree directory still present")
	}
}

func TestStoreCorruptionAndIO(t *testing.T) {
	dir := t.TempDir()
	st := &store{dir: dir}
	id := "chk_0123456789ab"
	if err := os.WriteFile(st.recordPath(id), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.get(id); err == nil || !strings.Contains(err.Error(), "corrupt record") {
		t.Fatalf("corrupt get = %v", err)
	}
	if _, err := st.list(); err == nil {
		t.Fatal("list must surface a corrupt record")
	}
	// Non-record files and directories under Dir are ignored by list.
	if err := os.Remove(st.recordPath(id)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if list, err := st.list(); err != nil || len(list) != 0 {
		t.Fatalf("list = %v %v", list, err)
	}
	if list, err := (&store{dir: filepath.Join(dir, "absent")}).list(); err != nil || len(list) != 0 {
		t.Fatalf("list of absent dir = %v %v", list, err)
	}
	if err := st.remove("chk_ffffffffffff"); err != nil {
		t.Fatalf("remove of absent record = %v", err)
	}
	// Writes into an unwritable dir fail loudly rather than silently.
	ro := &store{dir: filepath.Join(dir, "absent")}
	if err := ro.put(Record{ID: id}); err == nil {
		t.Fatal("put into a missing dir must fail")
	}
	if err := ro.appendIndex(indexEntry{Op: "create"}); err == nil {
		t.Fatal("appendIndex into a missing dir must fail")
	}
	if _, err := (&store{dir: filepath.Join(dir, "runs")}).list(); err != nil {
		t.Fatal(err)
	}
	// A directory where the record file should be is a read error.
	if err := os.MkdirAll(st.recordPath("chk_aaaaaaaaaaaa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.get("chk_aaaaaaaaaaaa"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("get of a directory = %v", err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
