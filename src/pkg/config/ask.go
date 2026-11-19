package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/keystore"
)

// Ask AI help (src/docs/design/dashboard-ask-ai-help.md, #19177): an optional
// docs-help panel whose backend calls the Kapa.ai Chat API server side. Every
// key here is OFF by default and inert when unconfigured. The API key is only
// ever read from a file under the allowed secret dirs; the project id and the
// integration id come from configuration or env, never from the repo. None of
// the three is ever logged or returned by an API.
const (
	// EnvAskEnabled overrides ask.enabled when it holds a recognised boolean.
	EnvAskEnabled = "HIVE_ASK_ENABLED"
	// EnvAskMode overrides ask.mode (off, direct or hub). The hub itself is
	// always direct and ignores it.
	EnvAskMode = "HIVE_ASK_MODE"
	// EnvAskKapaProjectID overrides ask.kapa.project_id.
	EnvAskKapaProjectID = "HIVE_ASK_KAPA_PROJECT_ID"
	// EnvAskKapaIntegrationID overrides ask.kapa.integration_id.
	EnvAskKapaIntegrationID = "HIVE_ASK_KAPA_INTEGRATION_ID"
	// EnvAskKapaAPIKeyFile overrides ask.kapa.api_key_file. It names a FILE;
	// there is deliberately no env var that holds the key itself.
	EnvAskKapaAPIKeyFile = "HIVE_ASK_KAPA_API_KEY_FILE"
	// EnvAskSendSectionLabel overrides ask.send_section_label.
	EnvAskSendSectionLabel = "HIVE_ASK_SEND_SECTION_LABEL"
	// EnvAskRelayHives overrides ask.relay.hives on the hub (none or hosted).
	EnvAskRelayHives = "HIVE_ASK_RELAY_HIVES"
	// EnvAskRelayDisabledHives is a comma-separated list of hive ids the hub
	// relay refuses even when they are otherwise allowed.
	EnvAskRelayDisabledHives = "HIVE_ASK_RELAY_DISABLED_HIVES"

	// Limit overrides. Each holds a positive integer; anything else keeps the
	// default.
	EnvAskLimitPersonPerMinute = "HIVE_ASK_LIMIT_PERSON_PER_MINUTE"
	EnvAskLimitPersonPerHour   = "HIVE_ASK_LIMIT_PERSON_PER_HOUR"
	EnvAskLimitHivePerHour     = "HIVE_ASK_LIMIT_HIVE_PER_HOUR"
	EnvAskLimitGlobalPerMinute = "HIVE_ASK_LIMIT_GLOBAL_PER_MINUTE"
	EnvAskLimitInFlight        = "HIVE_ASK_LIMIT_IN_FLIGHT"
	EnvAskLimitDaily           = "HIVE_ASK_LIMIT_DAILY"
)

// Ask modes.
const (
	AskModeOff    = "off"
	AskModeDirect = "direct"
	AskModeHub    = "hub"
)

// Hub relay scopes (ask.relay.hives).
const (
	AskRelayNone   = "none"
	AskRelayHosted = "hosted"
	// AskRelayRegistered (self-hosted hives registered with a hub) is
	// accepted in configuration but not served in this round: the operator
	// chose hosted hives only. The hub treats it as hosted and says so once.
	// TODO(#19177): serve registered self-hosted hives once the operator
	// approves; the relay gate is relayAllowsHive in pkg/hub/ask_relay.go.
	AskRelayRegistered = "registered"
)

// Limits. Every number is a proposal from the design, a named default and
// overridable by env. The vendor's chat limit is shared by the whole team
// (the docs site widget, any bots and every dashboard), so the hub-wide
// budget is deliberately a small fraction of it.
const (
	// KapaTeamChatLimitPerMinute is the vendor's documented per-team chat
	// limit.
	KapaTeamChatLimitPerMinute = 20
	// askGlobalShareDivisor keeps the default hub-wide budget at one fifth of
	// the vendor team limit.
	askGlobalShareDivisor = 5
	// askGlobalCeilingDivisor caps any override at half of the vendor team
	// limit, so a dashboard can never take the whole team budget.
	askGlobalCeilingDivisor = 2

	DefaultAskPersonPerMinute = 3
	DefaultAskPersonPerHour   = 20
	DefaultAskHivePerHour     = 60
	DefaultAskGlobalPerMinute = KapaTeamChatLimitPerMinute / askGlobalShareDivisor
	MaxAskGlobalPerMinute     = KapaTeamChatLimitPerMinute / askGlobalCeilingDivisor
	DefaultAskInFlight        = 2
	DefaultAskDaily           = 500

	// DefaultAskSendSectionLabel: the closed-vocabulary section label is sent
	// unless an operator turns it off (operator decision on #19177).
	DefaultAskSendSectionLabel = true
)

