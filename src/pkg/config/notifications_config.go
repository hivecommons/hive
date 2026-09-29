package config

type NotificationsConfig struct {
	Ntfy           *NtfyConfig           `yaml:"ntfy,omitempty"`
	Slack          *SlackConfig          `yaml:"slack,omitempty"`
	Matrix         *MatrixConfig         `yaml:"matrix,omitempty"`
	Telegram       *TelegramConfig       `yaml:"telegram,omitempty"`
	MSTeams        *MSTeamsConfig        `yaml:"msteams,omitempty"`
	Discord        *DiscordConfig        `yaml:"discord,omitempty"`
	GitHubActivity *GitHubActivityConfig `yaml:"github_activity,omitempty"`
	Events         []string              `yaml:"events,omitempty" json:"events,omitempty"`
}

type NtfyConfig struct {
	Server string `yaml:"server"`
	Topic  string `yaml:"topic"`
}

type SlackConfig struct {
	Webhook   string `yaml:"webhook"`
	Enabled   bool   `yaml:"enabled"`
	AppToken  string `yaml:"app_token"`
	BotToken  string `yaml:"bot_token"`
	ChannelID string `yaml:"channel_id"`
	// AllowedUsers is an allowlist of Slack user IDs permitted to issue bot
	// COMMANDS (!kick, !pause, agent actions — anything that drives an agent).
	// SECURITY: without it, any member of the channel who can post in the channel
	// can inject prompts into the agents. When empty, command handling is
	// DISABLED (fail closed) — the bot still posts status but accepts no
	// commands — so an operator must opt in by listing the trusted user IDs.
	// Entries may be "id" or "id:role"; the first bare entry is treated as
	// owner and later bare entries are treated as read.
	AllowedUsers []string `yaml:"allowed_users,omitempty"`
}

type TelegramConfig struct {
	Enabled  bool   `yaml:"enabled"`
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
	// AllowedUsers is an allowlist of Telegram user IDs permitted to issue bot
	// COMMANDS (!kick, !pause, agent actions — anything that drives an agent).
	// SECURITY: without it, any member of the chat who can post in the channel
	// can inject prompts into the agents. When empty, command handling is
	// DISABLED (fail closed) — the bot still posts status but accepts no
	// commands — so an operator must opt in by listing the trusted user IDs.
	// Entries may be "id" or "id:role"; the first bare entry is treated as
	// owner and later bare entries are treated as read.
	AllowedUsers []string `yaml:"allowed_users,omitempty"`
}

type MatrixConfig struct {
	Enabled       bool   `yaml:"enabled"`
	HomeserverURL string `yaml:"homeserver_url"`
	AccessToken   string `yaml:"access_token"`
	RoomID        string `yaml:"room_id"`
	// AllowedUsers is an allowlist of full Matrix user IDs permitted to issue bot
	// COMMANDS (!kick, !pause, agent actions — anything that drives an agent).
	// SECURITY: without it, any member of the room who can post in the room can
	// inject prompts into the agents. When empty, command handling is DISABLED
	// (fail closed) by the chat spine, so an operator must opt in by listing the
	// trusted MXIDs.
	// Entries may be "id" or "id:role"; the first bare entry is treated as
	// owner and later bare entries are treated as read.
	AllowedUsers []string `yaml:"allowed_users,omitempty"`
}

type MSTeamsConfig struct {
	Enabled      bool   `yaml:"enabled"`
	TenantID     string `yaml:"tenant_id"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	TeamID       string `yaml:"team_id"`
	ChannelID    string `yaml:"channel_id"`
	// WebhookURL is the outbound URL of a Teams Workflows (Power Automate)
	// "Post to a channel when a webhook request is received" flow targeting
	// channel_id; Hive posts Adaptive Card message envelopes to it. Legacy
	// Office 365 Incoming Webhook connector URLs stopped working when Microsoft
	// retired connectors in May 2026 and are not supported.
	WebhookURL string `yaml:"webhook_url"`
	// AllowedUsers is an allowlist of Azure AD user object IDs permitted to issue
	// bot COMMANDS (!kick, !pause, agent actions — anything that drives an
	// agent). SECURITY: without it, any channel member who can post in the channel
	// can inject prompts into the agents. When empty, command handling is
	// DISABLED (fail closed) — the bot still posts status but accepts no commands
	// — so an operator must opt in by listing the trusted AAD object IDs.
	// Entries may be "id" or "id:role"; the first bare entry is treated as
	// owner and later bare entries are treated as read.
	AllowedUsers []string `yaml:"allowed_users,omitempty"`
}

type DiscordConfig struct {
	Webhook        string `yaml:"webhook"`
	FactoryWebhook string `yaml:"factory_webhook,omitempty"`
	BotToken       string `yaml:"bot_token"`
	ChannelID      string `yaml:"channel_id"`
	// AllowedUsers is an allowlist of Discord user IDs permitted to issue bot
	// COMMANDS (!kick, !pause, agent actions — anything that drives an agent).
	// SECURITY: without it, any member of the guild who can post in the channel
	// can inject prompts into the agents. When empty, command handling is
	// DISABLED (fail closed) — the bot still posts status but accepts no
	// commands — so an operator must opt in by listing the trusted user IDs.
	// Entries may be "id" or "id:role"; the first bare entry is treated as
	// owner and later bare entries are treated as read.
	AllowedUsers []string `yaml:"allowed_users,omitempty"`
}
