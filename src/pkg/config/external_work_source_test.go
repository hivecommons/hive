package config

import "testing"

func validExternalSourceConfig() ExternalSourceConfig {
	return ExternalSourceConfig{
		Name:        "acme",
		DisplayName: "Acme Tracker",
		BaseURL:     "https://acme-shim.internal:8443",
		AuthToken:   "$ACME_WORKSOURCE_TOKEN",
		Repos:       []string{"your-org/app", "your-org/platform"},
		HoldLabels:  []string{"hold"},
	}
}

func TestExternalSourceConfig_ValidAcceptsADRExample(t *testing.T) {
	if err := validExternalSourceConfig().Validate(); err != nil {
		t.Fatalf("Validate() on the ADR-0020 example = %v, want nil", err)
	}
}

// TestExternalSourceConfig_FailsClosed covers every rule from ADR-0020's
// Configuration section. Each case is a way an operator (or a dashboard write)
// could otherwise produce a source that lists work Hive cannot safely key,
// reach, or authenticate.
func TestExternalSourceConfig_FailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		mutit func(*ExternalSourceConfig)
	}{
		{"empty name", func(e *ExternalSourceConfig) { e.Name = "" }},
		{"name with uppercase", func(e *ExternalSourceConfig) { e.Name = "Acme" }},
		{"name too short", func(e *ExternalSourceConfig) { e.Name = "a" }},
		{"name too long", func(e *ExternalSourceConfig) { e.Name = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{"name with separator", func(e *ExternalSourceConfig) { e.Name = "acme!x" }},
		{"name with slash", func(e *ExternalSourceConfig) { e.Name = "acme/x" }},
		{"reserved name github", func(e *ExternalSourceConfig) { e.Name = "github" }},
		{"reserved name run", func(e *ExternalSourceConfig) { e.Name = "run" }},
		{"reserved name external", func(e *ExternalSourceConfig) { e.Name = "external" }},
		{"missing base_url", func(e *ExternalSourceConfig) { e.BaseURL = "" }},
		{"non-loopback http base_url", func(e *ExternalSourceConfig) { e.BaseURL = "http://acme.example" }},
		{"non-http scheme", func(e *ExternalSourceConfig) { e.BaseURL = "file:///etc/passwd" }},
		{"base_url without host", func(e *ExternalSourceConfig) { e.BaseURL = "https:///v1" }},
		{"missing auth_token", func(e *ExternalSourceConfig) { e.AuthToken = "" }},
		{"literal auth_token", func(e *ExternalSourceConfig) { e.AuthToken = "shhh-real-token" }},
		{"partial reference auth_token", func(e *ExternalSourceConfig) { e.AuthToken = "Bearer ${ACME_TOKEN}" }},
		{"literal ca_bundle", func(e *ExternalSourceConfig) { e.CABundle = "-----BEGIN CERTIFICATE-----" }},
		{"empty repos", func(e *ExternalSourceConfig) { e.Repos = nil }},
		{"repo without owner", func(e *ExternalSourceConfig) { e.Repos = []string{"app"} }},
		{"repo with empty half", func(e *ExternalSourceConfig) { e.Repos = []string{"your-org/"} }},
		{"repo with extra segment", func(e *ExternalSourceConfig) { e.Repos = []string{"your-org/app/extra"} }},
		{"negative timeout", func(e *ExternalSourceConfig) { e.TimeoutSeconds = -1 }},
		{"timeout above the cap", func(e *ExternalSourceConfig) { e.TimeoutSeconds = MaxExternalTimeoutSeconds + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validExternalSourceConfig()
			tc.mutit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want an error for %s", tc.name)
			}
		})
	}
}

func TestExternalSourceConfig_AcceptsLoopbackSidecarAndBracedRefs(t *testing.T) {
	for _, base := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		cfg := validExternalSourceConfig()
		cfg.BaseURL = base
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with base_url %q = %v, want nil", base, err)
		}
	}
	cfg := validExternalSourceConfig()
	cfg.AuthToken = "${ACME_WORKSOURCE_TOKEN}"
	cfg.CABundle = "${ACME_WORKSOURCE_CA}"
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() with braced references = %v, want nil", err)
	}
}

func TestExternalSourceConfig_LabelAndTimeoutDefaults(t *testing.T) {
	cfg := validExternalSourceConfig()
	if got := cfg.Label(); got != "Acme Tracker" {
		t.Errorf("Label() = %q, want the display name", got)
	}
	cfg.DisplayName = ""
	if got := cfg.Label(); got != "acme" {
		t.Errorf("Label() without display_name = %q, want the name", got)
	}
	if got := cfg.EffectiveTimeoutSeconds(); got != DefaultExternalTimeoutSeconds {
		t.Errorf("EffectiveTimeoutSeconds() = %d, want the %d default", got, DefaultExternalTimeoutSeconds)
	}
	cfg.TimeoutSeconds = 5
	if got := cfg.EffectiveTimeoutSeconds(); got != 5 {
		t.Errorf("EffectiveTimeoutSeconds() = %d, want 5", got)
	}
}

// TestWorkSourceConfig_ExternalTypeValidates checks the type switch routes to
// the external block, and that an unknown type is still an error.
func TestWorkSourceConfig_ExternalTypeValidates(t *testing.T) {
	ok := WorkSourceConfig{Type: "external", External: validExternalSourceConfig()}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	bad := WorkSourceConfig{Type: "external"}
	if err := bad.Validate(); err == nil {
		t.Fatal("type=external with an empty block must fail closed")
	}
	if err := (WorkSourceConfig{Type: "not-a-source"}).Validate(); err == nil {
		t.Fatal("an unknown work_source type must still be a config error")
	}
}

// TestWorkSourceConfig_IsZeroCountsExternal guards the dashboard-overlay reload:
// an operator-set external block must not look like "no work source set".
func TestWorkSourceConfig_IsZeroCountsExternal(t *testing.T) {
	if !(WorkSourceConfig{}).IsZero() {
		t.Fatal("an empty WorkSourceConfig must be zero")
	}
	if (WorkSourceConfig{External: ExternalSourceConfig{Name: "acme"}}).IsZero() {
		t.Fatal("a configured external block must not read as zero")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "127.9.9.9": true,
		"::1": true, "[::1]": true, "acme.example": false, "10.0.0.1": false, "": false,
	}
	for host, want := range cases {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %t, want %t", host, got, want)
		}
	}
}
