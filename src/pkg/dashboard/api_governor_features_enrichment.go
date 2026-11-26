package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// featuresEnrichmentBody is the part of the Features PUT that covers
// customers.enrichment.*: the master switch, one switch per provider, the
// gitdm snapshot refresh interval, the devstats hourly lookup limit and the
// auto-accept policy. It also carries customers.store, the person-record
// store mode of the move to the colony store (#19851). Every field is a
// pointer so an absent key leaves the config untouched. The crowd.dev API
// token is a secret and never travels here; it is written through POST
// /api/customers/enrichment/settings.
type featuresEnrichmentBody struct {
	CustomersEnrichmentEnabled         *bool `json:"customersEnrichmentEnabled"`
	CustomersEnrichmentGitdmEnabled    *bool `json:"customersEnrichmentGitdmEnabled"`
	CustomersEnrichmentDevstatsEnabled *bool `json:"customersEnrichmentDevstatsEnabled"`
	CustomersEnrichmentCrowdDevEnabled *bool `json:"customersEnrichmentCrowdDevEnabled"`
	// CustomersEnrichmentGitdmRefresh is a Go duration ("24h"); empty means
	// the default. Values below config.MinCustomersEnrichmentGitdmRefresh are
	// rejected rather than silently raised, so the owner sees the floor.
	CustomersEnrichmentGitdmRefresh *string `json:"customersEnrichmentGitdmRefresh"`
	// CustomersEnrichmentDevstatsHourlyLimit is lookups per hour; 0 means
	// the default.
	CustomersEnrichmentDevstatsHourlyLimit *int `json:"customersEnrichmentDevstatsHourlyLimit"`
	// CustomersEnrichmentAutoAccept is customers.enrichment.auto_accept:
	// empty (the default, off), "off" or "gitdm_high". Any other value is
	// rejected, so a typo is never saved and silently read as off.
	CustomersEnrichmentAutoAccept *string `json:"customersEnrichmentAutoAccept"`
	// CustomersStore is customers.store: empty (the default, shadow), "json",
	// "shadow" or "colony". Any other value is rejected. It is read once at
	// startup, so a change applies on the next restart.
	CustomersStore *string `json:"customersStore"`
}

// featuresChoice validates an optional enumerated setting: empty means the
// default, anything else must parse. It returns the value to save.
func featuresChoice(raw *string, parse func(string) (string, bool), errMsg string) (string, error) {
	v := strings.TrimSpace(*raw)
	if v == "" {
		return "", nil
	}
	norm, ok := parse(v)
	if !ok {
		return "", errors.New(errMsg)
	}
	return norm, nil
}

