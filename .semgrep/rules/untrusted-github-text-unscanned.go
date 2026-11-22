// Fixture for hive-untrusted-github-text-unscanned. Not compiled: semgrep
// --test parses it and checks each ruleid/ok annotation against the line
// below it.
package scheduler

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
)

func (s *Scheduler) rawTitle(b *strings.Builder, issue github.Issue) {
	// ruleid: hive-untrusted-github-text-unscanned
	b.WriteString(issue.Title)
}

func (s *Scheduler) rawBodyFormatted(b *strings.Builder, issue github.Issue) {
	excerpt := strings.TrimSpace(issue.Body)
	// ruleid: hive-untrusted-github-text-unscanned
	fmt.Fprintf(b, "  context: %s\n", excerpt)
}

func (s *Scheduler) rawPRTitle(b *strings.Builder, pr github.PullRequest) {
	line := fmt.Sprintf("  #%d %s\n", pr.Number, pr.Title)
	// ruleid: hive-untrusted-github-text-unscanned
	b.WriteString(line)
}

func (s *Scheduler) rawComment(b *strings.Builder, c github.ThreadComment) {
	// ruleid: hive-untrusted-github-text-unscanned
	b.WriteString("    > " + c.Body + "\n")
}

func kickFor(agent string, issue github.Issue) KickMessage {
	// ruleid: hive-untrusted-github-text-unscanned
	return KickMessage{Agent: agent, Message: "Work on: " + issue.Title}
}

// The guarded shape: the scheduler wrapper applies ioscan.EnforceInputFrom.
func (s *Scheduler) scannedTitle(b *strings.Builder, issue github.Issue) {
	title, _ := s.enforceTextFrom(issue.Title, issueSource(issue))
	// ok: hive-untrusted-github-text-unscanned
	b.WriteString(title)
}

func (s *Scheduler) scannedDirect(b *strings.Builder, issue github.Issue) {
	body, _ := ioscan.EnforceInputFrom(issue.Body, issueSource(issue), s.ioscanPolicy())
	// ok: hive-untrusted-github-text-unscanned
	fmt.Fprintf(b, "  context: %s\n", body)
}

// Numbers and repo names are not free text.
func (s *Scheduler) refOnly(b *strings.Builder, issue github.Issue) {
	// ok: hive-untrusted-github-text-unscanned
	b.WriteString(fmt.Sprintf("  %s#%d\n", issue.Repo, issue.Number))
}

func scannedKick(agent, title string) KickMessage {
	// ok: hive-untrusted-github-text-unscanned
	return KickMessage{Agent: agent, Message: "Work on: " + title}
}
