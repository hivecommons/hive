package github

import (
	"strings"

	gh "github.com/google/go-github/v72/github"
)

func (c *Client) isTrustedAppBotCommentAuthor(comment *gh.IssueComment) bool {
	if c == nil || comment == nil || strings.TrimSpace(c.appBotLogin) == "" {
		return false
	}
	return strings.EqualFold(safeGetLogin(comment.GetUser()), c.appBotLogin)
}
