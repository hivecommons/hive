package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/releasefeed"
	"github.com/hivecommons/hive/pkg/scheduler"
)

type releaseFeedSent struct{ agent, message string }

func newReleaseFeedKickerForTest(t *testing.T, cfg *config.Config) (*releaseFeedKicker, *governor.Governor, *[]releaseFeedSent) {
	t.Helper()
	var sent []releaseFeedSent
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	k := &releaseFeedKicker{
		cfg: func() *config.Config { return cfg },
		gov: func() *governor.Governor { return gov },
		enqueue: func(agent, message string) error {
			sent = append(sent, releaseFeedSent{agent: agent, message: message})
			return nil
		},
	}
	return k, gov, &sent
}

func releaseFeedTestReleases() []releasefeed.Release {
	return []releasefeed.Release{{Source: "pypi:requests", Package: "requests", Old: "2.32.0", New: "2.33.0", URL: "https://pypi.org/project/requests/2.33.0/"}}
}

// A release-feed kick goes through the gates the webhook CI-failure kick
// uses, carries the release block, and leaves its reason in kick history.
func TestReleaseFeedKicker_GatesKicksAndRecordsReason(t *testing.T) {
	cfg := ciKickTestConfig()
	k, gov, sent := newReleaseFeedKickerForTest(t, cfg)
	rels := releaseFeedTestReleases()
	ctx := context.Background()

	if err := k.Kick(ctx, "quality", rels); err != nil {
		t.Fatalf("kick: %v", err)
	}
	if len(*sent) != 1 || (*sent)[0].agent != "quality" {
		t.Fatalf("sent = %+v", *sent)
	}
	msg := (*sent)[0].message
	if !strings.HasPrefix(msg, releaseFeedKickHeader) || !strings.Contains(msg, "- requests: 2.32.0 -> 2.33.0") {
		t.Fatalf("targeted kick = %q", msg)
	}
	hist := gov.KickHistory()
	if len(hist) != 1 || hist[0].Reason != "new release: requests 2.32.0 -> 2.33.0" {
		t.Fatalf("kick history = %+v", hist)
	}

	// The agent's own template places ${NEW_RELEASES}: its full prompt is sent.
	k.render = func(agent string) string {
		return "full prompt for " + agent + "\n" + releasefeed.FormatBlock(rels)
	}
	if err := k.Kick(ctx, "quality", rels); err != nil {
		t.Fatal(err)
	}
	if got := (*sent)[1].message; !strings.HasPrefix(got, "full prompt for quality") || strings.Contains(got, releaseFeedKickHeader) {
		t.Fatalf("templated kick = %q", got)
	}
	// A template without the variable still gets the targeted kick.
	k.render = func(string) string { return "full prompt" }
	if err := k.Kick(ctx, "quality", rels); err != nil {
		t.Fatal(err)
	}
	if got := (*sent)[2].message; !strings.HasPrefix(got, releaseFeedKickHeader) {
		t.Fatalf("fallback kick = %q", got)
	}

	gated := func(name string) {
		t.Helper()
		err := k.Kick(ctx, "quality", rels)
		if !errors.Is(err, releasefeed.ErrGated) || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	ac := cfg.Agents["quality"]
	ac.Paused = true
	cfg.Agents["quality"] = ac
	gated("agent paused")
	ac.Paused = false
	cfg.Agents["quality"] = ac

	k.isPaused = func(string) bool { return true }
	gated("agent paused")
	k.isPaused = nil

	if err := k.Kick(ctx, "ghost", rels); !errors.Is(err, releasefeed.ErrGated) {
		t.Fatalf("unknown agent: %v", err)
	}

	onDemand := governor.New(cfg.Governor, map[string]config.AgentConfig{"quality": {Enabled: true, OnDemand: true}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	k.gov = func() *governor.Governor { return onDemand }
	gated("governor gated")
	k.gov = func() *governor.Governor { return gov }

	k.cfg = func() *config.Config { return nil }
	gated("no config")
	k.cfg = func() *config.Config { return cfg }

	k.gov = nil
	k.enqueue = nil
	gated("no kick sender")

	k.enqueue = func(string, string) error { return errNoAgentRuntime }
	if err := k.Kick(ctx, "quality", rels); !errors.Is(err, errNoAgentRuntime) {
		t.Fatalf("delivery failure: %v", err)
	}
	k.cfg = nil
	if got := k.gate("quality"); got != "no config" {
		t.Fatalf("nil cfg func gate = %q", got)
	}
}

func TestReleaseFeedSubscriptions(t *testing.T) {
	if got := releaseFeedSubscriptions(nil); len(got) != 0 {
		t.Fatalf("nil config: %v", got)
	}
	if got := releaseFeedSubscriptions(&config.Config{}); len(got) != 0 {
		t.Fatalf("no block: %v", got)
	}
	cfg := &config.Config{Governor: config.GovernorConfig{ReleaseFeeds: &config.ReleaseFeedsConfig{
		Agents: map[string]config.ReleaseFeedAgentConfig{
			"deps": {Sources: []config.ReleaseFeedSource{{Kind: "pypi", Package: "requests", AuthVar: "TOK", Name: "Requests"}}},
			"idle": {},
		},
	}}}
	got := releaseFeedSubscriptions(cfg)
	if len(got) != 1 || len(got["deps"]) != 1 {
		t.Fatalf("subscriptions = %+v", got)
	}
	want := releasefeed.Source{Kind: "pypi", Package: "requests", AuthVar: "TOK", Name: "Requests"}
	if got["deps"][0] != want {
		t.Fatalf("source = %+v", got["deps"][0])
	}
}

// The auth token comes from the agent's effective variables; an unset or
// invalid name yields "".
func TestReleaseFeedToken(t *testing.T) {
	cfg := ciKickTestConfig()
	ac := cfg.Agents["quality"]
	ac.Variables = map[string]config.VarDef{"INDEX_TOKEN": {Type: "static", Value: "s3cret"}}
	cfg.Agents["quality"] = ac
	if got := releaseFeedToken(cfg, "quality", "INDEX_TOKEN"); got != "s3cret" {
		t.Fatalf("per-agent token = %q", got)
	}
	if got := releaseFeedToken(cfg, "scanner", "INDEX_TOKEN"); got != "" {
		t.Fatalf("another agent's variable leaked: %q", got)
	}
	if got := releaseFeedToken(cfg, "quality", "bad-name"); got != "" {
		t.Fatalf("invalid name = %q", got)
	}
	if got := releaseFeedToken(nil, "quality", "INDEX_TOKEN"); got != "" {
		t.Fatalf("nil config = %q", got)
	}
}

// startReleaseFeedWatcher builds the watcher once and installs it; without a
// context or config it does nothing.
func TestStartReleaseFeedWatcher(t *testing.T) {
	old := releaseFeedStatePath
	releaseFeedStatePath = filepath.Join(t.TempDir(), "release-feeds.json")
	t.Cleanup(func() { releaseFeedStatePath = old })

	(&boot{}).startReleaseFeedWatcher()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := ciKickTestConfig()
	b := &boot{ctx: ctx, cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	b.startReleaseFeedWatcher()
	first := b.releaseFeed
	if first == nil {
		t.Fatal("watcher not started")
	}
	b.startReleaseFeedWatcher()
	if b.releaseFeed != first {
		t.Fatal("watcher started twice")
	}

	if first.Block("quality") != "" {
		t.Fatal("fresh watcher has pending releases")
	}
}

// The boot kicker resolves the scheduler and runtime per call, so it works
// before boot finishes wiring them and picks them up once it has.
func TestNewReleaseFeedKicker_ResolvesBootState(t *testing.T) {
	cfg := ciKickTestConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := &boot{cfg: cfg, logger: logger}
	k := b.newReleaseFeedKicker()
	if k.config() != nil {
		t.Fatal("kicker must read the published snapshot, not b.cfg")
	}
	b.sweepConfig.Store(cfg)
	if k.config() != cfg || k.governor() != nil {
		t.Fatal("kicker not bound to the published config/governor")
	}
	if k.isPaused("quality") {
		t.Fatal("no runtime must read as not paused")
	}
	if got := k.render("quality"); got != "" {
		t.Fatalf("no scheduler must render empty, got %q", got)
	}
	if err := k.enqueue("quality", "m"); !errors.Is(err, errNoAgentRuntime) {
		t.Fatalf("no runtime: %v", err)
	}

	b.sched = scheduler.New(cfg, logger)
	b.agentMgr = cruxtest.NewRuntime(cfg.Agents, logger, crux.ProjectContext{})
	_ = k.isPaused("quality")
	// Delivery is not exercised here: the queue would start a real pump for
	// the fixture agent. The scheduler render is pure.
	_ = k.render("quality")
}
