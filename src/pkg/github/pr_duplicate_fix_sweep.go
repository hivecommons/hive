package github

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// Same-issue duplicate-PR resolver (hivecommons/hive#14799). When two open
// pull requests both link one issue with a closing keyword, the hive used
// to label the pair conflicts-with-open-pr and park the later one at
// stage/085 for the owner to pick. The pick is mechanical, so the sweep
// makes it: the earlier-created PR wins while its checks are green or
// pending, the later one gets one comment and is closed with the reason
// duplicate-of:<keeper>. Only the genuinely ambiguous case — the earlier PR
// is red or stale and the later one is green — still goes to the owner. A
// person's PR is never closed by this sweep: it gets the comment and stays
// open where it is. A registered relay contributor's later PR
// (hivecommons/hive#15469) is the hive's own agent for the write-back gate
// yet is also left open: one comment, held out of the merge queue, parked
// with its author at stage 072 by the stage engine.

const (
	// DuplicateFixSweepAuditPrefix prefixes the dashboard audit action for
	// every write the sweep takes.
	DuplicateFixSweepAuditPrefix = "duplicate-fix-"
	// DuplicateFixActionClosed: the later hive-authored PR was closed.
	DuplicateFixActionClosed = "closed"
	// DuplicateFixActionCommented: the later PR is a person's; the comment
	// was posted and the PR left open.
	DuplicateFixActionCommented = "commented"
	// DuplicateFixActionEscalated: the earlier PR is red or stale while the
	// later is green; the later PR was parked for the owner.
	DuplicateFixActionEscalated = "escalated"
	// DuplicateFixActionSuppressed records a pair the write-back gate kept
	// off GitHub (dashboard only).
	DuplicateFixActionSuppressed = "suppressed"

	// DefaultDuplicateFixSweepMaxActions bounds writes per sweep.
	DefaultDuplicateFixSweepMaxActions = 10
	// DuplicateFixStaleAfter is how long an earlier PR may go without an
	// update before a green later PR earns the owner's attention instead of
	// being closed.
	DuplicateFixStaleAfter = 24 * time.Hour

	// duplicateFixMarker tags the sweep's comment so a later sweep posts it
	// at most once per PR.
	duplicateFixMarker = "<!-- hive:duplicate-fix -->"
	// duplicateFixHumanLabel is the label the hive puts on PRs a person
	// authored; such PRs are commented on, never closed.
	duplicateFixHumanLabel = "author/human"
	// duplicateFixAuthorLabelPrefix is the hive's author attribution family.
	duplicateFixAuthorLabelPrefix = "author/"
)

// DuplicateFixCheckState is the rollup of the latest check runs on a PR head.
type DuplicateFixCheckState string

const (
	DuplicateFixChecksGreen   DuplicateFixCheckState = "green"
	DuplicateFixChecksPending DuplicateFixCheckState = "pending"
	DuplicateFixChecksRed     DuplicateFixCheckState = "red"
)

// DuplicateFixFacts is everything DecideDuplicateFix needs about one pair.
type DuplicateFixFacts struct {
	EarlierChecks DuplicateFixCheckState
	LaterChecks   DuplicateFixCheckState
	// EarlierStale: the earlier PR has had no update for DuplicateFixStaleAfter.
	EarlierStale bool
	// LaterHuman: a person authored the later PR.
	LaterHuman bool
	// LaterRelay: a relay contributor registered on this hive authored the
	// later PR (hivecommons/hive#15469). It is the hive's own agent, so the
	// notice is never gated as unsolicited — but the PR is left open and
	// parked with its author (stage 072, "duplicate of #keeper") rather
	// than closed: the relay's own hive or the delegate decides.
	LaterRelay bool
}

// DuplicateFixVerdict is DecideDuplicateFix's outcome.
type DuplicateFixVerdict string

