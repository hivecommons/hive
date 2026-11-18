package remoteitems

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hivecommons/hive#18199: the remote-item poller tracks the hive's own marked
// items in reference repos, routes new feedback to the agent that opened them,
// and does nothing else on the remote.

const (
	owner  = "nate-double-u"
	marker = "\n\n-- hive: agent=docs-assessor backend=claude"
)

type fakeForge struct {
	mu       sync.Mutex
	items    map[string][]RemoteItem // by repo
	comments map[string][]Comment    // by "repo#n"
	listErr  map[string]error
	fetchErr map[string]error
	calls    []string
}

func newForge() *fakeForge {
	return &fakeForge{items: map[string][]RemoteItem{}, comments: map[string][]Comment{}, listErr: map[string]error{}, fetchErr: map[string]error{}}
}

func (f *fakeForge) ListOwnOpenItems(_ context.Context, repo, login string) ([]RemoteItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "list "+repo+" "+login)
	if err := f.listErr[repo]; err != nil {
		return nil, err
	}
	var out []RemoteItem
	for _, it := range f.items[repo] {
		if strings.EqualFold(it.Author, login) {
			out = append(out, it)
		}
	}
	return out, nil
}

func (f *fakeForge) CommentsSince(_ context.Context, repo string, number int, _ bool, since time.Time) ([]Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := itemKey(repo, number)
	f.calls = append(f.calls, "comments "+key)
	if err := f.fetchErr[key]; err != nil {
		return nil, err
	}
	var out []Comment
	for _, c := range f.comments[key] {
		if c.CreatedAt.After(since) {
			out = append(out, c)
		}
	}
	return out, nil
}

type fakeResumer struct {
	sessions map[string]string
	busy     bool
	sent     []string
}

func (r *fakeResumer) SessionID(name string) (string, bool) {
	s, ok := r.sessions[name]
	return s, ok
}

func (r *fakeResumer) SendResumeKick(name, message, sessionID string) error {
	if r.busy {
		return errors.New("busy")
	}
	r.sent = append(r.sent, name+": "+message)
	return nil
}

func testPoller(t *testing.T, forge *fakeForge) (*Poller, *Store) {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "turn", "remote-items.json"))
	known := map[string]string{"docs-assessor": "docs-assessor", "Docs Assessor": "docs-assessor"}
	p := &Poller{
		Store:        store,
		Fetch:        forge,
		Marker:       testMarker,
		ResolveAgent: func(name string) string { return known[name] },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return p, store
}

// testMarker: the trailer names the agent after "agent="; the footer form
// names it in backticks; anything else is unmarked.
func testMarker(body string) (string, bool) {
	if i := strings.Index(body, "-- hive: agent="); i >= 0 {
		rest := body[i+len("-- hive: agent="):]
		if j := strings.IndexAny(rest, " \n"); j >= 0 {
			rest = rest[:j]
		}
		return rest, true
	}
	if i := strings.Index(body, "🐝 **Hive Agent**: `"); i >= 0 {
		rest := body[i+len("🐝 **Hive Agent**: `"):]
		if j := strings.Index(rest, "`"); j >= 0 {
			return rest[:j], true
		}
		return "", true
	}
	return "", false
}

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func TestRun_TracksMarkedOwnItemsOnly(t *testing.T) {
	forge := newForge()
	forge.items["hivecommons/docs"] = []RemoteItem{
		{Repo: "hivecommons/docs", Number: 7, Title: "Findings", Body: "Assessment findings" + marker, URL: "https://github.com/hivecommons/docs/issues/7", Author: owner, CreatedAt: t0},
		{Repo: "hivecommons/docs", Number: 8, IsPR: true, Body: "Fix headings\n\n---\n🐝 **Hive Agent**: `Docs Assessor` | **SHA:** `abc`", Author: owner, CreatedAt: t0},
		{Repo: "hivecommons/docs", Number: 9, Body: "I filed this by hand", Author: owner, CreatedAt: t0},                          // no marker
		{Repo: "hivecommons/docs", Number: 10, Body: "someone else's" + marker, Author: "other", CreatedAt: t0},                    // not ours
		{Repo: "hivecommons/docs", Number: 11, Body: "marker, unknown agent\n\n-- hive: agent=ghost", Author: owner, CreatedAt: t0}, // unknown agent
	}
	p, store := testPoller(t, forge)
	out := p.Run(context.Background(), []string{"hivecommons/docs", "hivecommons/site"}, owner, nil, t0.Add(time.Hour))
	if out.Repos != 2 || out.Added != 3 || out.Tracked != 3 || out.Pruned != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	items, err := store.Items()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("tracked %+v", items)
	}
	byNum := map[int]Item{}
	for _, it := range items {
		byNum[it.Number] = it
	}
	if it := byNum[7]; it.Agent != "docs-assessor" || it.IsPR || it.URL == "" || !it.Cursor.Equal(t0) || it.FirstSeen.IsZero() {
		t.Errorf("issue 7 = %+v", it)
	}
	if it := byNum[8]; it.Agent != "docs-assessor" || !it.IsPR {
		t.Errorf("PR 8 (footer display name) = %+v", it)
	}
	if it := byNum[11]; it.Agent != "" {
		t.Errorf("unknown agent must be tracked unrouted: %+v", it)
	}
	if _, ok := byNum[9]; ok {
		t.Error("an unmarked item by the owner is the owner's own, not the hive's")
	}
	if _, ok := byNum[10]; ok {
		t.Error("another author's item must never be tracked")
	}
	// The store file is owner-only.
	if info, err := os.Stat(store.Path()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode = %v err=%v", info.Mode().Perm(), err)
	}
	// Only listings and per-item comment reads touched the forge: no backlog
	// scan, nothing else.
	for _, c := range forge.calls {
		if !strings.HasPrefix(c, "list ") && !strings.HasPrefix(c, "comments ") {
			t.Errorf("unexpected forge call %q", c)
		}
	}
}

