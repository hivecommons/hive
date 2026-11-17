package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Opt-in per-repo advisory reports (#18448). By default a hive keeps exactly
// one Hive Advisory Report, in the primary repo (#18568). When the owner turns
// on governor.advisory.per_repo, every other repo in project.repos also gets
// one report, re-ensured each eval cycle while unresolved so a repo added in
// Settings gets its report without a restart.
//
// Unlike the primary report, a secondary report a human closed is left
// closed: EnsureSecondaryAdvisoryIssue finds it (title plus label or marker,
// any state) and reports it closed instead of reopening or replacing it, and
// the post path stops writing to a report that is closed mid-run.
//
// When per_repo is switched off nothing here runs: the existing secondary
// reports are neither updated nor closed.

const (
	// advisoryRepoIssueAlertPrefix namespaces the per-repo dashboard alerts
	// raised when a managed repo cannot hold an advisory report.
	advisoryRepoIssueAlertPrefix = "advisory-repo-issue:"
	// advisoryRepoIssueAlertSeverity is the dashboard severity of those
	// alerts: the rest of the hive keeps working, so it is a warning.
	advisoryRepoIssueAlertSeverity = "warning"
	// advisoryRepoIssueRefusedBackoff is how long a repo whose ensure was
	// refused for a reason retrying cannot fix (no write access, Issues
	// disabled, repo moved or archived) waits before the next attempt, so a
	// refused repo costs at most one lookup an hour, never one per cycle.
	advisoryRepoIssueRefusedBackoff = time.Hour
	// advisorySecondaryOpenCheckInterval paces the "is this report still
	// open?" check before a secondary digest post.
	advisorySecondaryOpenCheckInterval = time.Hour
)

// advisoryRepoIssueAlerts is the dashboard surface for per-repo ensure
// refusals; *dashboard.Server satisfies it.
type advisoryRepoIssueAlerts interface {
	AddSystemAlert(id, severity, message string)
	ClearSystemAlertsExcept(prefix string, keep map[string]struct{})
}

// advisoryRepoIssueRefusal is one repo's most recent non-transient ensure
// refusal: when the next attempt is allowed, and the operator-facing message.
type advisoryRepoIssueRefusal struct {
	retryAt time.Time
	message string
}

// advisoryRepoIssueBackoff remembers, across eval cycles, which secondary
// repos were refused (and already logged), which reports a human closed, and
// when each report was last confirmed open.
type advisoryRepoIssueBackoff struct {
	mu          sync.Mutex
	refused     map[string]advisoryRepoIssueRefusal
	logged      map[string]string // repo -> refusal message already logged
	closed      map[string]int    // repo -> human-closed report number
	openChecked map[string]time.Time
}

func newAdvisoryRepoIssueBackoff() *advisoryRepoIssueBackoff {
	return &advisoryRepoIssueBackoff{
		refused:     map[string]advisoryRepoIssueRefusal{},
		logged:      map[string]string{},
		closed:      map[string]int{},
		openChecked: map[string]time.Time{},
	}
}

// advisoryRepoIssueGate is the process-wide state the eval cycle uses.
var advisoryRepoIssueGate = newAdvisoryRepoIssueBackoff()

func (b *advisoryRepoIssueBackoff) get(repo string) (advisoryRepoIssueRefusal, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.refused[repo]
	return r, ok
}

// refuse records a refusal and reports whether this message is new for repo,
// i.e. whether it should be logged at warning level.
func (b *advisoryRepoIssueBackoff) refuse(repo string, r advisoryRepoIssueRefusal) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refused[repo] = r
	if b.logged[repo] == r.message {
		return false
	}
	b.logged[repo] = r.message
	return true
}

func (b *advisoryRepoIssueBackoff) clear(repo string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.refused, repo)
	delete(b.logged, repo)
}

func (b *advisoryRepoIssueBackoff) markClosed(repo string, num int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed[repo] = num
	delete(b.refused, repo)
	delete(b.logged, repo)
}

func (b *advisoryRepoIssueBackoff) isClosed(repo string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.closed[repo]
	return ok
}

// openCheckDue reports whether repo's report should be re-checked for a
// human close, and if so stamps now as the check time.
func (b *advisoryRepoIssueBackoff) openCheckDue(repo string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if last, ok := b.openChecked[repo]; ok && now.Sub(last) < advisorySecondaryOpenCheckInterval {
		return false
	}
	b.openChecked[repo] = now
	return true
}

