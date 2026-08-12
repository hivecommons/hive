package dashboard

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/governor"
	"github.com/kubestellar/hive/v2/pkg/tokens"
)

func TestFormatHumanTime(t *testing.T) {
	ts := time.Date(2025, 6, 15, 14, 30, 0, 0, time.UTC)
	result := formatHumanTime(ts)
	if result == "" {
		t.Error("expected non-empty formatted time")
	}
}

func TestParseCadenceDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"15m", 15 * time.Minute},
		{"1h", time.Hour},
		{"30s", 30 * time.Second},
		{"5min", 5 * time.Minute},
		{"off", 0},
		{"pause", 0},
		{"", 0},
		{"invalid", 0},
	}
	for _, tt := range tests {
		got := parseCadenceDuration(tt.input)
		if got != tt.want {
			t.Errorf("parseCadenceDuration(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestComputeNextKick(t *testing.T) {
	// off cadence
	result := computeNextKick(nil, "off")
	if result != "" {
		t.Errorf("expected empty for off cadence, got %q", result)
	}

	// pause cadence
	result = computeNextKick(nil, "pause")
	if result != "" {
		t.Errorf("expected empty for pause cadence, got %q", result)
	}

	// empty cadence
	result = computeNextKick(nil, "")
	if result != "" {
		t.Errorf("expected empty for empty cadence, got %q", result)
	}

	// valid cadence with no last kick
	result = computeNextKick(nil, "15m")
	if result == "" {
		t.Error("expected non-empty for valid cadence")
	}

	// valid cadence with last kick
	now := time.Now()
	result = computeNextKick(&now, "15m")
	if result == "" {
		t.Error("expected non-empty for valid cadence with last kick")
	}
}

func TestLookupCadence(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Cadences: map[string]config.Cadence{"scanner": "15m"}},
			},
		},
	}
	result := lookupCadence("scanner", cfg)
	if result != "15m" {
		t.Errorf("lookupCadence = %q, want 15m", result)
	}

	result = lookupCadence("nonexistent", cfg)
	if result != "" {
		t.Errorf("lookupCadence nonexistent = %q, want empty", result)
	}
}

func TestLookupCadenceForMode(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"busy": {Cadences: map[string]config.Cadence{"scanner": "5m"}},
			},
		},
	}
	result := lookupCadenceForMode("scanner", "busy", cfg)
	if result != "5m" {
		t.Errorf("got %q, want 5m", result)
	}

	result = lookupCadenceForMode("scanner", "nonexistent", cfg)
	if result != "" {
		t.Errorf("got %q, want empty", result)
	}
}

func TestBuildGovernor(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			EvalIntervalS: 300,
			Modes: map[string]config.ModeConfig{
				"quiet": {Threshold: 3},
				"busy":  {Threshold: 8},
				"surge": {Threshold: 15},
			},
		},
	}
	state := governor.State{Mode: governor.ModeBusy, QueueIssues: 12, QueuePRs: 3}
	result := buildGovernor(state, cfg)
	if !result.Active {
		t.Error("expected Active=true")
	}
	if result.Mode != "busy" {
		t.Errorf("mode = %q, want busy", result.Mode)
	}
	if result.Issues != 12 {
		t.Errorf("issues = %d, want 12", result.Issues)
	}
	if result.Thresholds.Quiet != 3 {
		t.Errorf("quiet = %d, want 3", result.Thresholds.Quiet)
	}
}

func TestBuildGovernor_DefaultThresholds(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			EvalIntervalS: 0,
			Modes:         map[string]config.ModeConfig{},
		},
	}
	state := governor.State{Mode: governor.ModeIdle}
	result := buildGovernor(state, cfg)
	// Default thresholds: quiet=2, busy=10, surge=20
	if result.Thresholds.Quiet != 2 {
		t.Errorf("default quiet = %d, want 2", result.Thresholds.Quiet)
	}
	if result.NextKick != "" {
		t.Errorf("nextKick should be empty with 0 interval, got %q", result.NextKick)
	}
}

