package github

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// mergeCIVerdict is the merge-request watcher's pre-merge CI verdict (#6173).
//
// Before this gate existed the watcher issued MergePR unconditionally and could
// merge a PR whose checks failed, or that never produced a check at all. The
// production case that surfaced this: every workflow on the head SHA concluded
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
	// hive. Refuse (unless the repo is opted in via auto_merge.no_ci_ok).
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

// SetMergeRequestPolicy installs the per-repo merge-request policy sets
// (#6281 compatibility): allowUnprotectedBase is accepted as a deprecated no-op
// so existing configs keep loading; noCIOK repos have the "unverified" CI verdict
// — zero statuses, check runs, and workflow runs — downgraded to green. Keys are
// lowercase "owner/repo" and/or bare repo names, as produced by
// config.AutoMergeConfig.AllowUnprotectedBaseSet / NoCIOKSet. nil/empty clears a
// set. Safe to call repeatedly on config reload; the watcher goroutine reads
// through mergePolicyMu.
func (c *Client) SetMergeRequestPolicy(allowUnprotectedBase, noCIOK map[string]bool) {
	if c == nil {
		return
	}
	c.mergePolicyMu.Lock()
	defer c.mergePolicyMu.Unlock()
	c.allowUnprotectedBaseRepos = normalizeRepoSet(c.org, allowUnprotectedBase)
	c.noCIAllowedRepos = normalizeRepoSet(c.org, noCIOK)
}

func (c *Client) repoAllowsUnprotectedBase(owner, name string) bool {
	if c == nil {
		return false
	}
	c.mergePolicyMu.RLock()
	defer c.mergePolicyMu.RUnlock()
	return c.repoInMergePolicySet(owner+"/"+name, c.allowUnprotectedBaseRepos)
}

