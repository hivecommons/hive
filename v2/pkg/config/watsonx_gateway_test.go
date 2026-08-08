package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A watsonx gateway parses from YAML with its kind, endpoint, project_id, and a
// key reference, and resolves its key from the referenced file — exactly like
// any other gateway. project_id/region are plain (non-secret) inline fields.
func TestWatsonxGatewayParse(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "wx_api_key")
	if err := os.WriteFile(keyFile, []byte("ibm-cloud-key-VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	y := `
name: watsonx
kind: watsonx
endpoint: https://us-south.ml.cloud.ibm.com/ml/gateway
project_id: 63dc4cf1-252f-424b-b52d-5cdd9814987f
region: us-south
api_key_file: ` + keyFile + `
default_model: ibm/granite-4-h-small
`
	var gw GatewayConfig
	if err := yaml.Unmarshal([]byte(y), &gw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if gw.Kind != GatewayKindWatsonx {
		t.Fatalf("kind = %q, want %q", gw.Kind, GatewayKindWatsonx)
	}
	if gw.ProjectID != "63dc4cf1-252f-424b-b52d-5cdd9814987f" {
		t.Fatalf("project_id = %q", gw.ProjectID)
	}
	if gw.Region != "us-south" {
		t.Fatalf("region = %q", gw.Region)
	}
	if gw.Endpoint != "https://us-south.ml.cloud.ibm.com/ml/gateway" {
		t.Fatalf("endpoint = %q", gw.Endpoint)
	}
	if got := gw.ResolveAPIKey(); got != "ibm-cloud-key-VALUE" {
		t.Fatalf("ResolveAPIKey = %q, want the file's key", got)
	}
}

// omitempty keeps project_id/region out of the serialized gateway for non-watsonx
// kinds, so existing gateways round-trip byte-identical (backward compatibility).
func TestNonWatsonxGatewayOmitsWatsonxFields(t *testing.T) {
	gw := GatewayConfig{
		Name:     "openrouter",
		Kind:     GatewayKindOpenRouter,
		Endpoint: "https://openrouter.ai/api/v1",
	}
	out, err := yaml.Marshal(gw)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "project_id") || strings.Contains(s, "region") {
		t.Fatalf("non-watsonx gateway serialized watsonx-only fields:\n%s", s)
	}
}

// A watsonx gateway name is accepted as a valid agent backend by validate(),
// just like any other gateway (routing resolves it via ResolveGateway).
func TestWatsonxGatewayNameValidAsBackend(t *testing.T) {
	c := &Config{
		Project: ProjectConfig{Org: "acme"},
		GitHub:  GitHubConfig{Token: "x"},
		Agents: map[string]AgentConfig{
			"granite-scanner": {Backend: "watsonx"},
		},
		Governor: GovernorConfig{
			Gateways: []GatewayConfig{
				{
					Name:      "watsonx",
					Kind:      GatewayKindWatsonx,
					Endpoint:  "https://us-south.ml.cloud.ibm.com/ml/gateway",
					ProjectID: "proj-1",
				},
			},
		},
	}
	if err := c.validate(); err != nil {
		t.Fatalf("watsonx gateway name should be a valid backend, got: %v", err)
	}
}
