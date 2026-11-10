package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/issueshape"
	"github.com/hivecommons/hive/pkg/redmain"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Shared-CI tracking issues: the deterministic half of the shared-CI
// detector.
//
// The `[shared-ci] <check> failing across <owner/repo>` incident issue is
// what the red-main breaker, the PR-request refusal, the kick builders and
// the rerun sweep all key on, but until now only an agent filed it: the
// fix-lane policy tells an agent to run hive-baseline-check.sh on a red PR
// it is repairing and file the incident on a DEFER_TO_INCIDENT verdict.
// When no agent picked one of the red PRs up, nothing was filed. On the
// projectbluefin hive (2026-11-08..10) `zizmor` sat red on ten
// projectbluefin/server PRs and `validate` on nine projectbluefin/finpilot
// PRs for more than a day each: the scanner's kick listed every one of
// them (most as fork PRs it may only comment on) and it moved on, and the
// red-main breaker never saw either check, because zizmor's newest run on
// main predated the cause and finpilot's validate does not run on main.
//
// SweepSharedCIIssues closes that gap from the governor's own snapshot, at
// no extra cost per PR: when one check is failing on MinPRs or more open
// PRs in a repository, or has tripped the red-main breaker, it files one
// tracking issue under the same title the agents use, or keeps the one it
// filed earlier up to date. An incident someone else opened is left alone:
// it has already reached the triage queue. It never comments on the PRs.

const (
	// SharedCIIssueMarker marks a tracking issue this sweep filed; only
	// such an issue's body is ever edited.
	SharedCIIssueMarker = "<!-- hive-shared-ci-tracker -->"

	// sharedCIIssueLane is the lane the tracking issue is routed to: CI is
	// the ci-maintainer's work.
	sharedCIIssueLane = "ci-maintainer"

	// DefaultSharedCIIssueMaxWrites caps the issue creates and body edits
	// one pass makes.
	DefaultSharedCIIssueMaxWrites = 5

	// sharedCIIssueSettle is how long a PR cluster must persist before an
	// issue is filed for it, so one burst of runner errors that reds a few
	// PRs at once and clears on the next run does not file an incident.
	// A red-main fact needs no settling: the breaker already waited hours.
	sharedCIIssueSettle = 30 * time.Minute

	// After an incident for the same check closes, the sweep waits this
	// long before filing a new one: a completed close gives the rerun
	// sweep and branch updates time to turn the PRs green; a "not
	// planned" close is a maintainer saying no.
	sharedCIIssueRefileAfterCompleted  = 24 * time.Hour
	sharedCIIssueRefileAfterNotPlanned = 7 * 24 * time.Hour

	sharedCIIssueMaxListedPRs    = 30
	sharedCIIssueMaxExcerptLines = 10
	// sharedCIIssueMaxIssuePages bounds each issue listing (newest first,
	// 100 per page); the red-main sweep's tracking lookup shares it.
	sharedCIIssueMaxIssuePages = 3
)

// sharedCIIssueLabels are applied to a new tracking issue.
var sharedCIIssueLabels = []string{config.AgentLaneLabel(sharedCIIssueLane), "kind/bug"}

// SharedCIIssueTitle is the incident title for check in fullRepo, the
// form SharedCITrackingCheck parses.
func SharedCIIssueTitle(check, fullRepo string) string {
	return fmt.Sprintf("%s %s failing across %s", sharedCIIncidentTitlePrefix, strings.TrimSpace(check), fullRepo)
}

// SharedCICluster is one check failing across a repository: on PRs open
// PRs at once, on the default branch past the red-main threshold (Main),
// or both.
type SharedCICluster struct {
	// Repo is "owner/repo".
	Repo string
	// Check is the check-run name as GitHub reports it.
	Check string
	// PRs are the open PRs failing Check, ascending.
	PRs []int
	// BaseRed is set when Check is also red on the newest run of one of
	// those PRs' base branch tip (PullRequest.BaseRedChecks).
	BaseRed bool
	// Main is the red-main fact when the breaker tripped on Check.
	Main *redmain.Fact
	// Excerpt are the distinct annotation lines the failing runs carry for
	// Check, without the "<check>: " prefix.
	Excerpt []string
}