func TestRun_RoutesFeedbackAndAdvancesCursor(t *testing.T) {
	forge := newForge()
	forge.items["hivecommons/docs"] = []RemoteItem{
		{Repo: "hivecommons/docs", Number: 8, IsPR: true, Title: "Fix headings", Body: "x" + marker, URL: "https://github.com/hivecommons/docs/pull/8", Author: owner, CreatedAt: t0},
	}
	forge.comments["hivecommons/docs#8"] = []Comment{
		{Kind: "comment", Author: owner, Body: "our own follow-up", CreatedAt: t0.Add(1 * time.Minute)},
		{Kind: "comment", Author: "dependabot[bot]", Body: "bot noise", CreatedAt: t0.Add(2 * time.Minute)},
		{Kind: "review", Author: "maintainer", Body: "[changes_requested] Please split this", URL: "https://github.com/hivecommons/docs/pull/8#r1", CreatedAt: t0.Add(3 * time.Minute)},
		{Kind: "review_comment", Author: "maintainer", Body: "typo here", CreatedAt: t0.Add(4 * time.Minute)},
	}
	p, store := testPoller(t, forge)
	r := &fakeResumer{sessions: map[string]string{"docs-assessor": "s1"}}

	out := p.Run(context.Background(), []string{"hivecommons/docs"}, owner, r, t0.Add(time.Hour))
	if out.Routed != 1 || out.Queued != 0 || out.Unrouted != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	if len(r.sent) != 1 {
		t.Fatalf("sent = %v", r.sent)
	}
	msg := r.sent[0]
	for _, want := range []string{
		"docs-assessor: REMOTE PR FOLLOW-UP: new activity on hivecommons/docs#8 (https://github.com/hivecommons/docs/pull/8)",
		"review from maintainer (https://github.com/hivecommons/docs/pull/8#r1): [changes_requested] Please split this",
		"review comment from maintainer: typo here",
		"push any fix to the SAME branch on the owner's fork",
		"do not label, assign, review or close anything there",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "our own follow-up") || strings.Contains(msg, "bot noise") {
		t.Errorf("own and bot comments must not be routed:\n%s", msg)
	}
	items, _ := store.Items()
	if !items[0].Cursor.Equal(t0.Add(4 * time.Minute)) {
		t.Fatalf("cursor = %v, want the newest routed comment", items[0].Cursor)
	}

	// Second pass with nothing new: no delivery, cursor stays.
	r.sent = nil
	out = p.Run(context.Background(), []string{"hivecommons/docs"}, owner, r, t0.Add(2*time.Hour))
	if out.Routed != 0 || len(r.sent) != 0 {
		t.Fatalf("quiet pass delivered: %+v %v", out, r.sent)
	}
	// Only our own comment arrives: the cursor still advances past it.
	forge.comments["hivecommons/docs#8"] = append(forge.comments["hivecommons/docs#8"], Comment{Kind: "comment", Author: owner, Body: "done", CreatedAt: t0.Add(5 * time.Minute)})
	p.Run(context.Background(), []string{"hivecommons/docs"}, owner, r, t0.Add(3*time.Hour))
	items, _ = store.Items()
	if !items[0].Cursor.Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("cursor must skip past our own comment, got %v", items[0].Cursor)
	}

	// A burst is bounded per pass; the rest comes next pass.
	for i := 0; i < maxCommentsPerItemPerPass+2; i++ {
		forge.comments["hivecommons/docs#8"] = append(forge.comments["hivecommons/docs#8"], Comment{Kind: "comment", Author: "m", Body: "c", CreatedAt: t0.Add(time.Duration(10+i) * time.Minute)})
	}
	r.sent = nil
	p.Run(context.Background(), []string{"hivecommons/docs"}, owner, r, t0.Add(4*time.Hour))
	if n := strings.Count(r.sent[0], "\n- "); n != maxCommentsPerItemPerPass {
		t.Fatalf("first burst pass routed %d comments, want %d", n, maxCommentsPerItemPerPass)
	}
	r.sent = nil
	p.Run(context.Background(), []string{"hivecommons/docs"}, owner, r, t0.Add(5*time.Hour))
	if n := strings.Count(r.sent[0], "\n- "); n != 2 {
		t.Fatalf("second burst pass routed %d comments, want the remaining 2", n)
	}
}

