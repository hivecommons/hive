package main

import (
	"context"
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
	if newRFCNotifySource(nil) != nil {
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
	src := newRFCNotifySource(c)
	open, err := src.ListOpen(context.Background())
	if err != nil || len(open) != 1 || open[0].Repo != "org/repo" || open[0].Number != 7 || open[0].Author != "ann" || open[0].StateReason != "completed" || len(open[0].Labels) != 1 {
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
