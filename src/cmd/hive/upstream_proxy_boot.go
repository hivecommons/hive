package main

import (
	"log/slog"
	"os"

	"github.com/hivecommons/hive/crux/upstreamproxy"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// configureUpstreamProxy resolves the upstream forward-proxy rule set
// (config block, HIVE_UPSTREAM_PROXY_* env, or HTTPS_PROXY fallback) and
// installs it process-wide: crux/upstreamproxy.Default for in-process git and
// the MITM proxy, pkg/github's shared transport for control-plane API calls.
// A malformed setting is logged and ignored so the hive still boots with
// direct egress rather than refusing to start.
func configureUpstreamProxy(logger *slog.Logger, cfg *config.Config) {
	sel, err := upstreamproxy.Resolve(cfg.Network.UpstreamProxy.UpstreamProxy(), os.Getenv)
	if err != nil {
		logger.Error("boot: upstream proxy config ignored", "error", err)
		sel = nil
	}
	upstreamproxy.SetDefault(sel)
	github.SetUpstreamProxy(sel)
	if sel == nil {
		return
	}
	logger.Info("boot: upstream proxy chaining enabled",
		"proxy", sel.URL().Scheme+"://"+sel.URL().Host,
		"source", sel.Source(),
		"restricted_hosts", sel.Restricted(),
	)
}
