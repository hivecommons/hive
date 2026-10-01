package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Parent-inherited acknowledgement (hivecommons/hive#9840).
//
// When an agent splits a human-filed issue, the children are filed by the
// hive and linked as GitHub sub-issues of the parent (#9435). Without this
// file they rank in the lowest kick tier and their PRs are held by the #5117
// self-authorization gate until a human acknowledges EACH child — although
// the human already asked for the work by filing the parent.
//
// A hive-filed child inherits the parent's acknowledgement when ALL of:
//
//   - the child itself is not human-filed and has no acknowledgement of its
//     own (a direct acknowledgement always wins and is reported as such);
//   - GitHub reports a parent in the SAME repository (parent_issue_url);
//   - the parent is open;
//   - the parent is not held (a hold on the parent is the maintainer saying
//     "not now" to the whole split);
//   - the parent is human-filed, or carries the approval label / a human
//     assignee (enumeration) or a human comment (the #5117 gate).
//
// Inheritance is one level deep by construction: the parent's own
// acknowledgement is read from the parent's author, labels, assignees and
// comments, never from the parent's parent. A grandchild therefore needs a
// human-filed or acknowledged child above it.
//
// go-github v72 does not model sub-issue relations, so the open-issue listing
// is decoded raw once (one request per page, exactly as before) and the
// parent_issue_url is read from the same payload — no per-issue call.

// issuesPerPage matches the page size the typed listing used.
const issuesPerPage = 100

// issueParentRelation is the slice of an issue payload that sub-issue
// support adds and go-github v72 drops on decode.
type issueParentRelation struct {
	ParentIssueURL string `json:"parent_issue_url"`
}

// listOpenIssuesWithParents lists every open issue (and PR, as the issues
// endpoint does) of owner/repoName, returning the typed issues plus a map of
// child issue number → parent issue number for children whose parent lives
// in the same repository.
func (c *Client) listOpenIssuesWithParents(ctx context.Context, owner, repoName string) ([]*gh.Issue, map[int]int, error) {
	var all []*gh.Issue
	parents := map[int]int{}
	for page := 1; ; {
		path := fmt.Sprintf("repos/%s/%s/issues?state=open&per_page=%d&page=%d", owner, repoName, issuesPerPage, page)
		req, err := c.client.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			return nil, nil, err
		}
		var raws []json.RawMessage
		resp, err := c.client.Do(ctx, req, &raws)
		if err != nil {
			return nil, nil, err
		}
		for _, raw := range raws {
			issue := new(gh.Issue)
			if err := json.Unmarshal(raw, issue); err != nil {
				return nil, nil, fmt.Errorf("decoding issue payload: %w", err)
			}
			all = append(all, issue)
			var rel issueParentRelation
			if json.Unmarshal(raw, &rel) == nil {
				if parent := parentIssueNumberFromURL(rel.ParentIssueURL, owner, repoName); parent > 0 {
					parents[issue.GetNumber()] = parent
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return all, parents, nil
}

// parentIssueNumberFromURL extracts the issue number from an API URL of the
// form .../repos/{owner}/{repo}/issues/{number}. It returns 0 for an empty
// URL, a malformed one, or a parent in a different repository — cross-repo
// parents do not confer acknowledgement.
func parentIssueNumberFromURL(raw, owner, repoName string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	idx := strings.Index(raw, "/repos/")
	if idx < 0 {
		return 0
	}
	parts := strings.Split(strings.Trim(raw[idx+len("/repos/"):], "/"), "/")
	if len(parts) != 4 || parts[2] != "issues" {
		return 0
	}
	if !strings.EqualFold(parts[0], owner) || !strings.EqualFold(parts[1], repoName) {
		return 0
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// parentConfersAcknowledgement reports whether parent is an open, unheld,
// same-repo issue that a human filed or cheaply acknowledged (label or
// assignee). It is the enumeration-time half; the #5117 gate additionally
// scans the parent's comments.
func (c *Client) parentConfersAcknowledgement(parent *gh.Issue) bool {
	if parent == nil || parent.IsPullRequest() {
		return false
	}
	if state := parent.GetState(); state != "" && !strings.EqualFold(state, "open") {
		return false
	}
	if c.isHeld(extractLabels(parent.Labels)) {
		return false
	}
	return c.isHumanAuthor(parent.GetUser()) || c.issueHasCheapHumanAcknowledgement(parent)
}

// annotateInheritedAcknowledgement sets HumanAcknowledged and AckParent on
// every actionable hive-filed issue whose same-repo parent (per parents)
// confers acknowledgement. open is the enumeration snapshot keyed by issue
// number; a parent missing from it is closed (or not an issue) and confers
// nothing.
func (c *Client) annotateInheritedAcknowledgement(actionable []Issue, open map[int]*gh.Issue, parents map[int]int) {
	if len(parents) == 0 {
		return
	}
	for i := range actionable {
		issue := &actionable[i]
		if issue.AuthorIsHuman || issue.HumanAcknowledged {
			continue
		}
		parentNumber := parents[issue.Number]
		if parentNumber <= 0 || parentNumber == issue.Number {
			continue
		}
		if !c.parentConfersAcknowledgement(open[parentNumber]) {
			continue
		}
		issue.HumanAcknowledged = true
		issue.AckParent = parentNumber
	}
}

// fetchParentIssue reads GET /repos/{owner}/{repo}/issues/{number}/parent.
// It returns (nil, nil) when the issue has no parent (GitHub answers 404)
// and the parent payload otherwise. go-github v72 has no typed wrapper.
func (c *Client) fetchParentIssue(ctx context.Context, owner, repoName string, number int) (*gh.Issue, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	path := fmt.Sprintf("repos/%s/%s/issues/%d/parent", owner, repoName, number)
	req, err := c.client.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	parent := new(gh.Issue)
	if _, err := c.client.Do(ctx, req, parent); err != nil {
		if githubStatusError(err, http.StatusNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return parent, nil
}

// parentIssueAcknowledges is the #5117 gate's half of inheritance: it
// resolves the issue's parent with one API call and reports whether that
// parent, in the same repository, is open, unheld and either human-filed or
// directly acknowledged (label, human assignee, or human comment).
func (c *Client) parentIssueAcknowledges(ctx context.Context, owner, repoName string, issue *gh.Issue) (bool, error) {
	parent, err := c.fetchParentIssue(ctx, owner, repoName, issue.GetNumber())
	if err != nil || parent == nil {
		return false, err
	}
	if parentIssueNumberFromURL(parent.GetURL(), owner, repoName) == 0 {
		// Cross-repo parent (or a payload without a URL): its number cannot
		// be used against owner/repoName, and cross-repo parents confer
		// nothing anyway.
		return false, nil
	}
	if parent.GetNumber() == issue.GetNumber() {
		return false, nil
	}
	if !parent.IsPullRequest() {
		if state := parent.GetState(); state != "" && !strings.EqualFold(state, "open") {
			return false, nil
		}
		if c.isHeld(extractLabels(parent.Labels)) {
			return false, nil
		}
		if c.isHumanAuthor(parent.GetUser()) {
			return true, nil
		}
		return c.issueHasDirectHumanAcknowledgement(ctx, owner, repoName, parent)
	}
	return false, nil
}