const (
	// DuplicateFixVerdictClose: comment on and close the later PR.
	DuplicateFixVerdictClose DuplicateFixVerdict = "close"
	// DuplicateFixVerdictCommentOnly: comment on the later PR, leave it open.
	DuplicateFixVerdictCommentOnly DuplicateFixVerdict = "comment-only"
	// DuplicateFixVerdictEscalate: park the later PR for the owner.
	DuplicateFixVerdictEscalate DuplicateFixVerdict = "escalate"
)

// DecideDuplicateFix is the pure rule: the earlier PR wins when its checks
// are green or pending; a red or stale earlier PR facing a green later one
// is the owner's call. A person's later PR is never closed — the verdict
// degrades to comment-only (or escalate, which also leaves it open).
func DecideDuplicateFix(f DuplicateFixFacts) DuplicateFixVerdict {
	earlierWeak := f.EarlierChecks == DuplicateFixChecksRed || f.EarlierStale
	if earlierWeak && f.LaterChecks == DuplicateFixChecksGreen {
		return DuplicateFixVerdictEscalate
	}
	if f.LaterHuman || f.LaterRelay {
		return DuplicateFixVerdictCommentOnly
	}
	return DuplicateFixVerdictClose
}

// DuplicateFixEvent is one write the sweep took.
type DuplicateFixEvent struct {
	Repo      string
	Number    int
	Author    string
	IssueRepo string
	Issue     int
	// KeeperPR is the earlier PR that stays open.
	KeeperPR int
	Action   string
	// Reason is the recorded disposition: "duplicate-of:<keeper>" for a
	// close or comment, "earlier-<red|stale>-later-green" for an escalation.
	Reason string
}

// DuplicateFixSweepOptions configures SweepDuplicateFixPRs.
type DuplicateFixSweepOptions struct {
	MaxActions int
	Audit      func(DuplicateFixEvent)
	// Now overrides the clock (tests).
	Now func() time.Time
}

// DuplicateFixSweepResult summarises one sweep.
type DuplicateFixSweepResult struct {
	Closed    []DuplicateFixEvent
	Commented []DuplicateFixEvent
	Escalated []DuplicateFixEvent
	// Pairs is the number of same-issue open-PR pairs examined.
	Pairs   int
	Skipped int
}

// Actions counts the writes the sweep took, against MaxActions.
func (r *DuplicateFixSweepResult) Actions() int {
	if r == nil {
		return 0
	}
	return len(r.Closed) + len(r.Commented) + len(r.Escalated)
}

// RenderDuplicateFixComment is the one comment the later PR receives.
func RenderDuplicateFixComment(keeper, issue int, human bool) string {
	if human {
		return fmt.Sprintf("%s\nSuperseded by #%d (earlier fix for #%d). Leaving open because a person authored this PR — "+
			"close it if #%d covers your change, or comment why both should land.", duplicateFixMarker, keeper, issue, keeper)
	}
	return fmt.Sprintf("%s\nSuperseded by #%d (earlier fix for #%d). Closing as duplicate.", duplicateFixMarker, keeper, issue)
}

// RenderDuplicateFixRelayComment is the one comment a registered relay
// contributor's later PR receives (hivecommons/hive#15469): held out of the
// merge queue and returned to its author as superseded, never closed by
// the sweep.
func RenderDuplicateFixRelayComment(keeper, issue int) string {
	return fmt.Sprintf("%s\nSuperseded by #%d (earlier fix for #%d). The merge queue carries one PR per issue, so this PR is held out of it "+
		"and returned to its author; close it if #%d covers the change, or let the delegate decide.", duplicateFixMarker, keeper, issue, keeper)
}