// prepare validates every field the request carried and returns the
// mutation to apply. Nothing is changed when an error is returned.
func (b featuresEnrichmentBody) prepare() (func(*config.Config), error) {
	var steps []func(*config.Config)
	for _, f := range []struct {
		src *bool
		dst func(*config.Config) **bool
	}{
		{b.CustomersEnrichmentEnabled, func(c *config.Config) **bool { return &c.Customers.Enrichment.Enabled }},
		{b.CustomersEnrichmentGitdmEnabled, func(c *config.Config) **bool { return &c.Customers.Enrichment.Gitdm.Enabled }},
		{b.CustomersEnrichmentDevstatsEnabled, func(c *config.Config) **bool { return &c.Customers.Enrichment.Devstats.Enabled }},
		{b.CustomersEnrichmentCrowdDevEnabled, func(c *config.Config) **bool { return &c.Customers.Enrichment.CrowdDev.Enabled }},
	} {
		if f.src == nil {
			continue
		}
		on, dst := *f.src, f.dst
		steps = append(steps, func(c *config.Config) { *dst(c) = &on })
	}
	if b.CustomersEnrichmentGitdmRefresh != nil {
		v := strings.TrimSpace(*b.CustomersEnrichmentGitdmRefresh)
		if v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < config.MinCustomersEnrichmentGitdmRefresh {
				return nil, fmt.Errorf("customers enrichment gitdm refresh_interval must be empty (default %s) or a duration of at least %s",
					config.DefaultCustomersEnrichmentGitdmRefresh, config.MinCustomersEnrichmentGitdmRefresh)
			}
		}
		steps = append(steps, func(c *config.Config) { c.Customers.Enrichment.Gitdm.RefreshInterval = v })
	}
	if b.CustomersEnrichmentDevstatsHourlyLimit != nil {
		n := *b.CustomersEnrichmentDevstatsHourlyLimit
		if n < 0 || n > config.MaxCustomersEnrichmentDevstatsHourlyLimit {
			return nil, fmt.Errorf("customers enrichment devstats hourly_limit must be between 0 (default %d) and %d",
				config.DefaultCustomersEnrichmentDevstatsHourlyLimit, config.MaxCustomersEnrichmentDevstatsHourlyLimit)
		}
		steps = append(steps, func(c *config.Config) { c.Customers.Enrichment.Devstats.HourlyLimit = n })
	}
	if b.CustomersEnrichmentAutoAccept != nil {
		v, err := featuresChoice(b.CustomersEnrichmentAutoAccept, config.ParseCustomersAutoAccept, fmt.Sprintf(
			"customers enrichment auto_accept must be empty (default %s), %s or %s",
			config.CustomersAutoAcceptOff, config.CustomersAutoAcceptOff, config.CustomersAutoAcceptGitdmHigh))
		if err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.Customers.Enrichment.AutoAccept = v })
	}
	if b.CustomersStore != nil {
		v, err := featuresChoice(b.CustomersStore, config.ParseCustomersStoreMode, fmt.Sprintf(
			"customers store must be empty (default %s), %s, %s or %s",
			config.DefaultCustomersStore, config.CustomersStoreJSON, config.CustomersStoreShadow, config.CustomersStoreColony))
		if err != nil {
			return nil, err
		}
		steps = append(steps, func(c *config.Config) { c.Customers.Store = v })
	}
	return func(c *config.Config) {
		for _, step := range steps {
			step(c)
		}
	}, nil
}

// featuresEnrichmentResponse is the GET side of featuresEnrichmentBody. Each
// switch is reported as saved and as effective (env overrides and parent
// switches applied), so the dialog can say when something else decides.
func featuresEnrichmentResponse(cfg *config.Config) map[string]interface{} {
	e := cfg.Customers.Enrichment
	// Only whether a token resolves, never its value.
	tok, tokErr := e.CrowdDev.Token(context.Background())
	return map[string]interface{}{
		"customersEnrichmentEnabled":             e.IsEnabled(),
		"customersEnrichmentEffective":           cfg.CustomersEnrichmentEnabled(),
		"customersEnrichmentGitdmEnabled":        e.Gitdm.IsEnabled(),
		"customersEnrichmentGitdmEffective":      cfg.CustomersEnrichmentGitdmEnabled(),
		"customersEnrichmentDevstatsEnabled":     e.Devstats.IsEnabled(),
		"customersEnrichmentDevstatsEffective":   cfg.CustomersEnrichmentDevstatsEnabled(),
		"customersEnrichmentCrowdDevEnabled":     e.CrowdDev.IsEnabled(),
		"customersEnrichmentCrowdDevEffective":   cfg.CustomersEnrichmentCrowdDevEnabled(),
		"customersEnrichmentCrowdDevTokenSet":    tokErr == nil && tok != "",
		"customersEnrichmentGitdmRefresh":        e.Gitdm.RefreshInterval,
		"customersEnrichmentDevstatsHourlyLimit": e.Devstats.HourlyLimit,
		"customersEnrichmentAutoAccept":          e.AutoAccept,
		"customersEnrichmentAutoAcceptEffective": cfg.CustomersEnrichmentAutoAccept(),
		"customersStore":                         cfg.Customers.Store,
		"customersStoreEffective":                cfg.CustomersStoreMode(),
		"customersEnrichmentDefaults": map[string]interface{}{
			"customersEnrichmentGitdmRefresh":        config.DefaultCustomersEnrichmentGitdmRefresh.String(),
			"customersEnrichmentDevstatsHourlyLimit": config.DefaultCustomersEnrichmentDevstatsHourlyLimit,
			"customersEnrichmentAutoAccept":          config.CustomersAutoAcceptOff,
			"customersStore":                         config.DefaultCustomersStore,
		},
	}
}
