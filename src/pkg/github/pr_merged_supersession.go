package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// MergedSupersession names the merged pull request that already delivered
// an open PR's work: the issue the open PR claims to close is closed and
// Closer merged closing it (Issue > 0), or Closer's body declares
// `Supersedes #<open PR>` (Issue == 0). It is the "already fixed" fact the
// delegated-decision ladder reads before it considers adopting a PR
// (hivecommons/hive#15289: #15219 was adopted although #15240 had already
// fixed #15202).
type MergedSupersession struct {
	Closer   int
	Issue    int
	MergeSHA string
	MergedAt time.Time
	URL      string
}

// DetectMergedSupersession reports whether pr (an open PR of repo,
// "owner/name") has been superseded by a merged PR, reusing the supersession
// sweep's detectors: the closed-issue / merged-closer rule first, then the
// declared `Supersedes #N` rule (hivecommons/hive#15208). The closer's merge
// commit is read so callers can name it. A lookup failure is returned, not
// read as "not superseded", because the caller's fallback is adoption.
func (c *Client) DetectMergedSupersession(ctx context.Context, repo string, pr *gh.PullRequest) (MergedSupersession, bool, error) {
	if c == nil || c.client == nil {
		return MergedSupersession{}, false, ErrNoGitHubClient
	}
	if pr == nil || pr.GetNumber() <= 0 {
		return MergedSupersession{}, false, nil
	}
	owner, name := c.splitRepo(repo)
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	candidate, closer, detected, err := c.detectSupersession(ctx, repo, pr, identity, time.Now())
	if err != nil {
		return MergedSupersession{}, false, err
	}
	out := MergedSupersession{}
	switch {
	case detected:
		out = MergedSupersession{Closer: closer.Number, Issue: candidate.Issue, MergedAt: closer.MergedAt, URL: closer.URL}
	default:
		declared, by, err := c.detectDeclaredSupersession(ctx, owner, name, pr)
		if err != nil {
			return MergedSupersession{}, false, fmt.Errorf("reading declared supersession for %s#%d: %w", repo, pr.GetNumber(), err)
		}
		if !declared {
			return MergedSupersession{}, false, nil
		}
		out = MergedSupersession{Closer: by.Number, MergedAt: by.MergedAt, URL: by.URL}
	}
	closerPR, _, err := c.client.PullRequests.Get(ctx, owner, name, out.Closer)
	if err != nil {
		return MergedSupersession{}, false, fmt.Errorf("reading superseding PR %s#%d: %w", repo, out.Closer, err)
	}
	if !closerPR.GetMerged() && closerPR.GetMergedAt().Time.IsZero() {
		// The reference said merged but the PR does not: do not close on
		// a stale or inconsistent read.
		return MergedSupersession{}, false, nil
	}
	out.MergeSHA = strings.TrimSpace(closerPR.GetMergeCommitSHA())
	if out.URL == "" {
		out.URL = closerPR.GetHTMLURL()
	}
	return out, true, nil
}
