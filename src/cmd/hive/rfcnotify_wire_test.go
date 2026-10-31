package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rfcnotify"
)

type rfcNotifyFakeSource struct {
	open, updated []rfcnotify.Issue
	opens, polls  int
}

func (f *rfcNotifyFakeSource) ListOpen(context.Context) ([]rfcnotify.Issue, error) {
	f.opens++
	return f.open, nil
}

func (f *rfcNotifyFakeSource) ListUpdatedSince(context.Context, time.Time) ([]rfcnotify.Issue, error) {
	f.polls++
	return f.updated, nil
}

func rfcNotifyConfig(token, channel string) *config.Config {
	cfg := &config.Config{}
	cfg.Project.Repos = []string{"hive"}
	cfg.Notifications.Discord = &config.DiscordConfig{BotToken: token, RFCChannelID: channel}
	return cfg
}

func TestRunRFCNotifySweepDisabledIsNoOp(t *testing.T) {
	src := &rfcNotifyFakeSource{open: []rfcnotify.Issue{{Repo: "o/r", Number: 1, Title: "RFC: a", State: "open"}}}
	state := &rfcNotifyState{newPoster: func(string, string) rfcnotify.Poster {
		t.Fatal("disabled notices must not build a poster")
		return nil
	}}
	for _, cfg := range []*config.Config{nil, {}, rfcNotifyConfig("", "123"), rfcNotifyConfig("tok", "")} {
		if res := runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, nil); res.Seeded || len(res.Posted) != 0 {
			t.Fatalf("disabled config acted: %+v", res)
		}
	}
	if src.opens != 0 {
		t.Fatal("disabled notices must not list issues")
	}
	if res := runRFCNotifySweep(context.Background(), nil, 1, rfcNotifyConfig("tok", "123"), nil, state, nil); res.Seeded {
		t.Fatal("no source must no-op")
	}
	if res := runRFCNotifySweep(context.Background(), src, 1, rfcNotifyConfig("tok", "123"), nil, nil, nil); res.Seeded {
		t.Fatal("no state must no-op")
	}
}

func TestRunRFCNotifySweepSeedsThenPosts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var posted []string
	var builtFor []string
	state := &rfcNotifyState{
		statePath: filepath.Join(t.TempDir(), "rfc-notify.json"),
		newPoster: func(token, channel string) rfcnotify.Poster {
			builtFor = append(builtFor, token+"@"+channel)
			return func(s string) error { posted = append(posted, s); return nil }
		},
	}
	held := rfcnotify.Issue{Repo: "hivecommons/hive", Number: 12062, Title: "[RFC]: Evidence pipeline", URL: "https://github.com/hivecommons/hive/issues/12062", State: "open", Labels: []string{"hold"}, CreatedAt: time.Now().Add(-time.Hour)}
	src := &rfcNotifyFakeSource{open: []rfcnotify.Issue{held}}
	cfg := rfcNotifyConfig("tok", "1540404487099777136")

	res := runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	if !res.Seeded || len(posted) != 0 || src.opens != 1 {
		t.Fatalf("seed: %+v posted=%v opens=%d", res, posted, src.opens)
	}
	if len(builtFor) != 1 || builtFor[0] != "tok@1540404487099777136" {
		t.Fatalf("poster built for %v", builtFor)
	}
	// Same cycle again: inside the interval, skipped without listing.
	if res := runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger); !res.Skipped || src.polls != 0 {
		t.Fatalf("expected skip: %+v polls=%d", res, src.polls)
	}
	// Force the next poll and lift the hold.
	lifted := held
	lifted.Labels = nil
	src.updated = []rfcnotify.Issue{lifted}
	state.sweeper = rfcnotify.NewSweeper(rfcnotify.Options{StatePath: state.statePath, Logger: logger})
	res = runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	if len(res.Posted) != 1 || len(posted) != 1 || !strings.HasPrefix(posted[0], "RFC ready to implement (hold removed): #12062 ") {
		t.Fatalf("lift: %+v posted=%v", res, posted)
	}
	// A channel change rebuilds the poster; the token alone does not.
	cfg.Notifications.Discord.RFCChannelID = "999"
	state.sweeper = rfcnotify.NewSweeper(rfcnotify.Options{StatePath: state.statePath, Logger: logger})
	runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	if len(builtFor) != 2 || builtFor[1] != "tok@999" {
		t.Fatalf("poster not rebuilt for the new channel: %v", builtFor)
	}
}

