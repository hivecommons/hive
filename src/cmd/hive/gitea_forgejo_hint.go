package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
)

const (
	// giteaForgejoHintInterval is how often instances not yet answered are
	// probed again, and how soon a reload that adds a gitea repo is noticed.
	giteaForgejoHintInterval = 10 * time.Minute
	// giteaVersionProbeTimeout bounds one /api/v1/version request.
	giteaVersionProbeTimeout = 15 * time.Second
)

// probeGiteaVersion fetches an instance's /api/v1/version. It is a variable
// so tests can substitute a fake.
var probeGiteaVersion = forge.GiteaFamilyVersion

// giteaProbeTarget is one instance URL that forge: gitea repos point at.
type giteaProbeTarget struct {
	url string
	// repos are the listed repos that resolve to this instance; empty when
	// only project.forge does.
	repos []string
	// sendToken is true when the gitea token may go to url for at least one
	// of them (CheckForgeTokenHost), the rule forgeForRepo applies.
	sendToken bool
}

// giteaProbeTargets returns each distinct instance URL that a repo on
// forge: gitea, or project.forge: gitea itself, resolves to, in config order.
func giteaProbeTargets(cfg *config.Config) []giteaProbeTarget {
	if cfg == nil {
		return nil
	}
	var out []giteaProbeTarget
	index := map[string]int{}
	add := func(repo string) {
		if cfg.ForgeKindForRepo(repo) != config.ForgeGitea {
			return
		}
		u := cfg.ForgeURLForRepo(repo)
		if u == "" {
			return
		}
		i, ok := index[u]
		if !ok {
			i = len(out)
			index[u] = i
			out = append(out, giteaProbeTarget{url: u})
		}
		t := &out[i]
		if repo != "" {
			t.repos = append(t.repos, repo)
		}
		if !t.sendToken && cfg.CheckForgeTokenHost(repo) == nil {
			t.sendToken = true
		}
	}
	for _, repo := range cfg.Project.Repos {
		add(repo)
	}
	add("")
	return out
}

// giteaForgejoHint logs once per instance when a forge: gitea config points
// at an instance whose /api/v1/version reports Forgejo, and suggests the
// forgejo kind: only that kind gets the Forgejo automerge and close-on-merge
// sweeps (config.ForgeSweepsFor). It is used from one goroutine.
type giteaForgejoHint struct {
	logger *slog.Logger
	// answered holds the instance URLs whose version has been read. A probe
	// that fails is retried on the next check, so an instance that was down
	// at startup is still checked.
	answered map[string]bool
}

func newGiteaForgejoHint(logger *slog.Logger) *giteaForgejoHint {
	if logger == nil {
		logger = slog.Default()
	}
	return &giteaForgejoHint{logger: logger, answered: map[string]bool{}}
}

// check probes every gitea instance in cfg not answered yet.
func (h *giteaForgejoHint) check(ctx context.Context, cfg *config.Config) {
	for _, t := range giteaProbeTargets(cfg) {
		if h.answered[t.url] || ctx.Err() != nil {
			continue
		}
		token := ""
		if t.sendToken {
			token = os.Getenv(cfg.Gitea.TokenEnvName())
		}
		pctx, cancel := context.WithTimeout(ctx, giteaVersionProbeTimeout)
		version, err := probeGiteaVersion(pctx, token, forge.Options{BaseURL: t.url})
		cancel()
		if err != nil {
			h.logger.Debug("gitea version probe failed; will retry", "url", t.url, "error", err)
			continue
		}
		h.answered[t.url] = true
		if forge.IsForgejoVersion(version) {
			h.logger.Warn("forge is gitea but the instance reports a Forgejo version; set forge: forgejo and move its URL and token_env to the forgejo: block to use the Forgejo adapter and its automerge and close-on-merge sweeps",
				"url", t.url, "version", version, "repos", t.repos)
		}
	}
}

// startGiteaForgejoHint checks cfgFn() now and then every interval until ctx
// ends, so a reload that adds a gitea repo is checked too. A hive with no
// gitea repo makes no request. The returned channel closes once the loop has
// exited.
func startGiteaForgejoHint(ctx context.Context, cfgFn func() *config.Config, logger *slog.Logger, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	h := newGiteaForgejoHint(logger)
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			h.check(ctx, cfgFn())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
