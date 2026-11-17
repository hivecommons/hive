package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureSlog points the default slog logger at a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// allowedRoot makes a temp secrets root, registers it as the only allowed
// root, and resets the warn-once memory.
func allowedRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(SetSecretFileRootsForTest(root))
	resetSkippedKeyFileWarnings()
	t.Cleanup(resetSkippedKeyFileWarnings)
	return root
}

func writeKey(t *testing.T, path, key string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSecretFilePathRejection(t *testing.T) {
	root := allowedRoot(t)
	if got := SecretFilePathRejection("api_key_file", ""); got != "" {
		t.Fatalf("empty path rejected: %q", got)
	}
	if got := SecretFilePathRejection("api_key_file", filepath.Join(root, "k")); got != "" {
		t.Fatalf("allowed path rejected: %q", got)
	}
	outside := "/secrets-cb/litellm_api_key"
	msg := SecretFilePathRejection("governor.litellm.api_key_file", outside)
	for _, want := range []string{"governor.litellm.api_key_file", outside, root, "mount the Kubernetes Secret"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("rejection %q does not mention %q", msg, want)
		}
	}
}

func TestSecretFileRootsIsACopy(t *testing.T) {
	root := allowedRoot(t)
	roots := SecretFileRoots()
	if len(roots) != 1 || roots[0] != root {
		t.Fatalf("SecretFileRoots() = %v, want [%s]", roots, root)
	}
	roots[0] = "/etc"
	if SecretFilePathAllowed("/etc/passwd") {
		t.Fatal("mutating the returned slice widened the allowed roots")
	}
}

func TestDescribeKeySource(t *testing.T) {
	allowedRoot(t)
	cases := []struct{ source, configured, want string }{
		{"", "", "no key found"},
		{"file:/secrets/litellm_api_key", "", "key from /secrets/litellm_api_key"},
		{"env:HIVE_LITELLM_API_KEY", "", "key from env HIVE_LITELLM_API_KEY"},
		{"other", "", "key from other"},
		{"file:/secrets/litellm_api_key", "/secrets-cb/k", "key from /secrets/litellm_api_key (configured path /secrets-cb/k not allowed)"},
	}
	for _, c := range cases {
		if got := DescribeKeySource(c.source, c.configured); got != c.want {
			t.Errorf("DescribeKeySource(%q, %q) = %q, want %q", c.source, c.configured, got, c.want)
		}
	}
}