func (c *Client) repoNoCIOK(owner, name string) bool {
	if c == nil {
		return false
	}
	c.mergePolicyMu.RLock()
	defer c.mergePolicyMu.RUnlock()
	return c.repoInMergePolicySet(owner+"/"+name, c.noCIAllowedRepos)
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
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:merge_request_ci_gate"), owner, name, number)
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

	cfgSet, cfgKnown := c.configRequiredChecksForRepo(owner + "/" + name)
	required, requiredKnown, fromConfig, _, _ := RequiredStatusCheckContextsDetailedWithSource(ctx, c.client, owner, name, baseBranch, cfgSet, cfgKnown)
	actualRequired, actualKnown := required, requiredKnown && !fromConfig
	if actualKnown && len(required) == 0 {
		// GitHub reports both "branch not protected" and "protected but no
		// required checks" as a known empty set. For the merge-request watcher
		// that must not mean "ignore failing CI": absent required-check config
		// falls back to Hive's positive evidence gate so red/pending non-meta
		// checks still block on protected and unprotected branches alike.
		required, requiredKnown = nil, false
	}
	var expected map[string]bool
	if !requiredKnown {
		expected, _ = ExpectedCommitChecksFromLatestMergedPR(ctx, c.client, owner, name, baseBranch)
	}
	var headPushedAt time.Time
	if updatedAt := pr.GetUpdatedAt(); !updatedAt.IsZero() {
		headPushedAt = updatedAt.Time
	}
	st, err := EvaluateCommitCI(ctx, c.client, owner, name, sha, CommitCIOptions{
		Required:                required,
		RequiredKnown:           requiredKnown,
		RequiredKnownFromConfig: fromConfig,
		ExpectedChecks:          expected,
		MinHeadAge:              c.configuredAutoMergeMinHeadAge(),
		HeadPushedAt:            headPushedAt,
	})
	if err != nil {
		return mergeCIUnverified, "ci gate: " + st.Reason, fmt.Errorf("ci gate: %s for %s/%s@%s: %w", st.Reason, owner, name, shortSHA(sha), err)
	}
	if cfgKnown && actualKnown {
		c.warnRequiredChecksMismatch(owner, name, cfgSet, actualRequired, st.Observed)
	}
	if !st.Green && (strings.HasPrefix(st.Reason, "pending: head pushed ") || (strings.HasPrefix(st.Reason, "pending: ") && strings.HasSuffix(st.Reason, " has not started"))) {
		c.info("merge-request CI gate blocked fresh or incomplete head", "repo", owner+"/"+name, "pr", number, "sha", sha, "reason", st.Reason)
	}

	// Workflow runs are the evidence that survives a zero-job failure. A run
	// whose jobs exist is already represented by its check runs (which the
	// required-set / ignore-list logic above judged); a run with NO jobs is
	// opaque, and an opaque failure is a failure, an opaque in-flight run is
	// pending. Completed runs that are not failures (success, cancelled,
	// skipped) carry no verdict of their own here.
	opaque, err := c.opaqueWorkflowRuns(ctx, owner, name, sha)
	if err != nil {
		return mergeCIUnverified, "ci gate: workflow-runs", fmt.Errorf("ci gate: listing workflow runs for %s/%s@%s: %w", owner, name, shortSHA(sha), err)
	}

	switch {
	case len(opaque.failed) > 0:
		return mergeCIRed, fmt.Sprintf("ci gate: required status check has not succeeded: workflow run(s) %s concluded failure without producing a job (zero check runs)", strings.Join(opaque.failed, ", ")), nil
	case !st.Green && !commitCIReasonIsPending(st.Reason):
		return mergeCIRed, fmt.Sprintf("ci gate: required status check has not succeeded (%s)", st.Reason), nil
	case !st.Green:
		return mergeCIPending, fmt.Sprintf("ci gate: CI still running (%s)", st.Reason), nil
	case len(st.MissingRequired) > 0:
		return mergeCIPending, fmt.Sprintf("ci gate: required check(s) not yet reported on %s: %s", shortSHA(sha), strings.Join(st.MissingRequired, ", ")), nil
	case len(opaque.pending) > 0:
		return mergeCIPending, fmt.Sprintf("ci gate: workflow run(s) %s still in flight without a job yet", strings.Join(opaque.pending, ", ")), nil
	case len(opaque.actionRequired) > 0:
		return mergeCIRed, forkRunApprovalReason(owner, name, opaque.actionRequired), nil
	case st.Evidence == 0:
		// Per-repo no-CI opt-in (#6281): a repo with genuinely no CI (docs-
		// only, config-only) can never produce evidence, so "unverified"
		// would refuse it forever. Only the explicit auto_merge.no_ci_ok
		// opt-in downgrades this — and ONLY this — verdict; red and pending
		// are never downgraded, and the default stays refuse.
		if c.repoNoCIOK(owner, name) {
			return mergeCIGreen, fmt.Sprintf("ci gate: no CI evidence on %s, permitted by explicit auto_merge.no_ci_ok opt-in for %s/%s", shortSHA(sha), owner, name), nil
		}
		return mergeCIUnverified, fmt.Sprintf("ci gate: no commit statuses, check runs, or workflow runs found on %s - absent CI is not passing", shortSHA(sha)), nil
	}
	return mergeCIGreen, fmt.Sprintf("ci gate: %d status/check run(s) on %s, all gating checks succeeded", st.Evidence, shortSHA(sha)), nil
}

func (c *Client) warnRequiredChecksMismatch(owner, repo string, declared, actual, observed map[string]bool) {
	if c == nil {
		return
	}
	missing := requiredChecksMismatchNames(declared, actual, observed)
	if len(missing) == 0 {
		return
	}
	key := strings.ToLower(owner + "/" + repo)
	c.requiredChecksMu.Lock()
	if c.requiredChecksMismatchWarned == nil {
		c.requiredChecksMismatchWarned = make(map[string]bool)
	}
	if c.requiredChecksMismatchWarned[key] {
		c.requiredChecksMu.Unlock()
		return
	}
	c.requiredChecksMismatchWarned[key] = true
	c.requiredChecksMu.Unlock()
	c.warn("automerge required_checks override does not match repo protection", "repo", owner+"/"+repo, "declared", requiredChecksSortedKeys(declared), "actual", requiredChecksSortedKeys(actual), "using", "actual", "mismatch", missing)
}

