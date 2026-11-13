package inbox

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestServiceSettingsFindAndConfigUpdate(t *testing.T) {
	src := &fakeSource{items: []Item{{Source: "plaud", ID: "rec-1", Title: "one"}}}
	spek := &recordingDest{name: DestSpekRFC}
	dibs := &recordingDest{name: DestDibs}
	svc := newTestService(t, config.InboxConfig{}, src, spek, dibs)
	if spek.Name() != DestSpekRFC || dibs.Name() != DestDibs {
		t.Fatal("destination names")
	}

	st := svc.Settings()
	if st.Rules == nil || len(st.Rules) != 0 || st.SpekReady || st.DibsTokenSet {
		t.Fatalf("default settings = %+v", st)
	}
	svc.SetReadiness(func() bool { return true }, func() bool { return true })
	svc.UpdateConfig(config.InboxConfig{
		Rules: []config.InboxRule{{TitlePrefix: "RFC:", Destination: DestSpekRFC}},
		Spek:  config.InboxSpek{Repo: "o/r"},
		Dibs:  config.InboxDibs{URL: "https://dibs.example"},
	})
	st = svc.Settings()
	if !st.SpekReady || !st.DibsTokenSet || st.SpekRepo != "o/r" || st.DibsURL != "https://dibs.example" || len(st.Rules) != 1 {
		t.Fatalf("settings after update = %+v", st)
	}

	it, ok, err := svc.Find(context.Background(), "plaud", "rec-1")
	if err != nil || !ok || it.Title != "one" {
		t.Fatalf("find = %+v %v %v", it, ok, err)
	}
	if _, ok, _ := svc.Find(context.Background(), "plaud", "missing"); ok {
		t.Fatal("found a missing item")
	}
	svc.Wait()
}

func TestServiceNowAndSortItems(t *testing.T) {
	fixed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("x", 3600))
	svc := newTestService(t, config.InboxConfig{}, &fakeSource{}, &recordingDest{name: DestSpekRFC})
	if !svc.now().Before(time.Now().Add(time.Second)) {
		t.Fatal("now() without override should be wall clock")
	}
	svc.Now = func() time.Time { return fixed }
	if got := svc.now(); got != fixed.UTC() {
		t.Fatalf("now = %v", got)
	}
	items := []Item{
		{ID: "a", RecordedAt: fixed.Add(-time.Hour)},
		{ID: "b", RecordedAt: fixed},
		{ID: "c", RecordedAt: fixed.Add(-2 * time.Hour)},
	}
	sortItems(items)
	if items[0].ID != "b" || items[1].ID != "a" || items[2].ID != "c" {
		t.Fatalf("sorted = %v %v %v", items[0].ID, items[1].ID, items[2].ID)
	}
}

type failingSource struct {
	fakeSource
	listErr  error
	fetchErr error
}

func (f *failingSource) List(context.Context) ([]Item, error) { return f.items, f.listErr }
func (f *failingSource) Fetch(context.Context, Item) (Content, error) {
	return f.body, f.fetchErr
}

