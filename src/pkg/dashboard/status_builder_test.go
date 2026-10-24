package dashboard

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/tokens"
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

func TestOperabilityAgentsHiddenFromDashboardBelowL5(t *testing.T) {
	level := 3
	cfg := &config.Config{
		ACMMLevel: &level,
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner":    {Backend: "copilot", Enabled: true},
				"telemetry":  {Backend: "copilot", Enabled: true},
				"operations": {Backend: "copilot", Enabled: true},
			},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {
				Cadences: map[string]config.Cadence{
					"scanner":    "4h",
					"telemetry":  "4h",
					"operations": "4h",
				},
			},
		}},
	}
	statuses := map[string]*crux.Process{
		"scanner":    {Name: "scanner", Config: cfg.Agents["scanner"]},
		"telemetry":  {Name: "telemetry", Config: cfg.Agents["telemetry"]},
		"operations": {Name: "operations", Config: cfg.Agents["operations"]},
	}

	if got := agentNamesFromConfigured(buildConfiguredAgents(cfg)); containsAny(got, "telemetry", "operations") {
		t.Fatalf("configured agents below L5 = %v, want no telemetry/operations", got)
	}
	if got := agentNamesFromFrontend(buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle})); containsAny(got, "telemetry", "operations") {
		t.Fatalf("frontend agents below L5 = %v, want no telemetry/operations", got)
	}
	if got := agentNamesFromCadence(buildCadenceMatrix(cfg, statuses, "surge")); containsAny(got, "telemetry", "operations") {
		t.Fatalf("cadence matrix below L5 = %v, want no telemetry/operations", got)
	}
}

func agentNamesFromConfigured(in []FrontendConfiguredAgent) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.Name)
	}
	return out
}

func agentNamesFromFrontend(in []FrontendAgent) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.Name)
	}
	return out
}

func agentNamesFromCadence(in []FrontendCadence) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.Agent)
	}
	return out
}

func containsAny(names []string, needles ...string) bool {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	for _, needle := range needles {
		if seen[needle] {
			return true
		}
	}
	return false
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

func TestBuildGovernorNextRunAnchorsToLastEval(t *testing.T) {
	lastEval := time.Date(2026, 9, 25, 16, 10, 0, 0, time.UTC)
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			EvalIntervalS: 60,
			Modes:         map[string]config.ModeConfig{},
		},
	}
	state := governor.State{Mode: governor.ModeIdle, LastEval: lastEval}
	result := buildGovernor(state, cfg)

	wantAt := "2026-09-25T16:11:00Z"
	if result.NextKickAt != wantAt {
		t.Fatalf("nextKickAt = %q, want %q", result.NextKickAt, wantAt)
	}
	if result.NextKick == "" {
		t.Fatal("nextKick should remain populated for clients that render the absolute timestamp")
	}
	if result.NextKickIn == "" {
		t.Fatal("nextKickIn should be populated so clients can render due/ETA honestly")
	}
}

func TestBuildGovernorNextRunUsesEffectiveEvalInterval(t *testing.T) {
	lastEval := time.Date(2026, 9, 25, 16, 10, 0, 0, time.UTC)
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			EvalIntervalS:              120,
			EvalIntervalMaxS:           1800,
			ConserveIntervalMultiplier: 3,
			Modes:                      map[string]config.ModeConfig{},
		},
	}
	state := governor.State{Mode: governor.ModeIdle, LastEval: lastEval}

	conserve := buildGovernor(state, cfg, map[string]any{"mode": "conserve"})
	if want := "2026-09-25T16:16:00Z"; conserve.NextKickAt != want {
		t.Fatalf("conserve nextKickAt = %q, want %q", conserve.NextKickAt, want)
	}

	critical := buildGovernor(state, cfg, map[string]any{"mode": "critical"})
	if want := "2026-09-25T16:18:00Z"; critical.NextKickAt != want {
		t.Fatalf("critical nextKickAt = %q, want %q", critical.NextKickAt, want)
	}

	cfg.Governor.EvalIntervalMaxS = 300
	capped := buildGovernor(state, cfg, map[string]any{"mode": "critical"})
	if want := "2026-09-25T16:15:00Z"; capped.NextKickAt != want {
		t.Fatalf("capped critical nextKickAt = %q, want %q", capped.NextKickAt, want)
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
	prUpdatedAt := time.Date(2026, 9, 21, 10, 11, 12, 0, time.UTC)
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
				{Repo: "repo2", Number: 10, Title: "pr1", UpdatedAt: prUpdatedAt},
			},
			StaleDrafts: []github.PullRequest{
				{Repo: "repo2", Number: 11, Title: "draft", Draft: true, UpdatedAt: prUpdatedAt.Add(time.Hour)},
			},
		},
		TotalByRepo: map[string]github.RepoCounts{
			"repo1": {Issues: 1, PRs: 0},
			"repo2": {Issues: 0, PRs: 1},
		},
		WorkBreakdownByRepo: map[string]github.RepoWorkBreakdown{
			"repo1": {Issues: github.RepoIssueBreakdown{Actionable: 1}},
			"repo2": {PRs: github.RepoPRBreakdown{Draft: 1}},
		},
	}

	repos := buildRepos(cfg, actionable, governor.State{RepoModes: map[string]governor.Mode{"repo1": governor.ModeSurge}})
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
	if repos[0].WorkBreakdown == nil || repos[0].WorkBreakdown.Issues.Actionable != 1 {
		t.Errorf("issue breakdown = %+v", repos[0].WorkBreakdown)
	}
	if repos[1].WorkBreakdown == nil || repos[1].WorkBreakdown.PRs.Draft != 1 {
		t.Errorf("PR breakdown = %+v", repos[1].WorkBreakdown)
	}
	if repos[0].Mode != "surge" {
		t.Errorf("mode = %q, want surge", repos[0].Mode)
	}
	if len(repos[1].OpenPrs) != 2 {
		t.Fatalf("open PRs = %d, want 2", len(repos[1].OpenPrs))
	}
	body, err := json.Marshal(repos[1].OpenPrs[0])
	if err != nil {
		t.Fatalf("marshal frontend PR: %v", err)
	}
	if !strings.Contains(string(body), `"updated_at":"2026-09-21T10:11:12Z"`) {
		t.Fatalf("frontend PR payload missing updated_at: %s", body)
	}
}

