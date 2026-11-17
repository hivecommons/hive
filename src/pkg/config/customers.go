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
