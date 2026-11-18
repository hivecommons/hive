package config

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
