package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

// Request-issue notice for derived reference repos that could not get a
// write credential (hivecommons/hive#18158).
//
// A derived entry (config.ReferenceRepo.DerivedFrom) was granted by a
// maintainer accepting a request issue in one of the hive's authorized repos.
// When the App is not installed on the requesting project's org the hive can
// still read the repos but cannot open issues or PRs there. That is the
// requesting project's call to make, not the operator's, so the hive says so
// once on the request issue instead of refusing to boot.

// DerivedReadOnlyMarkerPrefix opens the marker the notice carries. The
// complete marker names the read-only repo set, so one notice is posted per
// (issue, repo set): a reload with the same outcome finds it and posts
// nothing, a later grant that adds a repo posts a new one.
const DerivedReadOnlyMarkerPrefix = "<!-- hive:reference-repos-read-only "

// derivedReadOnlyMarkerSuffix closes the marker.
const derivedReadOnlyMarkerSuffix = " -->"

// derivedReadOnlyRepoSeparator joins the repo slugs inside the marker.
const derivedReadOnlyRepoSeparator = ","

// DerivedReadOnlyMarker is the full marker for a read-only repo set, with
// the slugs case-folded and sorted so the set compares as a set.
func DerivedReadOnlyMarker(repos []string) string {
	slugs := make([]string, 0, len(repos))
	seen := make(map[string]bool, len(repos))
	for _, r := range repos {
		slug := strings.ToLower(strings.TrimSpace(r))
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return DerivedReadOnlyMarkerPrefix + "repos=" + strings.Join(slugs, derivedReadOnlyRepoSeparator) +
		" state=" + config.ReferenceCredentialReadOnly + derivedReadOnlyMarkerSuffix
}

// DerivedReadOnlyComment renders the notice body for the read-only entries
// derived from one request issue. It lists each repo with its reason and
// says what the hive can and cannot do; it names no accounts, so nobody is
// notified beyond the issue's subscribers.
func DerivedReadOnlyComment(readOnly []config.ReferenceRepo) string {
	if len(readOnly) == 0 {
		return ""
	}
	repos := make([]string, 0, len(readOnly))
	for _, rr := range readOnly {
		repos = append(repos, rr.Repo)
	}
	var b strings.Builder
	b.WriteString(DerivedReadOnlyMarker(repos))
	b.WriteString("\n")
	if len(readOnly) == 1 {
		b.WriteString("This request grants the hive access to one repository it can read but not write:\n")
	} else {
		b.WriteString("This request grants the hive access to repositories it can read but not write:\n")
	}
	for _, rr := range readOnly {
		reason := strings.TrimSpace(rr.CredentialReason)
		if reason == "" {
			reason = DerivedReadOnlyReason(rr)
		}
		fmt.Fprintf(&b, "- `%s`: %s\n", rr.Repo, reason)
	}
	b.WriteString("\nThe hive will study these repositories (clone, read files, read issues and PRs) and keep its findings in the assessment deliverable here. ")
	b.WriteString("It will not open issues or pull requests on them. ")
	b.WriteString("To let it, an owner of that organization installs the same GitHub App there; the hive picks the installation up on its next config reload without a restart.")
	return AppendTrailer(b.String(), hiveWriteMeta())
}

// CommentDerivedReadOnly posts the read-only notice for one request issue
// (issueRepo#number, an authorized repo of this hive) once per repo set:
// when a comment of the hive's carrying the same marker exists nothing is
// posted and posted is false. readOnly are the derived entries from that
// issue whose CredentialState is read-only; an empty list posts nothing.
func (c *Client) CommentDerivedReadOnly(ctx context.Context, issueRepo string, number int, readOnly []config.ReferenceRepo) (posted bool, err error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	if len(readOnly) == 0 {
		return false, nil
	}
	repos := make([]string, 0, len(readOnly))
	for _, rr := range readOnly {
		repos = append(repos, rr.Repo)
	}
	marker := DerivedReadOnlyMarker(repos)
	res, err := c.EditIssueMarkerComment(ctx, issueRepo, number, marker, func(previous string) string {
		if previous != "" {
			// Same repo set already noticed: leave the existing comment alone.
			return previous
		}
		return DerivedReadOnlyComment(readOnly)
	})
	if err != nil {
		return false, fmt.Errorf("derived reference repos read-only notice on %s#%d: %w", issueRepo, number, err)
	}
	if res.Created {
		c.recordWriteAudit(AuditActionReferenceRepoReadOnlyNoticed, hiveWriteMeta(),
			WriteTarget{Repo: c.qualifiedRepo(issueRepo), Number: number}, "repos", strings.Join(repos, derivedReadOnlyRepoSeparator))
	}
	return res.Created, nil
}

// Request-issue discovery for derived reference repos (hivecommons/hive#18159).
//
// The hive derives reference repos from open issues in its authorized repos
// that carry refderive.GrantLabel. Listing is one call per repo; the
// accepter (who applied the label, and with what permission) costs a
// timeline read and a permission lookup per issue, so the caller caches
// those by (issue, updated_at) and only asks again when the issue moved.

// RequestIssueCandidate is one open issue carrying a grant label, as listed.
// UpdatedAt is GitHub's last-activity timestamp: the caller's cache key.
type RequestIssueCandidate struct {
	Repo      string
	Number    int
	Title     string
	Body      string
	Closed    bool
	Labels    []string
	UpdatedAt time.Time
}

// RequestIssueAccepter is who applied the grant label to a request issue,
// read from the latest "labeled" timeline event for that label.
type RequestIssueAccepter struct {
	// Login is the applier; empty when the timeline names no labeled event
	// for the label (the label was there before the timeline window, or the
	// event carries no actor).
	Login string
	// IsBot reports a bot or GitHub App account.
	IsBot bool
	// Permission is Login's permission on the issue's repo as GitHub reports
	// it ("admin", "maintain", "write", "triage", "read", "none"); empty when
	// Login is empty.
	Permission string
}

// ListRequestIssueCandidates lists the open issues (never pull requests) in
// repo ("owner/name" or bare) that carry label, newest activity first, at
// most limit of them. limit <= 0 means one page.
func (c *Client) ListRequestIssueCandidates(ctx context.Context, repo, label string, limit int) ([]RequestIssueCandidate, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	full := owner + "/" + name
	perPage := requestIssueListPageSize
	if limit > 0 && limit < perPage {
		perPage = limit
	}
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{label},
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	var out []RequestIssueCandidate
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, name, opts)
		if err != nil {
			return nil, fmt.Errorf("listing %s issues labelled %q: %w", full, label, err)
		}
		for _, is := range issues {
			if is == nil || is.IsPullRequest() {
				continue
			}
			cand := RequestIssueCandidate{
				Repo:   full,
				Number: is.GetNumber(),
				Title:  is.GetTitle(),
				Body:   is.GetBody(),
				Closed: !strings.EqualFold(is.GetState(), "open"),
				Labels: labelNames(is.Labels),
			}
			if is.UpdatedAt != nil {
				cand.UpdatedAt = is.UpdatedAt.Time
			}
			out = append(out, cand)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
		if limit <= 0 || resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

// requestIssueListPageSize is GitHub's largest issue page.
const requestIssueListPageSize = 100

// RequestIssueAccepterFor reads who applied label to repo#number from the
// issue's timeline (the latest "labeled" event for that label wins) and that
// login's permission on repo. An issue whose timeline shows no such event
// yields an empty Login and no error: the caller treats it as an
// unattributed grant.
func (c *Client) RequestIssueAccepterFor(ctx context.Context, repo string, number int, label string) (RequestIssueAccepter, error) {
	if c == nil || c.client == nil {
		return RequestIssueAccepter{}, ErrNoGitHubClient
	}
	events, err := c.IssueTimelineSince(ctx, repo, number, time.Time{})
	if err != nil {
		return RequestIssueAccepter{}, fmt.Errorf("reading %s#%d timeline: %w", c.qualifiedRepo(repo), number, err)
	}
	var acc RequestIssueAccepter
	for _, ev := range events {
		if ev.Event != timelineEventLabeled || !strings.EqualFold(strings.TrimSpace(ev.Label), label) {
			continue
		}
		// Oldest first: the last match is the latest application.
		acc = RequestIssueAccepter{Login: strings.TrimSpace(ev.Actor), IsBot: ev.ActorIsBot}
	}
	if acc.Login == "" {
		return acc, nil
	}
	owner, name := c.splitRepo(repo)
	perm, err := c.RepoPermission(ctx, owner, name, acc.Login)
	if err != nil {
		return RequestIssueAccepter{}, err
	}
	acc.Permission = perm
	return acc, nil
}

// timelineEventLabeled is GitHub's timeline event name for a label applied.
const timelineEventLabeled = "labeled"

// RepoPermission returns login's permission level on owner/repo as GitHub
// reports it: "admin", "maintain", "write", "triage", "read" or "none". It
// is the same call reporterHasWriteAccess makes, exported for callers that
// need the level rather than a verdict.
func (c *Client) RepoPermission(ctx context.Context, owner, repo, login string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	login = strings.TrimSpace(login)
	if login == "" {
		return "", fmt.Errorf("repo permission on %s/%s: empty login", owner, repo)
	}
	perm, _, err := c.client.Repositories.GetPermissionLevel(ctx, owner, repo, login)
	if err != nil {
		return "", fmt.Errorf("reading %s's permission on %s/%s: %w", login, owner, repo, err)
	}
	return strings.ToLower(strings.TrimSpace(perm.GetPermission())), nil
}