func (cl SharedCICluster) key() string {
	return strings.ToLower(cl.Repo) + "\x00" + strings.ToLower(cl.Check)
}

// SharedCIClusters groups red PRs by repository and failing check and keeps
// each group of at least minPRs PRs, plus every tripped red-main fact.
// repoKey normalizes a PR's repo to "owner/repo" (facts are keyed that
// way). A failing check that only refuses fork PRs (ForkGuardChecksFor) is
// not counted: it is red on every fork PR by design. The result is sorted
// by PR count, then repo and check, so a capped pass handles the widest
// failure first.
func SharedCIClusters(prs []PullRequest, facts map[string]*redmain.Fact, repoKey func(string) string, minPRs int) []SharedCICluster {
	if minPRs < 2 {
		minPRs = 2
	}
	if repoKey == nil {
		repoKey = func(s string) string { return s }
	}
	byKey := map[string]*SharedCICluster{}
	seenPR := map[string]bool{}
	seenLine := map[string]bool{}
	ordered := append([]PullRequest(nil), prs...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Number < ordered[j].Number })
	for _, pr := range ordered {
		if pr.Number <= 0 || pr.CIStatus != "failure" {
			continue
		}
		repo := repoKey(pr.Repo)
		for _, check := range pr.FailingChecks {
			if strings.TrimSpace(check) == "" || ForkGuardChecksFor(pr.FromFork, []string{check}, pr.CIFailureExcerpt) != nil {
				continue
			}
			cl := SharedCICluster{Repo: repo, Check: check}
			k := cl.key()
			if byKey[k] == nil {
				byKey[k] = &cl
			}
			entry := byKey[k]
			prKey := fmt.Sprintf("%s#%d", k, pr.Number)
			if seenPR[prKey] {
				continue
			}
			seenPR[prKey] = true
			entry.PRs = append(entry.PRs, pr.Number)
			for _, name := range pr.BaseRedChecks {
				if strings.EqualFold(name, check) {
					entry.BaseRed = true
				}
			}
			for _, line := range strings.Split(pr.CIFailureExcerpt, "\n") {
				msg, ok := strings.CutPrefix(strings.TrimSpace(line), check+": ")
				msg = strings.TrimSpace(msg)
				if !ok || msg == "" || seenLine[k+"\x00"+msg] || len(entry.Excerpt) >= sharedCIIssueMaxExcerptLines {
					continue
				}
				seenLine[k+"\x00"+msg] = true
				entry.Excerpt = append(entry.Excerpt, msg)
			}
		}
	}
	var out []SharedCICluster
	for _, cl := range byKey {
		if len(cl.PRs) >= minPRs {
			out = append(out, *cl)
		}
	}
	for repo, fact := range facts {
		if fact == nil || strings.TrimSpace(fact.Check) == "" {
			continue
		}
		cl := SharedCICluster{Repo: repoKey(repo), Check: fact.Check}
		merged := false
		for i := range out {
			if out[i].key() == cl.key() {
				out[i].Main = fact
				merged = true
				break
			}
		}
		if !merged {
			if pr := byKey[cl.key()]; pr != nil {
				cl = *pr // fewer than minPRs PRs, still worth naming
			}
			cl.Main = fact
			out = append(out, cl)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].PRs) != len(out[j].PRs) {
			return len(out[i].PRs) > len(out[j].PRs)
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Check < out[j].Check
	})
	return out
}

// sharedCIIssueExcerptLine makes one CI annotation line safe for the issue
// body: no code-fence breakout, and no `<two words>` span that the issue
// template guard would refuse as an unfilled placeholder.
func sharedCIIssueExcerptLine(s string) string {
	s = strings.ReplaceAll(s, "```", "'''")
	s = strings.ReplaceAll(s, "<", "‹")
	return strings.ReplaceAll(s, ">", "›")
}

// sharedCIIssuePRCount is "1 open pull request" or "N open pull requests".
func sharedCIIssuePRCount(n int) string {
	if n == 1 {
		return "1 open pull request"
	}
	return fmt.Sprintf("%d open pull requests", n)
}

