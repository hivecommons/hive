package dashboard

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	standbypkg "github.com/hivecommons/hive/pkg/standby"
)

func standbyTestConfig() *config.Config {
	return &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"quality": {Standby: &config.StandbyConfig{Enabled: true, MinModelCapability: "T2", DailyCapPerContributor: 1}}},
		},
		Hub: config.HubConfig{
			StandbyContributors: []string{"alice"},
			StandbyModelTiers:   []config.StandbyModelTier{{Backend: "claude", Model: "opus", ReasoningEffort: "high", Tier: "T2"}},
		},
	}
}

func addStandbyConnection(h *ContributeWSHub, username string, approved bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	model := "opus"
	effort := "high"
	if !approved {
		effort = ""
	}
	h.connections[username] = &ContributorConnection{
		profile:         &ContributorProfile{GitHubUsername: username},
		cliBackend:      "claude",
		model:           model,
		reasoningEffort: effort,
		standby: map[string]StandbyConnectionState{"quality": {
			Lane:            "quality",
			CLIBackend:      "claude",
			Model:           model,
			ReasoningEffort: effort,
			DailyCap:        1,
			UpdatedAt:       time.Now(),
		}},
	}
}

func TestQualifiedStandbyCountsPausedLane(t *testing.T) {
	cfg := standbyTestConfig()
	s := &Server{deps: &Dependencies{Config: cfg}, logger: slog.Default()}
	h := NewContributeWSHub(slog.Default(), s)
	addStandbyConnection(h, "alice", true)
	addStandbyConnection(h, "mallory", false)
	// No item-tier list on this hive, so item-tier matching is not in force
	// and this is S4's count: the lane floor decides alone.
	counts := h.QualifiedStandbyCounts([]string{"quality"}, nil)
	if counts["quality"] != 1 {
		t.Fatalf("qualified quality count = %d, want 1", counts["quality"])
	}
	// And passing the lane's queue changes nothing while the list is empty,
	// which is S7's acceptance rule at the surface that renders the number.
	withQueue := h.QualifiedStandbyCounts([]string{"quality"}, map[string][]standbypkg.Item{
		"quality": {{Repo: "my-org/repo-a", Labels: []string{"kind/bug"}}},
	})
	if withQueue["quality"] != 1 {
		t.Fatalf("qualified quality count with a queue = %d, want 1 — an empty item-tier list must change nothing", withQueue["quality"])
	}
}

func TestQualifiedStandbyCountsSkipsDisabledLane(t *testing.T) {
	cfg := standbyTestConfig()
	cfg.Agents["quality"].Standby.Enabled = false
	s := &Server{deps: &Dependencies{Config: cfg}, logger: slog.Default()}
	h := NewContributeWSHub(slog.Default(), s)
	addStandbyConnection(h, "alice", true)

	counts := h.QualifiedStandbyCounts([]string{"quality"}, nil)
	if _, ok := counts["quality"]; ok {
		t.Fatalf("disabled lane count present: %#v", counts)
	}
}

// TestQualifiedStandbyCountsHonourItemTiers is the tile's half of S7: once the
// owner writes an item-tier list, "M qualify" counts contributors who could be
// offered at least one item actually queued on the lane.
func TestQualifiedStandbyCountsHonourItemTiers(t *testing.T) {
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"quality": {Standby: &config.StandbyConfig{Enabled: true, MinModelCapability: "T3", DailyCapPerContributor: 1}}},
		},
		Hub: config.HubConfig{
			StandbyContributors: []string{"alice"},
			StandbyModelTiers:   []config.StandbyModelTier{{Backend: "claude", Model: "haiku", ReasoningEffort: "low", Tier: "T3"}},
			StandbyItemTiers: []config.StandbyItemTier{
				{Label: "dependencies", Tier: "T3", Signal: "the lockfile diff plus green CI"},
				{Label: "kind/security", Tier: "T1"},
			},
		},
	}
	s := &Server{deps: &Dependencies{Config: cfg}, logger: slog.Default()}
	h := NewContributeWSHub(slog.Default(), s)
	h.mu.Lock()
	h.connections["c1"] = &ContributorConnection{
		profile:         &ContributorProfile{GitHubUsername: "alice"},
		cliBackend:      "claude",
		model:           "haiku",
		reasoningEffort: "low",
		standby: map[string]StandbyConnectionState{"quality": {
			Lane: "quality", CLIBackend: "claude", Model: "haiku", ReasoningEffort: "low",
			DailyCap: 1, UpdatedAt: time.Now(),
		}},
	}
	h.mu.Unlock()

	for _, tc := range []struct {
		name  string
		queue []standbypkg.Item
		want  int
	}{
		{"a listed T3 item is donatable to a T3 configuration", []standbypkg.Item{{Repo: "my-org/repo-a", Labels: []string{"dependencies"}}}, 1},
		{"a listed T1 item is not", []standbypkg.Item{{Repo: "my-org/repo-a", Labels: []string{"kind/security"}}}, 0},
		{"an unlisted item is not, however ordinary it looks", []standbypkg.Item{{Repo: "my-org/repo-a", Labels: []string{"kind/bug"}}}, 0},
		{"one donatable item in the queue is enough", []standbypkg.Item{
			{Repo: "my-org/repo-a", Labels: []string{"kind/bug"}},
			{Repo: "my-org/repo-a", Labels: []string{"dependencies"}},
		}, 1},
		{"an empty queue has nothing to donate", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counts := h.QualifiedStandbyCounts([]string{"quality"}, map[string][]standbypkg.Item{"quality": tc.queue})
			if counts["quality"] != tc.want {
				t.Errorf("qualified quality count = %d, want %d", counts["quality"], tc.want)
			}
		})
	}
}