func TestServiceDeliverFailures(t *testing.T) {
	src := &failingSource{fakeSource: fakeSource{items: []Item{{Source: "plaud", ID: "rec-1", Title: "one"}}}, fetchErr: errors.New("boom")}
	dest := &recordingDest{name: DestSpekRFC, res: Result{Note: "delegated to agent x"}}
	svc := newTestService(t, config.InboxConfig{Spek: config.InboxSpek{Repo: "o/r"}}, src, dest)

	// Fetch error is recorded as failed.
	items, _ := svc.Items(context.Background())
	if _, err := svc.Route(context.Background(), items[0], Request{Destination: DestSpekRFC}); err != nil {
		t.Fatal(err)
	}
	items, _ = svc.Items(context.Background())
	if r := items[0].Routes[DestSpekRFC]; r == nil || r.Status != StatusFailed || !strings.Contains(r.Error, "boom") {
		t.Fatalf("fetch failure = %+v", r)
	}

	// Unknown source is recorded as failed.
	ghost := Item{Source: "ghost", ID: "g-1"}
	if _, err := svc.Route(context.Background(), ghost, Request{Destination: DestSpekRFC}); err != nil {
		t.Fatal(err)
	}
	if r, ok := svc.Store.Route(ghost.Key(), DestSpekRFC); !ok || r.Status != StatusFailed || !strings.Contains(r.Error, "not configured") {
		t.Fatalf("ghost route = %+v %v", r, ok)
	}

	// A note-only result (no URL) becomes delegated.
	src.fetchErr = nil
	if _, err := svc.Route(context.Background(), items[0], Request{Destination: DestSpekRFC}); err != nil {
		t.Fatal(err)
	}
	if r, ok := svc.Store.Route(items[0].Key(), DestSpekRFC); !ok || r.Status != StatusDelegated || r.Error != "delegated to agent x" {
		t.Fatalf("delegated route = %+v", r)
	}

	// Items surfaces list errors but still returns items.
	src.listErr = errors.New("list down")
	items, err := svc.Items(context.Background())
	if err == nil || !strings.Contains(err.Error(), "list down") || len(items) != 1 {
		t.Fatalf("items with list error = %d %v", len(items), err)
	}
	if _, err := svc.Sweep(context.Background()); err == nil {
		t.Fatal("sweep should surface the list error")
	}
	if known := svc.Store.Known(); len(known) != 2 || known[0] != ghost.Key() {
		t.Fatalf("known = %v", known)
	}
}

