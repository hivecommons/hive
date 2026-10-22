package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const fleetReportRepo = "hivecommons/hive"

// FleetReportOptions carries the per-hive facts the self-report path needs
// (hivecommons/hive#11868). Zero value = legacy behaviour: hivecommons/hive,
// anonymous hive, fingerprint-only dedupe.
type FleetReportOptions struct {
	// Repo is the report repository (owner/name). Empty = hivecommons/hive.
	Repo string
	// HiveSlug is the hub-assigned slug named in the seen-marker comment and
	// the Affected-hives block; empty keeps the hive anonymous.
	HiveSlug string
	Version  string
	Cluster  string
	// Now is the clock; nil = time.Now.
	Now func() time.Time
	// JoinExisting enables the looser class/keyword search for an existing
	// issue (including human-filed ones) before creating a new one.
	JoinExisting bool
	// SeenCommentAllowed is false when this hive already added itself to the
	// matched issue inside the once-per-24h window; the label and body
	// update still happen (idempotent), the comment is skipped.
	SeenCommentAllowed bool
	// CreateGate, when set, runs just before a NEW issue would be created
	// and its error aborts the create. The publisher uses it to apply the
	// per-day cap / min gap to creates only, so joins keep working.
	CreateGate func() error
}

func (o FleetReportOptions) repo() string {
	if r := strings.TrimSpace(o.Repo); r != "" {
		return r
	}
	return fleetReportRepo
}

func (o FleetReportOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

type FleetReportResult struct {
	Number       int
	URL          string
	Created      bool
	Commented    bool
	ReactionSent bool
	// Joined is true when an existing issue (fingerprint or looser match) was
	// updated instead of a new one created.
	Joined bool
	// ClosedAt is set (and nothing is written) when the fingerprint's issue
	// exists but is closed: the caller decides whether the closed cooldown
	// applies.
	ClosedAt time.Time
	// BodyUpdated reports whether the Affected-hives block was edited.
	BodyUpdated bool
}

// EnsureFleetReport writes or updates a fleet report in hivecommons/hive with
// the legacy (anonymous, fingerprint-only) options.
func (c *Client) EnsureFleetReport(ctx context.Context, report fleetreport.Report) (FleetReportResult, error) {
	return c.EnsureFleetReportWithOptions(ctx, report, FleetReportOptions{SeenCommentAllowed: true})
}

// EnsureFleetReportWithOptions is the join-first self-report writer:
//
//  1. search the report repo for the fingerprint marker (any state). Closed
//     → return ClosedAt and write nothing. Open → join it.
//  2. with JoinExisting, search open issues labelled hive/selfreport for the
//     class, then open issues whose title/body mention the class keywords
//     (catches human-filed issues). First match → join it.
//  3. otherwise create the issue.
//
// Joining = one "also seen" comment (if allowed), the hive/selfreport label,
// and an idempotent edit of the Affected-hives block in the body (the hive's
// line gains its instance, owner logins and seen/last-seen counters). The
// hive/<slug> label is retired (hivecommons/hive#12539): the seen marker
// and the Affected-hives block already name the joining hive.
func (c *Client) EnsureFleetReportWithOptions(ctx context.Context, report fleetreport.Report, opts FleetReportOptions) (FleetReportResult, error) {
	if c == nil || c.client == nil {
		return FleetReportResult{}, ErrNoGitHubClient
	}
	if strings.TrimSpace(report.Fingerprint) == "" {
		return FleetReportResult{}, fmt.Errorf("fleet report fingerprint is required")
	}
	repo := opts.repo()
	existing, err := c.findFleetReportIssueIn(ctx, repo, report.Fingerprint, false)
	if err != nil {
		return FleetReportResult{}, fmt.Errorf("searching fleet report issue: %w", err)
	}
	if existing != nil && existing.GetState() == "closed" {
		closedAt := existing.GetClosedAt().Time
		if closedAt.IsZero() {
			closedAt = opts.now()
		}
		return FleetReportResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), ClosedAt: closedAt}, nil
	}
	if existing == nil && opts.JoinExisting && report.Class != "" {
		existing, err = c.findSimilarSelfReportIssue(ctx, repo, report)
		if err != nil {
			return FleetReportResult{}, fmt.Errorf("searching similar self-report issue: %w", err)
		}
	}
	if existing != nil {
		return c.joinFleetReportIssue(ctx, repo, existing, report, opts)
	}
	if opts.CreateGate != nil {
		if err := opts.CreateGate(); err != nil {
			return FleetReportResult{}, err
		}
	}

	res, err := c.createIssue(ctx, CreateIssueRequest{Repo: repo, Title: report.Title, Body: report.Body, Labels: report.Labels, Origin: config.OriginSourceSelfReport, ConsolidateOpen: true})
	if err != nil {
		return FleetReportResult{}, err
	}
	outcome := auditOutcomeCreated
	if res.AlreadyExisted {
		outcome = auditOutcomeCommented
	}
	c.recordWriteAudit(AuditActionFleetReportPosted, hiveWriteMeta(),
		WriteTarget{Repo: repo, Number: res.Number},
		"outcome", outcome, "fingerprint", report.Fingerprint, "class", report.Class)
	return FleetReportResult{Number: res.Number, URL: res.URL, Created: !res.AlreadyExisted, Commented: res.AlreadyExisted}, nil
}