func TestBuildTokens_NilCollector(t *testing.T) {
	ft := buildTokens(nil)
	if ft.LookbackHours != defaultLookbackHours {
		t.Errorf("lookbackHours = %d", ft.LookbackHours)
	}
	if ft.Totals.Input != 0 {
		t.Errorf("input = %d", ft.Totals.Input)
	}
}

func TestBuildRepos(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1", "repo2"}},
	}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{
			Items: []github.Issue{
				{Repo: "repo1", Number: 1, Title: "issue1"},
			},
		},
		PRs: github.PRResult{
			Items: []github.PullRequest{
				{Repo: "repo2", Number: 10, Title: "pr1"},
			},
		},
		TotalByRepo: map[string]github.RepoCounts{
			"repo1": {Issues: 1, PRs: 0},
			"repo2": {Issues: 0, PRs: 1},
		},
	}

	repos := buildRepos(cfg, actionable)
	if len(repos) != 2 {
		t.Fatalf("repos len = %d, want 2", len(repos))
	}
	if repos[0].Name != "repo1" {
		t.Errorf("name = %q", repos[0].Name)
	}
	if repos[0].Full != "myorg/repo1" {
		t.Errorf("full = %q", repos[0].Full)
	}
	if repos[0].Issues != 1 {
		t.Errorf("issues = %d", repos[0].Issues)
	}
}

func TestBuildRepos_NilActionable(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}},
	}
	repos := buildRepos(cfg, nil)
	if len(repos) != 1 {
		t.Fatalf("repos len = %d", len(repos))
	}
	if repos[0].Issues != 0 {
		t.Errorf("issues = %d", repos[0].Issues)
	}
}

func TestBuildBeads(t *testing.T) {
	// nil stores
	fb := buildBeads(nil)
	if fb.Workers != 0 || fb.Supervisor != 0 {
		t.Error("expected zero beads for nil stores")
	}

	// with stores
	stores := map[string]*beads.Store{}
	fb = buildBeads(stores)
	if fb.Workers != 0 {
		t.Errorf("workers = %d", fb.Workers)
	}
}

func TestBuildHealth_NilClient(t *testing.T) {
	health := buildHealth(nil, nil)
	if health == nil {
		t.Fatal("expected non-nil health map")
	}
}

func TestBuildBudget(t *testing.T) {
	cfg := config.GovernorConfig{}
	gov := governor.New(cfg, map[string]config.AgentConfig{}, nil)
	gov.SetBudgetLimit(1000000)

	fb := buildBudget(gov, nil)
	if fb.WeeklyBudget != 1000000 {
		t.Errorf("weekly budget = %d", fb.WeeklyBudget)
	}
}

func TestBuildCadenceMatrix(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner":    {Backend: "claude"},
			"supervisor": {Backend: "claude"},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle":  {Cadences: map[string]config.Cadence{"scanner": "15m", "supervisor": "pause"}},
				"quiet": {Cadences: map[string]config.Cadence{"scanner": "10m"}},
				"busy":  {Cadences: map[string]config.Cadence{}},
				"surge": {Cadences: map[string]config.Cadence{}},
			},
		},
	}
	statuses := map[string]*agent.AgentProcess{
		"scanner":    {Paused: false},
		"supervisor": {Paused: true},
	}
	matrix := buildCadenceMatrix(cfg, statuses)
	if len(matrix) != 2 {
		t.Fatalf("matrix len = %d", len(matrix))
	}
}

func TestBuildHold_Nil(t *testing.T) {
	h := buildHold(nil)
	if h.Total != 0 {
		t.Errorf("total = %d", h.Total)
	}
	if h.Items == nil {
		t.Error("items should be non-nil slice")
	}
}