func TestStoreEdgeCases(t *testing.T) {
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mem := NewStore("")
	if mem.now().IsZero() {
		t.Fatal("now")
	}
	mem.Now = func() time.Time { return fixed }
	if st, err := mem.Begin("plaud/x", DestDibs, ""); err != nil || st.UpdatedAt != fixed {
		t.Fatalf("in-memory begin = %+v %v", st, err)
	}
	if _, ok := mem.Route("plaud/x", DestSpekRFC); ok {
		t.Fatal("route for another destination should be absent")
	}
	if _, ok := mem.Route("nope", DestDibs); ok {
		t.Fatal("route for unknown key should be absent")
	}

	// Corrupt file is tolerated; unwritable dir surfaces an error.
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if known := NewStore(bad).Known(); len(known) != 0 {
		t.Fatalf("corrupt store known = %v", known)
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(filepath.Join(blocker, "inbox.json")).Begin("k", DestDibs, ""); err == nil {
		t.Fatal("save under a file should fail")
	}
}

func TestFactSourceEdgeCases(t *testing.T) {
	src := &FactSource{ConnectorsDir: filepath.Join(t.TempDir(), "missing"), SourceType: "wisprflow"}
	if src.Type() != "wisprflow" {
		t.Fatal("type")
	}
	if items, err := src.List(context.Background()); err != nil || items != nil {
		t.Fatalf("missing dir = %v %v", items, err)
	}

	dir := t.TempDir()
	src.ConnectorsDir = dir
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFact(t, dir, "personal", "wisprflow-w-1.md", strings.Replace(plaudFact, "source: plaud", "source: wisprflow", 1))
	writeFact(t, dir, "personal", "wisprflow-w-2.md", "no front matter at all")
	writeFact(t, dir, "personal", "wisprflow-w-3.md", "---\nsource: wisprflow\nunterminated")
	writeFact(t, dir, "personal", "wisprflow-noid.md", "---\nsource: wisprflow\n---\nbody")
	if err := os.Mkdir(filepath.Join(dir, "personal", "wisprflow-dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	items, err := src.List(context.Background())
	if err != nil || len(items) != 1 || items[0].Source != "wisprflow" {
		t.Fatalf("items = %+v %v", items, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list = %v", err)
	}

	if _, err := src.Fetch(context.Background(), Item{FactPath: filepath.Join(dir, "nope.md")}); err == nil {
		t.Fatal("fetch of a missing fact should fail")
	}
	// Fetch falls back to the item title when the fact has none.
	path := filepath.Join(dir, "personal", "wisprflow-untitled.md")
	if err := os.WriteFile(path, []byte("---\nsource: wisprflow\nsource_id: u\n---\nhello"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := src.Fetch(context.Background(), Item{ID: "u", Title: "from item", FactPath: path})
	if err != nil || c.Title != "from item" || c.Markdown != "hello" || c.Provenance != "wisprflow://recordings/u" {
		t.Fatalf("fetch = %+v %v", c, err)
	}

	fm, body := splitFrontMatter("---\na: 1\n---")
	if fm["a"] != "1" || body != "" {
		t.Fatalf("front matter without trailing newline = %v %q", fm, body)
	}
}

func TestSpekDestinationRealExecAndLookPath(t *testing.T) {
	d := &SpekDestination{Binary: "definitely-not-a-binary-" + t.Name(), DefaultRepo: "o/r"}
	it := Item{Source: "plaud", ID: "r", Title: "RFC: x"}
	_, err := d.Deliver(context.Background(), it, Content{Transcript: "hi"}, Request{})
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing binary = %v", err)
	}
	if d.lookPath("sh") != nil {
		t.Skip("sh not on PATH")
	}
	out, err := d.exec(context.Background(), "sh", []string{"-c", "cat; echo done"}, os.Environ(), "input\n")
	if err != nil || !strings.Contains(out, "input") || !strings.Contains(out, "done") {
		t.Fatalf("exec = %q %v", out, err)
	}
	if got := lastLines("a\nb\nc\n", 2); got != "b\nc" {
		t.Fatalf("lastLines = %q", got)
	}
	if got := (&SpekDestination{Prefixes: []string{"X:"}}).prefixes(); len(got) != 1 || got[0] != "X:" {
		t.Fatalf("prefixes = %v", got)
	}
}

func TestSpekDestinationErrorPaths(t *testing.T) {
	it := Item{Source: "plaud", ID: "r", Title: "RFC: x"}
	ok := func(string) error { return nil }
	cases := []struct {
		name string
		d    *SpekDestination
		c    Content
		want string
	}{
		{"no repo", &SpekDestination{LookPath: ok}, Content{Transcript: "t"}, ErrNoRepo.Error()},
		{"empty", &SpekDestination{LookPath: ok, DefaultRepo: "o/r"}, Content{}, "no transcript"},
		{"token", &SpekDestination{LookPath: ok, DefaultRepo: "o/r", Token: func(context.Context, string) (string, error) { return "", errors.New("mint") }}, Content{Transcript: "t"}, "github token: mint"},
		{"exec", &SpekDestination{LookPath: ok, DefaultRepo: "o/r", Exec: func(context.Context, string, []string, []string, string) (string, error) {
			return "l1\nl2\nboom", errors.New("exit 2")
		}}, Content{Transcript: "t"}, "boom"},
		{"parse", &SpekDestination{LookPath: ok, DefaultRepo: "o/r", Exec: func(context.Context, string, []string, []string, string) (string, error) {
			return "nothing useful", nil
		}}, Content{Transcript: "t"}, "spek_rfc:"},
		{"kick fails", &SpekDestination{LookPath: func(string) error { return errors.New("nope") }, DefaultRepo: "o/r", Agent: "a", Kick: func(string, string) error { return errors.New("kick") }}, Content{Transcript: "t"}, "kick to a failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.d.Deliver(context.Background(), it, tc.c, Request{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNewServiceDefaultsLogger(t *testing.T) {
	svc := NewService(config.InboxConfig{}, NewStore(""), nil, nil, nil)
	if svc.Logger == nil {
		t.Fatal("nil logger should default")
	}
	svc.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	if items, err := svc.Items(context.Background()); err != nil || len(items) != 0 {
		t.Fatalf("items with no sources = %v %v", items, err)
	}
}
