package main

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/upstreamwatch"
)

// upstreamWatchStatePath is the upstream watch's durable state: per-repo
// watermark, filed-ref dedupe index and dismissals. It lives on the PVC
// (/data) so a restart never re-scans an upstream from zero; a var so tests
// can redirect it.
var upstreamWatchStatePath = "/data/upstream-watch.json"

// upstreamWatchLastRun is the wall-clock of the last watch pass (zero =
// never). Guarded by upstreamWatchMu.
var upstreamWatchLastRun time.Time

// upstreamWatchMu makes a pass single-flight: dashboard saves run whole eval
// cycles on their own goroutines, so two cycles can reach runUpstreamWatch at
// once.
var upstreamWatchMu sync.Mutex

// runUpstreamWatch is the eval-tick half of the opt-in upstream watch
// (hivecommons/hive#9967). With upstream_watch.enabled off (the default) it
// returns before touching GitHub or the disk. When on, at most once per
// upstream_watch.interval it lists each configured repo's upstream merged PRs
// and releases since the durable watermark, drops the ones that no longer
// apply to the fork, and opens a labelled fork issue for the rest. It opens
// issues only, never PRs. ctx bounds the pass: cancelling it stops the loop
// between items with the state saved.
func runUpstreamWatch(ctx context.Context, cfg *config.Config, ghClient *github.Client, logger *slog.Logger) {
	if cfg == nil || !cfg.UpstreamWatch.Enabled || len(cfg.UpstreamWatch.Repos) == 0 {
		return
	}
	if !upstreamWatchMu.TryLock() {
		return
	}
	defer upstreamWatchMu.Unlock()
	client := ghClient.GoGitHub()
	if client == nil {
		return
	}
	now := time.Now()
	interval := cfg.UpstreamWatch.Interval
	if interval <= 0 {
		interval = config.DefaultUpstreamWatchInterval
	}
	if !upstreamWatchLastRun.IsZero() && now.Sub(upstreamWatchLastRun) < interval {
		return
	}
	upstreamWatchLastRun = now

	store := upstreamwatch.NewFileStore(upstreamWatchStatePath)
	keys := make([]string, 0, len(cfg.UpstreamWatch.Repos))
	for k := range cfg.UpstreamWatch.Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		rc := cfg.UpstreamWatch.Repos[key]
		owner, repo, ok := upstreamWatchFork(cfg.Project.Org, key)
		if !ok {
			logger.Warn("upstream watch: cannot resolve fork owner/repo; set project.org", "repo", key)
			continue
		}
		src := upstreamwatch.NewGitHubSource(client, owner, repo, rc)
		upstream, err := src.Upstream(ctx)
		if err != nil {
			logger.Warn("upstream watch: resolve upstream failed", "repo", key, "error", err)
			continue
		}
		label := rc.Label
		if strings.TrimSpace(label) == "" {
			label = config.DefaultUpstreamWatchLabel
		}
		startFrom, _ := rc.StartFromTime()
		w := upstreamwatch.New(upstreamwatch.Options{
			Repo:            key,
			Upstream:        upstream,
			Label:           label,
			MaxIssuesPerRun: rc.MaxIssuesPerRun,
			StartFrom:       startFrom,
		}, src,
			upstreamwatch.NewGitHubForkContents(client, owner, repo),
			store,
			upstreamWatchFiler{Filer: upstreamwatch.NewGitHubFiler(client, owner, repo)})
		res, err := w.Run(ctx)
		attrs := []any{
			"repo", key, "upstream", upstream,
			"filed", len(res.Filed), "skipped", len(res.Skipped),
			"deduped", len(res.Deduped), "dismissed", len(res.Dismissed),
			"capped", res.Capped, "remaining", res.Remaining,
			"first_pass", res.FirstPass,
		}
		if res.FirstPass && !res.Start.IsZero() {
			attrs = append(attrs, "start", res.Start)
		}
		var truncated *upstreamwatch.TruncatedError
		if errors.As(err, &truncated) {
			logger.Warn("upstream watch: upstream listing truncated; nothing filed, watermark unchanged",
				append(attrs, "window", truncated.Window(), "advice", upstreamWatchTruncatedAdvice(key, res.FirstPass), "error", err)...)
			continue
		}
		if err != nil {
			logger.Warn("upstream watch pass failed", append(attrs, "error", err)...)
			continue
		}
		if len(res.Filed) > 0 || res.Capped {
			logger.Info("upstream watch pass", attrs...)
		} else {
			logger.Debug("upstream watch pass", attrs...)
		}
	}
}

// upstreamWatchTruncatedAdvice is the remedy logged with a truncated listing.
// start_from only moves a first pass, so a repo with history needs its state
// entry removed first; issues already filed are found again by their marker.
func upstreamWatchTruncatedAdvice(key string, firstPass bool) string {
	advice := "set a later upstream_watch.repos." + key + ".start_from (YYYY-MM-DD) so the listing fits the window"
	if firstPass {
		return advice
	}
	return "start_from applies only to a first pass: remove the " + key + " entry from " + upstreamWatchStatePath + " and " + advice
}

// upstreamWatchFork resolves an upstream_watch.repos key to the fork's
// owner/repo: an org-qualified key is split, a bare one is qualified with
// project.org.
func upstreamWatchFork(org, key string) (owner, repo string, ok bool) {
	if strings.Contains(key, "/") {
		return upstreamwatch.SplitRepo(key)
	}
	org = strings.TrimSpace(org)
	key = strings.TrimSpace(key)
	if org == "" || key == "" {
		return "", "", false
	}
	return org, key, true
}

// upstreamWatchFiler routes the rendered issue through the same mention
// sanitizer as everything else the hive posts: the summary is copied from an
// upstream PR body and must not ping upstream people from the fork.
type upstreamWatchFiler struct {
	upstreamwatch.Filer
}

func (f upstreamWatchFiler) File(ctx context.Context, issue upstreamwatch.Issue) (int, error) {
	issue.Title = advisory.NeutralizeMentions(issue.Title)
	issue.Body = advisory.NeutralizeMentions(issue.Body)
	return f.Filer.File(ctx, issue)
}
