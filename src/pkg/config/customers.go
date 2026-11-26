package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/keystore"
)

// EnvCustomersEnabled overrides customers.enabled when it holds a recognised
// boolean.
const EnvCustomersEnabled = "HIVE_CUSTOMERS_ENABLED"

// CustomersConfig (top-level `customers:`) gates the Customers section: a
// profile per person built from what the hive already holds, the people
// store under /data/customers and the edge index under /data/graph. Nothing
// it stores leaves the hive. Default ON (the same default-on `*bool` pattern
// as chat.grounded_answers); `enabled: false` opens no store, runs no sweep
// and hides the section.
type CustomersConfig struct {
	// Enabled turns the Customers section on. nil = true.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Inference gates the goal and satisfaction suggestions the hive makes
	// with a model. Default OFF: it sends text about people, some of whom
	// never signed up, to the model the meetings feature is configured to
	// use.
	Inference CustomersInferenceConfig `yaml:"inference,omitempty" json:"inference,omitempty"`
	// MentionSearch gates the one budgeted GitHub search made when an owner
	// accepts a suggested person (step E of the RFC). Default OFF: it asks
	// GitHub about a person's login and spends the shared API budget.
	MentionSearch CustomersMentionSearchConfig `yaml:"mention_search,omitempty" json:"mention_search,omitempty"`
	// Avatars gates GitHub profile pictures in the Customers list. Default
	// ON. It is the one part of the section that makes the viewer's browser
	// contact another site: github.com, for people already on screen.
	Avatars CustomersAvatarsConfig `yaml:"avatars,omitempty" json:"avatars,omitempty"`
	// HubImport gates the import of the real name, organization and country
	// a person entered on the hub and consented to share with this hive
	// (#19163). Default OFF.
	HubImport CustomersHubImportConfig `yaml:"hub_import,omitempty" json:"hub_import,omitempty"`
	// Enrichment gates public affiliation and activity facts from CNCF
	// sources (gitdm, devstats, crowd.dev) for confirmed customers. Default
	// OFF, and each provider has its own switch. See
	// src/docs/customers-enrichment.md.
	Enrichment CustomersEnrichmentConfig `yaml:"enrichment,omitempty" json:"enrichment,omitempty"`
	// ProfileFill gates filling customer profiles (real name, organization,
	// location, country) from each person's own public forge profile on
	// GitHub, GitHub Enterprise, GitLab, Gitea/Forgejo or Linear
	// (#19302). Default OFF: it is a periodic network lookup about people.
	ProfileFill CustomersProfileFillConfig `yaml:"profile_fill,omitempty" json:"profile_fill,omitempty"`
	// Store picks where person records and enrichment results live during
	// the move to the hivecommons/colony store (#19855): "json" (the
	// default), "shadow" or "colony". See CustomersStoreMode and
	// src/docs/design/colony-customer-data-backend.md.
	Store string `yaml:"store,omitempty" json:"store,omitempty"`
}

// EnvCustomersStore overrides customers.store when it names a known mode.
const EnvCustomersStore = "HIVE_CUSTOMERS_STORE"

// Customers store modes (customers.store / HIVE_CUSTOMERS_STORE).
const (
	// CustomersStoreJSON is today's behaviour: people.json and
	// enrichment.json only, and no colony.db is created. The default.
	CustomersStoreJSON = "json"
	// CustomersStoreShadow imports the JSON into colony.db on first boot,
	// writes every change to both, and keeps reading the JSON.
	CustomersStoreShadow = "shadow"
	// CustomersStoreColony reads person records and enrichment from
	// colony.db and still writes the JSON, so switching back to json loses
	// nothing.
	CustomersStoreColony = "colony"
)

// ParseCustomersStoreMode reads a store mode. An empty or unknown value is
// not ok; the caller then falls back, so a typo can only mean json.
func ParseCustomersStoreMode(raw string) (string, bool) {
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case CustomersStoreJSON, CustomersStoreShadow, CustomersStoreColony:
		return v, true
	}
	return "", false
}

