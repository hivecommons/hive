package github

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	// DefaultSharedCIRerunMaxPRs caps how many PRs one sweep pass re-runs.
	DefaultSharedCIRerunMaxPRs = 10
	// DefaultSharedCIRerunLookback is how long after a [shared-ci] incident
	// closes the sweep keeps looking for PRs stamped with its marker.
	DefaultSharedCIRerunLookback = 72 * time.Hour

	// sharedCIRerunForbiddenTTL is how long a refused rerun suppresses further
	// rerun attempts (and the warning) for that repo.
	sharedCIRerunForbiddenTTL = time.Hour

	sharedCIIncidentTitlePrefix = "[shared-ci]"
	sharedCIRerunMaxIssuePages  = 3
)

// sharedCIRerunMarkerRE matches the marker the rerun sweep stamps on the PR
// after re-running its failed jobs for incident <n>. It is the durable
// once-per-PR-per-incident record; sharedCIMarkerRE does not match it.
var sharedCIRerunMarkerRE = regexp.MustCompile(`<!-- hive-shared-ci-rerun-([1-9][0-9]*) -->`)

func sharedCIRerunMarker(incident int) string {
	return fmt.Sprintf("<!-- hive-shared-ci-rerun-%d -->", incident)
}

// SharedCIRerunOptions bounds one SweepSharedCIReruns pass.
type SharedCIRerunOptions struct {
	// MaxPRs caps PRs re-run per pass; non-positive means DefaultSharedCIRerunMaxPRs.
	MaxPRs int
	// Lookback is how far back a closed incident still triggers re-runs;
	// non-positive means DefaultSharedCIRerunLookback.
	Lookback time.Duration
	Audit    func(SharedCIRerunEvent)
	// Now overrides the clock in tests.
	Now func() time.Time
}

// SharedCIRerunEvent records one PR whose failed jobs were re-run.
type SharedCIRerunEvent struct {
	Repo      string
	Number    int
	HeadSHA   string
	Incidents []int
	RunIDs    []int64
}

// SharedCIRerunResult summarizes one pass.
type SharedCIRerunResult struct {
	Incidents int
	Seen      int
	Rerun     []SharedCIRerunEvent
	Skipped   int
}

// sharedCIRerunSeen memoizes a PR that needed no action, so later passes skip
// re-listing its comments until the PR or the closed-incident set changes.
type sharedCIRerunSeen struct {
	updatedAt time.Time
	incidents string
}