func TestRunRFCNotifySweepIncludesRepoForMultiRepoHive(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var posted []string
	state := &rfcNotifyState{
		statePath: filepath.Join(t.TempDir(), "rfc-notify.json"),
		newPoster: func(string, string) rfcnotify.Poster {
			return func(s string) error { posted = append(posted, s); return nil }
		},
	}
	cfg := rfcNotifyConfig("tok", "1")
	cfg.Project.Repos = []string{"a", "b"}
	src := &rfcNotifyFakeSource{}
	runRFCNotifySweep(context.Background(), src, 2, cfg, nil, state, logger)
	fresh := rfcnotify.Issue{Repo: "org/b", Number: 5, Title: "RFC: x", URL: "u", State: "open", Author: "bob", CreatedAt: time.Now().Add(time.Hour)}
	src.updated = []rfcnotify.Issue{fresh}
	state.sweeper = rfcnotify.NewSweeper(rfcnotify.Options{StatePath: state.statePath, IncludeRepo: true, Logger: logger})
	runRFCNotifySweep(context.Background(), src, 2, cfg, nil, state, logger)
	if len(posted) != 1 || posted[0] != "RFC opened: org/b#5 RFC: x - <u> (by @bob)" {
		t.Fatalf("posted = %v", posted)
	}
}

func TestRFCNotifySourceAdaptsClient(t *testing.T) {
	if newRFCNotifySource(nil, nil) != nil {
		t.Fatal("nil client must yield a nil source")
	}
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":7,"title":"RFC: seven","state":"closed","state_reason":"completed","html_url":"https://x/7","user":{"login":"ann"},"labels":[{"name":"hold"}],"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-27T00:00:00Z"}]`))
	}))
	defer server.Close()
	c := github.NewClientForTest(server.URL, "org", []string{"repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	src := newRFCNotifySource(c, &rfcNotifyState{lookupPrivate: func(context.Context, string) (bool, error) { return false, nil }})
	open, err := src.ListOpen(context.Background())
	if err != nil || len(open) != 1 || open[0].Repo != "org/repo" || open[0].Number != 7 || open[0].Author != "ann" || open[0].StateReason != "completed" || len(open[0].Labels) != 1 || open[0].Private {
		t.Fatalf("ListOpen = %+v %v", open, err)
	}
	since := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	if _, err := src.ListUpdatedSince(context.Background(), since); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "state=open") || !strings.Contains(queries[1], "state=all") || !strings.Contains(queries[1], "since=") {
		t.Fatalf("queries = %v", queries)
	}
}

func rfcNotifyRecorder(t *testing.T, posted *[]string) *rfcNotifyState {
	t.Helper()
	return &rfcNotifyState{
		statePath: filepath.Join(t.TempDir(), "rfc-notify.json"),
		newPoster: func(string, string) rfcnotify.Poster {
			return func(s string) error { *posted = append(*posted, s); return nil }
		},
	}
}

// forcePoll drops the sweeper so the next run rebuilds it on the same
// state file and polls instead of waiting out the interval.
func forcePoll(state *rfcNotifyState) { state.sweeper = nil }

func TestRFCNotifyGatesFor(t *testing.T) {
	if g := rfcNotifyGatesFor(nil, 1); g != (rfcNotifyGates{format: rfcnotify.FormatLine}) {
		t.Fatalf("nil config gates = %+v", g)
	}
	cfg := rfcNotifyConfig("tok", "1")
	if g := rfcNotifyGatesFor(cfg, 3); g != (rfcNotifyGates{includeRepo: true, format: rfcnotify.FormatLine}) {
		t.Fatalf("default gates = %+v", g)
	}
	d := cfg.Notifications.Discord
	d.RFCFormat, d.RFCThreads, d.RFCAnnouncer, d.RFCBacklinkComment, d.RFCIncludePrivate = "card", true, true, true, true
	want := rfcNotifyGates{format: rfcnotify.FormatCard, threads: true, announcer: true, backlink: true, includePrivate: true}
	if g := rfcNotifyGatesFor(cfg, 1); g != want {
		t.Fatalf("gates = %+v, want %+v", g, want)
	}
}

func TestRunRFCNotifySweepGatesFromConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var posted []string
	state := rfcNotifyRecorder(t, &posted)
	open := rfcnotify.Issue{Repo: "o/r", Number: 1, Title: "RFC: a", URL: "u", State: "open", CreatedAt: time.Now().Add(-time.Hour)}
	priv := rfcnotify.Issue{Repo: "o/p", Number: 2, Title: "RFC: b", URL: "v", State: "open", Private: true, CreatedAt: time.Now().Add(-time.Hour)}
	src := &rfcNotifyFakeSource{open: []rfcnotify.Issue{open, priv}}
	cfg := rfcNotifyConfig("tok", "1")
	cfg.Notifications.Discord.RFCFormat = "card"
	runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)

	// Rich mode without the announcer: nothing posted, state advances.
	closed := open
	closed.State, closed.StateReason = "closed", "completed"
	src.updated = []rfcnotify.Issue{closed}
	forcePoll(state)
	res := runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	if len(posted) != 0 || res.Withheld != 1 {
		t.Fatalf("non-announcer: %+v posted=%v", res, posted)
	}
	// A config change rebuilds the sweeper with the new gates.
	cfg.Notifications.Discord.RFCAnnouncer = true
	before := state.sweeper
	privClosed := priv
	privClosed.State, privClosed.StateReason = "closed", "completed"
	src.updated = []rfcnotify.Issue{closed, privClosed}
	res = runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	if state.sweeper == before || !state.gates.announcer {
		t.Fatal("config change did not rebuild the sweeper")
	}
	// #1 already advanced (no backfill); private #2 is withheld.
	if len(posted) != 0 || res.Withheld != 1 {
		t.Fatalf("announcer, private: %+v posted=%v", res, posted)
	}
}

func TestRunRFCNotifySweepIncludePrivate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var posted []string
	state := rfcNotifyRecorder(t, &posted)
	cfg := rfcNotifyConfig("tok", "1")
	cfg.Notifications.Discord.RFCIncludePrivate = true
	src := &rfcNotifyFakeSource{}
	runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	src.updated = []rfcnotify.Issue{{Repo: "o/p", Number: 3, Title: "RFC: c", URL: "w", State: "open", Private: true, CreatedAt: time.Now().Add(time.Hour)}}
	forcePoll(state)
	runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	if len(posted) != 1 || posted[0] != "RFC opened: #3 RFC: c - <w>" {
		t.Fatalf("posted = %v", posted)
	}
}

func TestRFCNotifyVisibilityCacheFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	calls := map[string]int{}
	state := &rfcNotifyState{
		now: func() time.Time { return now },
		lookupPrivate: func(_ context.Context, repo string) (bool, error) {
			calls[repo]++
			switch repo {
			case "o/pub":
				return false, nil
			case "o/priv":
				return true, nil
			}
			return false, errors.New("boom")
		},
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if priv, err := state.repoPrivate(ctx, nil, "o/pub"); priv || err != nil {
			t.Fatalf("public repo = %v, %v", priv, err)
		}
	}
	if calls["o/pub"] != 1 {
		t.Fatalf("public repo not cached: calls=%v", calls)
	}
	if priv, err := state.repoPrivate(ctx, nil, "o/priv"); !priv || err != nil {
		t.Fatal("private repo must be private")
	}
	for i := 0; i < 2; i++ {
		if priv, err := state.repoPrivate(ctx, nil, "o/err"); !priv || err == nil {
			t.Fatalf("failed lookup must fail closed with an error: %v, %v", priv, err)
		}
	}
	if calls["o/err"] != 2 {
		t.Fatalf("failed lookup must retry: calls=%v", calls)
	}
	now = now.Add(rfcRepoVisibilityTTL)
	_, _ = state.repoPrivate(ctx, nil, "o/pub")
	if calls["o/pub"] != 2 {
		t.Fatalf("visibility not refreshed after the TTL: calls=%v", calls)
	}

	// The source marks RFCs only, and a nil state fails closed.
	issues := []rfcnotify.Issue{{Repo: "o/pub", Title: "RFC: x"}, {Repo: "o/err", Title: "not one"}}
	got := rfcNotifySource{state: state}.markPrivate(ctx, append([]rfcnotify.Issue(nil), issues...))
	if got[0].Private || got[1].Private || calls["o/err"] != 2 {
		t.Fatalf("markPrivate = %+v calls=%v", got, calls)
	}
	if got := (rfcNotifySource{}).markPrivate(ctx, append([]rfcnotify.Issue(nil), issues...)); !got[0].Private || got[1].Private {
		t.Fatalf("nil state markPrivate = %+v", got)
	}
}