func TestBuildHold_WithItems(t *testing.T) {
	actionable := &github.ActionableResult{
		Hold: github.HoldResult{
			Total: 3,
			Items: []github.HoldItem{
				{Number: 1, Type: "issue"},
				{Number: 2, Type: "pr"},
				{Number: 3, Type: "issue"},
			},
		},
	}
	h := buildHold(actionable)
	if h.Total != 3 {
		t.Errorf("total = %d", h.Total)
	}
	if h.Issues != 2 {
		t.Errorf("issues = %d, want 2", h.Issues)
	}
	if h.PRs != 1 {
		t.Errorf("prs = %d, want 1", h.PRs)
	}
}

func TestBuildGHRateLimits_NilClient(t *testing.T) {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{Token: "tok"},
	}
	result := buildGHRateLimits(nil, nil, cfg)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	identity := result["identity"].(map[string]any)
	if identity["type"] != "token" {
		t.Errorf("type = %v", identity["type"])
	}
}

func TestBuildGHRateLimits_AppAuth(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg"},
		GitHub:  config.GitHubConfig{AppID: 123},
	}
	result := buildGHRateLimits(nil, nil, cfg)
	identity := result["identity"].(map[string]any)
	if identity["type"] != "app" {
		t.Errorf("type = %v", identity["type"])
	}
}

func TestBuildAgents(t *testing.T) {
	now := time.Now()
	buf := agent.NewRingBuffer(10)
	buf.Write("test output line")

	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude", Model: "sonnet"},
			State:        agent.StateRunning,
			LastKick:     &now,
			OutputBuffer: buf,
		},
		"supervisor": {
			Name:         "supervisor",
			Config:       config.AgentConfig{Backend: "claude", Model: "opus"},
			State:        agent.StatePaused,
			Paused:       true,
			PinnedCLI:    "claude",
			PinnedModel:  "opus",
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}

	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner":    {Backend: "claude", Model: "sonnet", SortOrder: 20},
			"supervisor": {Backend: "claude", Model: "opus", BeadRole: "supervisor", SortOrder: 10},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Cadences: map[string]config.Cadence{"scanner": "15m"}},
			},
		},
	}

	govState := governor.State{Mode: governor.ModeIdle}
	agents := buildAgents(statuses, cfg, govState)

	if len(agents) != 2 {
		t.Fatalf("agents len = %d, want 2", len(agents))
	}
	// supervisor should be first (sorted first)
	if agents[0].Name != "supervisor" {
		t.Errorf("first agent = %q, want supervisor", agents[0].Name)
	}
	if !agents[0].PinnedCli {
		t.Error("supervisor should have pinnedCli=true")
	}
	if !agents[0].PinnedBoth {
		t.Error("supervisor should have pinnedBoth=true")
	}
}

// TestBuildAgents_OffByCadence verifies that an agent whose cadence for the
// current governor mode is a non-kicking value ("pause"/"off") is flagged
// OffByCadence, while a normally-scheduled agent is not. This is the signal the
// dashboard uses to paint a hollow-green (alive-but-governor-off) dot for hives
// stuck in a mode where every cadence is paused (e.g. SURGE).
func TestBuildAgents_OffByCadence(t *testing.T) {
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude"},
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
		"quality": {
			Name:         "quality",
			Config:       config.AgentConfig{Backend: "claude"},
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
		"brainstorm": {
			Name:         "brainstorm",
			Config:       config.AgentConfig{Backend: "claude", OnDemand: true},
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner":    {Backend: "claude", SortOrder: 10},
			"quality":    {Backend: "claude", SortOrder: 20},
			"brainstorm": {Backend: "claude", OnDemand: true, SortOrder: 30},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				// SURGE mode pauses scanner but keeps quality on a timer;
				// brainstorm is paused too but is on-demand, so it must be excluded.
				"surge": {Cadences: map[string]config.Cadence{
					"scanner":    "pause",
					"quality":    "15m",
					"brainstorm": "pause",
				}},
			},
		},
	}

	agents := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeSurge})
	byName := map[string]FrontendAgent{}
	for _, a := range agents {
		byName[a.Name] = a
	}

	if !byName["scanner"].OffByCadence {
		t.Error("scanner cadence=pause in surge: want OffByCadence=true")
	}
	if byName["quality"].OffByCadence {
		t.Error("quality cadence=15m in surge: want OffByCadence=false")
	}
	if byName["brainstorm"].OffByCadence {
		t.Error("brainstorm is on-demand: want OffByCadence=false even though cadence=pause")
	}
}