// TestLiteLLMSkippedKeyFileWarnsOnce covers hive#18533 at runtime: the
// configured path outside the allowed dirs is skipped, the fallback source is
// used, and exactly one WARN names both.
func TestLiteLLMSkippedKeyFileWarnsOnce(t *testing.T) {
	allowedRoot(t)
	logs := captureSlog(t)
	outside := filepath.Join(t.TempDir(), "litellm_api_key")
	writeKey(t, outside, "new-key-never-read")
	t.Setenv(DefaultLiteLLMAPIKeyEnv, "env-fallback-key")

	c := &LiteLLMConfig{APIKeyFile: outside}
	if !c.ConfiguredKeyFileSkipped() {
		t.Fatal("outside path not reported as skipped")
	}
	for i := 0; i < 3; i++ {
		key, source := c.ResolveAPIKeyWithSource()
		if key != "env-fallback-key" || source != KeySourceEnvPrefix+DefaultLiteLLMAPIKeyEnv {
			t.Fatalf("resolve = (%q, %q), want the env fallback", key, source)
		}
	}
	out := logs.String()
	if n := strings.Count(out, "configured api_key_file is outside the allowed secret dirs"); n != 1 {
		t.Fatalf("warning logged %d times, want once:\n%s", n, out)
	}
	for _, want := range []string{"skipped_path=" + outside, "effective_key_source=env:" + DefaultLiteLLMAPIKeyEnv, LiteLLMAPIKeyFileField} {
		if !strings.Contains(out, want) {
			t.Fatalf("warning missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "env-fallback-key") || strings.Contains(out, "new-key-never-read") {
		t.Fatalf("warning leaked key material:\n%s", out)
	}
	if got := c.KeySourceDescription(); !strings.Contains(got, "not allowed") {
		t.Fatalf("KeySourceDescription() = %q, want the skipped-path note", got)
	}
	if c.APIKeyFileProblem() == "" {
		t.Fatal("APIKeyFileProblem() empty for an outside path")
	}
}

func TestLiteLLMAllowedKeyFileDoesNotWarn(t *testing.T) {
	root := allowedRoot(t)
	logs := captureSlog(t)
	path := filepath.Join(root, "litellm_api_key")
	writeKey(t, path, "allowed-key")
	c := &LiteLLMConfig{APIKeyFile: path}
	key, source := c.ResolveAPIKeyWithSource()
	if key != "allowed-key" || source != KeySourceFilePrefix+path {
		t.Fatalf("resolve = (%q, %q)", key, source)
	}
	if c.ConfiguredKeyFileSkipped() || c.APIKeyFileProblem() != "" {
		t.Fatal("allowed path reported as a problem")
	}
	if strings.Contains(logs.String(), "outside the allowed secret dirs") {
		t.Fatalf("allowed path warned:\n%s", logs.String())
	}
}

func TestGatewayResolveAPIKeyWithSource(t *testing.T) {
	root := allowedRoot(t)
	logs := captureSlog(t)

	path := filepath.Join(root, "gw_key")
	writeKey(t, path, "gw-file-key")
	if key, source := (GatewayConfig{Name: "gw", APIKeyFile: path}).ResolveAPIKeyWithSource(); key != "gw-file-key" || source != KeySourceFilePrefix+path {
		t.Fatalf("file resolve = (%q, %q)", key, source)
	}

	t.Setenv("HIVE_TEST_GW_KEY", "gw-env-key")
	if key, source := (GatewayConfig{Name: "gw", APIKeyEnv: "HIVE_TEST_GW_KEY", APIKeyFile: path}).ResolveAPIKeyWithSource(); key != "gw-env-key" || source != "env:HIVE_TEST_GW_KEY" {
		t.Fatalf("env resolve = (%q, %q)", key, source)
	}

	empty := filepath.Join(root, "empty")
	writeKey(t, empty, "")
	if key, source := (GatewayConfig{Name: "gw", APIKeyFile: empty}).ResolveAPIKeyWithSource(); key != "" || source != "" {
		t.Fatalf("empty file resolve = (%q, %q), want nothing", key, source)
	}

	outside := filepath.Join(t.TempDir(), "gw_key")
	writeKey(t, outside, "outside")
	gw := GatewayConfig{Name: "corp", APIKeyFile: outside}
	for i := 0; i < 2; i++ {
		if key := gw.ResolveAPIKey(); key != "" {
			t.Fatalf("outside gateway key read: %q", key)
		}
	}
	if n := strings.Count(logs.String(), "skipped_path="+outside); n != 1 {
		t.Fatalf("gateway warning logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "effective_key_source=none") || !strings.Contains(logs.String(), GatewayAPIKeyFileField("corp")) {
		t.Fatalf("gateway warning missing fields:\n%s", logs.String())
	}
	if !strings.Contains(gw.APIKeyFileProblem(), "governor.gateways[corp].api_key_file") {
		t.Fatalf("APIKeyFileProblem() = %q", gw.APIKeyFileProblem())
	}
}

func TestWarnSkippedKeyFileOnceReturnsWhetherItLogged(t *testing.T) {
	allowedRoot(t)
	captureSlog(t)
	if !warnSkippedKeyFileOnce("f", "/x", "") {
		t.Fatal("first call did not log")
	}
	if warnSkippedKeyFileOnce("f", "/x", "") {
		t.Fatal("second call logged again")
	}
	if !warnSkippedKeyFileOnce("f", "/y", "") {
		t.Fatal("a different path did not log")
	}
}