func TestBuildRepos_NilActionable(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}},
	}
	repos := buildRepos(cfg, nil, governor.State{})
	if len(repos) != 1 {
		t.Fatalf("repos len = %d", len(repos))
	}
	if repos[0].Issues != 0 {
		t.Errorf("issues = %d", repos[0].Issues)
	}
	if repos[0].WorkBreakdown != nil {
		t.Errorf("work breakdown = %+v, want nil when no classified scan exists", repos[0].WorkBreakdown)
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
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner":    {Backend: "claude"},
				"supervisor": {Backend: "claude"},
			},
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
	statuses := map[string]*crux.Process{
		"scanner":    {Paused: false},
		"supervisor": {Paused: true},
	}
	matrix := buildCadenceMatrix(cfg, statuses, "surge")
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

	statuses := map[string]*crux.Process{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude", Model: "sonnet"},
			State:        crux.StateRunning,
			LastKick:     &now,
			OutputBuffer: buf,
		},
		"supervisor": {
			Name:         "supervisor",
			Config:       config.AgentConfig{Backend: "claude", Model: "opus"},
			State:        crux.StatePaused,
			Paused:       true,
			PinnedCLI:    "claude",
			PinnedModel:  "opus",
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}

	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner":    {Backend: "claude", Model: "sonnet", SortOrder: 20},
				"supervisor": {Backend: "claude", Model: "opus", BeadRole: "supervisor", SortOrder: 10},
			},
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

func TestBuildAgents_ShowsGatewayNamedConfiguredAgentWithoutRuntimeStatus(t *testing.T) {
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"supervisor": {
					ID:          "supervisor",
					Backend:     "unsloth-local",
					Model:       "unsloth/gemma-4-E4B-it-qat-GGUF",
					Enabled:     true,
					DisplayName: "Supervisor",
					SortOrder:   10,
				},
			},
		},
		Governor: config.GovernorConfig{
			AgentRuntimeGovernorConfig: config.AgentRuntimeGovernorConfig{
				Gateways: []config.GatewayConfig{{
					Name:         "unsloth-local",
					Kind:         config.GatewayKindCustom,
					Endpoint:     "http://host.docker.internal:8888",
					DefaultModel: "unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF",
				}},
			},
		},
	}

	agents := buildAgents(map[string]*crux.Process{}, cfg, governor.State{Mode: governor.ModeIdle})
	if len(agents) != 1 {
		t.Fatalf("agents len = %d, want 1 for enabled agent using configured gateway backend", len(agents))
	}
	if agents[0].Name != "supervisor" {
		t.Fatalf("agent name = %q, want supervisor", agents[0].Name)
	}
	if agents[0].CLI != "unsloth-local" {
		t.Errorf("agent CLI = %q, want configured gateway backend", agents[0].CLI)
	}
	if agents[0].StructuredStatus != "BLOCKED" {
		t.Errorf("structured status = %q, want BLOCKED to make the missing runtime process visible", agents[0].StructuredStatus)
	}
}

