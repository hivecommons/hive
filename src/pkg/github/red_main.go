package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/redmain"
)

// Red-main circuit breaker, forge side (hivecommons/hive#13630, RFC #13617
// P3). The rules live in pkg/redmain; this file feeds them from GitHub and
// applies them:
//
//   - SweepRedMain reads each active repo's default branch tip: the required
//     checks (config, classic protection and rulesets, ReadBranchRules) that
//     concluded red, the run URL, the files the failing run's annotations
//     name, and the open `[shared-ci] <check> failing across <owner/repo>`
//     tracking issue. The tracker dates each check's red from its first
//     sighting since it was last green; a check whose tip run has not
//     completed keeps its previous state, so a push does not reset the clock.
//   - StampRedMain turns the tracker into per-repo facts on the actionable
//     snapshot, with the open PRs that fix main (the fixes-main label, a
//     closing reference to the tracking issue, or changed files overlapping
//     the failing check; passing PR checks only confirm).
//   - The review queue ranks those PRs first, the PR-request relay refuses
//     NEW PRs in the repo unless they fix main (redMainPRRequestRefusal,
//     which reads a named issue live when the sweep has not seen it), and
//     kicks and the repo card name the check and the fix PR.
//
// Every lookup failure leaves the repo's previous observation in place:
// a read error never trips the breaker and never clears it either.

// redMainMaxIssuePages bounds the open-issue listing used to find the
// `[shared-ci]` tracking issue (newest first, 100 per page).
const redMainMaxIssuePages = 3

// redMainMaxLiveIssueLookups bounds the issues a refused PR request names
// that the refusal path reads live to find a just-filed tracking issue.
const redMainMaxLiveIssueLookups = 3

// redMainMaxAnnotatedPaths bounds the failing-file list kept per check.
const redMainMaxAnnotatedPaths = 50

// redMainState is the client's breaker state. Guarded by Client.redMainMu.
type redMainState struct {
	tracker   *redmain.Tracker
	threshold func(repo string) time.Duration
	// fixes are the fix PRs StampRedMain last detected, per repo key, so the
	// PR-request refusal can name the fix PR.
	fixes map[string][]redmain.FixPR
	// paths caches the annotated files of a failing run by run ID; a run's
	// annotations never change once it has concluded.
	paths map[int64][]string
}

// SetRedMainThresholdFunc installs the per-repo threshold resolver
// (config.RepoRedMainThreshold). nil means redmain.DefaultThreshold.
func (c *Client) SetRedMainThresholdFunc(fn func(repo string) time.Duration) {
	if c == nil {
		return
	}
	c.redMainMu.Lock()
	defer c.redMainMu.Unlock()
	c.redMain.threshold = fn
}

func (c *Client) redMainTracker() *redmain.Tracker {
	c.redMainMu.Lock()
	defer c.redMainMu.Unlock()
	if c.redMain.tracker == nil {
		c.redMain.tracker = redmain.NewTracker()
	}
	return c.redMain.tracker
}

func (c *Client) redMainThreshold(repo string) time.Duration {
	c.redMainMu.Lock()
	fn := c.redMain.threshold
	c.redMainMu.Unlock()
	if fn == nil {
		return redmain.DefaultThreshold
	}
	return fn(repo)
}

// redMainRepoKey is the "owner/repo" form the breaker keys repos by.
func (c *Client) redMainRepoKey(repo string) string {
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	if owner == "" {
		return name
	}
	return owner + "/" + name
}

// ObserveRedMain records one look at repo's default branch (see
// redmain.Tracker.Observe). SweepRedMain is the production caller.
func (c *Client) ObserveRedMain(repo, branch string, red map[string]redmain.RedRun, now time.Time) {
	if c == nil {
		return
	}
	c.redMainTracker().Observe(c.redMainRepoKey(repo), branch, red, now)
}

