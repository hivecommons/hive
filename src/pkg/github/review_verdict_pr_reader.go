package github

import (
	"context"
	"errors"
	"strings"
)

// ReviewedPR is what the review verdict writer reads from the PR a verdict
// is about: the head the verdict binds to, the title and body its digest is
// taken from, and the author, base and labels its routing reads.
type ReviewedPR struct {
	HeadSHA string
	Title   string
	Body    string
	Author  string
	Base    string
	Labels  []string
}

// ReviewedPRReader is the forge-neutral PR read the verdict writer depends
// on, so a hive with no GitHub client can still validate, bind and record a
// verdict (hivecommons/hive#20289). The GitHub implementation reads through
// the Client's REST client; another forge supplies its own (forge.ReadAPI's
// PullView already returns every field).
type ReviewedPRReader interface {
	ReviewedPR(ctx context.Context, repo string, number int) (ReviewedPR, error)
}

// SetReviewedPRReader replaces the PR read the verdict writer uses. Nil
// restores the GitHub read. Call it before the relay starts.
func (c *Client) SetReviewedPRReader(r ReviewedPRReader) {
	c.reviewedPRReader = r
}

// verdictPRReader returns the configured PR reader, else the GitHub one when
// a REST client exists, else nil.
func (c *Client) verdictPRReader() ReviewedPRReader {
	if c == nil {
		return nil
	}
	if c.reviewedPRReader != nil {
		return c.reviewedPRReader
	}
	if c.client == nil {
		return nil
	}
	return githubReviewedPRReader{c: c}
}

// githubReviewedPRReader reads the reviewed PR through GitHub REST.
type githubReviewedPRReader struct{ c *Client }

func (r githubReviewedPRReader) ReviewedPR(ctx context.Context, repo string, number int) (ReviewedPR, error) {
	owner, name := r.c.splitRepo(strings.TrimSpace(repo))
	if owner == "" || name == "" || number <= 0 {
		return ReviewedPR{}, errors.New("reviewed PR: invalid repo or number")
	}
	pr, _, err := r.c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:review_verdict_relay"), owner, name, number)
	if err != nil {
		return ReviewedPR{}, err
	}
	if pr == nil {
		return ReviewedPR{}, errors.New("reviewed PR: empty response")
	}
	var labels []string
	for _, l := range pr.Labels {
		if l != nil {
			labels = append(labels, l.GetName())
		}
	}
	return ReviewedPR{
		HeadSHA: strings.TrimSpace(pr.GetHead().GetSHA()),
		Title:   pr.GetTitle(),
		Body:    pr.GetBody(),
		Author:  safeGetLogin(pr.GetUser()),
		Base:    strings.TrimSpace(pr.GetBase().GetRef()),
		Labels:  labels,
	}, nil
}