// RenderSharedCIIssueBody is the tracking issue body for cl. It carries no
// timestamp of its own, so an unchanged failure renders the same body and
// costs no edit.
func RenderSharedCIIssueBody(cl SharedCICluster) string {
	var b strings.Builder
	b.WriteString(SharedCIIssueMarker + "\n")
	branch := "the default branch"
	if cl.Main != nil && cl.Main.Branch != "" {
		branch = "`" + cl.Main.Branch + "`"
	}
	switch {
	case cl.Main != nil:
		fmt.Fprintf(&b, "**Summary:** The required check `%s` has been red on %s of %s since %s, so pull requests "+
			"opened against it are red too", cl.Check, branch, cl.Repo, cl.Main.Since.UTC().Format("2006-01-02 15:04 UTC"))
		if n := len(cl.PRs); n > 0 {
			fmt.Fprintf(&b, " (%s failing it now)", sharedCIIssuePRCount(n))
		}
		b.WriteString(". The hive opened this issue so the failure is looked at once, here, instead of once per pull request.\n\n")
	default:
		fmt.Fprintf(&b, "**Summary:** The `%s` check is failing on %s in %s at the same time. "+
			"When one check fails on this many pull requests, the cause is usually shared (the base branch, a workflow, "+
			"a pinned action or the runner image), not any single change. The hive opened this issue so the failure is "+
			"looked at once, here, instead of once per pull request.\n\n", cl.Check, sharedCIIssuePRCount(len(cl.PRs)), cl.Repo)
	}
	b.WriteString("### Evidence\n\n")
	fmt.Fprintf(&b, "- Check: `%s`\n", cl.Check)
	if len(cl.PRs) > 0 {
		refs := make([]string, 0, len(cl.PRs))
		for i, n := range cl.PRs {
			if i == sharedCIIssueMaxListedPRs {
				refs = append(refs, fmt.Sprintf("and %d more", len(cl.PRs)-i))
				break
			}
			refs = append(refs, fmt.Sprintf("#%d", n))
		}
		fmt.Fprintf(&b, "- Open pull requests failing it (%d): %s\n", len(cl.PRs), strings.Join(refs, ", "))
	}
	switch {
	case cl.Main != nil:
		fmt.Fprintf(&b, "- Default branch: red on %s since %s", branch, cl.Main.Since.UTC().Format("2006-01-02 15:04 UTC"))
		if cl.Main.RunURL != "" {
			fmt.Fprintf(&b, " (failing run: %s)", cl.Main.RunURL)
		}
		b.WriteString("\n")
	case cl.BaseRed:
		b.WriteString("- Default branch: the newest run of this check on the pull requests' base branch is red too.\n")
	default:
		b.WriteString("- Default branch: the newest run of this check on the base branch is not red. Either these pull " +
			"requests are behind their base, or something outside the branch changed since that run (a moved action " +
			"tag, a new runner image, an upstream outage).\n")
	}
	if len(cl.Excerpt) > 0 {
		b.WriteString("\nLog excerpt from the failing runs:\n\n```text\n")
		for _, line := range cl.Excerpt {
			b.WriteString(sharedCIIssueExcerptLine(line) + "\n")
		}
		b.WriteString("```\n")
	}
	b.WriteString("\n### Next step\n\n")
	b.WriteString("Repair the shared cause once, usually with one pull request against the base branch or the shared " +
		"workflow, then close this issue. Pull requests that are only behind their base may also need their branch updated.\n\n")
	b.WriteString("_The hive keeps this description in step with the failing pull requests while the failure lasts " +
		"(edits do not notify) and does not comment on the pull requests listed here._\n")
	return b.String()
}

// SharedCIIssueTracker remembers when each PR cluster was first seen, so a
// cluster is filed only once it has lasted sharedCIIssueSettle. The caller
// owns one per process; a restart only delays a filing.
type SharedCIIssueTracker struct {
	mu        sync.Mutex
	firstSeen map[string]time.Time
}

