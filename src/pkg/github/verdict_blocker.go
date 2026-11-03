package github

// Verdict-named blockers (hivecommons/hive#15023).
//
// A contributor relay backs a blocked verdict with the repository's `blocked`
// label, but writes nothing the hive can read about WHAT it is blocked on, so
// only a human could ever lift it — and the next agent re-discovered the same
// dependency (projectbluefin/testsuite#975, blocked on server#450, was
// re-reported by eight agents in two weeks). When the verdict's reason names
// its blocker, the hub records it in a form the hive already lifts on close:
//
//   - an open issue becomes a native GitHub "blocked by" link, which
//     ReconcileHiveHardSuppress lifts once every native blocker is closed;
//   - an open pull request becomes a `<!-- hive:blocked-on pr=… -->` marker
//     comment, which ReconcileBlockedIssues lifts once it merges or closes;
//   - anything else (closed target, unreadable target) records nothing and
//     the block stays human-lifted, exactly as before.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/writeback"
)

// VerdictBlocker is one blocker reference parsed from a blocked verdict.
type VerdictBlocker struct {
	Repo   string // owner/repo
	Number int
}

// String renders the reference as owner/repo#N.
func (b VerdictBlocker) String() string {
	return b.Repo + "#" + strconv.Itoa(b.Number)
}

// verdictBlockerRefRE matches, leftmost first, a GitHub issue or PR URL, an
// owner/repo#N reference, or a bare #N. The qualified and bare forms consume
// the preceding character (Go regexp has no look-behind) so a reference glued
// to a word, a path or an HTML entity (`&#39;`) is not taken.
var verdictBlockerRefRE = regexp.MustCompile(
	`https?://github\.com/([A-Za-z0-9-]+/[A-Za-z0-9_.-]+)/(?:issues|pull)/(\d+)` +
		`|(?:^|[^A-Za-z0-9_./#&-])([A-Za-z0-9-]+/[A-Za-z0-9_.-]+)#(\d+)\b` +
		`|(?:^|[^A-Za-z0-9_./#&-])#(\d+)\b`)

// ParseVerdictBlocker returns the first blocker reference in a blocked
// verdict's reason. A bare #N names taskRepo. A reference to the task issue
// itself is skipped, so "#975 is blocked on server#450" yields server#450.
func ParseVerdictBlocker(reason, taskRepo string, taskNumber int) (VerdictBlocker, bool) {
	taskRepo = strings.TrimSpace(taskRepo)
	for _, m := range verdictBlockerRefRE.FindAllStringSubmatch(reason, -1) {
		var ref VerdictBlocker
		var num string
		switch {
		case m[1] != "":
			ref.Repo, num = m[1], m[2]
		case m[3] != "":
			ref.Repo, num = m[3], m[4]
		default:
			if taskRepo == "" {
				continue
			}
			ref.Repo, num = taskRepo, m[5]
		}
		n, err := strconv.Atoi(num)
		if err != nil || n <= 0 {
			continue
		}
		ref.Number = n
		if n == taskNumber && strings.EqualFold(ref.Repo, taskRepo) {
			continue
		}
		return ref, true
	}
	return VerdictBlocker{}, false
}

// VerdictBlockerOutcome is what RecordVerdictBlocker did.
type VerdictBlockerOutcome string

const (
	// VerdictBlockerLinked: a native "blocked by" link was added.
	VerdictBlockerLinked VerdictBlockerOutcome = "linked"
	// VerdictBlockerAlreadyLinked: the native link already existed.
	VerdictBlockerAlreadyLinked VerdictBlockerOutcome = "already_linked"
	// VerdictBlockerMarked: a blocked-on PR marker comment was posted.
	VerdictBlockerMarked VerdictBlockerOutcome = "marked"
	// VerdictBlockerAlreadyMarked: the marker comment already existed.
	VerdictBlockerAlreadyMarked VerdictBlockerOutcome = "already_marked"
	// VerdictBlockerClosed: the blocker is already closed; nothing recorded.
	VerdictBlockerClosed VerdictBlockerOutcome = "closed"
)

// RecordVerdictBlocker records blocker as what repo#number is blocked on. It
// reads the blocker once: an open issue is linked natively (unless the link
// exists), an open PR gets the blocked-on marker comment (unless it is
// already there), and a closed one records nothing. A read failure is
// returned and nothing is written.
func (c *Client) RecordVerdictBlocker(ctx context.Context, repo string, number int, blocker VerdictBlocker) (VerdictBlockerOutcome, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	if err := validateRepoRef(owner, repoName); err != nil {
		return "", fmt.Errorf("RecordVerdictBlocker: %w", err)
	}
	bOwner, bRepo := c.splitRepo(blocker.Repo)
	if err := validateRepoRef(bOwner, bRepo); err != nil || number <= 0 || blocker.Number <= 0 {
		return "", fmt.Errorf("RecordVerdictBlocker: invalid blocker %q for %s/%s#%d", blocker.String(), owner, repoName, number)
	}
	item, err := c.WritebackItemForNumber(ctx, owner+"/"+repoName, number)
	if err != nil {
		return "", err
	}
	if d := c.WritebackAllowed(owner+"/"+repoName, item, writeback.KindGateComment, "blocked on "+blocker.String()); !d.Allowed {
		return "", fmt.Errorf("%w: %s", writeback.ErrSuppressed, d.Detail)
	}
	issue, _, err := c.client.Issues.Get(ctx, bOwner, bRepo, blocker.Number)
	if err != nil {
		return "", fmt.Errorf("reading blocker %s/%s#%d: %w", bOwner, bRepo, blocker.Number, err)
	}
	if !strings.EqualFold(issue.GetState(), "open") {
		return VerdictBlockerClosed, nil
	}
	if issue.IsPullRequest() {
		marker := BlockedOnMarker(bOwner+"/"+bRepo, blocker.Number)
		posted, err := c.IssueCommentsContain(ctx, owner+"/"+repoName, number, marker)
		if err != nil {
			return "", err
		}
		if posted {
			return VerdictBlockerAlreadyMarked, nil
		}
		if err := c.CreateIssueComment(ctx, owner+"/"+repoName, number, verdictBlockerMarkerComment(bOwner+"/"+bRepo, blocker.Number)); err != nil { // writeback:gated
			return "", err
		}
		return VerdictBlockerMarked, nil
	}
	existing, err := c.fetchBlockedBy(ctx, owner, repoName, number)
	if err != nil {
		return "", fmt.Errorf("listing blockers of %s/%s#%d: %w", owner, repoName, number, err)
	}
	for _, b := range existing {
		if b.Number == blocker.Number && strings.EqualFold(b.Owner, bOwner) && strings.EqualFold(b.Repo, bRepo) {
			return VerdictBlockerAlreadyLinked, nil
		}
	}
	if err := c.AddBlockedBy(ctx, owner+"/"+repoName, number, issue.GetID()); err != nil {
		return "", err
	}
	return VerdictBlockerLinked, nil
}

// verdictBlockerMarkerComment is the comment that carries the blocked-on
// marker for a PR a blocked verdict named.
func verdictBlockerMarkerComment(prRepo string, pr int) string {
	return BlockedOnMarker(prRepo, pr) + "\n" +
		fmt.Sprintf("Hive: a contributor's agent reported this issue blocked on %s#%d, an open pull request. The `%s` label is lifted when it merges or closes.", prRepo, pr, BlockedLabel)
}
