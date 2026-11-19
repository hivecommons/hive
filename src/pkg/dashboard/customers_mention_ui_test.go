package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestGovernorFeatures_CustomersMentionSearchToggleRoundTrip covers
// customers.mention_search.enabled: default off, owner-only, persisted,
// reported back with the effective value, left alone by a PUT that does not
// mention it, overridden by the env var, and off whenever Customers is off.
func TestGovernorFeatures_CustomersMentionSearchToggleRoundTrip(t *testing.T) {
	t.Setenv(config.EnvCustomersEnabled, "")
	t.Setenv(config.EnvCustomersMentionSearchEnabled, "")
	s := covApiServer(t)
	type payload struct {
		Features struct {
			Search          bool `json:"customersSearch"`
			SearchEffective bool `json:"customersSearchEffective"`
		} `json:"features"`
	}
	read := func() payload {
		var p payload
		if err := json.Unmarshal(doOwnerGet(s, "/api/config/governor").Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := read(); got.Features.Search || got.Features.SearchEffective {
		t.Fatalf("default payload = %+v, want off", got.Features)
	}
	if rec := putFeatures(s, map[string]any{"customersSearchEnabled": true}, func(r *http.Request) {
		r.Header.Set("X-Hive-Role", "read-write")
	}); rec.Code != http.StatusForbidden || s.deps.Config.Customers.MentionSearch.IsEnabled() {
		t.Fatalf("non-owner PUT = %d, enabled=%v", rec.Code, s.deps.Config.Customers.MentionSearch.IsEnabled())
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersSearchEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("PUT on: %d - %s", rec.Code, rec.Body.String())
	}
	if got := read(); !got.Features.Search || !got.Features.SearchEffective {
		t.Fatalf("after enable = %+v", got.Features)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"retroEnabled": true}); rec.Code != http.StatusOK || !s.deps.Config.Customers.MentionSearch.IsEnabled() {
		t.Fatalf("an unrelated PUT changed it: %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersEnabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT customers off: %d", rec.Code)
	}
	if got := read(); !got.Features.Search || got.Features.SearchEffective {
		t.Fatalf("customers off = %+v, want saved on, effective off", got.Features)
	}
	t.Setenv(config.EnvCustomersEnabled, "true")
	t.Setenv(config.EnvCustomersMentionSearchEnabled, "false")
	if got := read(); !got.Features.Search || got.Features.SearchEffective {
		t.Fatalf("env override = %+v", got.Features)
	}
}

func TestFeaturesTab_CustomersMentionSearchToggleMarkup(t *testing.T) {
	html := featuresMarkup(t)
	for _, want := range []string{
		`data-section="features" data-key="customersSearchEnabled"`,
		`yamlHint('customers.mention_search.enabled')`,
		`f.customersSearchEffective !== f.customersSearch`,
		`HIVE_CUSTOMERS_MENTION_SEARCH_ENABLED (or turning Customers off) overrides this setting`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