func TestRun_QueuesHandoffWhenNoSessionAndReconciles(t *testing.T) {
	forge := newForge()
	forge.items["hivecommons/docs"] = []RemoteItem{
		{Repo: "hivecommons/docs", Number: 7, Body: "x" + marker, URL: "https://github.com/hivecommons/docs/issues/7", Author: owner, CreatedAt: t0},
	}
	forge.comments["hivecommons/docs#7"] = []Comment{{Kind: "comment", Author: "maintainer", Body: "Can you add the TOC?", CreatedAt: t0.Add(time.Minute)}}
	p, store := testPoller(t, forge)

	// No live session: queued for the next kick, with an empty session.
	r := &fakeResumer{sessions: map[string]string{}}
	out := p.Run(context.Background(), []string{"hivecommons/docs"}, owner, r, t0.Add(time.Hour))
	if out.Queued != 1 || out.Routed != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	section := HandoffSection(store.Path(), "docs-assessor")
	for _, want := range []string{"🌐 REMOTE ITEM FOLLOW-UP", "issue hivecommons/docs#7 (https://github.com/hivecommons/docs/issues/7): comment from maintainer: Can you add the TOC?", "never label, assign, review or close"} {
		if !strings.Contains(section, want) {
			t.Errorf("handoff section lacks %q:\n%s", want, section)
		}
	}
	if HandoffSection(store.Path(), "other-agent") != "" {
		t.Error("another agent's kick must not carry it")
	}
	if HandoffSection(filepath.Join(t.TempDir(), "missing.json"), "docs-assessor") != "" {
		t.Error("a missing store renders nothing")
	}
	pend, _ := store.PendingFor("docs-assessor")
	if len(pend) != 1 || pend[0].Session != "" {
		t.Fatalf("pending = %+v", pend)
	}

	// The agent came up on a session: a fresh kick carried the handoff, so the
	// next pass drops it (session differs from the one recorded).
	r.sessions["docs-assessor"] = "s2"
	p.Run(context.Background(), []string{"hivecommons/docs"}, owner, r, t0.Add(2*time.Hour))
	if pend, _ := store.PendingFor("docs-assessor"); len(pend) != 0 {
		t.Fatalf("delivered handoff must be reconciled away, got %+v", pend)
	}

	// A busy session queues with that session recorded, and the entry stays
	// while the session is unchanged.
	forge.comments["hivecommons/docs#7"] = append(forge.comments["hivecommons/docs#7"], Comment{Kind: "comment", Author: "maintainer", Body: "also the index", CreatedAt: t0.Add(3 * time.Hour)})
	r.busy = true
	out = p.Run(context.Background(), []string{"hivecommons/docs"}, owner, r, t0.Add(4*time.Hour))
	if out.Queued != 1 {
		t.Fatalf("busy session must queue, got %+v", out)
	}
	pend, _ = store.PendingFor("docs-assessor")
	if len(pend) != 1 || pend[0].Session != "s2" {
		t.Fatalf("pending = %+v", pend)
	}
	r.busy = false
	p.Run(context.Background(), []string{"hivecommons/docs"}, owner, r, t0.Add(5*time.Hour))
	if pend, _ := store.PendingFor("docs-assessor"); len(pend) != 1 {
		t.Fatalf("same session: handoff must stay queued, got %+v", pend)
	}

	// Nil resumer: everything queues.
	forge.comments["hivecommons/docs#7"] = append(forge.comments["hivecommons/docs#7"], Comment{Kind: "comment", Author: "m", Body: "more", CreatedAt: t0.Add(6 * time.Hour)})
	if out := p.Run(context.Background(), []string{"hivecommons/docs"}, owner, nil, t0.Add(7*time.Hour)); out.Queued != 1 {
		t.Fatalf("nil resumer must queue, got %+v", out)
	}
}

