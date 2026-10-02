package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
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

func wireSpektacularRunnerWithCloneAuth(cfg *config.Config, srv *dashboard.Server, logger *slog.Logger, cloneAuth dashboard.SpekHubCloneAuth) bool {
	return wireSpektacularRunnerStages(cfg, srv, logger, cloneAuth, true)
}

// wireSpektacularRunnerStages is wireSpektacularRunnerWithCloneAuth with the
// hub executor install step made optional, so rewireSpektacular's keep path
// (hivecommons/hive#10069) can rebuild the stage runner and re-probe the
// binary without installing a new executor — doing so would swap out the
// kept one and stop it via dashboard.Server.SetStageExecutor's old-vs-new
// comparison, even though the swap is immediately reverted.
func wireSpektacularRunnerStages(cfg *config.Config, srv *dashboard.Server, logger *slog.Logger, cloneAuth dashboard.SpekHubCloneAuth, installExecutor bool) bool {
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
	if installExecutor && cfg.Runs.Spektacular.HubExecutorEnabled() {
		backend := cfg.Runs.Spektacular.HubExecutor.BackendOrDefault(defaultAgentBackend(cfg))
		exec := dashboard.NewSpekHubExecutor(srv, cfg.Runs, backend, "", cloneAuth, logger)
		srv.SetStageExecutor(exec)
		if logger != nil {
			logger.Info("[spektacular] hub executor installed",
				"identity", exec.Identity,
				"backend", exec.Backend,
				"max_concurrent", cfg.Runs.Spektacular.HubExecutor.MaxConcurrentOrDefault(),
				"max_stage_retries", cfg.Runs.MaxStageRetriesOrDefault())
		}
	}
	if logger != nil {
		logger.Info("[spektacular] stage runner installed",
			"binary", binary,
			"poll", cfg.Runs.Spektacular.PollInterval().String())
	}
	return true
}

// spektacularRewireMu serializes live rewires: two dashboard saves racing
// must not interleave their runner/executor swaps.
var spektacularRewireMu sync.Mutex

// rewireSpektacular re-applies runs.spektacular from the live config after a
// dashboard edit (#9172). The runner only carries config, so it is always
// rebuilt; the binary is re-probed so the Extensions card shows what is now
// installed. The hub executor is different: while it is running a stage it
// owns the in-flight bookkeeping that keeps that stage's lease alive and its
// worktree unswept, so a busy executor whose settings changed is never swapped
// out from under the stage. Then nothing is touched and false is returned; the
// dashboard retries on every stage worker tick until the executor is idle. An
// executor whose settings did not change is kept as is.
func rewireSpektacular(cfg *config.Config, srv *dashboard.Server, logger *slog.Logger, cloneAuth dashboard.SpekHubCloneAuth) bool {
	if cfg == nil || srv == nil {
		return false
	}
	spektacularRewireMu.Lock()
	defer spektacularRewireMu.Unlock()

	current := srv.StageExecutor()
	keepExecutor := false
	if current != nil {
		if spekExecutorUpToDate(current, cfg) {
			keepExecutor = true
		} else if current.Status().Running > 0 {
			if logger != nil {
				logger.Info("[spektacular] config change deferred until the hub executor's running stage finishes",
					"running", current.Status().Running)
			}
			return false
		}
	}

	if !cfg.Runs.Spektacular.Enabled {
		srv.SetStageRunner(nil)
		srv.SetStageExecutor(nil)
		srv.ClearSpektacularStatus()
		if logger != nil {
			logger.Info("[spektacular] stage runner removed (runs.spektacular.enabled is off)")
		}
		return true
	}
	if keepExecutor {
		// Rebuild the runner and re-probe the binary only; installing a new
		// executor here would swap the kept one out from under itself and
		// stop it, even though the swap is reverted right after
		// (hivecommons/hive#10069). Leaving it alone keeps its held
		// generations and activity — and keeps it running.
		wireSpektacularRunnerStages(cfg, srv, logger, cloneAuth, false)
	} else {
		srv.SetStageExecutor(nil)
		wireSpektacularRunnerWithCloneAuth(cfg, srv, logger, cloneAuth)
	}
	return true
}

// spekExecutorUpToDate reports whether the installed executor was built from
// the same settings the live config would build it from now.
func spekExecutorUpToDate(current dashboard.StageExecutor, cfg *config.Config) bool {
	exec, ok := current.(*dashboard.SpekHubExecutor)
	if !ok || !cfg.Runs.Spektacular.Enabled || !cfg.Runs.Spektacular.HubExecutorEnabled() {
		return false
	}
	want := dashboard.NewSpekHubExecutor(nil, cfg.Runs, cfg.Runs.Spektacular.HubExecutor.BackendOrDefault(defaultAgentBackend(cfg)), "", nil, nil)
	return exec.Backend == want.Backend && exec.Model == want.Model && exec.Identity == want.Identity &&
		reflect.DeepEqual(exec.Config, cfg.Runs)
}

func spektacularCloneAuth(minter pushbroker.TokenMinter) dashboard.SpekHubCloneAuth {
	if minter == nil {
		return nil
	}
	return func(ctx context.Context, repo, dir string) ([]string, string, func(), error) {
		token, err := minter.MintPushToken(ctx, repo)
		if err != nil {
			return nil, "", func() {}, err
		}
		if strings.TrimSpace(token) == "" {
			return nil, "", func() {}, errors.New("empty clone token")
		}
		path := filepath.Join(dir, dashboard.SpekHubCloneCredentialFilePrefix+strings.NewReplacer("/", "-", "#", "-").Replace(strings.TrimSpace(repo))+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
		if err := os.WriteFile(path, []byte("https://x-access-token:"+token+"@github.com\n"), 0o600); err != nil {
			return nil, "", func() {}, err
		}
		return []string{"-c", "credential.helper=store --file=" + path}, token, func() { _ = os.Remove(path) }, nil
	}
}

func defaultAgentBackend(cfg *config.Config) string {
	if cfg != nil {
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			a := cfg.Agents[name]
			if a.Enabled && strings.TrimSpace(a.Backend) != "" {
				return strings.TrimSpace(a.Backend)
			}
		}
	}
	return config.DefaultSpektacularHubExecutorBackend
}

// lazySpektacularCloneAuth resolves the minter at launch time so an App that
// arrives or is rebuilt after boot is used by the hub executor. With no
// minter it clones anonymously, as an unset CloneAuth does.
func lazySpektacularCloneAuth(minter func() pushbroker.TokenMinter) dashboard.SpekHubCloneAuth {
	return func(ctx context.Context, repo, dir string) ([]string, string, func(), error) {
		auth := spektacularCloneAuth(minter())
		if auth == nil {
			return nil, "", func() {}, nil
		}
		return auth(ctx, repo, dir)
	}
}