// NewSharedCIIssueTracker returns an empty tracker.
func NewSharedCIIssueTracker() *SharedCIIssueTracker {
	return &SharedCIIssueTracker{firstSeen: map[string]time.Time{}}
}

// observe records this pass's clusters and forgets the ones that are gone.
func (t *SharedCIIssueTracker) observe(clusters []SharedCICluster, now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	present := map[string]bool{}
	for _, cl := range clusters {
		k := cl.key()
		present[k] = true
		if _, ok := t.firstSeen[k]; !ok {
			t.firstSeen[k] = now
		}
	}
	for k := range t.firstSeen {
		if !present[k] {
			delete(t.firstSeen, k)
		}
	}
}

// settled reports whether cl may be filed at now.
func (t *SharedCIIssueTracker) settled(cl SharedCICluster, now time.Time) bool {
	if cl.Main != nil || t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	first, ok := t.firstSeen[cl.key()]
	return ok && now.Sub(first) >= sharedCIIssueSettle
}

// SharedCIIssueOptions bounds one SweepSharedCIIssues pass.
type SharedCIIssueOptions struct {
	// MinPRs is the PR threshold; non-positive means
	// config.DefaultSharedCIIssuesMinPRs.
	MinPRs int
	// MaxWrites caps creates plus edits; non-positive means
	// DefaultSharedCIIssueMaxWrites.
	MaxWrites int
	// Tracker settles new PR clusters; nil files them on first sight.
	Tracker *SharedCIIssueTracker
	Audit   func(SharedCIIssueEvent)
	// Now overrides the clock in tests.
	Now func() time.Time
}

// SharedCIIssueEvent is one tracking issue filed or updated.
type SharedCIIssueEvent struct {
	Repo   string
	Check  string
	Issue  int
	PRs    []int
	OnMain bool
	// Action is "filed" or "updated".
	Action string
}

// SharedCIIssueResult summarizes one pass.
type SharedCIIssueResult struct {
	Clusters int
	Events   []SharedCIIssueEvent
	Skipped  int
}

