package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/watchdog"
)

// These tests drive individual boot phases in isolation (#7571, step 1). They
// exist to show the split made each phase reachable from a test at all —
// main() was 4,142 lines at 0% because nothing could call into the middle of
// it. Step 2 gives the heavier phases a deps struct so the collaborators
// that touch the network, tmux, or /data can be faked; the phases below
// already run against real collaborators cheaply enough to assert on.

func TestDeferStack_RunsLIFOAndIgnoresNil(t *testing.T) {
	var d deferStack
	var order []string
	d.push(func() { order = append(order, "first") })
	d.push(nil)
	d.push(func() { order = append(order, "second") })
	d.push(func() { order = append(order, "third") })

	d.run()

	want := []string{"third", "second", "first"}
	if len(order) != len(want) {
		t.Fatalf("ran %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("ran %v, want %v (defer order is LIFO)", order, want)
		}
	}

	// An empty stack is a no-op, so main() can always defer run() up front.
	var empty deferStack
	empty.run()
}

// bootConfig's --version fast path must return without touching flags,
// config, or the singleton lock: the CI smoke test probes the binary this way
// on a host that has none of them.
func TestBootConfig_VersionFastPathSkipsBoot(t *testing.T) {
	for _, arg := range []string{"--version", "version"} {
		t.Run(arg, func(t *testing.T) {
			saved := os.Args
			os.Args = []string{"hive", arg}
			t.Cleanup(func() { os.Args = saved })

			b := &boot{}
			if b.bootConfig() {
				t.Fatalf("bootConfig(%q) = true, want false (main must return without booting)", arg)
			}
			if b.cfg != nil || b.ctx != nil || b.logger != nil {
				t.Fatalf("bootConfig(%q) populated boot state on the fast path: cfg=%v ctx=%v logger=%v", arg, b.cfg, b.ctx, b.logger)
			}
			if n := len(b.cleanup.fns); n != 0 {
				t.Fatalf("bootConfig(%q) registered %d cleanups on the fast path, want 0", arg, n)
			}
		})
	}
}

func TestBootStores_OpensEnabledStoresAndCountsFailures(t *testing.T) {
	dir := t.TempDir()
	// A regular file where a directory is needed makes MkdirAll fail, which is
	// the "store fails to open" path the failure counter exists for.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.HiveID = "boot-stores-test"
	cfg.Agents = map[string]config.AgentConfig{
		"scanner": {Enabled: true, BeadsDir: filepath.Join(dir, "scanner")},
		"ghost":   {Enabled: false, BeadsDir: filepath.Join(dir, "ghost")},
		"broken":  {Enabled: true, BeadsDir: filepath.Join(blocker, "beads")},
	}

	b := &boot{cfg: cfg, logger: restoreTestLogger()}
	b.bootStores()

	if b.beadStores["scanner"] == nil {
		t.Fatalf("enabled agent's store not opened; got %v", b.beadStores)
	}
	if _, ok := b.beadStores["ghost"]; ok {
		t.Fatalf("disabled agent's store was opened; got %v", b.beadStores)
	}
	if _, ok := b.beadStores["broken"]; ok {
		t.Fatalf("unopenable store was kept in the ledger; got %v", b.beadStores)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghost")); !os.IsNotExist(err) {
		t.Fatalf("disabled agent's beads dir was created (err=%v)", err)
	}
	// The orphan scan reads the fixed /data/beads path; on a host that has one
	// (a hive's own checkout) it may add stores and failures of its own, so the
	// exact count is only asserted where that scan cannot contribute.
	if _, err := os.Stat("/data/beads"); os.IsNotExist(err) {
		if b.beadStoreLoadFailures != 1 {
			t.Fatalf("beadStoreLoadFailures = %d, want 1 (the unopenable store)", b.beadStoreLoadFailures)
		}
	} else if b.beadStoreLoadFailures < 1 {
		t.Fatalf("beadStoreLoadFailures = %d, want >= 1 (the unopenable store)", b.beadStoreLoadFailures)
	}
}

