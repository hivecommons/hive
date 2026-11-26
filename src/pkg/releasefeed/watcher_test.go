package releasefeed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type kickCall struct {
	agent    string
	releases []Release
	block    string
}

type fakeKicker struct {
	w     *Watcher
	calls []kickCall
	err   error
}

func (k *fakeKicker) Kick(_ context.Context, agent string, releases []Release) error {
	if k.err != nil {
		return k.err
	}
	// The kicker renders the agent's prompt while the watcher is unlocked,
	// so ${NEW_RELEASES} (Block) must already carry the pending releases.
	k.calls = append(k.calls, kickCall{agent: agent, releases: releases, block: k.w.Block(agent)})
	return nil
}

type watcherRig struct {
	feed   *fakeFeed
	w      *Watcher
	kicker *fakeKicker
	now    time.Time
	subs   map[string][]Source
	store  *FileStore
	token  string
	tokens []string
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newWatcherRig(t *testing.T) *watcherRig {
	t.Helper()
	r := &watcherRig{
		feed:  newFakeFeed(t),
		now:   time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
		store: NewFileStore(filepath.Join(t.TempDir(), "state", "release-feeds.json")),
	}
	r.subs = map[string][]Source{"deps": {{Kind: KindPyPI, Package: "requests"}}}
	r.kicker = &fakeKicker{}
	r.w = r.newWatcher()
	return r
}

func (r *watcherRig) newWatcher() *Watcher {
	w := New(Options{
		Subscriptions: func() map[string][]Source { return r.subs },
		Interval:      func() time.Duration { return time.Minute },
		Cooldown:      func(string) time.Duration { return time.Hour },
		Token: func(agent, name string) string {
			r.tokens = append(r.tokens, agent+":"+name)
			return r.token
		},
		Fetcher: r.feed.fetcher(),
		Store:   r.store,
		Kicker:  r.kicker,
		Logger:  quietLogger(),
		Now:     func() time.Time { return r.now },
	})
	r.kicker.w = w
	return w
}

// The done-when of #18494: the first answer is a baseline (no kick), an
// unchanged feed kicks nothing, and a new version kicks the subscriber in
// the same poll with the release block in its prompt.
func TestWatcher_BaselineThenKickOnNewVersion(t *testing.T) {
	r := newWatcherRig(t)
	ctx := context.Background()

	res := r.w.Poll(ctx)
	if res.Fetched != 1 || len(res.Kicked) != 0 || len(r.kicker.calls) != 0 {
		t.Fatalf("baseline poll: %+v, calls %d", res, len(r.kicker.calls))
	}
	res = r.w.Poll(ctx)
	if res.NotModified != 1 || len(r.kicker.calls) != 0 {
		t.Fatalf("unchanged poll must not kick: %+v, calls %d", res, len(r.kicker.calls))
	}
	if r.feed.seen().ifNoneMatch != `"v1"` {
		t.Fatalf("second poll was not conditional: %+v", r.feed.seen())
	}

	r.feed.set(func(f *fakeFeed) { f.version, f.etag = "1.1.0", `"v2"` })
	res = r.w.Poll(ctx)
	if res.Detected != 1 || len(res.Kicked) != 1 || len(r.kicker.calls) != 1 {
		t.Fatalf("new version poll: %+v, calls %d", res, len(r.kicker.calls))
	}
	call := r.kicker.calls[0]
	want := Release{Source: "pypi:requests", Package: "requests", Old: "1.0.0", New: "1.1.0", URL: "https://pypi.org/project/requests/1.1.0/"}
	if call.agent != "deps" || len(call.releases) != 1 || call.releases[0] != want {
		t.Fatalf("kick = %+v", call)
	}
	if !strings.Contains(call.block, BlockHeader) || !strings.Contains(call.block, "- requests: 1.0.0 -> 1.1.0 (https://pypi.org/project/requests/1.1.0/)") {
		t.Fatalf("block = %q", call.block)
	}
	if got := r.w.Block("deps"); got != "" {
		t.Fatalf("delivered releases still pending: %q", got)
	}

	// Nothing changed again: no second kick.
	r.w.Poll(ctx)
	if len(r.kicker.calls) != 1 {
		t.Fatalf("unchanged feed kicked again: %d calls", len(r.kicker.calls))
	}
}

// A second release inside the cooldown waits; it goes out once the cooldown
// has passed, folded into one entry that keeps the original old version.
func TestWatcher_CooldownHoldsThenDelivers(t *testing.T) {
	r := newWatcherRig(t)
	ctx := context.Background()
	r.w.Poll(ctx)
	r.feed.set(func(f *fakeFeed) { f.version, f.etag = "1.1.0", `"v2"` })
	r.w.Poll(ctx)
	if len(r.kicker.calls) != 1 {
		t.Fatalf("first release: %d calls", len(r.kicker.calls))
	}

	r.now = r.now.Add(10 * time.Minute)
	r.feed.set(func(f *fakeFeed) { f.version, f.etag = "1.2.0", `"v3"` })
	r.w.Poll(ctx)
	r.feed.set(func(f *fakeFeed) { f.version, f.etag = "1.3.0", `"v4"` })
	r.w.Poll(ctx)
	if len(r.kicker.calls) != 1 {
		t.Fatalf("cooldown did not hold: %d calls", len(r.kicker.calls))
	}
	if b := r.w.Block("deps"); !strings.Contains(b, "1.1.0 -> 1.3.0") {
		t.Fatalf("pending block = %q", b)
	}

	r.now = r.now.Add(time.Hour)
	r.w.Poll(ctx)
	if len(r.kicker.calls) != 2 {
		t.Fatalf("after cooldown: %d calls", len(r.kicker.calls))
	}
	if got := r.kicker.calls[1].releases; len(got) != 1 || got[0].Old != "1.1.0" || got[0].New != "1.3.0" {
		t.Fatalf("folded release = %+v", got)
	}
}

// A gated or failed kick leaves the release pending for the next poll.
func TestWatcher_GatedKickStaysPending(t *testing.T) {
	r := newWatcherRig(t)
	ctx := context.Background()
	r.w.Poll(ctx)
	r.feed.set(func(f *fakeFeed) { f.version, f.etag = "2.0.0", `"v2"` })
	r.kicker.err = fmt.Errorf("%w: agent paused", ErrGated)
	if res := r.w.Poll(ctx); len(res.Kicked) != 0 {
		t.Fatalf("gated poll kicked: %+v", res)
	}
	r.kicker.err = errors.New("pane gone")
	r.w.Poll(ctx)
	if b := r.w.Block("deps"); !strings.Contains(b, "1.0.0 -> 2.0.0") {
		t.Fatalf("pending lost after refusals: %q", b)
	}
	r.kicker.err = nil
	if res := r.w.Poll(ctx); len(res.Kicked) != 1 {
		t.Fatalf("open gate did not deliver: %+v", res)
	}
}

// Last-seen versions survive a restart: a fresh watcher on the same state
// file does not re-baseline, so a release published while the hive was down
// still kicks.
func TestWatcher_StatePersistsAcrossRestart(t *testing.T) {
	r := newWatcherRig(t)
	ctx := context.Background()
	r.w.Poll(ctx)

	r.feed.set(func(f *fakeFeed) { f.version, f.etag = "1.5.0", `"v5"` })
	r.w = r.newWatcher()
	r.w.Poll(ctx)
	if len(r.kicker.calls) != 1 || r.kicker.calls[0].releases[0].Old != "1.0.0" {
		t.Fatalf("restart re-baselined: %+v", r.kicker.calls)
	}
	data, err := os.ReadFile(r.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"1.5.0"`) {
		t.Fatalf("state file missing the version: %s", data)
	}
}

// A source with auth_var resolves the token for its subscriber and sends it;
// an unresolved variable skips the feed instead of polling anonymously.
func TestWatcher_AuthVar(t *testing.T) {
	r := newWatcherRig(t)
	r.subs = map[string][]Source{"deps": {{Kind: KindIndex, URL: r.feed.srv.URL + "/private", AuthVar: "INDEX_TOKEN"}}}
	ctx := context.Background()

	res := r.w.Poll(ctx)
	if res.Errors != 1 || r.feedHits() != 0 {
		t.Fatalf("unset token must skip the feed: %+v hits=%d", res, r.feedHits())
	}
	r.token = "s3cret"
	r.w.Poll(ctx)
	if got := r.feed.seen().auth; got != "Bearer s3cret" {
		t.Fatalf("token not sent: %q", got)
	}
	if len(r.tokens) == 0 || r.tokens[0] != "deps:INDEX_TOKEN" {
		t.Fatalf("token lookups = %v", r.tokens)
	}
	data, _ := os.ReadFile(r.store.path)
	if strings.Contains(string(data), "s3cret") {
		t.Fatal("token reached the state file")
	}
}

// Feeds shared by two agents are fetched once and kick both; an agent that
// drops its subscription is forgotten, and a feed error is counted.
func TestWatcher_SharedFeedPruneAndErrors(t *testing.T) {
	r := newWatcherRig(t)
	src := Source{Kind: KindGitHubRelease, Repo: "psf/requests"}
	r.subs = map[string][]Source{"a": {src}, "b": {src}}
	ctx := context.Background()
	r.w.Poll(ctx)
	if r.feedHits() != 1 {
		t.Fatalf("shared feed fetched %d times", r.feedHits())
	}
	r.feed.set(func(f *fakeFeed) { f.version, f.etag = "1.1.0", `"v2"` })
	if res := r.w.Poll(ctx); len(res.Kicked) != 2 {
		t.Fatalf("both subscribers should be kicked: %+v", res)
	}

	r.subs = map[string][]Source{"a": {{Kind: KindPyPI, Package: "other"}}}
	r.w.Poll(ctx)
	r.w.mu.Lock()
	_, bKept := r.w.state.Agents["b"]
	_, oldKept := r.w.state.Sources[src.Key()]
	_, oldSeen := r.w.state.Agents["a"].Seen[src.Key()]
	r.w.mu.Unlock()
	if bKept || oldKept || oldSeen {
		t.Fatalf("unsubscribed state kept: agent b=%v source=%v seen=%v", bKept, oldKept, oldSeen)
	}

	r.feed.set(func(f *fakeFeed) { f.status = http.StatusInternalServerError })
	if res := r.w.Poll(ctx); res.Errors != 1 {
		t.Fatalf("feed error not counted: %+v", res)
	}
}

func TestWatcher_IndexVersionFieldsAreIndependent(t *testing.T) {
	r := newWatcherRig(t)
	src := Source{Kind: KindIndex, URL: r.feed.srv.URL}
	preview := src
	preview.VersionField = "preview"
	shared := preview
	shared.Name = "Preview channel"
	r.subs = map[string][]Source{"stable": {src}, "preview": {preview}, "shared": {shared}}
	ctx := context.Background()
	r.feed.set(func(f *fakeFeed) { f.body = `{"version":"1.0.0","preview":"2.0.0-rc1"}` })
	if res := r.w.Poll(ctx); res.Fetched != 2 || res.Detected != 0 {
		t.Fatalf("baseline poll = %+v", res)
	}

	r.feed.set(func(f *fakeFeed) {
		f.body, f.etag = `{"version":"1.0.0","preview":"2.0.0-rc2"}`, `"v2"`
	})
	if res := r.w.Poll(ctx); res.Fetched != 2 || res.Detected != 2 || len(res.Kicked) != 2 {
		t.Fatalf("preview-only update = %+v", res)
	}
	for _, call := range r.kicker.calls {
		if call.agent == "stable" || len(call.releases) != 1 {
			t.Fatalf("unexpected kick: %+v", call)
		}
		if got := call.releases[0]; got.Old != "2.0.0-rc1" || got.New != "2.0.0-rc2" {
			t.Fatalf("%s got release %+v", call.agent, got)
		}
	}
}

func TestWatcher_RebaselinesLegacyIndexStateOnce(t *testing.T) {
	r := newWatcherRig(t)
	stable := Source{Kind: KindIndex, URL: r.feed.srv.URL}
	pypi := Source{Kind: KindPyPI, Package: "requests"}
	r.subs = map[string][]Source{"stable": {stable}, "deps": {pypi}}
	// The previous configuration watched preview. Only the default field is
	// configured at upgrade, but v1 gives both selectors the same key.
	legacy := fmt.Sprintf(`{"version":1,"sources":{
		%q:{"etag":"\"v1\"","version":"2.0.0-rc1"},
		%q:{"etag":"\"v1\"","version":"3.0.0"}
	},"agents":{"stable":{"seen":{%q:"2.0.0-rc1"},"pending":[
		{"source":%q,"old":"2.0.0-rc0","new":"2.0.0-rc1"}
	]},"deps":{"seen":{%q:"3.0.0"},"pending":[
		{"source":%q,"old":"2.9.0","new":"3.0.0"}
	]}}}`,
		stable.Key(), pypi.Key(), stable.Key(), stable.Key(), pypi.Key(), pypi.Key())
	if err := os.MkdirAll(filepath.Dir(r.store.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.store.path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	r.feed.set(func(f *fakeFeed) { f.body = `{"version":"1.0.0","preview":"2.0.0-rc1"}` })
	ctx := context.Background()
	if res := r.w.Poll(ctx); res.Fetched != 1 || res.NotModified != 1 || res.Detected != 0 ||
		len(res.Kicked) != 1 || res.Kicked[0] != "deps" {
		t.Fatalf("migration poll = %+v", res)
	}
	if got := r.kicker.calls[0].releases; len(got) != 1 || got[0].Old != "2.9.0" || got[0].New != "3.0.0" {
		t.Fatalf("pending PyPI release after migration = %+v", got)
	}

	upgraded, err := r.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Version != 1 || !upgraded.IndexFieldsSeparated {
		t.Fatalf("upgraded state must use rollback-readable version 1 and record the migration: %+v", upgraded)
	}

	r.w = r.newWatcher()
	delete(r.subs, "deps")
	r.feed.set(func(f *fakeFeed) {
		f.body, f.etag = `{"version":"1.1.0","preview":"2.0.0-rc1"}`, `"v2"`
	})
	if res := r.w.Poll(ctx); res.Detected != 1 || len(res.Kicked) != 1 || res.Kicked[0] != "stable" {
		t.Fatalf("stable release after restart = %+v", res)
	}
	if got := r.kicker.calls[1].releases; len(got) != 1 || got[0].Old != "1.0.0" || got[0].New != "1.1.0" {
		t.Fatalf("stable release after migration = %+v", got)
	}
}

func TestWatcher_DisabledNilKickerAndRun(t *testing.T) {
	r := newWatcherRig(t)
	off := New(Options{Enabled: func() bool { return false }, Fetcher: r.feed.fetcher()})
	if res := off.Poll(context.Background()); res.Fetched != 0 || res.Errors != 0 || r.feedHits() != 0 {
		t.Fatalf("disabled watcher polled: %+v", res)
	}
	var nilW *Watcher
	if nilW.Block("x") != "" || off.Block("x") != "" {
		t.Fatal("Block on an unloaded watcher must be empty")
	}

	// No kicker and no store: detection still works, nothing panics.
	w := New(Options{
		Subscriptions: func() map[string][]Source { return r.subs },
		Fetcher:       r.feed.fetcher(),
	})
	w.Poll(context.Background())
	r.feed.set(func(f *fakeFeed) { f.version, f.etag = "3.0.0", `"v3"` })
	if res := w.Poll(context.Background()); res.Detected != 1 || len(res.Kicked) != 0 {
		t.Fatalf("nil kicker: %+v", res)
	}

	// Run polls at once and returns when the context ends.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	hits := r.feedHits()
	go func() { w.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for r.feedHits() == hits {
		select {
		case <-deadline:
			t.Fatal("Run did not poll")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}

	// A cancelled context stops a poll before it fetches.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	before := r.feedHits()
	w.Poll(cctx)
	if r.feedHits() != before {
		t.Fatal("cancelled poll fetched")
	}
}

func (r *watcherRig) feedHits() int { return r.feed.seen().hits }

func TestFormatBlockAndReason(t *testing.T) {
	if FormatBlock(nil) != "" {
		t.Fatal("empty block must be empty")
	}
	rels := []Release{
		{Package: "a", Old: "1", New: "2"},
		{Package: "b", Old: "1", New: "2", URL: "https://x"},
		{Package: "c", Old: "1", New: "2"},
		{Package: "d", Old: "1", New: "2"},
	}
	b := FormatBlock(rels)
	if !strings.HasPrefix(b, BlockHeader+"\n- a: 1 -> 2\n- b: 1 -> 2 (https://x)\n") {
		t.Fatalf("block = %q", b)
	}
	if got := Reason(rels); got != "new release: a 1 -> 2, b 1 -> 2, c 1 -> 2 (+1 more)" {
		t.Fatalf("reason = %q", got)
	}
	if got := Reason(rels[:1]); got != "new release: a 1 -> 2" {
		t.Fatalf("reason = %q", got)
	}
}

func TestAddPendingRevertDropsEntry(t *testing.T) {
	a := &AgentState{}
	addPending(a, Release{Source: "k", Old: "1", New: "2"})
	addPending(a, Release{Source: "k", Old: "2", New: "1"})
	if len(a.Pending) != 0 {
		t.Fatalf("a version that moved back must drop the entry: %+v", a.Pending)
	}
}

func TestDelivered_KeepsNewerPending(t *testing.T) {
	w := New(Options{})
	w.ensureLoaded()
	a := w.state.agent("deps")
	a.Pending = []Release{{Source: "k", Old: "1", New: "3"}, {Source: "j", Old: "1", New: "2"}}
	now := time.Now()
	w.delivered("deps", []Release{{Source: "k", Old: "1", New: "2"}, {Source: "j", Old: "1", New: "2"}}, now)
	if len(a.Pending) != 1 || a.Pending[0].Old != "2" || a.Pending[0].New != "3" || !a.LastKick.Equal(now) {
		t.Fatalf("pending after delivery = %+v", a.Pending)
	}
	w.delivered("ghost", nil, now)
}