// TestBuildAgents_OffByCadence verifies that an agent whose cadence for the
// current governor mode is a non-kicking value ("pause"/"off") is flagged
// OffByCadence, while a normally-scheduled agent is not. This is the signal the
// dashboard uses to paint a hollow-green (alive-but-governor-off) dot for hives
// stuck in a mode where every cadence is paused (e.g. SURGE).
func TestBuildAgents_OffByCadence(t *testing.T) {
	statuses := map[string]*crux.Process{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude"},
			State:        crux.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
		"quality": {
			Name:         "quality",
			Config:       config.AgentConfig{Backend: "claude"},
			State:        crux.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
		"brainstorm": {
			Name:         "brainstorm",
			Config:       config.AgentConfig{Backend: "claude", OnDemand: true},
			State:        crux.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner":    {Backend: "claude", SortOrder: 10},
				"quality":    {Backend: "claude", SortOrder: 20},
				"brainstorm": {Backend: "claude", OnDemand: true, SortOrder: 30},
			},
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
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Backend: "claude", Model: "sonnet"},
			},
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

	statuses := map[string]*crux.Process{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude", Model: "sonnet"},
			State:        crux.StateRunning,
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
	if payload.Features.ExperimentLab || payload.Features.StrategyLab {
		t.Error("features.experiment_lab/strategy_lab = true by default, want false")
	}

	cfg.Dashboard.ExperimentLab = true
	payload = BuildFrontendStatus(govState, nil, statuses, cfg, nil, gov, nil, nil, nil, nil)
	if !payload.Features.ExperimentLab {
		t.Error("features.experiment_lab = false with dashboard.experiment_lab enabled, want true")
	}
	if !payload.Features.StrategyLab {
		t.Error("features.strategy_lab alias = false with dashboard.experiment_lab enabled, want true for the one-release alias window (#11695)")
	}

	// The deprecated dashboard.strategy_lab key still lights the feature when
	// a config bypasses applyDefaults.
	cfg.Dashboard.ExperimentLab = false
	cfg.Dashboard.StrategyLab = true
	payload = BuildFrontendStatus(govState, nil, statuses, cfg, nil, gov, nil, nil, nil, nil)
	if !payload.Features.ExperimentLab {
		t.Error("features.experiment_lab = false with deprecated dashboard.strategy_lab enabled, want true")
	}
}

func TestBuildFrontendStatus_AgentAuthHealthFailsOnProviderAuthBlock(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Backend: "copilot", Model: "gpt-5", Enabled: true},
				"quality": {Backend: "copilot", Model: "gpt-5", Enabled: true},
			},
		},
		GitHub: config.GitHubConfig{Token: "tok"},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	statuses := map[string]*crux.Process{
		"scanner": {
			Name:                      "scanner",
			Config:                    cfg.Agents["scanner"],
			State:                     crux.StateRunning,
			OutputBuffer:              agent.NewRingBuffer(10),
			ProviderErrorClass:        "auth",
			ProviderErrorLine:         "✗ You are not licensed to use Copilot.",
			ProviderErrorBackoffUntil: time.Now().Add(time.Minute),
		},
		"quality": {
			Name:         "quality",
			Config:       cfg.Agents["quality"],
			State:        crux.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}

	payload := BuildFrontendStatus(gov.GetState(), nil, statuses, cfg, nil, gov, nil, nil, nil, nil)
	if got := payload.Health["agent_auth"]; got != 0 {
		t.Fatalf("health.agent_auth = %v, want 0 while a provider auth block is active", got)
	}
}

func TestBuildFrontendStatus_BackendAuthErrorBlocksWithoutLogin(t *testing.T) {
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Backend: "claude", Model: "claude-sonnet", Enabled: true},
			},
		},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	statuses := map[string]*crux.Process{
		"scanner": {
			Name:                      "scanner",
			Config:                    cfg.Agents["scanner"],
			State:                     crux.StateRunning,
			OutputBuffer:              agent.NewRingBuffer(10),
			ProviderErrorClass:        "backend_auth_error",
			ProviderErrorLine:         "Inference backend outage (401): Error querying the database: FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute",
			ProviderErrorBackoffUntil: time.Now().Add(time.Minute),
			BackendAuth: crux.BackendAuthState{
				Status:    crux.BackendAuthUnreachable,
				Since:     time.Now(),
				LastError: "Inference backend outage (401): Error querying the database",
			},
		},
	}

	payload := BuildFrontendStatus(gov.GetState(), nil, statuses, cfg, nil, gov, nil, nil, nil, nil)
	a := payload.Agents[0]
	if a.NeedsLogin {
		t.Fatal("backend_auth_error must not surface as an interactive login wait")
	}
	if a.StructuredStatus != "BLOCKED" || a.Condition != "ProviderError" {
		t.Fatalf("status/condition = %q/%q, want BLOCKED/ProviderError", a.StructuredStatus, a.Condition)
	}
	if !strings.Contains(a.StatusEvidence, "backend_auth_error") || !strings.Contains(a.StatusEvidence, "Inference backend outage") {
		t.Fatalf("StatusEvidence = %q, want backend auth outage detail", a.StatusEvidence)
	}
	if a.BackendAuthStatus != crux.BackendAuthUnreachable || a.BackendAuthLastError == "" {
		t.Fatalf("backend auth fields = %q/%q, want unreachable with detail", a.BackendAuthStatus, a.BackendAuthLastError)
	}
}

