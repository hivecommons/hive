package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/notify"
)

// Boot wiring for hub mode, extracted from runHub so it can be tested (#7224).
//
// runHub was 0% covered and unreachable from any test: it started
// network-probing background pollers on context.Background(), bound a port,
// and called os.Exit on a serve failure. None of those can happen in a unit
// test, so the wiring ABOVE them — port parsing, hook dispatch, the
// token-gated /api/reach source, the reach reporter, graceful shutdown — went
// unverified even though it is ordinary, reviewable logic that boots the hub.
//
// The three process-level effects move behind hubDeps. runHub stays a thin
// production wrapper, matching the seam this repo already uses for
// StartPermissionsWatcher -> runPermissionsWatcher (pkg/agent) and
// watchHubRollout -> watchHubRolloutWithInterval (pkg/hub, #7220).

// hubDeps are the process-level effects hub mode performs. Injected so the
// boot wiring can be exercised without starting pollers, binding a port, or
// terminating the test process.
type hubDeps struct {
	// startPollers launches the long-lived SaaS pollers (provision watcher,
	// SHA poller, auth audit, advisory diagnostics).
	startPollers func(ctx context.Context, srv *hub.HubServer)
	// serve runs the HTTP server until it stops.
	serve func(srv *hub.HubServer, port int) error
	// fatal ends the process after an unrecoverable serve error.
	fatal func(code int)
}

// defaultHubDeps is the production wiring: real pollers, real listener, real
// exit.
func defaultHubDeps() hubDeps {
	return hubDeps{
		startPollers: func(ctx context.Context, srv *hub.HubServer) { srv.StartBackgroundPollers(ctx) },
		serve:        func(srv *hub.HubServer, port int) error { return srv.Start(port) },
		fatal:        os.Exit,
	}
}

// defaultHubPort is the port hub mode listens on when HIVE_HUB_PORT is unset
// or unparseable.
const defaultHubPort = 3001

// hubPortFromEnv reads HIVE_HUB_PORT, falling back to defaultHubPort.
//
// A malformed value is deliberately ignored rather than fatal: hub mode
// serving on its documented default is a better failure mode than a hub that
// refuses to boot because of a typo in an env var.
func hubPortFromEnv(getenv func(string) string) int {
	if p := getenv("HIVE_HUB_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			return parsed
		}
	}
	return defaultHubPort
}

// wireHubHooks installs the notification hook dispatcher for hub mode.
//
// A missing config file is not an error — a hub can run without one, and hooks
// simply stay off. Any OTHER load failure is surfaced as a warning, because
// silently dropping notifications from a config that exists but is broken is
// how a fleet stops reporting without anyone noticing.
func wireHubHooks(logger *slog.Logger, configPath string) {
	cfg, err := config.LoadWithDashboardOverlay(configPath)
	if err == nil {
		notifier := notify.New(cfg.Notifications, logger)
		notifier.SetHiveID(cfg.HiveID)
		buildHookDispatcher(cfg, hookSinks{Notifier: notifier}, logger)
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		logger.Warn("hub hooks disabled: failed to load config", "path", configPath, "error", err)
	}
}

// wireHubReach wires the /api/reach data sources (#3994).
//
// The merged-PR source needs GitHub credentials; hub mode has no ambient
// client, so it reuses the standard token client when HIVE_GITHUB_TOKEN is
// set. Without a token NO source is installed and the endpoint reports 503 —
// deliberately, rather than serving fabricated data.
//
// The registry-backed reporter is unconditional (#3973 epic: producer #3993 ->
// consumer #3994): it has no external dependencies, and without it the
// endpoint would keep answering from the empty stub forever.
// The token-gated branch is announced at boot. Without it the only symptom of
// a missing token is a 503 at request time, with nothing in the log tying it
// back to credentials — the operator sees a broken endpoint, not a
// configuration gap.
func wireHubReach(srv *hub.HubServer, logger *slog.Logger, ghToken string) {
	if ghToken != "" {
		reachGH := github.NewClient(ghToken, "hivecommons", []string{"hive"}, logger, "")
		srv.SetReachPRSource(hub.NewGitHubPRSource(reachGH, gitBranch))
		logger.Info("reach PR source wired", "source", "github", "branch", gitBranch)
	} else {
		logger.Warn("reach PR source disabled: HIVE_GITHUB_TOKEN not set, /api/reach will report 503")
	}
	srv.SetReachReporter(srv.RegistryReachReporter())
}

// hubShutdownTimeout bounds graceful shutdown after a termination signal.
const hubShutdownTimeout = 10 * time.Second

// shutdownHubOnSignal blocks until a termination signal arrives, then shuts the
// server down gracefully. It returns when the channel yields or is closed, so a
// test can drive it without sending a real signal.
func shutdownHubOnSignal(sigCh <-chan os.Signal, srv *hub.HubServer, logger *slog.Logger) {
	sig, ok := <-sigCh
	if !ok {
		return
	}
	logger.Info("hub received signal, shutting down gracefully", "signal", sig)
	if err := srv.Shutdown(hubShutdownTimeout); err != nil {
		logger.Error("hub graceful shutdown failed", "error", err)
	}
}

// runHubWithDeps is runHub with its process-level effects injected.
//
// Order matters and is preserved from the original: hooks and reach wiring are
// installed BEFORE the pollers start, so no poller can observe a half-wired
// server.
func runHubWithDeps(ctx context.Context, logger *slog.Logger, configPath string, deps hubDeps) {
	port := hubPortFromEnv(os.Getenv)
	logger.Info("starting in HUB mode", "port", port)

	hubSrv := hub.NewHubServer(port, logger, gitShort, gitBranch)
	wireHubHooks(logger, configPath)
	installUpgradePauseEmitter(hubSrv)
	wireHubReach(hubSrv, logger, os.Getenv("HIVE_GITHUB_TOKEN"))

	// Long-lived SaaS pollers are started here — at the composition root — not
	// inside route registration, so constructing a HubServer stays free of
	// background goroutines.
	deps.startPollers(ctx, hubSrv)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go shutdownHubOnSignal(sigCh, hubSrv, logger)

	if err := deps.serve(hubSrv, port); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("hub server failed", "error", err)
		deps.fatal(1)
		return
	}
	logger.Info("hub server stopped")
}
