package complexity

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
)

// CheckInterval is how often the runner re-checks whether the current
// month's snapshot is missing for any repo. The check is a stat per repo, so
// it is cheap; the month boundary is what actually triggers work. Hourly
// also bounds how long a repo whose first attempt failed (no GitHub client
// yet on a hosted spoke, transient archive error) waits for a retry.
const CheckInterval = time.Hour

// RetainMonths is how many monthly snapshots are kept per repo (RFC #12062
// open question; 24 covers the two-year maintainability trend panel 7 draws).
// Older files are removed after a successful write.
const RetainMonths = 24

// Checkout is a readable copy of a repo's default branch. Cleanup releases
// it; nil is allowed.
type Checkout struct {
	Dir     string
	Ref     string
	Cleanup func()
}

// Source fetches a repo's default branch for measuring.
type Source interface {
	Fetch(ctx context.Context, repo string) (Checkout, error)
}

// SourceFunc adapts a function to Source.
type SourceFunc func(ctx context.Context, repo string) (Checkout, error)

// Fetch implements Source.
func (f SourceFunc) Fetch(ctx context.Context, repo string) (Checkout, error) { return f(ctx, repo) }

// Analyzer runs one static-analysis tool over a checkout and returns its
// line-oriented output, which CountFindings tallies. A nil error with empty
// output means no findings; ErrAnalyzerUnavailable means the tool is not
// installed and the snapshot simply omits it.
type Analyzer struct {
	Tool string
	Run  func(ctx context.Context, dir string) (string, error)
}

// ErrAnalyzerUnavailable is returned by an Analyzer whose tool is missing.
var ErrAnalyzerUnavailable = errors.New("complexity: analyzer unavailable")

// Runner writes the monthly snapshot for every managed repo. Report-only:
// nothing it produces is read by any merge path.
type Runner struct {
	// Root is the evidence root (snapshots land at
	// <Root>/<org>/<repo>/complexity/<YYYY-MM>.json).
	Root string
	// Repos returns the current managed repos as "org/name". Read on every
	// run so a config reload applies.
	Repos func() []string
	// Source fetches a default-branch checkout. Required.
	Source Source
	// Analyzers run after Measure; failures are logged and skipped.
	Analyzers []Analyzer
	// Threshold is the cyclomatic threshold; <=0 means DefaultThreshold.
	Threshold int
	// Retain is how many monthly files to keep per repo; <=0 means
	// RetainMonths.
	Retain int
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// RunResult summarises one pass.
type RunResult struct {
	Month   string
	Written []string
	Skipped []string
	Failed  map[string]error
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Runner) repos() []string {
	if r.Repos == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, repo := range r.Repos() {
		repo = strings.TrimSpace(repo)
		if repo == "" || seen[repo] {
			continue
		}
		seen[repo] = true
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

// Due returns the repos that have no snapshot for the month containing now.
// A repo whose path is invalid is omitted (RunOnce reports it as failed).
func (r *Runner) Due(now time.Time) []string {
	month := Month(now)
	var due []string
	for _, repo := range r.repos() {
		p, err := Path(r.Root, repo, month)
		if err != nil {
			due = append(due, repo)
			continue
		}
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			due = append(due, repo)
		}
	}
	return due
}

// RunOnce measures every repo missing the current month's snapshot. It is
// idempotent within a month: a repo with a file is skipped, and Write's
// ErrExists (a concurrent writer) counts as skipped too. Per-repo failures
// are logged and never abort the pass.
func (r *Runner) RunOnce(ctx context.Context) RunResult {
	now := r.now()
	res := RunResult{Month: Month(now), Failed: map[string]error{}}
	due := map[string]bool{}
	for _, repo := range r.Due(now) {
		due[repo] = true
	}
	for _, repo := range r.repos() {
		if ctx.Err() != nil {
			return res
		}
		if !due[repo] {
			res.Skipped = append(res.Skipped, repo)
			continue
		}
		path, err := r.snapshot(ctx, repo, res.Month, now)
		switch {
		case errors.Is(err, ErrExists):
			res.Skipped = append(res.Skipped, repo)
		case err != nil:
			res.Failed[repo] = err
			r.logger().Warn("complexity snapshot failed", "repo", repo, "month", res.Month, "error", err)
		default:
			res.Written = append(res.Written, repo)
			r.logger().Info("complexity snapshot written", "repo", repo, "month", res.Month, "path", path)
			if n, perr := Prune(r.Root, repo, r.retain()); perr != nil {
				r.logger().Warn("complexity snapshot prune failed", "repo", repo, "error", perr)
			} else if n > 0 {
				r.logger().Info("complexity snapshots pruned", "repo", repo, "removed", n)
			}
		}
	}
	return res
}

func (r *Runner) retain() int {
	if r.Retain > 0 {
		return r.Retain
	}
	return RetainMonths
}

func (r *Runner) snapshot(ctx context.Context, repo, month string, now time.Time) (string, error) {
	if _, err := Path(r.Root, repo, month); err != nil {
		return "", err
	}
	if r.Source == nil {
		return "", errors.New("complexity: no source configured")
	}
	co, err := r.Source.Fetch(ctx, repo)
	if err != nil {
		return "", err
	}
	if co.Cleanup != nil {
		defer co.Cleanup()
	}
	s, err := Measure(co.Dir, r.Threshold)
	if err != nil {
		return "", err
	}
	s.Repo, s.Month, s.Ref, s.CapturedAt = repo, month, co.Ref, now.UTC()
	for _, a := range r.Analyzers {
		if a.Run == nil || a.Tool == "" {
			continue
		}
		out, aerr := a.Run(ctx, co.Dir)
		if errors.Is(aerr, ErrAnalyzerUnavailable) {
			continue
		}
		if aerr != nil {
			r.logger().Warn("complexity static analysis skipped", "repo", repo, "tool", a.Tool, "error", aerr)
			continue
		}
		s.AddWarnings(a.Tool, CountFindings(out))
	}
	return Write(r.Root, s)
}

// Run does one pass immediately, then one on every tick, until ctx is done.
// It returns when ctx is cancelled, so callers can wait on it for shutdown.
func (r *Runner) Run(ctx context.Context, tick <-chan time.Time) {
	r.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			r.RunOnce(ctx)
		}
	}
}

// Start runs the runner in the background on a CheckInterval ticker and
// returns a channel closed when it has stopped (after ctx is cancelled).
func (r *Runner) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(CheckInterval)
		defer t.Stop()
		r.Run(ctx, t.C)
	}()
	return done
}