func TestBuildFrontendStatus_AgentAuthHealthRecoversWhenProviderBackoffExpires(t *testing.T) {
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Backend: "copilot", Model: "gpt-5", Enabled: true},
			},
		},
		GitHub: config.GitHubConfig{Token: "tok"},
	}
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	statuses := map[string]*crux.Process{
		"scanner": {
			Name:                      "scanner",
			Config:                    cfg.Agents["scanner"],
			State:                     crux.StateRunning,
			OutputBuffer:              agent.NewRingBuffer(10),
			ProviderErrorClass:        "auth",
			ProviderErrorLine:         "old auth error",
			ProviderErrorBackoffUntil: time.Now().Add(-time.Minute),
		},
	}

	payload := BuildFrontendStatus(gov.GetState(), nil, statuses, cfg, nil, gov, nil, nil, nil, nil)
	if got := payload.Health["agent_auth"]; got != 1 {
		t.Fatalf("health.agent_auth = %v, want 1 after provider auth backoff expires", got)
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

func TestBuildHealth_IgnoresCachedWorkflowHealth(t *testing.T) {
	// Legacy workflow-health cache entries must not leak into the dashboard
	// health payload now that the navbar and diagnostics use real spoke health
	// plus quality-agent stats instead.
	setCachedHealth(t, map[string]any{"ci": 95, "tests": 100})

	health := buildHealth(nil, nil)
	if len(health) != 0 {
		t.Errorf("health = %v, want empty workflow-health payload", health)
	}
}

func TestBuildAgents_WithOverrides(t *testing.T) {
	statuses := map[string]*crux.Process{
		"scanner": {
			Name:            "scanner",
			Config:          config.AgentConfig{Backend: "claude", Model: "sonnet"},
			State:           crux.StateRunning,
			BackendOverride: "aider",
			ModelOverride:   "opus",
			OutputBuffer:    agent.NewRingBuffer(10),
		},
	}
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Backend: "claude", Model: "sonnet"},
			},
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
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {},
			},
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
	statuses := map[string]*crux.Process{
		"scanner": {Paused: true},
	}
	matrix := buildCadenceMatrix(cfg, statuses, "surge")
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
	repos := buildRepos(cfg, actionable, governor.State{})
	if len(repos) != 1 {
		t.Fatalf("len = %d", len(repos))
	}
	// The card counts the items it can list (the canonical open-items set
	// the tile, pill and activity monitor share); what TotalByRepo reported
	// beyond them is kept apart as unlisted, never folded into the total
	// (hivecommons/hive#13373).
	if repos[0].Issues != 1 || repos[0].UnlistedIssues != 4 {
		t.Errorf("issues = %d unlisted %d, want 1 listed + 4 unlisted", repos[0].Issues, repos[0].UnlistedIssues)
	}
	if repos[0].PRs != 1 || repos[0].UnlistedPRs != 2 {
		t.Errorf("prs = %d unlisted %d, want 1 listed + 2 unlisted", repos[0].PRs, repos[0].UnlistedPRs)
	}
}

func TestBuildReposGroupsFullRepoWorksourceItemsUnderBareConfigRepo(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "clubanderson", Repos: []string{"hive-runs-e2e"}},
	}
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{{
			Repo:       "clubanderson/hive-runs-e2e",
			SourceType: "run",
			ExternalID: "crustify-fixture:parse-ast",
			Title:      "implement: Port the AST parser to Rust",
			Labels:     []string{"wavefront"},
		}}},
	}
	repos := buildRepos(cfg, actionable, governor.State{})
	if len(repos) != 1 {
		t.Fatalf("len = %d", len(repos))
	}
	if len(repos[0].ActionableIssues) != 1 {
		t.Fatalf("actionable issues = %+v", repos[0].ActionableIssues)
	}
}

