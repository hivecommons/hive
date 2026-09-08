package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// mergeCIVerdict is the merge-request watcher's pre-merge CI verdict (#6173).
//
// Before this gate existed the watcher issued MergePR unconditionally and
// relied on GitHub's branch protection to refuse a red PR. On a base branch
// with no protection rules nothing refuses it, so a PR whose checks failed,
// or that never produced a check at all, merged on request. The production
// case that surfaced this: every workflow on the head SHA concluded
// `failure` with ZERO jobs (startup failures), so the commit had zero check
// runs, an empty status rollup, and looked clean to anything that asks "is a
// check failing?". Absent is not passing. A merge now requires POSITIVE
// confirmation: at least one status/check run exists on the head SHA, every
// gating one has concluded success (neutral/skipped acceptable), no required
// check is still unreported, and no workflow run on the SHA has failed or is
// still running without having produced a job.
type mergeCIVerdict int

const (
	// mergeCIGreen: evidence present and every gating check succeeded.
	mergeCIGreen mergeCIVerdict = iota
	// mergeCIPending: CI is still reporting. Wait (do not fail the request).
	mergeCIPending
	// mergeCIRed: a gating check or an opaque workflow run failed. Refuse.
	mergeCIRed
	// mergeCIUnverified: zero statuses, zero check runs, zero workflow runs
	// on the head SHA. GitHub has no verdict at all, so neither does the
	// hive. Refuse.
	mergeCIUnverified
)

func (v mergeCIVerdict) String() string {
	switch v {
	case mergeCIGreen:
		return "green"
	case mergeCIPending:
		return "pending"
	case mergeCIRed:
		return "red"
	case mergeCIUnverified:
		return "unverified"
	}
	return fmt.Sprintf("mergeCIVerdict(%d)", int(v))
}

// mergeRequestMaxCIWaits bounds how many consecutive watcher ticks a request
// may sit in mergeCIPending before it is treated as a failed attempt. A
// pending verdict deliberately does NOT consume one of the
// mergeRequestMaxAttempts retries (CI that is merely slow is not a merge
// failure), but "required check never got created" must not park a request
// forever either. At the default 10s poll interval this is one hour.
const mergeRequestMaxCIWaits = 360

// workflowRunsPerPage is the page size for the head-SHA workflow-run listing.
// A single PR head rarely has more than a handful of runs; one page is ample.
const workflowRunsPerPage = 100

// workflowRunFailureConclusions are the workflow-run conclusions that mean
// "CI said no" even when the run produced no job (and therefore no check
// run). startup_failure is the canonical zero-job case; action_required
// (a fork PR awaiting approval) and timed_out are equally not a pass.
var workflowRunFailureConclusions = map[string]bool{
	"failure":         true,
	"timed_out":       true,
	"startup_failure": true,
	"action_required": true,
}

