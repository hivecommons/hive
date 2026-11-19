package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/upstreamproxy"
	"github.com/hivecommons/hive/pkg/config"
)

func TestConfigureUpstreamProxyInstallsSelectorFromConfig(t *testing.T) {
	t.Setenv("HIVE_UPSTREAM_PROXY_URL", "")
	t.Setenv("HIVE_UPSTREAM_PROXY_HOSTS", "")
	t.Setenv("HIVE_NO_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Cleanup(func() { upstreamproxy.SetDefault(nil) })

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.Config{}
	cfg.Network.UpstreamProxy = config.UpstreamProxyConfig{
		URL:   "http://user:secret@proxy.corp.example:3128",
		Hosts: []string{"ghe.corp.example"},
	}
	configureUpstreamProxy(logger, cfg)

	sel := upstreamproxy.Default()
	if sel == nil {
		t.Fatal("selector not installed")
	}
	if sel.ProxyFor("ghe.corp.example:443") == nil || sel.ProxyFor("api.github.com:443") != nil {
		t.Fatal("selector rules do not match the config block")
	}
	if !strings.Contains(buf.String(), "upstream proxy chaining enabled") || strings.Contains(buf.String(), "secret") {
		t.Fatalf("boot log must announce chaining without leaking credentials:\n%s", buf.String())
	}

	// Malformed input is logged and leaves egress direct instead of aborting boot.
	buf.Reset()
	cfg.Network.UpstreamProxy = config.UpstreamProxyConfig{URL: "socks5://nope:1080"}
	configureUpstreamProxy(logger, cfg)
	if upstreamproxy.Default() != nil || !strings.Contains(buf.String(), "upstream proxy config ignored") {
		t.Fatalf("bad config must clear the selector and log:\n%s", buf.String())
	}

	// Nothing configured: no selector, no log line.
	buf.Reset()
	cfg.Network.UpstreamProxy = config.UpstreamProxyConfig{}
	configureUpstreamProxy(logger, cfg)
	if upstreamproxy.Default() != nil || buf.Len() != 0 {
		t.Fatalf("unset config must be silent and direct:\n%s", buf.String())
	}
}