func TestBuildFrontendStatus_WithMetrics(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"scanner": {Backend: "claude"}},
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
	statuses := map[string]*crux.Process{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude"},
			State:        crux.StateRunning,
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

func TestBuildTokens_NilSummary(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := tokens.NewCollector(dir, logger)
	// Redirect the persisted-snapshot path away from the live
	// /data/token-summary.json BEFORE anything reads Summary(): the snapshot
	// restore is lazy (first read, #4585), so the redirect fully isolates
	// this test from a hive host's production state and the nil branch is
	// always testable.
	c.SetPersistPath(filepath.Join(t.TempDir(), "token-summary.json"))

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
	c := tokens.NewCollector(dir, logger)

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
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {},
			},
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
	statuses := map[string]*crux.Process{}
	matrix := buildCadenceMatrix(cfg, statuses, "surge")
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

// TestBuildBudget_HonorsConfiguredPeriod pins #4762: the governor already
// honored period_days, but the dashboard independently added the seven-day
// fallback to ResetAt and projected over 168 hours. A two-day budget therefore
// rendered as a one-week period even though enforcement rolled after two days.
func TestBuildBudget_HonorsConfiguredPeriod(t *testing.T) {
	const periodDays = 2
	cfg := config.GovernorConfig{Budget: config.BudgetConfig{PeriodDays: periodDays}}
	gov := governor.New(cfg, map[string]config.AgentConfig{}, nil)
	gov.SetBudgetLimit(10000)
	resetAt := time.Now().Add(-12 * time.Hour)
	gov.SetBudgetResetAt(resetAt)
	gov.UpdateBudgetFromTotals(1200, nil, nil)

	fb := buildBudget(gov, nil)
	start, err := time.Parse(time.RFC3339, fb.WindowStartsAt)
	if err != nil {
		t.Fatalf("parse window start %q: %v", fb.WindowStartsAt, err)
	}
	end, err := time.Parse(time.RFC3339, fb.WindowEndsAt)
	if err != nil {
		t.Fatalf("parse window end %q: %v", fb.WindowEndsAt, err)
	}
	if got, want := end.Sub(start), periodDays*24*time.Hour; got != want {
		t.Fatalf("dashboard budget period = %v, want configured %v", got, want)
	}
	if fb.WindowHoursRemaining < 35.9 || fb.WindowHoursRemaining > 36.1 {
		t.Errorf("window hours remaining = %.2f, want about 36", fb.WindowHoursRemaining)
	}
	// 1,200 tokens in 12 hours projects to about 4,800 over 48 hours,
	// not the old seven-day projection of about 16,800.
	if fb.ProjectedWeekly < 4790 || fb.ProjectedWeekly > 4810 {
		t.Errorf("period projection = %d, want about 4800", fb.ProjectedWeekly)
	}
}

func TestBuildBudget_WithTokenCollectorSummary(t *testing.T) {
	cfg := config.GovernorConfig{}
	gov := governor.New(cfg, map[string]config.AgentConfig{}, nil)
	// No weekly limit — should use totalTokens as used but no percentage calc.
	// Real logger + redirected persist path: the collector's lazy snapshot
	// restore would otherwise read the live /data/token-summary.json on a hive
	// host and, before NewCollector was nil-safe, panic on the nil logger (#4664).
	collector := tokens.NewCollector("/nonexistent", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	collector.SetPersistPath(filepath.Join(t.TempDir(), "token-summary.json"))
	fb := buildBudget(gov, collector)
	// With no spend and no limit, used should be 0 or whatever the collector says
	if fb.PctUsed != 0 {
		t.Errorf("pctUsed = %f, want 0 (no limit)", fb.PctUsed)
	}
}

func TestBuildBudget_USDOnlyBudget(t *testing.T) {
	gov := governor.New(config.GovernorConfig{}, map[string]config.AgentConfig{}, nil)
	gov.SetUSDBudget(5)
	gov.SeedBudget(0, nil, nil, time.Now())
	gov.UpdateBudgetFromTotalsCoinsAndUSD(0, nil, nil, 0, 6)

	fb := buildBudget(gov, nil)
	if !fb.Exhausted || fb.ExhaustedUnit != "usd" {
		t.Fatalf("budget exhausted=%v unit=%q, want usd exhaustion", fb.Exhausted, fb.ExhaustedUnit)
	}
	if fb.USDBudget != 5 || fb.USDUsed != 6 || fb.USDRemaining != 0 || fb.PctUsed != 120 {
		t.Fatalf("usd budget fields = %+v", fb)
	}
}

func TestBuildHealth_NilClient_NoCached(t *testing.T) {
	// Clear any cached state for the duration of this test only; the shared
	// hook restores whatever was cached before, so this test cannot erase
	// another test's fixture under -shuffle (#5570).
	setCachedHealth(t, nil)

	health := buildHealth(nil, nil)
	if len(health) != 0 {
		t.Errorf("health = %v, want empty workflow-health payload", health)
	}
}

func TestBuildCadenceMatrix_PausedAgent(t *testing.T) {
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {},
			},
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
	statuses := map[string]*crux.Process{
		"scanner": {Paused: true},
	}
	matrix := buildCadenceMatrix(cfg, statuses, "surge")
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

func TestBuildPlatform_NilConfigIsZeroValue(t *testing.T) {
	p := buildPlatform(nil)
	if p == nil {
		t.Fatal("expected non-nil FrontendPlatform for nil config")
	}
	if p.Forge.Kind != config.ForgeGitHub {
		t.Errorf("nil config forge kind = %q, want %q", p.Forge.Kind, config.ForgeGitHub)
	}
	if p.Forge.Repos == nil {
		t.Error("Forge.Repos must be non-nil (empty slice) so JSON is [] not null")
	}
	if p.Mint.Enabled {
		t.Error("nil config must report mint disabled")
	}
	if p.Skills.Available {
		t.Error("nil config must report skills unavailable")
	}
}

func TestBuildPlatform_ForgeMintSkills(t *testing.T) {
	// A mint key that exists on disk → KeyPresent true; a self-managed GitLab
	// instance URL is surfaced; skills stay unavailable (no /data/skills dir).
	dir := t.TempDir()
	keyPath := dir + "/mint.pem"
	if err := os.WriteFile(keyPath, []byte("-----BEGIN KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Project.Forge = config.ForgeGitLab
	cfg.Project.Repos = []string{"org/repo-a", "org/repo-b"}
	cfg.Project.PrimaryRepo = "org/repo-a"
	cfg.GitLab.URL = "https://gitlab.example.com"
	cfg.Mint.Enabled = true
	cfg.Mint.Issuer = "https://mint.example.com"
	cfg.Mint.KeyPath = keyPath

	p := buildPlatform(cfg)

	if p.Forge.Kind != config.ForgeGitLab {
		t.Errorf("forge kind = %q, want %q", p.Forge.Kind, config.ForgeGitLab)
	}
	if p.Forge.InstanceURL != "https://gitlab.example.com" {
		t.Errorf("instance URL = %q, want self-managed gitlab url", p.Forge.InstanceURL)
	}
	if p.Forge.RepoCount != 2 {
		t.Errorf("repo count = %d, want 2", p.Forge.RepoCount)
	}
	if p.Forge.PrimaryRepo != "org/repo-a" {
		t.Errorf("primary repo = %q, want org/repo-a", p.Forge.PrimaryRepo)
	}
	if !p.Mint.Enabled {
		t.Error("mint should be enabled")
	}
	if p.Mint.Issuer != "https://mint.example.com" {
		t.Errorf("mint issuer = %q, want https://mint.example.com", p.Mint.Issuer)
	}
	if !p.Mint.KeyPresent {
		t.Error("mint key file exists on disk, KeyPresent should be true")
	}
	if p.Skills.Available {
		t.Error("skills should be unavailable when no skills dir is present")
	}
	if p.Skills.Loaded != 0 {
		t.Errorf("skills loaded = %d, want 0 when unavailable", p.Skills.Loaded)
	}
}

func TestBuildPlatform_DefaultGitLabURLNotSurfaced(t *testing.T) {
	// Default gitlab.com SaaS instance → InstanceURL stays empty (only
	// self-managed instances are meaningful to show).
	cfg := &config.Config{}
	cfg.Project.Forge = config.ForgeGitLab
	p := buildPlatform(cfg)
	if p.Forge.InstanceURL != "" {
		t.Errorf("default gitlab.com should not surface instance URL, got %q", p.Forge.InstanceURL)
	}
	// Mint key path unset → KeyPresent false, never a panic on os.Stat("").
	if p.Mint.KeyPresent {
		t.Error("unset mint key path must report KeyPresent=false")
	}
}

func testBoolPtr(v bool) *bool { return &v }

func TestBuildSecurity(t *testing.T) {
	tests := []struct {
		name              string
		cfg               *config.Config
		wantTotal         int
		wantSandboxed     int
		wantReviewCapable int
	}{
		{
			name: "nil config",
		},
		{
			name: "empty agents map",
			cfg:  &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{}}},
		},
		{
			name: "all sandboxed",
			cfg: &config.Config{
				AgentRuntimeConfig: config.AgentRuntimeConfig{
					AgentSandbox: config.AgentSandboxConfig{Enabled: true},
					Agents: map[string]config.AgentConfig{
						"scanner": {Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: testBoolPtr(true)}},
						"planner": {Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: testBoolPtr(true)}},
					},
				},
			},
			wantTotal:     2,
			wantSandboxed: 2,
		},
		{
			name: "none sandboxed",
			cfg: &config.Config{
				AgentRuntimeConfig: config.AgentRuntimeConfig{
					AgentSandbox: config.AgentSandboxConfig{Enabled: true},
					Agents: map[string]config.AgentConfig{
						"scanner": {Enabled: true},
						"planner": {Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: testBoolPtr(false)}},
					},
				},
			},
			wantTotal: 2,
		},
		{
			name: "mixed sandbox and review capabilities",
			cfg: &config.Config{
				AgentRuntimeConfig: config.AgentRuntimeConfig{
					AgentSandbox: config.AgentSandboxConfig{Enabled: true},
					Agents: map[string]config.AgentConfig{
						"sandboxed-reviewer": {Enabled: true, Role: "code reviewer", Sandbox: &config.AgentSandboxOverride{Enabled: testBoolPtr(true)}},
						"explicit-reviewer":  {Enabled: true},
						"sandboxed-planner":  {Enabled: true, Role: "planner", Sandbox: &config.AgentSandboxOverride{Enabled: testBoolPtr(true)}},
						"disabled-reviewer":  {Enabled: false, Role: "reviewer", Sandbox: &config.AgentSandboxOverride{Enabled: testBoolPtr(true)}},
					},
				},
				Review: config.ReviewConfig{ReviewerAgents: []string{"explicit-reviewer"}},
			},
			wantTotal:         4,
			wantSandboxed:     3,
			wantReviewCapable: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSecurity(tt.cfg)
			if got == nil {
				t.Fatal("buildSecurity returned nil")
			}
			if got.TotalAgents != tt.wantTotal {
				t.Errorf("TotalAgents = %d, want %d", got.TotalAgents, tt.wantTotal)
			}
			if got.SandboxedAgents != tt.wantSandboxed {
				t.Errorf("SandboxedAgents = %d, want %d", got.SandboxedAgents, tt.wantSandboxed)
			}
			if got.ReviewCapableAgents != tt.wantReviewCapable {
				t.Errorf("ReviewCapableAgents = %d, want %d", got.ReviewCapableAgents, tt.wantReviewCapable)
			}
		})
	}
}