// verifyMergeRequestCI computes the pre-merge CI verdict for a merge request.
// repo may be "owner/repo" or a bare name (resolved against c.org, like
// MergePR). expectSHA, when set, is the SHA whose CI is judged; when empty the
// PR's current head is used. The returned reason is a human-readable
// explanation suitable for the result file and the log. A non-nil error means
// the verdict could not be computed (API failure) and the caller must treat
// that as a failed attempt, never as a pass.
func (c *Client) verifyMergeRequestCI(ctx context.Context, repo string, number int, expectSHA string) (mergeCIVerdict, string, error) {
	if c == nil || c.client == nil {
		return mergeCIUnverified, "no GitHub client", ErrNoGitHubClient
	}
	owner, name := splitRepo(repo)
	if owner == "" {
		owner = c.org
	}
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return mergeCIUnverified, "ci gate: fetching PR", fmt.Errorf("ci gate: fetching PR %s/%s#%d: %w", owner, name, number, err)
	}
	headSHA := pr.GetHead().GetSHA()
	baseBranch := pr.GetBase().GetRef()
	sha := strings.TrimSpace(expectSHA)
	if sha == "" {
		sha = headSHA
	}
	if sha == "" {
		return mergeCIUnverified, "ci gate: PR has no head SHA", fmt.Errorf("ci gate: PR %s/%s#%d has no head SHA", owner, name, number)
	}
	if headSHA != "" && !strings.EqualFold(sha, headSHA) {
		// The commit the governor judged eligible is no longer the PR head.
		// MergePR's pinned-SHA merge would 409 anyway; refusing here means the
		// CI of the WRONG commit is never what authorizes a merge.
		return mergeCIRed, fmt.Sprintf("ci gate: head moved: request pinned %s but PR head is %s", shortSHA(sha), shortSHA(headSHA)), nil
	}

	cfgSet, cfgKnown := c.configRequiredChecks()
	required, requiredKnown := RequiredStatusCheckContexts(ctx, c.client, owner, name, baseBranch, cfgSet, cfgKnown)
	st, err := EvaluateCommitCI(ctx, c.client, owner, name, sha, required, requiredKnown)
	if err != nil {
		return mergeCIUnverified, "ci gate: " + st.Reason, fmt.Errorf("ci gate: %s for %s/%s@%s: %w", st.Reason, owner, name, shortSHA(sha), err)
	}

	// Workflow runs are the evidence that survives a zero-job failure. A run
	// whose jobs exist is already represented by its check runs (which the
	// required-set / ignore-list logic above judged); a run with NO jobs is
	// opaque, and an opaque failure is a failure, an opaque in-flight run is
	// pending. Completed runs that are not failures (success, cancelled,
	// skipped) carry no verdict of their own here.
	opaqueFailed, opaquePending, err := c.opaqueWorkflowRuns(ctx, owner, name, sha)
	if err != nil {
		return mergeCIUnverified, "ci gate: workflow-runs", fmt.Errorf("ci gate: listing workflow runs for %s/%s@%s: %w", owner, name, shortSHA(sha), err)
	}

	switch {
	case len(opaqueFailed) > 0:
		return mergeCIRed, fmt.Sprintf("ci gate: required status check has not succeeded: workflow run(s) %s concluded failure without producing a job (zero check runs)", strings.Join(opaqueFailed, ", ")), nil
	case !st.Green && !strings.HasSuffix(st.Reason, "-pending"):
		return mergeCIRed, fmt.Sprintf("ci gate: required status check has not succeeded (%s)", st.Reason), nil
	case !st.Green:
		return mergeCIPending, fmt.Sprintf("ci gate: CI still running (%s)", st.Reason), nil
	case len(st.MissingRequired) > 0:
		return mergeCIPending, fmt.Sprintf("ci gate: required check(s) not yet reported on %s: %s", shortSHA(sha), strings.Join(st.MissingRequired, ", ")), nil
	case len(opaquePending) > 0:
		return mergeCIPending, fmt.Sprintf("ci gate: workflow run(s) %s still in flight without a job yet", strings.Join(opaquePending, ", ")), nil
	case st.Evidence == 0:
		return mergeCIUnverified, fmt.Sprintf("ci gate: no commit statuses, check runs, or workflow runs found on %s - absent CI is not passing", shortSHA(sha)), nil
	}
	return mergeCIGreen, fmt.Sprintf("ci gate: %d status/check run(s) on %s, all gating checks succeeded", st.Evidence, shortSHA(sha)), nil
}

// opaqueWorkflowRuns lists the workflow runs for sha and returns the names of
// those that FAILED without a job and those still IN FLIGHT without a job.
// Runs that produced jobs speak through their check runs and are not listed.
func (c *Client) opaqueWorkflowRuns(ctx context.Context, owner, repo, sha string) (failed, pending []string, err error) {
	runs, _, err := c.client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, &gh.ListWorkflowRunsOptions{
		HeadSHA:     sha,
		ListOptions: gh.ListOptions{PerPage: workflowRunsPerPage},
	})
	if err != nil {
		return nil, nil, err
	}
	if runs == nil {
		return nil, nil, nil
	}
	for _, run := range runs.WorkflowRuns {
		if run == nil {
			continue
		}
		status, conclusion := run.GetStatus(), run.GetConclusion()
		isFailure := status == "completed" && workflowRunFailureConclusions[conclusion]
		isPending := status != "completed"
		if !isFailure && !isPending {
			continue
		}
		jobs, _, jerr := c.client.Actions.ListWorkflowJobs(ctx, owner, repo, run.GetID(), &gh.ListWorkflowJobsOptions{
			ListOptions: gh.ListOptions{PerPage: 1},
		})
		if jerr != nil {
			return nil, nil, jerr
		}
		if jobs != nil && jobs.GetTotalCount() > 0 {
			continue // visible through its check runs
		}
		label := fmt.Sprintf("%q(%d)", run.GetName(), run.GetID())
		if isFailure {
			failed = append(failed, label)
		} else {
			pending = append(pending, label)
		}
	}
	return failed, pending, nil
}

// logCIVerdict records the gate's decision for the operator with the same
// fields the rest of the watcher uses.
func (c *Client) logCIVerdict(req MergeRequest, verdict mergeCIVerdict, reason string) {
	attrs := []any{
		slog.String("repo", req.Repo), slog.Int("number", req.Number),
		slog.String("agent", req.Agent), slog.String("verdict", verdict.String()),
		slog.String("reason", reason),
	}
	switch verdict {
	case mergeCIGreen:
		c.logger.Info("merge-request watcher: CI positively confirmed, proceeding to merge", attrs...)
	case mergeCIPending:
		c.logger.Info("merge-request watcher: CI not yet confirmed, waiting", attrs...)
	default:
		c.logger.Warn("merge-request watcher: REFUSED to merge, CI not confirmed", attrs...)
	}
}

// shortSHAPrefixLen is the abbreviated commit length used in gate messages.
const shortSHAPrefixLen = 12

func shortSHA(sha string) string {
	if len(sha) <= shortSHAPrefixLen {
		return sha
	}
	return sha[:shortSHAPrefixLen]
}