func TestRun_PrunesAndKeepsOnErrors(t *testing.T) {
	forge := newForge()
	forge.items["hivecommons/docs"] = []RemoteItem{{Repo: "hivecommons/docs", Number: 7, Body: "x" + marker, Author: owner, CreatedAt: t0}}
	forge.items["hivecommons/site"] = []RemoteItem{{Repo: "hivecommons/site", Number: 3, Body: "y" + marker, Author: owner, CreatedAt: t0}}
	p, store := testPoller(t, forge)
	repos := []string{"hivecommons/docs", "hivecommons/site"}
	p.Run(context.Background(), repos, owner, nil, t0.Add(time.Hour))
	if items, _ := store.Items(); len(items) != 2 {
		t.Fatalf("precondition: %+v", items)
	}

	// A listing error keeps that repo's items; a fetch error keeps the cursor.
	forge.listErr["hivecommons/site"] = errors.New("502")
	forge.fetchErr["hivecommons/docs#7"] = errors.New("503")
	forge.comments["hivecommons/docs#7"] = []Comment{{Kind: "comment", Author: "m", Body: "hi", CreatedAt: t0.Add(time.Minute)}}
	out := p.Run(context.Background(), repos, owner, nil, t0.Add(2*time.Hour))
	if out.ListErrs != 1 || out.FetchErrs != 1 || out.Pruned != 0 || out.Tracked != 2 {
		t.Fatalf("outcome = %+v", out)
	}
	items, _ := store.Items()
	for _, it := range items {
		if it.Number == 7 && !it.Cursor.Equal(t0) {
			t.Fatalf("fetch error must not move the cursor: %+v", it)
		}
	}
	delete(forge.listErr, "hivecommons/site")
	delete(forge.fetchErr, "hivecommons/docs#7")

	// Closed on the remote (no longer listed): pruned, and its pending with it.
	forge.items["hivecommons/site"] = nil
	out = p.Run(context.Background(), repos, owner, nil, t0.Add(3*time.Hour))
	if out.Pruned != 1 || out.Tracked != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	// The repo stops being a write-tier reference repo: its items drop too.
	out = p.Run(context.Background(), []string{"hivecommons/site"}, owner, nil, t0.Add(4*time.Hour))
	if out.Pruned != 1 || out.Tracked != 0 {
		t.Fatalf("outcome = %+v", out)
	}

	// Degenerate inputs do nothing and do not panic.
	if out := p.Run(context.Background(), nil, owner, nil, t0); out != (Outcome{}) {
		t.Fatalf("no repos = %+v", out)
	}
	if out := p.Run(context.Background(), repos, "", nil, t0); out != (Outcome{}) {
		t.Fatalf("no login = %+v", out)
	}
	if out := (*Poller)(nil).Run(context.Background(), repos, owner, nil, t0); out != (Outcome{}) {
		t.Fatalf("nil poller = %+v", out)
	}
	// A corrupt store is reported and the pass skipped, never a panic.
	if err := os.WriteFile(store.Path(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := p.Run(context.Background(), repos, owner, nil, t0); out != (Outcome{}) {
		t.Fatalf("corrupt store = %+v", out)
	}
	if _, err := store.Items(); err == nil {
		t.Fatal("corrupt store must surface as an error")
	}
}

func TestBuildMessage_IssueAndTruncation(t *testing.T) {
	it := Item{Repo: "hivecommons/docs", Number: 7}
	long := strings.Repeat("x", maxDetailRunes+50)
	msg := BuildMessage(it, []Comment{{Kind: "comment", Author: "m", Body: long}})
	if !strings.HasPrefix(msg, "REMOTE ISSUE FOLLOW-UP: new activity on hivecommons/docs#7,") {
		t.Fatalf("message = %q", msg)
	}
	if !strings.Contains(msg, "reply on the issue") || strings.Contains(msg, "fork") {
		t.Fatalf("issue message must not talk about the fork: %q", msg)
	}
	if strings.Contains(msg, long) || !strings.Contains(msg, "…") {
		t.Fatal("long comment must be truncated")
	}
	if Path() != DefaultPath {
		t.Fatalf("default path = %q", Path())
	}
	t.Setenv(FileEnv, "/tmp/x.json")
	if Path() != "/tmp/x.json" || NewStore("").Path() != "/tmp/x.json" {
		t.Fatal("env override not honoured")
	}
}

func TestPendingCapPerAgent(t *testing.T) {
	var list []Pending
	for i := 0; i < maxPendingPerAgent+3; i++ {
		list = appendPending(list, Pending{Key: "k", Agent: "a", Detail: strings.Repeat("d", i+1)})
	}
	if len(list) != maxPendingPerAgent {
		t.Fatalf("len = %d, want %d", len(list), maxPendingPerAgent)
	}
	if list[0].Detail != strings.Repeat("d", 4) {
		t.Fatalf("oldest entries must be dropped first, got %q", list[0].Detail)
	}
}
