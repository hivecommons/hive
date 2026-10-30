package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hotspot"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Merge-hotspot advisor wiring (pkg/hotspot). The merge train feeds PR file
// lists and sibling conflicts into one durable store; once an hour the eval
// cycle scores the window, publishes the result for GET /api/hotspots and
// files at most one modularize issue per hotspot through the writeback gate.

// hotspotStatePath is the advisor's durable state on the PVC; a var so
// tests can redirect it.
var hotspotStatePath = "/data/hotspots.json"

// hotspotAdvisorInterval bounds how often the analysis (and any issue
// filing) runs; the intake runs on every merge-train sweep regardless.
const hotspotAdvisorInterval = time.Hour

// hotspotUnseenAfter is how long a PR may go unobserved by the merge train
// before its open interval is closed at its last sighting. The train only
// lists candidates on bases with two or more PRs, so a PR left alone on its
// base simply stops colliding — which is the right outcome here too.
const hotspotUnseenAfter = 2 * time.Hour

var (
	hotspotStoreOnce sync.Once
	hotspotStoreInst *hotspot.Store
	hotspotMu        sync.Mutex
	hotspotLastRun   time.Time
)

// hotspotStore returns the process-wide store, creating it on first use.
func hotspotStore() *hotspot.Store {
	hotspotStoreOnce.Do(func() {
		st, err := hotspot.NewStore(hotspotStatePath)
		if err != nil {
			slog.Warn("hotspot: state unreadable; starting empty", "path", hotspotStatePath, "error", err)
			st, _ = hotspot.NewStore("")
		}
		hotspotStoreInst = st
	})
	return hotspotStoreInst
}

// hotspotRecorder adapts the store to automerge.HotspotRecorder.
type hotspotRecorder struct{ store *hotspot.Store }

func (r hotspotRecorder) RecordPRFiles(repo string, number int, files []string, openedAt time.Time) {
	r.store.ObservePR(repo, number, files, openedAt)
}

func (r hotspotRecorder) RecordConflict(repo string, number int, files []string) {
	r.store.ObserveConflict(repo, number, files)
}

// hotspotDashSource adapts the store to dashboard.HotspotSource.
type hotspotDashSource struct{ store *hotspot.Store }

func (d hotspotDashSource) HotspotsJSON(repo string) any { return d.store.Results(repo) }

func (d hotspotDashSource) HotspotCounts() (int, int, int) { return d.store.Counts() }

func (d hotspotDashSource) ObserveConflict(repo string, number int, files []string) {
	d.store.ObserveConflict(repo, number, files)
}

// hotspotGitHub adapts the GitHub client to the Filer's narrow interfaces.
type hotspotGitHub struct{ c *github.Client }

func (g hotspotGitHub) IssueFileAllowed(repo, file string) bool {
	item := writeback.Item{HiveAuthored: true}
	return g.c.WritebackAllowed(repo, item, writeback.KindIssueFile, "merge-hotspot issue for "+file).Allowed
}

func (g hotspotGitHub) CreateHotspotIssue(ctx context.Context, repo, title, body string, labels []string) (int, string, error) {
	res, err := g.c.CreateIssue(ctx, github.CreateIssueRequest{ // writeback:gated
		Repo:   repo,
		Title:  title,
		Body:   body,
		Labels: labels,
		Origin: config.OriginSourceGovernor,
	})
	if err != nil {
		return 0, "", err
	}
	return res.Number, res.URL, nil
}

func (g hotspotGitHub) HotspotIssueState(ctx context.Context, repo string, number int) (bool, time.Time, error) {
	sum, ok, err := g.c.IssueSummary(ctx, repo, number)
	if err != nil {
		return false, time.Time{}, err
	}
	if !ok {
		return false, time.Time{}, nil
	}
	return strings.EqualFold(sum.State, "open"), time.Time{}, nil
}

// hotspotSourceReader reads a repo file from the host-local checkout when
// project.checkouts_dir names one, else from the GitHub contents API, with
// a per-pass cache so the same file is fetched once.
func hotspotSourceReader(ctx context.Context, cfg *config.Config, ghClient *github.Client) hotspot.SourceReader {
	cache := map[string]hotspot.FileInfo{}
	var mu sync.Mutex
	return func(repo, path string) (hotspot.FileInfo, bool) {
		key := repo + ":" + path
		mu.Lock()
		if fi, ok := cache[key]; ok {
			mu.Unlock()
			return fi, fi.Lines > 0
		}
		mu.Unlock()
		var src []byte
		if root := cfg.Project.CheckoutRootFor(repo); root != "" {
			clean := filepath.Clean("/" + path)
			if data, err := os.ReadFile(filepath.Join(root, clean)); err == nil {
				src = data
			}
		}
		if src == nil && ghClient != nil {
			if gh := ghClient.GoGitHub(); gh != nil {
				owner, name, ok := strings.Cut(repo, "/")
				if ok {
					if fc, _, _, err := gh.Repositories.GetContents(ctx, owner, name, path, nil); err == nil && fc != nil {
						if body, err := fc.GetContent(); err == nil {
							src = []byte(body)
						}
					}
				}
			}
		}
		fi := hotspot.FileInfo{Lines: hotspot.CountLines(src), Source: src}
		mu.Lock()
		cache[key] = fi
		mu.Unlock()
		return fi, fi.Lines > 0
	}
}

