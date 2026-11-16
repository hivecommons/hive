package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Multi-repo governor issues (#18448). Every repo in project.repos gets its own
// pinned advisory ("governor") issue, not just the primary. The primary keeps
// its long-standing path (boot ensure, ensurePinnedAdvisoryIssue, the
// hub-facing digest error and the App banner). The other repos are handled
// here, re-ensured every eval cycle while unresolved, so a repo added later in
// Settings gets its issue on the next cycle without a restart. Creation stays
// idempotent through EnsureAdvisoryIssue, which reuses an existing issue (open,
// or closed and reopened) found by title and label before it creates one.

const (
	// advisoryRepoIssueAlertPrefix namespaces the per-repo dashboard alerts
	// raised when a managed repo cannot hold a governor issue.
	advisoryRepoIssueAlertPrefix = "advisory-repo-issue:"
	// advisoryRepoIssueAlertSeverity is the dashboard severity of those
	// alerts: the rest of the hive keeps working, so it is a warning.
	advisoryRepoIssueAlertSeverity = "warning"
	// advisoryRepoIssueRefusedBackoff is how long a repo whose ensure was
	// refused for a reason retrying cannot fix (no write access, Issues
	// disabled, repo moved or archived) waits before the next attempt, so a
	// refused repo costs a few calls an hour rather than a few per cycle.
	advisoryRepoIssueRefusedBackoff = time.Hour
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

// advisoryRepoIssueBackoff remembers refused repos across eval cycles.
type advisoryRepoIssueBackoff struct {
	mu      sync.Mutex
	refused map[string]advisoryRepoIssueRefusal
}

func newAdvisoryRepoIssueBackoff() *advisoryRepoIssueBackoff {
	return &advisoryRepoIssueBackoff{refused: map[string]advisoryRepoIssueRefusal{}}
}

// advisoryRepoIssueGate is the process-wide backoff the eval cycle uses.
var advisoryRepoIssueGate = newAdvisoryRepoIssueBackoff()

func (b *advisoryRepoIssueBackoff) get(repo string) (advisoryRepoIssueRefusal, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.refused[repo]
	return r, ok
}

func (b *advisoryRepoIssueBackoff) set(repo string, r advisoryRepoIssueRefusal) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refused[repo] = r
}

func (b *advisoryRepoIssueBackoff) clear(repo string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.refused, repo)
}

// advisorySecondaryRepos returns the managed repos other than the primary,
// whose governor issues this file owns.
func advisorySecondaryRepos(cfg *config.Config) []string {
	repos := advisory.ManagedRepos(cfg)
	if len(repos) <= 1 {
		return nil
	}
	return repos[1:]
}

// advisoryRepoIssueRefusalMessage reports whether err is a refusal retrying
// cannot fix and, if so, the operator-facing alert text naming repo and the
// remedy. A rate limit, 5xx or network error returns ok=false: the next cycle
// simply retries.
func advisoryRepoIssueRefusalMessage(repo string, err error) (string, bool) {
	var disabled *github.IssuesDisabledError
	if errors.As(err, &disabled) {
		return fmt.Sprintf("No governor issue in %s: %s.", repo, disabled.Error()), true
	}
	var moved *github.RepoMovedError
	if errors.As(err, &moved) {
		return fmt.Sprintf("No governor issue in %s: %s.", repo, moved.Error()), true
	}
	switch classifyAdvisoryPostError(err) {
	case advisoryPostWriteForbidden:
		return fmt.Sprintf("No governor issue in %s: the hive's GitHub App cannot create issues there. Add the repository to the App installation and grant Issues read and write; the hive retries automatically.", repo), true
	case advisoryPostRepoArchived:
		return fmt.Sprintf("No governor issue in %s: the repository is archived and read-only.", repo), true
	}
	return "", false
}

// ensureSecondaryAdvisoryIssues resolves (creating when missing) the governor
// issue of every non-primary managed repo that has none yet, recording each
// number in advisoryIssues under the repo's configured spelling.
//
// One repo's failure never stops the others. A refusal retrying cannot fix is
// logged, raised as a per-repo dashboard alert and backed off for
// advisoryRepoIssueRefusedBackoff; a transient failure is logged and retried
// next cycle. Alerts for repos that resolved or left project.repos are cleared.
// A paused repo is skipped quietly: the pause is the operator's choice.
//
// The returned map holds this cycle's ensure error per repo (nil-free), for
// tests and logging; callers need not act on it.
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
	if ensure == nil || advisoryIssues == nil {
		return errs
	}
	for _, repo := range repos {
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
			logger.Info("governor issue ready", "repo", repo, "number", num)
			continue
		}
		if err == nil {
			err = fmt.Errorf("governor issue for %s did not resolve to an issue number", repo)
		}
		errs[repo] = err
		var paused *github.RepoPausedError
		if errors.As(err, &paused) {
			gate.set(repo, advisoryRepoIssueRefusal{retryAt: now.Add(advisoryRepoIssueRefusedBackoff)})
			logger.Info("governor issue not ensured: repo paused", "repo", repo)
			continue
		}
		if msg, refused := advisoryRepoIssueRefusalMessage(repo, err); refused {
			gate.set(repo, advisoryRepoIssueRefusal{retryAt: now.Add(advisoryRepoIssueRefusedBackoff), message: msg})
			keep[advisoryRepoIssueAlertPrefix+repo] = struct{}{}
			if alerts != nil {
				alerts.AddSystemAlert(advisoryRepoIssueAlertPrefix+repo, advisoryRepoIssueAlertSeverity, msg)
			}
			logger.Warn("governor issue refused for repo; retrying later",
				"repo", repo, "retry_after", advisoryRepoIssueRefusedBackoff.String(), "error", err)
			continue
		}
		// Transient: keep any alert a refusal raised until a real answer.
		if r, ok := gate.get(repo); ok && r.message != "" {
			keep[advisoryRepoIssueAlertPrefix+repo] = struct{}{}
		}
		logger.Warn("governor issue still unresolved for repo; retrying next cycle", "repo", repo, "error", err)
	}
	return errs
}

// advisorySecondaryPostDeps are the effects postAdvisoryDigestToSecondaryRepos
// performs; the eval cycle wires them to the GitHub client and post gate.
type advisorySecondaryPostDeps struct {
	post          func(ctx context.Context, repo string, issueNum int, md string) error
	due           func(repo string) bool
	recordSuccess func(repo string)
	// recordForbidden pauses further post attempts for repo after a
	// non-transient 403.
	recordForbidden func(repo string)
}

// postAdvisoryDigestToSecondaryRepos writes the rendered digest to the
// governor issue of every non-primary repo that has one, paced per repo by the
// same post gate as the primary. Failures are logged per repo and never touch
// the primary's hub-facing digest error or the App banner, which describe the
// primary repo. It returns the repos posted to.
func postAdvisoryDigestToSecondaryRepos(
	ctx context.Context,
	md string,
	advisoryIssues map[string]int,
	repos []string,
	deps advisorySecondaryPostDeps,
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
