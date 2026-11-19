package hub

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestLandingIntegrationLogosAreEmbedded guards the landing page's logo rows:
// every /static/integrations/ image the page references must be embedded, so
// a renamed or missing asset fails here instead of rendering a broken chip.
func TestLandingIntegrationLogosAreEmbedded(t *testing.T) {
	b, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatalf("reading embedded index.html: %v", err)
	}
	refs := regexp.MustCompile(`src="/static/integrations/([^"]+)"`).FindAllStringSubmatch(string(b), -1)
	if len(refs) == 0 {
		t.Fatal("index.html references no /static/integrations/ logos")
	}
	for _, m := range refs {
		if _, err := fs.Stat(staticFS, "static/integrations/"+m[1]); err != nil {
			t.Errorf("index.html references /static/integrations/%s, which is not embedded: %v", m[1], err)
		}
	}
}

// TestLandingIsolationAndSecretsRow pins the "Isolation and secrets" row
// (#18217): the sandbox runtimes and the OpenBao keystore use their own
// project logos in the shared chip style.
func TestLandingIsolationAndSecretsRow(t *testing.T) {
	b, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatalf("reading embedded index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, `id="isolation-secrets-title"`)
	if start < 0 {
		t.Fatal("index.html has no Isolation and secrets group")
	}
	end := strings.Index(html[start:], "</section>")
	if end < 0 {
		t.Fatal("Isolation and secrets group is not closed")
	}
	group := html[start : start+end]
	if !strings.Contains(group, `class="hv-infra-logo-row"`) {
		t.Error("Isolation and secrets group does not use hv-infra-logo-row")
	}
	for _, logo := range []string{"openshell.svg", "agent-substrate.png", "openbao.svg", "podman.svg", "kubernetes.svg"} {
		if !strings.Contains(group, `src="/static/integrations/`+logo+`"`) {
			t.Errorf("Isolation and secrets group is missing %s", logo)
		}
	}
	if got := strings.Count(group, `class="hv-infra-logo-chip"`); got != 5 {
		t.Errorf("Isolation and secrets group has %d chips, want 5", got)
	}
}
