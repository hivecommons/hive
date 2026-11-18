package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestGovernorFeatures_CustomersInferenceToggleRoundTrip covers
// customers.inference.enabled: default off, owner-only, persisted, reported
// back with the effective value, left alone by a PUT that does not mention
// it, overridden by the env var, and off whenever Customers is off.
func TestGovernorFeatures_CustomersInferenceToggleRoundTrip(t *testing.T) {
	t.Setenv(config.EnvCustomersEnabled, "")
	t.Setenv(config.EnvCustomersInferenceEnabled, "")
	s := covApiServer(t)
	type payload struct {
		Features struct {
			Inference          bool `json:"customersInference"`
			InferenceEffective bool `json:"customersInferenceEffective"`
		} `json:"features"`
	}
	read := func() payload {
		var p payload
		if err := json.Unmarshal(doOwnerGet(s, "/api/config/governor").Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := read(); got.Features.Inference || got.Features.InferenceEffective {
		t.Fatalf("default payload = %+v, want off", got.Features)
	}
	if rec := putFeatures(s, map[string]any{"customersInferenceEnabled": true}, func(r *http.Request) {
		r.Header.Set("X-Hive-Role", "read-write")
	}); rec.Code != http.StatusForbidden || s.deps.Config.Customers.Inference.IsEnabled() {
		t.Fatalf("non-owner PUT = %d, enabled=%v", rec.Code, s.deps.Config.Customers.Inference.IsEnabled())
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersInferenceEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("PUT on: %d - %s", rec.Code, rec.Body.String())
	}
	if got := read(); !got.Features.Inference || !got.Features.InferenceEffective {
		t.Fatalf("after enable = %+v", got.Features)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"retroEnabled": true}); rec.Code != http.StatusOK || !s.deps.Config.Customers.Inference.IsEnabled() {
		t.Fatalf("an unrelated PUT changed it: %d", rec.Code)
	}
	// Customers off turns inference off in effect, and the payload says so.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"customersEnabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT customers off: %d", rec.Code)
	}
	if got := read(); !got.Features.Inference || got.Features.InferenceEffective {
		t.Fatalf("customers off = %+v, want saved on, effective off", got.Features)
	}
	t.Setenv(config.EnvCustomersEnabled, "true")
	t.Setenv(config.EnvCustomersInferenceEnabled, "false")
	if got := read(); !got.Features.Inference || got.Features.InferenceEffective {
		t.Fatalf("env override = %+v", got.Features)
	}
}

func TestFeaturesTab_CustomersInferenceToggleMarkup(t *testing.T) {
	html := featuresMarkup(t)
	for _, want := range []string{
		`data-section="features" data-key="customersInferenceEnabled"`,
		`yamlHint('customers.inference.enabled')`,
		`f.customersInferenceEffective !== f.customersInference`,
		`HIVE_CUSTOMERS_INFERENCE_ENABLED (or turning Customers off) overrides this setting`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