func TestDashboardAgentReviewCapable(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		cfg     config.AgentConfig
		allowed []string
		want    bool
	}{
		{
			name:    "explicit allow list inclusion",
			agent:   "security-reviewer",
			cfg:     config.AgentConfig{Enabled: true},
			allowed: []string{"security-reviewer", "auditor"},
			want:    true,
		},
		{
			name:    "explicit allow list exclusion beats keyword fallback",
			agent:   "security-reviewer",
			cfg:     config.AgentConfig{Enabled: true, Role: "reviewer"},
			allowed: []string{"auditor"},
		},
		{
			name:  "fallback role keyword",
			agent: "worker",
			cfg:   config.AgentConfig{Enabled: true, Role: "PR reviewer"},
			want:  true,
		},
		{
			name:  "fallback alias keyword",
			agent: "worker",
			cfg:   config.AgentConfig{Enabled: true, Aliases: []string{"code-review-bot"}},
			want:  true,
		},
		{
			name:  "fallback lane keyword",
			agent: "worker",
			cfg:   config.AgentConfig{Enabled: true, LaneKeywords: []string{"needs-review"}},
			want:  true,
		},
		{
			name:  "fallback detect keyword",
			agent: "worker",
			cfg:   config.AgentConfig{Enabled: true, DetectKeywords: []string{"review-requested"}},
			want:  true,
		},
		{
			name:    "replica of allowed reviewer is review capable",
			agent:   "security-reviewer-2",
			cfg:     config.AgentConfig{Enabled: true, ReplicaOf: "security-reviewer"},
			allowed: []string{"security-reviewer"},
			want:    true,
		},
		{
			name:    "replica of unlisted base is not review capable",
			agent:   "worker-2",
			cfg:     config.AgentConfig{Enabled: true, ReplicaOf: "worker"},
			allowed: []string{"security-reviewer"},
		},
		{
			name:  "paused agent is never review capable",
			agent: "security-reviewer",
			cfg:   config.AgentConfig{Enabled: true, Paused: true, Role: "reviewer"},
		},
		{
			name:  "on demand agent is never review capable",
			agent: "security-reviewer",
			cfg:   config.AgentConfig{Enabled: true, OnDemand: true, Role: "reviewer"},
		},
		{
			name:  "disabled agent is never review capable",
			agent: "security-reviewer",
			cfg:   config.AgentConfig{Enabled: false, Role: "reviewer"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dashboardAgentReviewCapable(tt.agent, tt.cfg, tt.allowed); got != tt.want {
				t.Errorf("dashboardAgentReviewCapable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildAgents_RedactsLastErrorAndPausedReason(t *testing.T) {
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		Agents: map[string]config.AgentConfig{
			"scanner": {ID: "scanner", Backend: "claude", Enabled: true},
		},
	}}
	statuses := map[string]*crux.Process{
		"scanner": {
			Name:         "scanner",
			Config:       config.AgentConfig{Backend: "claude"},
			State:        crux.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
			LastError:    "copilot hung: your one-time code: ABCD-1234 | key sk-ant-abcdef123456",
			PausedReason: "restart storm: ghp_abcdefghijklmnop1234 sk-ant-abcdef123456",
		},
	}
	agents := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle})
	if len(agents) != 1 {
		t.Fatalf("agents len = %d, want 1", len(agents))
	}
	got := agents[0].LastError + "\n" + agents[0].PausedReason
	for _, secret := range []string{"ABCD-1234", "sk-ant-abcdef123456", "ghp_abcdefghijklmnop1234"} {
		if strings.Contains(got, secret) {
			t.Fatalf("status payload leaked %q: %q", secret, got)
		}
	}
	if agents[0].LastError == "" || agents[0].PausedReason == "" {
		t.Fatalf("lastError/pausedReason should remain populated: %q / %q", agents[0].LastError, agents[0].PausedReason)
	}
}