func TestBuildFrontendStatus(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet"},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Cadences: map[string]config.Cadence{"scanner": "15m"}},
			},
		},
		GitHub: config.GitHubConfig{Token: "tok"},
	}

	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	govState := gov.GetState()

	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude", Model: "sonnet"},
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}

	payload := BuildFrontendStatus(govState, nil, statuses, cfg, nil, gov, nil, nil, nil, nil)
	if payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if payload.Timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
	if len(payload.Agents) != 1 {
		t.Errorf("agents len = %d", len(payload.Agents))
	}
}

func TestBuildFrontendStatusACMMLevelConfigured(t *testing.T) {
	cfg := &config.Config{}
	gov := governor.New(cfg.Governor, cfg.Agents, nil)

	payload := BuildFrontendStatus(gov.GetState(), nil, nil, cfg, nil, gov, nil, nil, nil, nil)
	if payload.ACMMLevel != 1 {
		t.Fatalf("default acmm level = %d, want 1", payload.ACMMLevel)
	}
	if payload.ACMMLevelConfigured {
		t.Fatal("default acmm level must not be reported as explicitly configured")
	}

	level := 4
	cfg.ACMMLevel = &level
	payload = BuildFrontendStatus(gov.GetState(), nil, nil, cfg, nil, gov, nil, nil, nil, nil)
	if payload.ACMMLevel != level {
		t.Fatalf("configured acmm level = %d, want %d", payload.ACMMLevel, level)
	}
	if !payload.ACMMLevelConfigured {
		t.Fatal("explicit acmm level must be reported as configured")
	}
}

func TestBuildBudget_WithSpend(t *testing.T) {
	cfg := config.GovernorConfig{}
	gov := governor.New(cfg, map[string]config.AgentConfig{}, nil)
	gov.SetBudgetLimit(1000000)
	// Open the window first so the totals count as in-window spend.
	gov.SetBudgetResetAt(time.Now())
	gov.UpdateBudgetFromTotals(250000, nil, nil)

	fb := buildBudget(gov, nil)
	if fb.WeeklyBudget != 1000000 {
		t.Errorf("weekly = %d", fb.WeeklyBudget)
	}
	if fb.Used != 250000 {
		t.Errorf("used = %d, want 250000", fb.Used)
	}
	if fb.Remaining != 750000 {
		t.Errorf("remaining = %d, want 750000", fb.Remaining)
	}
}

func TestBuildBudget_OverSpend(t *testing.T) {
	cfg := config.GovernorConfig{}
	gov := governor.New(cfg, map[string]config.AgentConfig{}, nil)
	gov.SetBudgetLimit(1000)
	// Open the window first so the totals count as in-window spend.
	gov.SetBudgetResetAt(time.Now())
	gov.UpdateBudgetFromTotals(2000, nil, nil)

	fb := buildBudget(gov, nil)
	if fb.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", fb.Remaining)
	}
}

func TestBuildBudget_NoBudget(t *testing.T) {
	cfg := config.GovernorConfig{}
	gov := governor.New(cfg, map[string]config.AgentConfig{}, nil)

	fb := buildBudget(gov, nil)
	if fb.WeeklyBudget != 0 {
		t.Errorf("weekly = %d", fb.WeeklyBudget)
	}
	if fb.PctUsed != 0 {
		t.Errorf("pctUsed = %f", fb.PctUsed)
	}
}

func TestBuildBeads_EmptyStores(t *testing.T) {
	fb := buildBeads(map[string]*beads.Store{})
	if fb.Workers != 0 || fb.Supervisor != 0 {
		t.Errorf("beads = %+v", fb)
	}
}