// SweepDuplicateFixPRs walks every configured repo's open PRs (the same
// listing the pre-dispatch guard caches) and resolves each issue two or more
// of them close.
func (c *Client) SweepDuplicateFixPRs(ctx context.Context, opts DuplicateFixSweepOptions) (*DuplicateFixSweepResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultDuplicateFixSweepMaxActions
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	result := &DuplicateFixSweepResult{}
	for _, repo := range c.getRepos() {
		if result.Actions() >= maxActions {
			break
		}
		idx, err := c.openPRIndexFor(ctx, repo)
		if err != nil {
			return result, err
		}
		byNumber := map[int]*gh.PullRequest{}
		for _, pr := range idx.prs {
			if pr != nil {
				byNumber[pr.GetNumber()] = pr
			}
		}
		for _, key := range sortedKeys(idx.byIssue) {
			if result.Actions() >= maxActions {
				break
			}
			links := idx.byIssue[key]
			if len(links) < 2 {
				continue
			}
			result.Pairs++
			events, err := c.resolveDuplicateFixGroup(ctx, repo, links, byNumber, now())
			if err != nil {
				return result, err
			}
			if len(events) == 0 {
				result.Skipped++
			}
			for _, ev := range events {
				switch ev.Action {
				case DuplicateFixActionClosed:
					result.Closed = append(result.Closed, ev)
				case DuplicateFixActionCommented:
					result.Commented = append(result.Commented, ev)
				case DuplicateFixActionEscalated:
					result.Escalated = append(result.Escalated, ev)
				}
				if opts.Audit != nil {
					opts.Audit(ev)
				}
			}
		}
		if result.Actions() > 0 {
			c.InvalidateOpenPRLinks(repo)
		}
	}
	return result, nil
}

