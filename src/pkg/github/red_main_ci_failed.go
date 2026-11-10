package github

import (
	"context"
	"sort"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// The ci.failed CEL trigger source (celtrigger.KindCIFailed). SweepRedMain
// already reads every active repo's default branch tip each pass; when a
// trigger rule names ci.failed it hands the tip's red check runs to
// observeCIFailed, which reports each check that concluded failure or
// timed_out, once per (repo, check, head SHA). There is no extra polling
// loop and, with no such rule, no extra call.

// ciFailedSeenMax bounds the dedupe map. It holds one entry per (repo,
// check), so only a fleet with thousands of distinct check names reaches
// it; it is then cleared, which may re-report a still-red SHA once.
const ciFailedSeenMax = 5000

// CIFailedRun is one check that concluded failure or timed_out as the newest
// completed run of that name on a repository's default branch tip, from a
// check suite created for the default branch itself (a pull request's run on
// the same commit is not one).
type CIFailedRun struct {
	// Repo is the repository as configured (project.repos spelling), the
	// same spelling issue and PR events carry.
	Repo string
	// Branch is the default branch.
	Branch string
	// Check is the check run name.
	Check string
	// HeadSHA is the commit the run concluded on.
	HeadSHA string
	// Conclusion is "failure" or "timed_out".
	Conclusion string
	// URL is the check run's page.
	URL string
}

// ciFailedState is the ci.failed hook and its dedupe map, kept in
// redMainState and guarded by Client.redMainMu.
type ciFailedState struct {
	want func() bool
	emit func(context.Context, CIFailedRun)
	// seen maps ciFailedSeenKey(repo, check) to the ciFailedSeenRun (head
	// SHA and check suite) last decided.
	// In memory: a restart may report a still-red SHA once more.
	seen map[string]string
}

// SetCIFailedHook installs the ci.failed trigger source. want reports
// whether any trigger rule names ci.failed; when it returns false (or
// either function is nil) the red-main sweep makes no ci.failed reads and
// reports nothing. emit is called synchronously from the sweep, without
// the client's locks held, once per newly failed (repo, check, head SHA).
func (c *Client) SetCIFailedHook(want func() bool, emit func(context.Context, CIFailedRun)) {
	if c == nil {
		return
	}
	c.redMainMu.Lock()
	defer c.redMainMu.Unlock()
	c.redMain.ciFailed.want = want
	c.redMain.ciFailed.emit = emit
}

// ciFailedEmitter returns the emit hook when a rule wants ci.failed, else nil.
func (c *Client) ciFailedEmitter() func(context.Context, CIFailedRun) {
	if c == nil {
		return nil
	}
	c.redMainMu.Lock()
	want, emit := c.redMain.ciFailed.want, c.redMain.ciFailed.emit
	c.redMainMu.Unlock()
	if want == nil || emit == nil || !want() {
		return nil
	}
	return emit
}

// ciFailedConclusion reports the conclusions ci.failed fires on. cancelled,
// action_required, stale, skipped and neutral do not: they are a superseded
// or waiting run, not a broken branch.
func ciFailedConclusion(conclusion string) bool {
	return conclusion == "failure" || conclusion == "timed_out"
}

func ciFailedSeenKey(repoKey, check string) string {
	return strings.ToLower(repoKey) + "\x00" + check
}

// ciFailedSeenRun identifies a decided run by head SHA and check suite, so a
// default-branch run of the check on a SHA whose pull request run was
// skipped is still reported.
func ciFailedSeenRun(cr *gh.CheckRun) string {
	return cr.GetHeadSHA() + "\x00" + strconv.FormatInt(cr.GetCheckSuite().GetID(), 10)
}

// observeCIFailed reports, through emit, each run in runs (redCheckRunsForRef
// on the default branch tip) that concluded failure or timed_out on a head SHA
// not yet reported for that check. A run is reported only when its check
// suite's head branch is the default branch, which leaves out a pull
// request's run on the same commit. A failed check-suite read reports
// nothing and records nothing, so the next pass retries.
func (c *Client) observeCIFailed(ctx context.Context, emit func(context.Context, CIFailedRun), repo, owner, name, branch string, runs map[string]*gh.CheckRun) {
	if c == nil || emit == nil || len(runs) == 0 {
		return
	}
	key := c.redMainRepoKey(repo)
	checks := make([]string, 0, len(runs))
	for check := range runs {
		checks = append(checks, check)
	}
	sort.Strings(checks)

	var fresh []string
	c.redMainMu.Lock()
	for _, check := range checks {
		cr := runs[check]
		if cr == nil || !ciFailedConclusion(cr.GetConclusion()) || cr.GetHeadSHA() == "" {
			continue
		}
		if c.redMain.ciFailed.seen[ciFailedSeenKey(key, check)] == ciFailedSeenRun(cr) {
			continue
		}
		fresh = append(fresh, check)
	}
	c.redMainMu.Unlock()
	if len(fresh) == 0 {
		return
	}

	suiteBranches := map[string]map[int64]string{}
	for _, check := range fresh {
		cr := runs[check]
		sha := cr.GetHeadSHA()
		branches, ok := suiteBranches[sha]
		if !ok {
			var err error
			branches, err = c.ciFailedSuiteBranches(ctx, owner, name, sha)
			if err != nil {
				c.logger.Debug("ci.failed: check suites unavailable; retrying next pass", "repo", key, "sha", sha, "error", err)
				return
			}
			suiteBranches[sha] = branches
		}
		// Either way the run on sha is decided, so it is recorded: a pull
		// request's failing run left on the tip must not re-read the check
		// suites on every pass.
		c.redMainMu.Lock()
		if c.redMain.ciFailed.seen == nil || len(c.redMain.ciFailed.seen) >= ciFailedSeenMax {
			c.redMain.ciFailed.seen = map[string]string{}
		}
		c.redMain.ciFailed.seen[ciFailedSeenKey(key, check)] = ciFailedSeenRun(cr)
		c.redMainMu.Unlock()
		if branches[cr.GetCheckSuite().GetID()] != branch {
			c.logger.Debug("ci.failed: failing run is not a default-branch run; skipping", "repo", key, "check", check, "sha", sha)
			continue
		}
		emit(ctx, CIFailedRun{
			Repo:       repo,
			Branch:     branch,
			Check:      check,
			HeadSHA:    sha,
			Conclusion: cr.GetConclusion(),
			URL:        cr.GetHTMLURL(),
		})
	}
}

// ciFailedSuiteBranches maps each check suite on sha to its head branch.
func (c *Client) ciFailedSuiteBranches(ctx context.Context, owner, repo, sha string) (map[int64]string, error) {
	out := map[int64]string{}
	opts := &gh.ListCheckSuiteOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		res, resp, err := c.client.Checks.ListCheckSuitesForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			return nil, err
		}
		if res != nil {
			for _, suite := range res.CheckSuites {
				if suite != nil && suite.GetID() != 0 {
					out[suite.GetID()] = suite.GetHeadBranch()
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}
