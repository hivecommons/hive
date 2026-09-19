package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/discord"
)

// launchHarness is a bootLaunchDeps whose spawn runs synchronously and whose
// effects are recorded, so the whole phase — including the launch loop — can
// be asserted in one test without goroutines or timers.
type launchHarness struct {
	mu        sync.Mutex
	spawned   []string
	started   []string
	waits     int
	serveErr  error
	startErr  map[string]error
	discord   *discord.Config
	discordAg []string
	discordEr error
	onDemand  map[string]bool
}

func (h *launchHarness) deps() bootLaunchDeps {
	return bootLaunchDeps{
		spawn: func(name string, fn func()) {
			h.mu.Lock()
			h.spawned = append(h.spawned, name)
			h.mu.Unlock()
			fn()
		},
		startDiscordBot: func(_ context.Context, cfg discord.Config, names []string, _ *slog.Logger) error {
			h.discord = &cfg
			h.discordAg = names
			return h.discordEr
		},
		onDemandFromPack: func() map[string]bool { return h.onDemand },
		waitStagger: func(ctx context.Context) bool {
			h.waits++
			return ctx.Err() == nil
		},
		startAgent: func(_ context.Context, _ *agent.Manager, name string) error {
			h.started = append(h.started, name)
			return h.startErr[name]
		},
		serve: func(*dashboard.Server) error { return h.serveErr },
	}
}

func newLaunchBoot(t *testing.T, agents map[string]config.AgentConfig) *boot {
	t.Helper()
	cfg := &config.Config{Agents: agents}
	cfg.Dashboard.Port = 8080
	b, _ := newDepsTestBoot(t, cfg)
	b.agentMgr = agent.NewManager(cfg.Agents, b.logger, agent.ProjectContext{})
	b.dashSrv = dashboard.NewServer(0, b.logger)
	return b
}

func healthStatus(t *testing.T, srv *dashboard.Server) int {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	return rec.Code
}

func auditActions(srv *dashboard.Server) map[string][]dashboard.AuditEntry {
	out := map[string][]dashboard.AuditEntry{}
	for _, e := range srv.GetAudit().Recent(50) {
		out[e.Action] = append(out[e.Action], e)
	}
	return out
}

func TestBootLaunchWithMarksReadyAndLaunchesPersistentAgents(t *testing.T) {
	b := newLaunchBoot(t, map[string]config.AgentConfig{
		"alpha":   {Enabled: true},
		"beta":    {Enabled: true, Paused: true},
		"lazy":    {Enabled: true, OnDemand: true},
		"packed":  {Enabled: true},
		"offline": {Enabled: false},
	})
	if healthStatus(t, b.dashSrv) != http.StatusServiceUnavailable {
		t.Fatal("dashboard should not be ready before bootLaunch")
	}
	h := &launchHarness{onDemand: map[string]bool{"packed": true}}
	b.bootLaunchWith(h.deps())

	if got := healthStatus(t, b.dashSrv); got != http.StatusOK {
		t.Fatalf("health after bootLaunch = %d, want 200", got)
	}
	if strings.Join(h.spawned, ",") != "dashboard-serve,agent-launch" {
		t.Fatalf("spawned = %v", h.spawned)
	}
	sort.Strings(h.started)
	if strings.Join(h.started, ",") != "alpha,beta" {
		t.Fatalf("started = %v, want alpha,beta (on-demand skipped)", h.started)
	}
	if h.waits != 1 {
		t.Fatalf("stagger waits = %d, want 1 (only between launches)", h.waits)
	}
	if h.discord != nil {
		t.Fatal("discord bot must not start without a token+channel")
	}
	if !b.onDemandFromPack["packed"] {
		t.Fatal("onDemandFromPack should be retained on boot")
	}

	acts := auditActions(b.dashSrv)
	if len(acts["hive_restart"]) != 1 || !strings.Contains(acts["hive_restart"][0].Detail, "build="+gitShort) {
		t.Fatalf("hive_restart audit = %+v", acts["hive_restart"])
	}
	detail := map[string]string{}
	for _, e := range acts["agent_start"] {
		detail[e.Agent] = e.Detail
	}
	if detail["alpha"] != "trigger=startup" {
		t.Fatalf("alpha detail = %q", detail["alpha"])
	}
	if !strings.Contains(detail["beta"], "restored paused") {
		t.Fatalf("beta detail = %q, want restored-paused marker", detail["beta"])
	}
}

