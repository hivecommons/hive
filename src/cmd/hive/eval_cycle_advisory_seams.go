package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
)

// digestSnapshotDeps are the GitHub reads pinDigestSnapshot needs to pin a
// digest to one commit. Same shape as the other eval-cycle seams (#7232): the
// decisions live here, the network lives in the caller. A nil latestCommit
// means "no GitHub client" and disables pinning entirely.
type digestSnapshotDeps struct {
	defaultBranch func(ctx context.Context, owner, repo string) (string, error)
	latestCommit  func(ctx context.Context, owner, repo, branch string) (string, error)
	pathExists    func(ctx context.Context, owner, repo, path, ref string) (bool, error)
	issueClosedAt func(ctx context.Context, owner, repo string, number int) (time.Time, bool, error)
}

// pinDigestSnapshot resolves the analyzed repo's latest commit on branch
// (or its default branch when branch is empty) and, on success, installs the
// Snapshot, VerifyPath and ResolveRef hooks on opts (#3704, #2364, #6080).
//
// Every failure is best-effort: an unresolvable branch or SHA leaves opts
// untouched so the digest still builds unpinned, and the installed hooks
// treat an inconclusive lookup as "still present" / "still open" — a
// transient error must never cost a real finding its top-N slot or retire it.
//
// It reports whether a snapshot was pinned so the caller can log once.
func pinDigestSnapshot(
	ctx context.Context,
	opts *advisory.DigestOptions,
	org, repoName, primaryRepo, branch string,
	deps digestSnapshotDeps,
	logger *slog.Logger,
) bool {
	if opts == nil || deps.latestCommit == nil || org == "" || repoName == "" {
		return false
	}
	if branch == "" && deps.defaultBranch != nil {
		b, err := deps.defaultBranch(ctx, org, repoName)
		if err != nil {
			logger.Warn("advisory: could not resolve default branch for snapshot", "repo", primaryRepo, "error", err)
		} else {
			branch = b
		}
	}
	if branch == "" {
		return false
	}
	sha, err := deps.latestCommit(ctx, org, repoName, branch)
	if err != nil {
		logger.Warn("advisory: could not resolve latest commit for snapshot", "repo", primaryRepo, "branch", branch, "error", err)
		return false
	}
	if sha == "" {
		return false
	}
	opts.Snapshot = &advisory.Snapshot{Owner: org, Repo: repoName, Branch: branch, SHA: sha}
	if deps.pathExists != nil {
		opts.VerifyPath = func(path string) bool {
			exists, verr := deps.pathExists(ctx, org, repoName, path, sha)
			if verr != nil {
				// Inconclusive (network/rate-limit, not a 404): treat as
				// existing so a transient error never mislabels a real path
				// as outdated.
				logger.Warn("advisory: path existence check failed", "path", path, "repo", primaryRepo, "sha", sha, "error", verr)
				return true
			}
			return exists
		}
	}
	if deps.issueClosedAt != nil {
		opts.ResolveRef = func(refOwner, refRepo string, number int) (advisory.RefState, bool) {
			closedAt, closed, rerr := deps.issueClosedAt(ctx, refOwner, refRepo, number)
			if rerr != nil {
				// Inconclusive: "cannot tell", so the finding stays open.
				logger.Warn("advisory: issue state lookup failed",
					"ref", fmt.Sprintf("%s/%s#%d", refOwner, refRepo, number), "error", rerr)
				return advisory.RefState{}, false
			}
			return advisory.RefState{Closed: closed, ClosedAt: closedAt}, true
		}
	}
	logger.Info("advisory digest pinned to commit", "repo", primaryRepo, "branch", branch, "sha", sha)
	return true
}

// advisoryPublishRoute is everything publishAdvisoryDigest needs to know about
// WHERE this cycle's digest goes, resolved by the caller before the call.
type advisoryPublishRoute struct {
	target      string // config.AdvisoryTargetGitHub or config.AdvisoryTargetLinear
	linearIssue string
	routeErr    error // non-nil: the non-GitHub route is misconfigured

	primaryRepo    string
	issueNum       int
	hasPinnedIssue bool
	ensureErr      error // this cycle's pinned-issue ensure failure, if any
}

// advisoryPublishDeps are the effects publishAdvisoryDigest may perform. Only
// postGitHub is required; every other hook is optional and skipped when nil,
// which is what lets the seam be driven with a handful of closures.
type advisoryPublishDeps struct {
	postGitHub func(ctx context.Context, repo string, issueNum int, md string) error
	postLinear func(ctx context.Context, linearIssue, md string) error

	// recordError marks the digest stale on the dashboard with a cause.
	recordError func(msg string)
	// recordPosted records a successful write: findings count, overflow, and
	// the per-repo success timestamp the pacing gate reads.
	recordPosted func(findings, overflow int)

	// onWriteForbidden runs when the App is installed but a real write was
	// refused; onAuthFailure runs for every other classified App failure.
	// onWriteProven runs after a successful App write — it clears the App
	// banner and retires healed access findings.
	onWriteForbidden func(ctx context.Context)
	onAuthFailure    func(ctx context.Context)
	onWriteProven    func(ctx context.Context)
}

// advisoryPublishOutcome names how a cycle's digest publish ended, so the
// caller and tests can assert the decision without reading dashboard state.
type advisoryPublishOutcome int