// RedMainFact returns repo's red-main fact at now, with the fix PRs the
// last StampRedMain detected, or nil when the breaker is not tripped.
func (c *Client) RedMainFact(repo string, now time.Time) *redmain.Fact {
	if c == nil {
		return nil
	}
	key := c.redMainRepoKey(repo)
	fact := c.redMainTracker().Active(key, c.redMainThreshold(key), now)
	if fact == nil {
		return nil
	}
	c.redMainMu.Lock()
	fact.FixPRs = append([]redmain.FixPR(nil), c.redMain.fixes[strings.ToLower(key)]...)
	c.redMainMu.Unlock()
	return fact
}

// RedMainSweepResult summarizes one SweepRedMain pass.
type RedMainSweepResult struct {
	Repos   int
	Skipped int
	// Red counts repos with at least one required check red on the tip.
	Red int
}

// SweepRedMain observes every active repo's default branch tip. A repo
// whose branch, required-check set or check runs cannot be read is skipped
// and keeps its previous observation.
func (c *Client) SweepRedMain(ctx context.Context, now time.Time) (RedMainSweepResult, error) {
	var res RedMainSweepResult
	if c == nil || c.client == nil {
		return res, ErrNoGitHubClient
	}
	ctx = WithRESTCaller(ctx, "hive:red_main")
	for _, repo := range c.activeRepos() {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Repos++
		key := c.redMainRepoKey(repo)
		owner, name := c.splitRepo(key)
		branch, err := c.DefaultBranch(ctx, owner, name)
		if err != nil {
			c.logger.Debug("red-main: default branch unavailable; keeping previous observation", "repo", key, "error", err)
			res.Skipped++
			continue
		}
		cfgSet, cfgKnown := c.configRequiredChecksForRepo(key)
		rules := ReadBranchRules(ctx, c.client, owner, name, branch, cfgSet, cfgKnown)
		if !rules.Known {
			c.logger.Debug("red-main: required checks unknown; keeping previous observation", "repo", key, "reason", rules.Reason)
			res.Skipped++
			continue
		}
		runs, settled, err := c.redCheckRunsForRef(ctx, owner, name, branch)
		if err != nil {
			c.logger.Debug("red-main: default branch check runs unavailable; keeping previous observation", "repo", key, "error", err)
			res.Skipped++
			continue
		}
		// A required check with no completed run on the tip yet (a push
		// whose CI is still running) is pending: it neither clears nor
		// re-dates a red entry.
		pending := map[string]bool{}
		for check, required := range rules.Required {
			if required && !settled[check] {
				pending[check] = true
			}
		}
		tracker := c.redMainTracker()
		red := map[string]redmain.RedRun{}
		for check, cr := range runs {
			if !rules.Required[check] {
				continue
			}
			run := redmain.RedRun{URL: cr.GetHTMLURL(), Paths: c.redMainAnnotatedPaths(ctx, owner, name, cr.GetID())}
			if cr.CompletedAt != nil {
				run.CompletedAt = cr.GetCompletedAt().Time
			}
			red[check] = run
		}
		if len(red) > 0 {
			res.Red++
			if tracking, err := c.openSharedCITrackingIssues(ctx, owner, name); err == nil {
				for check, run := range red {
					run.TrackingIssue = tracking[strings.ToLower(check)]
					red[check] = run
				}
			} else {
				c.logger.Debug("red-main: tracking issue lookup failed; keeping previous tracking issues", "repo", key, "error", err)
				for check, run := range red {
					run.TrackingIssue = tracker.TrackingIssue(key, check)
					red[check] = run
				}
			}
		}
		tracker.ObservePending(key, branch, red, pending, now)
	}
	return res, nil
}