func TestGithubRepoPrivate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/org/pub":
			_, _ = w.Write([]byte(`{"name":"pub","private":false,"visibility":"public"}`))
		case "/repos/org/old":
			_, _ = w.Write([]byte(`{"name":"old","private":false}`))
		case "/repos/org/priv":
			_, _ = w.Write([]byte(`{"name":"priv","private":true,"visibility":"private"}`))
		case "/repos/org/int":
			_, _ = w.Write([]byte(`{"name":"int","private":false,"visibility":"internal"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer server.Close()
	c := github.NewClientForTest(server.URL, "org", []string{"pub"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lookup := githubRepoPrivate(c)
	ctx := context.Background()
	for repo, want := range map[string]bool{"org/pub": false, "org/old": false, "org/priv": true, "org/int": true} {
		if got, err := lookup(ctx, repo); err != nil || got != want {
			t.Errorf("%s: private=%v err=%v, want %v", repo, got, err, want)
		}
	}
	for _, repo := range []string{"org/missing", "noslash", "/x"} {
		if got, err := lookup(ctx, repo); err == nil || !got {
			t.Errorf("%s: private=%v err=%v, want fail closed", repo, got, err)
		}
	}
	if got, err := githubRepoPrivate(nil)(ctx, "org/pub"); err == nil || !got {
		t.Error("nil client must fail closed")
	}
}

func TestRFCNotifySourceListsPRs(t *testing.T) {
	since := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	var paths []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/org/repo/pulls":
			if r.URL.Query().Get("page") == "2" {
				t.Error("paged past a PR updated before since")
			}
			w.Header().Set("Link", `<`+server.URL+`/repos/org/repo/pulls?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[
{"number":30,"title":"📝 proposal: cards","state":"open","html_url":"https://x/30","user":{"login":"hanthor"},"created_at":"2026-10-28T00:00:00Z","updated_at":"2026-10-29T00:00:00Z"},
{"number":31,"title":"✨ feat: cards doc","state":"open","html_url":"https://x/31","body":"Part of #1","head":{"sha":"sha31"},"created_at":"2026-10-28T00:00:00Z","updated_at":"2026-10-28T12:00:00Z"},
{"number":32,"title":"🐛 fix: typo","state":"closed","html_url":"https://x/32","merged_at":"2026-10-28T06:00:00Z","created_at":"2026-10-20T00:00:00Z","updated_at":"2026-10-28T06:00:00Z"},
{"number":33,"title":"✨ feat: old","state":"open","html_url":"https://x/33","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-26T00:00:00Z"}]`))
		case "/repos/org/repo/pulls/31/files":
			_, _ = w.Write([]byte(`[{"filename":"docs/rfc-cards.md","status":"added"}]`))
		case "/repos/org/repo/pulls/32/files":
			_, _ = w.Write([]byte(`[{"filename":"README.md","status":"modified"},{"filename":"docs/rfc-gone.md","status":"removed"}]`))
		case "/repos/org/repo/contents/docs/rfc-cards.md":
			if r.URL.Query().Get("ref") != "sha31" {
				t.Errorf("doc read at ref %q, want the PR head", r.URL.Query().Get("ref"))
			}
			_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("---\nx: y\n---\n# Discord RFC cards\n")) + `"}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := github.NewClientForTest(server.URL, "org", []string{"repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	src, ok := newRFCNotifySource(c, &rfcNotifyState{lookupPrivate: func(context.Context, string) (bool, error) { return false, nil }}).(rfcnotify.PRSource)
	if !ok {
		t.Fatal("the GitHub source must implement rfcnotify.PRSource")
	}
	prs, err := src.ListPRsUpdatedSince(context.Background(), since)
	if err != nil || len(prs) != 3 {
		t.Fatalf("ListPRsUpdatedSince = %+v %v", prs, err)
	}
	if p := prs[0]; !p.IsPR || p.Repo != "org/repo" || p.Author != "hanthor" || !p.RFC() || p.Files != nil || p.DocTitle != "" {
		t.Errorf("title-matched PR = %+v", p)
	}
	if p := prs[1]; !p.RFC() || p.DocTitle != "Discord RFC cards" || p.Body != "Part of #1" {
		t.Errorf("doc PR = %+v", p)
	}
	if p := prs[2]; p.RFC() || !p.Merged() || len(p.Files) != 1 {
		t.Errorf("unrelated PR = %+v", p)
	}
	if len(paths) == 0 || !strings.Contains(paths[0], "state=all") || !strings.Contains(paths[0], "sort=updated") || !strings.Contains(paths[0], "direction=desc") {
		t.Fatalf("pulls query = %v", paths)
	}
	for _, p := range paths {
		if strings.Contains(p, "/pulls/30/") || strings.Contains(p, "/pulls/33/") {
			t.Errorf("files listed for a title match or a PR older than since: %s", p)
		}
	}
}

func TestRFCNotifySourceCachesNotRFCPRsByHeadSHA(t *testing.T) {
	since := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	sha, fileCalls := "sha1", 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/org/repo/pulls":
			_, _ = w.Write([]byte(`[{"number":40,"title":"🐛 fix: typo","state":"open","head":{"sha":"` + sha + `"},"created_at":"2026-10-20T00:00:00Z","updated_at":"2026-10-28T00:00:00Z"}]`))
		case "/repos/org/repo/pulls/40/files":
			fileCalls++
			_, _ = w.Write([]byte(`[{"filename":"README.md","status":"modified"}]`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := github.NewClientForTest(server.URL, "org", []string{"repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	src := newRFCNotifySource(c, &rfcNotifyState{lookupPrivate: func(context.Context, string) (bool, error) { return false, nil }}).(rfcnotify.PRSource)
	poll := func(wantCalls int) {
		t.Helper()
		prs, err := src.ListPRsUpdatedSince(context.Background(), since)
		if err != nil || len(prs) != 1 || prs[0].RFC() {
			t.Fatalf("ListPRsUpdatedSince = %+v %v", prs, err)
		}
		if fileCalls != wantCalls {
			t.Fatalf("ListFiles calls = %d, want %d", fileCalls, wantCalls)
		}
	}
	poll(1)
	poll(1)
	sha = "sha2"
	poll(2)
	poll(2)
}

func TestRFCNotifyNotRFCPRCacheIsBounded(t *testing.T) {
	s := &rfcNotifyState{}
	for i := 0; i < rfcNotPRCacheMax*2; i++ {
		s.rememberNotRFCPR(fmt.Sprintf("org/repo#%d", i), "sha")
	}
	if len(s.notRFCPRs) > rfcNotPRCacheMax {
		t.Errorf("cache grew to %d entries, cap %d", len(s.notRFCPRs), rfcNotPRCacheMax)
	}
	var nilState *rfcNotifyState
	nilState.rememberNotRFCPR("k", "sha")
	if nilState.notRFCPR("k", "sha") {
		t.Error("nil state must not cache")
	}
}

type rfcNotifyPRFakeSource struct {
	rfcNotifyFakeSource
	prs     []rfcnotify.Issue
	prPolls int
}

func (f *rfcNotifyPRFakeSource) ListPRsUpdatedSince(context.Context, time.Time) ([]rfcnotify.Issue, error) {
	f.prPolls++
	return f.prs, nil
}

func TestRunRFCNotifySweepRFCPRs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	rfcIssue := rfcnotify.Issue{Repo: "o/r", Number: 1, Title: "RFC: a", URL: "u", State: "open", CreatedAt: time.Now().Add(-time.Hour)}
	pr := func(n int, title string, created time.Time, files ...string) rfcnotify.Issue {
		return rfcnotify.Issue{Repo: "o/r", Number: n, Title: title, URL: fmt.Sprintf("https://x/%d", n), State: "open", CreatedAt: created, Files: files, IsPR: true}
	}
	existing := pr(10, "📝 proposal: already open", time.Now().Add(-time.Hour))
	doc := pr(11, "✨ feat: doc", time.Now().Add(time.Hour), "docs/rfc-cards.md")
	proposal := pr(12, "📝 proposal: threads", time.Now().Add(time.Hour))
	proposal.Body = "Closes #1"
	unrelated := pr(13, "🐛 fix: typo", time.Now().Add(time.Hour), "README.md")

	// Line mode stays issues-only.
	var posted []string
	state := rfcNotifyRecorder(t, &posted)
	src := &rfcNotifyPRFakeSource{rfcNotifyFakeSource: rfcNotifyFakeSource{open: []rfcnotify.Issue{rfcIssue}}, prs: []rfcnotify.Issue{existing, doc, proposal, unrelated}}
	cfg := rfcNotifyConfig("tok", "1")
	runRFCNotifySweep(ctx, src, 1, cfg, nil, state, logger)
	forcePoll(state)
	runRFCNotifySweep(ctx, src, 1, cfg, nil, state, logger)
	if src.prPolls != 0 || len(posted) != 0 {
		t.Fatalf("line mode listed PRs: polls=%d posted=%v", src.prPolls, posted)
	}

	// Rich mode on the announcer: the docs/rfc-*.md and 📝 proposal: PRs
	// are announced once; the PR open at seed time and the unrelated PR
	// are not.
	state = rfcNotifyRecorder(t, &posted)
	src.prPolls = 0
	cfg.Notifications.Discord.RFCFormat, cfg.Notifications.Discord.RFCAnnouncer = "card", true
	runRFCNotifySweep(ctx, src, 1, cfg, nil, state, logger)
	forcePoll(state)
	runRFCNotifySweep(ctx, src, 1, cfg, nil, state, logger)
	want := []string{"RFC doc proposed: #11 ✨ feat: doc - <https://x/11>", "RFC doc proposed: #12 📝 proposal: threads - <https://x/12>"}
	if src.prPolls != 1 || strings.Join(posted, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rich mode: polls=%d posted=%q", src.prPolls, posted)
	}
	forcePoll(state)
	runRFCNotifySweep(ctx, src, 1, cfg, nil, state, logger)
	if len(posted) != 2 {
		t.Fatalf("re-announced: %q", posted)
	}

	// Merging the proposal posts "RFC doc accepted" for the linked RFC.
	proposal.State, proposal.MergedAt = "closed", time.Now()
	src.prs = []rfcnotify.Issue{proposal}
	forcePoll(state)
	res := runRFCNotifySweep(ctx, src, 1, cfg, nil, state, logger)
	if len(res.Events) != 1 || res.Events[0].Thread != "o/r#1" || posted[len(posted)-1] != "RFC doc accepted (RFC #1): #12 📝 proposal: threads - <https://x/12>" {
		t.Fatalf("merge: %+v posted=%q", res, posted)
	}
}

// rfcNotifyFakeThreads records what threads mode sends to Discord.
type rfcNotifyFakeThreads struct {
	posts, echoes []string
	threads       []string
	sends         []string
}

func (f *rfcNotifyFakeThreads) Post(content, nonce string) (string, error) {
	f.posts = append(f.posts, content+"|"+nonce)
	return "m1", nil
}

func (f *rfcNotifyFakeThreads) StartThread(messageID, name string) (string, error) {
	f.threads = append(f.threads, messageID+"|"+name)
	return "t1", nil
}

func (f *rfcNotifyFakeThreads) Send(threadID, content string) error {
	f.sends = append(f.sends, threadID+"|"+content)
	return nil
}

func (f *rfcNotifyFakeThreads) Echo(content string) error {
	f.echoes = append(f.echoes, content)
	return nil
}

func (f *rfcNotifyFakeThreads) Recent(int) ([]rfcnotify.Message, error) { return nil, nil }

func (f *rfcNotifyFakeThreads) ThreadURL(id string) (string, error) { return "https://d/" + id, nil }

func TestRunRFCNotifySweepThreadsMode(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var lines []string
	state := rfcNotifyRecorder(t, &lines)
	fake := &rfcNotifyFakeThreads{}
	var builtFor []string
	state.newThreads = func(token, channel string) rfcnotify.Threads {
		builtFor = append(builtFor, token+"@"+channel)
		return fake
	}
	src := &rfcNotifyFakeSource{open: []rfcnotify.Issue{{Repo: "o/r", Number: 1, Title: "RFC: a", URL: "u", State: "open", CreatedAt: time.Now().Add(-time.Hour)}}}
	cfg := rfcNotifyConfig("tok", "42")
	cfg.Notifications.Discord.RFCThreads = true
	cfg.Notifications.Discord.RFCAnnouncer = true
	runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)

	fresh := rfcnotify.Issue{Repo: "o/r", Number: 2, Title: "RFC: b", URL: "v", State: "open", CreatedAt: time.Now().Add(time.Hour)}
	src.updated = []rfcnotify.Issue{fresh}
	forcePoll(state)
	res := runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	if res.Threads != 1 || len(fake.posts) != 1 || len(fake.threads) != 1 || !strings.HasPrefix(fake.threads[0], "m1|#2 RFC: b") {
		t.Fatalf("threads mode: %+v posts=%v threads=%v", res, fake.posts, fake.threads)
	}
	if len(lines) != 0 {
		t.Fatalf("threads mode must not use the line poster: %v", lines)
	}
	if len(builtFor) != 1 || builtFor[0] != "tok@42" {
		t.Fatalf("threads built for %v", builtFor)
	}
	// A channel change rebuilds the Discord surface for the new channel.
	cfg.Notifications.Discord.RFCChannelID = "43"
	forcePoll(state)
	src.updated = nil
	runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	if len(builtFor) != 2 || builtFor[1] != "tok@43" {
		t.Fatalf("threads not rebuilt for the new channel: %v", builtFor)
	}
}

func TestRunRFCNotifySweepThreadsNeedAnnouncer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var lines []string
	state := rfcNotifyRecorder(t, &lines)
	fake := &rfcNotifyFakeThreads{}
	state.newThreads = func(string, string) rfcnotify.Threads { return fake }
	src := &rfcNotifyFakeSource{open: []rfcnotify.Issue{{Repo: "o/r", Number: 1, Title: "RFC: a", URL: "u", State: "open", CreatedAt: time.Now().Add(-time.Hour)}}}
	cfg := rfcNotifyConfig("tok", "42")
	cfg.Notifications.Discord.RFCThreads = true
	runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	src.updated = []rfcnotify.Issue{{Repo: "o/r", Number: 2, Title: "RFC: b", URL: "v", State: "open", CreatedAt: time.Now().Add(time.Hour)}}
	forcePoll(state)
	res := runRFCNotifySweep(context.Background(), src, 1, cfg, nil, state, logger)
	if res.Withheld != 1 || len(fake.posts)+len(fake.echoes)+len(fake.threads) != 0 || len(lines) != 0 {
		t.Fatalf("a hive that is not the announcer must not call Discord: %+v %+v", res, fake)
	}
}

func TestRFCNotifySourceCommentWithoutClient(t *testing.T) {
	var c rfcnotify.Commenter = rfcNotifySource{}
	if err := c.Comment(context.Background(), "o/r", 1, "Discussion thread: x"); !errors.Is(err, github.ErrNoGitHubClient) {
		t.Fatalf("err = %v, want ErrNoGitHubClient", err)
	}
}
