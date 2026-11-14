package keystore

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestBuildDefaultsToLegacyWithoutKeyMaterial(t *testing.T) {
	st, p, err := build(context.Background(), envOf(nil), os.ReadFile)
	if err != nil || p != nil {
		t.Fatalf("build = %v, %v", p, err)
	}
	if !st.Legacy || st.Active() || st.Provider != ProviderNone || !strings.Contains(st.Note, EnvSecretsKey) {
		t.Fatalf("status = %+v", st)
	}
}

func TestBuildExplicitNone(t *testing.T) {
	st, p, err := build(context.Background(), envOf(map[string]string{EnvProviderName: "none", EnvSecretsKey: testKeyA}), nil)
	if err != nil || p != nil || !st.Legacy {
		t.Fatalf("none: %+v, %v, %v", st, p, err)
	}
}

func TestBuildEnvProviderSourceOrder(t *testing.T) {
	ctx := context.Background()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(testKeyB+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Heartbeat key alone: the hub-provisioned default.
	st, p, err := build(ctx, envOf(map[string]string{EnvHeartbeatKey: testKeyA}), nil)
	if err != nil || p == nil || st.Source != "env:"+EnvHeartbeatKey || !st.Active() {
		t.Fatalf("heartbeat only: %+v, %v", st, err)
	}
	hbOnly := p.KeyID()

	// Explicit key wins over the heartbeat key; the heartbeat key stays
	// readable as a previous generation.
	st, p, err = build(ctx, envOf(map[string]string{EnvSecretsKey: testKeyB, EnvHeartbeatKey: testKeyA}), nil)
	if err != nil || st.Source != "env:"+EnvSecretsKey {
		t.Fatalf("explicit key: %+v, %v", st, err)
	}
	if p.KeyID() == hbOnly {
		t.Fatal("explicit key did not change the generation")
	}
	if len(st.Generations) != 2 || st.Generations[1] != hbOnly {
		t.Fatalf("Generations = %v, want current then heartbeat %s", st.Generations, hbOnly)
	}

	// The key file wins over everything.
	st, _, err = build(ctx, envOf(map[string]string{EnvSecretsKeyFile: keyFile, EnvSecretsKey: testKeyA}), os.ReadFile)
	if err != nil || st.Source != "file:"+keyFile {
		t.Fatalf("key file: %+v, %v", st, err)
	}
	// Previous material is decrypt-only and never current.
	st, _, err = build(ctx, envOf(map[string]string{EnvSecretsKey: testKeyA, EnvSecretsKeyPrevious: testKeyB}), nil)
	if err != nil || len(st.Generations) != 2 || st.Source != "env:"+EnvSecretsKey {
		t.Fatalf("previous: %+v, %v", st, err)
	}
	// Previous alone is a configuration mistake: it becomes current (there is
	// nothing else), which is still better than plaintext and is visible in
	// Source.
	st, _, err = build(ctx, envOf(map[string]string{EnvSecretsKeyPrevious: testKeyB}), nil)
	if err != nil || st.Source != "env:"+EnvSecretsKeyPrevious {
		t.Fatalf("previous alone: %+v, %v", st, err)
	}
}

func TestBuildErrors(t *testing.T) {
	ctx := context.Background()
	if _, _, err := build(ctx, envOf(map[string]string{EnvSecretsKeyFile: filepath.Join(t.TempDir(), "nope")}), os.ReadFile); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key file: %v", err)
	}
	if _, _, err := build(ctx, envOf(map[string]string{EnvSecretsKey: "tiny"}), nil); !errors.Is(err, ErrKeyMaterialTooShort) {
		t.Fatalf("short key: %v", err)
	}
	_, _, err := build(ctx, envOf(map[string]string{EnvProviderName: "vault"}), nil)
	if err == nil || !strings.Contains(err.Error(), `"vault"`) || !strings.Contains(err.Error(), ProviderEnv) {
		t.Fatalf("unknown provider: %v", err)
	}
}

