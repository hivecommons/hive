package config

// EscalationConfig tunes the fix-loop circuit breaker (pkg/escalation): after
// Threshold distinct failed fix attempts on the same agent-authored PR, the
// hub stops dispatching further fixes and escalates to a human with the raw
// CI failure evidence.
type EscalationConfig struct {
	// Disabled turns the breaker off entirely (the zero value keeps it on —
	// escalation is a safety net and must be opt-out, not opt-in).
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// Threshold is the distinct-red-attempt count that triggers escalation.
	// Zero means DefaultEscalationThreshold.
	Threshold int `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	// Email configures outbound SMTP escalation mail. Default disabled.
	Email EscalationEmailConfig `yaml:"email,omitempty" json:"email,omitempty"`
	// Push configures outbound push/on-call sinks. Default disabled.
	Push EscalationPushConfig `yaml:"push,omitempty" json:"push,omitempty"`
}

type EscalationEmailConfig struct {
	Enabled bool                   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	SMTP    EscalationSMTPConfig   `yaml:"smtp,omitempty" json:"smtp,omitempty"`
	From    string                 `yaml:"from,omitempty" json:"from,omitempty"`
	To      []string               `yaml:"to,omitempty" json:"to,omitempty"`
	Digest  EscalationDigestConfig `yaml:"digest,omitempty" json:"digest,omitempty"`
}

type EscalationSMTPConfig struct {
	Host     string `yaml:"host,omitempty" json:"host,omitempty"`
	Port     int    `yaml:"port,omitempty" json:"port,omitempty"`
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
}

type EscalationDigestConfig struct {
	To []string `yaml:"to,omitempty" json:"to,omitempty"`
	At string   `yaml:"at,omitempty" json:"at,omitempty"`
}

type EscalationPushConfig struct {
	Enabled     bool                      `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	MinSeverity string                    `yaml:"min_severity,omitempty" json:"min_severity,omitempty"`
	Ntfy        EscalationNtfyConfig      `yaml:"ntfy,omitempty" json:"ntfy,omitempty"`
	Pushover    EscalationPushoverConfig  `yaml:"pushover,omitempty" json:"pushover,omitempty"`
	PagerDuty   EscalationPagerDutyConfig `yaml:"pagerduty,omitempty" json:"pagerduty,omitempty"`
}

type EscalationNtfyConfig struct {
	URL   string `yaml:"url,omitempty" json:"url,omitempty"`
	Token string `yaml:"token,omitempty" json:"token,omitempty"`
}

type EscalationPushoverConfig struct {
	AppToken string `yaml:"app_token,omitempty" json:"app_token,omitempty"`
	UserKey  string `yaml:"user_key,omitempty" json:"user_key,omitempty"`
}

type EscalationPagerDutyConfig struct {
	RoutingKey string `yaml:"routing_key,omitempty" json:"routing_key,omitempty"`
}