// joinFleetReportIssue adds this hive to an existing issue.
func (c *Client) joinFleetReportIssue(ctx context.Context, repo string, existing *gh.Issue, report fleetreport.Report, opts FleetReportOptions) (FleetReportResult, error) {
	owner, name := c.splitRepo(repo)
	num := existing.GetNumber()
	out := FleetReportResult{Number: num, URL: existing.GetHTMLURL(), Joined: true}
	hive := strings.TrimSpace(opts.HiveSlug)
	if hive == "" {
		hive = report.InstanceID
	}
	if opts.SeenCommentAllowed {
		// Every trigger joins with the short "also seen" comment (#13470);
		// re-posting the whole report body as a comment was the legacy
		// ACMM-shortfall behaviour and buried the thread.
		var ev fleetreport.Evidence
		if len(report.Evidence) > 0 {
			ev = report.Evidence[0]
		}
		body := fleetreport.SeenComment(report.Fingerprint, hive, opts.Version, opts.Cluster, ev, opts.now())
		if err := c.CreateIssueComment(ctx, repo, num, body); err != nil {
			return out, fmt.Errorf("commenting on fleet report: %w", err)
		}
		out.Commented = true
		if _, _, err := c.client.Reactions.CreateIssueReaction(ctx, owner, name, num, "+1"); err == nil || isAlreadyExists(err) {
			out.ReactionSent = true
		} else {
			c.logger.Warn("fleet report: could not add +1 reaction", "issue", num, "error", err)
		}
	}
	// Identity enrichment applies to every trigger (#13470): the self-report
	// label so the join-first search finds the issue, and the Affected-hives
	// block naming this hive, its owners and repos.
	if !ghIssueHasLabel(existing, fleetreport.LabelSelfReport) {
		if err := c.EnsureIssueLabel(ctx, repo, fleetreport.LabelSelfReport, "d93f0b", "Filed or joined automatically by a hive self-report"); err == nil {
			_ = c.AddLabels(ctx, repo, num, []string{fleetreport.LabelSelfReport})
		}
	}
	entry := fleetreport.AffectedHive{Hive: hive, Name: report.HiveName, URL: report.SpokeURL, Instance: report.InstanceID, Owners: report.Owners, OwnersHidden: report.OwnersHidden, Last: opts.now()}
	if newBody, changed := fleetreport.UpdateAffected(existing.GetBody(), entry, report.Repos, opts.SeenCommentAllowed); changed {
		if _, _, err := c.client.Issues.Edit(ctx, owner, name, num, &gh.IssueRequest{Body: gh.Ptr(logscrub.ScrubString(newBody))}); err != nil {
			c.logger.Warn("fleet report: could not update affected-hives block", "issue", num, "error", err)
		} else {
			out.BodyUpdated = true
		}
	}
	c.recordWriteAudit(AuditActionFleetReportPosted, hiveWriteMeta(),
		WriteTarget{Repo: repo, Number: num},
		"outcome", auditOutcomeCommented, "reaction", strconv.FormatBool(out.ReactionSent),
		"fingerprint", report.Fingerprint, "class", report.Class, "joined", "true")
	return out, nil
}

func ghIssueHasLabel(issue *gh.Issue, name string) bool {
	for _, l := range issue.Labels {
		if strings.EqualFold(l.GetName(), name) {
			return true
		}
	}
	return false
}

// findSimilarSelfReportIssue is the looser join-first search: open issues
// labelled hive/selfreport mentioning the class, then open issues whose
// title/body mention one of the class keywords. Bounded to two Search calls.
func (c *Client) findSimilarSelfReportIssue(ctx context.Context, repo string, report fleetreport.Report) (*gh.Issue, error) {
	class := logscrub.ScrubString(strings.TrimSpace(report.Class))
	if class == "" {
		return nil, nil
	}
	q := fmt.Sprintf("repo:%s is:issue is:open label:%s %q", repo, fleetreport.LabelSelfReport, class)
	if issue, err := c.firstIssueMatching(ctx, q); err != nil || issue != nil {
		return issue, err
	}
	spec := fleetreport.Classes[class]
	if len(spec.Keywords) == 0 {
		return nil, nil
	}
	var parts []string
	for _, kw := range spec.Keywords {
		parts = append(parts, fmt.Sprintf("%q", kw))
	}
	q = fmt.Sprintf("repo:%s is:issue is:open in:title,body %s", repo, strings.Join(parts, " OR "))
	return c.firstIssueMatching(ctx, q)
}