func sortedKeys(m map[string][]OpenPRLink) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveDuplicateFixGroup resolves one issue's open PRs: the earliest is
// the keeper, every later one is decided against it.
func (c *Client) resolveDuplicateFixGroup(ctx context.Context, repo string, links []OpenPRLink, byNumber map[int]*gh.PullRequest, now time.Time) ([]DuplicateFixEvent, error) {
	owner, repoName := c.splitRepo(repo)
	keeperLink := links[0]
	keeper := byNumber[keeperLink.PRNumber]
	if keeper == nil {
		return nil, nil
	}
	// The issue must still be open: a closed issue is the supersession
	// sweep's territory (its merged closer decides), not this one's.
	issueOwner, issueRepoName := c.splitRepo(keeperLink.Repo)
	issue, _, err := c.client.Issues.Get(WithRESTCaller(ctx, "hive:duplicate_fix"), issueOwner, issueRepoName, keeperLink.Issue)
	if err != nil {
		c.logger.Warn("duplicate-fix sweep: could not read issue, skipping", slog.String("repo", keeperLink.Repo), slog.Int("issue", keeperLink.Issue), slog.String("error", err.Error()))
		return nil, nil
	}
	if issue != nil && !strings.EqualFold(issue.GetState(), "open") {
		return nil, nil
	}
	earlierChecks, err := c.duplicateFixCheckState(ctx, owner, repoName, keeper)
	if err != nil {
		return nil, err
	}
	earlierStale := now.Sub(keeper.GetUpdatedAt().Time) > DuplicateFixStaleAfter

	var events []DuplicateFixEvent
	for _, link := range links[1:] {
		later := byNumber[link.PRNumber]
		if later == nil {
			continue
		}
		labels := labelNames(later.Labels)
		// An operator's hold or exemption is an instruction to leave the PR
		// alone, as every other sweep honours it.
		if c.isHeld(labels) || c.isExempt(labels) {
			continue
		}
		// A stage-074 adoption pair is not a duplicate: the superseding App
		// PR carries the original's commits and closes the original when it
		// merges. Closing the adoptee as "duplicate of" the PR it supersedes
		// (hivecommons/hive#14977 → #14947) undoes the adoption.
		if PRSupersedes(later.GetBody(), keeper.GetNumber()) || PRSupersedes(keeper.GetBody(), later.GetNumber()) {
			c.logger.Info("duplicate-fix sweep: adoption pair left to the stage pass", slog.String("repo", repo), slog.Int("issue", link.Issue), slog.Int("keeper", keeper.GetNumber()), slog.Int("pr", later.GetNumber()))
			continue
		}
		already, err := c.hasDuplicateFixComment(ctx, owner, repoName, later.GetNumber())
		if err != nil {
			return events, err
		}
		if already {
			continue
		}
		laterChecks, err := c.duplicateFixCheckState(ctx, owner, repoName, later)
		if err != nil {
			return events, err
		}
		laterRelay := c.duplicateFixIsRelay(later)
		facts := DuplicateFixFacts{
			EarlierChecks: earlierChecks,
			LaterChecks:   laterChecks,
			EarlierStale:  earlierStale,
			LaterHuman:    !laterRelay && c.duplicateFixIsHuman(later),
			LaterRelay:    laterRelay,
		}
		verdict := DecideDuplicateFix(facts)
		event := DuplicateFixEvent{
			Repo:      repo,
			Number:    later.GetNumber(),
			Author:    later.GetUser().GetLogin(),
			IssueRepo: link.Repo,
			Issue:     link.Issue,
			KeeperPR:  keeper.GetNumber(),
		}
		// Write-back gate: the later PR may be a contributor's. Below the
		// level duplicate_fix_notice needs, or without the opt-in label,
		// the pair is logged and nothing is written on it.
		item := c.sentinelWritebackItem(later)
		item.HiveAuthored = item.HiveAuthored || !facts.LaterHuman
		if d := c.WritebackAllowed(repo, item, writeback.KindDuplicateFixNotice, RenderDuplicateFixComment(keeper.GetNumber(), link.Issue, true)); !d.Allowed {
			event.Action = DuplicateFixActionSuppressed
			event.Reason = "write-back gate: " + d.Reason
			events = append(events, event)
			continue
		}
		switch verdict {
		case DuplicateFixVerdictEscalate:
			event.Action = DuplicateFixActionEscalated
			event.Reason = "earlier-red-later-green"
			if earlierChecks != DuplicateFixChecksRed {
				event.Reason = "earlier-stale-later-green"
			}
			body := fmt.Sprintf("%s\nBoth this PR and #%d fix #%d. #%d opened first but its checks are %s%s while this PR is green, "+
				"so the hive is asking the owner to pick rather than closing either.",
				duplicateFixMarker, keeper.GetNumber(), link.Issue, keeper.GetNumber(), earlierChecks, duplicateFixStaleNote(earlierStale))
			if err := c.CreateIssueComment(ctx, repo, later.GetNumber(), body); err != nil { // writeback:gated
				return events, fmt.Errorf("commenting on duplicate PR %s#%d: %w", repo, later.GetNumber(), err)
			}
			if err := c.parkSupersededPR(ctx, repo, later.GetNumber()); err != nil {
				return events, err
			}
		case DuplicateFixVerdictCommentOnly:
			event.Action = DuplicateFixActionCommented
			event.Reason = fmt.Sprintf("duplicate-of:%d", keeper.GetNumber())
			body := RenderDuplicateFixComment(keeper.GetNumber(), link.Issue, true)
			if facts.LaterRelay {
				body = RenderDuplicateFixRelayComment(keeper.GetNumber(), link.Issue)
			}
			if err := c.CreateIssueComment(ctx, repo, later.GetNumber(), body); err != nil { // writeback:gated
				return events, fmt.Errorf("commenting on duplicate PR %s#%d: %w", repo, later.GetNumber(), err)
			}
		default:
			event.Action = DuplicateFixActionClosed
			event.Reason = fmt.Sprintf("duplicate-of:%d", keeper.GetNumber())
			if err := c.CreateIssueComment(ctx, repo, later.GetNumber(), RenderDuplicateFixComment(keeper.GetNumber(), link.Issue, false)); err != nil { // writeback:gated
				return events, fmt.Errorf("commenting on duplicate PR %s#%d: %w", repo, later.GetNumber(), err)
			}
			if err := c.labelAndCloseSupersededPR(ctx, owner, repoName, later.GetNumber()); err != nil {
				return events, err
			}
		}
		c.logger.Info("duplicate-fix sweep: resolved same-issue PR pair",
			slog.String("repo", repo), slog.Int("issue", link.Issue), slog.Int("keeper", keeper.GetNumber()),
			slog.Int("pr", later.GetNumber()), slog.String("action", event.Action), slog.String("reason", event.Reason),
			slog.String("earlier_checks", string(earlierChecks)), slog.String("later_checks", string(laterChecks)))
		events = append(events, event)
	}
	return events, nil
}

