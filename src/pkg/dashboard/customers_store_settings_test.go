package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// customersStoreFeatures is the part of the governor payload that reports
// customers.store and customers.enrichment.auto_accept (#19857).
type customersStoreFeatures struct {
	Features struct {
		Store               string `json:"customersStore"`
		StoreEffective      string `json:"customersStoreEffective"`
		AutoAccept          string `json:"customersEnrichmentAutoAccept"`
		AutoAcceptEffective string `json:"customersEnrichmentAutoAcceptEffective"`
		Defaults            struct {
			Store      string `json:"customersStore"`
			AutoAccept string `json:"customersEnrichmentAutoAccept"`
		} `json:"customersEnrichmentDefaults"`
	} `json:"features"`
}

func readCustomersStoreFeatures(t *testing.T, s *Server) customersStoreFeatures {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d - %s", rec.Code, rec.Body.String())
	}
	var p customersStoreFeatures
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearCustomersStoreEnv(t *testing.T) {
	t.Helper()
	clearEnrichmentEnvDashboard(t)
	t.Setenv(config.EnvCustomersStore, "")
	t.Setenv(config.EnvCustomersEnrichmentAutoAccept, "")
}

// The store mode and the auto-accept policy default to shadow (#19972) and
// off, only
// an owner can change them, the saved value round-trips with its effective
// value, an unrelated PUT leaves them alone, and an empty value goes back to
// the default.
func TestGovernorFeatures_CustomersStoreAndAutoAcceptRoundTrip(t *testing.T) {
	clearCustomersStoreEnv(t)
	s := covApiServer(t)
	got := readCustomersStoreFeatures(t, s).Features
	if got.Store != "" || got.StoreEffective != config.CustomersStoreShadow ||
		got.AutoAccept != "" || got.AutoAcceptEffective != config.CustomersAutoAcceptOff {
		t.Fatalf("default payload = %+v, want unset, shadow and off", got)
	}
	if got.Defaults.Store != config.CustomersStoreShadow || got.Defaults.AutoAccept != config.CustomersAutoAcceptOff {
		t.Fatalf("defaults = %+v", got.Defaults)
	}

	body := map[string]any{
		"customersEnrichmentEnabled":      true,
		"customersEnrichmentGitdmEnabled": true,
		"customersEnrichmentAutoAccept":   " GITDM_HIGH ",
		"customersStore":                  "Shadow",
	}
	if rec := putFeatures(s, body, func(r *http.Request) {
		r.Header.Set("X-Hive-Role", "read-write")
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT = %d, want 403", rec.Code)
	}
	if s.deps.Config.Customers.Store != "" || s.deps.Config.Customers.Enrichment.AutoAccept != "" {
		t.Fatal("a refused PUT changed the config")
	}
	if rec := doPut(s, "/api/config/governor/features", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d - %s", rec.Code, rec.Body.String())
	}
	if c := s.deps.Config.Customers; c.Store != config.CustomersStoreShadow || c.Enrichment.AutoAccept != config.CustomersAutoAcceptGitdmHigh {
		t.Fatalf("saved store=%q auto_accept=%q, want normalised shadow and gitdm_high", c.Store, c.Enrichment.AutoAccept)
	}
	got = readCustomersStoreFeatures(t, s).Features
	if got.Store != config.CustomersStoreShadow || got.StoreEffective != config.CustomersStoreShadow ||
		got.AutoAccept != config.CustomersAutoAcceptGitdmHigh || got.AutoAcceptEffective != config.CustomersAutoAcceptGitdmHigh {
		t.Fatalf("after PUT = %+v", got)
	}

	if rec := doPut(s, "/api/config/governor/features", map[string]any{"retroEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT: %d", rec.Code)
	}
	if c := s.deps.Config.Customers; c.Store != config.CustomersStoreShadow || c.Enrichment.AutoAccept != config.CustomersAutoAcceptGitdmHigh {
		t.Fatal("a PUT that did not mention them changed the store or auto-accept")
	}

	// gitdm off: the saved policy stays, the effective one is off.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersEnrichmentGitdmEnabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT gitdm off: %d", rec.Code)
	}
	got = readCustomersStoreFeatures(t, s).Features
	if got.AutoAccept != config.CustomersAutoAcceptGitdmHigh || got.AutoAcceptEffective != config.CustomersAutoAcceptOff {
		t.Fatalf("gitdm off = %+v, want saved gitdm_high, effective off", got)
	}

	// Env overrides are reported as effective.
	t.Setenv(config.EnvCustomersStore, config.CustomersStoreColony)
	if got = readCustomersStoreFeatures(t, s).Features; got.Store != config.CustomersStoreShadow || got.StoreEffective != config.CustomersStoreColony {
		t.Fatalf("env override = %+v, want saved shadow, effective colony", got)
	}
	t.Setenv(config.EnvCustomersStore, "")

	// Empty returns both to the default.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersStore": "", "customersEnrichmentAutoAccept": ""}); rec.Code != http.StatusOK {
		t.Fatalf("PUT empty: %d", rec.Code)
	}
	if c := s.deps.Config.Customers; c.Store != "" || c.Enrichment.AutoAccept != "" {
		t.Fatalf("empty saved store=%q auto_accept=%q, want both unset", c.Store, c.Enrichment.AutoAccept)
	}
	if got = readCustomersStoreFeatures(t, s).Features; got.StoreEffective != config.CustomersStoreShadow {
		t.Fatalf("store effective after clear = %q, want the shadow default", got.StoreEffective)
	}

	// An explicit json is saved and stays json: it is the rollback.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersStore": "json"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT json: %d", rec.Code)
	}
	if got = readCustomersStoreFeatures(t, s).Features; got.Store != config.CustomersStoreJSON || got.StoreEffective != config.CustomersStoreJSON {
		t.Fatalf("explicit json = %+v, want saved and effective json", got)
	}
}

