package dashboard

import (
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The Settings form mirrors the reserved pod metadata keys from crux/config;
// fail if the JS copy drifts from the Go constants.
func TestSandboxReservedPodKeysMatchJS(t *testing.T) {
	src, err := os.ReadFile("static-src/js/config-sandbox-source.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	for _, want := range []string{
		"const SANDBOX_RESERVED_POD_LABELS = ['" + config.SandboxPodJobNameLabel + "', '" + config.SandboxPodManagedByLabel + "'];",
		"const SANDBOX_RESERVED_POD_LABEL_PREFIX = '" + config.SandboxPodReservedLabelPrefix + "';",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("config-sandbox-source.js is missing %q", want)
		}
	}
}

func TestSandboxMetadataMapRefusesCredentials(t *testing.T) {
	_, err := sandboxMetadataMapFromBody("job.podAnnotations", map[string]interface{}{"example.com/token": "ghp_" + strings.Repeat("a", 36)})
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("err = %v, want credential refusal", err)
	}
	got, err := sandboxMetadataMapFromBody("job.podAnnotations", map[string]interface{}{"example.com/note": "  hello  "})
	if err != nil || got["example.com/note"] != "  hello  " {
		t.Fatalf("got %v, %v; want annotation value kept untrimmed", got, err)
	}
}
