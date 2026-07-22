package dashboard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/governor"
)

func TestConfigCoordinatorPreparesManagerBeforeGovernorPublication(t *testing.T) {
	logger := coordinatorTestLogger()
	cfg := &config.Config{Agents: map[string]config.AgentConfig{}, Governor: config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]string{}}},
	}}
	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	coordinator := NewConfigCoordinator(cfg, gov, mgr)
	coordinator.SetSaveFunc(func(*config.Config) error { return nil })

	if err := coordinator.Mutate(func(candidate *config.Config) error {
		candidate.Agents["worker"] = config.AgentConfig{Enabled: true, Role: "worker"}
		mode := candidate.Governor.Modes["idle"]
		mode.Cadences["worker"] = "1ns"
		candidate.Governor.Modes["idle"] = mode
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.GetStatus("worker"); err != nil {
		t.Fatalf("enabled worker was not prepared in Manager: %v", err)
	}
	if due := gov.Evaluate(0, 0, 0, 0); !slices.Contains(due, "worker") {
		t.Fatalf("published Governor snapshot did not include prepared worker: %v", due)
	}
}

func TestConfigCoordinatorFailedSavePublishesNothing(t *testing.T) {
	logger := coordinatorTestLogger()
	cfg := &config.Config{Agents: map[string]config.AgentConfig{}, Governor: config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]string{}}},
	}}
	before := cfg.Clone()
	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	coordinator := NewConfigCoordinator(cfg, gov, mgr)
	wantErr := errors.New("save failed")
	coordinator.SetSaveFunc(func(*config.Config) error { return wantErr })

	err := coordinator.Mutate(func(candidate *config.Config) error {
		candidate.Agents["worker"] = config.AgentConfig{Enabled: true}
		mode := candidate.Governor.Modes["idle"]
		mode.Cadences["worker"] = "1ns"
		candidate.Governor.Modes["idle"] = mode
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Mutate error = %v, want %v", err, wantErr)
	}
	if got := coordinator.Snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed save changed published Config:\n got: %#v\nwant: %#v", got, before)
	}
	if _, err := mgr.GetStatus("worker"); err == nil {
		t.Fatal("failed save published worker to Manager")
	}
	if due := gov.Evaluate(0, 0, 0, 0); len(due) != 0 {
		t.Fatalf("failed save published worker to Governor: %v", due)
	}
}

func TestConfigCoordinatorNeverPublishesMixedGovernorAndAgentSnapshot(t *testing.T) {
	logger := coordinatorTestLogger()
	stateA := &config.Config{
		Agents: map[string]config.AgentConfig{"worker": {Enabled: true, OnDemand: true}},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]string{"worker": "1ns"}},
		}},
	}
	stateB := &config.Config{
		Agents: map[string]config.AgentConfig{"worker": {Enabled: true, OnDemand: false}},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]string{"worker": "paused"}},
		}},
	}
	cfg := stateA.Clone()
	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	coordinator := NewConfigCoordinator(cfg, gov, nil)

	var mixed atomic.Bool
	done := make(chan struct{})
	firstRead := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		first := true
		for {
			select {
			case <-done:
				return
			default:
				due := gov.Evaluate(0, 0, 0, 0)
				if first {
					close(firstRead)
					first = false
				}
				if len(due) != 0 {
					mixed.Store(true)
					return
				}
			}
		}
	}()
	<-firstRead

	const replacements = 2000
	for i := 0; i < replacements; i++ {
		next := stateA
		if i%2 == 0 {
			next = stateB
		}
		if err := coordinator.Replace(next, nil); err != nil {
			close(done)
			readers.Wait()
			t.Fatal(err)
		}
	}
	close(done)
	readers.Wait()
	if mixed.Load() {
		t.Fatal("Governor observed cadence from one config with agents from another")
	}
}

