package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/snapshot"
)

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// applyConfigOverrides must replay every persisted override field onto the
// loaded config. The sensing_login branch is covered separately in
// login_patterns_overrides_test.go; this exercises everything else.
func TestApplyConfigOverrides_ReplaysAllFields(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			EvalIntervalS: 60,
			Modes: map[string]config.ModeConfig{
				"active": {Threshold: 1},
			},
		},
	}
	applyConfigOverrides(cfg, &snapshot.ConfigOverrides{
		ProjectRepos:        []string{"org/repo-a", "org/repo-b"},
		EvalIntervalS:       intPtr(120),
		Thresholds:          map[string]int{"active": 7, "unknown-mode": 99},
		SensingGHRate:       []string{"rate limit hit"},
		SensingCLIExclude:   []string{"harmless"},
		SensingTTL:          intPtr(300),
		SensingPullback:     intPtr(45),
		ExemptLabels:        []string{"urgent", "security"},
		NtfyServer:          "https://ntfy.example",
		NtfyTopic:           "hive-alerts",
		DiscordWebhook:      "https://discord.example/webhook",
		HealthcheckInterval: intPtr(240),
		RestartCooldown:     intPtr(90),
		ModelLock:           boolPtr(true),
		LogMaxSizeMB:        intPtr(64),
		LogMaxAgeDays:       intPtr(14),
		LogMaxBackups:       intPtr(5),
		LogCompress:         boolPtr(true),
		LogLevel:            "debug",
	})

	if got := cfg.Project.Repos; len(got) != 2 || got[0] != "org/repo-a" || got[1] != "org/repo-b" {
		t.Errorf("Project.Repos = %v, want the override list", got)
	}
	if cfg.Governor.EvalIntervalS != 120 {
		t.Errorf("EvalIntervalS = %d, want 120", cfg.Governor.EvalIntervalS)
	}
	if got := cfg.Governor.Modes["active"].Threshold; got != 7 {
		t.Errorf("Modes[active].Threshold = %d, want 7", got)
	}
	if _, ok := cfg.Governor.Modes["unknown-mode"]; ok {
		t.Error("a threshold for an unconfigured mode must not invent the mode")
	}
	if got := cfg.Governor.Sensing.GHRatePatterns; len(got) != 1 || got[0] != "rate limit hit" {
		t.Errorf("Sensing.GHRatePatterns = %v", got)
	}
	if got := cfg.Governor.Sensing.CLIExcludePatterns; len(got) != 1 || got[0] != "harmless" {
		t.Errorf("Sensing.CLIExcludePatterns = %v", got)
	}
	if cfg.Governor.Sensing.TTLSeconds != 300 {
		t.Errorf("Sensing.TTLSeconds = %d, want 300", cfg.Governor.Sensing.TTLSeconds)
	}
	if cfg.Governor.Sensing.PullbackSeconds != 45 {
		t.Errorf("Sensing.PullbackSeconds = %d, want 45", cfg.Governor.Sensing.PullbackSeconds)
	}
	if got := cfg.Governor.Labels.Exempt; len(got) != 2 || got[0] != "urgent" || got[1] != "security" {
		t.Errorf("Labels.Exempt = %v", got)
	}
	if cfg.Notifications.Ntfy == nil {
		t.Fatal("Ntfy overrides must create the Ntfy config when absent")
	}
	if cfg.Notifications.Ntfy.Server != "https://ntfy.example" || cfg.Notifications.Ntfy.Topic != "hive-alerts" {
		t.Errorf("Ntfy = %+v", cfg.Notifications.Ntfy)
	}
	if cfg.Notifications.Discord == nil {
		t.Fatal("Discord webhook override must create the Discord config when absent")
	}
	if cfg.Notifications.Discord.Webhook != "https://discord.example/webhook" {
		t.Errorf("Discord.Webhook = %q", cfg.Notifications.Discord.Webhook)
	}
	if cfg.Governor.Health.HealthcheckInterval != 240 {
		t.Errorf("Health.HealthcheckInterval = %d, want 240", cfg.Governor.Health.HealthcheckInterval)
	}
	if cfg.Governor.Health.RestartCooldown != 90 {
		t.Errorf("Health.RestartCooldown = %d, want 90", cfg.Governor.Health.RestartCooldown)
	}
	if !cfg.Governor.Health.ModelLock {
		t.Error("Health.ModelLock not applied")
	}
	if cfg.Governor.Logging.MaxSizeMB != 64 || cfg.Governor.Logging.MaxAgeDays != 14 ||
		cfg.Governor.Logging.MaxBackups != 5 || !cfg.Governor.Logging.Compress ||
		cfg.Governor.Logging.Level != "debug" {
		t.Errorf("Logging = %+v", cfg.Governor.Logging)
	}
}

