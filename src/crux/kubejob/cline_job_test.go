package kubejob

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/cline"
	"github.com/hivecommons/hive/crux/sandbox"
)

func TestClineJobUsesSecretReferenceNotHostCredentialMaterial(t *testing.T) {
	t.Setenv(cline.ProviderSecretEnv, "must-not-copy-host-credential")
	l := &Launcher{Options: Options{WorkspaceClaim: "hive-data", EnvFromSecrets: []string{"cline-provider"}}}
	manifest := l.manifest("spoke", "cline-kick", "kick-workspace", sandbox.LaunchSpec{
		Image: "cline-image", Workspace: "/data/kick-workspace",
		Command:      []string{"crux", "cline-turn", "--provider-secret", "--model", "openrouter/test"},
		EnvAllowlist: []string{cline.ProviderSecretEnv},
		Env:          map[string]string{cline.ProviderSecretEnv: "must-not-copy-host-credential"},
	})
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "must-not-copy-host-credential") ||
		!strings.Contains(string(raw), `"secretRef":{"name":"cline-provider"}`) {
		t.Fatal("Job must reference only the explicit Secret, not copy its material")
	}
	if strings.Contains(string(raw), `"hostPath"`) || strings.Contains(string(raw), cline.ProviderSecretEnv) {
		t.Fatal("Job added a host credential mount or explicit credential environment")
	}
}