const (
	// advisoryPublishSkipped: nothing rendered, nothing written.
	advisoryPublishSkipped advisoryPublishOutcome = iota
	// advisoryPublishPosted: the comment was written to its route.
	advisoryPublishPosted
	// advisoryPublishFailed: a write was attempted or required and did not
	// happen; recordError has been called with the cause.
	advisoryPublishFailed
)

func (o advisoryPublishOutcome) String() string {
	switch o {
	case advisoryPublishSkipped:
		return "skipped"
	case advisoryPublishPosted:
		return "posted"
	case advisoryPublishFailed:
		return "failed"
	}
	return fmt.Sprintf("advisoryPublishOutcome(%d)", int(o))
}

// publishAdvisoryDigest writes an already-rendered digest comment to its
// configured route and records the outcome (#1927, #2353, #2575, #4167).
//
// Routing rules, in order:
//   - md == "" → skipped.
//   - Non-GitHub target with routeErr → failed; the operator opted out of the
//     GitHub issue, so a misconfiguration is recorded, never redirected.
//   - Linear target → post there; failure recorded.
//   - GitHub target with a pinned issue → the App is the sole writer. On
//     failure the error is classified: write-forbidden and auth failures call
//     their hooks so the caller can raise the App banner honestly; a rate
//     limit only logs. On success onWriteProven runs.
//   - GitHub target with no pinned issue but something to say → failed, with
//     the ensure cause in the recorded message (#4167).
func publishAdvisoryDigest(
	ctx context.Context,
	md string,
	digest *advisory.Digest,
	route advisoryPublishRoute,
	deps advisoryPublishDeps,
	logger *slog.Logger,
) advisoryPublishOutcome {
	if md == "" || digest == nil {
		return advisoryPublishSkipped
	}
	recordError := deps.recordError
	if recordError == nil {
		recordError = func(string) {}
	}
	recordPosted := deps.recordPosted
	if recordPosted == nil {
		recordPosted = func(int, int) {}
	}

	if route.target != config.AdvisoryTargetGitHub {
		if route.routeErr != nil {
			recordError(route.routeErr.Error())
			logger.Error("advisory digest not posted: target misconfigured",
				"target", route.target, "error", route.routeErr)
			return advisoryPublishFailed
		}
		if deps.postLinear == nil {
			recordError("advisory digest target " + route.target + " has no poster wired")
			return advisoryPublishFailed
		}
		if err := deps.postLinear(ctx, route.linearIssue, md); err != nil {
			recordError(err.Error())
			logger.Warn("failed to post advisory digest to linear", "issue", route.linearIssue, "error", err)
			return advisoryPublishFailed
		}
		logger.Info("posted advisory digest", "linear_issue", route.linearIssue, "findings", digest.TotalCount, "via", "linear")
		recordPosted(digest.TotalCount, digest.OverflowCount)
		return advisoryPublishPosted
	}

	if !route.hasPinnedIssue {
		// Something to publish, nowhere to put it. Recording this as a post
		// FAILURE is what lets the hub flag a wedged digest instead of reading
		// the hive as "not an advisory participant" (#4167).
		recordError(advisoryIssueMissingError(route.primaryRepo, route.ensureErr))
		logger.Warn("advisory digest not posted: no pinned advisory issue",
			"repo", route.primaryRepo, "findings", digest.TotalCount)
		return advisoryPublishFailed
	}

	if deps.postGitHub == nil {
		recordError(advisoryIssueMissingError(route.primaryRepo, route.ensureErr))
		return advisoryPublishFailed
	}
	err := deps.postGitHub(ctx, route.primaryRepo, route.issueNum, md)
	if err == nil {
		logger.Info("posted advisory digest", "repo", route.primaryRepo, "issue", route.issueNum, "findings", digest.TotalCount, "via", "app")
		recordPosted(digest.TotalCount, digest.OverflowCount)
		if deps.onWriteProven != nil {
			deps.onWriteProven(ctx)
		}
		return advisoryPublishPosted
	}

	// err.Error() is the same string logged below — log-safe, never key material.
	recordError(err.Error())
	logger.Warn("failed to post advisory digest via app", "repo", route.primaryRepo, "issue", route.issueNum, "error", err)
	switch classifyAdvisoryPostError(err) {
	case advisoryPostWriteForbidden:
		if deps.onWriteForbidden != nil {
			deps.onWriteForbidden(ctx)
		}
	case advisoryPostRateLimited:
		logger.Warn("GitHub API rate limit hit, skipping advisory digest post", "repo", route.primaryRepo)
	default:
		if deps.onAuthFailure != nil {
			deps.onAuthFailure(ctx)
		}
	}
	return advisoryPublishFailed
}

// summarizeDigestForLog renders the severity/agent breakdown logged once per
// digest post. Pure; kept separate so the format is pinned by a test.
func summarizeDigestForLog(digest *advisory.Digest) (bySeverity map[string]int, agents string) {
	bySeverity = map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0, "info": 0}
	agentNames := make([]string, 0, len(digest.ByAgent))
	for agentName, findings := range digest.ByAgent {
		agentNames = append(agentNames, fmt.Sprintf("%s(%d)", agentName, len(findings)))
		for _, f := range findings {
			bySeverity[strings.ToLower(f.Severity)]++
		}
	}
	sort.Strings(agentNames)
	return bySeverity, strings.Join(agentNames, ", ")
}
