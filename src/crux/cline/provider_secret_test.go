package cline

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderSecretNarrowingAndFailures(t *testing.T) {
	tests := []struct {
		name, encoded string
		valid         bool
	}{
		{"selected only", base64.StdEncoding.EncodeToString([]byte(providerConfig)), true},
		{"missing", "", false},
		{"bad base64", "invalid-secret-value", false},
		{"bad JSON", base64.StdEncoding.EncodeToString([]byte("invalid-secret-value")), false},
		{"wrong provider", base64.StdEncoding.EncodeToString([]byte(`{"version":1,"providers":{}}`)), false},
		{"OAuth", base64.StdEncoding.EncodeToString([]byte(`{"version":1,"providers":{"openrouter":{"settings":{},"tokenSource":"oauth"}}}`)), false},
		{"oversized", strings.Repeat("A", base64.StdEncoding.EncodedLen(maxProviderSecretBytes)+4), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(ProviderSecretEnv, tt.encoded)
			out, err := selectedProviderSecret("openrouter")
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v, err=%v", tt.valid, err)
			}
			if _, found := os.LookupEnv(ProviderSecretEnv); found {
				t.Fatal("provider transport was not removed")
			}
			if tt.valid && (!strings.Contains(string(out), `"openrouter"`) || strings.Contains(string(out), "must-not-copy")) {
				t.Fatal("provider narrowing failed")
			}
			if err != nil && strings.Contains(err.Error(), "invalid-secret-value") {
				t.Fatal("credential content leaked in error")
			}
		})
	}
}

func TestProviderSecretTurnAndNoFallback(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("HOME", root)
	t.Setenv("HIVE_CLINE_CONFIG_DIR", "")
	binary := fakeCLI(t, `
test -z "${HIVE_CLINE_PROVIDERS_B64+x}" || exit 81
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--config" ]; then shift; config="$1"; fi
  shift
done
grep -q test-key "$config/data/settings/providers.json" || exit 82
if grep -q must-not-copy "$config/data/settings/providers.json"; then exit 83; fi
cat >/dev/null
printf '%s\n' '{"type":"run_result","finishReason":"completed","text":"done"}'
`)
	t.Setenv(ProviderSecretEnv, base64.StdEncoding.EncodeToString([]byte(providerConfig)))
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--cline", binary, "--model", "openrouter/anthropic/test", "--provider-secret",
		"--cwd", workspace, "--state-dir", state}, strings.NewReader("bounded task"), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
	if !strings.Contains(stdout.String(), "done") || strings.Contains(stdout.String(), "test-key") {
		t.Fatal("unexpected turn output")
	}
	matches, err := filepath.Glob(filepath.Join(state, "turn-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("private turn state retained: %v, %v", matches, err)
	}
	// A valid default credential file must never rescue a missing Job Secret.
	credentials(t, filepath.Join(root, ".cline"), providerConfig)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"--cline", binary, "--model", "openrouter/test", "--provider-secret", "--check"},
		nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "job.env_from_secrets") {
		t.Fatalf("missing Secret fell back to host credentials: %s", &stderr)
	}
	for _, flag := range []string{"--config", "--stage-config"} {
		stdout.Reset()
		stderr.Reset()
		if code := Run([]string{"--cline", binary, "--model", "openrouter/test", "--provider-secret", flag, workspace},
			nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "cannot be combined") {
			t.Fatalf("mixed credential sources accepted: %s", &stderr)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"--cline", binary, "--model", "openrouter/test", "--provider-secret", "--export-provider-config"},
		nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "cannot be combined") {
		t.Fatalf("Secret export accepted: %s", &stderr)
	}
	// Even an image with HOME inside /workspace cannot materialize keys there.
	t.Setenv(ProviderSecretEnv, base64.StdEncoding.EncodeToString([]byte(providerConfig)))
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"--cline", binary, "--model", "openrouter/test", "--provider-secret",
		"--cwd", workspace, "--state-dir", filepath.Join(workspace, "state")}, nil, &stdout, &stderr); code == 0 ||
		!strings.Contains(stderr.String(), "outside the Git workspace") {
		t.Fatalf("Secret state in workspace accepted: %s", &stderr)
	}
}

func TestSecretStateOutsideWorkspace(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	nested := filepath.Join(workspace, "state")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outside, "link")
	if err := os.Symlink(nested, link); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{workspace, nested, link, filepath.Join(outside, "missing")} {
		if err := secretStateOutsideWorkspace(state, workspace); err == nil {
			t.Fatalf("unsafe state accepted: %s", state)
		}
	}
	if err := secretStateOutsideWorkspace(outside, filepath.Join(workspace, "missing")); err == nil {
		t.Fatal("unresolvable workspace accepted")
	}
	if err := secretStateOutsideWorkspace(outside, workspace); err != nil {
		t.Fatal(err)
	}
}

func TestProbeDoesNotInheritProviderSecrets(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "cline")
	script := "#!/bin/sh\n[ -z \"${HIVE_CLINE_PROVIDERS_B64+x}\" ] || exit 91\nprintf '%s\\n' '" + help + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ProviderSecretEnv, "must-not-reach-help")
	if err := Probe(context.Background(), binary); err != nil {
		t.Fatal(err)
	}
}