func TestBuildHealth_WithCachedHealth(t *testing.T) {
	// Set cached health and then call with nil client
	cachedHealthMu.Lock()
	cachedHealth = map[string]any{"ci": 95, "tests": 100}
	cachedHealthMu.Unlock()

	defer func() {
		cachedHealthMu.Lock()
		cachedHealth = nil
		cachedHealthMu.Unlock()
	}()

	health := buildHealth(nil, nil)
	if health["ci"] != 95 {
		t.Errorf("ci = %v, want 95", health["ci"])
	}
}

func TestBuildAgents_WithOverrides(t *testing.T) {
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name:            "scanner",
			Config:          config.AgentConfig{Backend: "claude", Model: "sonnet"},
			State:           agent.StateRunning,
			BackendOverride: "aider",
			ModelOverride:   "opus",
			OutputBuffer:    agent.NewRingBuffer(10),
		},
	}
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet"},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{},
		},
	}
	agents := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle})
	if len(agents) != 1 {
		t.Fatalf("len = %d", len(agents))
	}
	if agents[0].CLI != "aider" {
		t.Errorf("cli = %q, want aider", agents[0].CLI)
	}
	if agents[0].Model != "opus" {
		t.Errorf("model = %q, want opus", agents[0].Model)
	}
}

func TestBuildCadenceMatrix_PausedWithCadence(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle":  {Cadences: map[string]config.Cadence{"scanner": "15m"}},
				"busy":  {Cadences: map[string]config.Cadence{"scanner": "5m"}},
				"quiet": {Cadences: map[string]config.Cadence{"scanner": "30m"}},
				"surge": {Cadences: map[string]config.Cadence{"scanner": "2m"}},
			},
		},
	}
	statuses := map[string]*agent.AgentProcess{
		"scanner": {Paused: true},
	}
	matrix := buildCadenceMatrix(cfg, statuses)
	if len(matrix) != 1 {
		t.Fatalf("len = %d", len(matrix))
	}
	// All cadences should be "paused" since the agent is paused and has non-off cadences
	if matrix[0].Idle != "paused" {
		t.Errorf("idle = %q, want paused", matrix[0].Idle)
	}
	if matrix[0].Busy != "paused" {
		t.Errorf("busy = %q, want paused", matrix[0].Busy)
	}
}

func TestBuildRepos_WithActionable(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}},
	}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{
			Items: []github.Issue{{Repo: "repo1", Number: 1, Title: "bug"}},
		},
		PRs: github.PRResult{
			Items: []github.PullRequest{{Repo: "repo1", Number: 2, Title: "fix"}},
		},
		TotalByRepo: map[string]github.RepoCounts{
			"repo1": {Issues: 5, PRs: 3},
		},
	}
	repos := buildRepos(cfg, actionable)
	if len(repos) != 1 {
		t.Fatalf("len = %d", len(repos))
	}
	if repos[0].Issues != 5 {
		t.Errorf("issues = %d, want 5", repos[0].Issues)
	}
	if repos[0].PRs != 3 {
		t.Errorf("prs = %d, want 3", repos[0].PRs)
	}
}

func TestBuildBeads_WithData(t *testing.T) {
	dir := t.TempDir()
	s1, err := beads.NewStore(dir + "/supervisor")
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	s2, err := beads.NewStore(dir + "/worker")
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	stores := map[string]*beads.Store{
		"supervisor": s1,
		"worker":     s2,
	}
	fb := buildBeads(stores)
	// Empty stores, count should be 0 for both
	if fb.Supervisor != 0 {
		t.Errorf("supervisor = %d", fb.Supervisor)
	}
	if fb.Workers != 0 {
		t.Errorf("workers = %d", fb.Workers)
	}
}