// prSupersedesRe matches the "Supersedes #N" line the adoption writes into
// a superseding PR's body (AdoptionPRBody).
var prSupersedesRe = regexp.MustCompile(`(?im)^\s*Supersedes\s+#(\d+)\b`)

// PRSupersedes reports whether body declares the PR a supersession of
// pull request n ("Supersedes #n", the adoption's body line).
func PRSupersedes(body string, n int) bool {
	if n <= 0 {
		return false
	}
	for _, m := range prSupersedesRe.FindAllStringSubmatch(body, -1) {
		if v, err := strconv.Atoi(m[1]); err == nil && v == n {
			return true
		}
	}
	return false
}

func duplicateFixStaleNote(stale bool) string {
	if stale {
		return " and it has had no update for over 24h"
	}
	return ""
}

// duplicateFixIsHuman: the hive's author/* attribution is authoritative
// when present — author/human is a person, author/clanker or another
// author/* is an agent run under a person's login and may be closed. With
// no attribution yet, any login that is not the hive's identity or a bot
// is treated as a person.
// duplicateFixIsRelay: the author has a contributor profile on this hive
// (SetRelayKnownFunc) and no author/human attribution says otherwise.
func (c *Client) duplicateFixIsRelay(pr *gh.PullRequest) bool {
	for _, l := range pr.Labels {
		name := strings.ToLower(strings.TrimSpace(l.GetName()))
		if strings.HasPrefix(name, duplicateFixAuthorLabelPrefix) {
			return name != duplicateFixHumanLabel && c.IsRelayContributorLogin(pr.GetUser().GetLogin())
		}
	}
	return c.IsRelayContributorLogin(pr.GetUser().GetLogin())
}

func (c *Client) duplicateFixIsHuman(pr *gh.PullRequest) bool {
	for _, l := range pr.Labels {
		name := strings.ToLower(strings.TrimSpace(l.GetName()))
		if strings.HasPrefix(name, duplicateFixAuthorLabelPrefix) {
			return name == duplicateFixHumanLabel
		}
	}
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	return supersessionIsHuman(pr.GetUser().GetLogin(), pr.GetUser().GetType(), identity)
}

// hasDuplicateFixComment reports whether the sweep already commented on the PR.
func (c *Client) hasDuplicateFixComment(ctx context.Context, owner, repo string, number int) (bool, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.client.Issues.ListComments(WithRESTCaller(ctx, "hive:duplicate_fix"), owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing comments on %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, cm := range comments {
			if strings.Contains(cm.GetBody(), duplicateFixMarker) {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false, nil
		}
		opts.Page = resp.NextPage
	}
}

// duplicateFixCheckState rolls the latest check runs on the PR head up to
// green, pending or red. No check runs at all is pending: the head may not
// have been built yet, and "nothing ran" must not read as a pass.
func (c *Client) duplicateFixCheckState(ctx context.Context, owner, repo string, pr *gh.PullRequest) (DuplicateFixCheckState, error) {
	sha := pr.GetHead().GetSHA()
	if sha == "" {
		return DuplicateFixChecksPending, nil
	}
	ctx = WithRESTCaller(ctx, "hive:duplicate_fix")
	var all []*gh.CheckRun
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			return "", fmt.Errorf("listing check runs for %s/%s#%d: %w", owner, repo, pr.GetNumber(), err)
		}
		if runs != nil {
			all = append(all, runs.CheckRuns...)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	counted, pending := 0, false
	for _, cr := range latestCheckRunsByNameAndApp(all) {
		name := cr.GetName()
		if name == "" || isMetaCheck(name) || isIgnorableCICheck(name) {
			continue
		}
		counted++
		if cr.GetStatus() != "completed" {
			pending = true
			continue
		}
		if checkConclusionIsRed(cr.GetConclusion()) {
			return DuplicateFixChecksRed, nil
		}
	}
	if counted == 0 || pending {
		return DuplicateFixChecksPending, nil
	}
	return DuplicateFixChecksGreen, nil
}
