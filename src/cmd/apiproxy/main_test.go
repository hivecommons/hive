package main

import (
	"errors"
	"strings"
	"testing"
)

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