// askIdentifierPattern bounds the project and integration ids to the vendor's
// UUID-like shape, so a value can never smuggle a path or a query into the
// vendor URL.
var askIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// AskConfig is the top-level `ask:` block.
type AskConfig struct {
	// Enabled is the owner or operator switch. Nil or false is off.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Mode is off, direct (this process calls the vendor with its own key)
	// or hub (a hosted hive calls the hub relay). Empty means off.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Kapa holds the vendor identifiers and the key file path.
	Kapa AskKapaConfig `yaml:"kapa,omitempty" json:"kapa,omitempty"`
	// SendSectionLabel includes the dashboard section label (from a closed
	// list of our own ids) with the question. Nil means
	// DefaultAskSendSectionLabel.
	SendSectionLabel *bool `yaml:"send_section_label,omitempty" json:"send_section_label,omitempty"`
	// Limits overrides the named defaults. Zero keeps the default.
	Limits AskLimitsConfig `yaml:"limits,omitempty" json:"limits,omitempty"`
	// Relay is read by the hub only, from env (HIVE_ASK_RELAY_*); the hub has
	// no hive.yaml, so it is not a hive.yaml key.
	Relay AskRelayConfig `yaml:"-" json:"-"`
}

// AskKapaConfig is ask.kapa.
type AskKapaConfig struct {
	// ProjectID and IntegrationID are never serialised to JSON, so no API
	// that encodes the config can return them.
	ProjectID     string `yaml:"project_id,omitempty" json:"-"`
	IntegrationID string `yaml:"integration_id,omitempty" json:"-"`
	// APIKeyFile is a path under the allowed secret dirs, mounted from a
	// Secret. There is no inline key.
	APIKeyFile string `yaml:"api_key_file,omitempty" json:"api_key_file,omitempty"`
}

// AskLimitsConfig is ask.limits.
type AskLimitsConfig struct {
	PersonPerMinute int `yaml:"person_per_minute,omitempty" json:"person_per_minute,omitempty"`
	PersonPerHour   int `yaml:"person_per_hour,omitempty" json:"person_per_hour,omitempty"`
	HivePerHour     int `yaml:"hive_per_hour,omitempty" json:"hive_per_hour,omitempty"`
	GlobalPerMinute int `yaml:"global_per_minute,omitempty" json:"global_per_minute,omitempty"`
	InFlight        int `yaml:"in_flight,omitempty" json:"in_flight,omitempty"`
	Daily           int `yaml:"daily,omitempty" json:"daily,omitempty"`
}

// AskRelayConfig is ask.relay (hub only).
type AskRelayConfig struct {
	// Hives is none, hosted (default) or registered.
	Hives string `yaml:"hives,omitempty" json:"hives,omitempty"`
	// DisabledHives are hive ids the relay refuses.
	DisabledHives []string `yaml:"disabled_hives,omitempty" json:"disabled_hives,omitempty"`
}

// AskLimits are the effective limits after defaults and overrides.
type AskLimits struct {
	PersonPerMinute int `json:"person_per_minute"`
	PersonPerHour   int `json:"person_per_hour"`
	HivePerHour     int `json:"hive_per_hour"`
	GlobalPerMinute int `json:"global_per_minute"`
	InFlight        int `json:"in_flight"`
	Daily           int `json:"daily"`
}

// AskSettings is the resolved, effective Ask configuration. It never carries
// the API key; LoadAskAPIKey reads it at the point of use.
type AskSettings struct {
	// Requested is the switch as set; Enabled is whether the feature is
	// actually on (switched on AND fully configured).
	Requested bool
	Enabled   bool
	Mode      string
	// ProjectID, IntegrationID and APIKeyFile are configuration values. They
	// are never logged, never returned by an API, never put in an error.
	ProjectID        string
	IntegrationID    string
	APIKeyFile       string
	SendSectionLabel bool
	Limits           AskLimits
	RelayHives       string
	DisabledHives    map[string]bool
	// Problem says, without values, why the feature is off although it was
	// requested. Empty when Enabled or when not requested.
	Problem string
}

// AskSettings resolves the hive's ask block with the process env.
func (c *Config) AskSettings() AskSettings {
	if c == nil {
		return ResolveAsk(AskConfig{}, os.Getenv, false)
	}
	return ResolveAsk(c.Ask, os.Getenv, false)
}