// hotspotSourceFileCount counts the files under a generated file's source
// tree: in the host-local checkout when project.checkouts_dir names one,
// else via the GitHub git-trees API (recursive) with the contents API
// directory listing as a last resort, mirroring hotspotSourceReader. 0
// means unknown.
func hotspotSourceFileCount(ctx context.Context, cfg *config.Config, ghClient *github.Client) func(repo, dir string) int {
	cache := map[string]int{}
	var mu sync.Mutex
	return func(repo, dir string) int {
		key := repo + ":" + dir
		mu.Lock()
		if n, ok := cache[key]; ok {
			mu.Unlock()
			return n
		}
		mu.Unlock()
		n := 0
		if root := cfg.Project.CheckoutRootFor(repo); root != "" {
			_ = filepath.WalkDir(filepath.Join(root, filepath.Clean("/"+dir)), func(_ string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					n++
				}
				return nil
			})
		}
		if n == 0 && ghClient != nil {
			if api := ghClient.GoGitHub(); api != nil {
				n = hotspotSourceFileCountGitHub(ctx, api, repo, dir)
			}
		}
		mu.Lock()
		cache[key] = n
		mu.Unlock()
		return n
	}
}

// hotspotSourceFileCountGitHub counts blobs under dir on the default branch.
// The trees endpoint accepts `HEAD:<path>` as the tree reference and lists
// the subtree recursively; when it fails (or truncates to nothing) the
// contents API's one-level directory listing is used instead.
func hotspotSourceFileCountGitHub(ctx context.Context, api *gh.Client, repo, dir string) int {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return 0
	}
	dir = strings.Trim(filepath.ToSlash(filepath.Clean("/"+dir)), "/")
	if dir == "" {
		return 0
	}
	n := 0
	if tree, _, err := api.Git.GetTree(ctx, owner, name, "HEAD:"+dir, true); err == nil && tree != nil {
		for _, e := range tree.Entries {
			if e.GetType() == "blob" {
				n++
			}
		}
	}
	if n > 0 {
		return n
	}
	if _, entries, _, err := api.Repositories.GetContents(ctx, owner, name, dir, nil); err == nil {
		for _, e := range entries {
			if e.GetType() == "file" {
				n++
			}
		}
	}
	return n
}

// runHotspotAdvisor is the eval-tick half of the advisor: at most once per
// hotspotAdvisorInterval it closes stale PR intervals, prunes the window,
// scores every repo the merge train has reported on, publishes the result
// and — unless governor.hotspot_enabled is false — files modularize issues
// through the writeback gate. It never lists PRs or files from GitHub; the
// only reads are the hotspot files' current contents (for size and
// top-level symbols) and the state of issues it filed earlier.
func runHotspotAdvisor(ctx context.Context, cfg *config.Config, ghClient *github.Client, actionable *github.ActionableResult, logger *slog.Logger) {
	if cfg == nil || ghClient == nil {
		return
	}
	if !hotspotMu.TryLock() {
		return
	}
	defer hotspotMu.Unlock()
	now := time.Now()
	if !hotspotLastRun.IsZero() && now.Sub(hotspotLastRun) < hotspotAdvisorInterval {
		return
	}
	hotspotLastRun = now

	store := hotspotStore()
	if actionable != nil {
		open := map[string][]int{}
		for _, pr := range actionable.PRs.Items {
			open[pr.Repo] = append(open[pr.Repo], pr.Number)
		}
		for repo, nums := range open {
			store.TouchOpen(repo, nums)
		}
	}
	store.ExpireUnseen(hotspotUnseenAfter)
	g := cfg.Governor
	window := time.Duration(g.HotspotWindowDaysOrDefault()) * 24 * time.Hour
	refile := time.Duration(g.HotspotRefileDaysOrDefault()) * 24 * time.Hour
	store.Prune(window, refile)
	opts := hotspot.Options{
		Window:          window,
		MinPRs:          g.HotspotMinPRsOrDefault(),
		MinOverlaps:     g.HotspotMinOverlapsOrDefault(),
		MinLines:        g.HotspotMinLinesOrDefault(),
		SourceFileCount: hotspotSourceFileCount(ctx, cfg, ghClient),
	}
	read := hotspotSourceReader(ctx, cfg, ghClient)
	ghAdapter := hotspotGitHub{c: ghClient}
	filer := &hotspot.Filer{
		Store: store, Gate: ghAdapter, Creator: ghAdapter, Stater: ghAdapter,
		Opts: opts, Refile: refile,
		Logf: func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...)) },
	}
	for _, repo := range store.Repos() {
		if ctx.Err() != nil {
			return
		}
		prs, conflicts := store.Snapshot(repo)
		hs := hotspot.Analyze(repo, prs, conflicts, read, opts, now)
		if g.HotspotIssuesEnabled() {
			filer.File(ctx, repo, hs)
		}
		store.SetResults(repo, hs)
		if len(hs) > 0 {
			logger.Info("hotspot advisor: scored", "repo", repo, "hotspots", len(hs), "top", hs[0].File, "top_score", hs[0].Score, "top_recommendation", hs[0].Recommendation)
		}
	}
}
