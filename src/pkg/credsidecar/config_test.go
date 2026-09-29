package credsidecar

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hmac-key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnabled(t *testing.T) {
	if Enabled(envMap(nil)) {
		t.Fatal("unset URL reported enabled")
	}
	if Enabled(envMap(map[string]string{URLEnv: "  "})) {
		t.Fatal("blank URL reported enabled")
	}
	if !Enabled(envMap(map[string]string{URLEnv: DefaultURL})) {
		t.Fatal("set URL reported disabled")
	}
}

func TestLoadClientConfig(t *testing.T) {
	good := writeKeyFile(t, strings.Repeat("s", MinKeyBytes)+"\n")
	short := writeKeyFile(t, "short")

	cfg, err := LoadClientConfig(envMap(map[string]string{URLEnv: DefaultURL + "/", KeyFileEnv: good}))
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if cfg.URL.String() != DefaultURL || string(cfg.Key) != strings.Repeat("s", MinKeyBytes) || cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("config = %v %q %d (the trailing newline must be trimmed)", cfg.URL, cfg.Key, cfg.MaxBodyBytes)
	}
	cfg, err = LoadClientConfig(envMap(map[string]string{URLEnv: "http://localhost:9", KeyFileEnv: good, MaxBodyBytesEnv: "1024"}))
	if err != nil || cfg.MaxBodyBytes != 1024 {
		t.Fatalf("localhost + max body: %v %d", err, cfg.MaxBodyBytes)
	}

	for name, env := range map[string]map[string]string{
		"unset":         {},
		"bad url":       {URLEnv: "http://[::1", KeyFileEnv: good},
		"https":         {URLEnv: "https://127.0.0.1:1", KeyFileEnv: good},
		"remote host":   {URLEnv: "http://10.0.0.5:18445", KeyFileEnv: good},
		"hostname":      {URLEnv: "http://sidecar.svc:18445", KeyFileEnv: good},
		"no port":       {URLEnv: "http://127.0.0.1", KeyFileEnv: good},
		"path":          {URLEnv: "http://127.0.0.1:1/x", KeyFileEnv: good},
		"query":         {URLEnv: "http://127.0.0.1:1?a=b", KeyFileEnv: good},
		"userinfo":      {URLEnv: "http://u:p@127.0.0.1:1", KeyFileEnv: good},
		"missing key":   {URLEnv: DefaultURL, KeyFileEnv: filepath.Join(t.TempDir(), "absent")},
		"short key":     {URLEnv: DefaultURL, KeyFileEnv: short},
		"bad max body":  {URLEnv: DefaultURL, KeyFileEnv: good, MaxBodyBytesEnv: "lots"},
		"zero max body": {URLEnv: DefaultURL, KeyFileEnv: good, MaxBodyBytesEnv: "0"},
	} {
		if _, err := LoadClientConfig(envMap(env)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := LoadClientConfig(envMap(nil)); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unset: %v, want ErrNotConfigured", err)
	}
	if _, err := LoadClientConfig(envMap(map[string]string{URLEnv: DefaultURL, KeyFileEnv: short})); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("short key: %v, want ErrKeyTooShort", err)
	}
}

// The default key path is used when the variable is unset; on a dev host it
// does not exist, which must be an error naming the variable, not a panic or a
// silent empty key.
func TestLoadKey_DefaultPathNamesTheVariable(t *testing.T) {
	_, err := loadKey(envMap(nil))
	if err == nil {
		t.Skip("default key file exists on this host")
	}
	if !strings.Contains(err.Error(), KeyFileEnv) || !strings.Contains(err.Error(), DefaultKeyFile) {
		t.Fatalf("error %q does not name %s / %s", err, KeyFileEnv, DefaultKeyFile)
	}
}

func TestLoadServerConfig(t *testing.T) {
	good := writeKeyFile(t, strings.Repeat("s", MinKeyBytes))
	base := map[string]string{
		KeyFileEnv:        good,
		AppIDEnv:          "12",
		InstallationIDEnv: "34",
		ExtraHostsEnv:     " GHE.example.com , ,",
		GitHubAPIURLEnv:   "https://ghe.example.com/api/v3",
	}
	cfg, err := LoadServerConfig(envMap(base))
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if cfg.ListenAddr != DefaultListenAddr || cfg.AppID != 12 || cfg.InstallationID != 34 ||
		cfg.AppKeyFile != DefaultAppKeyFile || cfg.APIURL != "https://ghe.example.com/api/v3" ||
		cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("config = %+v", cfg)
	}
	for _, h := range []string{"api.github.com", "github.com", "ghe.example.com"} {
		if !cfg.AllowedHosts[h] {
			t.Fatalf("allowlist missing %s: %v", h, cfg.AllowedHosts)
		}
	}
	if len(cfg.AllowedHosts) != 3 {
		t.Fatalf("allowlist has extras: %v", cfg.AllowedHosts)
	}

	withOverride := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	cfg, err = LoadServerConfig(envMap(withOverride(AppKeyFileEnv, "/k.pem")))
	if err != nil || cfg.AppKeyFile != "/k.pem" {
		t.Fatalf("app key override: %v %q", err, cfg.AppKeyFile)
	}
	for name, env := range map[string]map[string]string{
		"public listen":   withOverride(ListenEnv, "0.0.0.0:18445"),
		"missing key":     withOverride(KeyFileEnv, filepath.Join(t.TempDir(), "absent")),
		"bad max body":    withOverride(MaxBodyBytesEnv, "-1"),
		"no app id":       withOverride(AppIDEnv, ""),
		"bad install id":  withOverride(InstallationIDEnv, "x"),
		"zero install id": withOverride(InstallationIDEnv, "0"),
	} {
		if _, err := LoadServerConfig(envMap(env)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
