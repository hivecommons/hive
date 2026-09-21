package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// bootHeartbeatFake records what bootHeartbeatWith asked for. The collector
// and the hub→spoke callbacks are captured, not run.
type bootHeartbeatFake struct {
	deps bootHeartbeatDeps
	log  bytes.Buffer

	identity        []any
	heartbeatURL    string
	interval        time.Duration
	collect         spoke.StatusCollector
	callbacks       []any
	taskURL         string
	taskCollect     spoke.TaskStatusCollector
	heartbeatStarts int
}

func newBootHeartbeatFake() *bootHeartbeatFake {
	f := &bootHeartbeatFake{}
	f.deps = bootHeartbeatDeps{
		publishIdentity: func(hiveID, org, primaryRepo string, repos []string, reporter, startedAt, gitHash string) {
			f.identity = []any{hiveID, org, primaryRepo, repos, reporter, startedAt, gitHash}
		},
		startHeartbeat: func(_ context.Context, hubURL string, collect spoke.StatusCollector, interval time.Duration, _ *slog.Logger, callbacks ...any) {
			f.heartbeatStarts++
			f.heartbeatURL, f.collect, f.interval, f.callbacks = hubURL, collect, interval, callbacks
		},
		startTaskStatusPush: func(_ context.Context, hubURL string, collect spoke.TaskStatusCollector, _ *slog.Logger) {
			f.taskURL, f.taskCollect = hubURL, collect
		},
	}
	return f
}

// callback returns the single callback of type T the phase registered.
func heartbeatCallback[T any](t *testing.T, f *bootHeartbeatFake) T {
	t.Helper()
	var found []T
	for _, cb := range f.callbacks {
		if v, ok := cb.(T); ok {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		var zero T
		t.Fatalf("registered %d callbacks of type %T, want exactly 1", len(found), zero)
	}
	return found[0]
}

func bootHeartbeatConfig() *config.Config {
	cfg := &config.Config{}
	cfg.HiveID = "boot-heartbeat-test"
	cfg.Project.Org = "acme"
	cfg.Project.PrimaryRepo = "widgets"
	cfg.Project.Repos = []string{"widgets", "gadgets"}
	cfg.Hub.Enabled = true
	cfg.Hub.URL = "https://hub.example.test"
	cfg.Agents = map[string]config.AgentConfig{}
	return cfg
}

func newBootHeartbeatBoot(t *testing.T, f *bootHeartbeatFake, cfg *config.Config) *boot {
	t.Helper()
	t.Setenv("HIVE_HUB_URL", "")
	t.Setenv("HIVE_CLUSTER_ID", "")
	logger := slog.New(slog.NewTextHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := &boot{
		ctx:                     ctx,
		cfg:                     cfg,
		logger:                  logger,
		startTime:               time.Now(),
		reporterName:            "test-host/1",
		processStartedAt:        time.Now(),
		repoTargetMisconfigured: func() bool { return false },
		repoTargetIssueMessage:  func() string { return "" },
		gov:                     governor.New(cfg.Governor, cfg.Agents, logger),
		agentMgr:                agent.NewManager(cfg.Agents, logger, agent.ProjectContext{}),
		dashSrv:                 dashboard.NewServer(0, logger),
		onDemandFromPack:        map[string]bool{},
	}
	b.wireBootClosures()
	return b
}

func TestBootHeartbeatWith_NoHubStartsNothing(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"disabled": func(c *config.Config) { c.Hub.Enabled = false },
		"no url":   func(c *config.Config) { c.Hub.URL = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newBootHeartbeatFake()
			cfg := bootHeartbeatConfig()
			mutate(cfg)
			b := newBootHeartbeatBoot(t, f, cfg)

			b.bootHeartbeatWith(f.deps)

			if f.identity != nil || f.heartbeatStarts != 0 || f.taskCollect != nil {
				t.Fatal("a hive with no hub to beat to must not publish or start push loops")
			}
		})
	}
}

func TestBootHeartbeatWith_EnvHubTargetOverridesConfigAndIsWrittenBack(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	cfg.Hub.Enabled = false
	b := newBootHeartbeatBoot(t, f, cfg)
	t.Setenv("HIVE_HUB_URL", "https://env-hub.example.test")
	t.Setenv("HIVE_CLUSTER_ID", "cluster-42")

	b.bootHeartbeatWith(f.deps)

	if !cfg.Hub.Enabled || cfg.Hub.URL != "https://env-hub.example.test" || cfg.Hub.ClusterID != "cluster-42" {
		t.Fatalf("resolved hub target not written back to cfg.Hub: %+v", cfg.Hub)
	}
	if f.heartbeatURL != "https://env-hub.example.test" || f.taskURL != f.heartbeatURL {
		t.Fatalf("push loops aimed at %q / %q, want the env hub", f.heartbeatURL, f.taskURL)
	}
}

