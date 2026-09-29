package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/credsidecar"
)

func sidecarKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hmac")
	if err := os.WriteFile(path, []byte(strings.Repeat("k", credsidecar.MinKeyBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Unset sidecar URL: every existing posture passes untouched, including the
// ones phase 1 only warns about. The guard is fatal only for the new opt-in.
func TestValidateCredSidecar_UnsetNeverFatal(t *testing.T) {
	for _, inject := range []string{"", ProxyInjectGHAuthOnValue, ProxyInjectGHAuthOffValue, "TRUE", "1"} {
		env := map[string]string{ProxyInjectGHAuthEnv: inject}
		if err := ValidateCredSidecar(func(k string) string { return env[k] }); err != nil {
			t.Fatalf("inject=%q with no sidecar: %v", inject, err)
		}
	}
}

// Sidecar configured while the hive process would still hold agent tokens
// (injection unset, off, or unrecognized) must refuse to start: that is the
// "both token sources" misconfiguration #9586 asks to make impossible.
func TestValidateCredSidecar_RefusesInProcessTokenSource(t *testing.T) {
	key := sidecarKeyFile(t)
	for _, inject := range []string{"", ProxyInjectGHAuthOffValue, "TRUE", "yes"} {
		env := map[string]string{
			credsidecar.URLEnv:     credsidecar.DefaultURL,
			credsidecar.KeyFileEnv: key,
			ProxyInjectGHAuthEnv:   inject,
		}
		err := ValidateCredSidecar(func(k string) string { return env[k] })
		if !errors.Is(err, ErrCredSidecarWithInProcessToken) {
			t.Fatalf("inject=%q: %v, want ErrCredSidecarWithInProcessToken", inject, err)
		}
		for _, want := range []string{credsidecar.URLEnv, ProxyInjectGHAuthEnv, ProxyInjectGHAuthOnValue} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %q", err, want)
			}
		}
	}
}

func TestValidateCredSidecar_RefusesUnusableSidecarConfig(t *testing.T) {
	key := sidecarKeyFile(t)
	for name, env := range map[string]map[string]string{
		"remote url":  {credsidecar.URLEnv: "http://10.1.2.3:18445", credsidecar.KeyFileEnv: key},
		"missing key": {credsidecar.URLEnv: credsidecar.DefaultURL, credsidecar.KeyFileEnv: filepath.Join(t.TempDir(), "absent")},
	} {
		env[ProxyInjectGHAuthEnv] = ProxyInjectGHAuthOnValue
		err := ValidateCredSidecar(func(k string) string { return env[k] })
		if !errors.Is(err, ErrCredSidecarMisconfigured) {
			t.Fatalf("%s: %v, want ErrCredSidecarMisconfigured", name, err)
		}
	}
}

func TestValidateCredSidecar_ConsistentPostureBoots(t *testing.T) {
	env := map[string]string{
		credsidecar.URLEnv:     credsidecar.DefaultURL,
		credsidecar.KeyFileEnv: sidecarKeyFile(t),
		ProxyInjectGHAuthEnv:   ProxyInjectGHAuthOnValue,
	}
	if err := ValidateCredSidecar(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("consistent sidecar posture refused: %v", err)
	}
}