func TestDashboardNestedToolEditPublishesDetachedSnapshot(t *testing.T) {
	logger := coordinatorTestLogger()
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{"worker": {
			Enabled: true,
			Tools:   &config.ToolsConfig{Preset: "old", Rules: []config.ToolRule{{Pattern: "old", Action: "deny"}}},
		}},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{}},
	}
	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	coordinator := NewConfigCoordinator(cfg, gov, mgr)
	var persistedCandidate *config.Config
	coordinator.SetSaveFunc(func(candidate *config.Config) error {
		persistedCandidate = candidate
		return nil
	})

	server := NewServer(0, logger)
	server.RegisterAPI(&Dependencies{
		Config:            cfg,
		ConfigCoordinator: coordinator,
		AgentMgr:          mgr,
		Governor:          gov,
		Logger:            logger,
		Ctx:               context.Background(),
	})
	w := doPut(server, "/api/config/agent/worker/tools", config.ToolsConfig{
		Preset: "restricted",
		Rules:  []config.ToolRule{{Pattern: "github.*", Action: "allow", Reason: "test"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("tool edit status = %d, body=%s", w.Code, w.Body.String())
	}

	if persistedCandidate == nil {
		t.Fatal("save hook did not receive candidate")
	}
	persistedCandidate.Agents["worker"].Tools.Rules[0].Pattern = "mutated-after-save"
	wantPattern := "github.*"
	if got := coordinator.Snapshot().Agents["worker"].Tools.Rules[0].Pattern; got != wantPattern {
		t.Fatalf("published Config aliases persisted candidate: got %q, want %q", got, wantPattern)
	}
	status, err := mgr.GetStatus("worker")
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Config.Tools.Rules[0].Pattern; got != wantPattern {
		t.Fatalf("Manager aliases persisted candidate: got %q, want %q", got, wantPattern)
	}

	detached := coordinator.Snapshot()
	detached.Agents["worker"].Tools.Rules[0].Pattern = "mutated-snapshot"
	if got := coordinator.Snapshot().Agents["worker"].Tools.Rules[0].Pattern; got != wantPattern {
		t.Fatalf("Snapshot returned aliased nested tool rules: got %q, want %q", got, wantPattern)
	}
}

func TestApplyPackFailedSaveDoesNotPartiallyPublishRuntime(t *testing.T) {
	logger := coordinatorTestLogger()
	cfg := &config.Config{
		Data:   config.DataConfig{AgentsDir: t.TempDir()},
		Agents: map[string]config.AgentConfig{},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]string{}},
		}},
	}
	before := cfg.Clone()
	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	coordinator := NewConfigCoordinator(cfg, gov, mgr)
	coordinator.SetSaveFunc(func(*config.Config) error { return errors.New("pack save failed") })
	server := NewServer(0, logger)
	server.RegisterAPI(&Dependencies{
		Config:            cfg,
		ConfigCoordinator: coordinator,
		AgentMgr:          mgr,
		Governor:          gov,
		Logger:            logger,
		Ctx:               context.Background(),
	})

	if _, err := server.ApplyPack(1); err == nil {
		t.Fatal("ApplyPack unexpectedly succeeded")
	}
	if got := coordinator.Snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed pack published Config:\n got: %#v\nwant: %#v", got, before)
	}
	if len(mgr.AllStatuses()) != 0 {
		t.Fatalf("failed pack published Manager agents: %#v", mgr.AllStatuses())
	}
	if due := gov.Evaluate(0, 0, 0, 0); len(due) != 0 {
		t.Fatalf("failed pack published Governor agents: %v", due)
	}
}

