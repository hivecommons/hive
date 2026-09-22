package config

import (
	"strings"
	"testing"
	"time"
)

func TestGitHubMentionsConfigDefaultsAndValidation(t *testing.T) {
	var m GitHubMentionsConfig
	if m.MinRoleEffective() != RoleReadWrite {
		t.Fatalf("min role = %q", m.MinRoleEffective())
	}
	if m.PerUserPerHourEffective() != 6 || m.PerRepoPerHourEffective() != 30 {
		t.Fatalf("rate defaults = %d/%d", m.PerUserPerHourEffective(), m.PerRepoPerHourEffective())
	}
	if m.AckReactionEffective() != "eyes" {
		t.Fatalf("ack = %q", m.AckReactionEffective())
	}
	if m.PollIntervalEffective() != 5*time.Minute {
		t.Fatalf("poll = %v", m.PollIntervalEffective())
	}
	if m.WebhookMinGapEffective() != 30*time.Second {
		t.Fatalf("webhook min gap = %v", m.WebhookMinGapEffective())
	}
	t.Setenv("MENTION_WEBHOOK_SECRET", "secret")
	withWebhook := GitHubMentionsConfig{Enabled: true, WebhookEnabled: true, WebhookSecretEnv: "MENTION_WEBHOOK_SECRET", WebhookMinGap: time.Second}
	if withWebhook.WebhookSecretEffective() != "secret" || withWebhook.WebhookMinGapEffective() != time.Second {
		t.Fatalf("webhook effective values = %q/%v", withWebhook.WebhookSecretEffective(), withWebhook.WebhookMinGapEffective())
	}
	if err := (GitHubMentionsConfig{MinRole: "bogus"}).Validate(); err == nil {
		t.Fatal("invalid min_role accepted")
	}
	if err := (GitHubMentionsConfig{PollInterval: -time.Second}).Validate(); err == nil {
		t.Fatal("negative poll interval accepted")
	}
	if err := (GitHubMentionsConfig{WebhookEnabled: true}).Validate(); err == nil {
		t.Fatal("webhook without secret accepted")
	}
	if err := (GitHubMentionsConfig{WebhookEnabled: true, WebhookSecretEnv: "MENTION_WEBHOOK_SECRET"}).Validate(); err == nil {
		t.Fatal("webhook without poller accepted")
	}
	t.Setenv("EMPTY_MENTION_WEBHOOK_SECRET", "")
	if err := (GitHubMentionsConfig{Enabled: true, WebhookEnabled: true, WebhookSecretEnv: "EMPTY_MENTION_WEBHOOK_SECRET"}).Validate(); err == nil {
		t.Fatal("webhook with unset secret env accepted")
	}
	if err := (GitHubMentionsConfig{WebhookMinGap: -time.Second}).Validate(); err == nil {
		t.Fatal("negative webhook min gap accepted")
	}
}

func TestGitHubActionsConfigDefaultsAndValidation(t *testing.T) {
	var a GitHubActionsConfig
	if a.SourceLabelEffective() != DefaultGitHubActionSourceLabel {
		t.Fatalf("source label = %q", a.SourceLabelEffective())
	}
	if got := strings.Join(a.AllowedCommandsEffective(), ","); got != "status,review" {
		t.Fatalf("allowed command defaults = %q", got)
	}
	custom := GitHubActionsConfig{SourceLabel: "ci", AllowedCommands: []string{"status", "kick"}}
	if got := strings.Join(custom.AllowedCommandsEffective(), ","); got != "status,kick" {
		t.Fatalf("custom allowed commands = %q", got)
	}
	if err := custom.Validate(); err != nil {
		t.Fatalf("valid custom actions config rejected: %v", err)
	}
	if err := (GitHubActionsConfig{AllowedCommands: []string{"merge"}}).Validate(); err == nil {
		t.Fatal("unknown action command accepted")
	}
}

func TestValidateChannelsAcceptsMentionRuntime(t *testing.T) {
	if err := ValidateChannels("scanner", []ChannelConfig{{Type: ChannelTypeKick}, {Type: ChannelTypeMention}}); err != nil {
		t.Fatalf("mention+kick channel rejected: %v", err)
	}
	err := ValidateChannels("scanner", []ChannelConfig{{Type: ChannelTypeMention}})
	if err == nil || !strings.Contains(err.Error(), "must be paired") {
		t.Fatalf("mention-only error = %v, want paired error", err)
	}
	err = ValidateChannels("scanner", []ChannelConfig{{Type: "discord"}})
	if err == nil || !strings.Contains(err.Error(), ChannelTypeMention) {
		t.Fatalf("error = %v, want mention named as supported", err)
	}
}

func TestHasEnabledChannelHonorsDisabledMention(t *testing.T) {
	off := false
	a := AgentConfig{Channels: []ChannelConfig{{Type: ChannelTypeMention, Enabled: &off}}}
	if a.HasEnabledChannel(ChannelTypeMention) {
		t.Fatal("disabled mention channel reported enabled")
	}
}