func (c *Client) firstIssueMatching(ctx context.Context, query string) (*gh.Issue, error) {
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{Sort: "created", Order: "asc", ListOptions: gh.ListOptions{PerPage: 10}})
	if err != nil {
		return nil, err
	}
	for _, issue := range result.Issues {
		if issue.GetPullRequestLinks() != nil {
			continue
		}
		return issue, nil
	}
	return nil, nil
}

// PostFleetReportRollup posts an ACMM-shortfall roll-up comment on the open
// self-report issue it folds onto (#13470). It is the same write as a
// cleared comment: one comment, no label or body edit, never a close.
func (c *Client) PostFleetReportRollup(ctx context.Context, repo string, issueNumber int, report fleetreport.Report) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	if issueNumber <= 0 {
		return fmt.Errorf("fleet report roll-up issue number is required")
	}
	if strings.TrimSpace(repo) == "" {
		repo = fleetReportRepo
	}
	if err := c.CreateIssueComment(ctx, repo, issueNumber, report.Body); err != nil {
		return err
	}
	c.recordWriteAudit(AuditActionFleetReportPosted, hiveWriteMeta(),
		WriteTarget{Repo: repo, Number: issueNumber},
		"outcome", auditOutcomeCommented, "fingerprint", report.Fingerprint, "rollup_of", report.RollupOf)
	return nil
}

// PostFleetReportCleared posts the one-time "cleared on <hive>" comment.
func (c *Client) PostFleetReportCleared(ctx context.Context, repo string, issueNumber int, body string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	if strings.TrimSpace(repo) == "" {
		repo = fleetReportRepo
	}
	return c.CreateIssueComment(ctx, repo, issueNumber, body)
}

func (c *Client) PostFleetReportRecovery(ctx context.Context, issueNumber int, report fleetreport.Report, closeIfOwner bool) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	if issueNumber <= 0 {
		return fmt.Errorf("fleet report issue number is required")
	}
	if err := c.CreateIssueComment(ctx, fleetReportRepo, issueNumber, report.Body); err != nil {
		return err
	}
	var closeErr error
	if closeIfOwner {
		_, _, closeErr = c.client.Issues.Edit(ctx, "hivecommons", "hive", issueNumber, &gh.IssueRequest{State: gh.Ptr("closed")})
	}
	// The comment landed either way, so the entry is written either way; a
	// failed close is recorded as closed=false and still returned.
	c.recordWriteAudit(AuditActionFleetReportRecovered, hiveWriteMeta(),
		WriteTarget{Repo: fleetReportRepo, Number: issueNumber},
		"closed", strconv.FormatBool(closeIfOwner && closeErr == nil),
		"fingerprint", report.Fingerprint)
	return closeErr
}

func (c *Client) findFleetReportIssue(ctx context.Context, fingerprint string) (*gh.Issue, error) {
	return c.findFleetReportIssueIn(ctx, fleetReportRepo, fingerprint, true)
}

// findFleetReportIssueIn searches repo for the fingerprint marker. openOnly
// restricts to open issues; otherwise open issues are preferred and a closed
// one is returned only when no open match exists.
func (c *Client) findFleetReportIssueIn(ctx context.Context, repo, fingerprint string, openOnly bool) (*gh.Issue, error) {
	state := " is:open"
	if !openOnly {
		state = ""
	}
	query := fmt.Sprintf("repo:%s is:issue%s %q", repo, state, "hive-fleet-fingerprint:"+logscrub.ScrubString(fingerprint))
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{Sort: "updated", Order: "desc", ListOptions: gh.ListOptions{PerPage: 10}})
	if err != nil {
		return nil, err
	}
	var closed *gh.Issue
	for _, issue := range result.Issues {
		if issue.GetPullRequestLinks() != nil {
			continue
		}
		if issue.GetState() == "closed" {
			if closed == nil {
				closed = issue
			}
			continue
		}
		return issue, nil
	}
	return closed, nil
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	if ghErr, ok := err.(*gh.ErrorResponse); ok && ghErr.Response != nil {
		return ghErr.Response.StatusCode == http.StatusUnprocessableEntity
	}
	return strings.Contains(strings.ToLower(err.Error()), "already")
}

func (c *Client) FleetReportIssue(ctx context.Context, fingerprint string) (FleetReportResult, bool, error) {
	if c == nil || c.client == nil {
		return FleetReportResult{}, false, ErrNoGitHubClient
	}
	issue, err := c.findFleetReportIssue(ctx, fingerprint)
	if err != nil {
		return FleetReportResult{}, false, err
	}
	if issue == nil {
		return FleetReportResult{}, false, nil
	}
	return FleetReportResult{Number: issue.GetNumber(), URL: issue.GetHTMLURL()}, true, nil
}