func requiredChecksMismatchNames(declared, actual, observed map[string]bool) []string {
	out := make([]string, 0)
	for name := range declared {
		if !actual[name] && !observed[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func requiredChecksSortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type opaqueWorkflowRunResult struct {
	failed         []string
	actionRequired []string
	pending        []string
}

// opaqueWorkflowRuns lists the workflow runs for sha and returns those that
// failed, await fork-PR approval, or remain in flight without a job. Runs that
// produced jobs speak through their check runs and are not listed.
func (c *Client) opaqueWorkflowRuns(ctx context.Context, owner, repo, sha string) (opaqueWorkflowRunResult, error) {
	runs, _, err := c.client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, &gh.ListWorkflowRunsOptions{
		HeadSHA:     sha,
		ListOptions: gh.ListOptions{PerPage: workflowRunsPerPage},
	})
	if err != nil {
		return opaqueWorkflowRunResult{}, err
	}
	if runs == nil {
		return opaqueWorkflowRunResult{}, nil
	}
	var out opaqueWorkflowRunResult
	for _, run := range latestWorkflowRunsByWorkflowAndEvent(runs.WorkflowRuns) {
		if run == nil {
			continue
		}
		status, conclusion := run.GetStatus(), run.GetConclusion()
		isActionRequired := status == "completed" && conclusion == "action_required"
		isFailure := status == "completed" && workflowRunFailureConclusions[conclusion] && !isActionRequired
		isPending := status != "completed"
		if !isFailure && !isActionRequired && !isPending {
			continue
		}
		jobs, _, jerr := c.client.Actions.ListWorkflowJobs(ctx, owner, repo, run.GetID(), &gh.ListWorkflowJobsOptions{
			ListOptions: gh.ListOptions{PerPage: 1},
		})
		if jerr != nil {
			return opaqueWorkflowRunResult{}, jerr
		}
		if jobs != nil && jobs.GetTotalCount() > 0 {
			continue // visible through its check runs
		}
		label := fmt.Sprintf("%q(%d)", run.GetName(), run.GetID())
		switch {
		case isActionRequired:
			out.actionRequired = append(out.actionRequired, label)
		case isFailure:
			out.failed = append(out.failed, label)
		default:
			out.pending = append(out.pending, label)
		}
	}
	return out, nil
}

type workflowRunIdentity struct {
	workflowID int64
	event      string
}

func latestWorkflowRunsByWorkflowAndEvent(runs []*gh.WorkflowRun) []*gh.WorkflowRun {
	if len(runs) == 0 {
		return nil
	}
	latest := make(map[workflowRunIdentity]*gh.WorkflowRun, len(runs))
	for _, run := range runs {
		if run == nil {
			continue
		}
		key := workflowRunIdentity{workflowID: run.GetWorkflowID(), event: run.GetEvent()}
		if prev := latest[key]; prev == nil || workflowRunIsNewer(run, prev) {
			latest[key] = run
		}
	}
	out := make([]*gh.WorkflowRun, 0, len(latest))
	for _, run := range latest {
		out = append(out, run)
	}
	return out
}

func workflowRunIsNewer(candidate, current *gh.WorkflowRun) bool {
	candidateStarted := candidate.GetRunStartedAt().Time
	currentStarted := current.GetRunStartedAt().Time
	if candidateStarted.IsZero() {
		candidateStarted = candidate.GetCreatedAt().Time
	}
	if currentStarted.IsZero() {
		currentStarted = current.GetCreatedAt().Time
	}
	switch {
	case candidateStarted.After(currentStarted):
		return true
	case currentStarted.After(candidateStarted):
		return false
	default:
		return candidate.GetID() > current.GetID()
	}
}

func forkRunApprovalReason(owner, repo string, labels []string) string {
	return fmt.Sprintf("ci gate: fork PR workflow runs are awaiting maintainer approval (action_required): %s. Approve the runs or relax the repo setting at https://github.com/%s/%s/settings/actions (Approval for running fork pull request workflows)", strings.Join(labels, ", "), owner, repo)
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
