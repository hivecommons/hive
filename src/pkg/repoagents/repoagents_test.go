package repoagents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

type fakeClient struct {
	mu      sync.Mutex
	files   map[string]string // "owner/repo/path" -> content
	listErr error
	readErr map[string]error
	lists   int
}

func newFakeClient() *fakeClient {
	return &fakeClient{files: map[string]string{}, readErr: map[string]error{}}
}

func (f *fakeClient) put(slug, p, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[slug+"/"+p] = content
}

func (f *fakeClient) remove(slug, p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, slug+"/"+p)
}

func (f *fakeClient) ListDirectoryFiles(_ context.Context, owner, repo, dir, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	prefix := owner + "/" + repo + "/" + dir + "/"
	var out []string
	for k := range f.files {
		if rest, ok := strings.CutPrefix(k, prefix); ok && !strings.Contains(rest, "/") {
			out = append(out, rest)
		}
	}
	return out, nil
}

func (f *fakeClient) GetFileContentRef(_ context.Context, owner, repo, p, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := owner + "/" + repo + "/" + p
	if err := f.readErr[key]; err != nil {
		return "", err
	}
	c, ok := f.files[key]
	if !ok {
		return "", fmt.Errorf("404 %s", key)
	}
	return c, nil
}

func def(name string) string {
	return "apiVersion: hive.kubestellar.io/v1\nkind: AgentDefinition\nmetadata:\n  name: " + name +
		"\n  displayName: Foo Agent\n  emoji: \"🦊\"\n  description: does foo\nspec:\n  backend: claude\n  model: opus\n"
}

const slug = "acme/widgets"

func newTestDiscoverer(t *testing.T, c Client, allow func(string) bool) (*Discoverer, *[]Entry) {
	t.Helper()
	var orphaned []Entry
	d := New(Options{
		Client:    c,
		Allow:     allow,
		Repos:     func() []string { return []string{slug} },
		StatePath: filepath.Join(t.TempDir(), "state", "repo-agents.json"),
		OnOrphan:  func(e Entry) { orphaned = append(orphaned, e) },
		Now:       func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) },
	})
	return d, &orphaned
}

func allowAll(string) bool { return true }

// TestLifecycle walks the "done when" of #18491: a new file shows up pending,
// approval registers it, deletion pauses it (orphaned), and a returning file
// tracks again without un-pausing.
func TestLifecycle(t *testing.T) {
	c := newFakeClient()
	c.put(slug, ".hive/agents/foo.yaml", def("Foo Bot"))
	c.put(slug, ".hive/agents/foo.md", "prompt")
	c.put(slug, ".hive/agents/README.txt", "ignored")
	d, orphaned := newTestDiscoverer(t, c, allowAll)

	d.ScanAll(context.Background())
	list := d.List()
	if len(list) != 1 {
		t.Fatalf("entries = %+v, want 1", list)
	}
	e := list[0]
	if e.Status != StatusPending || e.Name != "foo-bot" || e.DisplayName != "Foo Agent" || e.Emoji != "🦊" || e.Description != "does foo" {
		t.Fatalf("pending entry = %+v", e)
	}
	if e.ID != EntryID(slug, ".hive/agents/foo.yaml") || e.PromptPath != ".hive/agents/foo.md" {
		t.Fatalf("id/prompt = %q / %q", e.ID, e.PromptPath)
	}
	if e.Owner() != "acme" || e.RepoName() != "widgets" {
		t.Fatalf("owner/repo = %q/%q", e.Owner(), e.RepoName())
	}

	got, err := d.Approve(e.ID, "foo-bot", "owner1")
	if err != nil || got.Status != StatusApproved || got.Agent != "foo-bot" || got.DecidedBy != "owner1" || got.DecidedAt == nil {
		t.Fatalf("approve = %+v, %v", got, err)
	}
	if _, err := d.Approve(e.ID, "foo-bot", "owner1"); err == nil {
		t.Fatal("second approve should fail")
	}

	// Rescan with the file still there keeps it approved and does not orphan.
	if err := d.ScanRepo(context.Background(), slug); err != nil {
		t.Fatal(err)
	}
	if g, _ := d.Get(e.ID); g.Status != StatusApproved || len(*orphaned) != 0 {
		t.Fatalf("after rescan: %+v orphaned=%d", g, len(*orphaned))
	}

	c.remove(slug, ".hive/agents/foo.yaml")
	if err := d.ScanRepo(context.Background(), slug); err != nil {
		t.Fatal(err)
	}
	g, _ := d.Get(e.ID)
	if g.Status != StatusOrphaned || g.OrphanedAt == nil {
		t.Fatalf("after delete: %+v", g)
	}
	if len(*orphaned) != 1 || (*orphaned)[0].Agent != "foo-bot" {
		t.Fatalf("OnOrphan calls = %+v", *orphaned)
	}
	// A second scan does not pause again.
	_ = d.ScanRepo(context.Background(), slug)
	if len(*orphaned) != 1 {
		t.Fatalf("orphan re-fired: %d", len(*orphaned))
	}

	c.put(slug, ".hive/agents/foo.yaml", def("Foo Bot"))
	_ = d.ScanRepo(context.Background(), slug)
	if g, _ := d.Get(e.ID); g.Status != StatusApproved || g.OrphanedAt != nil {
		t.Fatalf("returned file: %+v", g)
	}
}

