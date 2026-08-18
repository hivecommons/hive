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

func TestProxyAuthTokenFromEnvWarnsOnAnthropicFallback(t *testing.T) {
	var warnings []string
	getenv := func(key string) string {
		switch key {
		case "PROXY_AUTH_TOKEN":
			return ""
		case "ANTHROPIC_API_KEY":
			return "sk-ant"
		default:
			return ""
		}
	}
	token := proxyAuthTokenFromEnv(getenv, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	if token != "sk-ant" {
		t.Fatalf("token = %q, want ANTHROPIC_API_KEY fallback", token)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "PROXY_AUTH_TOKEN is unset") {
		t.Fatalf("expected actionable fallback warning, got %#v", warnings)
	}
	if strings.Contains(warnings[0], "Unauthenticated callers will be granted") {
		t.Fatalf("warning must not claim unauthenticated callers receive the host key: %q", warnings[0])
	}
}

func TestProxyAuthTokenFromEnvPrefersProxyAuthToken(t *testing.T) {
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
	token := proxyAuthTokenFromEnv(getenv, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	if token != "proxy-token" {
		t.Fatalf("token = %q, want PROXY_AUTH_TOKEN", token)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no fallback warning when PROXY_AUTH_TOKEN is set, got %#v", warnings)
	}
}
