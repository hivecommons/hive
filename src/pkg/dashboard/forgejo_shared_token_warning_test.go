package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The forge settings carry the same shared-write-token warning boot logs
// (hivecommons/hive#19917): set for Forgejo repos without
// forgejo.integration, empty otherwise.
func TestFeaturesPlatformResponseForgejoSharedTokenWarning(t *testing.T) {
	cfg := &config.Config{}
	if w := featuresPlatformResponse(cfg)["forgejoSharedTokenWarning"]; w != "" {
		t.Fatalf("GitHub-only hive warning = %q, want empty", w)
	}
	cfg.Project.Forge = config.ForgeForgejo
	w, _ := featuresPlatformResponse(cfg)["forgejoSharedTokenWarning"].(string)
	if w == "" || w != cfg.ForgejoSharedWriteTokenWarning() {
		t.Fatalf("forgejo hive warning = %q, want the config's warning", w)
	}
	cfg.Forgejo.Integration = &config.ForgejoIntegrationConfig{}
	if w := featuresPlatformResponse(cfg)["forgejoSharedTokenWarning"]; w != "" {
		t.Fatalf("warning with forgejo.integration = %q, want empty", w)
	}
}

// The Settings dialog renders the warning, escaped, under the Forgejo fields.
func TestForgejoSharedTokenWarningRendered(t *testing.T) {
	html := indexHTML(t)
	const marker = `data-testid="forgejo-shared-token-warning">${esc(f.forgejoSharedTokenWarning)}</div>`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatal("the dialog does not render forgejoSharedTokenWarning escaped")
	}
	if j := strings.Index(html, `data-arg1="forgejoTokenEnv"`); j < 0 || j > i {
		t.Fatal("the warning is not rendered after the Forgejo token field")
	}
}
