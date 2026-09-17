package dashboard

import (
	"strings"
	"testing"
)

func TestProjectObservabilityUIWiring(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{
		"Project Observability",
		"function renderGovProjectObservability",
		"project_observability",
		"project-observability",
		"operabilityAgentStatusRow",
		"telemetry_status",
		"operations_status",
		"Endpoint environment variable",
		"Credential secret reference",
		"detected platform suggestions",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("UI missing %q", want)
		}
	}
}
