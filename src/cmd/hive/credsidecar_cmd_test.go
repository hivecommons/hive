package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/credsidecar"
)

func credSidecarEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	hmacPath := filepath.Join(dir, "hmac")
	if err := os.WriteFile(hmacPath, []byte(strings.Repeat("h", credsidecar.MinKeyBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	appKey := filepath.Join(dir, "app.pem")
	writeTestKey(t, appKey)
	return map[string]string{
		credsidecar.KeyFileEnv:        hmacPath,
		credsidecar.AppIDEnv:          "7",
		credsidecar.InstallationIDEnv: "8",
		credsidecar.AppKeyFileEnv:     appKey,
	}
}

func stubCredSidecarServe(t *testing.T, fn func(ctx context.Context, s *credsidecar.Server, addr string) error) {
	t.Helper()
	orig := credSidecarServe
	credSidecarServe = fn
	t.Cleanup(func() { credSidecarServe = orig })
}

func TestRunCredSidecar_ServesOnConfiguredLoopback(t *testing.T) {
	env := credSidecarEnv(t)
	var gotAddr string
	var gotServer *credsidecar.Server
	stubCredSidecarServe(t, func(_ context.Context, s *credsidecar.Server, addr string) error {
		gotAddr, gotServer = addr, s
		return nil
	})
	var out, errOut bytes.Buffer
	if code := runCredSidecar(nil, func(k string) string { return env[k] }, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, log:\n%s", code, out.String())
	}
	if gotAddr != credsidecar.DefaultListenAddr || gotServer == nil {
		t.Fatalf("served on %q (server nil=%v), want %q", gotAddr, gotServer == nil, credsidecar.DefaultListenAddr)
	}
}

func TestRunCredSidecar_RefusesBadConfig(t *testing.T) {
	cases := map[string]func(map[string]string){
		"no hmac key":    func(e map[string]string) { e[credsidecar.KeyFileEnv] = "/nonexistent/hmac" },
		"no app id":      func(e map[string]string) { delete(e, credsidecar.AppIDEnv) },
		"public listen":  func(e map[string]string) { e[credsidecar.ListenEnv] = "0.0.0.0:18445" },
		"missing appkey": func(e map[string]string) { e[credsidecar.AppKeyFileEnv] = "/nonexistent/app.pem" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env := credSidecarEnv(t)
			mutate(env)
			stubCredSidecarServe(t, func(context.Context, *credsidecar.Server, string) error {
				t.Fatal("served despite an unusable configuration")
				return nil
			})
			var out, errOut bytes.Buffer
			if code := runCredSidecar(nil, func(k string) string { return env[k] }, &out, &errOut); code != credSidecarConfigExitCode {
				t.Fatalf("exit %d, want %d", code, credSidecarConfigExitCode)
			}
			if !strings.Contains(out.String(), "refusing to start") {
				t.Fatalf("no refusal logged:\n%s", out.String())
			}
		})
	}
}

func TestRunCredSidecar_RejectsArgsAndReportsServeFailure(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runCredSidecar([]string{"--listen", "x"}, func(string) string { return "" }, &out, &errOut); code != credSidecarConfigExitCode {
		t.Fatalf("args accepted: exit %d", code)
	}
	if !strings.Contains(errOut.String(), credsidecar.KeyFileEnv) {
		t.Fatalf("usage does not name the env: %q", errOut.String())
	}

	env := credSidecarEnv(t)
	stubCredSidecarServe(t, func(context.Context, *credsidecar.Server, string) error { return errors.New("bind failed") })
	out.Reset()
	if code := runCredSidecar(nil, func(k string) string { return env[k] }, &out, &errOut); code != credSidecarServeExitCode {
		t.Fatalf("serve failure exit %d, want %d", code, credSidecarServeExitCode)
	}
}

// The subcommand is reachable through the real dispatcher, before any hive
// boot work runs.
func TestDispatchSubcommand_CredSidecar(t *testing.T) {
	stubCredSidecarServe(t, func(context.Context, *credsidecar.Server, string) error {
		t.Fatal("served with an empty environment")
		return nil
	})
	t.Setenv(credsidecar.KeyFileEnv, filepath.Join(t.TempDir(), "absent"))
	var out, errOut bytes.Buffer
	handled, code := dispatchSubcommand([]string{credsidecar.Subcommand}, &out, &errOut)
	if !handled || code != credSidecarConfigExitCode {
		t.Fatalf("handled=%v code=%d, want the subcommand to run and refuse its empty config", handled, code)
	}
}