// CustomersStoreMode is the customers store mode: HIVE_CUSTOMERS_STORE when
// it names a known mode, then customers.store, then json. An unknown value
// anywhere fails closed to json.
func (c *Config) CustomersStoreMode() string {
	if v, ok := ParseCustomersStoreMode(os.Getenv(EnvCustomersStore)); ok {
		return v
	}
	if c != nil {
		if v, ok := ParseCustomersStoreMode(c.Customers.Store); ok {
			return v
		}
	}
	return CustomersStoreJSON
}

// EnvCustomersProfileFillEnabled overrides customers.profile_fill.enabled
// when it holds a recognised boolean.
const EnvCustomersProfileFillEnabled = "HIVE_CUSTOMERS_PROFILE_FILL_ENABLED"

// DefaultCustomersProfileFillMaxPerHour is how many forge requests the
// profile fill may spend in one hour, across the periodic pass, the backfill,
// accepts and "Refresh from profiles" together. A GitHub lookup is one or two
// requests, so the default refreshes about 30 to 60 people an hour, about one
// percent of a GitHub App's hourly core quota.
const DefaultCustomersProfileFillMaxPerHour = 60

// MaxCustomersProfileFillMaxPerHour caps what a config value may ask for.
const MaxCustomersProfileFillMaxPerHour = 300

// CustomersProfileFillConfig (`customers.profile_fill:`) controls the profile
// fill: a bounded pass that reads each confirmed customer's own public forge
// profile and fills the profile fields nobody set by hand. See
// src/docs/design/customers.md.
type CustomersProfileFillConfig struct {
	// Enabled turns the fill on. nil = false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxRequestsPerHour bounds the forge requests of all lookups in one
	// hour. 0 = DefaultCustomersProfileFillMaxPerHour; values above
	// MaxCustomersProfileFillMaxPerHour are lowered to it.
	MaxRequestsPerHour int `yaml:"max_requests_per_hour,omitempty" json:"max_requests_per_hour,omitempty"`
}