// An unknown mode or policy is refused, and nothing in the same PUT is
// applied, so a typo can never be saved and later read as the default.
func TestGovernorFeatures_CustomersStoreRejectsUnknownValues(t *testing.T) {
	clearCustomersStoreEnv(t)
	s := covApiServer(t)
	for _, body := range []map[string]any{
		{"customersEnrichmentEnabled": true, "customersStore": "colony-only"},
		{"customersEnrichmentEnabled": true, "customersStore": "sqlite"},
		{"customersEnrichmentEnabled": true, "customersEnrichmentAutoAccept": "all"},
		{"customersEnrichmentEnabled": true, "customersEnrichmentAutoAccept": "gitdm"},
	} {
		rec := doPut(s, "/api/config/governor/features", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v = %d, want 400", body, rec.Code)
		}
		if c := s.deps.Config.Customers; c.Enrichment.IsEnabled() || c.Store != "" || c.Enrichment.AutoAccept != "" {
			t.Fatalf("%v changed the config: store=%q auto_accept=%q", body, c.Store, c.Enrichment.AutoAccept)
		}
	}
}

// Both keys have a Settings control (their parity allowlist entries were
// removed in #19857) and a search entry.
func TestFeaturesTab_CustomersStoreMarkup(t *testing.T) {
	html := featuresMarkup(t)
	for _, want := range []string{
		`yamlHint('customers.store')`,
		`yamlHint('customers.enrichment.auto_accept')`,
		`data-arg1="customersStore" data-arg-types="s,s,v"`,
		`data-arg1="customersEnrichmentAutoAccept" data-arg-types="s,s,v"`,
		`['shadow', 'shadow']`,
		`['colony', 'colony']`,
		`['gitdm_high', 'gitdm_high']`,
		`key: 'customers.store'`,
		`key: 'customers.enrichment.auto_accept'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, `'colony-only'`) {
		t.Error("the Settings select offers colony-only, which no release implements yet")
	}
}