type fakeProvider struct{ name, kid string }

func (f fakeProvider) Name() string  { return f.name }
func (f fakeProvider) KeyID() string { return f.kid }
func (f fakeProvider) Encrypt(_ context.Context, pt, _ []byte) ([]byte, error) {
	return append([]byte("fake:"), pt...), nil
}
func (f fakeProvider) Decrypt(_ context.Context, _ string, ct, _ []byte) ([]byte, error) {
	return bytes.TrimPrefix(ct, []byte("fake:")), nil
}

func TestRegisteredProviderThroughInit(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	name := "testprov-" + t.Name()
	Register(name, func(_ context.Context, getenv func(string) string) (Provider, error) {
		if getenv("FAIL") != "" {
			return nil, errors.New("backend unreachable")
		}
		return fakeProvider{name: name, kid: "k1"}, nil
	})
	if !contains(Registered(), name) {
		t.Fatalf("Registered = %v", Registered())
	}
	// A factory failure refuses: no provider becomes active.
	ResetForTest()
	_, err := Init(context.Background(), envOf(map[string]string{EnvProviderName: name, "FAIL": "1"}), nil)
	if err == nil || !strings.Contains(err.Error(), "backend unreachable") {
		t.Fatalf("Init with failing factory = %v", err)
	}
	if Active() != nil {
		t.Fatal("a failed Init left a provider active")
	}
	if InitError() == nil {
		t.Fatal("InitError is nil after a failed Init")
	}
	// Init is once: a second call returns the same verdict.
	if _, err2 := Init(context.Background(), envOf(nil), nil); err2 == nil {
		t.Fatal("second Init did not return the first result")
	}

	ResetForTest()
	st, err := Init(context.Background(), envOf(map[string]string{EnvProviderName: name}), nil)
	if err != nil || st.Provider != name || st.KeyID != "k1" || !st.Active() {
		t.Fatalf("Init = %+v, %v", st, err)
	}
	if p := Active(); p == nil || p.Name() != name {
		t.Fatalf("Active = %v", p)
	}
	// Register guards.
	for _, bad := range []string{"", ProviderEnv, ProviderNone, name} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) did not panic", bad)
				}
			}()
			Register(bad, func(context.Context, func(string) string) (Provider, error) { return nil, nil })
		}()
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestLazyActiveInitialisesFromEnvironment(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvProviderName, "")
	t.Setenv(EnvSecretsKeyFile, "")
	t.Setenv(EnvSecretsKey, testKeyA)
	t.Setenv(EnvSecretsKeyPrevious, "")
	t.Setenv(EnvHeartbeatKey, "")
	t.Setenv(EnvHeartbeatKeyPrevious, "")
	p := Active()
	if p == nil || p.Name() != ProviderEnv {
		t.Fatalf("lazy Active = %v", p)
	}
	if InitError() != nil {
		t.Fatal(InitError())
	}
}

func TestLogStatusOnce(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	LogStatus(logger, Status{Provider: ProviderNone, Legacy: true, Note: "no key"})
	LogStatus(logger, Status{Provider: ProviderNone, Legacy: true, Note: "no key"})
	out := buf.String()
	if strings.Count(out, "PLAINTEXT") != 1 || !strings.Contains(out, "level=WARN") {
		t.Fatalf("legacy log:\n%s", out)
	}
	ResetForTest()
	buf.Reset()
	LogStatus(logger, Status{Provider: ProviderEnv, KeyID: "abcd1234", Source: "env:" + EnvHeartbeatKey})
	if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), "abcd1234") {
		t.Fatalf("active log:\n%s", buf.String())
	}
	buf.Reset()
	LogStatus(nil, Status{Provider: ProviderEnv})
	if buf.Len() != 0 {
		t.Fatal("LogStatus logged twice")
	}
}