// A repo served from its last-good enumeration after a transient GitHub
// refusal keeps its counts on the card, is flagged stale, and keeps the
// Overview KPI snapshot out of history (hosted hive, 2026-10-21).
func TestBuildRepos_StaleRepoKeepsCountsAndMarksKPIIncomplete(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1", "repo2"}},
	}
	// The last-good substitution keeps the repo's whole enumeration — its
	// work-detail rows included — so the items are there to count.
	details := func(repo string, issues, prs int) github.RepoWorkDetails {
		var d github.RepoWorkDetails
		for n := 1; n <= issues; n++ {
			d.Issues = append(d.Issues, github.RepoWorkIssue{Repo: repo, Number: n, Bucket: "filtered"})
		}
		for n := 1; n <= prs; n++ {
			d.PRs = append(d.PRs, github.RepoWorkPR{Repo: repo, Number: 100 + n, Bucket: "draft", Draft: true})
		}
		return d
	}
	actionable := &github.ActionableResult{
		TotalByRepo: map[string]github.RepoCounts{
			"repo1": {Issues: 19, PRs: 2},
			"repo2": {Issues: 1, PRs: 0},
		},
		WorkDetailsByRepo: map[string]github.RepoWorkDetails{
			"repo1": details("repo1", 19, 2),
			"repo2": details("repo2", 1, 0),
		},
		StaleRepos: []string{"repo1"},
		StaleSince: time.Date(2026, 10, 21, 14, 7, 0, 0, time.UTC),
	}
	repos := buildRepos(cfg, actionable, governor.State{})
	if len(repos) != 2 {
		t.Fatalf("repos len = %d, want 2", len(repos))
	}
	if repos[0].CountsStaleSince != "2026-10-21T14:07:00Z" || repos[1].CountsStaleSince != "" {
		t.Fatalf("stale-since markers = %q / %q", repos[0].CountsStaleSince, repos[1].CountsStaleSince)
	}
	if got := staleSinceRFC3339(actionable); got != "2026-10-21T14:07:00Z" {
		t.Fatalf("staleSinceRFC3339 = %q", got)
	}
	if repos[0].Issues != 19 || repos[0].PRs != 2 {
		t.Fatalf("stale repo counts = %d/%d, want last-good 19/2", repos[0].Issues, repos[0].PRs)
	}
	if !repos[0].CountsStale || repos[0].CountsIncomplete {
		t.Fatalf("repo1 stale=%v incomplete=%v, want stale only", repos[0].CountsStale, repos[0].CountsIncomplete)
	}
	if repos[1].CountsStale {
		t.Fatal("fresh repo2 must not be marked stale")
	}
	if !overviewKPIIncomplete(cfg, actionable) {
		t.Fatal("a snapshot with a stale repo must be KPI-incomplete so history skips it")
	}
	actionable.StaleRepos = nil
	if overviewKPIIncomplete(cfg, actionable) {
		t.Fatal("a fully fresh snapshot must be KPI-complete")
	}
	if staleSinceRFC3339(actionable) != "" {
		t.Fatal("fresh snapshot must carry no stale-since marker")
	}
}