// advisorySecondaryRepos returns the managed repos other than the primary, in
// configured order. An entry naming the primary or an earlier entry, bare
// ("talks") or org-qualified ("org/talks") and in any case, is dropped so one
// repository never gets two reports. The spelling kept is the first seen,
// because advisoryIssues and the post gate are keyed by that exact string.
func advisorySecondaryRepos(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	org := strings.TrimSpace(cfg.Project.Org)
	key := func(repo string) string {
		if !strings.Contains(repo, "/") && org != "" {
			repo = org + "/" + repo
		}
		return strings.ToLower(repo)
	}
	primary := strings.TrimSpace(advisory.PrimaryRepo(cfg))
	seen := map[string]struct{}{}
	if primary != "" {
		seen[key(primary)] = struct{}{}
	}
	var out []string
	for _, r := range cfg.Project.Repos {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		k := key(r)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}

// advisoryPerRepoSecondaryRepos is the set the eval cycle ensures and posts
// to: the secondary repos when governor.advisory.per_repo is on, else none.
func advisoryPerRepoSecondaryRepos(cfg *config.Config) []string {
	if cfg == nil || !cfg.Governor.Advisory.PerRepoEnabled() {
		return nil
	}
	return advisorySecondaryRepos(cfg)
}

// advisoryRepoIssueRefusalMessage reports whether err is a refusal retrying
// cannot fix and, if so, the operator-facing alert text naming repo and the
// remedy. A rate limit, 5xx or network error returns ok=false: the next cycle
// simply retries.
func advisoryRepoIssueRefusalMessage(repo string, err error) (string, bool) {
	var disabled *github.IssuesDisabledError
	if errors.As(err, &disabled) {
		return fmt.Sprintf("No advisory report in %s: %s.", repo, disabled.Error()), true
	}
	var moved *github.RepoMovedError
	if errors.As(err, &moved) {
		return fmt.Sprintf("No advisory report in %s: %s.", repo, moved.Error()), true
	}
	switch classifyAdvisoryPostError(err) {
	case advisoryPostWriteForbidden:
		return fmt.Sprintf("No advisory report in %s: the hive's GitHub App cannot create issues there. Add the repository to the App installation and grant Issues read and write, or turn off \"Advisory report in every repo\".", repo), true
	case advisoryPostRepoArchived:
		return fmt.Sprintf("No advisory report in %s: the repository is archived and read-only.", repo), true
	}
	return "", false
}

// ensureSecondaryAdvisoryIssues resolves (creating when missing) the advisory
// report of every repo in repos that has none yet, recording each number in
// advisoryIssues under the repo's configured spelling. repos is empty unless
// governor.advisory.per_repo is on; with it empty, only stale alerts are
// cleared.
//
// One repo's failure never stops the others. A report found closed is
// remembered and the repo is skipped for the rest of the run. A refusal
// retrying cannot fix is logged once, raised as a per-repo dashboard alert and
// backed off for advisoryRepoIssueRefusedBackoff; a transient failure is
// logged and retried next cycle. A paused repo is skipped quietly.
//
// The returned map holds this cycle's ensure error per repo, for tests and
// logging; callers need not act on it.
func ensureSecondaryAdvisoryIssues(
	ctx context.Context,
	advisoryIssues map[string]int,
	repos []string,
	ensure func(ctx context.Context, repo string) (int, error),
	gate *advisoryRepoIssueBackoff,
	alerts advisoryRepoIssueAlerts,
	now time.Time,
	logger *slog.Logger,
) map[string]error {
	if logger == nil {
		logger = slog.Default()
	}
	errs := map[string]error{}
	keep := map[string]struct{}{}
	defer func() {
		if alerts != nil {
			alerts.ClearSystemAlertsExcept(advisoryRepoIssueAlertPrefix, keep)
		}
	}()
	if ensure == nil || advisoryIssues == nil || gate == nil {
		return errs
	}
	for _, repo := range repos {
		if gate.isClosed(repo) {
			continue
		}
		if !advisoryIssueUnresolved(advisoryIssues, repo) {
			gate.clear(repo)
			continue
		}
		if r, ok := gate.get(repo); ok && now.Before(r.retryAt) {
			if r.message != "" {
				keep[advisoryRepoIssueAlertPrefix+repo] = struct{}{}
			}
			continue
		}
		num, err := ensure(ctx, repo)
		if err == nil && num > 0 {
			advisoryIssues[repo] = num
			gate.clear(repo)
			logger.Info("per-repo advisory report ready", "repo", repo, "number", num)
			continue
		}
		if err == nil {
			err = fmt.Errorf("advisory report for %s did not resolve to an issue number", repo)
		}
		errs[repo] = err
		var closed *github.AdvisoryReportClosedError
		if errors.As(err, &closed) {
			gate.markClosed(repo, closed.Number)
			logger.Info("per-repo advisory report was closed by a person; leaving it closed and not posting there",
				"repo", repo, "number", closed.Number)
			continue
		}
		var paused *github.RepoPausedError
		if errors.As(err, &paused) {
			gate.refuse(repo, advisoryRepoIssueRefusal{retryAt: now.Add(advisoryRepoIssueRefusedBackoff)})
			logger.Debug("per-repo advisory report not ensured: repo paused", "repo", repo)
			continue
		}
		if msg, refused := advisoryRepoIssueRefusalMessage(repo, err); refused {
			first := gate.refuse(repo, advisoryRepoIssueRefusal{retryAt: now.Add(advisoryRepoIssueRefusedBackoff), message: msg})
			keep[advisoryRepoIssueAlertPrefix+repo] = struct{}{}
			if alerts != nil {
				alerts.AddSystemAlert(advisoryRepoIssueAlertPrefix+repo, advisoryRepoIssueAlertSeverity, msg)
			}
			if first {
				logger.Warn("per-repo advisory report skipped for repo",
					"repo", repo, "recheck_after", advisoryRepoIssueRefusedBackoff.String(), "error", err)
			} else {
				logger.Debug("per-repo advisory report still refused for repo", "repo", repo, "error", err)
			}
			continue
		}
		// Transient: keep any alert a refusal raised until a real answer.
		if r, ok := gate.get(repo); ok && r.message != "" {
			keep[advisoryRepoIssueAlertPrefix+repo] = struct{}{}
		}
		logger.Warn("per-repo advisory report still unresolved; retrying next cycle", "repo", repo, "error", err)
	}
	return errs
}

// advisorySecondaryPostDeps are the effects postAdvisoryDigestToSecondaryRepos
// performs; the eval cycle wires them to the GitHub client and post gate.
type advisorySecondaryPostDeps struct {
	post func(ctx context.Context, repo string, issueNum int, md string) error
	// isOpen reports whether the report is still open; nil skips the check.
	isOpen        func(ctx context.Context, repo string, issueNum int) (bool, error)
	due           func(repo string) bool
	recordSuccess func(repo string)
	// recordForbidden pauses further post attempts for repo after a
	// non-transient 403.
	recordForbidden func(repo string)
}

// postAdvisoryDigestToSecondaryRepos writes the rendered digest to the
// advisory report of every repo in repos that has one, paced per repo by the
// same post gate as the primary. A report found closed is dropped from
// advisoryIssues and never posted to again this run. Failures are logged per
// repo and never touch the primary's hub-facing digest error or the App
// banner. It returns the repos posted to.
func postAdvisoryDigestToSecondaryRepos(
	ctx context.Context,
	md string,
	advisoryIssues map[string]int,
	repos []string,
	gate *advisoryRepoIssueBackoff,
	deps advisorySecondaryPostDeps,
	now time.Time,
	logger *slog.Logger,
) []string {
	if md == "" || deps.post == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	var posted []string
	for _, repo := range repos {
		if gate != nil && gate.isClosed(repo) {
			continue
		}
		num, ok := advisoryIssueNumber(advisoryIssues, repo)
		if !ok {
			continue
		}
		if _, archived := archivedAdvisoryRepos.Load(repo); archived {
			continue
		}
		if deps.due != nil && !deps.due(repo) {
			continue
		}
		if deps.isOpen != nil && gate != nil && gate.openCheckDue(repo, now) {
			open, err := deps.isOpen(ctx, repo, num)
			if err != nil {
				logger.Debug("could not confirm per-repo advisory report is open; posting anyway",
					"repo", repo, "issue", num, "error", err)
			} else if !open {
				gate.markClosed(repo, num)
				delete(advisoryIssues, repo)
				logger.Info("per-repo advisory report was closed by a person; leaving it closed and not posting there",
					"repo", repo, "number", num)
				continue
			}
		}
		err := deps.post(ctx, repo, num, md)
		var paused *github.RepoPausedError
		switch {
		case err == nil:
			posted = append(posted, repo)
			if deps.recordSuccess != nil {
				deps.recordSuccess(repo)
			}
			logger.Info("posted advisory digest", "repo", repo, "issue", num, "via", "app")
		case errors.As(err, &paused):
			if deps.recordSuccess != nil {
				deps.recordSuccess(repo)
			}
		case classifyAdvisoryPostError(err) == advisoryPostRepoArchived:
			archivedAdvisoryRepos.Store(repo, struct{}{})
			logger.Warn("advisory repo is archived and read-only, skipping digest posts", "repo", repo)
		case classifyAdvisoryPostError(err) == advisoryPostWriteForbidden:
			if deps.recordForbidden != nil {
				deps.recordForbidden(repo)
			}
			logger.Warn("advisory digest write forbidden for repo", "repo", repo, "issue", num, "error", err)
		default:
			logger.Warn("failed to post advisory digest to repo", "repo", repo, "issue", num, "error", err)
		}
	}
	return posted
}