func TestBuildFrontendStatus_WithMetrics(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}},
		Agents:  map[string]config.AgentConfig{"scanner": {Backend: "claude"}},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle": {Cadences: map[string]config.Cadence{"scanner": "15m"}},
			},
		},
		GitHub: config.GitHubConfig{Token: "tok"},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	govState := gov.GetState()
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude"},
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}
	// Test with non-nil metrics collector (just to hit the non-nil branch)
	// We create a minimal one without GH client
	mc := &MetricsCollector{
		metrics: map[string]any{"outreach": map[string]any{"stars": 42}},
	}
	payload := BuildFrontendStatus(govState, nil, statuses, cfg, nil, gov, nil, nil, nil, mc)
	if payload == nil {
		t.Fatal("expected non-nil payload")
	}
	if payload.AgentMetrics == nil {
		t.Error("expected non-nil agent metrics")
	}
	if payload.AgentMetrics["outreach"] == nil {
		t.Error("expected outreach in metrics")
	}
}

func TestComputeNextKick_WithDuration(t *testing.T) {
	// Cover the parseCadenceDuration returning 0 branch
	result := computeNextKick(nil, "invalid-cadence")
	if result != "" {
		t.Errorf("result = %q, want empty", result)
	}
}

func TestBuildTokens_NilSummary(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := tokens.NewCollectorWithPersistPath(dir, filepath.Join(dir, "token-summary.json"), logger)

	// The collector's Summary() returns nil until scan runs, so this tests the nil branch
	ft := buildTokens(c)
	if ft.Totals.Input != 0 {
		t.Errorf("input = %d, want 0", ft.Totals.Input)
	}
}

func TestBuildTokens_WithSessionData(t *testing.T) {
	dir := t.TempDir()
	// Create a fake session JSONL file
	sessionData := `{"role":"user","message":"[agent:scanner] Fix bug","input_tokens":100,"output_tokens":50,"model":"sonnet"}
{"role":"assistant","message":"Fixed","input_tokens":200,"output_tokens":100,"model":"sonnet"}
`
	os.WriteFile(dir+"/session1.jsonl", []byte(sessionData), 0o644)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := tokens.NewCollectorWithPersistPath(dir, filepath.Join(dir, "token-summary.json"), logger)

	// Trigger a scan by calling Start with an immediate stop
	stop := make(chan struct{})
	go func() {
		close(stop)
	}()
	c.Start(stop)

	ft := buildTokens(c)
	if ft.Totals.Input <= 0 {
		t.Errorf("expected positive total input, got %d", ft.Totals.Input)
	}
	if len(ft.Sessions) != 0 {
		t.Errorf("sessions len = %d, want 0 (sessions excluded from status payload)", len(ft.Sessions))
	}
	if ft.Totals.Sessions != 1 {
		t.Errorf("totals.sessions = %d, want 1", ft.Totals.Sessions)
	}
	if len(ft.ByModel) == 0 {
		t.Error("expected non-empty ByModel")
	}
	if ft.Totals.Messages != 2 {
		t.Errorf("messages = %d, want 2", ft.Totals.Messages)
	}
}

func TestFormatCadenceDuration_Table(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{3600, "1h"},
		{7200, "2h"},
		{60, "1m"},
		{300, "5m"},
		{45, "45s"},
		{90, "90s"},
	}
	for _, tt := range tests {
		got := formatCadenceDuration(tt.seconds)
		if got != tt.want {
			t.Errorf("formatCadenceDuration(%d) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}

func TestLoadStatsConfig_MissingFile(t *testing.T) {
	result := loadStatsConfig("nonexistent-agent-xyz")
	if len(result) != 0 {
		t.Errorf("expected empty slice, got %d items", len(result))
	}
}

func TestBuildCadenceMatrix_PauseCadence(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle":  {Cadences: map[string]config.Cadence{"scanner": "pause"}},
				"quiet": {Cadences: map[string]config.Cadence{"scanner": ""}},
				"busy":  {Cadences: map[string]config.Cadence{}},
				"surge": {Cadences: map[string]config.Cadence{}},
			},
		},
	}
	statuses := map[string]*agent.AgentProcess{}
	matrix := buildCadenceMatrix(cfg, statuses)
	if len(matrix) != 1 {
		t.Fatalf("len = %d", len(matrix))
	}
	// "pause" cadence should be mapped to "off"
	if matrix[0].Idle != "off" {
		t.Errorf("idle = %q, want off", matrix[0].Idle)
	}
}

