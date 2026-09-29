package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type GitHubMentionsConfig struct {
	Enabled          bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	PollInterval     time.Duration `yaml:"poll_interval,omitempty" json:"poll_interval,omitempty"`
	DefaultAgent     string        `yaml:"default_agent,omitempty" json:"default_agent,omitempty"`
	Summoners        []string      `yaml:"summoners,omitempty" json:"summoners,omitempty"`
	MinRole          string        `yaml:"min_role,omitempty" json:"min_role,omitempty"`
	PerUserPerHour   int           `yaml:"per_user_per_hour,omitempty" json:"per_user_per_hour,omitempty"`
	PerRepoPerHour   int           `yaml:"per_repo_per_hour,omitempty" json:"per_repo_per_hour,omitempty"`
	PerThreadMax     int           `yaml:"per_thread_max,omitempty" json:"per_thread_max,omitempty"`
	AckReaction      *string       `yaml:"ack_reaction,omitempty" json:"ack_reaction,omitempty"`
	WebhookEnabled   bool          `yaml:"webhook_enabled,omitempty" json:"webhook_enabled,omitempty"`
	WebhookSecretEnv string        `yaml:"webhook_secret_env,omitempty" json:"webhook_secret_env,omitempty"`
	WebhookMinGap    time.Duration `yaml:"webhook_min_gap,omitempty" json:"webhook_min_gap,omitempty"`
}

const (
	DefaultMentionPollInterval   = 5 * time.Minute
	DefaultMentionMinRole        = RoleReadWrite
	DefaultMentionPerUserPerHour = 6
	DefaultMentionPerRepoPerHour = 30
	DefaultMentionPerThreadMax   = 3
	DefaultMentionAckReaction    = "eyes"
	DefaultMentionWebhookMinGap  = 30 * time.Second
)

type GitHubActionsConfig struct {
	Enabled               bool                    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	SourceLabel           string                  `yaml:"source_label,omitempty" json:"source_label,omitempty"`
	AllowedCommands       []string                `yaml:"allowed_commands,omitempty" json:"allowed_commands,omitempty"`
	AllowApply            bool                    `yaml:"allow_apply,omitempty" json:"allow_apply,omitempty"`
	TrustedCommentAuthors []string                `yaml:"trusted_comment_authors,omitempty" json:"trusted_comment_authors,omitempty"`
	IdentityMap           map[string]string       `yaml:"identity_map,omitempty" json:"identity_map,omitempty"`
	OIDC                  GitHubActionsOIDCConfig `yaml:"oidc,omitempty" json:"oidc,omitempty"`
}

type GitHubActionsOIDCConfig struct {
	Enabled  bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Audience string        `yaml:"audience,omitempty" json:"audience,omitempty"`
	JWKSURL  string        `yaml:"jwks_url,omitempty" json:"jwks_url,omitempty"`
	MaxSkew  time.Duration `yaml:"max_skew,omitempty" json:"max_skew,omitempty"`
}

const (
	DefaultGitHubActionSourceLabel          = "action"
	DefaultGitHubActionTrustedCommentAuthor = "github-actions[bot]"
	DefaultGitHubActionsOIDCJWKSURL         = "https://token.actions.githubusercontent.com/.well-known/jwks"
	DefaultGitHubActionsOIDCMaxSkew         = 2 * time.Minute
)

var DefaultGitHubActionAllowedCommands = []string{"status", "review"}

func (a GitHubActionsConfig) SourceLabelEffective() string {
	if strings.TrimSpace(a.SourceLabel) != "" {
		return strings.TrimSpace(a.SourceLabel)
	}
	return DefaultGitHubActionSourceLabel
}