func TestBootGovernor_WiresSchedulerAndPrimer(t *testing.T) {
	for _, knowledge := range []bool{false, true} {
		t.Run(map[bool]string{false: "knowledge-off", true: "knowledge-on"}[knowledge], func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Agents = map[string]config.AgentConfig{}
			cfg.Knowledge.Enabled = knowledge

			b := &boot{cfg: cfg, logger: restoreTestLogger()}
			b.bootGovernor()

			if b.gov == nil || b.sched == nil || b.definitionResolver == nil {
				t.Fatalf("bootGovernor left a collaborator nil: gov=%v sched=%v resolver=%v", b.gov, b.sched, b.definitionResolver)
			}
			if got := b.sched.GetPrimer() != nil; got != knowledge {
				t.Fatalf("scheduler primer set = %v, want %v (knowledge.enabled=%v)", got, knowledge, knowledge)
			}
		})
	}
}

func TestBootSupervision_WatchdogFollowsConfiguredMode(t *testing.T) {
	newBoot := func(t *testing.T, cfg *config.Config) *boot {
		t.Helper()
		logger := restoreTestLogger()
		cfg.Agents = map[string]config.AgentConfig{}
		return &boot{
			ctx:      context.Background(),
			cfg:      cfg,
			logger:   logger,
			gov:      governor.New(cfg.Governor, cfg.Agents, logger),
			sched:    scheduler.New(cfg, logger),
			agentMgr: agent.NewManager(cfg.Agents, logger, agent.ProjectContext{}),
			dashSrv:  dashboard.NewServer(0, logger),
		}
	}

	t.Run("mode off leaves the watchdog nil", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Governor.Watchdog.Mode = "off"
		b := newBoot(t, cfg)
		b.bootSupervision()
		if b.wd != nil {
			t.Fatalf("watchdog built with mode off: %v", b.wd)
		}
		if b.rotationMgr != nil {
			t.Fatalf("rotation manager built while rotation is disabled: %v", b.rotationMgr)
		}
	})

	t.Run("default mode builds an observing watchdog", func(t *testing.T) {
		cfg := &config.Config{}
		b := newBoot(t, cfg)
		b.bootSupervision()
		if b.wd == nil {
			t.Fatal("watchdog not built under the default settings")
		}
		if got := b.wd.Mode(); got != watchdog.ModeObserve {
			t.Fatalf("watchdog mode = %q, want %q", got, watchdog.ModeObserve)
		}
	})

	t.Run("linear credential falls back to the work-source key", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Governor.Watchdog.Mode = "off"
		cfg.Governor.WorkSource.Type = "linear"
		cfg.Governor.WorkSource.Linear.APIKey = "  lin_api_test  "
		b := newBoot(t, cfg)
		b.bootSupervision()
		if b.linearCredentialResolver == nil {
			t.Fatal("linear credential resolver not wired")
		}
		got := b.linearCredentialResolver()
		if got.APIKey != "lin_api_test" || got.AccessToken != "" {
			t.Fatalf("linear credential = %+v, want trimmed work-source API key and no access token", got)
		}
	})
}

func TestBootLanes_BuildsOnlyTheRunnableLanes(t *testing.T) {
	newBoot := func(t *testing.T, cfg *config.Config) *boot {
		t.Helper()
		logger := restoreTestLogger()
		cfg.Agents = map[string]config.AgentConfig{}
		return &boot{
			cfg:      cfg,
			logger:   logger,
			gov:      governor.New(cfg.Governor, cfg.Agents, logger),
			agentMgr: agent.NewManager(cfg.Agents, logger, agent.ProjectContext{}),
			dashSrv:  dashboard.NewServer(0, logger),
		}
	}

	t.Run("defaults: replan on, trajectory unrunnable without a reviewer, retro off", func(t *testing.T) {
		b := newBoot(t, &config.Config{})
		b.bootLanes()
		if b.replanLane == nil {
			t.Fatal("stall-replan lane not built (it defaults on)")
		}
		if b.trajLane != nil {
			t.Fatalf("trajectory lane built with no reviewer endpoint: %v", b.trajLane)
		}
		if b.retroLane != nil {
			t.Fatalf("retro lane built while retro is disabled: %v", b.retroLane)
		}
	})

	t.Run("replan disabled explicitly", func(t *testing.T) {
		off := false
		cfg := &config.Config{}
		cfg.Governor.Replan.Enabled = &off
		b := newBoot(t, cfg)
		b.bootLanes()
		if b.replanLane != nil {
			t.Fatalf("stall-replan lane built with replan.enabled=false: %v", b.replanLane)
		}
	})
}
