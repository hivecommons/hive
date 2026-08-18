package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestDefaultProxyHostIsLoopback(t *testing.T) {
	if defaultProxyHost != "127.0.0.1" {
		t.Fatalf("defaultProxyHost = %q, want loopback 127.0.0.1", defaultProxyHost)
	}
}

func TestClientAuthTokenFromEnvWarnsWhenUnset(t *testing.T) {
	var warnings []string
	getenv := func(key string) string {
		switch key {
		case "ANTHROPIC_API_KEY":
			return "sk-ant"
		default:
			return ""
		}
	}
	token := clientAuthTokenFromEnv(getenv, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	if token != "" {
		t.Fatalf("token = %q, want empty when PROXY_AUTH_TOKEN is unset", token)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "PROXY_AUTH_TOKEN is unset") {
		t.Fatalf("expected actionable warning when the gate is disabled, got %#v", warnings)
	}
}

func TestClientAuthTokenFromEnvUsesProxyAuthToken(t *testing.T) {
	var warnings []string
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
	token := clientAuthTokenFromEnv(getenv, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	if token != "proxy-token" {
		t.Fatalf("token = %q, want PROXY_AUTH_TOKEN", token)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warning when PROXY_AUTH_TOKEN is set, got %#v", warnings)
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