// SweepSharedCIIssues files or updates one `[shared-ci]` tracking issue per
// check that is failing across a repository (see the file comment).
// actionable is the governor's latest snapshot; the sweep reads PRs and
// red-main facts from it and makes no per-PR call. Only repositories with a
// cluster are read: one open-issue listing each, plus one closed-issue
// listing before a new issue is filed. A failed lookup files nothing.
func (c *Client) SweepSharedCIIssues(ctx context.Context, actionable *ActionableResult, opts SharedCIIssueOptions) (*SharedCIIssueResult, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	res := &SharedCIIssueResult{}
	if actionable == nil {
		return res, nil
	}
	minPRs := opts.MinPRs
	if minPRs <= 0 {
		minPRs = config.DefaultSharedCIIssuesMinPRs
	}
	maxWrites := opts.MaxWrites
	if maxWrites <= 0 {
		maxWrites = DefaultSharedCIIssueMaxWrites
	}
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	prs := make([]PullRequest, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	prs = append(prs, actionable.PRs.Items...)
	prs = append(prs, actionable.PRs.Held...)
	clusters := SharedCIClusters(prs, actionable.RedMain, c.redMainRepoKey, minPRs)
	opts.Tracker.observe(clusters, now)
	active := map[string]bool{}
	for _, repo := range c.activeRepos() {
		active[strings.ToLower(c.redMainRepoKey(repo))] = true
	}
	ctx = WithRESTCaller(ctx, "hive:shared_ci_issues")
	openByRepo := map[string]map[string]*gh.Issue{}
	writes := 0
	for _, cl := range clusters {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		repoKey := strings.ToLower(cl.Repo)
		if !active[repoKey] {
			continue
		}
		res.Clusters++
		if writes >= maxWrites {
			res.Skipped++
			continue
		}
		owner, name := c.splitRepo(cl.Repo)
		open, known := openByRepo[repoKey]
		if !known {
			var err error
			open, err = c.openSharedCIIncidentIssues(ctx, owner, name)
			if err != nil {
				c.logger.Debug("shared-ci issues: open incident lookup failed; filing nothing for the repo this pass", "repo", cl.Repo, "error", err)
			}
			openByRepo[repoKey] = open
		}
		if open == nil {
			res.Skipped++
			continue
		}
		body := advisory.NeutralizeMentions(RenderSharedCIIssueBody(cl))
		if existing := open[strings.ToLower(cl.Check)]; existing != nil {
			ev, wrote, err := c.updateSharedCIIssue(ctx, cl, existing, body)
			if wrote {
				writes++
			}
			if err != nil {
				c.logger.Warn("shared-ci issues: tracking issue update failed", "repo", cl.Repo, "check", cl.Check, "issue", existing.GetNumber(), "error", err)
				res.Skipped++
				continue
			}
			if ev != nil {
				res.Events = append(res.Events, *ev)
				if opts.Audit != nil {
					opts.Audit(*ev)
				}
			}
			continue
		}
		if !opts.Tracker.settled(cl, now) {
			continue
		}
		recent, err := c.sharedCIIssueRecentlyClosed(ctx, owner, name, cl.Check, now)
		if err != nil || recent {
			if err != nil {
				c.logger.Debug("shared-ci issues: closed incident lookup failed; not filing", "repo", cl.Repo, "check", cl.Check, "error", err)
			}
			res.Skipped++
			continue
		}
		title := SharedCIIssueTitle(cl.Check, owner+"/"+name)
		if d := c.WritebackAllowed(cl.Repo, writeback.Item{HiveAuthored: true}, writeback.KindIssueFile, title); !d.Allowed {
			res.Skipped++
			continue
		}
		writes++
		created, err := c.CreateIssue(ctx, CreateIssueRequest{Repo: owner + "/" + name, Title: title, Body: body, Labels: sharedCIIssueLabels, Origin: config.OriginSourceGovernor}) // writeback:gated — WritebackAllowed consulted above
		if err != nil {
			c.logger.Warn("shared-ci issues: filing the tracking issue failed", "repo", cl.Repo, "check", cl.Check, "error", err)
			res.Skipped++
			continue
		}
		open[strings.ToLower(cl.Check)] = &gh.Issue{Number: gh.Ptr(created.Number), Title: gh.Ptr(title), Body: gh.Ptr(body)}
		if created.AlreadyExisted || created.Consolidated || created.RejectedTwin {
			// An open twin already tracks it, or a maintainer rejected the
			// same finding: nothing new was filed.
			continue
		}
		if cl.Main != nil {
			c.redMainTracker().SetTrackingIssue(c.redMainRepoKey(cl.Repo), cl.Main.Check, created.Number)
		}
		ev := SharedCIIssueEvent{Repo: owner + "/" + name, Check: cl.Check, Issue: created.Number, PRs: cl.PRs, OnMain: cl.Main != nil, Action: "filed"}
		c.logger.Info("shared-ci issues: tracking issue filed", "repo", ev.Repo, "check", ev.Check, "issue", ev.Issue, "prs", len(ev.PRs), "on_main", ev.OnMain)
		res.Events = append(res.Events, ev)
		if opts.Audit != nil {
			opts.Audit(ev)
		}
	}
	return res, nil
}

// sharedCIIssueOwned reports whether issue is a tracking issue this sweep
// filed: it carries the marker and, when the hive knows its own accounts,
// one of them wrote it.
func (c *Client) sharedCIIssueOwned(issue *gh.Issue) bool {
	if issue == nil || !strings.Contains(issue.GetBody(), SharedCIIssueMarker) {
		return false
	}
	return !c.knowsOwnAccounts() || c.isHiveAccount(issue.GetUser())
}

// updateSharedCIIssue edits the body of a tracking issue this sweep filed
// when the failure it describes changed. Any other open incident for the
// check is left alone. wrote reports an attempted GitHub write.
func (c *Client) updateSharedCIIssue(ctx context.Context, cl SharedCICluster, existing *gh.Issue, body string) (ev *SharedCIIssueEvent, wrote bool, err error) {
	if !c.sharedCIIssueOwned(existing) {
		return nil, false, nil
	}
	owner, name := c.splitRepo(cl.Repo)
	if leak, ok := c.scanCanaryText(body, "hive-shared-ci-issue:"+owner+"/"+name); ok && c.canaryFailClosed {
		return nil, false, fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
	}
	body = logscrub.ScrubString(body)
	if strings.TrimSpace(existing.GetBody()) == strings.TrimSpace(body) {
		return nil, false, nil
	}
	item := writeback.Item{HiveAuthored: true, Number: existing.GetNumber(), Author: safeGetLogin(existing.GetUser()), Labels: issueLabelNames(existing.Labels)}
	if d := c.WritebackAllowed(cl.Repo, item, writeback.KindIssueFile, existing.GetTitle()); !d.Allowed {
		return nil, false, nil
	}
	var resp *gh.Response
	_, resp, err = c.client.Issues.Edit(ctx, owner, name, existing.GetNumber(), &gh.IssueRequest{Body: gh.Ptr(body)}) // writeback:gated — WritebackAllowed consulted above
	c.noteWritebackOutcome(owner+"/"+name, resp, err)
	if err != nil {
		return nil, true, err
	}
	existing.Body = gh.Ptr(body)
	return &SharedCIIssueEvent{Repo: owner + "/" + name, Check: cl.Check, Issue: existing.GetNumber(), PRs: cl.PRs, OnMain: cl.Main != nil, Action: "updated"}, true, nil
}

// sharedCIIncidentCheck is SharedCITrackingCheck that also accepts an
// incident an agent titled with its own tag in front
// (`[scanner] [shared-ci] lint failing across o/r`), so the sweep never
// files a second incident beside it.
func sharedCIIncidentCheck(title, fullRepo string) (string, bool) {
	if check, ok := SharedCITrackingCheck(title, fullRepo); ok {
		return check, true
	}
	if tag := issueshape.AgentPrefix(title); tag != "" && tag != "shared-ci" {
		return SharedCITrackingCheck(issueshape.StripAgentPrefix(title), fullRepo)
	}
	return "", false
}

// openSharedCIIncidentIssues maps a lower-cased check name to the open
// `[shared-ci] <check> failing across <owner/repo>` issue in owner/repo
// (the lowest number when several are open).
func (c *Client) openSharedCIIncidentIssues(ctx context.Context, owner, repo string) (map[string]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Sort:        "created",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	out := map[string]*gh.Issue{}
	for page := 0; page < sharedCIIssueMaxIssuePages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open shared-CI incidents in %s/%s: %w", owner, repo, err)
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() {
				continue
			}
			check, ok := sharedCIIncidentCheck(issue.GetTitle(), owner+"/"+repo)
			if !ok {
				continue
			}
			if prev, seen := out[check]; !seen || issue.GetNumber() < prev.GetNumber() {
				out[check] = issue
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

// sharedCIIssueRecentlyClosed reports whether an incident for check in
// owner/repo closed recently enough that a new one must wait: within
// sharedCIIssueRefileAfterCompleted of a completed close, or
// sharedCIIssueRefileAfterNotPlanned of a "not planned" one.
func (c *Client) sharedCIIssueRecentlyClosed(ctx context.Context, owner, repo, check string, now time.Time) (bool, error) {
	want := strings.ToLower(strings.TrimSpace(check))
	since := now.Add(-sharedCIIssueRefileAfterNotPlanned)
	opts := &gh.IssueListByRepoOptions{
		State:       "closed",
		Sort:        "updated",
		Direction:   "desc",
		Since:       cacheableSince(since),
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 0; page < sharedCIIssueMaxIssuePages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return false, fmt.Errorf("listing closed shared-CI incidents in %s/%s: %w", owner, repo, err)
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() || issue.ClosedAt == nil {
				continue
			}
			if got, ok := sharedCIIncidentCheck(issue.GetTitle(), owner+"/"+repo); !ok || got != want {
				continue
			}
			wait := sharedCIIssueRefileAfterCompleted
			if strings.EqualFold(issue.GetStateReason(), "not_planned") {
				wait = sharedCIIssueRefileAfterNotPlanned
			}
			if now.Sub(issue.GetClosedAt().Time) < wait {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return false, nil
}