func (a GitHubActionsConfig) AllowedCommandsEffective() []string {
	if len(a.AllowedCommands) == 0 {
		return append([]string(nil), DefaultGitHubActionAllowedCommands...)
	}
	out := make([]string, 0, len(a.AllowedCommands))
	for _, cmd := range a.AllowedCommands {
		if trimmed := strings.ToLower(strings.TrimSpace(cmd)); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (a GitHubActionsConfig) TrustedCommentAuthorsEffective() []string {
	if len(a.TrustedCommentAuthors) == 0 {
		return []string{DefaultGitHubActionTrustedCommentAuthor}
	}
	out := make([]string, 0, len(a.TrustedCommentAuthors))
	for _, login := range a.TrustedCommentAuthors {
		if trimmed := strings.TrimSpace(login); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (a GitHubActionsConfig) Validate() error {
	if strings.TrimSpace(a.SourceLabelEffective()) == "" {
		return fmt.Errorf("github.actions.source_label must be non-empty")
	}
	valid := map[string]bool{"status": true, "review": true, "kick": true}
	for _, cmd := range a.AllowedCommandsEffective() {
		if !valid[cmd] {
			return fmt.Errorf("github.actions.allowed_commands contains unknown command %q", cmd)
		}
	}
	if err := a.OIDC.Validate(); err != nil {
		return err
	}
	return nil
}

func (o GitHubActionsOIDCConfig) JWKSURLEffective() string {
	if strings.TrimSpace(o.JWKSURL) != "" {
		return strings.TrimSpace(o.JWKSURL)
	}
	return DefaultGitHubActionsOIDCJWKSURL
}

func (o GitHubActionsOIDCConfig) MaxSkewEffective() time.Duration {
	if o.MaxSkew > 0 {
		return o.MaxSkew
	}
	return DefaultGitHubActionsOIDCMaxSkew
}

func (o GitHubActionsOIDCConfig) Validate() error {
	if o.Enabled && strings.TrimSpace(o.Audience) == "" {
		return fmt.Errorf("github.actions.oidc.audience is required when github.actions.oidc.enabled is true")
	}
	return nil
}

func (m GitHubMentionsConfig) PollIntervalEffective() time.Duration {
	if m.PollInterval > 0 {
		return m.PollInterval
	}
	return DefaultMentionPollInterval
}

func (m GitHubMentionsConfig) MinRoleEffective() string {
	if strings.TrimSpace(m.MinRole) != "" {
		return strings.TrimSpace(m.MinRole)
	}
	return DefaultMentionMinRole
}

func (m GitHubMentionsConfig) PerUserPerHourEffective() int {
	if m.PerUserPerHour > 0 {
		return m.PerUserPerHour
	}
	return DefaultMentionPerUserPerHour
}

func (m GitHubMentionsConfig) PerRepoPerHourEffective() int {
	if m.PerRepoPerHour > 0 {
		return m.PerRepoPerHour
	}
	return DefaultMentionPerRepoPerHour
}

// PerThreadMaxEffective returns github.mentions.per_thread_max: how many
// mention completion replies the hive posts on one issue or PR conversation.
// Only App-authored replies carrying the mention reply marker count; stage
// comments and other App comments on the conversation do not (#9164). It is
// deliberately independent of classification.review_bots.max_attempts_per_thread.
func (m GitHubMentionsConfig) PerThreadMaxEffective() int {
	if m.PerThreadMax > 0 {
		return m.PerThreadMax
	}
	return DefaultMentionPerThreadMax
}

func (m GitHubMentionsConfig) AckReactionEffective() string {
	if m.AckReaction == nil {
		return DefaultMentionAckReaction
	}
	return strings.TrimSpace(*m.AckReaction)
}

func (m GitHubMentionsConfig) WebhookSecretEffective() string {
	if strings.TrimSpace(m.WebhookSecretEnv) == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(strings.TrimSpace(m.WebhookSecretEnv)))
}

func (m GitHubMentionsConfig) WebhookMinGapEffective() time.Duration {
	if m.WebhookMinGap > 0 {
		return m.WebhookMinGap
	}
	return DefaultMentionWebhookMinGap
}

func (m GitHubMentionsConfig) Validate() error {
	if !ValidRole(m.MinRoleEffective()) {
		return fmt.Errorf("github.mentions.min_role %q is invalid (must be read, read-write, merger, or owner)", m.MinRole)
	}
	if m.PollInterval < 0 {
		return fmt.Errorf("github.mentions.poll_interval must be non-negative")
	}
	if m.PerUserPerHour < 0 || m.PerRepoPerHour < 0 || m.PerThreadMax < 0 {
		return fmt.Errorf("github.mentions rate limits must be non-negative")
	}
	if m.WebhookEnabled && strings.TrimSpace(m.WebhookSecretEnv) == "" {
		return fmt.Errorf("github.mentions.webhook_secret_env must be set when webhook_enabled is true")
	}
	if m.WebhookEnabled && m.WebhookSecretEffective() == "" {
		return fmt.Errorf("github.mentions.webhook_secret_env %q is empty or unset", strings.TrimSpace(m.WebhookSecretEnv))
	}
	if m.WebhookEnabled && !m.Enabled {
		return fmt.Errorf("github.mentions.webhook_enabled requires github.mentions.enabled")
	}
	if m.WebhookMinGap < 0 {
		return fmt.Errorf("github.mentions.webhook_min_gap must be non-negative")
	}
	return nil
}
