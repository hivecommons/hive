package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenEventLogIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := openEventLog(path)
	if err != nil {
		t.Fatalf("openEventLog: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close event log: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat event log: %v", err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("event log permissions = %04o, group/world bits must be clear", got)
	}
	if got := info.Mode().Perm(); got&0o600 != 0o600 {
		t.Fatalf("event log permissions = %04o, want owner read/write", got)
	}
}

func TestOpenEventLogTightensExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seed event log: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}
	f, err := openEventLog(path)
	if err != nil {
		t.Fatalf("openEventLog: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close event log: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat event log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("pre-existing event log permissions = %04o, want 0600", got)
	}
}

func TestDefaultProxyHostIsLoopback(t *testing.T) {
	if defaultProxyHost != "127.0.0.1" {
		t.Fatalf("defaultProxyHost = %q, want loopback 127.0.0.1", defaultProxyHost)
	}
}

func TestClientAuthTokenFromEnvFailsClosedWhenUnset(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case "ANTHROPIC_API_KEY":
			return "sk-ant"
		default:
			return ""
		}
	}
	token, err := clientAuthTokenFromEnv(getenv)
	if token != "" {
		t.Fatalf("token = %q, want empty when PROXY_AUTH_TOKEN is unset", token)
	}
	if !errors.Is(err, errMissingClientAuthToken) {
		t.Fatalf("err = %v, want errMissingClientAuthToken so the proxy refuses to start", err)
	}
	if !strings.Contains(err.Error(), "PROXY_AUTH_TOKEN") {
		t.Fatalf("error %q must name the required variable", err)
	}
}

func TestClientAuthTokenFromEnvUsesProxyAuthToken(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case "PROXY_AUTH_TOKEN":
			return "proxy-token"
		case "ANTHROPIC_API_KEY":
			return "sk-ant"
		default:
			return ""
		}
	}
	token, err := clientAuthTokenFromEnv(getenv)
	if err != nil {
		t.Fatalf("unexpected error when PROXY_AUTH_TOKEN is set: %v", err)
	}
	if token != "proxy-token" {
		t.Fatalf("token = %q, want PROXY_AUTH_TOKEN", token)
	}
}

// The upstream key is read from ANTHROPIC_API_KEY only; it must never double as
// the caller-facing gate token.
func TestUpstreamAPIKeyFromEnvIgnoresProxyAuthToken(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case "PROXY_AUTH_TOKEN":
			return "proxy-token"
		case "ANTHROPIC_API_KEY":
			return "sk-ant-upstream"
		default:
			return ""
		}
	}
	if got := upstreamAPIKeyFromEnv(getenv); got != "sk-ant-upstream" {
		t.Fatalf("upstream key = %q, want ANTHROPIC_API_KEY", got)
	}
}