// A whole-pass failure republishes the current status as stale-as-of
// instead of leaving figures that look fresh (or dropping to zero).
func TestMarkStatusStaleRepublishesLastGoodWithMarker(t *testing.T) {
	s := &Server{}
	s.MarkStatusStale(time.Now()) // no status yet: no-op
	s.status = &StatusPayload{Repos: []FrontendRepo{
		{Name: "repo1", Issues: 19, PRs: 2},
		{Name: "repo2", Issues: 1, CountsStale: true, CountsStaleSince: "2026-10-21T13:00:00Z"},
	}}
	since := time.Date(2026, 10, 21, 14, 7, 0, 0, time.UTC)
	s.MarkStatusStale(since)
	got := s.status
	if got.StaleSince != "2026-10-21T14:07:00Z" || !got.OverviewKPIIncomplete {
		t.Fatalf("status = since:%q incomplete:%v", got.StaleSince, got.OverviewKPIIncomplete)
	}
	if got.Repos[0].Issues != 19 || !got.Repos[0].CountsStale || got.Repos[0].CountsStaleSince != got.StaleSince {
		t.Fatalf("repo1 = %+v, want last-good counts marked stale", got.Repos[0])
	}
	if got.Repos[1].CountsStaleSince != "2026-10-21T13:00:00Z" {
		t.Fatal("an already-stale repo keeps its own, earlier marker")
	}
	s.MarkStatusStale(since)
	if s.status != got {
		t.Fatal("same marker must not republish")
	}
}
