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
	if err := (GitHubMentionsConfig{MinRole: "bogus"}).Validate(); err == nil {
		t.Fatal("invalid min_role accepted")
	}
	if err := (GitHubMentionsConfig{PollInterval: -time.Second}).Validate(); err == nil {
		t.Fatal("negative poll interval accepted")
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