// redCheckRunsForRef returns the latest red check run per name on ref
// (meta checks excluded) — redCheckNamesForRef with the runs kept — and the
// names whose latest runs have all completed (red or not). A name missing
// from settled has a run still queued or in progress.
func (c *Client) redCheckRunsForRef(ctx context.Context, owner, repo, ref string) (map[string]*gh.CheckRun, map[string]bool, error) {
	var all []*gh.CheckRun
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, opts)
		if err != nil {
			return nil, nil, err
		}
		all = append(all, runs.CheckRuns...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	red := map[string]*gh.CheckRun{}
	settled := map[string]bool{}
	running := map[string]bool{}
	for _, cr := range latestCheckRunsByNameAndApp(all) {
		name := cr.GetName()
		if name == "" || isMetaCheck(name) {
			continue
		}
		if cr.GetStatus() != "completed" {
			running[name] = true
			continue
		}
		settled[name] = true
		if checkConclusionIsRed(cr.GetConclusion()) {
			red[name] = cr
		}
	}
	for name := range running {
		delete(settled, name)
	}
	return red, settled, nil
}

// redMainAnnotatedPaths returns the files a failing run's annotations name,
// cached per run. A failed read yields nil and is retried next pass.
func (c *Client) redMainAnnotatedPaths(ctx context.Context, owner, repo string, runID int64) []string {
	if runID == 0 {
		return nil
	}
	c.redMainMu.Lock()
	cached, ok := c.redMain.paths[runID]
	c.redMainMu.Unlock()
	if ok {
		return cached
	}
	anns, _, err := c.client.Checks.ListCheckRunAnnotations(ctx, owner, repo, runID, &gh.ListOptions{PerPage: redMainMaxAnnotatedPaths})
	if err != nil {
		return nil
	}
	var paths []string
	seen := map[string]bool{}
	for _, a := range anns {
		p := strings.TrimSpace(a.GetPath())
		if p == "" || p == ".github" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	c.redMainMu.Lock()
	if c.redMain.paths == nil || len(c.redMain.paths) > 1000 {
		c.redMain.paths = map[int64][]string{}
	}
	c.redMain.paths[runID] = paths
	c.redMainMu.Unlock()
	return paths
}

// openSharedCITrackingIssues maps a lower-cased check name to the open
// `[shared-ci] <check> failing across <owner/repo>` issue tracking it in
// owner/repo (the lowest number when several are open).
func (c *Client) openSharedCITrackingIssues(ctx context.Context, owner, repo string) (map[string]int, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Sort:        "created",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	out := map[string]int{}
	for page := 0; page < redMainMaxIssuePages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open shared-CI incidents in %s/%s: %w", owner, repo, err)
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() {
				continue
			}
			check, ok := SharedCITrackingCheck(issue.GetTitle(), owner+"/"+repo)
			if !ok {
				continue
			}
			if n, seen := out[check]; !seen || issue.GetNumber() < n {
				out[check] = issue.GetNumber()
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

// SharedCITrackingCheck parses a `[shared-ci] <check> failing across
// <owner/repo>` incident title and returns the lower-cased check name when
// the title is one and names fullRepo (or no repo at all).
func SharedCITrackingCheck(title, fullRepo string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(title), sharedCIIncidentTitlePrefix)
	if !ok {
		return "", false
	}
	check, across, found := strings.Cut(strings.TrimSpace(rest), " failing across ")
	if !found {
		check, found = strings.CutSuffix(strings.TrimSpace(rest), " failing")
		if !found {
			return "", false
		}
	} else if across = strings.TrimSpace(across); across != "" && !strings.EqualFold(across, fullRepo) {
		return "", false
	}
	check = strings.ToLower(strings.TrimSpace(check))
	return check, check != ""
}

// RedMainFact is the red-main fact as surfaced to callers that read it
// through this package (the dashboard repo card) without importing
// pkg/redmain themselves.
type RedMainFact = redmain.Fact

// RedMainFactFor finds repo's fact in a snapshot's RedMain map, matching a
// bare repo name against an "owner/repo" key.
func RedMainFactFor(facts map[string]*redmain.Fact, repo string) *redmain.Fact {
	if len(facts) == 0 {
		return nil
	}
	if f, ok := facts[repo]; ok {
		return f
	}
	for key, f := range facts {
		if repoRefsMatch(key, repo) {
			return f
		}
	}
	return nil
}

// RedMainPRInput is the redmain.PRInput view of a PullRequest. paths are
// its changed files, nil when unknown.
func RedMainPRInput(pr PullRequest, paths []string) redmain.PRInput {
	in := redmain.PRInput{
		Labels:        pr.Labels,
		Paths:         paths,
		CIStatus:      pr.CIStatus,
		FailingChecks: pr.FailingChecks,
	}
	for _, li := range pr.LinkedIssues {
		if li.Via == "" || li.Via == PRLinkViaFixes {
			in.ClosingIssues = append(in.ClosingIssues, li.Number)
		}
	}
	for _, li := range pr.ReferencedIssues {
		if li.Via == PRLinkViaFixes {
			in.ClosingIssues = append(in.ClosingIssues, li.Number)
		}
	}
	return in
}

// RedMainFixPR reports whether pr fixes main per fact, and how.
func RedMainFixPR(fact *redmain.Fact, pr PullRequest, paths []string) (redmain.FixPR, bool) {
	in := RedMainPRInput(pr, paths)
	via, ok := redmain.FixesMain(fact, in)
	if !ok {
		return redmain.FixPR{}, false
	}
	return redmain.FixPR{
		Number:    pr.Number,
		URL:       pr.URL,
		Via:       via,
		Confirmed: redmain.Confirmed(fact, in),
		WaitingOn: redMainWaitingOn(pr),
	}, true
}

// redMainWaitingOn names who a fix PR is waiting on, for the repo card.
func redMainWaitingOn(pr PullRequest) string {
	switch {
	case pr.Draft:
		return "@" + pr.Author + " (draft)"
	case pr.CIStatus == "failure":
		return "@" + pr.Author + " (CI red)"
	case pr.CIStatus != "success":
		return "CI"
	}
	var who []string
	for _, r := range pr.RequestedReviewers {
		who = append(who, "@"+r)
	}
	for _, t := range pr.RequestedTeams {
		who = append(who, "@"+t)
	}
	if len(who) > 0 {
		return strings.Join(who, ", ")
	}
	return "review"
}

// DetectRedMainFixPRs returns the PRs in prs (same repo as fact) that fix
// main, best first. changedPaths may be nil; it must not call GitHub.
func DetectRedMainFixPRs(fact *redmain.Fact, prs []PullRequest, changedPaths func(fullRepo string, number int, headSHA string) []string) []redmain.FixPR {
	if fact == nil {
		return nil
	}
	var out []redmain.FixPR
	seen := map[int]bool{}
	for _, pr := range prs {
		if pr.Number <= 0 || seen[pr.Number] || !repoRefsMatch(pr.Repo, fact.Repo) {
			continue
		}
		seen[pr.Number] = true
		var paths []string
		if changedPaths != nil {
			paths = changedPaths(fact.Repo, pr.Number, pr.HeadSHA)
		}
		if fix, ok := RedMainFixPR(fact, pr, paths); ok {
			out = append(out, fix)
		}
	}
	redmain.SortFixPRs(out)
	return out
}

// StampRedMain sets actionable.RedMain to the red-main fact of every active
// repo whose breaker is tripped at now, with the fix PRs among the
// snapshot's open PRs, and remembers those fix PRs for the PR-request
// refusal. A repo whose main went green drops out, which is what lifts the
// pause and the fix-first ordering.
func (c *Client) StampRedMain(actionable *ActionableResult, changedPaths func(fullRepo string, number int, headSHA string) []string, now time.Time) {
	if c == nil || actionable == nil {
		return
	}
	prs := make([]PullRequest, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	prs = append(prs, actionable.PRs.Items...)
	prs = append(prs, actionable.PRs.Held...)
	facts := map[string]*redmain.Fact{}
	fixes := map[string][]redmain.FixPR{}
	for _, repo := range c.activeRepos() {
		key := c.redMainRepoKey(repo)
		fact := c.redMainTracker().Active(key, c.redMainThreshold(key), now)
		if fact == nil {
			continue
		}
		fact.FixPRs = DetectRedMainFixPRs(fact, prs, changedPaths)
		facts[key] = fact
		fixes[strings.ToLower(key)] = fact.FixPRs
	}
	c.redMainMu.Lock()
	c.redMain.fixes = fixes
	c.redMainMu.Unlock()
	if len(facts) == 0 {
		actionable.RedMain = nil
		return
	}
	actionable.RedMain = facts
}

// redMainPRRequestRefusal returns the reason a PR-open request is refused
// while repo's main is red, or "" when the breaker is not tripped or the
// request itself fixes main: it names the red-main tracking issue in
// --issues or closes it in its title/body. When the request names an issue
// the last sweep did not know as the tracking issue — the agent just filed
// it, as PauseReason tells it to — the issue is read live
// (redMainLiveTrackingIssue) before refusing. Existing PRs never pass
// through here, and issue filing is a different relay, so neither is
// affected.
func (c *Client) redMainPRRequestRefusal(ctx context.Context, req PRRequest, now time.Time) string {
	fact := c.RedMainFact(req.Repo, now)
	if fact == nil {
		return ""
	}
	named := redMainRequestIssues(req, fact.Repo)
	for _, n := range named {
		if fact.TrackingIssue > 0 && n == fact.TrackingIssue {
			return ""
		}
	}
	if c.redMainLiveTrackingIssue(ctx, fact, named) > 0 {
		return ""
	}
	return fact.PauseReason()
}

// redMainRequestIssues returns the same-repo issues a PR request names in
// --issues or closes in its title/body, in order, without duplicates.
func redMainRequestIssues(req PRRequest, repo string) []int {
	var out []int
	seen := map[int]bool{}
	add := func(n int) {
		if n > 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range req.IssueN {
		add(n)
	}
	for _, ref := range ParseClaimedIssues(req.Title+"\n"+req.Body, repo) {
		if ref.Repo == "" || strings.EqualFold(ref.Repo, repo) {
			add(ref.Issue)
		}
	}
	return out
}

// redMainLiveTrackingIssue reads up to redMainMaxLiveIssueLookups of the
// issues a refused request names and returns the first that is an open
// `[shared-ci] <fact.Check> failing across <repo>` issue, 0 when none is.
// A match is recorded in the tracker (when no tracking issue is known) so
// later requests and fix-PR detection see it before the next sweep. Only
// the refusal path calls it, and only for named issues.
func (c *Client) redMainLiveTrackingIssue(ctx context.Context, fact *redmain.Fact, issues []int) int {
	if c.client == nil || fact == nil || len(issues) == 0 {
		return 0
	}
	ctx = WithRESTCaller(ctx, "hive:red_main")
	owner, name := c.splitRepo(fact.Repo)
	fullRepo := owner + "/" + name
	want := strings.ToLower(strings.TrimSpace(fact.Check))
	for i, n := range issues {
		if i >= redMainMaxLiveIssueLookups {
			break
		}
		issue, _, err := c.client.Issues.Get(ctx, owner, name, n)
		if err != nil {
			c.logger.Debug("red-main: named issue unavailable", "repo", fullRepo, "issue", n, "error", err)
			continue
		}
		if issue == nil || issue.IsPullRequest() || issue.GetState() != "open" {
			continue
		}
		if check, ok := SharedCITrackingCheck(issue.GetTitle(), fullRepo); ok && check == want {
			if fact.TrackingIssue == 0 {
				c.redMainTracker().SetTrackingIssue(fact.Repo, fact.Check, n)
			}
			return n
		}
	}
	return 0
}