// Empty overrides carry no operator intent and must leave the loaded config
// untouched — including NOT materializing notification configs.
func TestApplyConfigOverrides_EmptyOverridesAreNoOp(t *testing.T) {
	cfg := &config.Config{
		Governor: config.GovernorConfig{
			EvalIntervalS: 60,
			Modes: map[string]config.ModeConfig{
				"active": {Threshold: 3},
			},
			Sensing: config.SensingConfig{
				TTLSeconds:      100,
				PullbackSeconds: 20,
			},
		},
	}
	cfg.Project.Repos = []string{"org/keep"}

	applyConfigOverrides(cfg, &snapshot.ConfigOverrides{})

	if len(cfg.Project.Repos) != 1 || cfg.Project.Repos[0] != "org/keep" {
		t.Errorf("Project.Repos mutated: %v", cfg.Project.Repos)
	}
	if cfg.Governor.EvalIntervalS != 60 {
		t.Errorf("EvalIntervalS mutated: %d", cfg.Governor.EvalIntervalS)
	}
	if cfg.Governor.Modes["active"].Threshold != 3 {
		t.Errorf("threshold mutated: %d", cfg.Governor.Modes["active"].Threshold)
	}
	if cfg.Governor.Sensing.TTLSeconds != 100 || cfg.Governor.Sensing.PullbackSeconds != 20 {
		t.Errorf("sensing mutated: %+v", cfg.Governor.Sensing)
	}
	if cfg.Notifications.Ntfy != nil {
		t.Error("empty overrides must not materialize an Ntfy config")
	}
	if cfg.Notifications.Discord != nil {
		t.Error("empty overrides must not materialize a Discord config")
	}
}

// A partial ntfy override (topic only) must update just that field on an
// existing config, not blank its sibling.
func TestApplyConfigOverrides_PartialNtfyKeepsExistingServer(t *testing.T) {
	cfg := &config.Config{}
	cfg.Notifications.Ntfy = &config.NtfyConfig{Server: "https://keep.example", Topic: "old"}

	applyConfigOverrides(cfg, &snapshot.ConfigOverrides{NtfyTopic: "new-topic"})

	if cfg.Notifications.Ntfy.Server != "https://keep.example" {
		t.Errorf("Server blanked by topic-only override: %q", cfg.Notifications.Ntfy.Server)
	}
	if cfg.Notifications.Ntfy.Topic != "new-topic" {
		t.Errorf("Topic = %q, want new-topic", cfg.Notifications.Ntfy.Topic)
	}
}

// providerLimitHeartbeatFields must prefer the proxy's spending-limit latch
// over pane-derived quota, with rebuff-count-aware phrasing.
func TestProviderLimitHeartbeatFields_SingleRebuffPhrasing(t *testing.T) {
	dashboard.SetInferenceBudgetProvider(func() (string, time.Time, time.Time, int) {
		return "credit balance too low", time.Now(), time.Now(), 1
	})
	t.Cleanup(func() { dashboard.SetInferenceBudgetProvider(nil) })

	reason, rebuffs, hiveWide, names := providerLimitHeartbeatFields([]hub.AgentSummary{
		{Name: "guide", State: "running", QuotaExhausted: true},
	})
	if rebuffs != 1 {
		t.Fatalf("rebuffs = %d, want 1", rebuffs)
	}
	if !hiveWide || len(names) != 0 {
		t.Fatalf("hiveWide/names = %v/%v, want hive-wide provider latch", hiveWide, names)
	}
	want := "provider spending limit reached — credit balance too low"
	if reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

func TestProviderLimitHeartbeatFields_MultiRebuffPhrasing(t *testing.T) {
	dashboard.SetInferenceBudgetProvider(func() (string, time.Time, time.Time, int) {
		return "credit balance too low", time.Now(), time.Now(), 4
	})
	t.Cleanup(func() { dashboard.SetInferenceBudgetProvider(nil) })

	reason, rebuffs, hiveWide, names := providerLimitHeartbeatFields(nil)
	if rebuffs != 4 {
		t.Fatalf("rebuffs = %d, want 4", rebuffs)
	}
	if !hiveWide || len(names) != 0 {
		t.Fatalf("hiveWide/names = %v/%v, want hive-wide provider latch", hiveWide, names)
	}
	want := "provider spending limit reached — 4 refused calls: credit balance too low"
	if reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

// actionableIssueRef's identityless fallbacks: when worksource.Ref.Key()
// yields no identity, the ref must fall back to the bare repo, then the
// external ID, without ever fabricating "#0". (The repo#N and repo!externalID
// key paths are covered by TestActionableIssueRefPinsGitHubAndWorksourceIdentity.)
func TestActionableIssueRef_IdentitylessFallbacks(t *testing.T) {
	cases := []struct {
		name  string
		issue github.Issue
		want  string
	}{
		{"repo only", github.Issue{Repo: "org/repo"}, "org/repo"},
		{"external id only", github.Issue{ExternalID: "ENG-9"}, "ENG-9"},
		{"empty issue", github.Issue{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := actionableIssueRef(tc.issue); got != tc.want {
				t.Errorf("actionableIssueRef(%+v) = %q, want %q", tc.issue, got, tc.want)
			}
		})
	}
}
