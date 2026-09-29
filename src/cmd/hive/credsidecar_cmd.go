package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/credsidecar"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/proxy"
)

// Exit codes of `hive credsidecar`.
const (
	// credSidecarConfigExitCode: the sidecar's configuration is unusable (a
	// usage-class error, like jev's 2). Fix the env, do not restart-loop.
	credSidecarConfigExitCode = 2
	// credSidecarServeExitCode: the server failed after starting.
	credSidecarServeExitCode = 1
)

// credSidecarDialTimeout bounds the sidecar's connect+TLS to GitHub, matching
// the proxy's own upstream dial bound.
const credSidecarDialTimeout = 15 * time.Second

// credSidecarServe runs the server until ctx ends. A seam so tests can check
// the wiring without binding a port for the life of the test.
var credSidecarServe = func(ctx context.Context, s *credsidecar.Server, addr string) error {
	return s.ListenAndServe(ctx, addr)
}

// runCredSidecar implements `hive credsidecar` (#9586 phase 2): the isolated
// GitHub credential holder. It holds the App key, mints tier-scoped tokens,
// and attaches them only to requests the hive's proxy signed. See
// pkg/credsidecar for the protocol and the threat model.
func runCredSidecar(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "hive %s takes no arguments; it is configured by environment (%s, %s, %s, %s)\n",
			credsidecar.Subcommand, credsidecar.KeyFileEnv, credsidecar.AppIDEnv, credsidecar.InstallationIDEnv, credsidecar.AppKeyFileEnv)
		return credSidecarConfigExitCode
	}
	logger := slog.New(logscrub.NewHandler(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := credsidecar.LoadServerConfig(getenv)
	if err != nil {
		logger.Error("credential sidecar: refusing to start, configuration unusable", "error", err.Error())
		return credSidecarConfigExitCode
	}
	auth, err := github.NewAppAuth(cfg.AppID, cfg.InstallationID, cfg.AppKeyFile, logger, cfg.APIURL)
	if err != nil {
		logger.Error("credential sidecar: refusing to start, GitHub App key unusable", "error", err.Error(), "app_key_file", cfg.AppKeyFile)
		return credSidecarConfigExitCode
	}
	srv, err := credsidecar.NewServer(cfg, auth, logger, credsidecar.WithDialContext(proxy.EgressMarkDialContext(credSidecarDialTimeout)))
	if err != nil {
		logger.Error("credential sidecar: refusing to start", "error", err.Error())
		return credSidecarConfigExitCode
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := credSidecarServe(ctx, srv, cfg.ListenAddr); err != nil {
		logger.Error("credential sidecar stopped", "error", err.Error())
		return credSidecarServeExitCode
	}
	return 0
}