func TestPersistenceSurvivesRestart(t *testing.T) {
	c := newFakeClient()
	c.put(slug, ".hive/agents/foo.yaml", def("foo"))
	statePath := filepath.Join(t.TempDir(), "repo-agents.json")
	d := New(Options{Client: c, Allow: allowAll, StatePath: statePath})
	_ = d.ScanRepo(context.Background(), slug)
	if _, err := d.Approve(EntryID(slug, ".hive/agents/foo.yaml"), "foo", "o"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != stateFileMode {
		t.Errorf("state mode = %v", info.Mode().Perm())
	}
	d2 := New(Options{Client: c, Allow: allowAll, StatePath: statePath})
	if g, ok := d2.Get(EntryID(slug, ".hive/agents/foo.yaml")); !ok || g.Status != StatusApproved || g.Agent != "foo" {
		t.Fatalf("reloaded = %+v ok=%v", g, ok)
	}
}

func TestLoadBadState(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := New(Options{StatePath: corrupt}).List(); len(got) != 0 {
		t.Fatalf("corrupt state should start empty: %+v", got)
	}
	noID := filepath.Join(dir, "noid.json")
	if err := os.WriteFile(noID, []byte(`{"entries":[{"repo":"a/b"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := New(Options{StatePath: noID}).List(); len(got) != 0 {
		t.Fatalf("entry without id should be dropped: %+v", got)
	}
	// A directory where the file should be is unreadable as a file.
	if got := New(Options{StatePath: dir}).List(); len(got) != 0 {
		t.Fatalf("unreadable state should start empty: %+v", got)
	}
}

func TestSaveFailureIsLoggedNotFatal(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := newFakeClient()
	c.put(slug, ".hive/agents/foo.yaml", def("foo"))
	// The parent of the state path is a regular file, so MkdirAll fails.
	d := New(Options{Client: c, Allow: allowAll, StatePath: filepath.Join(blocker, "state.json")})
	if err := d.ScanRepo(context.Background(), slug); err != nil {
		t.Fatal(err)
	}
	if len(d.List()) != 1 {
		t.Fatal("entries must stay in memory when save fails")
	}
}

func TestInvalidAndRejected(t *testing.T) {
	c := newFakeClient()
	c.put(slug, ".hive/agents/bad.yml", "kind: Wrong\n")
	c.put(slug, ".hive/agents/nope.yaml", def("nope"))
	d, _ := newTestDiscoverer(t, c, allowAll)
	_ = d.ScanRepo(context.Background(), slug)

	badID := EntryID(slug, ".hive/agents/bad.yml")
	bad, _ := d.Get(badID)
	if bad.Status != StatusInvalid || bad.Error == "" {
		t.Fatalf("bad = %+v", bad)
	}
	if _, err := d.Approve(badID, "bad", "o"); err == nil {
		t.Fatal("invalid entry must not be approvable")
	}
	// Fixing the file moves it back to pending.
	c.put(slug, ".hive/agents/bad.yml", def("bad"))
	_ = d.ScanRepo(context.Background(), slug)
	if g, _ := d.Get(badID); g.Status != StatusPending || g.Error != "" {
		t.Fatalf("fixed = %+v", g)
	}
	// Breaking it again returns to invalid.
	c.put(slug, ".hive/agents/bad.yml", "::: nope")
	_ = d.ScanRepo(context.Background(), slug)
	if g, _ := d.Get(badID); g.Status != StatusInvalid {
		t.Fatalf("rebroken = %+v", g)
	}

	nopeID := EntryID(slug, ".hive/agents/nope.yaml")
	if g, err := d.Reject(nopeID, "o"); err != nil || g.Status != StatusRejected {
		t.Fatalf("reject = %+v %v", g, err)
	}
	if _, err := d.Reject(nopeID, "o"); err == nil {
		t.Fatal("rejecting a rejected entry should fail")
	}
	_ = d.ScanRepo(context.Background(), slug)
	if g, _ := d.Get(nopeID); g.Status != StatusRejected {
		t.Fatalf("rejected must stay rejected while the file exists: %+v", g)
	}
	// Removing a rejected or pending file drops its entry; nothing is paused.
	c.remove(slug, ".hive/agents/nope.yaml")
	c.remove(slug, ".hive/agents/bad.yml")
	_ = d.ScanRepo(context.Background(), slug)
	if got := d.List(); len(got) != 0 {
		t.Fatalf("entries after removal = %+v", got)
	}
	if _, err := d.Reject("missing", "o"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v", err)
	}
}

func TestReadErrors(t *testing.T) {
	c := newFakeClient()
	c.put(slug, ".hive/agents/foo.yaml", def("foo"))
	key := slug + "/.hive/agents/foo.yaml"
	c.readErr[key] = errors.New("rate limited")
	d, orphaned := newTestDiscoverer(t, c, allowAll)
	_ = d.ScanRepo(context.Background(), slug)
	id := EntryID(slug, ".hive/agents/foo.yaml")
	if g, _ := d.Get(id); g.Status != StatusInvalid || !strings.Contains(g.Error, "rate limited") {
		t.Fatalf("unreadable new file = %+v", g)
	}

	// An approved entry whose read fails keeps its status.
	delete(c.readErr, key)
	_ = d.ScanRepo(context.Background(), slug)
	if _, err := d.Approve(id, "foo", "o"); err != nil {
		t.Fatal(err)
	}
	c.readErr[key] = errors.New("flaky")
	_ = d.ScanRepo(context.Background(), slug)
	if g, _ := d.Get(id); g.Status != StatusApproved || g.Error == "" {
		t.Fatalf("approved with read error = %+v", g)
	}

	// A listing failure never orphans.
	c.listErr = errors.New("502")
	if err := d.ScanRepo(context.Background(), slug); err == nil {
		t.Fatal("listing failure should return an error")
	}
	d.ScanAll(context.Background()) // logs and continues
	if g, _ := d.Get(id); g.Status != StatusApproved || len(*orphaned) != 0 {
		t.Fatalf("listing failure changed entry: %+v", g)
	}
}

func TestScanGates(t *testing.T) {
	c := newFakeClient()
	c.put(slug, ".hive/agents/foo.yaml", def("foo"))

	denied, _ := newTestDiscoverer(t, c, func(string) bool { return false })
	_ = denied.ScanRepo(context.Background(), slug)
	if len(denied.List()) != 0 || c.lists != 0 {
		t.Fatalf("non-allowlisted repo was read: entries=%d lists=%d", len(denied.List()), c.lists)
	}

	nilAllow := New(Options{Client: c})
	_ = nilAllow.ScanRepo(context.Background(), slug)
	if len(nilAllow.List()) != 0 {
		t.Fatal("nil allow must deny")
	}

	noClient := New(Options{Allow: allowAll})
	if err := noClient.ScanRepo(context.Background(), slug); err != nil || len(noClient.List()) != 0 {
		t.Fatalf("no client: err=%v entries=%d", err, len(noClient.List()))
	}
	if _, _, err := noClient.FetchDefinition(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fetch unknown = %v", err)
	}

	if err := noClient.ScanRepo(context.Background(), "no-slash"); err == nil {
		t.Fatal("bad slug should error")
	}
	// ScanAll without Repos is a no-op.
	noClient.ScanAll(context.Background())
}

func TestTooManyFiles(t *testing.T) {
	c := newFakeClient()
	for i := 0; i < maxFilesPerRepo+5; i++ {
		c.put(slug, fmt.Sprintf(".hive/agents/a%03d.yaml", i), def(fmt.Sprintf("a%03d", i)))
	}
	d, _ := newTestDiscoverer(t, c, allowAll)
	_ = d.ScanRepo(context.Background(), slug)
	if got := len(d.List()); got != maxFilesPerRepo {
		t.Fatalf("entries = %d, want %d", got, maxFilesPerRepo)
	}
}

func TestListSortsByRepoThenPath(t *testing.T) {
	c := newFakeClient()
	c.put("b/two", ".hive/agents/x.yaml", def("x"))
	c.put("a/one", ".hive/agents/z.yaml", def("z"))
	c.put("a/one", ".hive/agents/y.yaml", def("y"))
	d := New(Options{Client: c, Allow: allowAll, Repos: func() []string { return []string{"b/two", "a/one"} }})
	d.ScanAll(context.Background())
	got := d.List()
	if len(got) != 3 || got[0].Path != ".hive/agents/y.yaml" || got[1].Path != ".hive/agents/z.yaml" || got[2].Repo != "b/two" {
		t.Fatalf("order = %+v", got)
	}
}

func TestFetchDefinition(t *testing.T) {
	c := newFakeClient()
	c.put(slug, ".hive/agents/foo.yaml", def("foo"))
	d, _ := newTestDiscoverer(t, c, allowAll)
	_ = d.ScanRepo(context.Background(), slug)
	id := EntryID(slug, ".hive/agents/foo.yaml")
	parsed, e, err := d.FetchDefinition(context.Background(), id)
	if err != nil || parsed.Metadata.Name != "foo" || e.ID != id {
		t.Fatalf("fetch = %+v %+v %v", parsed, e, err)
	}
	// The allowlist is checked again at fetch time.
	locked := New(Options{Client: c, Allow: func(string) bool { return false }})
	locked.entries[id] = &Entry{ID: id, Repo: slug, Path: ".hive/agents/foo.yaml"}
	if _, _, err := locked.FetchDefinition(context.Background(), id); err == nil {
		t.Fatal("fetch from a de-allowlisted repo must fail")
	}
}

func TestRunScansAndTriggers(t *testing.T) {
	c := newFakeClient()
	c.put(slug, ".hive/agents/foo.yaml", def("foo"))
	d, _ := newTestDiscoverer(t, c, allowAll)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx, 0); close(done) }()

	waitFor(t, func() bool { return len(d.List()) == 1 })
	c.put(slug, ".hive/agents/bar.yaml", def("bar"))
	if !d.Trigger(slug) {
		t.Fatal("trigger dropped")
	}
	waitFor(t, func() bool { return len(d.List()) == 2 })
	// A failing triggered rescan is logged, not fatal.
	c.mu.Lock()
	c.listErr = errors.New("boom")
	c.mu.Unlock()
	d.Trigger(slug)
	cancel()
	<-done
	if d.Trigger("") {
		t.Fatal("empty slug must not queue")
	}
}

func TestTriggerDropsWhenFull(t *testing.T) {
	d := New(Options{})
	for i := 0; i < triggerBuffer; i++ {
		if !d.Trigger(slug) {
			t.Fatalf("trigger %d dropped early", i)
		}
	}
	if d.Trigger(slug) {
		t.Fatal("full queue should drop")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	const waitTimeout = 5 * time.Second
	testutil.Eventually(t, waitTimeout, cond, "repo agent scan did not land")
}

func TestPushRescanRepo(t *testing.T) {
	many := strings.Repeat(`{"added":[],"removed":[],"modified":["x"]},`, pushCommitsCap)
	many = "[" + strings.TrimSuffix(many, ",") + "]"
	cases := []struct {
		name, payload, want string
	}{
		{"agents file changed", `{"ref":"refs/heads/main","repository":{"full_name":"acme/widgets","default_branch":"main"},"commits":[{"modified":["README.md"]},{"removed":[".hive/agents/foo.yaml"]}]}`, slug},
		{"added counts", `{"ref":"refs/heads/main","repository":{"full_name":"acme/widgets","default_branch":"main"},"commits":[{"added":[".hive/agents/new.yaml"]}]}`, slug},
		{"unrelated change", `{"ref":"refs/heads/main","repository":{"full_name":"acme/widgets","default_branch":"main"},"commits":[{"modified":["src/main.go"]}]}`, ""},
		{"other branch", `{"ref":"refs/heads/feature","repository":{"full_name":"acme/widgets","default_branch":"main"},"commits":[{"added":[".hive/agents/new.yaml"]}]}`, ""},
		{"no commits listed", `{"ref":"refs/heads/main","repository":{"full_name":"acme/widgets","default_branch":"main"},"commits":[]}`, slug},
		{"truncated commit list", `{"ref":"refs/heads/main","repository":{"full_name":"acme/widgets","default_branch":"main"},"commits":` + many + `}`, slug},
		{"no repo", `{"ref":"refs/heads/main","repository":{"default_branch":"main"}}`, ""},
		{"no default branch", `{"ref":"refs/heads/main","repository":{"full_name":"acme/widgets"}}`, ""},
		{"garbage", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PushRescanRepo([]byte(tc.payload)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHelpers(t *testing.T) {
	if got := RepoSlug("acme", "widgets"); got != slug {
		t.Errorf("RepoSlug = %q", got)
	}
	if got := RepoSlug("acme", "other/widgets"); got != "other/widgets" {
		t.Errorf("qualified RepoSlug = %q", got)
	}
	if got := RepoSlug("", "widgets"); got != "widgets" {
		t.Errorf("no-org RepoSlug = %q", got)
	}
	if got := RepoSlug("acme", " "); got != "" {
		t.Errorf("blank RepoSlug = %q", got)
	}
	if got := AgentName(" Foo Bot "); got != "foo-bot" {
		t.Errorf("AgentName = %q", got)
	}
	if _, ok := New(Options{}).Get("nope"); ok {
		t.Error("Get on empty should miss")
	}
}
