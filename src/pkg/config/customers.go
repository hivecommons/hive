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
