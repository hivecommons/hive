package config

import (
	"strings"
	"testing"
)

// OAuth client registrations (zoom, teams) are referenced by environment
// variable NAME in auth.*_env; values are still rejected everywhere.
func TestValidateKnowledgeConnectorsOAuthClientAuth(t *testing.T) {
	base := func(auth KnowledgeConnectorAuth) error {
		return ValidateKnowledgeConnectors([]KnowledgeConnector{{Name: "calls", Type: "zoom", Layer: "personal", Auth: auth}})
	}
	if err := base(KnowledgeConnectorAuth{Mode: KnowledgeConnectorAuthModeUserOAuth, ClientIDEnv: "ZOOM_CLIENT_ID", ClientSecretEnv: "ZOOM_CLIENT_SECRET"}); err != nil {
		t.Fatalf("env-name references must validate: %v", err)
	}
	if err := base(KnowledgeConnectorAuth{Mode: KnowledgeConnectorAuthModeServerToServer, ClientIDEnv: "A", ClientSecretEnv: "B", AccountIDEnv: "C", TenantIDEnv: "D"}); err != nil {
		t.Fatalf("all env references must validate: %v", err)
	}
	if err := base(KnowledgeConnectorAuth{Mode: "implicit", ClientIDEnv: "A", ClientSecretEnv: "B"}); err == nil || !strings.Contains(err.Error(), "auth.mode") {
		t.Fatalf("unknown mode: %v", err)
	}
	if err := base(KnowledgeConnectorAuth{ClientIDEnv: "A", ClientSecretEnv: "not a name!"}); err == nil || !strings.Contains(err.Error(), "auth.client_secret_env") {
		t.Fatalf("a value in place of a name must be rejected: %v", err)
	}
	if err := base(KnowledgeConnectorAuth{ClientIDEnv: "A", Inline: map[string]any{"client_secret": "pasted"}}); err == nil || !strings.Contains(err.Error(), "inline secrets are rejected") {
		t.Fatalf("inline client_secret must be rejected: %v", err)
	}
}