func TestBuildBudget_BurnRate(t *testing.T) {
	cfg := config.GovernorConfig{}
	gov := governor.New(cfg, map[string]config.AgentConfig{}, nil)
	gov.SetBudgetLimit(1000000)
	// Set reset time to 10 hours ago to trigger burn rate calculation
	gov.SetBudgetResetAt(time.Now().Add(-10 * time.Hour))
	gov.UpdateBudgetFromTotals(100000, nil, nil)

	fb := buildBudget(gov, nil)
	if fb.WeeklyBudget != 1000000 {
		t.Errorf("weekly = %d", fb.WeeklyBudget)
	}
	if fb.Used != 100000 {
		t.Errorf("used = %d, want 100000", fb.Used)
	}
	if fb.BurnRateHourly <= 0 {
		t.Errorf("burn rate hourly = %f, want > 0", fb.BurnRateHourly)
	}
	if fb.HoursElapsed < 9.9 {
		t.Errorf("hours elapsed = %f, want ~10", fb.HoursElapsed)
	}
	if fb.ProjectedWeekly <= 0 {
		t.Errorf("projected weekly = %d, want > 0", fb.ProjectedWeekly)
	}
	if fb.HoursRemaining <= 0 {
		t.Errorf("hours remaining = %f, want > 0", fb.HoursRemaining)
	}
}

func TestBuildBudget_WithTokenCollectorSummary(t *testing.T) {
	cfg := config.GovernorConfig{}
	gov := governor.New(cfg, map[string]config.AgentConfig{}, nil)
	// No weekly limit — should use totalTokens as used but no percentage calc
	collector := tokens.NewCollectorWithPersistPath("/nonexistent", filepath.Join(t.TempDir(), "token-summary.json"), nil)
	fb := buildBudget(gov, collector)
	// With no spend and no limit, used should be 0 or whatever the collector says
	if fb.PctUsed != 0 {
		t.Errorf("pctUsed = %f, want 0 (no limit)", fb.PctUsed)
	}
}

func TestBuildHealth_NilClient_NoCached(t *testing.T) {
	// Clear any cached state
	cachedHealthMu.Lock()
	cachedHealth = nil
	cachedHealthMu.Unlock()

	health := buildHealth(nil, nil)
	if health["ci"] != 100 {
		t.Errorf("ci = %v, want 100 (default fallback)", health["ci"])
	}
}

func TestBuildCadenceMatrix_PausedAgent(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle":  {Cadences: map[string]config.Cadence{"scanner": "15m"}},
				"quiet": {Cadences: map[string]config.Cadence{"scanner": "10m"}},
				"busy":  {Cadences: map[string]config.Cadence{"scanner": "5m"}},
				"surge": {Cadences: map[string]config.Cadence{"scanner": "2m"}},
			},
		},
	}
	statuses := map[string]*agent.AgentProcess{
		"scanner": {Paused: true},
	}
	matrix := buildCadenceMatrix(cfg, statuses)
	if len(matrix) != 1 {
		t.Fatalf("len = %d", len(matrix))
	}
	// All cadences should be "paused" for a paused agent
	if matrix[0].Idle != "paused" {
		t.Errorf("idle = %q, want paused", matrix[0].Idle)
	}
	if matrix[0].Surge != "paused" {
		t.Errorf("surge = %q, want paused", matrix[0].Surge)
	}
}

func TestLoadStatsConfig_NoFile(t *testing.T) {
	stats := loadStatsConfig("nonexistent-agent-xyz")
	if len(stats) != 0 {
		t.Errorf("expected empty stats for missing file, got %d", len(stats))
	}
}

func TestComputeNextKick_OffCadence(t *testing.T) {
	result := computeNextKick(nil, "off")
	if result != "" {
		t.Errorf("expected empty for off cadence, got %q", result)
	}
}

