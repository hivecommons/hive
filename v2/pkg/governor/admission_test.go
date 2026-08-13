package governor

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestAdmitWorkUsesNormalGovernorStateAndRecordsReasons(t *testing.T) {
	governor := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"quality": "1m"}},
	}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := WorkAdmissionRequest{
		SourceExternalRef: "visual-hive://owner/repo/" + strings.Repeat("a", 64), PacketDigest: strings.Repeat("b", 64),
		FindingFingerprint: strings.Repeat("c", 64), RepositoryFingerprint: strings.Repeat("a", 64), Role: "quality",
		RoutingReason: "verified route", RoutingAllowed: true, AuthorityAllowsWork: true, SafeExecutionReady: true,
		ActiveWIP: 0, WIPLimit: 1, Evidence: visualhive.SpecialistEvidenceIdentity{VerificationReceiptSHA256: strings.Repeat("d", 64)},
	}
	if decision := governor.AdmitWork(request); !decision.Allowed || decision.Code != "allowed" {
		t.Fatalf("allow decision = %+v", decision)
	}
	request.ActiveWIP = 1
	if decision := governor.AdmitWork(request); decision.Allowed || decision.Code != "wip_limit" {
		t.Fatalf("WIP decision = %+v", decision)
	}
	governor.SetBudgetLimit(1)
	governor.SeedBudget(1, nil, nil, governor.GetBudget().ResetAt)
	request.ActiveWIP = 0
	if decision := governor.AdmitWork(request); decision.Allowed || decision.Code != "budget_exhausted" {
		t.Fatalf("budget decision = %+v", decision)
	}
	if history := governor.AdmissionHistory(); len(history) != 3 || history[0].Code != "allowed" || history[2].Code != "budget_exhausted" {
		t.Fatalf("admission history = %+v", history)
	}
}

func TestBindAdmissionRequestClearsCadenceWhenCurrentModeIsUnconfigured(t *testing.T) {
	gov := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"quality": "1m"}},
	}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admitted := gov.BindAdmissionRequest(WorkAdmissionRequest{Role: "quality"})
	if admitted.ConfiguredCadence != "1m" {
		t.Fatalf("initial cadence = %q, want 1m", admitted.ConfiguredCadence)
	}

	gov.SetMode(ModeBusy)
	current := gov.BindAdmissionRequest(admitted)
	if current.GovernorMode != ModeBusy || current.ConfiguredCadence != "" {
		t.Fatalf("unconfigured current mode retained stale admission policy: %+v", current)
	}
}

func TestGovernorAgentSnapshotAndAdmissionDigestAreDeeplyImmutable(t *testing.T) {
	enabled, include := true, true
	agents := map[string]config.AgentConfig{"quality": {
		Enabled: true, Role: "quality", IncludeRepos: &include,
		Aliases: []string{"test-adequacy"}, LaneKeywords: []string{"tests"}, DetectKeywords: []string{"coverage"}, ACMMLevels: []int{3, 4, 5},
		Channels:     []config.ChannelConfig{{Type: "bead", Enabled: &enabled, Events: []string{"ready"}, Patterns: []string{"visual-*"}, Match: map[string]string{"kind": "visual"}, Repos: []string{"owner/repo"}}},
		Tools:        &config.ToolsConfig{Preset: "repair", Rules: []config.ToolRule{{Pattern: "go test", Action: "allow"}}},
		Connections:  []config.ConnectionConfig{{Name: "evidence", Type: "filesystem", Auth: &config.ConnectionAuth{Type: "env", EnvVar: "TOKEN"}, Options: map[string]string{"root": "evidence"}}},
		StatsDisplay: []config.StatsDisplayEntry{{Key: "visual", Label: "Visual"}},
	}}
	gov := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]config.Cadence{"quality": "1m"}}}}, agents, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := WorkAdmissionRequest{Role: "quality"}
	before := gov.BindAdmissionRequest(request)
	*agents["quality"].IncludeRepos = false
	agents["quality"].Aliases[0] = "tampered"
	agents["quality"].LaneKeywords[0] = "tampered"
	agents["quality"].DetectKeywords[0] = "tampered"
	agents["quality"].ACMMLevels[0] = 6
	*agents["quality"].Channels[0].Enabled = false
	agents["quality"].Channels[0].Events[0] = "tampered"
	agents["quality"].Channels[0].Patterns[0] = "tampered"
	agents["quality"].Channels[0].Match["kind"] = "tampered"
	agents["quality"].Channels[0].Repos[0] = "other/repo"
	agents["quality"].Tools.Rules[0].Action = "deny"
	agents["quality"].Connections[0].Auth.EnvVar = "OTHER"
	agents["quality"].Connections[0].Options["root"] = "tampered"
	agents["quality"].StatsDisplay[0].Label = "Tampered"
	afterMutation := gov.BindAdmissionRequest(request)
	if before.RoleConfigSHA256 == "" || before.RoleConfigSHA256 != afterMutation.RoleConfigSHA256 || !afterMutation.RoleConfigured {
		t.Fatalf("caller mutation changed Governor snapshot: before=%+v after=%+v", before, afterMutation)
	}
	gov.UpdateAgents(agents)
	afterUpdate := gov.BindAdmissionRequest(request)
	if afterUpdate.RoleConfigSHA256 == before.RoleConfigSHA256 || !afterUpdate.RoleConfigured {
		t.Fatalf("explicit agent snapshot update was not reflected: before=%+v after=%+v", before, afterUpdate)
	}
	*agents["quality"].IncludeRepos = true
	agents["quality"].Aliases[0] = "mutated-again"
	agents["quality"].LaneKeywords[0] = "mutated-again"
	agents["quality"].DetectKeywords[0] = "mutated-again"
	agents["quality"].ACMMLevels[0] = 2
	*agents["quality"].Channels[0].Enabled = true
	agents["quality"].Channels[0].Events[0] = "mutated-again"
	agents["quality"].Channels[0].Patterns[0] = "mutated-again"
	agents["quality"].Channels[0].Match["kind"] = "mutated-again"
	agents["quality"].Channels[0].Repos[0] = "third/repo"
	agents["quality"].Tools.Rules[0].Action = "allow"
	agents["quality"].Connections[0].Auth.EnvVar = "THIRD"
	agents["quality"].Connections[0].Options["root"] = "mutated-again"
	agents["quality"].StatsDisplay[0].Label = "Mutated Again"
	afterSecondCallerMutation := gov.BindAdmissionRequest(request)
	if afterSecondCallerMutation.RoleConfigSHA256 != afterUpdate.RoleConfigSHA256 || afterSecondCallerMutation.RoleConfigured != afterUpdate.RoleConfigured {
		t.Fatalf("caller mutation after UpdateAgents changed Governor snapshot: updated=%+v mutated=%+v", afterUpdate, afterSecondCallerMutation)
	}
	gov.UpdateConfigAndAgents(config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]config.Cadence{"quality": "2m"}}}}, agents)
	afterAtomicUpdate := gov.BindAdmissionRequest(request)
	if afterAtomicUpdate.RoleConfigSHA256 == afterUpdate.RoleConfigSHA256 || afterAtomicUpdate.ConfiguredCadence != "2m" {
		t.Fatalf("atomic config and agent snapshot update was not reflected: before=%+v after=%+v", afterUpdate, afterAtomicUpdate)
	}
	agents["quality"].Aliases[0] = "third-mutation"
	agents["quality"].LaneKeywords[0] = "third-mutation"
	agents["quality"].DetectKeywords[0] = "third-mutation"
	agents["quality"].ACMMLevels[0] = 1
	agents["quality"].Tools.Rules[0].Action = "deny"
	if afterThirdCallerMutation := gov.BindAdmissionRequest(request); afterThirdCallerMutation.RoleConfigSHA256 != afterAtomicUpdate.RoleConfigSHA256 || afterThirdCallerMutation.ConfiguredCadence != "2m" {
		t.Fatalf("caller mutation after UpdateConfigAndAgents changed Governor snapshot: updated=%+v mutated=%+v", afterAtomicUpdate, afterThirdCallerMutation)
	}
}