// SweepSharedCIReruns re-runs the failed jobs on open PRs that a fix lane
// deferred to a [shared-ci] incident (`<!-- hive-shared-ci-<n> -->`) once that
// incident is closed, so a PR left red by the broken baseline gets a fresh
// verdict without a human pushing or clicking re-run (hivecommons/hive#12253).
// It pushes nothing to the branch, so it applies to every author. Each PR is
// re-run at most once per incident: the `<!-- hive-shared-ci-rerun-<n> -->`
// comment it leaves afterwards is the dedupe record.
func (c *Client) SweepSharedCIReruns(ctx context.Context, opts SharedCIRerunOptions) (*SharedCIRerunResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	maxPRs := opts.MaxPRs
	if maxPRs <= 0 {
		maxPRs = DefaultSharedCIRerunMaxPRs
	}
	lookback := opts.Lookback
	if lookback <= 0 {
		lookback = DefaultSharedCIRerunLookback
	}
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	ctx = WithRESTCaller(ctx, "hive:shared_ci_rerun")
	result := &SharedCIRerunResult{}

	for _, repo := range c.getRepos() {
		if len(result.Rerun) >= maxPRs {
			break
		}
		owner, repoName := c.splitRepo(repo)
		closed, err := c.recentlyClosedSharedCIIncidents(ctx, owner, repoName, now.Add(-lookback))
		if err != nil {
			return result, err
		}
		if len(closed) == 0 {
			continue
		}
		result.Incidents += len(closed)
		incidentKey := sharedCIIncidentSetKey(closed)
		prs, err := c.listOpenPRsForSupersessionSweep(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		for _, pr := range prs {
			if len(result.Rerun) >= maxPRs {
				break
			}
			if pr == nil {
				continue
			}
			result.Seen++
			event, ok, err := c.trySharedCIRerun(ctx, repo, owner, repoName, pr, closed, incidentKey)
			if err != nil {
				return result, err
			}
			if !ok {
				result.Skipped++
				continue
			}
			result.Rerun = append(result.Rerun, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

// recentlyClosedSharedCIIncidents returns the [shared-ci] incident issues in
// owner/repo closed at or after since. Incidents closed as not planned are
// excluded, matching .github/scripts/pr-auto-update.sh: nothing was fixed.
func (c *Client) recentlyClosedSharedCIIncidents(ctx context.Context, owner, repo string, since time.Time) (map[int]bool, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "closed",
		Sort:        "updated",
		Direction:   "desc",
		Since:       since,
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	out := map[int]bool{}
	for page := 0; page < sharedCIRerunMaxIssuePages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing closed shared-CI incidents in %s/%s: %w", owner, repo, err)
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() {
				continue
			}
			if !strings.HasPrefix(strings.TrimSpace(issue.GetTitle()), sharedCIIncidentTitlePrefix) {
				continue
			}
			if strings.EqualFold(issue.GetStateReason(), "not_planned") {
				continue
			}
			if issue.ClosedAt == nil || issue.GetClosedAt().Time.Before(since) {
				continue
			}
			out[issue.GetNumber()] = true
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

func sharedCIIncidentSetKey(incidents map[int]bool) string {
	nums := make([]int, 0, len(incidents))
	for n := range incidents {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// pendingSharedCIReruns returns the closed incidents a PR's comments (oldest
// first) defer it to that have not been re-run yet. A PR whose newest marker
// names an incident outside closed is still deferred elsewhere and gets none.
func pendingSharedCIReruns(texts []string, closed map[int]bool) []int {
	newest := newestSharedCIMarker(texts)
	if newest == 0 || !closed[newest] {
		return nil
	}
	stamped := map[int]bool{}
	done := map[int]bool{}
	for _, text := range texts {
		for _, m := range sharedCIMarkerRE.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				stamped[n] = true
			}
		}
		for _, m := range sharedCIRerunMarkerRE.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				done[n] = true
			}
		}
	}
	var out []int
	for n := range stamped {
		if closed[n] && !done[n] {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func (c *Client) trySharedCIRerun(ctx context.Context, repo, owner, repoName string, pr *gh.PullRequest, closed map[int]bool, incidentKey string) (SharedCIRerunEvent, bool, error) {
	if labels := labelNames(pr.Labels); c.isHeld(labels) || c.isExempt(labels) {
		return SharedCIRerunEvent{}, false, nil
	}
	seenKey := fmt.Sprintf("%s#%d", repo, pr.GetNumber())
	updatedAt := pr.GetUpdatedAt().Time
	c.sharedCIRerunMu.Lock()
	prev, known := c.sharedCIRerunSeen[seenKey]
	c.sharedCIRerunMu.Unlock()
	if known && prev.incidents == incidentKey && prev.updatedAt.Equal(updatedAt) {
		return SharedCIRerunEvent{}, false, nil
	}
	markSeen := func() {
		c.sharedCIRerunMu.Lock()
		if c.sharedCIRerunSeen == nil {
			c.sharedCIRerunSeen = map[string]sharedCIRerunSeen{}
		}
		c.sharedCIRerunSeen[seenKey] = sharedCIRerunSeen{updatedAt: updatedAt, incidents: incidentKey}
		c.sharedCIRerunMu.Unlock()
	}

	comments, err := c.listIssueComments(ctx, owner, repoName, pr.GetNumber())
	if err != nil {
		return SharedCIRerunEvent{}, false, err
	}
	texts := make([]string, 0, len(comments))
	for _, cm := range comments {
		texts = append(texts, cm.GetBody())
	}
	incidents := pendingSharedCIReruns(texts, closed)
	if len(incidents) == 0 {
		markSeen()
		return SharedCIRerunEvent{}, false, nil
	}

	sha := pr.GetHead().GetSHA()
	runs, err := c.latestFailedWorkflowRuns(ctx, owner, repoName, sha)
	if err != nil {
		return SharedCIRerunEvent{}, false, err
	}
	if len(runs) == 0 {
		// Nothing red to re-run (already green, re-run by hand, or still
		// pending). No comment: the memo keeps this PR quiet until it changes.
		markSeen()
		return SharedCIRerunEvent{}, false, nil
	}

	event := SharedCIRerunEvent{Repo: repo, Number: pr.GetNumber(), HeadSHA: sha, Incidents: incidents}
	var rerun []*gh.WorkflowRun
	for _, run := range runs {
		if c.sharedCIRerunRefused(repo) {
			break
		}
		if _, err := c.client.Actions.RerunFailedJobsByID(ctx, owner, repoName, run.GetID()); err != nil {
			if isWriteForbiddenError(err) {
				// The App installation lacks the Actions write permission for
				// rerun-failed-jobs: a permanent gap, so stop this pass and
				// warn once per repo per TTL instead of once per run.
				if c.markSharedCIRerunForbidden(repo) {
					c.logger.Warn("shared-ci rerun forbidden: the App installation lacks Actions write permission for rerun-failed-jobs", "repo", repo, "pr", pr.GetNumber(), "run", run.GetID(), "error", err)
				}
				break
			}
			c.logger.Warn("shared-ci rerun failed", "repo", repo, "pr", pr.GetNumber(), "run", run.GetID(), "error", err)
			continue
		}
		rerun = append(rerun, run)
		event.RunIDs = append(event.RunIDs, run.GetID())
	}
	if len(rerun) == 0 {
		// Every re-run was refused (permissions, run too old to re-run).
		// Memoize so the sweep does not retry each pass for this PR state.
		markSeen()
		return SharedCIRerunEvent{}, false, nil
	}

	body := renderSharedCIRerunComment(incidents, sha, rerun)
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, pr.GetNumber(), &gh.IssueComment{Body: &body}); err != nil {
		return event, false, fmt.Errorf("stamping shared-CI rerun marker on %s#%d: %w", repo, pr.GetNumber(), err)
	}
	for _, incident := range incidents {
		c.logger.Info("shared-ci rerun", "repo", repo, "pr", pr.GetNumber(), "incident", "#"+strconv.Itoa(incident), "head", sha, "runs", len(event.RunIDs))
	}
	return event, true, nil
}

// sharedCIRerunRefused reports whether a rerun was refused for repo within the TTL.
func (c *Client) sharedCIRerunRefused(repo string) bool {
	c.sharedCIRerunMu.Lock()
	defer c.sharedCIRerunMu.Unlock()
	at, ok := c.sharedCIRerunForbidden[repo]
	return ok && time.Since(at) < sharedCIRerunForbiddenTTL
}

// markSharedCIRerunForbidden records a refusal and reports whether it is the
// first one in the current TTL window (so the caller logs it once).
func (c *Client) markSharedCIRerunForbidden(repo string) bool {
	c.sharedCIRerunMu.Lock()
	defer c.sharedCIRerunMu.Unlock()
	if at, ok := c.sharedCIRerunForbidden[repo]; ok && time.Since(at) < sharedCIRerunForbiddenTTL {
		return false
	}
	if c.sharedCIRerunForbidden == nil {
		c.sharedCIRerunForbidden = map[string]time.Time{}
	}
	c.sharedCIRerunForbidden[repo] = time.Now()
	return true
}

// latestFailedWorkflowRuns returns, per workflow, the newest run for sha when
// that run completed as failure or timed_out.
func (c *Client) latestFailedWorkflowRuns(ctx context.Context, owner, repo, sha string) ([]*gh.WorkflowRun, error) {
	if sha == "" {
		return nil, nil
	}
	opts := &gh.ListWorkflowRunsOptions{HeadSHA: sha, ListOptions: gh.ListOptions{PerPage: 100}}
	latest := map[int64]*gh.WorkflowRun{}
	for page := 0; page < claimSearchMaxPages; page++ {
		runs, resp, err := c.client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing workflow runs for %s/%s@%s: %w", owner, repo, sha, err)
		}
		if runs != nil {
			for _, run := range runs.WorkflowRuns {
				if run == nil || run.GetHeadSHA() != sha {
					continue
				}
				cur, ok := latest[run.GetWorkflowID()]
				if !ok || run.GetRunNumber() > cur.GetRunNumber() ||
					(run.GetRunNumber() == cur.GetRunNumber() && run.GetRunAttempt() > cur.GetRunAttempt()) {
					latest[run.GetWorkflowID()] = run
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	var out []*gh.WorkflowRun
	for _, run := range latest {
		if run.GetStatus() != "completed" {
			continue
		}
		switch run.GetConclusion() {
		case "failure", "timed_out":
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetID() < out[j].GetID() })
	return out, nil
}

func renderSharedCIRerunComment(incidents []int, sha string, runs []*gh.WorkflowRun) string {
	var b strings.Builder
	refs := make([]string, len(incidents))
	for i, n := range incidents {
		fmt.Fprintln(&b, sharedCIRerunMarker(n))
		refs[i] = "#" + strconv.Itoa(n)
	}
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	fmt.Fprintf(&b, "Shared-CI incident %s is closed, so Hive re-ran the failed jobs on `%s`:\n\n", strings.Join(refs, ", "), short)
	for _, run := range runs {
		name := run.GetName()
		if name == "" {
			name = fmt.Sprintf("run %d", run.GetID())
		}
		if url := run.GetHTMLURL(); url != "" {
			fmt.Fprintf(&b, "- [%s](%s)\n", name, url)
		} else {
			fmt.Fprintf(&b, "- %s\n", name)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_Hive re-runs a PR at most once per incident; the new results are this PR's verdict._")
	return b.String()
}