func TestComputeNextKick_PauseCadence(t *testing.T) {
	result := computeNextKick(nil, "pause")
	if result != "" {
		t.Errorf("expected empty for pause cadence, got %q", result)
	}
}

func TestComputeNextKick_EmptyCadence(t *testing.T) {
	result := computeNextKick(nil, "")
	if result != "" {
		t.Errorf("expected empty for empty cadence, got %q", result)
	}
}

func TestComputeNextKick_ValidCadenceWithLastKick(t *testing.T) {
	lastKick := time.Now().Add(-5 * time.Minute)
	result := computeNextKick(&lastKick, "10m")
	if result == "" {
		t.Error("expected non-empty result for valid cadence with last kick")
	}
}

func TestComputeNextKick_ValidCadenceNoLastKick(t *testing.T) {
	result := computeNextKick(nil, "15m")
	if result == "" {
		t.Error("expected non-empty result for valid cadence without last kick")
	}
}

func TestReadCgroupInt64_MaxReturnsNegOne(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/memory.max"
	os.WriteFile(path, []byte("max\n"), 0o644)
	result := readCgroupInt64(path)
	if result != -1 {
		t.Errorf("readCgroupInt64('max') = %d, want -1", result)
	}
}

func TestReadCgroupInt64_ValidNumber(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/memory.max"
	os.WriteFile(path, []byte("1073741824\n"), 0o644)
	result := readCgroupInt64(path)
	if result != 1073741824 {
		t.Errorf("readCgroupInt64 = %d, want 1073741824", result)
	}
}

func TestReadProcMemTotalBytes_ValidFile(t *testing.T) {
	// We can't easily mock procMeminfo path, but we can verify the function
	// returns a reasonable value on Linux or -1 on other platforms.
	result := readProcMemTotalBytes()
	// On macOS (test environment) this will be -1 since /proc/meminfo doesn't exist.
	// On Linux it should be > 0.
	if result == 0 {
		t.Error("readProcMemTotalBytes should never return 0 (either >0 or -1)")
	}
}

func TestCollectSystemResources_DoesNotPanic(t *testing.T) {
	// Ensure collectSystemResources doesn't panic on any platform.
	res := collectSystemResources()
	if res == nil {
		t.Fatal("expected non-nil SystemResources")
	}
	// CPU percentage should be in [0, 100] range (not 354%)
	if res.CpuPct < 0 || res.CpuPct > 100 {
		t.Errorf("CpuPct = %.1f, want [0, 100]", res.CpuPct)
	}
}

func TestCPUUsagePercent(t *testing.T) {
	tests := []struct {
		name      string
		deltaUsec int64
		elapsed   time.Duration
		cpuCount  int
		want      float64
	}{
		{name: "half of one CPU", deltaUsec: 100_000, elapsed: 200 * time.Millisecond, cpuCount: 1, want: 50},
		{name: "half of two CPUs", deltaUsec: 200_000, elapsed: 200 * time.Millisecond, cpuCount: 2, want: 50},
		{name: "uses measured elapsed time", deltaUsec: 201_200, elapsed: 202 * time.Millisecond, cpuCount: 1, want: 99.6},
		{name: "bounds accounting overshoot", deltaUsec: 201_200, elapsed: 200 * time.Millisecond, cpuCount: 1, want: 100},
		{name: "invalid CPU count", deltaUsec: 100_000, elapsed: 200 * time.Millisecond, cpuCount: 0, want: 0},
		{name: "invalid interval", deltaUsec: 100_000, elapsed: 0, cpuCount: 1, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cpuUsagePercent(tt.deltaUsec, tt.elapsed, tt.cpuCount); got != tt.want {
				t.Fatalf("cpuUsagePercent(%d, %s, %d) = %.1f, want %.1f", tt.deltaUsec, tt.elapsed, tt.cpuCount, got, tt.want)
			}
		})
	}
}
