// Package scheduler is the fixture for UntrustedGitHubTextReachesKick.ql.
// A line ending in `// $ Alert` must be reported; every other line must not.
// .github/codeql/check-fixture-alerts.py compares the two.
package scheduler

import (
	"fmt"
	"strings"

	"hivefixture/pkg/github"
	"hivefixture/pkg/ioscan"
)

type KickMessage struct {
	Agent   string
	Message string
}

type Scheduler struct{ enabled bool }

// enforceTextFrom mirrors the real wrapper, including its disabled
// passthrough: the wrapper is the sanctioned gate, so the query trusts it.
func (s *Scheduler) enforceTextFrom(text string, src ioscan.Source) (string, ioscan.Verdict) {
	if !s.enabled {
		return text, ioscan.Verdict{}
	}
	return ioscan.EnforceInputFrom(text, src, ioscan.Policy{})
}

func RawTitle(issue github.Issue) KickMessage {
	return KickMessage{Agent: "fixer", Message: "Work on: " + issue.Title} // $ Alert
}

// The body travels through a helper and a builder before it lands.
func excerpt(text string) string { return strings.TrimSpace(text) }

func RawBodyViaBuilder(issue github.Issue) KickMessage {
	var b strings.Builder
	b.WriteString("Context:\n")
	b.WriteString(excerpt(issue.Body))
	return KickMessage{Agent: "fixer", Message: b.String()} // $ Alert
}

func buildPRLine(pr github.PullRequest) string {
	return fmt.Sprintf("#%d %s", pr.Number, pr.Title)
}

// Cross-function: the tainted line is built in one function and stored in
// another, as BuildKickMessagesWithSkips stores buildAgentMessageFor's result.
func RawPRTitleAssigned(pr github.PullRequest) KickMessage {
	var km KickMessage
	km.Agent = "reviewer"
	km.Message = buildPRLine(pr) // $ Alert
	return km
}

func RawComment(c github.ThreadComment) KickMessage {
	return KickMessage{Message: "> " + c.Body} // $ Alert
}

// Scanned through the scheduler wrapper.
func (s *Scheduler) ScannedTitle(issue github.Issue) KickMessage {
	title, _ := s.enforceTextFrom(issue.Title, ioscan.Source{Surface: "issue_title"})
	return KickMessage{Agent: "fixer", Message: "Work on: " + title}
}

// Scanned directly.
func ScannedBody(issue github.Issue) KickMessage {
	body, _ := ioscan.EnforceInputFrom(issue.Body, ioscan.Source{Surface: "issue_body"}, ioscan.Policy{})
	return KickMessage{Agent: "fixer", Message: body}
}

// A number and a repo name are not free text.
func RefOnly(issue github.Issue) KickMessage {
	return KickMessage{Message: fmt.Sprintf("%s#%d", issue.Repo, issue.Number)}
}

// A comment author is a login, not text the query tracks.
func AuthorOnly(c github.ThreadComment) KickMessage {
	return KickMessage{Message: "from @" + c.Author}
}