func TestBootLaunchWithLogsServeAndStartFailures(t *testing.T) {
	b := newLaunchBoot(t, map[string]config.AgentConfig{"alpha": {Enabled: true}})
	var sb strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&sb, nil))
	h := &launchHarness{
		serveErr: errors.New("listen tcp: address in use"),
		startErr: map[string]error{"alpha": errors.New("no binary")},
	}
	b.bootLaunchWith(h.deps())

	out := sb.String()
	if !strings.Contains(out, "dashboard server failed") || !strings.Contains(out, "address in use") {
		t.Fatalf("missing serve failure log:\n%s", out)
	}
	if !strings.Contains(out, "failed to start agent") || !strings.Contains(out, "no binary") {
		t.Fatalf("missing start failure log:\n%s", out)
	}
	if got := auditActions(b.dashSrv)["agent_start"]; len(got) != 0 {
		t.Fatalf("failed start must not audit agent_start, got %+v", got)
	}
	if healthStatus(t, b.dashSrv) != http.StatusOK {
		t.Fatal("readiness must not depend on agent launch success")
	}
}

func TestBootLaunchWithStartsDiscordBotWhenConfigured(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantLog string
	}{
		{"ok", nil, "discord bot started"},
		{"fail", errors.New("bad token"), "discord bot failed to start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newLaunchBoot(t, map[string]config.AgentConfig{"alpha": {Enabled: true}})
			var sb strings.Builder
			b.logger = slog.New(slog.NewTextHandler(&sb, nil))
			b.cfg.Notifications.Discord = &config.DiscordConfig{BotToken: "tok", ChannelID: "chan", AllowedUsers: []string{"u1"}}
			t.Setenv("HIVE_DASHBOARD_TOKEN", "dash-secret")
			h := &launchHarness{discordEr: tc.err}
			b.bootLaunchWith(h.deps())

			if h.discord == nil {
				t.Fatal("discord bot not started")
			}
			if h.discord.Token != "tok" || h.discord.ChannelID != "chan" || h.discord.DashboardURL != "http://localhost:8080" ||
				h.discord.DashboardToken != "dash-secret" || len(h.discord.AllowedUsers) != 1 {
				t.Fatalf("discord config = %+v", *h.discord)
			}
			if len(h.discordAg) != 1 || h.discordAg[0] != "alpha" {
				t.Fatalf("agent names = %v", h.discordAg)
			}
			if !strings.Contains(sb.String(), tc.wantLog) {
				t.Fatalf("missing %q in:\n%s", tc.wantLog, sb.String())
			}
		})
	}
}

func TestBootLaunchWithAbortsLaunchOnShutdown(t *testing.T) {
	b := newLaunchBoot(t, map[string]config.AgentConfig{
		"a1": {Enabled: true}, "a2": {Enabled: true}, "a3": {Enabled: true},
	})
	var sb strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&sb, nil))
	ctx, cancel := context.WithCancel(context.Background())
	b.ctx = ctx
	// Cancel while starting the first agent (whichever map order yields): the
	// stagger before the second observes ctx.Done and the loop aborts.
	h := &launchHarness{}
	deps := h.deps()
	orig := deps.startAgent
	deps.startAgent = func(ctx context.Context, mgr *agent.Manager, name string) error {
		cancel()
		return orig(ctx, mgr, name)
	}
	b.bootLaunchWith(deps)

	if len(h.started) != 1 {
		t.Fatalf("started = %v, want exactly one before abort", h.started)
	}
	if !strings.Contains(sb.String(), "aborting staggered agent launch") {
		t.Fatalf("missing abort log:\n%s", sb.String())
	}
}

func TestDefaultBootLaunchDepsWaitStaggerHonorsContext(t *testing.T) {
	deps := defaultBootLaunchDeps()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if deps.waitStagger(ctx) {
		t.Fatal("waitStagger must return false once ctx is done")
	}
	if deps.onDemandFromPack() == nil {
		t.Fatal("onDemandFromPack should return a (possibly empty) map")
	}
}