// TestStandbyLaneItemsUsesDispatchableQueue verifies the tile counts only
// unheld admission items that standby dispatch can select for each lane.
func TestStandbyLaneItemsUsesDispatchableQueue(t *testing.T) {
	queue := []ReadyQueueItem{
		{Repo: "my-org/repo-a", Lane: "architect", Labels: []string{"rfc"}},
		{Repo: "my-org/repo-b", Lane: "guide", Labels: []string{"dependencies"}},
		{Repo: "my-org/repo-b", Lane: "scanner", Labels: []string{"kind/security"}, Held: true},
	}
	items := standbyLaneItems([]string{"scanner", "guide", "architect"}, queue)
	if len(items["scanner"]) != 0 {
		t.Errorf("scanner items = %v, want no dispatchable items", items["scanner"])
	}
	if len(items["guide"]) != 1 {
		t.Errorf("guide items = %v, want only its dispatchable issue", items["guide"])
	}
	if len(items["architect"]) != 1 {
		t.Errorf("architect items = %v, want only its dispatchable issue", items["architect"])
	}
	if got := items["guide"][0].Labels; len(got) != 1 || got[0] != "dependencies" {
		t.Errorf("labels were not carried: %v", got)
	}
	if standbyLaneItems(nil, queue) != nil || standbyLaneItems([]string{"guide"}, nil) != nil {
		t.Error("standbyLaneItems must be nil when there are no lanes or no queue")
	}
}

func TestQualifiedStandbyCountsDecrementsCapOnDispatch(t *testing.T) {
	cfg := standbyTestConfig()
	s := &Server{deps: &Dependencies{Config: cfg}, logger: slog.Default()}
	h := NewContributeWSHub(slog.Default(), s)
	addStandbyConnection(h, "alice", true)
	h.recordStandbyDispatch("alice", "quality", time.Now())
	counts := h.QualifiedStandbyCounts([]string{"quality"}, nil)
	if counts["quality"] != 0 {
		t.Fatalf("qualified count after dispatch = %d, want 0", counts["quality"])
	}
}

func TestQualifiedStandbyCountsExcludesSuspendedConfiguration(t *testing.T) {
	cfg := standbyTestConfig()
	s := &Server{deps: &Dependencies{Config: cfg}, logger: slog.Default()}
	h := NewContributeWSHub(slog.Default(), s)
	addStandbyConnection(h, "alice", true)
	standbyCfg := standbypkg.Configuration{Backend: "claude", Model: "opus", ReasoningEffort: "high"}
	key := standbyOutcomeKey("alice", standbyCfg)
	h.appendStandbyOutcome(standbyOutcomeRecord{Key: key, Lane: "quality", Kind: standbypkg.OutcomeClosedUnmerged})
	h.appendStandbyOutcome(standbyOutcomeRecord{Key: key, Lane: "quality", Kind: standbypkg.OutcomeClosedUnmerged})

	counts := h.QualifiedStandbyCounts([]string{"quality"}, nil)
	if counts["quality"] != 0 {
		t.Fatalf("qualified count for suspended config = %d, want 0", counts["quality"])
	}
	suspended := h.SuspendedStandbyCounts([]string{"quality"})
	if suspended["quality"] != 1 {
		t.Fatalf("suspended count = %d, want 1", suspended["quality"])
	}
}

func TestStandbyOutcomeLedgerPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), standbyOutcomesFileName)
	first := NewContributeWSHub(slog.Default(), nil)
	first.standbyOutcomesFile = path
	cfg := standbypkg.Configuration{Backend: "claude", Model: "opus", ReasoningEffort: "high"}
	key := standbyOutcomeKey("alice", cfg)
	first.appendStandbyOutcome(standbyOutcomeRecord{Key: key, Kind: standbypkg.OutcomeClosedUnmerged})
	first.appendStandbyOutcome(standbyOutcomeRecord{Key: key, Kind: standbypkg.OutcomeClosedUnmerged})

	second := NewContributeWSHub(slog.Default(), nil)
	second.standbyOutcomesFile = path
	second.loadStandbyOutcomes()
	suspended, streak := second.standbySuspended(key)
	if !suspended || streak != 2 {
		t.Fatalf("loaded suspension = (%v,%d), want (true,2)", suspended, streak)
	}
	second.clearStandbySuspension("alice", cfg)
	suspended, streak = second.standbySuspended(key)
	if suspended || streak != 0 {
		t.Fatalf("cleared suspension = (%v,%d), want (false,0)", suspended, streak)
	}
}

func TestStandbyModeCeilingRefusesNonPRLane(t *testing.T) {
	level := 2
	cfg := &config.Config{
		ACMMLevel: &level,
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"quality": {Mode: "ADVISORY"}},
		},
	}
	if got := standbyModeCeilingReason(cfg, "quality"); got == "" {
		t.Fatal("standbyModeCeilingReason = empty, want refusal")
	}
	cfg.Agents["quality"] = config.AgentConfig{Mode: "ISSUES_AND_PRS"}
	if got := standbyModeCeilingReason(cfg, "quality"); got != "" {
		t.Fatalf("standbyModeCeilingReason = %q, want allowed", got)
	}
}
