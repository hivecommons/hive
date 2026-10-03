package worksource

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func externalSourceConfigForFactory() config.WorkSourceConfig {
	return config.WorkSourceConfig{
		Type: "external",
		External: config.ExternalSourceConfig{
			Name:        "acme",
			DisplayName: "Acme Tracker",
			BaseURL:     "https://acme-shim.internal:8443",
			AuthToken:   "$ACME_WORKSOURCE_TOKEN",
			Repos:       []string{"your-org/app"},
			HoldLabels:  []string{"hold"},
		},
	}
}

// TestFromConfig_ExternalResolvesThroughRegistry is the wiring guard: the
// registry turns work_source.type=external into the adapter, and SourceType()
// is the operator-chosen name rather than the literal "external".
func TestFromConfig_ExternalResolvesThroughRegistry(t *testing.T) {
	t.Setenv("ACME_WORKSOURCE_TOKEN", "provider-token")

	ws, err := FromConfig(externalSourceConfigForFactory(), nil, "", "", slog.Default())
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	src, ok := ws.(*externalSource)
	if !ok {
		t.Fatalf("FromConfig returned %T, want *externalSource", ws)
	}
	if got := src.SourceType(); got != "acme" {
		t.Errorf("SourceType() = %q, want the configured name", got)
	}
	if got := src.DisplayName(); got != "Acme Tracker" {
		t.Errorf("DisplayName() = %q", got)
	}
	if src.cfg.AuthToken != "provider-token" {
		t.Errorf("auth token was not resolved from the environment reference: %q", src.cfg.AuthToken)
	}
	if src.cfg.Timeout.Seconds() != float64(config.DefaultExternalTimeoutSeconds) {
		t.Errorf("Timeout = %v, want the %ds default", src.cfg.Timeout, config.DefaultExternalTimeoutSeconds)
	}
	if _, allowed := src.repos["your-org/app"]; !allowed {
		t.Errorf("repos allow-list = %v", src.repos)
	}
}

// TestFromConfig_ExternalFailsClosedOnBadConfig: a block that would not pass
// config validation must not produce a work source, because a constructed-but-
// wrong source enumerates work the operator never authorized.
func TestFromConfig_ExternalFailsClosedOnBadConfig(t *testing.T) {
	t.Setenv("ACME_WORKSOURCE_TOKEN", "provider-token")

	cases := []struct {
		name  string
		mutit func(*config.ExternalSourceConfig)
	}{
		{"missing name", func(e *config.ExternalSourceConfig) { e.Name = "" }},
		{"reserved name", func(e *config.ExternalSourceConfig) { e.Name = "linear" }},
		{"plain http base_url", func(e *config.ExternalSourceConfig) { e.BaseURL = "http://acme.example" }},
		{"literal token", func(e *config.ExternalSourceConfig) { e.AuthToken = "a-real-token" }},
		{"empty repos", func(e *config.ExternalSourceConfig) { e.Repos = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := externalSourceConfigForFactory()
			tc.mutit(&cfg.External)
			if _, err := FromConfig(cfg, nil, "", "", slog.Default()); err == nil {
				t.Fatal("FromConfig must fail closed")
			}
		})
	}
}

// TestFromConfig_ExternalUnsetEnvRefNamesVariableNotValue keeps the secrets
// rule honest: the operator needs to know which variable to set, and the log
// must not become the place the credential leaks.
func TestFromConfig_ExternalUnsetEnvRefNamesVariableNotValue(t *testing.T) {
	cfg := externalSourceConfigForFactory()
	cfg.External.AuthToken = "${ACME_UNSET_WORKSOURCE_TOKEN}"

	_, err := FromConfig(cfg, nil, "", "", slog.Default())
	if err == nil {
		t.Fatal("an unset environment reference must fail closed")
	}
	if !strings.Contains(err.Error(), "ACME_UNSET_WORKSOURCE_TOKEN") {
		t.Errorf("error = %v, want it to name the environment variable", err)
	}
	if !strings.Contains(err.Error(), "work_source.external.auth_token") {
		t.Errorf("error = %v, want it to name the config field", err)
	}
}

// TestFromConfig_ExternalResolvesCABundleReference checks the optional bundle
// goes through the same resolve-at-use-time path as the token.
func TestFromConfig_ExternalResolvesCABundleReference(t *testing.T) {
	cfg := externalSourceConfigForFactory()
	cfg.External.CABundle = "$ACME_UNSET_WORKSOURCE_CA"
	t.Setenv("ACME_WORKSOURCE_TOKEN", "provider-token")

	if _, err := FromConfig(cfg, nil, "", "", slog.Default()); err == nil {
		t.Fatal("an unset ca_bundle reference must fail closed rather than disable verification")
	}
}

// TestRegisterPrimary_DuplicatePanics mirrors RegisterAdditive: two builders
// for one type would make the selected source depend on link order.
func TestRegisterPrimary_DuplicatePanics(t *testing.T) {
	const name = "test-primary-dup"
	RegisterPrimary(name, func(config.WorkSourceConfig, PrimaryDeps) (WorkSource, error) { return nil, nil })
	defer func() {
		if recover() == nil {
			t.Fatal("second RegisterPrimary with the same name should panic")
		}
	}()
	RegisterPrimary(name, func(config.WorkSourceConfig, PrimaryDeps) (WorkSource, error) { return nil, nil })
}

// TestRegisterPrimary_BuiltInsRegistered guards the refactor: every type that
// FromConfig's switch used to accept is still in the registry.
func TestRegisterPrimary_BuiltInsRegistered(t *testing.T) {
	for _, name := range []string{
		PrimaryGitHub, PrimaryGitHubProjects, PrimaryLinear,
		PrimaryJira, PrimaryGitea, PrimaryGitLab, PrimaryExternal,
	} {
		if _, ok := lookupPrimary(name); !ok {
			t.Errorf("primary source %q is not registered", name)
		}
	}
	if _, ok := lookupPrimary("not-a-source"); ok {
		t.Error("an unknown type must not resolve to a builder")
	}
}

// TestFromConfig_ExternalDoesNotReceiveGitHubCredentials: the provider gets its
// own bearer token and nothing else (ADR-0020, "Secrets").
func TestFromConfig_ExternalDoesNotReceiveGitHubCredentials(t *testing.T) {
	t.Setenv("ACME_WORKSOURCE_TOKEN", "provider-token")

	ws, err := FromConfig(externalSourceConfigForFactory(), nil, "ghs_installation_token", "my-org", slog.Default())
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	src := ws.(*externalSource)
	if strings.Contains(src.cfg.AuthToken, "ghs_") {
		t.Fatalf("external source received a GitHub token: %q", src.cfg.AuthToken)
	}
	if src.cfg.AuthToken != "provider-token" {
		t.Fatalf("AuthToken = %q, want only the provider's own token", src.cfg.AuthToken)
	}
}

// TestFromConfig_ExternalComposesWithAdditiveSources checks the external
// primary keeps the additive composition behavior every other primary has.
func TestFromConfig_ExternalComposesWithAdditiveSources(t *testing.T) {
	t.Setenv("ACME_WORKSOURCE_TOKEN", "provider-token")

	cfg := externalSourceConfigForFactory()
	cfg.RunStages = true
	ws, err := FromConfig(cfg, nil, "", "", slog.Default())
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if _, ok := ws.(*Composite); !ok {
		t.Fatalf("RunStages=true should wrap the external primary in Composite, got %T", ws)
	}
	if got := ws.SourceType(); got != "acme" {
		t.Fatalf("SourceType = %q, want the external primary's name", got)
	}
}
