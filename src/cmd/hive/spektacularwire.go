package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/pushbroker"
	"github.com/hivecommons/hive/pkg/spektacular"
)

// wireSpektacularRunner installs the Spektacular stage runner on the
// dashboard server when runs.spektacular.enabled is set
// (hivecommons/hive#8303). It is the one place the two worlds meet:
// pkg/dashboard exposes its lease registry as primitives and never imports
// pkg/spektacular (its internal-import ratchet), and pkg/spektacular takes
// that registry through an interface. It reports whether a runner was
// installed; with the feature off (the default) nothing is constructed and
// no Spektacular process is ever started.
func wireSpektacularRunner(cfg *config.Config, srv *dashboard.Server, logger *slog.Logger) bool {
	return wireSpektacularRunnerWithCloneAuth(cfg, srv, logger, nil)
}

func wireSpektacularRunnerWithCloneAuth(cfg *config.Config, srv *dashboard.Server, logger *slog.Logger, cloneAuth func(context.Context, string, string) ([]string, func(), error)) bool {
	if cfg == nil || srv == nil || !cfg.Runs.Spektacular.Enabled {
		return false
	}
	binary := cfg.Runs.Spektacular.BinaryOrDefault()
	probe, err := spektacular.Probe(context.Background(), binary)
	srv.SetSpektacularStatus(dashboard.FrontendSpektacular{Present: probe.Present, Version: probe.Version, Binary: probe.Binary})
	if logger != nil {
		if err != nil {
			logger.Warn("[spektacular] binary not available", "binary", binary, "error", err)
		} else {
			logger.Info("[spektacular] binary detected", "binary", probe.Binary, "version", probe.Version)
		}
	}
	srv.SetStageRunner(spektacular.NewHubRunner(cfg.Runs, srv, logger))
	if cfg.Runs.Spektacular.HubExecutorEnabled() {
		srv.SetStageExecutor(dashboard.NewSpekHubExecutor(srv, cfg.Runs, defaultAgentBackend(cfg), "", cloneAuth, logger))
	}
	if logger != nil {
		logger.Info("[spektacular] stage runner installed",
			"binary", binary,
			"poll", cfg.Runs.Spektacular.PollInterval().String(),
			"max_stage_retries", cfg.Runs.MaxStageRetriesOrDefault())
	}
	return true
}

func spektacularCloneAuth(minter pushbroker.TokenMinter) func(context.Context, string, string) ([]string, func(), error) {
	if minter == nil {
		return nil
	}
	return func(ctx context.Context, repo, dir string) ([]string, func(), error) {
		token, err := minter.MintPushToken(ctx, repo)
		if err != nil {
			return nil, func() {}, err
		}
		if strings.TrimSpace(token) == "" {
			return nil, func() {}, errors.New("empty clone token")
		}
		path := filepath.Join(dir, ".hive-git-credentials-"+strings.NewReplacer("/", "-", "#", "-").Replace(strings.TrimSpace(repo))+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
		if err := os.WriteFile(path, []byte("https://x-access-token:"+token+"@github.com\n"), 0o600); err != nil {
			return nil, func() {}, err
		}
		return []string{"-c", "credential.helper=store --file=" + path}, func() { _ = os.Remove(path) }, nil
	}
}

func defaultAgentBackend(cfg *config.Config) string {
	if cfg != nil {
		for _, a := range cfg.Agents {
			if a.Enabled && strings.TrimSpace(a.Backend) != "" {
				return strings.TrimSpace(a.Backend)
			}
		}
	}
	return config.DefaultSpektacularHubExecutorBackend
}
