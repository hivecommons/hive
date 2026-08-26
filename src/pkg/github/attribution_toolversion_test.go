package github

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestBackendToolBinary pins the backend → (tool, binary) table so a drift
// from config/backends.conf's backend_binary() shows up as a test failure
// rather than a silently wrong attribution trailer.
func TestBackendToolBinary(t *testing.T) {
	cases := []struct {
		backend, wantTool, wantBinary string
	}{
		{backendBob, toolBobshell, backendBob},
		{backendLiteLLM, binaryClaude, binaryClaude},
		{"", "", ""},
		// Unknown/inference backends map to themselves.
		{"copilot", "copilot", "copilot"},
		{"claude", "claude", "claude"},
	}
	for _, tc := range cases {
		tool, binary := backendToolBinary(tc.backend)
		if tool != tc.wantTool || binary != tc.wantBinary {
			t.Errorf("backendToolBinary(%q) = (%q, %q), want (%q, %q)",
				tc.backend, tool, binary, tc.wantTool, tc.wantBinary)
		}
	}
}

func TestResolveToolVersion(t *testing.T) {
	t.Run("empty backend resolves to nothing", func(t *testing.T) {
		tool, version := ResolveToolVersion("")
		if tool != "" || version != "" {
			t.Errorf("ResolveToolVersion(\"\") = (%q, %q), want empty", tool, version)
		}
	})

	t.Run("cached version is served without probing", func(t *testing.T) {
		// Pre-seed the process-global cache for a binary that does not exist;
		// a probe would have stored "" instead.
		toolVersionCache.Store("hive-test-cached-cli", "3.1.4")
		tool, version := ResolveToolVersion("hive-test-cached-cli")
		if tool != "hive-test-cached-cli" || version != "3.1.4" {
			t.Errorf("got (%q, %q), want (hive-test-cached-cli, 3.1.4)", tool, version)
		}
	})

	t.Run("missing binary caches empty version", func(t *testing.T) {
		const backend = "hive-test-no-such-cli"
		tool, version := ResolveToolVersion(backend)
		if tool != backend || version != "" {
			t.Errorf("got (%q, %q), want (%q, \"\")", tool, version, backend)
		}
		if v, ok := toolVersionCache.Load(backend); !ok || v.(string) != "" {
			t.Errorf("cache for %q = (%v, %v), want (\"\", true)", backend, v, ok)
		}
	})

	t.Run("probes the binary and extracts the version", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("shell-script fake binary requires a POSIX shell")
		}
		const backend = "hive-test-versioned-cli"
		dir := t.TempDir()
		script := filepath.Join(dir, backend)
		if err := os.WriteFile(script, []byte("#!/bin/sh\necho \""+backend+" version 9.8.7 (test build)\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		tool, version := ResolveToolVersion(backend)
		if tool != backend || version != "9.8.7" {
			t.Errorf("got (%q, %q), want (%q, 9.8.7)", tool, version, backend)
		}
	})
}

// TestSetAttributionResolver covers the staged-startup seam: the resolver is
// installed after the client exists, and attributionMeta must switch from the
// name-only fallback to resolver-provided metadata.
func TestSetAttributionResolver(t *testing.T) {
	t.Run("nil client is a safe no-op", func(t *testing.T) {
		var c *Client
		c.SetAttributionResolver(func(string) InvocationMeta { return InvocationMeta{} })
		if got := c.attributionMeta("scanner"); got.Agent != "scanner" || got.Backend != "" {
			t.Errorf("nil-client attributionMeta = %+v, want name-only", got)
		}
	})

	t.Run("installed resolver supplies metadata", func(t *testing.T) {
		c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"},
			slog.New(slog.NewTextHandler(io.Discard, nil)))

		// Before installation: name-only fallback.
		if got := c.attributionMeta("quality"); got.Backend != "" || got.Agent != "quality" {
			t.Fatalf("pre-resolver attributionMeta = %+v, want name-only", got)
		}

		c.SetAttributionResolver(func(agent string) InvocationMeta {
			return InvocationMeta{Backend: "claude", Model: "opus"}
		})
		got := c.attributionMeta("quality")
		if got.Backend != "claude" || got.Model != "opus" {
			t.Errorf("attributionMeta = %+v, want resolver metadata", got)
		}
		// The resolver left Agent empty; attributionMeta must backfill it.
		if got.Agent != "quality" {
			t.Errorf("Agent = %q, want backfilled \"quality\"", got.Agent)
		}
	})
}
