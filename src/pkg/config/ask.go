package config

import (
	"os"
	"strconv"
	"strings"
)

// Ask AI help (#19177, phase 1 #19180). Off by default and inert: nothing in
// this file opens a route, makes a request or changes a header unless the
// operator switches it on AND supplies every value the vendor call needs.
// See src/docs/design/dashboard-ask-ai-help.md.
const (
	// EnvAskEnabled overrides ask.enabled when it holds a recognised boolean.
	EnvAskEnabled = "HIVE_ASK_ENABLED"
	// EnvAskMode overrides ask.mode (off, direct or hub).
	EnvAskMode = "HIVE_ASK_MODE"
	// EnvAskKapaProjectID overrides ask.kapa.project_id.
	EnvAskKapaProjectID = "HIVE_ASK_KAPA_PROJECT_ID"
	// EnvAskKapaIntegrationID overrides ask.kapa.integration_id.
	EnvAskKapaIntegrationID = "HIVE_ASK_KAPA_INTEGRATION_ID"
	// EnvAskKapaAPIKeyFile overrides ask.kapa.api_key_file. There is no env
	// var that holds the key itself, by design.
	EnvAskKapaAPIKeyFile = "HIVE_ASK_KAPA_API_KEY_FILE"
	// EnvAskSendSectionLabel overrides ask.send_section_label.
	EnvAskSendSectionLabel = "HIVE_ASK_SEND_SECTION_LABEL"

	EnvAskLimitPersonBurst     = "HIVE_ASK_LIMIT_PERSON_BURST"
	EnvAskLimitPersonPerHour   = "HIVE_ASK_LIMIT_PERSON_PER_HOUR"
	EnvAskLimitHivePerHour     = "HIVE_ASK_LIMIT_HIVE_PER_HOUR"
	EnvAskLimitGlobalPerMinute = "HIVE_ASK_LIMIT_GLOBAL_PER_MINUTE"
	EnvAskLimitInFlight        = "HIVE_ASK_LIMIT_IN_FLIGHT"
	EnvAskLimitDaily           = "HIVE_ASK_LIMIT_DAILY"
	EnvAskLimitQuestionChars   = "HIVE_ASK_LIMIT_QUESTION_CHARS"
	EnvAskLimitAnswerChars     = "HIVE_ASK_LIMIT_ANSWER_CHARS"
	EnvAskLimitTimeoutSeconds  = "HIVE_ASK_LIMIT_TIMEOUT_SECONDS"
)

// Ask modes.
const (
	AskModeOff    = "off"
	AskModeDirect = "direct"
	AskModeHub    = "hub"
)

// Default Ask limits. Every number is a proposal from the design record, not
// a measurement; each is overridable by config and env. The hub-wide numbers
// sit well under the vendor's 20-per-minute per-team chat limit, which the
// docs site's own widget shares.
const (
	DefaultAskPersonBurst     = 3
	DefaultAskPersonPerHour   = 30
	DefaultAskHivePerHour     = 120
	DefaultAskGlobalPerMinute = 8
	DefaultAskInFlight        = 4
	DefaultAskDaily           = 500
	DefaultAskQuestionChars   = 1000
	DefaultAskAnswerChars     = 4000
	DefaultAskTimeoutSeconds  = 30
)

// AskConfig (top-level `ask:`) is the optional Ask AI help block. Zero value =
// off.
type AskConfig struct {
	// Enabled is the switch. nil or false = off: no route, no request.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Mode is "off", "direct" (this process calls the vendor with its own
	// key) or "hub" (a hive calls the hub relay, a later phase). Empty means
	// the process default: direct on the hub.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Kapa holds the vendor project values. None is ever returned by an API.
	Kapa AskKapaConfig `yaml:"kapa,omitempty" json:"kapa,omitempty"`
	// SendSectionLabel includes the closed-vocabulary dashboard section label
	// in the question. nil = true.
	SendSectionLabel *bool `yaml:"send_section_label,omitempty" json:"send_section_label,omitempty"`
	// Limits tunes the named limits. Zero = the default.
	Limits AskLimitsConfig `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// AskKapaConfig is the vendor block. The project and integration ids are
// configuration, never in the repo, never logged. Their JSON tags are "-" so
// no JSON API response built from Config can carry them; the config export
// path redacts them from the YAML form (see pkg/dashboard).
type AskKapaConfig struct {
	ProjectID     string `yaml:"project_id,omitempty" json:"-"`
	IntegrationID string `yaml:"integration_id,omitempty" json:"-"`
	// APIKeyFile is a path under the allowed secret directory, mounted from
	// a Secret. There is no inline key and no env var holding the key.
	APIKeyFile string `yaml:"api_key_file,omitempty" json:"-"`
}

// AskLimitsConfig holds optional overrides for the Ask limits.
type AskLimitsConfig struct {
	PersonBurst     int `yaml:"person_burst,omitempty" json:"person_burst,omitempty"`
	PersonPerHour   int `yaml:"person_per_hour,omitempty" json:"person_per_hour,omitempty"`
	HivePerHour     int `yaml:"hive_per_hour,omitempty" json:"hive_per_hour,omitempty"`
	GlobalPerMinute int `yaml:"global_per_minute,omitempty" json:"global_per_minute,omitempty"`
	InFlight        int `yaml:"in_flight,omitempty" json:"in_flight,omitempty"`
	Daily           int `yaml:"daily,omitempty" json:"daily,omitempty"`
	QuestionChars   int `yaml:"question_chars,omitempty" json:"question_chars,omitempty"`
	AnswerChars     int `yaml:"answer_chars,omitempty" json:"answer_chars,omitempty"`
	TimeoutSeconds  int `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
}