// ResolveAsk applies env overrides (env wins over hive.yaml, as for the other
// feature switches) and defaults. onHub forces direct mode: the hub always
// calls the vendor itself. getenv is os.Getenv in production.
func ResolveAsk(c AskConfig, getenv func(string) string, onHub bool) AskSettings {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	s := AskSettings{
		Mode:             strings.ToLower(strings.TrimSpace(c.Mode)),
		ProjectID:        strings.TrimSpace(c.Kapa.ProjectID),
		IntegrationID:    strings.TrimSpace(c.Kapa.IntegrationID),
		APIKeyFile:       strings.TrimSpace(c.Kapa.APIKeyFile),
		SendSectionLabel: DefaultAskSendSectionLabel,
		RelayHives:       strings.ToLower(strings.TrimSpace(c.Relay.Hives)),
		DisabledHives:    map[string]bool{},
	}
	if c.Enabled != nil {
		s.Requested = *c.Enabled
	}
	if v, ok := askBoolEnv(getenv(EnvAskEnabled)); ok {
		s.Requested = v
	}
	if v := strings.ToLower(strings.TrimSpace(getenv(EnvAskMode))); v != "" {
		s.Mode = v
	}
	if v := strings.TrimSpace(getenv(EnvAskKapaProjectID)); v != "" {
		s.ProjectID = v
	}
	if v := strings.TrimSpace(getenv(EnvAskKapaIntegrationID)); v != "" {
		s.IntegrationID = v
	}
	if v := strings.TrimSpace(getenv(EnvAskKapaAPIKeyFile)); v != "" {
		s.APIKeyFile = v
	}
	if c.SendSectionLabel != nil {
		s.SendSectionLabel = *c.SendSectionLabel
	}
	if v, ok := askBoolEnv(getenv(EnvAskSendSectionLabel)); ok {
		s.SendSectionLabel = v
	}
	if v := strings.ToLower(strings.TrimSpace(getenv(EnvAskRelayHives))); v != "" {
		s.RelayHives = v
	}
	switch s.RelayHives {
	case AskRelayNone, AskRelayHosted, AskRelayRegistered:
	default:
		s.RelayHives = AskRelayHosted
	}
	disabled := append([]string(nil), c.Relay.DisabledHives...)
	disabled = append(disabled, strings.Split(getenv(EnvAskRelayDisabledHives), ",")...)
	for _, id := range disabled {
		if id = strings.TrimSpace(id); id != "" {
			s.DisabledHives[id] = true
		}
	}
	s.Limits = AskLimits{
		PersonPerMinute: askLimit(c.Limits.PersonPerMinute, getenv(EnvAskLimitPersonPerMinute), DefaultAskPersonPerMinute),
		PersonPerHour:   askLimit(c.Limits.PersonPerHour, getenv(EnvAskLimitPersonPerHour), DefaultAskPersonPerHour),
		HivePerHour:     askLimit(c.Limits.HivePerHour, getenv(EnvAskLimitHivePerHour), DefaultAskHivePerHour),
		GlobalPerMinute: askLimit(c.Limits.GlobalPerMinute, getenv(EnvAskLimitGlobalPerMinute), DefaultAskGlobalPerMinute),
		InFlight:        askLimit(c.Limits.InFlight, getenv(EnvAskLimitInFlight), DefaultAskInFlight),
		Daily:           askLimit(c.Limits.Daily, getenv(EnvAskLimitDaily), DefaultAskDaily),
	}
	if s.Limits.GlobalPerMinute > MaxAskGlobalPerMinute {
		s.Limits.GlobalPerMinute = MaxAskGlobalPerMinute
	}
	if onHub {
		s.Mode = AskModeDirect
	}
	switch s.Mode {
	case AskModeDirect, AskModeHub:
	default:
		s.Mode = AskModeOff
	}
	if !s.Requested {
		return s
	}
	s.Problem = s.configProblem()
	s.Enabled = s.Problem == ""
	return s
}

// configProblem names, without any value, the first thing that keeps a
// requested feature off.
func (s AskSettings) configProblem() string {
	switch s.Mode {
	case AskModeOff:
		return "ask.mode is off; set it to direct or hub"
	case AskModeHub:
		return ""
	}
	if s.ProjectID == "" {
		return "ask.kapa.project_id is not set"
	}
	if !askIdentifierPattern.MatchString(s.ProjectID) {
		return "ask.kapa.project_id is not a valid identifier"
	}
	if s.IntegrationID == "" {
		return "ask.kapa.integration_id is not set"
	}
	if !askIdentifierPattern.MatchString(s.IntegrationID) {
		return "ask.kapa.integration_id is not a valid identifier"
	}
	if s.APIKeyFile == "" {
		return "ask.kapa.api_key_file is not set"
	}
	if !SecretFilePathAllowed(s.APIKeyFile) {
		// No path in the message: the design keeps every ask value out of
		// logs and responses.
		return "ask.kapa.api_key_file is outside the allowed secret dirs (" + strings.Join(SecretFileRoots(), ", ") + ")"
	}
	if _, err := LoadAskAPIKey(s.APIKeyFile); err != nil {
		return "ask.kapa.api_key_file could not be read or is empty"
	}
	return ""
}

// errAskKeyUnavailable never carries the path's contents.
var errAskKeyUnavailable = errors.New("ask api key unavailable")

// LoadAskAPIKey reads the key file through the keystore, refusing any path
// outside the allowed secret dirs. The error never contains the key.
func LoadAskAPIKey(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || !SecretFilePathAllowed(path) {
		return "", errAskKeyUnavailable
	}
	data, err := keystore.ReadFile(context.Background(), path)
	if err != nil {
		return "", fmt.Errorf("%w: read failed", errAskKeyUnavailable)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("%w: empty", errAskKeyUnavailable)
	}
	return key, nil
}

func askBoolEnv(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// askLimit picks env, then config, then the default; only positive values
// count.
func askLimit(configured int, env string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(env)); err == nil && n > 0 {
		return n
	}
	if configured > 0 {
		return configured
	}
	return def
}
