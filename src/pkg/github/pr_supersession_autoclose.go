package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Auto-close of superseded PRs (hivecommons/hive#11418). The supersession
// sweep already notices when a PR's claimed issue was closed by a different
// merged PR; this file decides when such a PR can be closed instead of left
// open, and performs the close for human-authored PRs after a grace window.
const (
	// SupersededLabel marks a PR the supersession sweep closed because a
	// different merged PR already landed the same change.
	SupersededLabel = "hive/superseded"
	// SupersessionKeepOpenLabel marks a superseded PR an operator chose to
	// keep open (the dashboard Review Queue's keep-open action,
	// hivecommons/hive#11430). The sweep never auto-closes a PR carrying it.
	SupersessionKeepOpenLabel = "hive/keep-open"
	// DefaultSupersessionGracePeriod is how long a human-authored PR stays
	// open after the supersession notice before the sweep may close it.
	DefaultSupersessionGracePeriod = 24 * time.Hour
	// supersessionMaxAutoCloseChangedLines mirrors the prow size/M upper
	// bound: PRs at size/L and above are never closed automatically.
	supersessionMaxAutoCloseChangedLines = 100
	supersessionAutoCloseMarker          = "<!-- hive:pr-supersession-autoclose -->"
	supersededLabelColor                 = "cfd3d7"
	supersededLabelDescription           = "Closed by Hive: a different merged PR already landed this change"
	keepOpenLabelColor                   = "0e8a16"
	keepOpenLabelDescription             = "An operator kept this PR open: the supersession sweep will not close it"
)

// SupersessionAutoCloseFacts are the observations DecideSupersessionAutoClose
// needs. The caller has already established that the PR's claimed issue was
// closed by a different, merged PR.
type SupersessionAutoCloseFacts struct {
	// KeptOpen is true when the PR carries SupersessionKeepOpenLabel.
	KeptOpen bool
	// HiveAuthored PRs skip the grace window and the human-reply check.
	HiveAuthored bool
	// OpenClaimRemaining is true when the PR also claims an issue that is
	// still open.
	OpenClaimRemaining bool
	// FilesSubset is true when every file the PR touches was also touched by
	// the closing PR.
	FilesSubset bool
	// SizeLabel is the PR's size/* label, if any.
	SizeLabel string
	// ChangedLines is additions+deletions; consulted only when SizeLabel is
	// empty. Negative means unknown.
	ChangedLines int
	// ThreadsUnknown is true when review threads could not be read.
	ThreadsUnknown bool
	// UnresolvedAuthorThreads is true when an unresolved review thread
	// contains a comment from the PR author.
	UnresolvedAuthorThreads bool
	// HumanActivityAfterNotice is true when a human (including the author)
	// commented after the supersession notice was first posted.
	HumanActivityAfterNotice bool
	// NoticeAt is when the current grace-window notice was posted; zero
	// when it has not been posted yet.
	NoticeAt    time.Time
	Now         time.Time
	GracePeriod time.Duration
}

// SupersessionAutoCloseVerdict is the outcome of DecideSupersessionAutoClose.
type SupersessionAutoCloseVerdict string

const (
	// SupersessionKeep: a guard failed; leave the PR open for a human.
	SupersessionKeep SupersessionAutoCloseVerdict = "keep"
	// SupersessionWaitGrace: eligible, but the grace window has not elapsed
	// (or has not started because the notice is not posted yet).
	SupersessionWaitGrace SupersessionAutoCloseVerdict = "wait-grace"
	// SupersessionClose: close the PR now.
	SupersessionClose SupersessionAutoCloseVerdict = "close"
)

// SupersessionAutoCloseDecision carries the verdict and a human-readable reason.
type SupersessionAutoCloseDecision struct {
	Verdict SupersessionAutoCloseVerdict
	Reason  string
}

// DecideSupersessionAutoClose is the pure decision for closing a superseded
// PR. It is deliberately conservative: any doubt keeps the PR open.
func DecideSupersessionAutoClose(f SupersessionAutoCloseFacts) SupersessionAutoCloseDecision {
	keep := func(reason string) SupersessionAutoCloseDecision {
		return SupersessionAutoCloseDecision{Verdict: SupersessionKeep, Reason: reason}
	}
	if f.KeptOpen {
		return keep("an operator chose to keep this PR open")
	}
	if f.OpenClaimRemaining {
		return keep("this PR also claims another issue that is still open")
	}
	if !f.FilesSubset {
		return keep("this PR touches files the merged PR did not")
	}
	if ok, why := supersessionSizeAllowsAutoClose(f.SizeLabel, f.ChangedLines); !ok {
		return keep(why)
	}
	if f.HiveAuthored {
		return SupersessionAutoCloseDecision{Verdict: SupersessionClose, Reason: "hive-authored PR superseded by a merged PR"}
	}
	if f.ThreadsUnknown {
		return keep("review threads could not be read")
	}
	if f.UnresolvedAuthorThreads {
		return keep("the author has unresolved review threads on this PR")
	}
	if f.HumanActivityAfterNotice {
		return keep("a human commented after the supersession notice")
	}
	grace := f.GracePeriod
	if grace <= 0 {
		grace = DefaultSupersessionGracePeriod
	}
	if f.NoticeAt.IsZero() {
		return SupersessionAutoCloseDecision{Verdict: SupersessionWaitGrace, Reason: "grace window starts with the notice"}
	}
	if f.Now.Sub(f.NoticeAt) < grace {
		return SupersessionAutoCloseDecision{Verdict: SupersessionWaitGrace, Reason: "grace window has not elapsed"}
	}
	return SupersessionAutoCloseDecision{Verdict: SupersessionClose, Reason: "grace window elapsed with no human reply"}
}

func supersessionSizeAllowsAutoClose(sizeLabel string, changedLines int) (bool, string) {
	switch strings.ToLower(strings.TrimSpace(sizeLabel)) {
	case "size/xs", "size/s", "size/m":
		return true, ""
	case "":
	default:
		return false, fmt.Sprintf("this PR is %s; only size/XS–size/M PRs are closed automatically", sizeLabel)
	}
	if changedLines < 0 {
		return false, "this PR's size is unknown"
	}
	if changedLines >= supersessionMaxAutoCloseChangedLines {
		return false, fmt.Sprintf("this PR changes %d lines; only size/XS–size/M PRs are closed automatically", changedLines)
	}
	return true, ""
}

func supersessionHasLabel(pr *gh.PullRequest, name string) bool {
	for _, l := range pr.Labels {
		if strings.EqualFold(l.GetName(), name) {
			return true
		}
	}
	return false
}

func supersessionSizeLabel(pr *gh.PullRequest) string {
	for _, l := range pr.Labels {
		if name := l.GetName(); strings.HasPrefix(strings.ToLower(name), "size/") {
			return name
		}
	}
	return ""
}

func supersessionIsHuman(login, userType string, identity HiveIdentity) bool {
	if login == "" || identity.Matches(login) {
		return false
	}
	if strings.EqualFold(userType, "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	return true
}

func renderSupersessionGraceNotice(grace time.Duration) string {
	if grace <= 0 {
		grace = DefaultSupersessionGracePeriod
	}
	return fmt.Sprintf("A different merged PR closed the claimed issue and every file this PR touches was also changed by it. "+
		"Unless someone comments here, the sweep will close this PR as superseded once %s have passed since this notice. "+
		"Reply on this PR to keep it open.", formatSupersessionGrace(grace))
}

func formatSupersessionGrace(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.String()
}

func renderSupersessionCloseComment(closer supersessionCloser, grace time.Duration) string {
	var b strings.Builder
	fmt.Fprintln(&b, supersessionAutoCloseMarker)
	ref := fmt.Sprintf("%s#%d", closer.Repo, closer.Number)
	if closer.URL != "" {
		ref = fmt.Sprintf("[#%d](%s)", closer.Number, closer.URL)
	}
	merged := "which has merged"
	if !closer.MergedAt.IsZero() {
		merged = "which merged at " + closer.MergedAt.UTC().Format("2006-01-02 15:04Z")
	}
	fmt.Fprintf(&b, "Closing as superseded by %s, %s. Every file this PR touches was also changed there and nobody replied within the %s grace window, so nothing from this branch looks still needed.\n",
		ref, merged, formatSupersessionGrace(grace))
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Reopen if that's wrong. The `%s` label marks PRs the supersession sweep closed.\n", SupersededLabel)
	return b.String()
}

type supersessionCommentScan struct {
	notice         *gh.IssueComment
	closeCommented bool
}

func (c *Client) scanSupersessionComments(ctx context.Context, owner, repo string, number int) (supersessionCommentScan, []*gh.IssueComment, error) {
	var scan supersessionCommentScan
	var all []*gh.IssueComment
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; page < claimSearchMaxPages; page++ {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return scan, nil, fmt.Errorf("listing comments on %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, cm := range comments {
			if cm == nil {
				continue
			}
			all = append(all, cm)
			body := cm.GetBody()
			if scan.notice == nil && strings.Contains(body, supersessionSweepMarker) {
				scan.notice = cm
			}
			if strings.Contains(body, supersessionAutoCloseMarker) {
				scan.closeCommented = true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return scan, all, nil
}

type supersessionThreadFacts struct {
	unresolvedAuthor bool
	humanAfter       bool
}

func (c *Client) supersessionReviewThreads(ctx context.Context, owner, repo string, number int, author string, since time.Time, identity HiveIdentity) (supersessionThreadFacts, error) {
	payload := map[string]any{
		"query": `query($owner:String!,$repo:String!,$number:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100){nodes{isResolved comments(first:50){nodes{createdAt author{__typename login}}}}}}}}`,
		"variables": map[string]any{
			"owner":  owner,
			"repo":   repo,
			"number": number,
		},
	}
	req, err := c.client.NewRequest("POST", "graphql", payload)
	if err != nil {
		return supersessionThreadFacts{}, err
	}
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									CreatedAt time.Time `json:"createdAt"`
									Author    struct {
										Typename string `json:"__typename"`
										Login    string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := c.client.Do(ctx, req, &resp); err != nil {
		return supersessionThreadFacts{}, fmt.Errorf("reading review threads on %s/%s#%d: %w", owner, repo, number, err)
	}
	if len(resp.Errors) > 0 || resp.Data.Repository.PullRequest == nil {
		return supersessionThreadFacts{}, fmt.Errorf("reading review threads on %s/%s#%d: incomplete GraphQL response", owner, repo, number)
	}
	var out supersessionThreadFacts
	for _, thread := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
		for _, cm := range thread.Comments.Nodes {
			if !thread.IsResolved && author != "" && strings.EqualFold(cm.Author.Login, author) {
				out.unresolvedAuthor = true
			}
			if !since.IsZero() && cm.CreatedAt.After(since) && supersessionIsHuman(cm.Author.Login, cm.Author.Typename, identity) {
				out.humanAfter = true
			}
		}
	}
	return out, nil
}

// trySupersessionAutoCloseContributorPR runs the grace-window path for a
// human-authored PR whose claimed issue was closed by closer and whose files
// are a subset of closer's. The grace window runs from the notice's last
// edit (so a reworded notice restarts it); a human reply is detected from the
// notice's creation, so a later re-edit can never hide an earlier reply.
func (c *Client) trySupersessionAutoCloseContributorPR(ctx context.Context, owner, repo string, pr *gh.PullRequest, event SupersessionSweepEvent, closer supersessionCloser, identity HiveIdentity, now time.Time, grace time.Duration) (SupersessionSweepEvent, string, error) {
	if grace <= 0 {
		grace = DefaultSupersessionGracePeriod
	}
	number := pr.GetNumber()
	scan, comments, err := c.scanSupersessionComments(ctx, owner, repo, number)
	if err != nil {
		return event, "", err
	}
	graceBody := renderSupersessionComment(event, closer, renderSupersessionGraceNotice(grace))

	facts := SupersessionAutoCloseFacts{
		KeptOpen:     supersessionHasLabel(pr, SupersessionKeepOpenLabel),
		FilesSubset:  true,
		SizeLabel:    supersessionSizeLabel(pr),
		ChangedLines: -1,
		Now:          now,
		GracePeriod:  grace,
	}
	if facts.SizeLabel == "" {
		full, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
		if err != nil {
			return event, "", fmt.Errorf("reading superseded PR %s/%s#%d: %w", owner, repo, number, err)
		}
		facts.ChangedLines = full.GetAdditions() + full.GetDeletions()
	}
	var noticeCreated time.Time
	if scan.notice != nil {
		noticeCreated = scan.notice.GetCreatedAt().Time
		if scan.notice.GetBody() == graceBody {
			facts.NoticeAt = scan.notice.GetUpdatedAt().Time
			if facts.NoticeAt.IsZero() {
				facts.NoticeAt = noticeCreated
			}
		}
		for _, cm := range comments {
			if cm.GetCreatedAt().Time.After(noticeCreated) && supersessionIsHuman(safeGetLogin(cm.GetUser()), cm.GetUser().GetType(), identity) {
				facts.HumanActivityAfterNotice = true
				break
			}
		}
	}
	threads, err := c.supersessionReviewThreads(ctx, owner, repo, number, event.Author, noticeCreated, identity)
	if err != nil {
		c.logger.Warn("supersession sweep: review threads unreadable; keeping PR open", "repo", owner+"/"+repo, "pr", number, "error", err)
		facts.ThreadsUnknown = true
	}
	facts.UnresolvedAuthorThreads = threads.unresolvedAuthor
	facts.HumanActivityAfterNotice = facts.HumanActivityAfterNotice || threads.humanAfter

	decision := DecideSupersessionAutoClose(facts)
	switch decision.Verdict {
	case SupersessionClose:
		event.Action = "closed-contributor"
		if !scan.closeCommented {
			body := renderSupersessionCloseComment(closer, grace)
			if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: &body}); err != nil {
				return event, "", fmt.Errorf("commenting on superseded PR %s/%s#%d: %w", owner, repo, number, err)
			}
		}
		if err := c.labelAndCloseSupersededPR(ctx, owner, repo, number); err != nil {
			return event, "", err
		}
		return event, "closed", nil
	case SupersessionWaitGrace:
		event.Action = "commented-grace"
		if err := c.ensureSupersessionComment(ctx, owner, repo, number, graceBody); err != nil {
			return event, "", err
		}
		return event, "commented", nil
	default:
		event.Action = "commented-contributor"
		body := renderSupersessionComment(event, closer, "A different merged PR closed the claimed issue. Leaving this contributor PR open for a human to review: "+decision.Reason+".")
		if err := c.ensureSupersessionComment(ctx, owner, repo, number, body); err != nil {
			return event, "", err
		}
		return event, "commented", nil
	}
}

// SupersessionAutoCloseMarker tags the one close comment on a superseded PR.
const SupersessionAutoCloseMarker = supersessionAutoCloseMarker

// RenderSupersessionOperatorCloseComment is the close comment for a PR an
// operator closed from the dashboard Review Queue before the grace window
// ended. It carries the auto-close marker so the sweep never posts its own.
func RenderSupersessionOperatorCloseComment(closerRef, user string) string {
	var b strings.Builder
	fmt.Fprintln(&b, supersessionAutoCloseMarker)
	by := "an operator"
	if user != "" {
		by = "@" + user
	}
	fmt.Fprintf(&b, "Closing as superseded by %s: %s confirmed the close from the Hive dashboard before the grace window ended.\n", closerRef, by)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Reopen if that's wrong. The `%s` label marks PRs the supersession sweep closed.\n", SupersededLabel)
	return b.String()
}

// CloseSupersededPR labels a superseded PR with SupersededLabel and closes
// it: the same path the sweep's auto-close takes. repo is owner/name.
func (c *Client) CloseSupersededPR(ctx context.Context, repo string, number int) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	return c.labelAndCloseSupersededPR(ctx, owner, name, number)
}

// KeepSupersededPROpen applies SupersessionKeepOpenLabel so the sweep stops
// counting down the grace window for this PR. repo is owner/name.
func (c *Client) KeepSupersededPROpen(ctx context.Context, repo string, number int) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	if err := c.EnsureIssueLabel(ctx, repo, SupersessionKeepOpenLabel, keepOpenLabelColor, keepOpenLabelDescription); err != nil {
		c.logger.Warn("supersession keep-open: could not ensure label", "label", SupersessionKeepOpenLabel, "repo", repo, "error", err)
	}
	return c.AddLabels(ctx, repo, number, []string{SupersessionKeepOpenLabel})
}

// labelAndCloseSupersededPR applies SupersededLabel and closes the PR. A
// failure to pre-create the label is not fatal: adding a missing label
// creates it.
func (c *Client) labelAndCloseSupersededPR(ctx context.Context, owner, repo string, number int) error {
	if err := c.EnsureIssueLabel(ctx, owner+"/"+repo, SupersededLabel, supersededLabelColor, supersededLabelDescription); err != nil {
		c.logger.Warn("supersession sweep: could not ensure label", "label", SupersededLabel, "repo", owner+"/"+repo, "error", err)
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{SupersededLabel}); err != nil {
		return fmt.Errorf("labeling superseded PR %s/%s#%d: %w", owner, repo, number, err)
	}
	closed := "closed"
	if _, _, err := c.client.Issues.Edit(ctx, owner, repo, number, &gh.IssueRequest{State: &closed}); err != nil {
		return fmt.Errorf("closing superseded PR %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}