func TestGovernorConfigSnapshotIsDeeplyImmutableAtEveryBoundary(t *testing.T) {
	newConfig := func(prefix, cadence string) config.GovernorConfig {
		return config.GovernorConfig{
			Modes:  map[string]config.ModeConfig{"idle": {Threshold: 1, Cadences: map[string]config.Cadence{"quality": config.Cadence(cadence)}}},
			Labels: config.LabelsConfig{Exempt: []string{prefix + "-label"}},
			Sensing: config.SensingConfig{
				GHRatePatterns: []string{prefix + "-rate"}, CLIExcludePatterns: []string{prefix + "-cli"}, LoginPatterns: []string{prefix + "-login"},
			},
		}
	}
	mutate := func(cfg *config.GovernorConfig) {
		mode := cfg.Modes["idle"]
		mode.Threshold = 99
		mode.Cadences["quality"] = "tampered"
		cfg.Modes["idle"] = mode
		cfg.Labels.Exempt[0] = "tampered"
		cfg.Sensing.GHRatePatterns[0] = "tampered"
		cfg.Sensing.CLIExcludePatterns[0] = "tampered"
		cfg.Sensing.LoginPatterns[0] = "tampered"
	}
	assertSnapshot := func(t *testing.T, gov *Governor, prefix, cadence string) {
		t.Helper()
		bound := gov.BindAdmissionRequest(WorkAdmissionRequest{Role: "quality"})
		gov.mu.RLock()
		defer gov.mu.RUnlock()
		mode := gov.cfg.Modes["idle"]
		if bound.ConfiguredCadence != cadence || mode.Threshold != 1 || mode.Cadences["quality"] != config.Cadence(cadence) ||
			len(gov.cfg.Labels.Exempt) != 1 || gov.cfg.Labels.Exempt[0] != prefix+"-label" ||
			len(gov.cfg.Sensing.GHRatePatterns) != 1 || gov.cfg.Sensing.GHRatePatterns[0] != prefix+"-rate" ||
			len(gov.cfg.Sensing.CLIExcludePatterns) != 1 || gov.cfg.Sensing.CLIExcludePatterns[0] != prefix+"-cli" ||
			len(gov.cfg.Sensing.LoginPatterns) != 1 || gov.cfg.Sensing.LoginPatterns[0] != prefix+"-login" {
			t.Fatalf("Governor config snapshot changed through caller aliases: bound=%+v cfg=%+v", bound, gov.cfg)
		}
	}

	initial := newConfig("initial", "1m")
	gov := New(initial, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mutate(&initial)
	assertSnapshot(t, gov, "initial", "1m")

	updated := newConfig("updated", "2m")
	gov.UpdateConfig(updated)
	mutate(&updated)
	assertSnapshot(t, gov, "updated", "2m")

	atomic := newConfig("atomic", "3m")
	gov.UpdateConfigAndAgents(atomic, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}})
	mutate(&atomic)
	assertSnapshot(t, gov, "atomic", "3m")
}
