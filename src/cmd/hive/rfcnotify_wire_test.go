package main

import (
	"context"
	"errors"
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