func TestApplyPackPublishesManagerAndGovernorTogether(t *testing.T) {
	logger := coordinatorTestLogger()
	cfg := &config.Config{
		Data: config.DataConfig{AgentsDir: t.TempDir()},
		Agents: map[string]config.AgentConfig{
			"guide": {Enabled: true},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{}},
	}
	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	coordinator := NewConfigCoordinator(cfg, gov, mgr)
	coordinator.SetSaveFunc(func(*config.Config) error { return nil })
	server := NewServer(0, logger)
	server.RegisterAPI(&Dependencies{
		Config:            cfg,
		ConfigCoordinator: coordinator,
		AgentMgr:          mgr,
		Governor:          gov,
		Logger:            logger,
		Ctx:               context.Background(),
	})

	result, err := server.ApplyPack(1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.Created, "brainstorm") {
		t.Fatalf("pack did not create brainstorm: %#v", result)
	}
	for _, name := range []string{"guide", "brainstorm"} {
		if _, err := mgr.GetStatus(name); err != nil {
			t.Fatalf("pack agent %q missing from Manager: %v", name, err)
		}
	}
	snapshot := coordinator.Snapshot()
	if snapshot.ACMMLevel == nil || *snapshot.ACMMLevel != 1 {
		t.Fatalf("pack ACMM level not published: %#v", snapshot.ACMMLevel)
	}
	if snapshot.Agents["guide"].StaleTimeout != 28800 {
		t.Fatalf("pack stale timeout not published: %#v", snapshot.Agents["guide"])
	}
	if due := gov.Evaluate(0, 0, 0, 0); !slices.Contains(due, "guide") {
		t.Fatalf("Governor did not receive pack cadence and enabled guide: %v", due)
	}
}

func TestAgentImportPublishesManagerBeforeGovernor(t *testing.T) {
	logger := coordinatorTestLogger()
	cfg := &config.Config{
		Data:   config.DataConfig{AgentsDir: t.TempDir()},
		Agents: map[string]config.AgentConfig{},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]string{"imported": "1ns"}},
		}},
	}
	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	coordinator := NewConfigCoordinator(cfg, gov, mgr)
	coordinator.SetSaveFunc(func(*config.Config) error { return nil })
	server := NewServer(0, logger)
	server.RegisterAPI(&Dependencies{
		Config:            cfg,
		ConfigCoordinator: coordinator,
		AgentMgr:          mgr,
		Governor:          gov,
		Logger:            logger,
		Ctx:               context.Background(),
	})

	definition := `apiVersion: hive.kubestellar.io/v1
kind: AgentDefinition
metadata:
  name: Imported
spec:
  backend: copilot
  role: quality
  tools:
    preset: restricted
    rules:
      - pattern: github.*
        action: allow
`
	w := doPost(server, "/api/agents/import", map[string]any{
		"source":  "paste",
		"content": definition,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("import status = %d, body=%s", w.Code, w.Body.String())
	}
	status, err := mgr.GetStatus("imported")
	if err != nil {
		t.Fatalf("imported agent missing from Manager: %v", err)
	}
	if status.Config.Tools == nil || status.Config.Tools.Rules[0].Pattern != "github.*" {
		t.Fatalf("Manager did not receive imported nested config: %#v", status.Config.Tools)
	}
	if due := gov.Evaluate(0, 0, 0, 0); !slices.Contains(due, "imported") {
		t.Fatalf("Governor admitted import before/without matching Manager entry: %v", due)
	}
}

func TestAgentDeleteRemovesGovernorBeforeManagerEntry(t *testing.T) {
	logger := coordinatorTestLogger()
	agentsDir := t.TempDir()
	worker := config.AgentConfig{Enabled: true, Managed: true, Role: "worker"}
	if err := config.SaveAgentFile(agentsDir, "worker", worker); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Data:   config.DataConfig{AgentsDir: agentsDir},
		Agents: map[string]config.AgentConfig{"worker": worker},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]string{"worker": "1ns"}},
		}},
	}
	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	coordinator := NewConfigCoordinator(cfg, gov, mgr)
	coordinator.SetSaveFunc(func(*config.Config) error { return nil })
	server := NewServer(0, logger)
	server.RegisterAPI(&Dependencies{
		Config:            cfg,
		ConfigCoordinator: coordinator,
		AgentMgr:          mgr,
		Governor:          gov,
		Logger:            logger,
		Ctx:               context.Background(),
	})

	w := doDelete(server, "/api/agents/worker", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", w.Code, w.Body.String())
	}
	if _, exists := coordinator.Snapshot().Agents["worker"]; exists {
		t.Fatal("deleted agent remains in published Config")
	}
	if due := gov.Evaluate(0, 0, 0, 0); slices.Contains(due, "worker") {
		t.Fatalf("deleted agent remains in Governor: %v", due)
	}
	if _, err := mgr.GetStatus("worker"); err == nil {
		t.Fatal("deleted agent remains in Manager")
	}
}

func coordinatorTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
