package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// The `edit_pr` relay operation (hivecommons/hive#19399).
//
// A PR's title and body go stale when the PR changes after review, and the
// lane that answers the review is often not the lane that opened the PR. The
// relays had no operation for a title or body, so a lane listed under
// write_surface.enforce, or one whose direct PATCH /pulls/{n} is refused, could
// not correct a description even on a PR the hive itself opened. The
// issue-request watcher's `edit_pr` kind closes that gap with the same
// file-UID authorizer, lane allowlist, repo pause, repo scope, outbound prose
// filter and redacted audit entry as every other relay operation.
//
// Its own content gate is authorship: the relay edits only pull requests whose
// author is one of this hive's accounts (the App bot login or
// project.ai_author). Hive never edits a human-authored PR body, and "the
// author could not be read" never resolves into editing one. A merged PR is
// refused too: its description is the record of what landed. A body rewrite
// never drops a `<!-- hive:carried-fence … -->` marker the current body
// carries: the relay re-appends any the new body lacks, because that marker is
// what keeps an adopted contributor PR out of unattended merge.

// editPRShapeError returns why an edit_pr request can never succeed, or ""
// when it is well formed. The body checks are the comment relay's: a body
// that still carries a template placeholder or literal "\n" sequences is a
// broken render, not a description.
func editPRShapeError(req IssueRequest) string {
	if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || strings.TrimSpace(req.Agent) == "" {
		return "edit_pr request requires repo, number, and agent"
	}
	if strings.TrimSpace(req.Title) == "" && strings.TrimSpace(req.Body) == "" {
		return "edit_pr request requires a title or a body (an empty body cannot be written through the relay)"
	}
	for _, field := range []struct{ name, text string }{{"title", req.Title}, {"body", req.Body}} {
		if placeholder, ok := issueUnsubstitutedTemplatePlaceholder(field.text); ok {
			return "edit_pr request " + field.name + " contains unsubstituted template placeholder " + strconv.Quote(placeholder)
		}
	}
	if strings.Contains(req.Title, "\n") {
		return "edit_pr request title must be a single line"
	}
	if issueBodyHasMisEscapedNewlines(req.Body) {
		return "edit_pr request body contains literal newline escape sequences; use real newlines or --body-file"
	}
	return ""
}

// CarriedFenceMarkerPrefix opens the HTML comment an adopted contributor PR's
// App replacement carries when the adopted head tripped a diff fence (tests
// removed, workflow touched); automerge.CarriedFenceMarkerPrefix is this
// constant. The marker is what keeps that PR out of unattended merge, so the
// edit_pr relay never lets a body rewrite drop it.
const CarriedFenceMarkerPrefix = "<!-- hive:carried-fence"

// carriedFenceMarkerPattern matches one whole carried-fence marker comment.
var carriedFenceMarkerPattern = regexp.MustCompile(regexp.QuoteMeta(CarriedFenceMarkerPrefix) + `[^>]*?-->`)

// preserveCarriedFenceMarkers returns newBody with every carried-fence marker
// of currentBody that newBody lacks appended on its own line, in their
// original order. A body that already keeps them all is returned unchanged.
func preserveCarriedFenceMarkers(currentBody, newBody string) string {
	var missing []string
	for _, m := range carriedFenceMarkerPattern.FindAllString(currentBody, -1) {
		if !strings.Contains(newBody, m) && !slices.Contains(missing, m) {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return newBody
	}
	out := strings.TrimRight(newBody, "\n")
	for _, m := range missing {
		out += "\n\n" + m
	}
	return out + "\n"
}

// editPRBodyDigest is the short SHA-256 of a body written through edit_pr,
// recorded in the agent_pr_edited audit entry so an edit can be matched to
// the body it wrote without storing the text.
func editPRBodyDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}

// editPRRefusal reads the target PR and reports whether the edit_pr relay must
// refuse it, and why, along with the PR's current body (for marker
// preservation). A lookup failure is returned as err for the watcher's
// retry/quarantine classification (a 404, i.e. not a PR, is a definite
// refusal and is quarantined), never as permission to edit.
func (c *Client) editPRRefusal(ctx context.Context, repo string, number int) (currentBody, reason string, refused bool, err error) {
	if c == nil || c.client == nil {
		return "", "", false, ErrNoGitHubClient
	}
	if !c.commentOnlyGuardActive() {
		return "", "edit_pr refused: this hive has no configured identity (project.ai_author or App bot login), so it cannot tell its own pull requests from anyone else's", true, nil
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:edit_pr_relay"), owner, repoName, number)
	if err != nil {
		return "", "", false, err
	}
	if pr == nil {
		return "", fmt.Sprintf("edit_pr refused: %s#%d could not be read as a pull request", repo, number), true, nil
	}
	if !c.isHiveAccount(pr.GetUser()) {
		login := strings.TrimSpace(SafeGetLogin(pr.GetUser()))
		if login == "" {
			login = "an unidentifiable account"
		} else {
			login = "@" + login
		}
		return "", fmt.Sprintf("edit_pr refused: %s#%d was opened by %s, not by one of this hive's accounts; the hive never edits a human-authored pull request's title or body. Leave a comment instead", repo, number, login), true, nil
	}
	if pr.GetMerged() {
		return "", fmt.Sprintf("edit_pr refused: %s#%d is merged; its description is the record of what landed. Leave a comment instead", repo, number), true, nil
	}
	return pr.GetBody(), "", false, nil
}

// EditPullRequest replaces a pull request's title and/or body. A nil field is
// left as it is; at least one must be set.
func (c *Client) EditPullRequest(ctx context.Context, repo string, number int, title, body *string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	if title == nil && body == nil {
		return nil
	}
	owner, repoName := c.splitRepo(repo)
	_, _, err := c.client.PullRequests.Edit(ctx, owner, repoName, number, &gh.PullRequest{Title: title, Body: body}) // writeback:solicited edit_pr relay, hive-authored PR only (editPRRefusal)
	return err
}