// AskLimits is the resolved set of limits.
type AskLimits struct {
	PersonBurst     int
	PersonPerHour   int
	HivePerHour     int
	GlobalPerMinute int
	InFlight        int
	Daily           int
	QuestionChars   int
	AnswerChars     int
	TimeoutSeconds  int
}

// AskSettings is the resolved Ask configuration for one process. When Active
// is false nothing else is meaningful except Reason, which names why the
// feature is off without carrying any configured value.
type AskSettings struct {
	// Active is true only when the switch is on and every required value is
	// present and usable.
	Active bool
	// Requested is true when the operator switched the feature on, whether or
	// not it could start. Callers log one warning when Requested && !Active.
	Requested bool
	// Reason is a log-safe explanation, with no configured value in it.
	Reason string

	Mode             string
	ProjectID        string
	IntegrationID    string
	APIKeyFile       string
	SendSectionLabel bool
	Limits           AskLimits
}

// ResolveAskSettings merges ask: with the env overrides (env wins) and checks
// the values a vendor call needs. isHub is true for the hub process, whose
// only valid mode is direct. cfg may be nil (env only). The key file is
// checked for placement under the allowed secret directory; whether it is
// readable is checked by the caller that loads the key.
func ResolveAskSettings(cfg *Config, isHub bool) AskSettings {
	var ask AskConfig
	if cfg != nil {
		ask = cfg.Ask
	}
	s := AskSettings{
		Mode:             strings.ToLower(strings.TrimSpace(ask.Mode)),
		ProjectID:        strings.TrimSpace(ask.Kapa.ProjectID),
		IntegrationID:    strings.TrimSpace(ask.Kapa.IntegrationID),
		APIKeyFile:       strings.TrimSpace(ask.Kapa.APIKeyFile),
		SendSectionLabel: ask.SendSectionLabel == nil || *ask.SendSectionLabel,
	}
	enabled := ask.Enabled != nil && *ask.Enabled
	if v, ok := parseBoolEnv(EnvAskEnabled); ok {
		enabled = v
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvAskMode))); v != "" {
		s.Mode = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvAskKapaProjectID)); v != "" {
		s.ProjectID = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvAskKapaIntegrationID)); v != "" {
		s.IntegrationID = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvAskKapaAPIKeyFile)); v != "" {
		s.APIKeyFile = v
	}
	if v, ok := parseBoolEnv(EnvAskSendSectionLabel); ok {
		s.SendSectionLabel = v
	}
	s.Limits = resolveAskLimits(ask.Limits)
	s.Requested = enabled
	if !enabled {
		s.Reason = "ask is not enabled"
		return s.inert()
	}

	if s.Mode == "" {
		if isHub {
			s.Mode = AskModeDirect
		} else {
			s.Mode = AskModeOff
		}
	}
	switch {
	case s.Mode == AskModeOff:
		s.Reason = "ask.mode is off"
	case s.Mode != AskModeDirect && s.Mode != AskModeHub:
		s.Reason = "ask.mode is not one of off, direct or hub"
	case s.Mode == AskModeHub && isHub:
		s.Reason = "the hub always uses direct mode"
	case s.Mode == AskModeHub:
		s.Reason = "hub relay mode is not available yet"
	case s.ProjectID == "":
		s.Reason = "ask.kapa.project_id is missing"
	case s.IntegrationID == "":
		s.Reason = "ask.kapa.integration_id is missing"
	case s.APIKeyFile == "":
		s.Reason = "ask.kapa.api_key_file is missing"
	case !SecretFilePathAllowed(s.APIKeyFile):
		s.Reason = "ask.kapa.api_key_file is outside the allowed secret directories"
	}
	if s.Reason != "" {
		return s.inert()
	}
	s.Active = true
	return s
}

// inert returns s with every vendor value cleared, so an inactive setting can
// never leak one by accident.
func (s AskSettings) inert() AskSettings {
	s.Active = false
	s.ProjectID, s.IntegrationID, s.APIKeyFile = "", "", ""
	return s
}

func resolveAskLimits(c AskLimitsConfig) AskLimits {
	pick := func(env string, cfg, def int) int {
		if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(env))); err == nil && v > 0 {
			return v
		}
		if cfg > 0 {
			return cfg
		}
		return def
	}
	return AskLimits{
		PersonBurst:     pick(EnvAskLimitPersonBurst, c.PersonBurst, DefaultAskPersonBurst),
		PersonPerHour:   pick(EnvAskLimitPersonPerHour, c.PersonPerHour, DefaultAskPersonPerHour),
		HivePerHour:     pick(EnvAskLimitHivePerHour, c.HivePerHour, DefaultAskHivePerHour),
		GlobalPerMinute: pick(EnvAskLimitGlobalPerMinute, c.GlobalPerMinute, DefaultAskGlobalPerMinute),
		InFlight:        pick(EnvAskLimitInFlight, c.InFlight, DefaultAskInFlight),
		Daily:           pick(EnvAskLimitDaily, c.Daily, DefaultAskDaily),
		QuestionChars:   pick(EnvAskLimitQuestionChars, c.QuestionChars, DefaultAskQuestionChars),
		AnswerChars:     pick(EnvAskLimitAnswerChars, c.AnswerChars, DefaultAskAnswerChars),
		TimeoutSeconds:  pick(EnvAskLimitTimeoutSeconds, c.TimeoutSeconds, DefaultAskTimeoutSeconds),
	}
}