// IsEnabled reports whether the fill is on (default off).
func (c CustomersProfileFillConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// RequestsPerHour is the hourly request budget with the default and the cap
// applied.
func (c CustomersProfileFillConfig) RequestsPerHour() int {
	switch {
	case c.MaxRequestsPerHour <= 0:
		return DefaultCustomersProfileFillMaxPerHour
	case c.MaxRequestsPerHour > MaxCustomersProfileFillMaxPerHour:
		return MaxCustomersProfileFillMaxPerHour
	default:
		return c.MaxRequestsPerHour
	}
}

// CustomersProfileFillEnabled reports whether the profile fill is on: the
// HIVE_CUSTOMERS_PROFILE_FILL_ENABLED env wins over
// customers.profile_fill.enabled when it holds a recognised boolean. It also
// needs the Customers section to be on. It is independent of
// customers.mention_search.
func (c *Config) CustomersProfileFillEnabled() bool {
	if !c.CustomersEnabled() {
		return false
	}
	if v, ok := parseBoolEnv(EnvCustomersProfileFillEnabled); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.Customers.ProfileFill.IsEnabled()
}

// EnvCustomersHubImportEnabled overrides customers.hub_import.enabled when it
// holds a recognised boolean.
const EnvCustomersHubImportEnabled = "HIVE_CUSTOMERS_HUB_IMPORT_ENABLED"

// CustomersHubImportConfig (`customers.hub_import:`) lets this hive ask its
// hub for fields its confirmed customers entered there themselves. On, the
// hive posts salted hashes of its confirmed customers' GitHub logins to the
// hub on a bounded interval and when a customer is accepted; the hub answers
// only for people who consented to share with this hive. The switch is
// necessary and never sufficient: the person's consent on the hub decides
// what arrives. See src/docs/design/customers-hub-consent.md.
type CustomersHubImportConfig struct {
	// Enabled turns the import on. nil = false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// IsEnabled reports whether the import is on (default off).
func (c CustomersHubImportConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// EnvCustomersAvatarsEnabled overrides customers.avatars.enabled when it
// holds a recognised boolean.
const EnvCustomersAvatarsEnabled = "HIVE_CUSTOMERS_AVATARS_ENABLED"

// CustomersAvatarsConfig (`customers.avatars:`) turns GitHub profile pictures
// on the Customers list on or off. On, the viewer's browser requests
// https://github.com/<login>.png for each customer or suggestion whose
// identity is a GitHub login, with no referrer. Off, the list shows initials
// and makes no such request.
type CustomersAvatarsConfig struct {
	// Enabled turns avatars on. nil = true.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// IsEnabled reports whether avatars are on (default on).
func (c CustomersAvatarsConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// EnvCustomersInferenceEnabled overrides customers.inference.enabled when it
// holds a recognised boolean.
const EnvCustomersInferenceEnabled = "HIVE_CUSTOMERS_INFERENCE_ENABLED"

// CustomersInferenceConfig (`customers.inference:`) turns goal and
// satisfaction inference on. Suggestions only: a person confirms, edits or
// rejects each one, and inference never overwrites a confirmed or manual
// value. The model is the one configured for meetings (`meetings.llm`).
type CustomersInferenceConfig struct {
	// Enabled turns inference on. nil = false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// IsEnabled reports whether inference is on (default off).
func (c CustomersInferenceConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// IsEnabled reports whether the Customers section is on (default on).
func (c CustomersConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// CustomersEnabled reports whether the Customers section is on: the
// HIVE_CUSTOMERS_ENABLED env wins over customers.enabled when it holds a
// recognised boolean. A nil config is the default, on.
func (c *Config) CustomersEnabled() bool {
	if v, ok := parseBoolEnv(EnvCustomersEnabled); ok {
		return v
	}
	if c == nil {
		return true
	}
	return c.Customers.IsEnabled()
}

// CustomersInferenceEnabled reports whether inference is on: the
// HIVE_CUSTOMERS_INFERENCE_ENABLED env wins over customers.inference.enabled
// when it holds a recognised boolean. Inference also needs the Customers
// section itself to be on.
func (c *Config) CustomersInferenceEnabled() bool {
	if !c.CustomersEnabled() {
		return false
	}
	if v, ok := parseBoolEnv(EnvCustomersInferenceEnabled); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.Customers.Inference.IsEnabled()
}

// CustomersAvatarsEnabled reports whether GitHub avatars show on the
// Customers list: the HIVE_CUSTOMERS_AVATARS_ENABLED env wins over
// customers.avatars.enabled when it holds a recognised boolean. A nil config
// is the default, on.
func (c *Config) CustomersAvatarsEnabled() bool {
	if v, ok := parseBoolEnv(EnvCustomersAvatarsEnabled); ok {
		return v
	}
	if c == nil {
		return true
	}
	return c.Customers.Avatars.IsEnabled()
}

// CustomersHubImportEnabled reports whether the hub import is switched on:
// the HIVE_CUSTOMERS_HUB_IMPORT_ENABLED env wins over
// customers.hub_import.enabled when it holds a recognised boolean. It also
// needs the Customers section to be on. A hub registration is checked by the
// caller, which holds it.
func (c *Config) CustomersHubImportEnabled() bool {
	if !c.CustomersEnabled() {
		return false
	}
	if v, ok := parseBoolEnv(EnvCustomersHubImportEnabled); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.Customers.HubImport.IsEnabled()
}

// EnvCustomersMentionSearchEnabled overrides customers.mention_search.enabled
// when it holds a recognised boolean.
const EnvCustomersMentionSearchEnabled = "HIVE_CUSTOMERS_MENTION_SEARCH_ENABLED"

// DefaultCustomersMentionSearchMaxPerHour is how many GitHub search requests
// the accepts of all suggested people may spend together in one hour. Each
// accept spends at most two (customers.MentionSearchMaxRequests), so the
// default allows about five accepts an hour.
const DefaultCustomersMentionSearchMaxPerHour = 10

// MaxCustomersMentionSearchMaxPerHour caps what a config value may ask for, so
// a typo cannot hand the feature the whole search quota.
const MaxCustomersMentionSearchMaxPerHour = 60

// CustomersMentionSearchConfig (`customers.mention_search:`) controls the
// budgeted GitHub search for mentions and comments that runs when an owner
// accepts a suggested GitHub login, so the new profile starts with history
// beyond what the hive already holds. The discovery sweep never runs it.
type CustomersMentionSearchConfig struct {
	// Enabled turns the search on. nil = false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxRequestsPerHour bounds the search requests all accepts may make in
	// one hour. 0 = DefaultCustomersMentionSearchMaxPerHour; values above
	// MaxCustomersMentionSearchMaxPerHour are lowered to it.
	MaxRequestsPerHour int `yaml:"max_requests_per_hour,omitempty" json:"max_requests_per_hour,omitempty"`
}

// IsEnabled reports whether the search is on (default off).
func (c CustomersMentionSearchConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// RequestsPerHour is the hourly request budget with the default and the cap
// applied.
func (c CustomersMentionSearchConfig) RequestsPerHour() int {
	switch {
	case c.MaxRequestsPerHour <= 0:
		return DefaultCustomersMentionSearchMaxPerHour
	case c.MaxRequestsPerHour > MaxCustomersMentionSearchMaxPerHour:
		return MaxCustomersMentionSearchMaxPerHour
	default:
		return c.MaxRequestsPerHour
	}
}

// CustomersMentionSearchEnabled reports whether the accept-time search is on:
// the HIVE_CUSTOMERS_MENTION_SEARCH_ENABLED env wins over
// customers.mention_search.enabled when it holds a recognised boolean. It
// also needs the Customers section to be on.
func (c *Config) CustomersMentionSearchEnabled() bool {
	if !c.CustomersEnabled() {
		return false
	}
	if v, ok := parseBoolEnv(EnvCustomersMentionSearchEnabled); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.Customers.MentionSearch.IsEnabled()
}

// Customers enrichment (`customers.enrichment:`) adds public affiliation and
// activity facts about confirmed customers from CNCF-published sources, through
// the hivecommons/colony resolver. Everything is OFF by default. The block and
// each provider have their own switch, every switch also needs the one above it
// on, and an environment variable overrides each one. See
// src/docs/customers-enrichment.md.
const (
	// EnvCustomersEnrichmentEnabled overrides customers.enrichment.enabled.
	EnvCustomersEnrichmentEnabled = "HIVE_CUSTOMERS_ENRICHMENT_ENABLED"
	// EnvCustomersEnrichmentGitdmEnabled overrides
	// customers.enrichment.gitdm.enabled.
	EnvCustomersEnrichmentGitdmEnabled = "HIVE_CUSTOMERS_ENRICHMENT_GITDM_ENABLED"
	// EnvCustomersEnrichmentGitdmRefreshInterval overrides
	// customers.enrichment.gitdm.refresh_interval (a Go duration).
	EnvCustomersEnrichmentGitdmRefreshInterval = "HIVE_CUSTOMERS_ENRICHMENT_GITDM_REFRESH_INTERVAL"
	// EnvCustomersEnrichmentDevstatsEnabled overrides
	// customers.enrichment.devstats.enabled.
	EnvCustomersEnrichmentDevstatsEnabled = "HIVE_CUSTOMERS_ENRICHMENT_DEVSTATS_ENABLED"
	// EnvCustomersEnrichmentDevstatsHourlyLimit overrides
	// customers.enrichment.devstats.hourly_limit.
	EnvCustomersEnrichmentDevstatsHourlyLimit = "HIVE_CUSTOMERS_ENRICHMENT_DEVSTATS_HOURLY_LIMIT"
	// EnvCustomersEnrichmentCrowdDevEnabled overrides
	// customers.enrichment.crowddev.enabled.
	EnvCustomersEnrichmentCrowdDevEnabled = "HIVE_CUSTOMERS_ENRICHMENT_CROWDDEV_ENABLED"
	// EnvCustomersEnrichmentCrowdDevToken supplies the crowd.dev API token
	// directly; it wins over the keystore-sealed token file. A secret.
	EnvCustomersEnrichmentCrowdDevToken = "HIVE_CUSTOMERS_ENRICHMENT_CROWDDEV_TOKEN"
	// EnvCustomersEnrichmentCrowdDevTokenFile overrides
	// customers.enrichment.crowddev.token_file.
	EnvCustomersEnrichmentCrowdDevTokenFile = "HIVE_CUSTOMERS_ENRICHMENT_CROWDDEV_TOKEN_FILE"
)

const (
	// DefaultCustomersEnrichmentGitdmRefresh is how often the public gitdm
	// affiliation snapshot is downloaded again.
	DefaultCustomersEnrichmentGitdmRefresh = 24 * time.Hour
	// MinCustomersEnrichmentGitdmRefresh is the shortest refresh a config
	// value may ask for; shorter values are raised to it, so a typo cannot
	// turn the snapshot download into a loop.
	MinCustomersEnrichmentGitdmRefresh = time.Hour
	// DefaultCustomersEnrichmentDevstatsHourlyLimit is how many devstats
	// lookups all people together may make in one hour.
	DefaultCustomersEnrichmentDevstatsHourlyLimit = 30
	// MaxCustomersEnrichmentDevstatsHourlyLimit caps what a config value may
	// ask for.
	MaxCustomersEnrichmentDevstatsHourlyLimit = 120
	// DefaultCustomersEnrichmentCrowdDevTokenFile is where Settings seals the
	// crowd.dev API token.
	DefaultCustomersEnrichmentCrowdDevTokenFile = WritableSecretsDir + "/customers_crowddev_token"
)

// CustomersEnrichmentConfig (`customers.enrichment:`) is the master switch and
// the per-provider settings for enrichment.
type CustomersEnrichmentConfig struct {
	// Enabled turns enrichment on. nil = false. Each provider needs its own
	// switch on as well.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Gitdm matches confirmed customers against the public cncf/gitdm
	// affiliation files on this hive. Only the public snapshot is
	// downloaded; nothing about a person is sent.
	Gitdm CustomersEnrichmentGitdmConfig `yaml:"gitdm,omitempty" json:"gitdm,omitempty"`
	// Devstats looks up a confirmed customer's public contribution activity
	// on CNCF devstats by GitHub login (never an email).
	Devstats CustomersEnrichmentDevstatsConfig `yaml:"devstats,omitempty" json:"devstats,omitempty"`
	// CrowdDev looks up affiliation and activity on crowd.dev (LFX
	// Insights) by GitHub login, with an API token.
	CrowdDev CustomersEnrichmentCrowdDevConfig `yaml:"crowddev,omitempty" json:"crowddev,omitempty"`
	// AutoAccept is the auto-accept policy for provider affiliations: "off"
	// (the default, every value is a suggestion) or "gitdm_high". See
	// CustomersEnrichmentAutoAccept.
	AutoAccept string `yaml:"auto_accept,omitempty" json:"auto_accept,omitempty"`
}

// EnvCustomersEnrichmentAutoAccept overrides customers.enrichment.auto_accept
// when it names a known policy.
const EnvCustomersEnrichmentAutoAccept = "HIVE_CUSTOMERS_ENRICHMENT_AUTO_ACCEPT"

// Enrichment auto-accept policies.
const (
	// CustomersAutoAcceptOff leaves every provider value a suggestion an
	// owner accepts or dismisses. The default.
	CustomersAutoAcceptOff = "off"
	// CustomersAutoAcceptGitdmHigh accepts a current gitdm affiliation
	// matched by login (confidence high) into an organization field that is
	// empty or holds an automatic value. It never overwrites a value a
	// person set or one shared from the hub, and it stays dismissible.
	CustomersAutoAcceptGitdmHigh = "gitdm_high"
)

// ParseCustomersAutoAccept reads an auto-accept policy; an empty or unknown
// value is not ok.
func ParseCustomersAutoAccept(raw string) (string, bool) {
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case CustomersAutoAcceptOff, CustomersAutoAcceptGitdmHigh:
		return v, true
	}
	return "", false
}

// CustomersEnrichmentAutoAccept is the auto-accept policy:
// HIVE_CUSTOMERS_ENRICHMENT_AUTO_ACCEPT when it names a known policy, then
// customers.enrichment.auto_accept, then off. It is off whenever the gitdm
// provider is off, and an unknown value fails closed to off.
func (c *Config) CustomersEnrichmentAutoAccept() string {
	if !c.CustomersEnrichmentGitdmEnabled() {
		return CustomersAutoAcceptOff
	}
	if v, ok := ParseCustomersAutoAccept(os.Getenv(EnvCustomersEnrichmentAutoAccept)); ok {
		return v
	}
	if c != nil {
		if v, ok := ParseCustomersAutoAccept(c.Customers.Enrichment.AutoAccept); ok {
			return v
		}
	}
	return CustomersAutoAcceptOff
}

// IsEnabled reports whether the enrichment block is on (default off).
func (c CustomersEnrichmentConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// CustomersEnrichmentGitdmConfig (`customers.enrichment.gitdm:`).
type CustomersEnrichmentGitdmConfig struct {
	// Enabled turns the gitdm provider on. nil = false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// RefreshInterval is how often the public snapshot is downloaded again,
	// as a Go duration ("24h"). Empty = DefaultCustomersEnrichmentGitdmRefresh;
	// values below MinCustomersEnrichmentGitdmRefresh are raised to it.
	RefreshInterval string `yaml:"refresh_interval,omitempty" json:"refresh_interval,omitempty"`
}

// IsEnabled reports whether gitdm is on (default off).
func (c CustomersEnrichmentGitdmConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// CustomersEnrichmentDevstatsConfig (`customers.enrichment.devstats:`).
type CustomersEnrichmentDevstatsConfig struct {
	// Enabled turns the devstats provider on. nil = false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// HourlyLimit bounds the devstats lookups all people may make in one
	// hour. 0 = DefaultCustomersEnrichmentDevstatsHourlyLimit; values above
	// MaxCustomersEnrichmentDevstatsHourlyLimit are lowered to it.
	HourlyLimit int `yaml:"hourly_limit,omitempty" json:"hourly_limit,omitempty"`
}

// IsEnabled reports whether devstats is on (default off).
func (c CustomersEnrichmentDevstatsConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// CustomersEnrichmentCrowdDevConfig (`customers.enrichment.crowddev:`). The
// API token is never stored in hive.yaml: it lives in a keystore-sealed file
// (TokenFile) or arrives through HIVE_CUSTOMERS_ENRICHMENT_CROWDDEV_TOKEN.
type CustomersEnrichmentCrowdDevConfig struct {
	// Enabled turns the crowd.dev provider on. nil = false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// TokenFile is the keystore-sealed token file. Empty =
	// DefaultCustomersEnrichmentCrowdDevTokenFile.
	TokenFile string `yaml:"token_file,omitempty" json:"token_file,omitempty"`
}

// IsEnabled reports whether crowd.dev is on (default off).
func (c CustomersEnrichmentCrowdDevConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// TokenFileOrDefault returns the token file path: the env override, then
// token_file, then the default.
func (c CustomersEnrichmentCrowdDevConfig) TokenFileOrDefault() string {
	if v := strings.TrimSpace(os.Getenv(EnvCustomersEnrichmentCrowdDevTokenFile)); v != "" {
		return v
	}
	if v := strings.TrimSpace(c.TokenFile); v != "" {
		return v
	}
	return DefaultCustomersEnrichmentCrowdDevTokenFile
}

// Token returns the crowd.dev API token: the env value, then the sealed
// file. A missing file is "". A secret; never log it.
func (c CustomersEnrichmentCrowdDevConfig) Token(ctx context.Context) (string, error) {
	if v := strings.TrimSpace(os.Getenv(EnvCustomersEnrichmentCrowdDevToken)); v != "" {
		return v, nil
	}
	path := c.TokenFileOrDefault()
	v, err := keystore.ReadString(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("customers enrichment: reading crowd.dev token file %s: %w", path, err)
	}
	return strings.TrimSpace(v), nil
}

// WriteCustomersEnrichmentSecret seals value into the secret file at path
// through the keystore (owner-only mode). An empty value removes the file.
func WriteCustomersEnrichmentSecret(ctx context.Context, path, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("customers enrichment: removing secret file %s: %w", path, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("customers enrichment: creating secret dir: %w", err)
	}
	return keystore.WriteFile(ctx, path, []byte(value))
}

// CustomersEnrichmentEnabled reports whether enrichment is on: the
// HIVE_CUSTOMERS_ENRICHMENT_ENABLED env wins over customers.enrichment.enabled
// when it holds a recognised boolean. It also needs the Customers section on.
func (c *Config) CustomersEnrichmentEnabled() bool {
	if !c.CustomersEnabled() {
		return false
	}
	if v, ok := parseBoolEnv(EnvCustomersEnrichmentEnabled); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.Customers.Enrichment.IsEnabled()
}

// customersEnrichmentProviderEnabled applies the shared rule for a provider
// switch: enrichment on, then the env override, then the saved value.
func (c *Config) customersEnrichmentProviderEnabled(env string, saved func(CustomersEnrichmentConfig) bool) bool {
	if !c.CustomersEnrichmentEnabled() {
		return false
	}
	if v, ok := parseBoolEnv(env); ok {
		return v
	}
	if c == nil {
		return false
	}
	return saved(c.Customers.Enrichment)
}

// CustomersEnrichmentGitdmEnabled reports whether the gitdm provider is on.
// It needs enrichment on; HIVE_CUSTOMERS_ENRICHMENT_GITDM_ENABLED overrides
// the saved value.
func (c *Config) CustomersEnrichmentGitdmEnabled() bool {
	return c.customersEnrichmentProviderEnabled(EnvCustomersEnrichmentGitdmEnabled, func(e CustomersEnrichmentConfig) bool { return e.Gitdm.IsEnabled() })
}

// CustomersEnrichmentDevstatsEnabled reports whether the devstats provider is
// on. It needs enrichment on; HIVE_CUSTOMERS_ENRICHMENT_DEVSTATS_ENABLED
// overrides the saved value.
func (c *Config) CustomersEnrichmentDevstatsEnabled() bool {
	return c.customersEnrichmentProviderEnabled(EnvCustomersEnrichmentDevstatsEnabled, func(e CustomersEnrichmentConfig) bool { return e.Devstats.IsEnabled() })
}

// CustomersEnrichmentCrowdDevEnabled reports whether the crowd.dev provider
// is on. It needs enrichment on; HIVE_CUSTOMERS_ENRICHMENT_CROWDDEV_ENABLED
// overrides the saved value. A missing token is checked by the caller.
func (c *Config) CustomersEnrichmentCrowdDevEnabled() bool {
	return c.customersEnrichmentProviderEnabled(EnvCustomersEnrichmentCrowdDevEnabled, func(e CustomersEnrichmentConfig) bool { return e.CrowdDev.IsEnabled() })
}

// CustomersEnrichmentGitdmRefresh is the gitdm snapshot refresh interval:
// HIVE_CUSTOMERS_ENRICHMENT_GITDM_REFRESH_INTERVAL, then
// customers.enrichment.gitdm.refresh_interval, then the default. An
// unparsable or non-positive value falls back to the default, and a value
// below MinCustomersEnrichmentGitdmRefresh is raised to it.
func (c *Config) CustomersEnrichmentGitdmRefresh() time.Duration {
	raw := strings.TrimSpace(os.Getenv(EnvCustomersEnrichmentGitdmRefreshInterval))
	if raw == "" && c != nil {
		raw = strings.TrimSpace(c.Customers.Enrichment.Gitdm.RefreshInterval)
	}
	d, err := time.ParseDuration(raw)
	switch {
	case raw == "" || err != nil || d <= 0:
		return DefaultCustomersEnrichmentGitdmRefresh
	case d < MinCustomersEnrichmentGitdmRefresh:
		return MinCustomersEnrichmentGitdmRefresh
	default:
		return d
	}
}

// CustomersEnrichmentDevstatsHourlyLimit is the devstats lookup budget per
// hour: HIVE_CUSTOMERS_ENRICHMENT_DEVSTATS_HOURLY_LIMIT, then
// customers.enrichment.devstats.hourly_limit, then the default, capped at
// MaxCustomersEnrichmentDevstatsHourlyLimit.
func (c *Config) CustomersEnrichmentDevstatsHourlyLimit() int {
	n := 0
	if raw := strings.TrimSpace(os.Getenv(EnvCustomersEnrichmentDevstatsHourlyLimit)); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			n = v
		}
	}
	if n <= 0 && c != nil {
		n = c.Customers.Enrichment.Devstats.HourlyLimit
	}
	switch {
	case n <= 0:
		return DefaultCustomersEnrichmentDevstatsHourlyLimit
	case n > MaxCustomersEnrichmentDevstatsHourlyLimit:
		return MaxCustomersEnrichmentDevstatsHourlyLimit
	default:
		return n
	}
}