func TestBootHeartbeatWith_PublishesIdentityBeforeTheLoopAndWiresEveryCallback(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	b := newBootHeartbeatBoot(t, f, cfg)

	b.bootHeartbeatWith(f.deps)

	if f.identity == nil {
		t.Fatal("identity not published — a spoke whose first collect times out would read OFFLINE")
	}
	if f.identity[0] != cfg.HiveID || f.identity[1] != cfg.Project.Org || f.identity[2] != cfg.Project.PrimaryRepo {
		t.Fatalf("identity = %v, want hive/org/primary repo from cfg", f.identity)
	}
	if f.heartbeatStarts != 1 || f.interval != heartbeatSendInterval || f.collect == nil {
		t.Fatalf("heartbeat starts=%d interval=%v collect=%v; want one loop at the fixed cadence", f.heartbeatStarts, f.interval, f.collect != nil)
	}
	if f.interval >= 5*time.Minute {
		t.Fatalf("interval %v is not under the hub's 5-minute staleness threshold", f.interval)
	}
	// One of each hub→spoke callback; a missing one is a silently-ignored
	// instruction on every heartbeat-only spoke.
	heartbeatCallback[spoke.RestartSpokeCallback](t, f)
	heartbeatCallback[spoke.UpgradeCallback](t, f)
	heartbeatCallback[spoke.GitHubAppConfigCallback](t, f)
	heartbeatCallback[spoke.HubBannerCallback](t, f)
	heartbeatCallback[spoke.UpgradePolicyCallback](t, f)
	heartbeatCallback[spoke.VisibilityCallback](t, f)
	heartbeatCallback[spoke.SwitchBranchCallback](t, f)
	heartbeatCallback[spoke.AgentRestartResetCallback](t, f)
	heartbeatCallback[spoke.AuthorizedUsersCallback](t, f)
	heartbeatCallback[spoke.ProjectConfigCallback](t, f)
	heartbeatCallback[spoke.GatewayConfigCallback](t, f)
	heartbeatCallback[spoke.FreshStatusCollector](t, f)
	if len(f.callbacks) != 12 {
		t.Fatalf("registered %d callbacks, want 12 (one of each)", len(f.callbacks))
	}

	// The collector honours a runtime hub.enabled=false without being torn
	// down: it returns nil so StartHeartbeat skips the beat.
	cfg.Hub.Enabled = false
	if p := f.collect(); p != nil {
		t.Fatalf("collect() with hub disabled = %+v, want nil", p)
	}
}

func TestBootHeartbeatWith_CallbacksReconcileRunningConfig(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	cfg.Dashboard.AuthorizedUsers = []string{"alice"}
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	heartbeatCallback[spoke.VisibilityCallback](t, f)(true)
	if !cfg.Hub.IsPublic || !strings.Contains(f.log.String(), "hub overrode visibility") {
		t.Fatal("visibility override not applied/logged")
	}
	f.log.Reset()
	heartbeatCallback[spoke.VisibilityCallback](t, f)(true)
	if strings.Contains(f.log.String(), "hub overrode visibility") {
		t.Fatal("unchanged visibility logged as an override")
	}

	users := heartbeatCallback[spoke.AuthorizedUsersCallback](t, f)
	users([]string{"alice", "bob"}, map[string]string{"bob": "Bob"})
	if !sameStringSlice(cfg.Dashboard.AuthorizedUsers, []string{"alice", "bob"}) || cfg.Dashboard.AuthorizedUserNames["bob"] != "Bob" {
		t.Fatalf("authorized users not reconciled: %v %v", cfg.Dashboard.AuthorizedUsers, cfg.Dashboard.AuthorizedUserNames)
	}
	f.log.Reset()
	users([]string{"alice", "bob"}, nil)
	if strings.Contains(f.log.String(), "authorized users updated") {
		t.Fatal("unchanged allowlist logged as an update")
	}
	if cfg.Dashboard.AuthorizedUserNames != nil {
		t.Fatal("cosmetic names must follow the hub even to nil")
	}

	// Banner set/clear and upgrade policy are pass-throughs to the dashboard;
	// they must accept the hub's nil ("clear") spelling.
	banner := heartbeatCallback[spoke.HubBannerCallback](t, f)
	banner(&spoke.HubBanner{ID: "b1", Message: "maintenance", Color: "amber"})
	banner(nil)
	heartbeatCallback[spoke.UpgradePolicyCallback](t, f)(&spoke.HeartbeatUpgradePolicy{HubManaged: true})
	heartbeatCallback[spoke.UpgradePolicyCallback](t, f)(nil)

	// Unknown agent: warn, never panic — the hub may name an agent this
	// spoke's pack no longer has.
	f.log.Reset()
	heartbeatCallback[spoke.AgentRestartResetCallback](t, f)("ghost")
	if !strings.Contains(f.log.String(), "agent restart reset from hub failed") {
		t.Fatalf("missing-agent reset not reported:\n%s", f.log.String())
	}

	// A gateway delivery without a key is a no-op, not an error.
	f.log.Reset()
	gw := heartbeatCallback[spoke.GatewayConfigCallback](t, f)
	gw(nil)
	gw(&spoke.HeartbeatGatewayConfig{Name: "openrouter"})
	if strings.Contains(f.log.String(), "failed to apply hub-delivered gateway") {
		t.Fatal("keyless gateway delivery treated as an apply")
	}
}

func TestBootHeartbeatWith_TaskStatusPayloadCarriesHiveID(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	b := newBootHeartbeatBoot(t, f, cfg)
	b.bootHeartbeatWith(f.deps)

	if f.taskCollect == nil {
		t.Fatal("task-status push not started")
	}
	p := f.taskCollect()
	if p == nil || p.HiveID != cfg.HiveID {
		t.Fatalf("task-status payload = %+v, want HiveID %q", p, cfg.HiveID)
	}
	if p.Leaderboard == nil {
		t.Fatal("Leaderboard must be an empty slice, not nil, so the hub sees [] rather than null")
	}
}
